package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// RowSourcePlan owns immutable type and semantic metadata, never invocation values.
// Open borrows arguments until the returned iterator is closed.
type RowSourcePlan interface {
	OwnedBytes() uint64
	Open(context.Context, []model.Value, RowSourceOptions) (RowSourceIterator, error)
}

// RowSourceIterator returns a borrowed row valid until the next Next or Close.
// Callers retain a pending row across output backpressure without advancing.
// Implementations preflight materialization-time errors before yielding rows.
// Next and Close are serialized.
type RowSourceIterator interface {
	Next(context.Context) ([]model.Value, bool, error)
	Close() error
}

// Runtime capabilities are borrowed for the iterator lifetime. Retained and
// transient allocations stay within MaxMemoryBytes; each yielded row is bounded
// independently by MaxRowBytes.
type RowSourceOptions struct {
	MaxInputBytes  uint64
	MaxRowBytes    uint64
	MaxMemoryBytes uint64
	Allocator      columnar.Allocator
	Temporal       model.TemporalRuntime
	Settings       model.SettingsRuntime
	TextSearch     model.TextSearchRuntime
	Relations            model.RelationRuntime
	Native               model.NativeScalarRuntime
	SessionIntrospection model.SessionIntrospection
	CatalogMetadata      model.CatalogMetadataIntrospection
}
