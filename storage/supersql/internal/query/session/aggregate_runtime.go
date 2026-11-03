package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

// aggregateRuntime resolves SQL-defined aggregates and their support routines
// against one catalog snapshot. It implements exaggregate.AggregateResolver, so
// the executor can run a declared aggregate without importing the catalog
// encoding.
//
// The snapshot is borrowed: the frame that owns it outlives every plan built
// from it, which is the same lifetime the relation and sequence runtimes rely on.
type aggregateRuntime struct {
	snapshot  *catalog.CatalogSnapshot
	functions expr.Resolver
	builtin   builtin.ResolveOptions
}

// newAggregateRuntime builds the resolver for one capability frame.
func newAggregateRuntime(snapshot *catalog.CatalogSnapshot, functions expr.Resolver, builtins builtin.ResolveOptions) *aggregateRuntime {
	return &aggregateRuntime{
		snapshot:  snapshot,
		functions: functions,
		builtin:   builtins,
	}
}

// ResolveAggregate returns the declaration of one SQL-defined aggregate.
// A pinned aggregate identity is reported as absent so the builtin inventory
// remains the single source of truth for builtins.
func (r *aggregateRuntime) ResolveAggregate(ctx context.Context, function model.FunctionID) (exaggregate.AggregateMetadata, bool, error) {
	if r == nil || r.snapshot == nil {
		return exaggregate.AggregateMetadata{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		return exaggregate.AggregateMetadata{}, false, err
	}
	if _, pinned := builtin.LookupSignature(function); pinned {
		return exaggregate.AggregateMetadata{}, false, nil
	}
	descriptor, err := r.snapshot.ResolveOID(ctx, model.OID(function))
	if err != nil || descriptor.Descriptor == nil {
		return exaggregate.AggregateMetadata{}, false, nil
	}
	metadata, ok := descriptor.Descriptor.Aggregate()
	if !ok {
		return exaggregate.AggregateMetadata{}, false, nil
	}
	return exaggregate.AggregateMetadata{
		Owner:            metadata.Owner,
		Kind:             uint8(metadata.Kind),
		Arguments:        metadata.Arguments,
		Result:           metadata.Result,
		DirectArguments:  metadata.DirectArguments,
		Transition:       metadata.Transition,
		Final:            metadata.Final,
		Combine:          metadata.Combine,
		TransitionLayout: metadata.TransitionLayout,
		HasInitialValue:  metadata.HasInitialValue,
		InitialValue:     metadata.InitialValue,
		FinalExtra:       metadata.FinalExtra,
		FinalModify:      uint8(metadata.FinalModify),
	}, true, nil
}

// ResolveFunction resolves one support routine into an executable call through
// the procedural resolver, so the routine runs with the statement's transaction,
// capabilities, cancellation, and resource limits.
func (r *aggregateRuntime) ResolveFunction(id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout) (expr.ScalarCall, error) {
	if r == nil || r.functions == nil {
		return nil, columnar.ErrValueLayout
	}
	return r.functions.Resolve(expr.Function{ID: id, Arguments: arguments, Result: result, Strict: r.FunctionStrict(id)})
}

// FunctionStrict reports whether a routine skips NULL inputs. The pinned
// inventory answers for builtins; a user routine answers from its descriptor.
// A declaration's support routines are resolved once per plan, so this is not a
// hot path and needs no cache.
func (r *aggregateRuntime) FunctionStrict(id model.FunctionID) bool {
	if r == nil || id == 0 {
		return false
	}
	if signature, pinned := builtin.LookupSignature(id); pinned {
		return signature.Strict
	}
	if r.snapshot == nil {
		return false
	}
	descriptor, err := r.snapshot.ResolveOID(context.Background(), model.OID(id))
	if err != nil || descriptor.Descriptor == nil {
		return false
	}
	routine, ok := descriptor.Descriptor.Routine()
	return ok && routine.Strict
}

// InitialValue parses a declared INITCOND into the transition state by invoking
// the state type's own input function, so the value is a real instance of that
// type rather than a textual placeholder. An empty INITCOND is a legitimate
// explicit empty string.
func (r *aggregateRuntime) InitialValue(ctx context.Context, layout model.TypeLayout, text string) (model.Value, error) {
	if layout.Type.ID == 2281 {
		// The `internal` pseudo-type has no SQL input syntax; PostgreSQL allows
		// INITCOND only for states with a textual representation, so declaring one
		// for `internal` is a declaration error rather than an empty value. Storage
		// alone cannot decide this: jsonb, uuid, and arrays share a byte storage
		// class, so the pseudo-type identity is what matters.
		return model.Value{}, &model.Error{Code: "42P13", Message: "initcond is not supported for the internal transition state"}
	}
	if err := layout.Validate(); err != nil {
		return model.Value{}, err
	}
	call, err := r.resolveInput(ctx, layout, text)
	if err != nil {
		return model.Value{}, err
	}
	return call(ctx, []model.Value{{Type: model.TypeString, StrVal: text}}, builtin.CallBudget{OutputBytes: int64(len(text)), MemoryBytes: uint64(len(text)) + 1024})
}

// resolveInput resolves the text input codec of the transition state type using
// the same resolver as ordinary type input, so a declared INITCOND is parsed by
// the state type's real input function (including its typmod contract) rather
// than by a hand-rolled call shape. The codec's callable and expr.ScalarCall
// share an identical underlying signature, so it converts directly.
func (r *aggregateRuntime) resolveInput(ctx context.Context, layout model.TypeLayout, text string) (expr.ScalarCall, error) {
	maximum := uint64(len(text)) + 4096
	codec, err := builtin.ResolveTypeTextCodec(ctx, layout, r.builtin, maximum)
	if err != nil {
		return nil, err
	}
	return expr.ScalarCall(codec.Input), nil
}
