package bind

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) regclassLiteral(s *scope, e expression, target model.TypeLayout) (expression, error) {
	oid, numeric, err := builtin.ParseRelationOID(b.ctx, e.value.StrVal, b.options.Limits.MaxBytes)
	if err != nil {
		return e, err
	}
	if !numeric {
		parts, err := builtin.ParseQualifiedName(b.ctx, e.value.StrVal, b.options.Limits.MaxBytes)
		if err != nil {
			var sql *model.Error
			if errors.As(err, &sql) && sql.Code == "22023" {
				return e, bindError("42602", "invalid name syntax", e.position)
			}
			return e, err
		}
		catalogOID, isCatalog, err := b.catalogRegclassOID(parts, e.position)
		if err != nil {
			return e, err
		}
		if isCatalog {
			oid = catalogOID
		} else {
			descriptor, namespace, err := b.declarationRelation(parts)
			if err != nil {
				return e, err
			}
			b.dependency(namespace, 0)
			b.dependency(descriptor, 0)
			oid = descriptor.OID()
		}
	}
	if b.rowRegclassLiterals != nil && e.literal != nil {
		if _, exists := b.rowRegclassLiterals[e.literal]; !exists {
			if uint64(len(b.rowRegclassLiterals)) >= uint64(b.options.Limits.MaxOperands) || uint64(len(b.rowRegclassLiterals)+1)*32 > b.options.Limits.MaxBytes {
				return e, model.ErrValueBudget
			}
		}
		b.rowRegclassLiterals[e.literal] = oid
	}
	return b.addScalar(s, expression{typ: target, value: model.Int64Val(int64(oid)), position: e.position}, ir.Scalar{Kind: ir.Const}, nil)
}

// ResolveRelationName borrows parsed identifiers and a pinned catalog snapshot.
// It returns both relation and namespace so binders can record their dependencies.
func ResolveRelationName(ctx context.Context, snapshot CatalogSnapshot, path, parts []string) (*model.Descriptor, *model.Descriptor, error) {
	return resolveNamespaceObject(ctx, snapshot, path, parts, []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation}, "42P01", 0)
}

// catalogRegclassOID resolves a regclass literal name against the logical
// catalog registry. PostgreSQL's regclass accepts every pg_class row, and the
// logical catalog relations are exactly that: pg_constraint, pg_proc and their
// peers carry no stored descriptor, so descriptor resolution alone rejects
// canonical view expressions such as 'pg_catalog.pg_constraint'::regclass.
func (b *binder) catalogRegclassOID(parts []string, position int) (model.OID, bool, error) {
	if len(parts) == 0 || len(parts) > 2 {
		return 0, false, nil
	}
	name := parts[len(parts)-1]
	if len(parts) == 2 {
		return b.catalogRegclassRelation(parts[0], name, position)
	}
	// An unqualified name follows relational resolution: pg_catalog is searched
	// implicitly unless the search path names it explicitly, then each search
	// path entry in order.
	if !slices.Contains(b.options.SearchPath, "pg_catalog") {
		oid, ok, err := b.catalogRegclassRelation("pg_catalog", name, position)
		if err != nil || ok {
			return oid, ok, err
		}
	}
	for _, schema := range b.options.SearchPath {
		oid, ok, err := b.catalogRegclassRelation(schema, name, position)
		if err != nil || ok {
			return oid, ok, err
		}
	}
	return 0, false, nil
}

func (b *binder) catalogRegclassRelation(schema, name string, position int) (model.OID, bool, error) {
	relation, ok, err := b.catalogRange(schema, name, position)
	if err != nil || !ok {
		return 0, false, err
	}
	if relation.Identity.RelationOID == 0 {
		return 0, false, bindError("55000", "logical catalog relation identity is unavailable", position)
	}
	// A sealed registry is the runtime form, where naming a logical relation
	// also protects the catalog inventory read, exactly as a catalog scan does.
	if b.options.CatalogRelations.Complete() {
		if err := b.catalogDependencies(b.ctx); err != nil {
			return 0, false, err
		}
	}
	return relation.Identity.RelationOID, true, nil
}
