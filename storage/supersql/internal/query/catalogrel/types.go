package catalogrel

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// RelationID identifies a fixed embedded_pg18 logical catalog projection. It is
// independent of SQL OIDs and durable user object identities.
type RelationID = model.CatalogRelationID

type Column struct {
	ID                 model.ColumnID
	Name               string
	Layout             model.TypeLayout
	Nullable           bool
	UnsupportedFeature string
}
type RelationIdentity struct {
	RelationOID model.OID
	RowTypeOID  model.OID
	ArrayTypeOID model.OID
}
type NamespaceIdentity struct {
	ObjectID model.ObjectID
	OID      model.OID
}
type InformationDomainIdentity struct {
	TypeOID       model.OID
	ArrayTypeOID  model.OID
	ConstraintOID model.OID
}
type RegisteredInformationDomain struct {
	Definition builtin.InformationDomainInfo
	Identity   InformationDomainIdentity
	Layout     model.TypeLayout
	Array      model.TypeLayout
}
type RegistryVersion [32]byte
type BoundViewDefinition struct {
	Relation        RelationID
	RegistryVersion RegistryVersion
	Semantic        model.ExpressionDefinition
}
type ViewDefinition struct {
	SQL   string
	Bound BoundViewDefinition
}
type ViewOutputColumn struct {
	Name   string
	Layout model.TypeLayout
}
type RegistryConfig struct {
	InformationSchema          NamespaceIdentity
	Relations                  map[RelationID]RelationIdentity
	InformationDomains        map[builtin.InformationDomainKind]InformationDomainIdentity
	InformationSchemaFunctions map[builtin.InformationSchemaFunction]model.OID
	ViewDefinitions            map[RelationID]BoundViewDefinition
}
type Relation struct {
	ID        RelationID
	Namespace string
	Name      string
	Identity  RelationIdentity
	Columns   []Column
	ViewDefinition ViewDefinition
}

func (r Relation) Clone() Relation {
	r.ViewDefinition.SQL = strings.Clone(r.ViewDefinition.SQL)
	r.ViewDefinition.Bound.Semantic.SQL = strings.Clone(r.ViewDefinition.Bound.Semantic.SQL)
	r.ViewDefinition.Bound.Semantic.Encoding = slices.Clone(r.ViewDefinition.Bound.Semantic.Encoding)
	r.ViewDefinition.Bound.Semantic.Dependencies = slices.Clone(r.ViewDefinition.Bound.Semantic.Dependencies)
	r.Columns = slices.Clone(r.Columns)
	for i := range r.Columns {
		r.Columns[i].Layout = cloneLayout(r.Columns[i].Layout)
	}
	return r
}

func cloneLayout(layout model.TypeLayout) model.TypeLayout {
	layout.FieldNames = slices.Clone(layout.FieldNames)
	layout.Children = slices.Clone(layout.Children)
	for i := range layout.Children {
		layout.Children[i] = cloneLayout(layout.Children[i])
	}
	return layout
}

type ScanRequest struct {
	Relation   RelationID
	Columns    []model.ColumnID
	BatchRows  int
	BatchBytes uint64
}

type Scanner interface {
	OpenCatalog(context.Context, ScanRequest) (storage.BatchCursor, error)
}

type Settings interface {
	CatalogSettings(context.Context) ([]Setting, error)
}

type Setting struct {
	Name             string
	Value            model.Value
	ReadOnly         bool
	Unit             string
	Category         string
	Description      string
	ExtraDescription string
	Context          string
	ValueType        string
	Source           string
	MinValue         string
	MaxValue         string
	EnumValues       []string
	BootValue        string
	ResetValue       string
	SourceFile       string
	SourceLine       *int32
	PendingRestart   bool
}

type SQLFeature struct {
	ID            string
	Name          string
	SubfeatureID  string
	SubfeatureName string
	Supported     bool
	VerifiedBy    string
	Comment       string
}

type FeatureSource interface {
	CatalogFeatures(context.Context) ([]SQLFeature, error)
}

type SQLPart struct {
	ID         string
	Name       string
	Supported  bool
	VerifiedBy string
	Comments   string
}

type SQLSizing struct {
	ID             int32
	Name           string
	SupportedValue *int32
	Comments       string
}

type StandardsSource interface {
	CatalogSQLParts(context.Context) ([]SQLPart, error)
	CatalogSQLSizing(context.Context) ([]SQLSizing, error)
}

type SettingsAttacher interface {
	WithSettings(Settings) Scanner
}

func AttachSettings(scanner Scanner, settings Settings) (Scanner, error) {
	attacher, ok := scanner.(SettingsAttacher)
	if !ok || settings == nil {
		return nil, &model.Error{Code: "55000", Message: "catalog scanner does not accept session settings"}
	}
	return attacher.WithSettings(settings), nil
}

type ReadView interface {
	storage.SnapshotReader
	Timestamp() model.Timestamp
}

type ViewAttacher interface {
	WithCatalogView(ReadView) Scanner
}

func AttachView(scanner Scanner, view ReadView) (Scanner, error) {
	attacher, ok := scanner.(ViewAttacher)
	if !ok || view == nil || view.Timestamp() <= 0 {
		return nil, &model.Error{Code: "55000", Message: "catalog scanner does not accept the statement read view"}
	}
	return attacher.WithCatalogView(view), nil
}

// Snapshot is dedicated to one catalog cursor. Implementations must expose one
// immutable catalog timestamp and return detached descriptors.
type Snapshot interface {
	io.Closer
	Timestamp() model.Timestamp
	Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error)
	ScanNamespace(context.Context, model.ObjectID, model.ObjectKind, func(catalog.PublishedDescriptor) error) error
}

type SnapshotFactory interface {
	OpenCatalogSnapshot(context.Context, ReadView) (Snapshot, error)
}

type SnapshotFactoryFunc func(context.Context, ReadView) (Snapshot, error)

func (f SnapshotFactoryFunc) OpenCatalogSnapshot(ctx context.Context, view ReadView) (Snapshot, error) {
	return f(ctx, view)
}

type CatalogStoreFactory struct {
	Store *catalog.Store
}

func (f CatalogStoreFactory) OpenCatalogSnapshot(_ context.Context, view ReadView) (Snapshot, error) {
	if f.Store == nil {
		return nil, &model.Error{Code: "55000", Message: "catalog store is not configured"}
	}
	return f.Store.TransactionSnapshot(view, catalog.BorrowReader)
}

type Statistics interface {
	Timestamp() model.Timestamp
	Lookup(context.Context, storage.ObjectRef) (*statistics.Table, bool, error)
}

type StatisticsFactory interface {
	OpenCatalogStatistics(context.Context, ReadView) (Statistics, error)
}

type StatisticsStoreFactory struct {
	Store *statistics.Store
}

func (f StatisticsStoreFactory) OpenCatalogStatistics(_ context.Context, view ReadView) (Statistics, error) {
	if f.Store == nil {
		return nil, &model.Error{Code: "55000", Message: "statistics store is not configured"}
	}
	return f.Store.Snapshot(view, view.Timestamp())
}
// SequenceStateLookup reads one incarnation through the statement's pinned
// reader and retains neither the reader nor returned state.
type SequenceStateLookup interface {
	Get(context.Context, schema.JobReader, schema.SequenceIncarnation) (schema.SequenceState, error)
}
var _ SequenceStateLookup = (*schema.SequenceStore)(nil)


type Database struct {
	OID                   model.OID
	Name                  string
	Owner                 model.OID
	Encoding              int32
	Collation             string
	CharacterType         string
	LocaleProvider        byte
	Locale                string
	CollationVersion      string
	CollationOID          model.CollationID
	IsTemplate            bool
	AllowConnections      bool
	HasLoginEventTriggers bool
	ConnectionLimit       int32
	RoleOID               model.OID
	RoleName              string
	RoleSuperuser         bool
	RoleInherit           bool
	RoleCreateRole        bool
	RoleCreateDB          bool
	RoleCanLogin          bool
	RoleReplication       bool
	RoleConnectionLimit   int32
	RoleBypassRLS         bool
}

type SourceOptions struct {
	Snapshot       SnapshotFactory
	Statistics     StatisticsFactory
	Sequences      SequenceStateLookup
	Settings       Settings
	Features       FeatureSource
	Database       Database
	Registry       *Registry
	Standards      StandardsSource
	MaxRows            uint64
	MaxBytes           uint64
	MaxDescriptors     uint64
	MaxDescriptorBytes uint64
	MaxDescriptorItems int
	MaxDescriptorDepth int
}
