package aggregate

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

// AggregateMetadata is one SQL-defined aggregate's declared executable
// identity. It mirrors model.AggregateMetadata without importing the catalog
// encoding into the executor: the session resolves descriptors and supplies
// this value, keeping the aggregate package free of catalog dependencies.
type AggregateMetadata struct {
	Owner           model.OID
	Kind            uint8
	Arguments       []model.TypeLayout
	Result          model.TypeLayout
	DirectArguments uint16

	Transition model.FunctionID
	Final      model.FunctionID
	Combine    model.FunctionID
	Serial     model.FunctionID
	Deserial   model.FunctionID

	// TransitionLayout is the executor's bounded representation of the state.
	// An `internal` (2281) state is carried as bytes, exactly as declared.
	TransitionLayout model.TypeLayout

	// HasInitialValue distinguishes INITCOND = '' from an absent INITCOND.
	HasInitialValue bool
	InitialValue    string

	FinalExtra  bool
	FinalModify uint8
}

// AggregateResolver supplies everything the executor needs to run a SQL-defined
// aggregate: its declaration, its support routines as executable calls, and the
// classification of those routines. The session implements it over the catalog
// and the procedural resolver, so an aggregate's support routines run through
// the same transaction, capability, and cancellation lifecycle as any other
// routine call.
type AggregateResolver interface {
	// ResolveAggregate returns the declaration for a user-defined aggregate.
	// A false second result means the identity is not a SQL-defined aggregate.
	ResolveAggregate(context.Context, model.FunctionID) (AggregateMetadata, bool, error)
	// ResolveFunction resolves one support routine into an executable call.
	ResolveFunction(model.FunctionID, []model.TypeLayout, model.TypeLayout) (expr.ScalarCall, error)
	// FunctionStrict reports whether the routine skips NULL inputs.
	FunctionStrict(model.FunctionID) bool
	// InitialValue parses a declared INITCOND into the transition layout.
	InitialValue(context.Context, model.TypeLayout, string) (model.Value, error)
}

// userAggregatePlan is the compiled execution contract for one declared
// aggregate. Every callable is resolved once at compile time and then invoked
// per group; nothing is resolved during row processing.
type userAggregatePlan struct {
	metadata AggregateMetadata

	transition expr.ScalarCall
	final      expr.ScalarCall

	transitionLayout model.TypeLayout
	resultLayout     model.TypeLayout

	initial    model.Value
	hasInitial bool

	// finalExtra appends the declaration's input argument types after the
	// direct arguments, which is the extra-argument shape PostgreSQL's own
	// support functions declare (percentile_disc_final takes
	// internal, float8, anyelement for exactly this reason).
	finalExtra  bool
	directCount int
	argumentN   int

	strictTransition bool
	strictFinal      bool
}

// userDefinedAggregate is the operation for a declaration that runs its own
// transition and optional final routines.
const userDefinedAggregate operation = 200

// resolveUserAggregate compiles a declaration into an executable plan. The
// resolver must supply a transition function; the final function is optional and
// the transition state is published unchanged when it is absent.
func resolveUserAggregate(ctx context.Context, metadata AggregateMetadata, options *Options) (*userAggregatePlan, error) {
	if options.Aggregates == nil {
		return nil, &UnsupportedError{Function: metadata.Transition, Reason: "no aggregate resolver"}
	}
	if metadata.Transition == 0 {
		return nil, columnar.ErrValueLayout
	}
	plan := &userAggregatePlan{
		metadata:         metadata,
		transitionLayout: metadata.TransitionLayout,
		resultLayout:     metadata.Result,
		finalExtra:       metadata.FinalExtra,
		directCount:      int(metadata.DirectArguments),
		argumentN:        len(metadata.Arguments),
	}
	if err := metadata.TransitionLayout.Validate(); err != nil {
		return nil, err
	}
	if err := metadata.Result.Validate(); err != nil {
		return nil, err
	}
	// The transition function receives the state followed by the aggregated
	// (ORDER BY) arguments; direct arguments reach the final function instead.
	aggregated := metadata.Arguments[min(int(metadata.DirectArguments), len(metadata.Arguments)):]
	transitionArguments := make([]model.TypeLayout, 0, len(aggregated)+1)
	transitionArguments = append(transitionArguments, metadata.TransitionLayout)
	transitionArguments = append(transitionArguments, aggregated...)
	transition, err := options.Aggregates.ResolveFunction(metadata.Transition, transitionArguments, metadata.TransitionLayout)
	if err != nil {
		return nil, err
	}
	plan.transition = transition
	plan.strictTransition = options.Aggregates.FunctionStrict(metadata.Transition)
	if metadata.Final != 0 {
		finalArguments := []model.TypeLayout{metadata.TransitionLayout}
		if metadata.FinalExtra {
			// The extra arguments are the declared arguments themselves: the real
			// direct values first, then placeholders for the aggregated ones.
			finalArguments = append(finalArguments, metadata.Arguments...)
		}
		final, err := options.Aggregates.ResolveFunction(metadata.Final, finalArguments, metadata.Result)
		if err != nil {
			return nil, err
		}
		plan.final = final
		plan.strictFinal = options.Aggregates.FunctionStrict(metadata.Final)
	}
	if metadata.HasInitialValue {
		value, err := options.Aggregates.InitialValue(ctx, metadata.TransitionLayout, metadata.InitialValue)
		if err != nil {
			return nil, err
		}
		plan.initial, plan.hasInitial = value, true
	}
	return plan, nil
}

// initialTransition returns the starting state for one group. PostgreSQL starts
// from INITCOND when the declaration provides one and from a NULL state
// otherwise, passing that NULL to the first transition invocation.
func (p *userAggregatePlan) initialTransition() transition {
	if p.hasInitial {
		return transition{value: p.initial}
	}
	return transition{value: model.NullVal(p.transitionLayout.Type.Storage)}
}

// advance folds one row into the group state. A strict transition function is
// skipped for any NULL input, and a non-strict one receives the NULLs.
func (c *cursor) advanceUserAggregate(ctx context.Context, plan *userAggregatePlan, state *transition, arguments []model.Value) error {
	if state.count == math.MaxInt64 {
		return overflow()
	}
	if plan.strictTransition && !allPresent(arguments) {
		return nil
	}
	budget := builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes * 8}
	inputs := make([]model.Value, 0, len(arguments)+1)
	inputs = append(inputs, state.value)
	inputs = append(inputs, arguments...)
	result, err := plan.transition(ctx, inputs, budget)
	if err != nil {
		return err
	}
	// The transition result is owned by the call; the state must outlive it.
	owned, _, err := model.CopyValue(ctx, result, c.options.MaxRecordBytes)
	if err != nil {
		return err
	}
	state.value = owned
	state.count++
	return nil
}

// finishUserAggregate publishes one group's result, applying FINALFUNC when the
// declaration has one. A NULL state stays NULL unless a non-strict final
// function is declared, exactly as PostgreSQL distinguishes the two.
func (c *cursor) finishUserAggregate(ctx context.Context, plan *userAggregatePlan, state *transition, direct []model.Value) (model.Value, error) {
	if plan.final == nil {
		// The transition state itself is the result; it may legitimately be NULL
		// when every input was skipped.
		if state.value.IsNull {
			return model.NullVal(plan.resultLayout.Type.Storage), nil
		}
		owned, _, err := model.CopyValue(ctx, state.value, c.options.MaxRecordBytes)
		return owned, err
	}
	if plan.strictFinal && state.value.IsNull {
		return model.NullVal(plan.resultLayout.Type.Storage), nil
	}
	budget := builtin.CallBudget{OutputBytes: int64(c.options.MaxRecordBytes), MemoryBytes: c.options.MaxRecordBytes * 8}
	inputs := make([]model.Value, 0, 1+plan.directCount+plan.argumentN)
	inputs = append(inputs, state.value)
	// FINALFUNC_EXTRA passes the direct arguments, then a NULL for each
	// aggregated input argument as a type placeholder. PostgreSQL's own
	// percentile_disc_final(internal, float8, anyelement) is that shape: the real
	// direct fraction followed by a null of the ordered type.
	if plan.finalExtra {
		inputs = append(inputs, direct[:plan.directCount]...)
		for i := 0; i < plan.argumentN-plan.directCount; i++ {
			inputs = append(inputs, model.NullVal(plan.metadata.Arguments[plan.directCount+i].Type.Storage))
		}
	}
	result, err := plan.final(ctx, inputs, budget)
	if err != nil {
		return model.Value{}, err
	}
	return result, nil
}

// pinnedSupportOperation maps a declared (transition, final) support pair onto the
// pinned aggregate whose operation the executor already implements. A SQL
// declaration that names PostgreSQL's own ordered-set or range support routines
// is describing exactly one of those aggregates, so it must run on that same
// machinery rather than a reimplementation. The returned identity is the pinned
// aggregate the existing compiler switches on.
func pinnedSupportOperation(transition, final model.FunctionID, input model.TypeLayout) (model.FunctionID, bool) {
	switch transition {
	case 3970, 3971:
		switch final {
		case 3973:
			return 3972, true
		case 3975, 3977:
			return 3974, true
		case 3979:
			return 3978, true
		case 3981, 3983:
			return 3980, true
		case 3985:
			return 3984, true
		case 3987:
			return 3986, true
		case 3989:
			return 3988, true
		case 3991:
			return 3990, true
		case 3993:
			return 3992, true
		}
	case 4299:
		if final == 4300 {
			return 4301, true
		}
	case 6225:
		if final == 6226 {
			return 6227, true
		}
	case 4401:
		// range_intersect_agg has no final function: its transition state is the
		// result published unchanged. Two pinned aggregates share this transition
		// and differ only by input type — 4389 takes anymultirange and 4450 takes
		// anyrange — so the declared input decides which one is meant.
		if final == 0 {
			if input.Type.ID == 4537 {
				return 4389, true
			}
			return 4450, true
		}
	}
	return 0, false
}

// compileUserAggregate compiles one SQL-defined aggregate. A declaration built on
// PostgreSQL's own ordered-set or range support routines is routed to that
// pinned aggregate's operation; every other declaration runs its declared
// transition and final routines directly.
func compileUserAggregate(ctx context.Context, function model.FunctionID, call Call, outputLayout model.TypeLayout, layouts []model.TypeLayout, options *Options) (compiledCall, error) {
	metadata, found, err := options.Aggregates.ResolveAggregate(ctx, function)
	if err != nil {
		return compiledCall{}, err
	}
	if !found {
		return compiledCall{}, &UnsupportedError{Function: function, Reason: "no such aggregate"}
	}
	var input model.TypeLayout
	if len(metadata.Arguments) != 0 {
		input = metadata.Arguments[0]
	}
	if pinned, ok := pinnedSupportOperation(metadata.Transition, metadata.Final, input); ok {
		// Reuse the pinned operation by compiling the call under the pinned
		// aggregate's identity; only the identity selects the machinery, so the
		// user's own argument and result layouts are still validated below.
		substituted := call
		substituted.Function = pinned
		compiled, err := compileCall(ctx, substituted, outputLayout, layouts, options)
		if err != nil {
			return compiledCall{}, err
		}
		return compiled, nil
	}
	plan, err := resolveUserAggregate(ctx, metadata, options)
	if err != nil {
		return compiledCall{}, err
	}
	// Direct arguments are the declaration's leading arguments; they are
	// evaluated per group so the final function can receive them.
	if len(call.DirectLayouts) != int(metadata.DirectArguments) || call.DirectEvaluator == nil && len(call.DirectLayouts) != 0 {
		return compiledCall{}, columnar.ErrValueLayout
	}
	result := newCompiledCall(len(call.Arguments), outputLayout.Type)
	result.op = userDefinedAggregate
	result.user = plan
	result.directCount = len(call.DirectLayouts)
	result.directEvaluator = call.DirectEvaluator
	// The declared result must be the layout the plan produces. A declaration
	// whose final function returns a polymorphic type (anyelement, anyarray) has
	// no single declared layout, so the planner's concrete output is authoritative
	// and only storage compatibility is required.
	if !polymorphicResult(plan.resultLayout) {
		if !model.TypeLayoutsEqual(outputLayout, plan.resultLayout) {
			return compiledCall{}, columnar.ErrValueLayout
		}
	} else if outputLayout.Type.Storage != plan.resultLayout.Type.Storage {
		return compiledCall{}, columnar.ErrValueLayout
	}
	// Every aggregated argument is the declared type, or a concrete
	// instantiation of a polymorphic declaration. The call carries only the
	// aggregated arguments; the direct ones travel in DirectLayouts.
	if len(call.Arguments) != len(metadata.Arguments)-int(metadata.DirectArguments) {
		return compiledCall{}, columnar.ErrValueLayout
	}
	for i, column := range call.Arguments {
		if column < 0 || column >= len(layouts) {
			return compiledCall{}, columnar.ErrValueLayout
		}
		declared := metadata.Arguments[int(metadata.DirectArguments)+i]
		if declared.Type.Storage != model.TypeAny && !sameDeclaredLayout(declared, layouts[column]) {
			return compiledCall{}, columnar.ErrValueLayout
		}
		result.arguments[i], result.inputs[i] = column, layouts[column].Type
	}
	if call.FilterColumn != nil {
		column := *call.FilterColumn
		if column < 0 || column >= len(layouts) || layouts[column].Type.ID != 16 || layouts[column].Type.Storage != model.TypeBoolean {
			return compiledCall{}, columnar.ErrValueLayout
		}
		result.filter = column
	}
	// DISTINCT and ORDER BY reach the declared aggregate through the same
	// qualification and sort machinery a pinned aggregate uses: the executor
	// keys DISTINCT on the grouping columns plus the call's arguments and sorts
	// by the call's ORDER BY specification, both of which are op-agnostic. A
	// declaration therefore supports the modifiers PostgreSQL allows on an
	// ordinary aggregate rather than silently ignoring them.
	if err := applyAggregateCallModifiers(ctx, call, &result, layouts, options); err != nil {
		return compiledCall{}, err
	}
	// Complete aggregation is always executable for a declared aggregate: it runs
	// the declaration's own transition and, when declared, its final routine over
	// each group with no merge step. A declaration that carries COMBINE metadata
	// is executable here too — the metadata is unused because complete
	// aggregation never merges partial states.
	//
	// The planner does not offer a decomposed alternative for a declared
	// aggregate, because the combine contract is recognized by pinned identity
	// only, so a non-complete phase reaching this executor is an internal
	// inconsistency rather than a SQL-level feature exclusion.
	if options.Phase != Complete {
		return compiledCall{}, &UnsupportedError{Function: function, Reason: "internal aggregate plan without a proven merge contract"}
	}
	return result, nil
}

// polymorphicResult reports whether a declared result is a polymorphic
// pseudo-type, whose concrete layout only the planner can supply. The set is the
// binder's polymorphic type family, which a declared aggregate may legally
// return through a polymorphic final function.
func polymorphicResult(layout model.TypeLayout) bool {
	switch layout.Type.ID {
	case 2276, 2277, 2283, 2776, 3500, 3831, 4537, 4538, 5077, 5078, 5079, 5080:
		return true
	}
	return layout.Type.Storage == model.TypeAny
}

// sameDeclaredLayout compares a declared argument layout with the concrete one
// the planner supplied, ignoring modifiers a concrete instantiation may refine.
func sameDeclaredLayout(declared, actual model.TypeLayout) bool {
	if declared.Type.ID != actual.Type.ID || declared.Type.Storage != actual.Type.Storage {
		return false
	}
	return len(declared.Children) == len(actual.Children)
}

func allPresent(values []model.Value) bool {
	for _, value := range values {
		if value.IsNull {
			return false
		}
	}
	return true
}
