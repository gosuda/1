package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/viewmutation"
)

type RelationCatalog interface {
	bind.CatalogSnapshot
	ResolveOID(context.Context, model.OID) (catalog.PublishedDescriptor, error)
}
type relationRuntime struct {
	catalog          RelationCatalog
	catalogRelations *catalogrel.Registry
	path             []string
}

// NewRelationRuntime borrows one pinned catalog and owns the semantic search path.
// Runtime name/OID reads must be covered by the invocation's PCC dependencies.
func NewRelationRuntime(snapshot RelationCatalog, catalogRelations *catalogrel.Registry, path []string, pathBytes uint64) (model.RelationRuntime, error) {
	if historicalNil(snapshot) || pathBytes == 0 {
		return nil, errPreparedInvocation
	}
	header := uint64(unsafe.Sizeof(""))
	if uint64(len(path)) > pathBytes/header {
		return nil, model.ErrValueBudget
	}
	remaining := pathBytes - uint64(len(path))*header
	for _, name := range path {
		if !utf8.ValidString(name) || uint64(len(name)) > remaining {
			return nil, model.ErrValueBudget
		}
		remaining -= uint64(len(name))
	}
	owned := make([]string, len(path))
	for i, name := range path {
		owned[i] = strings.Clone(name)
	}
	return &relationRuntime{catalog: snapshot, catalogRelations: catalogRelations, path: owned}, nil
}
func (r *relationRuntime) CatalogViewDefinitionByOID(ctx context.Context, oid model.OID) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if r.catalogRelations == nil {
		return "", false, nil
	}
	definition, ok := r.catalogRelations.ViewDefinitionByOID(oid)
	return definition.SQL, ok, nil
}

func (r *relationRuntime) CatalogViewDefinitionByName(ctx context.Context, parts []string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if r.catalogRelations == nil {
		return "", false, nil
	}
	switch len(parts) {
	case 1:
		explicitCatalog := false
		for _, namespace := range r.path {
			explicitCatalog = explicitCatalog || namespace == "pg_catalog"
		}
		if !explicitCatalog {
			if sql, found, err := r.viewDefinitionInNamespace(ctx, "pg_catalog", parts[0]); err != nil || found {
				return sql, found, err
			}
		}
		for _, namespace := range r.path {
			if sql, found, err := r.viewDefinitionInNamespace(ctx, namespace, parts[0]); err != nil || found {
				return sql, found, err
			}
		}
	case 2:
		return r.viewDefinitionInNamespace(ctx, parts[0], parts[1])
	}
	return "", false, nil
}

func (r *relationRuntime) viewDefinitionInNamespace(ctx context.Context, namespace, name string) (string, bool, error) {
	if definition, ok := r.catalogRelations.ViewDefinition(namespace, name); ok {
		return definition.SQL, true, nil
	}
	var sql string
	found, err := r.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectView, model.ObjectMaterializedView}, []string{namespace, name}, "", func(summary model.CatalogObjectSummary) error {
		view, ok := summary.Descriptor.View()
		if ok {
			sql = strings.Clone(view.Definition)
		}
		return nil
	})
	return sql, found && sql != "", err
}
func (r *relationRuntime) ResolveRole(ctx context.Context, name string) (model.OID, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	principal := model.Principal{}
	if name != principal.Name() {
		return 0, false, nil
	}
	return principal.OID(), true, nil
}

func (r *relationRuntime) RoleName(ctx context.Context, oid model.OID) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	principal := model.Principal{}
	if oid != principal.OID() {
		return "", false, nil
	}
	return principal.Name(), true, nil
}
func (r *relationRuntime) ResolveRelation(ctx context.Context, text string, maxBytes uint64) (model.OID, error) {
	oid, numeric, err := builtin.ParseRelationOID(ctx, text, maxBytes)
	if err != nil || numeric {
		return oid, err
	}
	parts, err := builtin.ParseQualifiedName(ctx, text, maxBytes)
	if err != nil {
		var sql *model.Error
		if errors.As(err, &sql) && sql.Code == "22023" {
			return 0, &model.Error{Code: "42602", Message: "invalid name syntax", Cause: err}
		}
		return 0, err
	}
	descriptor, _, err := bind.ResolveRelationName(ctx, r.catalog, r.path, parts)
	if err != nil {
		return 0, err
	}
	return descriptor.OID(), nil
}
func relationObject(kind model.ObjectKind) bool {
	switch kind {
	case model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation:
		return true
	}
	return false
}
func numericRelationName(oid model.OID, maxBytes uint64) (string, error) {
	var buffer [10]byte
	text := strconv.AppendUint(buffer[:0], uint64(oid), 10)
	if uint64(len(text)) > maxBytes {
		return "", model.ErrValueBudget
	}
	return string(text), nil
}
func (r *relationRuntime) RelationName(ctx context.Context, oid model.OID, maxBytes uint64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if oid == 0 {
		if maxBytes < 1 {
			return "", model.ErrValueBudget
		}
		return "-", nil
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return numericRelationName(oid, maxBytes)
	}
	if err != nil {
		return "", err
	}
	descriptor := published.Descriptor
	if descriptor == nil {
		return "", &model.Error{Code: "XX000", Message: "relation descriptor is unavailable"}
	}
	if !relationObject(descriptor.Kind()) || descriptor.State() != model.StatePublic {
		return numericRelationName(oid, maxBytes)
	}
	name := descriptor.Name()
	visible, _, err := bind.ResolveRelationName(ctx, r.catalog, r.path, []string{name.Name})
	if err == nil && visible.ID() == descriptor.ID() {
		return builtin.QuoteQualifiedName(ctx, []string{name.Name}, maxBytes)
	}
	if err != nil {
		var sql *model.Error
		if !errors.As(err, &sql) || sql.Code != "42P01" {
			return "", err
		}
	}
	namespace, err := r.catalog.Descriptor(ctx, name.Parent)
	if err != nil {
		return "", err
	}
	if namespace.Descriptor == nil {
		return "", &model.Error{Code: "XX000", Message: "relation namespace descriptor is unavailable"}
	}
	return builtin.QuoteQualifiedName(ctx, []string{namespace.Descriptor.Name().Name, name.Name}, maxBytes)
}
func (r *relationRuntime) InformationSchemaIndexPosition(ctx context.Context, oid model.OID, attribute int16) (int16, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if published.Descriptor.Kind() != model.ObjectIndex || published.Descriptor.State() != model.StatePublic {
		return 0, false, nil
	}
	index, ok := published.Descriptor.Index()
	if !ok {
		return 0, false, nil
	}
	column := model.ColumnID(attribute)
	for position, key := range index.Keys {
		if key.Column == column {
			return int16(position + 1), true, nil
		}
	}
	for offset, include := range index.Include {
		if include == column {
			return int16(len(index.Keys) + offset + 1), true, nil
		}
	}
	return 0, false, nil
}

func (r *relationRuntime) CurrentSchemas(ctx context.Context, includeImplicit bool) ([]string, error) {
	names := make([]string, 0, len(r.path)+1)
	if includeImplicit {
		explicit := false
		for _, name := range r.path {
			explicit = explicit || name == "pg_catalog"
		}
		if !explicit {
			names = append(names, "pg_catalog")
		}
	}
	for _, name := range r.path {
		if _, err := r.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name}); err != nil {
			if errors.Is(err, catalog.ErrUnknownObject) {
				continue
			}
			return nil, err
		}
		names = append(names, strings.Clone(name))
	}
	return names, ctx.Err()
}

func (r *relationRuntime) TemporarySchema(ctx context.Context) (model.OID, bool, error) {
	for _, name := range r.path {
		if !strings.HasPrefix(name, "pg_temp_") {
			continue
		}
		published, err := r.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
		if err != nil {
			return 0, false, err
		}
		return published.Descriptor.OID(), true, nil
	}
	return 0, false, ctx.Err()
}

func (r *relationRuntime) IsOtherTemporarySchema(ctx context.Context, oid model.OID) (bool, error) {
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	name := published.Descriptor.Name().Name
	if published.Descriptor.Kind() != model.ObjectSchema || !strings.HasPrefix(name, "pg_temp_") {
		return false, nil
	}
	current, found, err := r.TemporarySchema(ctx)
	return !found || current != oid, err
}

func (r *relationRuntime) DescribeObject(ctx context.Context, class, object model.OID, subobject int32, maxBytes uint64) (string, error) {
	published, err := r.catalog.ResolveOID(ctx, object)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return fmt.Sprintf("unrecognized object %d %d %d", class, object, subobject), nil
	}
	if err != nil {
		return "", err
	}
	descriptor := published.Descriptor
	text := fmt.Sprintf("%d %s", descriptor.Kind(), descriptor.Name().Name)
	if uint64(len(text)) > maxBytes {
		return "", &model.Error{Code: "54000", Message: "object description exceeds output budget"}
	}
	return text, nil
}

func catalogKindIncluded(kinds []model.ObjectKind, kind model.ObjectKind) bool {
	for _, candidate := range kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func validCatalogKinds(kinds []model.ObjectKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, kind := range kinds {
		if kind < model.ObjectSchema || kind > model.ObjectTextSearchTemplate {
			return false
		}
	}
	return true
}

func (r *relationRuntime) visitCatalogSummary(ctx context.Context, published catalog.PublishedDescriptor, namespace *model.Descriptor, visit func(model.CatalogObjectSummary) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	descriptor := published.Descriptor
	if descriptor == nil || descriptor.State() != model.StatePublic {
		return nil
	}
	summary := model.CatalogObjectSummary{Descriptor: descriptor, PublishedAt: published.PublishedAt}
	if descriptor.Kind() == model.ObjectSchema {
		summary.NamespaceOID = descriptor.OID()
		summary.NamespaceName = descriptor.Name().Name
	} else {
		if namespace == nil {
			parent, err := r.catalog.Descriptor(ctx, descriptor.Name().Parent)
			if err != nil {
				return err
			}
			namespace = parent.Descriptor
		}
		if namespace == nil || namespace.Kind() != model.ObjectSchema || namespace.State() != model.StatePublic {
			return &model.Error{Code: "XX000", Message: "catalog object has no public namespace"}
		}
		summary.NamespaceOID = namespace.OID()
		summary.NamespaceName = namespace.Name().Name
	}
	return visit(summary)
}

func (r *relationRuntime) VisitCatalogObject(ctx context.Context, oid model.OID, visit func(model.CatalogObjectSummary) error) (bool, error) {
	if visit == nil {
		return false, &model.Error{Code: "22023", Message: "catalog object visitor is nil"}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return false, nil
	}
	return true, r.visitCatalogSummary(ctx, published, nil, visit)
}

func (r *relationRuntime) resolveCatalogSchema(ctx context.Context, name string) (catalog.PublishedDescriptor, bool, error) {
	published, err := r.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
	if errors.Is(err, catalog.ErrUnknownObject) {
		return catalog.PublishedDescriptor{}, false, nil
	}
	if err != nil {
		return catalog.PublishedDescriptor{}, false, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return catalog.PublishedDescriptor{}, false, nil
	}
	return published, true, nil
}

func (r *relationRuntime) resolveCatalogRoutine(ctx context.Context, namespace catalog.PublishedDescriptor, name, signature string) (catalog.PublishedDescriptor, bool, error) {
	if signature != "" {
		published, err := r.catalog.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectRoutine, Name: name, Signature: signature})
		if errors.Is(err, catalog.ErrUnknownObject) {
			return catalog.PublishedDescriptor{}, false, nil
		}
		if err != nil {
			return catalog.PublishedDescriptor{}, false, err
		}
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return catalog.PublishedDescriptor{}, false, nil
		}
		return published, true, nil
	}
	var match catalog.PublishedDescriptor
	err := r.catalog.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(candidate catalog.PublishedDescriptor) error {
		if candidate.Descriptor == nil || candidate.Descriptor.State() != model.StatePublic || candidate.Descriptor.Name().Name != name {
			return nil
		}
		if match.Descriptor != nil {
			return &model.Error{Code: "42725", Message: "more than one routine named " + name}
		}
		match = candidate
		return nil
	})
	return match, match.Descriptor != nil, err
}

func (r *relationRuntime) ResolveCatalogObject(ctx context.Context, kinds []model.ObjectKind, parts []string, signature string, visit func(model.CatalogObjectSummary) error) (bool, error) {
	if !validCatalogKinds(kinds) || len(parts) == 0 || len(parts) > 2 || visit == nil {
		return false, &model.Error{Code: "22023", Message: "invalid catalog object lookup"}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if catalogKindIncluded(kinds, model.ObjectSchema) {
		if len(parts) == 1 {
			schema, found, err := r.resolveCatalogSchema(ctx, parts[0])
			if err != nil || !found {
				return false, err
			}
			return true, r.visitCatalogSummary(ctx, schema, nil, visit)
		}
		return false, nil
	}
	path := r.path
	name := parts[0]
	if len(parts) == 2 {
		path = parts[:1]
		name = parts[1]
	}
	for _, schemaName := range path {
		namespace, found, err := r.resolveCatalogSchema(ctx, schemaName)
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		for _, kind := range kinds {
			var published catalog.PublishedDescriptor
			if kind == model.ObjectRoutine {
				published, found, err = r.resolveCatalogRoutine(ctx, namespace, name, signature)
			} else {
				published, err = r.catalog.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: name})
				if errors.Is(err, catalog.ErrUnknownObject) {
					err = nil
					found = false
				} else {
					found = err == nil && published.Descriptor != nil && published.Descriptor.State() == model.StatePublic
				}
			}
			if err != nil {
				return false, err
			}
			if found {
				return true, r.visitCatalogSummary(ctx, published, namespace.Descriptor, visit)
			}
		}
	}
	return false, nil
}

func (r *relationRuntime) VisitCatalogObjects(ctx context.Context, kinds []model.ObjectKind, visit func(model.CatalogObjectSummary) error) error {
	if !validCatalogKinds(kinds) || visit == nil {
		return &model.Error{Code: "22023", Message: "invalid catalog object visitor"}
	}
	return r.catalog.ScanNamespace(ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic {
			return nil
		}
		if catalogKindIncluded(kinds, model.ObjectSchema) {
			if err := r.visitCatalogSummary(ctx, namespace, nil, visit); err != nil {
				return err
			}
		}
		for _, kind := range kinds {
			if kind == model.ObjectSchema {
				continue
			}
			if err := r.catalog.ScanNamespace(ctx, namespace.Descriptor.ID(), kind, func(published catalog.PublishedDescriptor) error {
				return r.visitCatalogSummary(ctx, published, namespace.Descriptor, visit)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *relationRuntime) CatalogObjectVisible(ctx context.Context, kind model.ObjectKind, oid model.OID) (bool, error) {
	if !validCatalogKinds([]model.ObjectKind{kind}) {
		return false, &model.Error{Code: "22023", Message: "invalid catalog object kind"}
	}
	var target model.CatalogObjectSummary
	found, err := r.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		target = summary
		return nil
	})
	if err != nil || !found || target.Descriptor.Kind() != kind {
		return false, err
	}
	name := target.Descriptor.Name()
	var resolved model.ObjectID
	found, err = r.ResolveCatalogObject(ctx, []model.ObjectKind{kind}, []string{name.Name}, name.Signature, func(summary model.CatalogObjectSummary) error {
		resolved = summary.Descriptor.ID()
		return nil
	})
	return found && resolved == target.Descriptor.ID(), err
}
func (r *relationRuntime) RelationUpdatability(ctx context.Context, oid model.OID, includeTriggers bool) (int32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.catalogRelations != nil {
		for _, relation := range r.catalogRelations.All() {
			if relation.Identity.RelationOID == oid {
				return 0, nil
			}
		}
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return 0, nil
	}
	capabilities, err := viewmutation.RelationCapabilities(ctx, r.catalog, published.Descriptor, includeTriggers, nil)
	return int32(capabilities), err
}

func (r *relationRuntime) ColumnUpdatable(ctx context.Context, oid model.OID, attribute int64, includeTriggers bool) (bool, error) {
	if attribute <= 0 {
		return false, nil
	}
	if r.catalogRelations != nil {
		for _, relation := range r.catalogRelations.All() {
			if relation.Identity.RelationOID == oid {
				return false, nil
			}
		}
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return false, nil
	}
	if err != nil || published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return false, err
	}
	var columns []model.ColumnDescriptor
	if table, ok := published.Descriptor.Table(); ok {
		columns = table.Columns
	} else if view, ok := published.Descriptor.View(); ok {
		columns = view.Columns
	} else {
		return false, nil
	}
	if attribute > int64(^uint32(0)) {
		return false, nil
	}
	columnID := model.ColumnID(attribute)
	for _, column := range columns {
		if column.ID != columnID || column.State != model.StatePublic || column.SystemIdentity {
			continue
		}
		capabilities, capabilityErr := viewmutation.RelationCapabilities(ctx, r.catalog, published.Descriptor, includeTriggers, []model.ColumnID{column.ID})
		return capabilities&viewmutation.Update != 0, capabilityErr
	}
	return false, nil
}

var _ model.CatalogIntrospection = (*relationRuntime)(nil)
var _ model.RoleIntrospection = (*relationRuntime)(nil)
