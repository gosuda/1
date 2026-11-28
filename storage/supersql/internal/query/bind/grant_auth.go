package bind

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const maxPrivilegeRoleScan uint64 = 65536

func privilegeObjectKindLabel(kind model.ObjectKind) string {
	switch kind {
	case model.ObjectTable:
		return "table"
	case model.ObjectView:
		return "view"
	case model.ObjectMaterializedView:
		return "materialized view"
	case model.ObjectSequence:
		return "sequence"
	case model.ObjectSchema:
		return "schema"
	case model.ObjectRoutine:
		return "function"
	default:
		return "relation"
	}
}

func isSystemOrTempSchema(name string) bool {
	return name == "pg_catalog" ||
		name == "information_schema" ||
		name == "pg_temp" ||
		strings.HasPrefix(name, "pg_temp_")
}

func isTempSchema(name string) bool {
	return name == "pg_temp" || strings.HasPrefix(name, "pg_temp_")
}

// hasObjectPrivilege reports whether currentRole holds required (all bits when
// anyBit is false, or at least one bit when anyBit is true) on descriptor.
func hasObjectPrivilege(roles map[string]model.RoleMetadata, currentRole string, descriptor *model.Descriptor, required model.PrivilegeMask, anyBit bool) bool {
	if descriptor == nil || required == 0 {
		return true
	}
	r := normalizeRoleName(currentRole)
	if r == "" || roleIsSuperuser(roles, r) {
		return true
	}
	if descriptor.Kind() == model.ObjectSchema {
		name := descriptor.Name().Name
		if isTempSchema(name) {
			return true
		}
		if name == "pg_catalog" || name == "information_schema" {
			if anyBit {
				return (required & model.PrivilegeUsage) != 0
			}
			return required == model.PrivilegeUsage
		}
	}
	if roleIsOwner(roles, r, descriptor.OwnerName()) {
		return true
	}
	var effective model.PrivilegeMask
	for _, grant := range descriptor.ACL() {
		if grant.Grantee == "public" || roleIsMemberOf(roles, r, grant.Grantee) {
			effective |= grant.Privileges
		}
	}
	if anyBit {
		return (effective & required) != 0
	}
	return (effective & required) == required
}

// hasGrantOption reports whether currentRole is authorized to GRANT or REVOKE
// all bits in mask on descriptor (either as superuser, object owner, or holder
// of WITH GRANT OPTION on every bit in mask).
func hasGrantOption(roles map[string]model.RoleMetadata, currentRole string, descriptor *model.Descriptor, mask model.PrivilegeMask) bool {
	if descriptor == nil || mask == 0 {
		return true
	}
	r := normalizeRoleName(currentRole)
	if r == "" || roleIsSuperuser(roles, r) {
		return true
	}
	if roleIsOwner(roles, r, descriptor.OwnerName()) {
		return true
	}
	var grantable model.PrivilegeMask
	for _, grant := range descriptor.ACL() {
		if grant.Grantee == "public" || roleIsMemberOf(roles, r, grant.Grantee) {
			grantable |= grant.GrantOption
		}
	}
	return (grantable & mask) == mask
}

func permissionDeniedError(descriptor *model.Descriptor, pos int) error {
	if descriptor == nil {
		return bindError("42501", "permission denied", pos)
	}
	return bindError("42501", fmt.Sprintf("permission denied for %s %s", privilegeObjectKindLabel(descriptor.Kind()), descriptor.Name().Name), pos)
}

// CheckObjectPrivilege verifies that currentRole holds all bits in required on
// descriptor (directly, via superuser, via ownership, via PUBLIC, or via
// transitive role membership). Returns SQLSTATE 42501 on denial.
func CheckObjectPrivilege(ctx context.Context, snapshot CatalogSnapshot, currentRole string, descriptor *model.Descriptor, required model.PrivilegeMask) error {
	return checkObjectPrivilegeWithSnapshot(ctx, snapshot, currentRole, descriptor, required, false)
}

// CheckObjectPrivilegeAny verifies that currentRole holds at least one bit in
// anyOf on descriptor (for example, USAGE or UPDATE for sequence nextval).
func CheckObjectPrivilegeAny(ctx context.Context, snapshot CatalogSnapshot, currentRole string, descriptor *model.Descriptor, anyOf model.PrivilegeMask) error {
	return checkObjectPrivilegeWithSnapshot(ctx, snapshot, currentRole, descriptor, anyOf, true)
}

func checkObjectPrivilegeWithSnapshot(ctx context.Context, snapshot CatalogSnapshot, currentRole string, descriptor *model.Descriptor, required model.PrivilegeMask, anyBit bool) error {
	if descriptor == nil || required == 0 {
		return nil
	}
	r := normalizeRoleName(currentRole)
	if r == "" {
		return nil
	}
	roles, _, err := loadCatalogRoles(ctx, snapshot, maxPrivilegeRoleScan)
	if err != nil {
		return err
	}
	if hasObjectPrivilege(roles, r, descriptor, required, anyBit) {
		return nil
	}
	return permissionDeniedError(descriptor, 0)
}

func (b *binder) checkObjectPrivilege(descriptor *model.Descriptor, required model.PrivilegeMask, pos int) error {
	if descriptor == nil || required == 0 || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if hasObjectPrivilege(roles, b.options.CurrentRole, descriptor, required, false) {
		return nil
	}
	return permissionDeniedError(descriptor, pos)
}

func (b *binder) checkObjectPrivilegeAny(descriptor *model.Descriptor, anyOf model.PrivilegeMask, pos int) error {
	if descriptor == nil || anyOf == 0 || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if hasObjectPrivilege(roles, b.options.CurrentRole, descriptor, anyOf, true) {
		return nil
	}
	return permissionDeniedError(descriptor, pos)
}

func (b *binder) checkSchemaUsage(schema *model.Descriptor, pos int) error {
	if schema == nil || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	return b.checkObjectPrivilege(schema, model.PrivilegeUsage, pos)
}

func (b *binder) checkSchemaCreate(schema *model.Descriptor, pos int) error {
	if schema == nil || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	return b.checkObjectPrivilege(schema, model.PrivilegeCreate, pos)
}

// validateRoleExists verifies that roleName names an existing catalog role or
// built-in superuser alias, recording a catalog dependency when found.
func (b *binder) validateRoleExists(roles map[string]model.RoleMetadata, roleName string, pos int) (model.OID, error) {
	target := normalizeRoleName(roleName)
	if target == "" || target == "public" {
		return 0, bindError("42704", fmt.Sprintf("role %q does not exist", roleName), pos)
	}
	if published, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: target}); err == nil {
		b.dependency(published.Descriptor, 0)
		return published.Descriptor.OID(), nil
	} else if !isMissingObject(err) {
		return 0, err
	}
	if isBuiltinSuperuserAlias(target) {
		return 0, nil
	}
	return 0, bindError("42704", fmt.Sprintf("role %q does not exist", roleName), pos)
}

// validateNewOwner verifies ALTER ... OWNER TO authorization:
//   - target role must exist (42704)
//   - caller must be superuser, OR (current owner of target AND member of newOwner) (42501)
func (b *binder) validateNewOwner(spec *zpgparse.RoleSpec, target *model.Descriptor, pos int) (model.OID, string, error) {
	if spec == nil {
		return 0, "", bindError("42601", "missing new owner specification", pos)
	}
	roleName, err := roleSpecName(&zpgparse.Node{Node: &zpgparse.Node_RoleSpec{RoleSpec: spec}}, b.options.CurrentRole, b.options.SessionUser)
	if err != nil {
		return 0, "", err
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return 0, "", err
	}
	ownerOID, err := b.validateRoleExists(roles, roleName, pos)
	if err != nil {
		return 0, "", err
	}
	if !roleIsSuperuser(roles, b.options.CurrentRole) {
		if target != nil && !roleIsOwner(roles, b.options.CurrentRole, target.OwnerName()) {
			return 0, "", bindError("42501", fmt.Sprintf("must be owner of %s %s", privilegeObjectKindLabel(target.Kind()), target.Name().Name), pos)
		}
		if !roleIsMemberOf(roles, b.options.CurrentRole, roleName) {
			return 0, "", bindError("42501", fmt.Sprintf("must be able to SET ROLE %q", roleName), pos)
		}
	}
	return ownerOID, normalizeOwnerName(roleName), nil
}

// astContainsColumnRef reports whether node contains any ColumnRef anywhere in
// its AST subtree (used to detect whether an UPDATE RHS reads table columns).
func astContainsColumnRef(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	return protoContainsColumnRef(node.ProtoReflect())
}

func protoContainsColumnRef(msg protoreflect.Message) bool {
	if !msg.IsValid() {
		return false
	}
	if n, ok := msg.Interface().(*zpgparse.Node); ok && n != nil && n.GetColumnRef() != nil {
		return true
	}
	found := false
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			list := v.List()
			for i := range list.Len() {
				if protoContainsColumnRef(list.Get(i).Message()) {
					found = true
					return false
				}
			}
		case !fd.IsList() && !fd.IsMap() && fd.Kind() == protoreflect.MessageKind:
			if protoContainsColumnRef(v.Message()) {
				found = true
				return false
			}
		}
		return true
	})
	return found
}
