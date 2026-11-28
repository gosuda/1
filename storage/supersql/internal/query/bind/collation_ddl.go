package bind

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindCreateCollation(ctx context.Context, statement *zpgparse.DefineStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Kind != zpgparse.ObjectType_OBJECT_COLLATION {
		return nil, bindError("42601", "missing CREATE COLLATION statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DefineStmt{DefineStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Defnames)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid collation name", 0)
	}
	var schemaName, collName string
	var schemaDesc catalog.PublishedDescriptor
	if len(parts) == 2 {
		schemaName, collName = parts[0], parts[1]
		if err := declarationName(collName); err != nil {
			return nil, err
		}
		resolved, rerr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if rerr != nil {
			if isMissingObject(rerr) {
				return nil, bindError("3F000", fmt.Sprintf("schema %q does not exist", schemaName), 0)
			}
			return nil, rerr
		}
		schemaDesc = resolved
	} else {
		collName = parts[0]
		if err := declarationName(collName); err != nil {
			return nil, err
		}
		foundSchema := false
		for _, candidate := range options.Bind.SearchPath {
			if candidate == "" || candidate == "pg_catalog" || candidate == "$user" {
				continue
			}
			resolved, rerr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: candidate})
			if rerr != nil {
				if isMissingObject(rerr) {
					continue
				}
				return nil, rerr
			}
			schemaName = candidate
			schemaDesc = resolved
			foundSchema = true
			break
		}
		if !foundSchema {
			return nil, bindError("3F000", "no schema has been selected to create in", 0)
		}
	}
	if err := b.checkSchemaCreate(schemaDesc.Descriptor, 0); err != nil {
		return nil, err
	}
	b.dependency(schemaDesc.Descriptor, 0)

	if _, err := snapshot.Resolve(ctx, model.NamespaceName{
		Parent: schemaDesc.Descriptor.ID(),
		Kind:   model.ObjectCollation,
		Name:   collName,
	}); err == nil {
		if statement.IfNotExists {
			return &BoundObjectCommand{
				dependencies: b.dependencies,
				warnings:     append(b.warnings, model.Error{Code: "42710", Message: fmt.Sprintf("collation %q already exists, skipping", collName)}),
				tag:          "CREATE COLLATION",
				noop:         true,
			}, ctx.Err()
		}
		return nil, bindError("42710", fmt.Sprintf("collation %q already exists", collName), 0)
	} else if !isMissingObject(err) {
		return nil, err
	}

	provider := "libc"
	providerSet := false
	locale := ""
	deterministic := true
	hasFrom := false

	for _, defNode := range statement.Definition {
		def := defNode.GetDefElem()
		if def == nil {
			continue
		}
		switch strings.ToLower(def.Defname) {
		case "from":
			hasFrom = true
			fromParts, err := collationFromArg(def.Arg)
			if err != nil || len(fromParts) == 0 || len(fromParts) > 2 {
				return nil, bindError("42704", "source collation does not exist", 0)
			}
			srcProvider, srcLocale, srcDet, found, err := b.resolveCollationSpec(fromParts)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, bindError("42704", fmt.Sprintf("collation %q does not exist", strings.Join(fromParts, ".")), 0)
			}
			provider, locale, deterministic = srcProvider, srcLocale, srcDet
		case "provider":
			providerSet = true
			p := strings.ToLower(strings.TrimSpace(defElemString(def.Arg)))
			if p != "icu" && p != "builtin" && p != "libc" {
				return nil, bindError("22023", fmt.Sprintf("unrecognized collation provider %q", p), 0)
			}
			if p == "libc" {
				return nil, unsupported(LocaleProviders, int(def.Location)+1)
			}
			provider = p
		case "locale", "lc_collate", "lc_ctype":
			locale = strings.TrimSpace(defElemString(def.Arg))
		case "deterministic":
			deterministic = collationBoolArg(def.Arg)
		case "rules":
			return nil, unsupported(LocaleProviders, int(def.Location)+1)
		case "version":
			// Accept optional version metadata string cleanly.
		default:
			return nil, bindError("42601", fmt.Sprintf("collation option %q not recognized", def.Defname), int(def.Location)+1)
		}
	}

	if !hasFrom && !providerSet {
		return nil, unsupported(LocaleProviders, 0)
	}

	if !hasFrom && locale == "" {
		return nil, bindError("42601", "parameter \"locale\" must be specified", 0)
	}
	if provider == "icu" {
		if _, _, err := builtin.ValidateICULocale(locale); err != nil {
			return nil, err
		}
	} else if provider == "builtin" {
		upper := strings.ToUpper(locale)
		if upper != "C" && upper != "POSIX" && upper != "C.UTF-8" && upper != "C.UTF8" && upper != "PG_UNICODE_FAST" {
			return nil, bindError("22023", fmt.Sprintf("unsupported builtin collation locale %q", locale), 0)
		}
	}

	ownerName := normalizeOwnerName(options.Bind.CurrentRole)
	ownerID := options.Owner
	if ownerID == 0 {
		ownerID = 10
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	providerByte := byte('i')
	if provider == "builtin" {
		providerByte = 'b'
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID:      id,
		OID:     oid,
		Name:    model.NamespaceName{Parent: schemaDesc.Descriptor.ID(), Kind: model.ObjectCollation, Name: collName},
		Version: 1,
		State:   model.StatePublic,
		Collation: &model.CollationMetadata{
			Owner:         ownerID,
			OwnerName:     ownerName,
			Provider:      providerByte,
			Locale:        locale,
			Deterministic: deterministic,
		},
	}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{
		creates:      []*model.Descriptor{descriptor},
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "CREATE COLLATION",
	}, ctx.Err()
}

func bindDropCollation(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.RemoveType != zpgparse.ObjectType_OBJECT_COLLATION {
		return nil, bindError("42601", "missing DROP COLLATION statement", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	var drops []*model.Descriptor
	seen := make(map[model.ObjectID]struct{})
	for _, objNode := range statement.Objects {
		list := objNode.GetList()
		if list == nil {
			return nil, bindError("42601", "invalid collation name in DROP COLLATION", 0)
		}
		parts, err := names(list.Items)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 || len(parts) > 2 {
			return nil, bindError("42601", "invalid collation name in DROP COLLATION", 0)
		}
		collName := parts[len(parts)-1]
		var found *model.Descriptor
		var foundSchema *model.Descriptor
		for _, schema := range b.functionPath(parts) {
			if schema == "pg_catalog" {
				continue
			}
			schemaDesc, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
			if err != nil {
				if isMissingObject(err) {
					continue
				}
				return nil, err
			}
			collDesc, err := snapshot.Resolve(ctx, model.NamespaceName{
				Parent: schemaDesc.Descriptor.ID(),
				Kind:   model.ObjectCollation,
				Name:   collName,
			})
			if err != nil {
				if isMissingObject(err) {
					continue
				}
				return nil, err
			}
			found = collDesc.Descriptor
			foundSchema = schemaDesc.Descriptor
			break
		}
		if found == nil {
			if statement.MissingOk {
				b.warnings = append(b.warnings, model.Error{Code: "42704", Message: fmt.Sprintf("collation %q does not exist, skipping", collName)})
				continue
			}
			return nil, bindError("42704", fmt.Sprintf("collation %q does not exist", collName), 0)
		}
		if err := b.checkSchemaUsage(foundSchema, 0); err != nil {
			return nil, err
		}
		if err := b.requireTableOwner(found, 0); err != nil {
			return nil, err
		}
		if _, dup := seen[found.ID()]; !dup {
			seen[found.ID()] = struct{}{}
			if foundSchema != nil {
				b.dependency(foundSchema, 0)
			}
			b.dependency(found, 0)
			drops = append(drops, found)
		}
	}
	return &BoundObjectCommand{
		drops:        drops,
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "DROP COLLATION",
		cascade:      statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE,
		noop:         len(drops) == 0,
	}, ctx.Err()
}

func (b *binder) resolveCollationSpec(parts []string) (provider, locale string, deterministic, found bool, err error) {
	collName := parts[len(parts)-1]
	for _, schema := range b.functionPath(parts) {
		if schema == "pg_catalog" {
			if c, ok := builtin.LookupCollationName(collName); ok {
				if c.ID == 963 || c.ID == 6411 {
					return "icu", "und", true, true, nil
				}
				return "builtin", c.Name, c.Deterministic, true, nil
			}
			if c, ok := builtin.LookupICUCollationName(collName); ok {
				spec, _ := builtin.LookupICUCollation(c.ID, nil)
				return "icu", spec.Locale, spec.Deterministic, true, nil
			}
			continue
		}
		if b.catalog != nil {
			schemaDesc, rerr := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
			if rerr != nil {
				if isMissingObject(rerr) {
					continue
				}
				return "", "", false, false, rerr
			}
			collDesc, rerr := b.catalog.Resolve(b.ctx, model.NamespaceName{
				Parent: schemaDesc.Descriptor.ID(),
				Kind:   model.ObjectCollation,
				Name:   collName,
			})
			if rerr != nil {
				if isMissingObject(rerr) {
					continue
				}
				return "", "", false, false, rerr
			}
			meta, ok := collDesc.Descriptor.Collation()
			if !ok {
				return "", "", false, false, model.ErrInvalidDescriptor
			}
			b.dependency(collDesc.Descriptor, 0)
			srcProvider := "icu"
			if meta.Provider == 'b' || meta.Provider == 'c' {
				srcProvider = "builtin"
			}
			return srcProvider, meta.Locale, meta.Deterministic, true, nil
		}
	}
	return "", "", false, false, nil
}

func defElemString(arg *zpgparse.Node) string {
	if arg == nil {
		return ""
	}
	if s := arg.GetString_(); s != nil {
		return s.Sval
	}
	if t := arg.GetTypeName(); t != nil {
		if p, err := names(t.Names); err == nil && len(p) > 0 {
			return p[len(p)-1]
		}
	}
	if b := arg.GetBoolean(); b != nil {
		if b.Boolval {
			return "true"
		}
		return "false"
	}
	return ""
}

func collationBoolArg(arg *zpgparse.Node) bool {
	if arg == nil {
		return true
	}
	if b := arg.GetBoolean(); b != nil {
		return b.Boolval
	}
	if i := arg.GetInteger(); i != nil {
		return i.Ival != 0
	}
	s := strings.ToLower(strings.TrimSpace(defElemString(arg)))
	return s != "false" && s != "0" && s != "off" && s != "no"
}

func collationFromArg(arg *zpgparse.Node) ([]string, error) {
	if arg == nil {
		return nil, nil
	}
	if list := arg.GetList(); list != nil {
		return names(list.Items)
	}
	if s := arg.GetString_(); s != nil {
		return []string{s.Sval}, nil
	}
	if t := arg.GetTypeName(); t != nil {
		return names(t.Names)
	}
	return nil, nil
}
