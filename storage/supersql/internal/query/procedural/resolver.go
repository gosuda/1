package procedural

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

type Definition struct {
	ID       model.FunctionID
	Object   model.ObjectID
	Metadata model.RoutineMetadata
	Name     string
}

type routinePlan struct {
	metadata model.RoutineMetadata
	program  *Program
	inputs   []model.TypeLayout
}

// Resolver is immutable after construction and safe for concurrent expression
// states. Definitions must come from the statement's pinned catalog snapshot.
type Resolver struct {
	plans    map[model.FunctionID]routinePlan
	fallback expr.Resolver
	objects  map[model.ObjectID]model.FunctionID
}

func NewResolver(ctx context.Context, definitions []Definition, fallback expr.Resolver, limits Limits) (*Resolver, error) {
	resolver := &Resolver{plans: make(map[model.FunctionID]routinePlan, len(definitions)), objects: make(map[model.ObjectID]model.FunctionID, len(definitions)), fallback: fallback}
	for _, definition := range definitions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if definition.ID == 0 {
			return nil, ErrInvalidProgram
		}
		if _, exists := resolver.plans[definition.ID]; exists {
			return nil, ErrInvalidProgram
		}
		program, err := Decode(ctx, definition.Metadata.Body, limits)
		if err != nil {
			return nil, err
		}
		if program.procedure != definition.Metadata.Procedure || program.atomic != definition.Metadata.Atomic || program.returnsSet != definition.Metadata.ReturnsSet || !model.TypeLayoutsEqual(program.result, definition.Metadata.Result) {
			return nil, ErrInvalidProgram
		}
		program.settings = slices.Clone(definition.Metadata.Settings)
		program.securityDefiner = definition.Metadata.SecurityDefiner
		program.routineOID = model.OID(definition.ID)
		program.routineName = definition.Name
		plan := routinePlan{metadata: definition.Metadata, program: program}
		for _, parameter := range definition.Metadata.Parameters {
			if parameter.Mode == "in" || parameter.Mode == "inout" || parameter.Mode == "variadic" {
				plan.inputs = append(plan.inputs, parameter.Layout)
			}
		}
		resolver.plans[definition.ID] = plan
		if definition.Object != 0 {
			resolver.objects[definition.Object] = definition.ID
		}
	}
	return resolver, nil
}

func (r *Resolver) Resolve(function expr.Function) (expr.ScalarCall, error) {
	plan, ok := r.plans[function.ID]
	if !ok {
		if r.fallback != nil {
			return r.fallback.Resolve(function)
		}
		return nil, sqlError("42883", "routine definition is not present in the pinned snapshot", nil)
	}
	result := plan.metadata.Result
	if markerLayout(result) && !model.IsRoutineParameterPseudoTypeID(result.Type.ID) {
		// Result-only pseudo-types (void, trigger, ...) bind calls with the
		// type's primitive storage convention instead.
		physical, ok := builtin.BuiltinTypeLayout(result.Type.ID, result.Type.Modifier)
		if !ok {
			return nil, sqlError("42804", "routine result pseudo-type has no physical layout", nil)
		}
		result = physical
	} else if containsMarkerLayout(result) {
		// A polymorphic result marker — or a record result embedding marker
		// children — has no physical layout of its own; the bound call's
		// resolved result is the inferred actual type.
		result = function.Result
	}
	if plan.metadata.ReturnsSet || len(function.Arguments) != len(plan.inputs) || !model.TypeLayoutsEqual(function.Result, result) || function.Strict != plan.metadata.Strict {
		return nil, sqlError("42804", "routine call does not match its compiled scalar signature", nil)
	}
	for i, declared := range plan.inputs {
		if !matchRoutineInput(declared, function.Arguments[i]) {
			return nil, sqlError("42804", "routine call does not match its compiled scalar signature", nil)
		}
	}
	signature := &resolvedSignature{inputs: function.Arguments, result: result}
	return func(ctx context.Context, arguments []model.Value, budget builtin.CallBudget) (model.Value, error) {
		if len(arguments) != len(plan.inputs) {
			return model.Value{}, ErrInvalidProgram
		}
		result, err := plan.program.executeResolved(ctx, arguments, signature)
		if err != nil {
			return model.Value{}, err
		}
		if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
			return model.Value{}, sqlError("21000", "scalar function did not return exactly one value", nil)
		}
		value := result.Rows[0][0]
		limit := budget.MemoryBytes
		if budget.OutputBytes < 0 {
			return model.Value{}, ErrLimit
		}
		limit = min(limit, uint64(budget.OutputBytes))
		if budget.Allocator != nil {
			limit = min(limit, budget.Allocator.AvailableBytes())
		}
		owned, _, err := model.CopyValue(ctx, value, limit)
		if err != nil {
			return model.Value{}, sqlError("54000", "routine result exceeds invocation budget", err)
		}
		return owned, nil
	}, nil
}

func (r *Resolver) Invoke(ctx context.Context, id model.FunctionID, arguments []model.Value) (SQLResult, error) {
	plan, ok := r.plans[id]
	if !ok {
		return SQLResult{}, sqlError("42883", "routine definition is not present in the pinned snapshot", nil)
	}
	return plan.program.Execute(ctx, arguments)
}

func (r *Resolver) InvokeTrigger(ctx context.Context, id model.FunctionID, trigger TriggerContext) (*model.Row, bool, error) {
	plan, ok := r.plans[id]
	if !ok || plan.metadata.Procedure || plan.metadata.ReturnsSet || len(plan.inputs) != 0 {
		return nil, false, sqlError("42883", "trigger routine definition is unavailable or incompatible", nil)
	}
	return plan.program.ExecuteTrigger(ctx, trigger)
}

func matchRoutineInput(declared, actual model.TypeLayout) bool {
	if markerLayout(declared) {
		// A polymorphic parameter marker admits the layout the call site
		// inferred; cross-position consistency was enforced at bind time.
		return actual.Type.ID != 0
	}
	return model.TypeLayoutsEqual(declared, actual)
}

