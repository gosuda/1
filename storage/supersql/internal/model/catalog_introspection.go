package model

import "context"

// CatalogObjectSummary borrows one immutable descriptor and its namespace only
// during the visitor call. PublishedAt is its publication timestamp in the
// invocation's pinned logical catalog snapshot.
type CatalogObjectSummary struct {
	Descriptor    *Descriptor
	NamespaceOID  OID
	NamespaceName string
	PublishedAt   Timestamp
}

// CatalogIntrospection is a leaf view of one pinned logical catalog. It neither
// owns the snapshot nor permits callers to retain the capability after the
// invocation. Names passed to ResolveCatalogObject are normalized identifier
// components; signature is the canonical routine signature, or empty.
type CatalogIntrospection interface {
	VisitCatalogObject(context.Context, OID, func(CatalogObjectSummary) error) (bool, error)
	ResolveCatalogObject(context.Context, []ObjectKind, []string, string, func(CatalogObjectSummary) error) (bool, error)
	VisitCatalogObjects(context.Context, []ObjectKind, func(CatalogObjectSummary) error) error
	CatalogObjectVisible(context.Context, ObjectKind, OID) (bool, error)
	RelationUpdatability(context.Context, OID, bool) (int32, error)
	ColumnUpdatable(context.Context, OID, int64, bool) (bool, error)
}

// SettingSummary borrows EnumValues only during the visitor call.
type SettingSummary struct {
	Name             string
	Setting          string
	Unit             string
	Category         string
	ShortDescription string
	ExtraDescription string
	Context          string
	ValueType        string
	Source           string
	Minimum          string
	Maximum          string
	EnumValues       []string
	BootValue        string
	ResetValue       string
	SourceFile       string
	SourceLine       int32
	HasSourceLine    bool
	ReadOnly         bool
	PendingRestart   bool
}

// SettingsIntrospection extends the mutation/read capability with a synchronous,
// context-cancellable visit over the session's actual setting state.
type SettingsIntrospection interface {
	SettingsRuntime
	VisitSettings(context.Context, func(SettingSummary) error) error
}

// CatalogForeignKeySummary describes one declared relationship between logical
// catalog projections. Column names are borrowed only during the callback.
type CatalogForeignKeySummary struct {
	ForeignTable    OID
	ForeignColumns  []string
	PrimaryTable    OID
	PrimaryColumns  []string
	ForeignIsArray  bool
	ForeignOptional bool
}

// CatalogMetadataIntrospection exposes static relationships of the active
// logical-catalog projection without exposing catalogrel implementation types.
type CatalogMetadataIntrospection interface {
	VisitCatalogForeignKeys(context.Context, func(CatalogForeignKeySummary) error) error
}

// RoleIntrospection resolves the immutable principals visible to one
// invocation. Returned names remain owned by the runtime.
type RoleIntrospection interface {
	ResolveRole(context.Context, string) (OID, bool, error)
	RoleName(context.Context, OID) (string, bool, error)
}
