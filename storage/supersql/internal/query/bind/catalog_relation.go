package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) rangeVar(node *zpgparse.RangeVar, scope *scope) (relation, error) {
	position := int(node.Location) + 1
	if node.Catalogname != "" {
		return relation{}, bindError("0A000", "cross-database reference is not supported", position)
	}
	if node.Schemaname == "" {
		for current := scope; current != nil; current = current.parent {
			if cte, ok := current.ctes[node.Relname]; ok {
				return b.cteConsumer(cte, node.Alias, node.Relname, scope)
			}
		}
		explicitCatalog := false
		for _, schema := range b.options.SearchPath {
			explicitCatalog = explicitCatalog || schema == "pg_catalog"
		}
		if !explicitCatalog {
			if catalog, ok, err := b.catalogRange("pg_catalog", node.Relname, position); err != nil {
				return relation{}, err
			} else if ok {
				return b.catalogRelation(catalog, node, scope, position)
			}
		}
		for _, schema := range b.options.SearchPath {
			if catalog, ok, err := b.catalogRange(schema, node.Relname, position); err != nil {
				return relation{}, err
			} else if ok {
				return b.catalogRelation(catalog, node, scope, position)
			}
			descriptor, namespace, ok, err := b.resolveRangeInSchema(schema, node.Relname)
			if err != nil {
				return relation{}, err
			}
			if ok {
				return b.descriptorRelation(descriptor, namespace, node, scope, position)
			}
		}
		return relation{}, bindError("42P01", "object does not exist", position)
	}
	if catalog, ok, err := b.catalogRange(node.Schemaname, node.Relname, position); err != nil {
		return relation{}, err
	} else if ok {
		return b.catalogRelation(catalog, node, scope, position)
	}
	if node.Schemaname == "pg_catalog" || node.Schemaname == "information_schema" {
		return relation{}, bindError("42P01", "object does not exist", position)
	}
	descriptor, namespace, ok, err := b.resolveRangeInSchema(node.Schemaname, node.Relname)
	if err != nil {
		return relation{}, err
	}
	if !ok {
		return relation{}, bindError("42P01", "object does not exist", position)
	}
	return b.descriptorRelation(descriptor, namespace, node, scope, position)
}

func (b *binder) catalogRange(schema, name string, position int) (catalogrel.Relation, bool, error) {
	if schema != "pg_catalog" && schema != "information_schema" {
		return catalogrel.Relation{}, false, nil
	}
	if !b.options.CatalogRelations.IdentityComplete() {
		if _, known := catalogrel.Lookup(schema, name); known {
			return catalogrel.Relation{}, false, bindError("55000", "logical catalog relation registry is not configured", position)
		}
		if schema == "pg_catalog" {
			if err := ValidateBuiltinRelation(schema+"."+name, position); err != nil {
				return catalogrel.Relation{}, false, err
			}
		}
		return catalogrel.Relation{}, false, nil
	}
	relation, ok := b.options.CatalogRelations.Lookup(schema, name)
	if ok {
		return relation, true, nil
	}
	if schema == "pg_catalog" {
		if err := ValidateBuiltinRelation(schema+"."+name, position); err != nil {
			return catalogrel.Relation{}, false, err
		}
	}
	return catalogrel.Relation{}, false, nil
}

func (b *binder) resolveRangeInSchema(schema, name string) (*model.Descriptor, *model.Descriptor, bool, error) {
	if b.catalog == nil {
		return nil, nil, false, nil
	}
	namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
	if isMissingObject(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView} {
		published, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: name})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, nil, false, err
		}
		if published.Descriptor.State() == model.StatePublic {
			b.dependency(namespace.Descriptor, 0)
			b.dependency(published.Descriptor, 0)
			return published.Descriptor, namespace.Descriptor, true, nil
		}
	}
	return nil, nil, false, nil
}

func (b *binder) scanOccurrence(position int) (model.ScanOccurrence, error) {
	if b.nextScanOccurrence == ^model.ScanOccurrence(0) {
		return 0, bindError("54000", "scan occurrence limit exceeded", position)
	}
	b.nextScanOccurrence++
	return b.nextScanOccurrence, nil
}

func (b *binder) descriptorRelation(descriptor, namespace *model.Descriptor, node *zpgparse.RangeVar, scope *scope, position int) (relation, error) {
	pushed := slices.Clone(b.rangeRowLocks[node])
	delete(b.rangeRowLocks, node)
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return relation{}, err
	}
	if spec.View != nil && descriptor.Kind() == model.ObjectView {
		bound, err := b.view(descriptor, spec.View, node.Alias, scope, position, pushed)
		if err != nil {
			return relation{}, err
		}
		qualifier := relationQualifier{namespace: namespace.Name().Name, name: node.Relname}
		if node.Alias == nil {
			setRelationNamespace(bound.columns, namespace.Name().Name)
		} else {
			qualifier = relationQualifier{name: node.Alias.Aliasname}
		}
		named, err := b.nameRelation(bound, scope, qualifier, descriptor)
		if err != nil {
			return relation{}, err
		}
		for i := range named.namespaces {
			named.namespaces[i].subquery = true
		}
		named.lockTargets = slices.Clone(named.namespaces)
		return named, nil
	}
	if descriptor.Kind() == model.ObjectMaterializedView {
		if spec.View == nil || spec.Table == nil {
			return relation{}, model.ErrInvalidDescriptor
		}
		if !spec.View.Populated {
			return relation{}, &model.Error{Code: "55000", Message: `materialized view "` + node.Relname + `" has not been populated`, Hint: "Use the REFRESH MATERIALIZED VIEW command.", Position: position}
		}
	}
	if spec.Table == nil {
		return relation{}, missing("materialized-view scan", position)
	}
	occurrence, err := b.scanOccurrence(position)
	if err != nil {
		return relation{}, err
	}
	scan, err := b.inheritanceRelation(descriptor, spec.Table, node.Inh, scope, position, occurrence)
	if err != nil {
		return relation{}, err
	}
	columns, err := aliasColumns(scan.columns, node.Alias, node.Relname)
	if err != nil {
		return relation{}, err
	}
	namespaceName := namespace.Name().Name
	if node.Alias == nil {
		setRelationNamespace(columns, namespaceName)
	}
	qualifier := relationQualifier{namespace: namespaceName, name: node.Relname}
	if node.Alias != nil {
		qualifier = relationQualifier{name: node.Alias.Aliasname}
	}
	named, err := b.nameRelation(relation{id: scan.id, columns: columns, occurrence: occurrence}, scope, qualifier, descriptor)
	if err == nil {
		named.lockTargets = named.namespaces
	}
	return named, err
}

func (b *binder) catalogRelation(descriptor catalogrel.Relation, node *zpgparse.RangeVar, scope *scope, position int) (relation, error) {
	if descriptor.Identity.RelationOID == 0 || descriptor.Identity.RowTypeOID == 0 || descriptor.Identity.ArrayTypeOID == 0 || descriptor.Identity.RelationOID == descriptor.Identity.RowTypeOID || descriptor.Identity.RelationOID == descriptor.Identity.ArrayTypeOID || descriptor.Identity.RowTypeOID == descriptor.Identity.ArrayTypeOID {
		return relation{}, bindError("55000", "logical catalog relation type identity is unavailable", position)
	}
	if uint64(len(descriptor.Columns)) > uint64(b.options.Limits.MaxSlots) {
		return relation{}, bindError("54000", "logical catalog column limit exceeded", position)
	}
	columns := make([]column, len(descriptor.Columns))
	for i, descriptorColumn := range descriptor.Columns {
		unsupportedFeature := descriptorColumn.UnsupportedFeature
		unsupportedDetail := ""
		if unsupportedFeature != "" {
			if descriptorColumn.Nullable {
				unsupportedFeature = ""
			} else {
				unsupportedDetail = descriptor.Namespace + "." + descriptor.Name + "." + descriptorColumn.Name
			}
		}
		slot, err := b.slot(scope, descriptorColumn.Layout, descriptorColumn.Nullable, 0, descriptorColumn.ID)
		if err != nil {
			return relation{}, err
		}
		columns[i] = column{
			ResultColumn: ResultColumn{Name: descriptorColumn.Name, Layout: descriptorColumn.Layout, Nullable: descriptorColumn.Nullable, Slot: slot},
			unsupported:  unsupportedFeature, unsupportedDetail: unsupportedDetail,
		}
	}
	columns, err := aliasOwnedColumns(columns, node.Alias, node.Relname)
	if err != nil {
		return relation{}, err
	}
	if node.Alias == nil {
		setRelationNamespace(columns, descriptor.Namespace)
	}
	if b.options.CatalogRelations.Complete() {
		if err := b.catalogDependencies(b.ctx); err != nil {
			return relation{}, err
		}
	}
	effects := ir.Effects{Volatility: ir.Stable}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.CatalogScan, Scope: scope.id, Catalog: descriptor.ID, Span: sourceSpan(position), Effects: effects}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	qualifier := relationQualifier{namespace: descriptor.Namespace, name: node.Relname}
	if node.Alias != nil {
		qualifier = relationQualifier{name: node.Alias.Aliasname}
	}
	named, err := b.nameRelationWithRowType(relation{id: id, columns: columns, effects: effects}, scope, qualifier, nil, model.TypeID(descriptor.Identity.RowTypeOID))
	if err == nil {
		for i := range named.namespaces {
			named.namespaces[i].systemColumns = true
		}
	}
	return named, err
}

func (b *binder) catalogDependencies(ctx context.Context) error {
	if b.catalog == nil {
		return bindError("55000", "catalog snapshot is not configured", 0)
	}
	if b.catalogDependenciesBound {
		return nil
	}
	var schemas []*model.Descriptor
	count := uint64(0)
	if err := b.catalog.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		count++
		if count > uint64(b.options.Limits.MaxOperands) {
			return bindError("54000", "logical catalog dependency limit exceeded", 0)
		}
		b.dependency(schema.Descriptor, 0)
		schemas = append(schemas, schema.Descriptor)
		return nil
	}); err != nil {
		return err
	}
	for _, schema := range schemas {
		for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectType, model.ObjectCompositeRelation} {
			if err := b.catalog.ScanNamespace(ctx, schema.ID(), kind, func(published catalog.PublishedDescriptor) error {
				if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
					return nil
				}
				count++
				if count > uint64(b.options.Limits.MaxOperands) {
					return bindError("54000", "logical catalog dependency limit exceeded", 0)
				}
				b.dependency(published.Descriptor, 0)
				return nil
			}); err != nil {
				return err
			}
		}
	}
	b.catalogDependenciesBound = true
	return nil
}
