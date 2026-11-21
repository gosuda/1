package bind

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// normalizeRoleName returns the canonical lowercase role identifier, or "" for
// the unset/NONE default principal.
func normalizeRoleName(name string) string {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if trimmed == "none" {
		return ""
	}
	return trimmed
}

// normalizeOwnerName returns the stored descriptor OwnerName for currentRole.
// Default built-in superuser aliases store "" (meaning default superuser owner)
// so legacy and newly created superuser-owned objects share one representation.
func normalizeOwnerName(currentRole string) string {
	role := normalizeRoleName(currentRole)
	if isBuiltinSuperuserAlias(role) {
		return ""
	}
	return role
}

func isBuiltinSuperuserAlias(role string) bool {
	return role == "" || role == "embedded" || role == "supersql" || role == "postgres"
}

func loadCatalogRoles(ctx context.Context, snapshot CatalogSnapshot, limit uint64) (map[string]model.RoleMetadata, []*model.Descriptor, error) {
	roles := make(map[string]model.RoleMetadata)
	if snapshot == nil {
		return roles, nil, nil
	}
	var descriptors []*model.Descriptor
	var count uint64
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectRole, func(published catalog.PublishedDescriptor) error {
		count++
		if limit > 0 && count > limit {
			return bindError("54000", "role catalog scan limit exceeded", 0)
		}
		d := published.Descriptor
		if d == nil || d.State() != model.StatePublic {
			return nil
		}
		meta, ok := d.Role()
		if !ok {
			return nil
		}
		roles[strings.ToLower(d.Name().Name)] = meta
		descriptors = append(descriptors, d)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return roles, descriptors, nil
}

// roleIsSuperuser reports whether role holds direct superuser privileges.
// Superuser is NEVER inherited via MemberOf (PostgreSQL standard).
func roleIsSuperuser(roles map[string]model.RoleMetadata, role string) bool {
	r := normalizeRoleName(role)
	if r == "" {
		return true
	}
	if meta, exists := roles[r]; exists {
		return meta.Superuser
	}
	return isBuiltinSuperuserAlias(r)
}

// roleHasBypassRLS reports whether role holds an explicit direct BYPASSRLS
// attribute. BYPASSRLS is NEVER inherited via MemberOf (PostgreSQL standard).
func roleHasBypassRLS(roles map[string]model.RoleMetadata, role string) bool {
	r := normalizeRoleName(role)
	if r == "" {
		return false
	}
	if meta, exists := roles[r]; exists {
		return meta.BypassRLS
	}
	return false
}

// roleIsMemberOf reports whether role is equal to target or transitively a
// member of target via MemberOf links.
func roleIsMemberOf(roles map[string]model.RoleMetadata, role, target string) bool {
	r := normalizeRoleName(role)
	t := normalizeRoleName(target)
	if r == "" || t == "" {
		return r == t
	}
	if r == t {
		return true
	}
	visited := map[string]struct{}{r: {}}
	queue := []string{r}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		meta, ok := roles[current]
		if !ok {
			continue
		}
		for _, parent := range meta.MemberOf {
			p := strings.ToLower(parent)
			if p == t {
				return true
			}
			if _, seen := visited[p]; !seen {
				visited[p] = struct{}{}
				queue = append(queue, p)
			}
		}
	}
	return false
}

// roleIsOwner reports whether currentRole owns an object with ownerName.
func roleIsOwner(roles map[string]model.RoleMetadata, currentRole, ownerName string) bool {
	if roleIsSuperuser(roles, currentRole) {
		return true
	}
	r := normalizeRoleName(currentRole)
	owner := normalizeRoleName(ownerName)
	if owner == "" {
		return false
	}
	return r == owner || roleIsMemberOf(roles, r, owner)
}

// rlsApplies reports whether Row-Level Security policies must be enforced for
// (currentRole, table). FORCE ROW LEVEL SECURITY enforces policies even for
// superusers and table owners unless the role has explicit BYPASSRLS.
func rlsApplies(roles map[string]model.RoleMetadata, currentRole string, table *model.TableMetadata) bool {
	if table == nil || !table.RowSecurity {
		return false
	}
	if roleHasBypassRLS(roles, currentRole) {
		return false
	}
	if table.ForceRowSecurity {
		return true
	}
	if roleIsSuperuser(roles, currentRole) {
		return false
	}
	if roleIsOwner(roles, currentRole, table.OwnerName) {
		return false
	}
	return true
}

// policyMatchesRole reports whether a policy targeting policyRoles applies to
// currentRole (either directly, via PUBLIC, via transitive membership, or via
// built-in default superuser aliases).
func policyMatchesRole(roles map[string]model.RoleMetadata, currentRole string, policyRoles []string) bool {
	if len(policyRoles) == 0 {
		return true
	}
	r := normalizeRoleName(currentRole)
	_, explicit := roles[r]
	implicitBuiltin := r == "" || (!explicit && isBuiltinSuperuserAlias(r))
	for _, candidate := range policyRoles {
		target := strings.ToLower(strings.TrimSpace(candidate))
		if target == "" || target == "public" {
			return true
		}
		if implicitBuiltin && isBuiltinSuperuserAlias(target) {
			return true
		}
		if r != "" && (r == target || roleIsMemberOf(roles, r, target)) {
			return true
		}
	}
	return false
}

// CheckSetRoleAuthorization verifies that currentRole is allowed to execute
// SET [LOCAL] ROLE targetRole against snapshot and returns the normalized
// target role name ("" for NONE/reset).
func CheckSetRoleAuthorization(ctx context.Context, snapshot CatalogSnapshot, currentRole, targetRole string, maxRoles uint64) (string, error) {
	target := normalizeRoleName(targetRole)
	if target == "" {
		return "", nil
	}
	roles, _, err := loadCatalogRoles(ctx, snapshot, maxRoles)
	if err != nil {
		return "", err
	}
	_, exists := roles[target]
	if !exists && !isBuiltinSuperuserAlias(target) {
		return "", &model.Error{Code: "22023", Message: fmt.Sprintf("role %q does not exist", targetRole)}
	}
	if !roleIsSuperuser(roles, currentRole) && !roleIsMemberOf(roles, currentRole, target) {
		return "", &model.Error{Code: "42501", Message: fmt.Sprintf("permission denied to set role %q", targetRole)}
	}
	return target, nil
}

func (b *binder) cachedRoles() (map[string]model.RoleMetadata, error) {
	if b.rlsRolesLoaded {
		return b.rlsRoles, nil
	}
	roles, descriptors, err := loadCatalogRoles(b.ctx, b.catalog, uint64(b.options.Limits.MaxOperands))
	if err != nil {
		return nil, err
	}
	for _, d := range descriptors {
		b.dependency(d, 0)
	}
	b.rlsRoles = roles
	b.rlsRolesLoaded = true
	return roles, nil
}

func (b *binder) requireSuperuser(action string, position int) error {
	if normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if roleIsSuperuser(roles, b.options.CurrentRole) {
		return nil
	}
	return bindError("42501", "permission denied to "+action, position)
}

func (b *binder) requireTableOwner(table *model.Descriptor, position int) error {
	if table == nil || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if roleIsSuperuser(roles, b.options.CurrentRole) {
		return nil
	}
	ownerName := ""
	switch table.Kind() {
	case model.ObjectTable, model.ObjectMaterializedView:
		if meta, ok := table.Table(); ok {
			ownerName = meta.OwnerName
		}
	case model.ObjectView:
		if meta, ok := table.View(); ok {
			ownerName = meta.OwnerName
		}
	case model.ObjectSchema:
		if spec, err := table.Spec(b.ctx); err == nil && spec.Schema != nil {
			ownerName = spec.Schema.OwnerName
		}
	default:
		return nil
	}
	if roleIsOwner(roles, b.options.CurrentRole, ownerName) {
		return nil
	}
	return bindError("42501", fmt.Sprintf("must be owner of %s %s", ownerObjectKindLabel(table.Kind()), table.Name().Name), position)
}

func ownerObjectKindLabel(kind model.ObjectKind) string {
	switch kind {
	case model.ObjectTable:
		return "table"
	case model.ObjectView:
		return "view"
	case model.ObjectMaterializedView:
		return "materialized view"
	case model.ObjectSchema:
		return "schema"
	default:
		return "relation"
	}
}

func roleSpecName(node *zpgparse.Node, currentRole, sessionUser string) (string, error) {
	if node == nil {
		return "", bindError("42601", "missing role specification", 0)
	}
	if rs := node.GetRoleSpec(); rs != nil {
		switch rs.Roletype {
		case zpgparse.RoleSpecType_ROLESPEC_CSTRING:
			return strings.ToLower(strings.TrimSpace(rs.Rolename)), nil
		case zpgparse.RoleSpecType_ROLESPEC_CURRENT_ROLE, zpgparse.RoleSpecType_ROLESPEC_CURRENT_USER:
			r := normalizeRoleName(currentRole)
			if r == "" {
				return (model.Principal{}).Name(), nil
			}
			return r, nil
		case zpgparse.RoleSpecType_ROLESPEC_SESSION_USER:
			s := normalizeRoleName(sessionUser)
			if s == "" {
				return (model.Principal{}).Name(), nil
			}
			return s, nil
		case zpgparse.RoleSpecType_ROLESPEC_PUBLIC:
			return "public", nil
		}
	}
	if s := node.GetString_(); s != nil {
		return strings.ToLower(strings.TrimSpace(s.Sval)), nil
	}
	if ac := node.GetAConst(); ac != nil && ac.GetSval() != nil {
		return strings.ToLower(strings.TrimSpace(ac.GetSval().Sval)), nil
	}
	return "", bindError("42601", "invalid role specification", 0)
}
