package bind

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// IsRoleOrPolicyCommand reports whether node is a native role or RLS policy
// DDL command executed through BoundObjectCommand against MVCC catalog storage.
func IsRoleOrPolicyCommand(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch s := node.Node.(type) {
	case *zpgparse.Node_CreateRoleStmt, *zpgparse.Node_AlterRoleStmt, *zpgparse.Node_DropRoleStmt, *zpgparse.Node_GrantRoleStmt,
		*zpgparse.Node_GrantStmt, *zpgparse.Node_AlterOwnerStmt,
		*zpgparse.Node_CreatePolicyStmt, *zpgparse.Node_AlterPolicyStmt:
		return true
	case *zpgparse.Node_DropStmt:
		return s.DropStmt != nil && s.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_POLICY
	case *zpgparse.Node_RenameStmt:
		return s.RenameStmt != nil && s.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_POLICY
	default:
		return false
	}
}

func bindCreateRole(ctx context.Context, statement *zpgparse.CreateRoleStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE ROLE statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateRoleStmt{CreateRoleStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := b.requireSuperuser("create role", 0); err != nil {
		return nil, err
	}
	roleName := strings.ToLower(strings.TrimSpace(statement.Role))
	if roleName == "" {
		return nil, bindError("42601", "role name cannot be empty", 0)
	}
	if roleName == "public" || roleName == "none" {
		return nil, bindError("42939", fmt.Sprintf("role name %q is reserved", statement.Role), 0)
	}
	if err := declarationName(roleName); err != nil {
		return nil, err
	}
	if _, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: roleName}); err == nil {
		return nil, bindError("42710", fmt.Sprintf("role %q already exists", statement.Role), 0)
	} else if !isMissingObject(err) {
		return nil, err
	}
	meta := model.RoleMetadata{}
	var memberTargets []string
	var addMembers []string
	for _, optNode := range statement.Options {
		def := optNode.GetDefElem()
		if def == nil {
			continue
		}
		val := roleBoolOption(def)
		switch strings.ToLower(def.Defname) {
		case "superuser":
			meta.Superuser = val
		case "nosuperuser":
			meta.Superuser = false
		case "bypassrls":
			meta.BypassRLS = val
		case "nobypassrls":
			meta.BypassRLS = false
		case "addroleto":
			if list := def.Arg.GetList(); list != nil {
				for _, item := range list.Items {
					target, err := roleSpecName(item, options.Bind.CurrentRole, options.Bind.SessionUser)
					if err != nil {
						return nil, err
					}
					if target != "" && target != "public" && !slices.Contains(memberTargets, target) {
						memberTargets = append(memberTargets, target)
					}
				}
			}
		case "rolemembers":
			if list := def.Arg.GetList(); list != nil {
				for _, item := range list.Items {
					member, err := roleSpecName(item, options.Bind.CurrentRole, options.Bind.SessionUser)
					if err != nil {
						return nil, err
					}
					if member != "" && member != "public" && !slices.Contains(addMembers, member) {
						addMembers = append(addMembers, member)
					}
				}
			}
		}
	}
	for _, target := range memberTargets {
		if target == roleName {
			return nil, bindError("0LP01", fmt.Sprintf("role %q cannot be a member of itself", roleName), 0)
		}
		if published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: target}); err == nil {
			b.dependency(published.Descriptor, 0)
		} else if !isMissingObject(err) {
			return nil, err
		} else if !isBuiltinSuperuserAlias(target) {
			return nil, bindError("42704", fmt.Sprintf("role %q does not exist", target), 0)
		}
		meta.MemberOf = append(meta.MemberOf, target)
	}
	var updates []ObjectMutation
	for _, member := range addMembers {
		if member == roleName {
			return nil, bindError("0LP01", fmt.Sprintf("role %q cannot be a member of itself", roleName), 0)
		}
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: member})
		if err != nil {
			if isMissingObject(err) {
				return nil, bindError("42704", fmt.Sprintf("role %q does not exist", member), 0)
			}
			return nil, err
		}
		memberMeta, ok := published.Descriptor.Role()
		if !ok {
			return nil, model.ErrInvalidDescriptor
		}
		if !slices.Contains(memberMeta.MemberOf, roleName) {
			memberMeta.MemberOf = append(slices.Clone(memberMeta.MemberOf), roleName)
			next, err := published.Descriptor.WithRole(memberMeta)
			if err != nil {
				return nil, err
			}
			b.dependency(published.Descriptor, 0)
			updates = append(updates, ObjectMutation{Base: published.Descriptor, Next: next})
		}
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID:      id,
		OID:     oid,
		Name:    model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: roleName},
		Version: 1,
		State:   model.StatePublic,
		Role:    &meta,
	}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		creates:      []*model.Descriptor{descriptor},
		updates:      updates,
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "CREATE ROLE",
	}, ctx.Err()
}

func bindAlterRole(ctx context.Context, statement *zpgparse.AlterRoleStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Role == nil {
		return nil, bindError("42601", "missing ALTER ROLE target", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterRoleStmt{AlterRoleStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := b.requireSuperuser("alter role", 0); err != nil {
		return nil, err
	}
	roleName, err := roleSpecName(&zpgparse.Node{Node: &zpgparse.Node_RoleSpec{RoleSpec: statement.Role}}, options.Bind.CurrentRole, options.Bind.SessionUser)
	if err != nil {
		return nil, err
	}
	published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: roleName})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("42704", fmt.Sprintf("role %q does not exist", roleName), 0)
		}
		return nil, err
	}
	base := published.Descriptor
	meta, ok := base.Role()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	meta.MemberOf = slices.Clone(meta.MemberOf)
	for _, optNode := range statement.Options {
		def := optNode.GetDefElem()
		if def == nil {
			continue
		}
		val := roleBoolOption(def)
		switch strings.ToLower(def.Defname) {
		case "superuser":
			meta.Superuser = val
		case "nosuperuser":
			meta.Superuser = false
		case "bypassrls":
			meta.BypassRLS = val
		case "nobypassrls":
			meta.BypassRLS = false
		}
	}
	next, err := base.WithRole(meta)
	if err != nil {
		return nil, err
	}
	b.dependency(base, 0)
	return &BoundObjectCommand{
		updates:      []ObjectMutation{{Base: base, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "ALTER ROLE",
	}, ctx.Err()
}

func bindDropRole(ctx context.Context, statement *zpgparse.DropRoleStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || len(statement.Roles) == 0 {
		return nil, bindError("42601", "missing DROP ROLE target", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropRoleStmt{DropRoleStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := b.requireSuperuser("drop role", 0); err != nil {
		return nil, err
	}
	command := &BoundObjectCommand{tag: "DROP ROLE"}
	droppedNames := make(map[string]struct{}, len(statement.Roles))
	seenIDs := make(map[model.ObjectID]struct{}, len(statement.Roles))
	for _, roleNode := range statement.Roles {
		roleName, err := roleSpecName(roleNode, options.Bind.CurrentRole, options.Bind.SessionUser)
		if err != nil {
			return nil, err
		}
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: roleName})
		if err != nil {
			if isMissingObject(err) {
				if statement.MissingOk {
					command.warnings = append(command.warnings, model.Error{Code: "42704", Message: fmt.Sprintf("role %q does not exist, skipping", roleName)})
					continue
				}
				return nil, bindError("42704", fmt.Sprintf("role %q does not exist", roleName), 0)
			}
			return nil, err
		}
		d := published.Descriptor
		if _, dup := seenIDs[d.ID()]; dup {
			continue
		}
		seenIDs[d.ID()] = struct{}{}
		droppedNames[strings.ToLower(d.Name().Name)] = struct{}{}
		command.drops = append(command.drops, d)
		b.dependency(d, 0)
	}
	if len(command.drops) == 0 {
		command.noop = true
		command.dependencies = b.dependencies
		return command, ctx.Err()
	}
	// Clean up MemberOf references in all surviving roles within the same transaction.
	_, allRoleDescs, err := loadCatalogRoles(ctx, snapshot, uint64(options.Bind.Limits.MaxOperands))
	if err != nil {
		return nil, err
	}
	for _, d := range allRoleDescs {
		if _, dropping := seenIDs[d.ID()]; dropping {
			continue
		}
		meta, ok := d.Role()
		if !ok || len(meta.MemberOf) == 0 {
			continue
		}
		changed := false
		filtered := make([]string, 0, len(meta.MemberOf))
		for _, member := range meta.MemberOf {
			if _, removed := droppedNames[strings.ToLower(member)]; removed {
				changed = true
				continue
			}
			filtered = append(filtered, member)
		}
		if !changed {
			continue
		}
		meta.MemberOf = filtered
		next, err := d.WithRole(meta)
		if err != nil {
			return nil, err
		}
		b.dependency(d, 0)
		command.updates = append(command.updates, ObjectMutation{Base: d, Next: next})
	}
	command.dependencies = b.dependencies
	command.warnings = append(command.warnings, b.warnings...)
	return command, ctx.Err()
}

func bindGrantRole(ctx context.Context, statement *zpgparse.GrantRoleStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing GRANT/REVOKE ROLE statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_GrantRoleStmt{GrantRoleStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := b.requireSuperuser("grant or revoke role", 0); err != nil {
		return nil, err
	}
	roles, roleDescs, err := loadCatalogRoles(ctx, snapshot, uint64(options.Bind.Limits.MaxOperands))
	if err != nil {
		return nil, err
	}
	descByName := make(map[string]*model.Descriptor, len(roleDescs))
	for _, d := range roleDescs {
		descByName[strings.ToLower(d.Name().Name)] = d
		b.dependency(d, 0)
	}
	grantedRoles := make([]string, 0, len(statement.GrantedRoles))
	for _, rNode := range statement.GrantedRoles {
		var name string
		if ap := rNode.GetAccessPriv(); ap != nil {
			name = strings.ToLower(strings.TrimSpace(ap.PrivName))
		} else {
			name, err = roleSpecName(rNode, options.Bind.CurrentRole, options.Bind.SessionUser)
			if err != nil {
				return nil, err
			}
		}
		if name == "" {
			continue
		}
		if _, ok := roles[name]; !ok && !isBuiltinSuperuserAlias(name) {
			return nil, bindError("42704", fmt.Sprintf("role %q does not exist", name), 0)
		}
		if !slices.Contains(grantedRoles, name) {
			grantedRoles = append(grantedRoles, name)
		}
	}
	tag := "REVOKE ROLE"
	if statement.IsGrant {
		tag = "GRANT ROLE"
	}
	updatedMeta := make(map[string]model.RoleMetadata)
	for _, gNode := range statement.GranteeRoles {
		grantee, err := roleSpecName(gNode, options.Bind.CurrentRole, options.Bind.SessionUser)
		if err != nil {
			return nil, err
		}
		if grantee == "" {
			continue
		}
		meta, ok := roles[grantee]
		if !ok {
			return nil, bindError("42704", fmt.Sprintf("role %q does not exist", grantee), 0)
		}
		if staged, exists := updatedMeta[grantee]; exists {
			meta = staged
		} else {
			meta.MemberOf = slices.Clone(meta.MemberOf)
		}
		for _, granted := range grantedRoles {
			if statement.IsGrant {
				if grantee == granted || roleIsMemberOf(roles, granted, grantee) {
					return nil, bindError("0LP01", fmt.Sprintf("role %q is already a member of role %q", granted, grantee), 0)
				}
				if !slices.Contains(meta.MemberOf, granted) {
					meta.MemberOf = append(meta.MemberOf, granted)
					roles[grantee] = meta
					updatedMeta[grantee] = meta
				}
			} else {
				idx := slices.Index(meta.MemberOf, granted)
				if idx >= 0 {
					meta.MemberOf = slices.Delete(meta.MemberOf, idx, idx+1)
					roles[grantee] = meta
					updatedMeta[grantee] = meta
				}
			}
		}
	}
	command := &BoundObjectCommand{tag: tag, dependencies: b.dependencies, warnings: b.warnings}
	for name, meta := range updatedMeta {
		base := descByName[name]
		next, err := base.WithRole(meta)
		if err != nil {
			return nil, err
		}
		command.updates = append(command.updates, ObjectMutation{Base: base, Next: next})
	}
	if len(command.updates) == 0 {
		command.noop = true
	}
	return command, ctx.Err()
}

func bindCreatePolicy(ctx context.Context, statement *zpgparse.CreatePolicyStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Table == nil || statement.PolicyName == "" {
		return nil, bindError("42601", "invalid CREATE POLICY statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreatePolicyStmt{CreatePolicyStmt: statement}})
	if err != nil {
		return nil, err
	}
	table, schemaName, err := resolvePolicyTable(b, statement.Table)
	if err != nil {
		return nil, err
	}
	if err := b.requireTableOwner(table, int(statement.Table.Location)+1); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Table == nil {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	cmd := strings.ToUpper(strings.TrimSpace(statement.CmdName))
	if cmd == "" {
		cmd = "ALL"
	}
	if err := validatePolicyClauses(cmd, statement.Qual != nil, statement.WithCheck != nil); err != nil {
		return nil, err
	}
	if slices.ContainsFunc(spec.Table.Policies, func(p model.PolicyDescriptor) bool { return p.Name == statement.PolicyName }) {
		return nil, bindError("42710", fmt.Sprintf("policy %q for table %q already exists", statement.PolicyName, table.Name().Name), 0)
	}
	policyRoles, err := bindPolicyRoles(b, statement.Roles, options.Bind.CurrentRole, options.Bind.SessionUser)
	if err != nil {
		return nil, err
	}
	var usingDef, checkDef *model.ExpressionDefinition
	if statement.Qual != nil {
		usingDef, err = bindPolicyExpressionDef(ctx, snapshot, options, table, schemaName, spec.Table, statement.Qual)
		if err != nil {
			return nil, err
		}
	}
	if statement.WithCheck != nil {
		checkDef, err = bindPolicyExpressionDef(ctx, snapshot, options, table, schemaName, spec.Table, statement.WithCheck)
		if err != nil {
			return nil, err
		}
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	policy := model.PolicyDescriptor{
		ID:         id,
		OID:        oid,
		Name:       statement.PolicyName,
		Permissive: statement.Permissive,
		Command:    cmd,
		Roles:      policyRoles,
		Using:      usingDef,
		WithCheck:  checkDef,
	}
	spec.Table.Policies = append(slices.Clone(spec.Table.Policies), policy)
	return finishPolicyTableMutation(ctx, b, table, spec, options, "CREATE POLICY")
}

func bindAlterPolicy(ctx context.Context, statement *zpgparse.AlterPolicyStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Table == nil || statement.PolicyName == "" {
		return nil, bindError("42601", "invalid ALTER POLICY statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterPolicyStmt{AlterPolicyStmt: statement}})
	if err != nil {
		return nil, err
	}
	table, schemaName, err := resolvePolicyTable(b, statement.Table)
	if err != nil {
		return nil, err
	}
	if err := b.requireTableOwner(table, int(statement.Table.Location)+1); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Table == nil {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	idx := slices.IndexFunc(spec.Table.Policies, func(p model.PolicyDescriptor) bool { return p.Name == statement.PolicyName })
	if idx < 0 {
		return nil, bindError("42704", fmt.Sprintf("policy %q for table %q does not exist", statement.PolicyName, table.Name().Name), 0)
	}
	policies := slices.Clone(spec.Table.Policies)
	policy := policies[idx]
	if err := validatePolicyClauses(policy.Command, statement.Qual != nil, statement.WithCheck != nil); err != nil {
		return nil, err
	}
	if len(statement.Roles) > 0 {
		policyRoles, err := bindPolicyRoles(b, statement.Roles, options.Bind.CurrentRole, options.Bind.SessionUser)
		if err != nil {
			return nil, err
		}
		policy.Roles = policyRoles
	}
	if statement.Qual != nil {
		usingDef, err := bindPolicyExpressionDef(ctx, snapshot, options, table, schemaName, spec.Table, statement.Qual)
		if err != nil {
			return nil, err
		}
		policy.Using = usingDef
	}
	if statement.WithCheck != nil {
		checkDef, err := bindPolicyExpressionDef(ctx, snapshot, options, table, schemaName, spec.Table, statement.WithCheck)
		if err != nil {
			return nil, err
		}
		policy.WithCheck = checkDef
	}
	policies[idx] = policy
	spec.Table.Policies = policies
	return finishPolicyTableMutation(ctx, b, table, spec, options, "ALTER POLICY")
}

func bindDropPolicy(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || len(statement.Objects) != 1 {
		return nil, bindError("42601", "invalid DROP POLICY statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	items := statement.Objects[0].GetList().GetItems()
	if len(items) < 2 {
		return nil, bindError("42601", "invalid DROP POLICY target", 0)
	}
	policyNode := items[len(items)-1].GetString_()
	if policyNode == nil || policyNode.Sval == "" {
		return nil, bindError("42601", "invalid DROP POLICY name", 0)
	}
	policyName := policyNode.Sval
	relParts, err := names(items[:len(items)-1])
	if err != nil || len(relParts) == 0 || len(relParts) > 2 {
		return nil, bindError("42601", "invalid DROP POLICY relation", 0)
	}
	relation := &zpgparse.RangeVar{Relname: relParts[len(relParts)-1]}
	if len(relParts) == 2 {
		relation.Schemaname = relParts[0]
	}
	table, _, err := resolvePolicyTable(b, relation)
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && (diagnostic.Code == "42P01" || diagnostic.Code == "3F000") {
				return &BoundObjectCommand{tag: "DROP POLICY", noop: true, warnings: []model.Error{{Code: diagnostic.Code, Message: "policy does not exist, skipping"}}}, nil
			}
		}
		return nil, err
	}
	if err := b.requireTableOwner(table, 0); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Table == nil {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	idx := slices.IndexFunc(spec.Table.Policies, func(p model.PolicyDescriptor) bool { return p.Name == policyName })
	if idx < 0 {
		if statement.MissingOk {
			return &BoundObjectCommand{tag: "DROP POLICY", noop: true, dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist, skipping", policyName, table.Name().Name)}}}, nil
		}
		return nil, bindError("42704", fmt.Sprintf("policy %q for table %q does not exist", policyName, table.Name().Name), 0)
	}
	spec.Table.Policies = slices.Delete(slices.Clone(spec.Table.Policies), idx, idx+1)
	return finishPolicyTableMutation(ctx, b, table, spec, options, "DROP POLICY")
}

func bindRenamePolicy(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Subname == "" || statement.Newname == "" {
		return nil, bindError("42601", "invalid ALTER POLICY RENAME statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	table, _, err := resolvePolicyTable(b, statement.Relation)
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && (diagnostic.Code == "42P01" || diagnostic.Code == "3F000") {
				return &BoundObjectCommand{tag: "ALTER POLICY", noop: true, warnings: []model.Error{{Code: diagnostic.Code, Message: "policy does not exist, skipping"}}}, nil
			}
		}
		return nil, err
	}
	if err := b.requireTableOwner(table, int(statement.Relation.Location)+1); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Table == nil {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	idx := slices.IndexFunc(spec.Table.Policies, func(p model.PolicyDescriptor) bool { return p.Name == statement.Subname })
	if idx < 0 {
		if statement.MissingOk {
			return &BoundObjectCommand{tag: "ALTER POLICY", noop: true, dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist, skipping", statement.Subname, table.Name().Name)}}}, nil
		}
		return nil, bindError("42704", fmt.Sprintf("policy %q for table %q does not exist", statement.Subname, table.Name().Name), 0)
	}
	if slices.ContainsFunc(spec.Table.Policies, func(p model.PolicyDescriptor) bool { return p.Name == statement.Newname }) {
		return nil, bindError("42710", fmt.Sprintf("policy %q for table %q already exists", statement.Newname, table.Name().Name), 0)
	}
	policies := slices.Clone(spec.Table.Policies)
	policies[idx].Name = statement.Newname
	spec.Table.Policies = policies
	return finishPolicyTableMutation(ctx, b, table, spec, options, "ALTER POLICY")
}

func resolvePolicyTable(b *binder, rel *zpgparse.RangeVar) (*model.Descriptor, string, error) {
	if rel == nil || rel.Relname == "" || rel.Catalogname != "" {
		return nil, "", bindError("42601", "invalid policy target relation", 0)
	}
	parts := []string{rel.Relname}
	if rel.Schemaname != "" {
		parts = []string{rel.Schemaname, rel.Relname}
	}
	return b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(rel.Location)+1)
}

func validatePolicyClauses(cmd string, hasQual, hasWithCheck bool) error {
	switch cmd {
	case "ALL":
		return nil
	case "SELECT":
		if hasWithCheck {
			return bindError("42601", "WITH CHECK cannot be applied to SELECT policy", 0)
		}
	case "DELETE":
		if hasWithCheck {
			return bindError("42601", "WITH CHECK cannot be applied to DELETE policy", 0)
		}
	case "INSERT":
		if hasQual {
			return bindError("42601", "only WITH CHECK expression allowed for INSERT policy", 0)
		}
	case "UPDATE":
		return nil
	default:
		return bindError("42601", "invalid policy command", 0)
	}
	return nil
}

func bindPolicyRoles(b *binder, nodes []*zpgparse.Node, currentRole, sessionUser string) ([]string, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(nodes))
	for _, rNode := range nodes {
		name, err := roleSpecName(rNode, currentRole, sessionUser)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		if name != "public" && !isBuiltinSuperuserAlias(name) {
			if _, exists := roles[name]; !exists {
				return nil, bindError("42704", fmt.Sprintf("role %q does not exist", name), 0)
			}
		}
		if !slices.Contains(result, name) {
			result = append(result, name)
		}
	}
	return result, nil
}

func bindPolicyExpressionDef(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, schemaName string, meta *model.TableMetadata, exprNode *zpgparse.Node) (*model.ExpressionDefinition, error) {
	if exprNode == nil {
		return nil, nil
	}
	if err := ValidateStatementFeatures(ctx, exprNode); err != nil {
		return nil, err
	}
	// Validate the policy expression against the table's public column scope so
	// unknown columns, type errors, aggregates, and window functions fail at
	// CREATE/ALTER POLICY time rather than silently deferring to query time.
	eb, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	eb.rlsStack = map[model.ObjectID]bool{table.ID(): true}
	s, err := eb.newScope(nil)
	if err != nil {
		return nil, err
	}
	columns := publicColumns(meta)
	boundCols := make([]column, 0, len(columns))
	for _, col := range columns {
		slot, err := eb.slot(s, col.Layout, col.Nullable, table.ID(), col.ID)
		if err != nil {
			return nil, err
		}
		boundCols = append(boundCols, column{
			ResultColumn: ResultColumn{Name: col.Name, Layout: col.Layout, Nullable: col.Nullable, Slot: slot},
			qualifier:    table.Name().Name,
			namespace:    schemaName,
		})
	}
	s.columns = boundCols
	s.namespaces = []relationNamespace{{
		qualifier:     relationQualifier{namespace: schemaName, name: table.Name().Name},
		descriptor:    table,
		columns:       boundCols,
		systemColumns: true,
	}}
	boundExpr, err := eb.scalar(exprNode, s)
	if err != nil {
		return nil, err
	}
	if _, err := eb.coerce(s, boundExpr, eb.primitive(16), ir.ImplicitCast); err != nil {
		return nil, err
	}
	sqlText, err := zpgparse.DeparseExpression(exprNode)
	if err != nil {
		return nil, err
	}
	encoding, err := proto.Marshal(exprNode)
	if err != nil {
		return nil, err
	}
	return &model.ExpressionDefinition{
		SQL:             sqlText,
		SemanticVersion: 1,
		Encoding:        encoding,
	}, nil
}

func finishPolicyTableMutation(ctx context.Context, b *binder, table *model.Descriptor, spec model.DescriptorSpec, options CreateDeclarationOptions, tag string) (*BoundObjectCommand, error) {
	if table.Version() == math.MaxUint64 {
		return nil, model.ErrDescriptorLimit
	}
	spec.Version = table.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	b.dependency(table, 0)
	return &BoundObjectCommand{
		tag:          tag,
		updates:      []ObjectMutation{{Base: table, Next: next}},
		dependencies: b.dependencies,
		warnings:     b.warnings,
	}, ctx.Err()
}

func roleBoolOption(def *zpgparse.DefElem) bool {
	if def.Arg == nil {
		return true
	}
	if b := def.Arg.GetBoolean(); b != nil {
		return b.Boolval
	}
	if iv := def.Arg.GetInteger(); iv != nil {
		return iv.Ival != 0
	}
	if ac := def.Arg.GetAConst(); ac != nil {
		if b := ac.GetBoolval(); b != nil {
			return b.Boolval
		}
		if iv := ac.GetIval(); iv != nil {
			return iv.Ival != 0
		}
	}
	return true
}

// checkSchemaDropOwnership verifies that the current role owns the schema and,
// when CASCADE is requested, owns every table/view inside the schema so a
// non-owner cannot drop another owner's RLS-protected tables via DROP SCHEMA CASCADE.
func (b *binder) checkSchemaDropOwnership(ctx context.Context, snapshot CatalogSnapshot, schema *model.Descriptor, cascade bool, position int) error {
	if err := b.requireTableOwner(schema, position); err != nil {
		return err
	}
	if !cascade || normalizeRoleName(b.options.CurrentRole) == "" {
		return nil
	}
	roles, err := b.cachedRoles()
	if err != nil {
		return err
	}
	if roleIsSuperuser(roles, b.options.CurrentRole) {
		return nil
	}
	for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView} {
		if err := snapshot.ScanNamespace(ctx, schema.ID(), kind, func(published catalog.PublishedDescriptor) error {
			d := published.Descriptor
			if d == nil || d.State() != model.StatePublic {
				return nil
			}
			return b.requireTableOwner(d, position)
		}); err != nil {
			return err
		}
	}
	return nil
}
