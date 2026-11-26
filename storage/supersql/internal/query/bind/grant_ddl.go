package bind

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindGrantStatement(ctx context.Context, statement *zpgparse.GrantStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing GRANT or REVOKE statement", 0)
	}
	if statement.Targtype == zpgparse.GrantTargetType_ACL_TARGET_DEFAULTS {
		return nil, unsupported(Authorization, 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_GrantStmt{GrantStmt: statement}})
	if err != nil {
		return nil, err
	}
	mask, err := resolveGrantPrivileges(statement.Objtype, statement.Privileges)
	if err != nil {
		return nil, err
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return nil, err
	}
	grantees, err := resolveGrantGrantees(b, roles, statement.Grantees, options.Bind.CurrentRole, options.Bind.SessionUser)
	if err != nil {
		return nil, err
	}
	targets, err := resolveGrantTargets(ctx, b, snapshot, statement.Targtype, statement.Objtype, statement.Objects)
	if err != nil {
		return nil, err
	}

	tag := "REVOKE"
	if statement.IsGrant {
		tag = "GRANT"
	}
	if len(targets) == 0 {
		return &BoundObjectCommand{dependencies: slices.Clone(b.dependencies), tag: tag, noop: true}, nil
	}

	grantor := normalizeRoleName(options.Bind.CurrentRole)
	if isBuiltinSuperuserAlias(grantor) {
		grantor = "supersql"
	}
	updates := make([]ObjectMutation, 0, len(targets))
	seenTargets := make(map[model.ObjectID]struct{}, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if _, dup := seenTargets[target.ID()]; dup {
			continue
		}
		seenTargets[target.ID()] = struct{}{}
		if !hasGrantOption(roles, options.Bind.CurrentRole, target, mask) {
			return nil, permissionDeniedError(target, 0)
		}
		revokeAllGrantors := roleIsSuperuser(roles, options.Bind.CurrentRole) || roleIsOwner(roles, options.Bind.CurrentRole, target.OwnerName())
		nextACL := applyGrantToACL(target.ACL(), grantees, mask, grantor, statement.IsGrant, statement.GrantOption, revokeAllGrantors)
		next, err := target.WithACL(nextACL)
		if err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: target, Next: next})
	}
	return &BoundObjectCommand{
		updates:      updates,
		dependencies: slices.Clone(b.dependencies),
		tag:          tag,
		noop:         len(updates) == 0,
	}, nil
}

func resolveGrantPrivileges(objtype zpgparse.ObjectType, privNodes []*zpgparse.Node) (model.PrivilegeMask, error) {
	allMask, allowed, err := objectTypePrivilegeSet(objtype)
	if err != nil {
		return 0, err
	}
	// Empty Privileges slice in PostgreSQL AST represents ALL [PRIVILEGES].
	if len(privNodes) == 0 {
		return allMask, nil
	}
	var mask model.PrivilegeMask
	for _, node := range privNodes {
		priv := node.GetAccessPriv()
		if priv == nil {
			return 0, bindError("42601", "invalid privilege specification", 0)
		}
		if len(priv.Cols) != 0 {
			return 0, unsupported(Authorization, 0)
		}
		name := strings.ToLower(strings.TrimSpace(priv.PrivName))
		if name == "" || name == "all" || name == "all privileges" {
			mask |= allMask
			continue
		}
		bit, ok := allowed[name]
		if !ok {
			return 0, bindError("0LP01", fmt.Sprintf("invalid privilege type %s for object", strings.ToUpper(priv.PrivName)), 0)
		}
		mask |= bit
	}
	if mask == 0 {
		return 0, bindError("42601", "missing privilege specification", 0)
	}
	return mask, nil
}

func objectTypePrivilegeSet(objtype zpgparse.ObjectType) (model.PrivilegeMask, map[string]model.PrivilegeMask, error) {
	switch objtype {
	case zpgparse.ObjectType_OBJECT_TABLE:
		return model.PrivilegeAllTable, map[string]model.PrivilegeMask{
			"select":     model.PrivilegeSelect,
			"insert":     model.PrivilegeInsert,
			"update":     model.PrivilegeUpdate,
			"delete":     model.PrivilegeDelete,
			"truncate":   model.PrivilegeTruncate,
			"references": model.PrivilegeReferences,
			"trigger":    model.PrivilegeTrigger,
		}, nil
	case zpgparse.ObjectType_OBJECT_SEQUENCE:
		return model.PrivilegeAllSequence, map[string]model.PrivilegeMask{
			"usage":  model.PrivilegeUsage,
			"select": model.PrivilegeSelect,
			"update": model.PrivilegeUpdate,
		}, nil
	case zpgparse.ObjectType_OBJECT_SCHEMA:
		return model.PrivilegeAllSchema, map[string]model.PrivilegeMask{
			"usage":  model.PrivilegeUsage,
			"create": model.PrivilegeCreate,
		}, nil
	case zpgparse.ObjectType_OBJECT_FUNCTION, zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE:
		return model.PrivilegeAllRoutine, map[string]model.PrivilegeMask{
			"execute": model.PrivilegeExecute,
		}, nil
	default:
		return 0, nil, unsupported(Authorization, 0)
	}
}

func resolveGrantGrantees(b *binder, roles map[string]model.RoleMetadata, nodes []*zpgparse.Node, currentRole, sessionUser string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, bindError("42601", "missing grantee specification", 0)
	}
	grantees := make([]string, 0, len(nodes))
	for _, node := range nodes {
		name, err := roleSpecName(node, currentRole, sessionUser)
		if err != nil {
			return nil, err
		}
		if name != "public" {
			if _, err := b.validateRoleExists(roles, name, 0); err != nil {
				return nil, err
			}
		}
		if !slices.Contains(grantees, name) {
			grantees = append(grantees, name)
		}
	}
	return grantees, nil
}

func resolveGrantTargets(ctx context.Context, b *binder, snapshot CatalogSnapshot, targtype zpgparse.GrantTargetType, objtype zpgparse.ObjectType, objects []*zpgparse.Node) ([]*model.Descriptor, error) {
	switch targtype {
	case zpgparse.GrantTargetType_ACL_TARGET_OBJECT:
		return resolveSingleGrantTargets(ctx, b, snapshot, objtype, objects)
	case zpgparse.GrantTargetType_ACL_TARGET_ALL_IN_SCHEMA:
		return resolveAllInSchemaTargets(ctx, b, snapshot, objtype, objects)
	default:
		return nil, unsupported(Authorization, 0)
	}
}

func resolveSingleGrantTargets(ctx context.Context, b *binder, snapshot CatalogSnapshot, objtype zpgparse.ObjectType, objects []*zpgparse.Node) ([]*model.Descriptor, error) {
	targets := make([]*model.Descriptor, 0, len(objects))
	for _, obj := range objects {
		switch objtype {
		case zpgparse.ObjectType_OBJECT_TABLE:
			rv := obj.GetRangeVar()
			if rv == nil || rv.Relname == "" || rv.Catalogname != "" {
				return nil, bindError("42601", "invalid GRANT relation target", 0)
			}
			parts := []string{rv.Relname}
			if rv.Schemaname != "" {
				parts = []string{rv.Schemaname, rv.Relname}
			}
			desc, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}, "42P01", int(rv.Location)+1)
			if err != nil {
				return nil, err
			}
			targets = append(targets, desc)
		case zpgparse.ObjectType_OBJECT_SEQUENCE:
			rv := obj.GetRangeVar()
			if rv == nil || rv.Relname == "" || rv.Catalogname != "" {
				return nil, bindError("42601", "invalid GRANT sequence target", 0)
			}
			parts := []string{rv.Relname}
			if rv.Schemaname != "" {
				parts = []string{rv.Schemaname, rv.Relname}
			}
			desc, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectSequence}, "42P01", int(rv.Location)+1)
			if err != nil {
				return nil, err
			}
			targets = append(targets, desc)
		case zpgparse.ObjectType_OBJECT_SCHEMA:
			schemaName := obj.GetString_().GetSval()
			if schemaName == "" {
				return nil, bindError("42601", "invalid GRANT schema target", 0)
			}
			published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
			if isMissingObject(err) {
				return nil, bindError("3F000", fmt.Sprintf("schema %q does not exist", schemaName), 0)
			}
			if err != nil {
				return nil, err
			}
			b.dependency(published.Descriptor, 0)
			targets = append(targets, published.Descriptor)
		case zpgparse.ObjectType_OBJECT_FUNCTION, zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE:
			desc, err := resolveRoutineReference(ctx, b, snapshot, obj.GetObjectWithArgs(), objtype)
			if err != nil {
				return nil, err
			}
			targets = append(targets, desc)
		default:
			return nil, unsupported(Authorization, 0)
		}
	}
	return targets, nil
}

func resolveAllInSchemaTargets(ctx context.Context, b *binder, snapshot CatalogSnapshot, objtype zpgparse.ObjectType, objects []*zpgparse.Node) ([]*model.Descriptor, error) {
	var kinds []model.ObjectKind
	switch objtype {
	case zpgparse.ObjectType_OBJECT_TABLE:
		kinds = []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}
	case zpgparse.ObjectType_OBJECT_SEQUENCE:
		kinds = []model.ObjectKind{model.ObjectSequence}
	case zpgparse.ObjectType_OBJECT_FUNCTION, zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE:
		kinds = []model.ObjectKind{model.ObjectRoutine}
	default:
		return nil, unsupported(Authorization, 0)
	}

	var targets []*model.Descriptor
	for _, obj := range objects {
		schemaName := obj.GetString_().GetSval()
		if schemaName == "" {
			return nil, bindError("42601", "invalid schema name in GRANT/REVOKE", 0)
		}
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			return nil, bindError("3F000", fmt.Sprintf("schema %q does not exist", schemaName), 0)
		}
		if err != nil {
			return nil, err
		}
		b.dependency(published.Descriptor, 0)
		for _, kind := range kinds {
			err := snapshot.ScanNamespace(ctx, published.Descriptor.ID(), kind, func(item catalog.PublishedDescriptor) error {
				d := item.Descriptor
				if d == nil || d.State() != model.StatePublic {
					return nil
				}
				if kind == model.ObjectRoutine {
					spec, specErr := d.Spec(ctx)
					if specErr != nil {
						return specErr
					}
					if !routineKindMatches(spec.Routine, objtype) {
						return nil
					}
				}
				b.dependency(d, 0)
				targets = append(targets, d)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return targets, nil
}

func applyGrantToACL(existing []model.PrivilegeGrant, grantees []string, mask model.PrivilegeMask, grantor string, isGrant, grantOption, revokeAllGrantors bool) []model.PrivilegeGrant {
	type aclKey struct {
		grantee string
		grantor string
	}
	acl := make([]model.PrivilegeGrant, len(existing))
	copy(acl, existing)
	indexByKey := make(map[aclKey]int, len(acl))
	for i, entry := range acl {
		indexByKey[aclKey{grantee: entry.Grantee, grantor: entry.Grantor}] = i
	}
	for _, grantee := range grantees {
		if isGrant {
			key := aclKey{grantee: grantee, grantor: grantor}
			idx, found := indexByKey[key]
			if !found {
				entry := model.PrivilegeGrant{
					Grantee:    grantee,
					Privileges: mask,
					Grantor:    grantor,
				}
				if grantOption {
					entry.GrantOption = mask
				}
				indexByKey[key] = len(acl)
				acl = append(acl, entry)
			} else {
				acl[idx].Privileges |= mask
				if grantOption {
					acl[idx].GrantOption |= mask
				}
			}
			continue
		}
		// REVOKE: superusers and object owners revoke across all grantors for
		// grantee; non-owner holders of WITH GRANT OPTION revoke only grants
		// they themselves issued (matching entry.Grantor == grantor).
		for idx := range acl {
			if acl[idx].Grantee != grantee {
				continue
			}
			if !revokeAllGrantors && acl[idx].Grantor != grantor {
				continue
			}
			if grantOption {
				// REVOKE GRANT OPTION FOR ... removes only grantability.
				acl[idx].GrantOption &^= mask
			} else {
				acl[idx].Privileges &^= mask
				acl[idx].GrantOption &^= mask
			}
		}
	}
	filtered := make([]model.PrivilegeGrant, 0, len(acl))
	for _, entry := range acl {
		if entry.Privileges != 0 {
			entry.GrantOption &= entry.Privileges
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func bindAlterOwner(ctx context.Context, statement *zpgparse.AlterOwnerStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newowner == nil {
		return nil, bindError("42601", "invalid ALTER OWNER statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterOwnerStmt{AlterOwnerStmt: statement}})
	if err != nil {
		return nil, err
	}
	var base *model.Descriptor
	var tag string
	switch statement.ObjectType {
	case zpgparse.ObjectType_OBJECT_SCHEMA:
		schemaName := statement.Object.GetString_().GetSval()
		if schemaName == "" {
			return nil, bindError("42601", "invalid schema name in ALTER SCHEMA OWNER TO", 0)
		}
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			return nil, bindError("3F000", fmt.Sprintf("schema %q does not exist", schemaName), 0)
		}
		if err != nil {
			return nil, err
		}
		b.dependency(published.Descriptor, 0)
		base = published.Descriptor
		tag = "ALTER SCHEMA"
	case zpgparse.ObjectType_OBJECT_FUNCTION, zpgparse.ObjectType_OBJECT_PROCEDURE, zpgparse.ObjectType_OBJECT_ROUTINE:
		desc, err := resolveRoutineReference(ctx, b, snapshot, statement.Object.GetObjectWithArgs(), statement.ObjectType)
		if err != nil {
			return nil, err
		}
		base = desc
		tag = objectCommandTag("ALTER", statement.ObjectType)
	default:
		return nil, unsupported(Authorization, 0)
	}
	newOwnerOID, newOwnerName, err := b.validateNewOwner(statement.Newowner, base, 0)
	if err != nil {
		return nil, err
	}
	next, err := base.WithOwner(newOwnerOID, newOwnerName)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: slices.Clone(b.dependencies),
		tag:          tag,
	}, nil
}
