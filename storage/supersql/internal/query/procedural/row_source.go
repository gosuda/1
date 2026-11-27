package procedural

import (
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

type routineRowSourcePlan struct {
	program    *Program
	inputs     []model.TypeLayout
	result     model.TypeLayout
	signature  *resolvedSignature
	strict     bool
	set        bool
	expand     bool
	ownedBytes uint64
}

func (r *Resolver) ResolveRows(function expr.RowSourceFunction) (builtin.RowSourcePlan, error) {
	if function.Context == nil || function.MaximumBytes == 0 {
		return nil, ErrLimit
	}
	if err := function.Context.Err(); err != nil {
		return nil, err
	}
	plan, ok := r.plans[function.ID]
	if !ok {
		if fallback, ok := r.fallback.(expr.RowSourceResolver); ok {
			return fallback.ResolveRows(function)
		}
		return nil, sqlError("42883", "set-returning routine definition is not present in the pinned snapshot", nil)
	}
	result := plan.metadata.Result
	if markerLayout(result) && !model.IsRoutineParameterPseudoTypeID(result.Type.ID) {
		if physical, ok := builtin.BuiltinTypeLayout(result.Type.ID, result.Type.Modifier); ok {
			result = physical
		} else {
			return nil, sqlError("42804", "routine result pseudo-type has no physical layout", nil)
		}
	} else if containsMarkerLayout(result) || result.Type.ID == 2249 && len(result.Children) == 0 {
		// A polymorphic or bare-record result has no fixed shape; the bound
		// relation carries the call-site row shape (OUT parameters or the
		// column definition list).
		result = function.Result
	}
	// Durable layout equality compares the physical shape only: catalog reload
	// strips FieldNames presentation, and call-site aliases rename OUT fields.
	if len(function.Arguments) != len(plan.inputs) || !model.TypeLayoutsEqual(function.Result, result) || function.Strict != plan.metadata.Strict {
		return nil, sqlError("42804", "routine row source does not match its compiled signature", nil)
	}
	for i, declared := range plan.inputs {
		if !matchRoutineInput(declared, function.Arguments[i]) {
			return nil, sqlError("42804", "routine row source does not match its compiled signature", nil)
		}
	}
	base := uint64(unsafe.Sizeof(routineRowSourcePlan{})) + uint64(len(plan.inputs))*uint64(unsafe.Sizeof(model.TypeLayout{}))
	if base > function.MaximumBytes {
		return nil, ErrLimit
	}
	remaining := function.MaximumBytes - base
	inputs := make([]model.TypeLayout, len(plan.inputs))
	for i := range inputs {
		var used uint64
		var err error
		inputs[i], used, err = model.CopyTypeLayout(function.Context, function.Arguments[i], remaining)
		if err != nil || used > remaining {
			return nil, ErrLimit
		}
		remaining -= used
	}
	result, used, err := model.CopyTypeLayout(function.Context, result, remaining)
	if err != nil || used > remaining {
		return nil, ErrLimit
	}
	remaining -= used
	outputs := 0
	for _, parameter := range plan.metadata.Parameters {
		if parameter.Mode == "out" || parameter.Mode == "inout" || parameter.Mode == "table" {
			outputs++
		}
	}
	signature := &resolvedSignature{inputs: inputs, result: result}
	// A composite result spreads across output columns unless one output
	// parameter keeps the composite value in a single named column.
	expand := result.Type.Storage == model.TypeComposite && outputs != 1
	return &routineRowSourcePlan{program: plan.program, inputs: inputs, result: result, signature: signature, strict: plan.metadata.Strict, set: plan.metadata.ReturnsSet, expand: expand, ownedBytes: function.MaximumBytes - remaining}, nil
}

func (plan *routineRowSourcePlan) OwnedBytes() uint64 { return plan.ownedBytes }

func (plan *routineRowSourcePlan) Open(ctx context.Context, arguments []model.Value, options builtin.RowSourceOptions) (builtin.RowSourceIterator, error) {
	if plan == nil || plan.program == nil || len(arguments) != len(plan.inputs) || options.MaxInputBytes == 0 || options.MaxRowBytes == 0 || options.MaxMemoryBytes == 0 {
		return nil, ErrInvalidProgram
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remaining := options.MaxInputBytes
	for _, value := range arguments {
		bytes, err := model.ValueBytes(ctx, value, remaining)
		if err != nil || bytes > remaining {
			return nil, ErrLimit
		}
		remaining -= bytes
	}
	strictNull := false
	if plan.strict {
		for _, value := range arguments {
			strictNull = strictNull || value.IsNull
		}
	}
	if strictNull {
		// A strict set routine yields no rows on NULL input; a strict
		// non-set routine still produces its single NULL result.
		if !plan.set {
			return materializeSingleRow(ctx, plan.nullRow(), options)
		}
		return &routineRowIterator{}, nil
	}
	if !plan.set {
		return plan.openSingle(ctx, arguments, options)
	}
	// Materialize set-returning routine output synchronously before returning
	// (matching PostgreSQL's SFRM_Materialize for PL/pgSQL SRFs). This guarantees
	// the session routine mutex and SECURITY DEFINER / SET state are released
	// before peer SRFs or outer scalar PL/pgSQL calls execute.
	var (
		rows     [][]model.Value
		retained uint64
		claim    columnar.Allocation
	)
	rowHeader := uint64(unsafe.Sizeof([]model.Value{}))
	err := plan.program.executeRowsResolved(ctx, arguments, func(encoded []model.Value) error {
		row, err := plan.splitRow(encoded)
		if err != nil {
			return err
		}
		if rowHeader > options.MaxMemoryBytes || retained > options.MaxMemoryBytes-rowHeader {
			return ErrLimit
		}
		limit := min(options.MaxRowBytes, options.MaxMemoryBytes-retained-rowHeader)
		owned, used, err := ownRowValues(ctx, row, limit)
		if err != nil {
			return err
		}
		nextRetained := retained + rowHeader + used
		if options.Allocator != nil {
			if claim == nil {
				claim, err = options.Allocator.Reserve(nextRetained)
			} else {
				err = claim.Resize(nextRetained)
			}
			if err != nil {
				return err
			}
		}
		retained = nextRetained
		rows = append(rows, owned)
		return nil
	}, plan.signature)
	if err != nil {
		if claim != nil {
			claim.Release()
		}
		for i := range rows {
			clear(rows[i])
		}
		clear(rows)
		return nil, err
	}
	return &routineRowIterator{rows: rows, claim: claim}, nil
}

// splitRow converts one emitted routine value into the relation's output row:
// unexpanded results pass through as one column, composite results split into
// fields, and a NULL composite yields an all-NULL row (a NULL record has no
// field values to read). Field storage must match the declared row shape —
// the non-set path has no emit-time validation to reject a mismatched body.
func (plan *routineRowSourcePlan) splitRow(encoded []model.Value) ([]model.Value, error) {
	mismatch := sqlError("42804", "function return row and query-specified return row do not match", nil)
	if !plan.expand {
		if len(encoded) != 1 || !encoded[0].IsNull && encoded[0].Type != plan.result.Type.Storage {
			return nil, mismatch
		}
		return encoded, nil
	}
	if len(encoded) != 1 || !encoded[0].IsNull && encoded[0].Type != model.TypeComposite {
		return nil, mismatch
	}
	if encoded[0].IsNull {
		return plan.nullRow(), nil
	}
	fields := encoded[0].ArrayVal
	if len(fields) != len(plan.result.Children) {
		return nil, mismatch
	}
	for i, field := range fields {
		if !field.IsNull && field.Type != plan.result.Children[i].Type.Storage {
			return nil, mismatch
		}
	}
	return fields, nil
}

func (plan *routineRowSourcePlan) nullRow() []model.Value {
	if plan.expand {
		row := make([]model.Value, len(plan.result.Children))
		for i := range row {
			row[i] = model.NullVal(plan.result.Children[i].Type.Storage)
		}
		return row
	}
	return []model.Value{model.NullVal(plan.result.Type.Storage)}
}

// ownRowValues copies a delivered row's payloads within limit and reports the
// retained byte count so the materialized iterator owns its bytes independently
// of the routine frame that produced them.
func ownRowValues(ctx context.Context, row []model.Value, limit uint64) ([]model.Value, uint64, error) {
	owned := make([]model.Value, len(row))
	used := uint64(len(row)) * uint64(unsafe.Sizeof(model.Value{}))
	if used > limit {
		return nil, 0, ErrLimit
	}
	for i, value := range row {
		copied, bytes, err := model.CopyValue(ctx, value, limit-used)
		if err != nil || bytes > limit-used {
			return nil, 0, ErrLimit
		}
		owned[i], used = copied, used+bytes
	}
	return owned, used, nil
}

func materializeSingleRow(ctx context.Context, row []model.Value, options builtin.RowSourceOptions) (builtin.RowSourceIterator, error) {
	rowHeader := uint64(unsafe.Sizeof([]model.Value{}))
	if rowHeader > options.MaxMemoryBytes {
		return nil, ErrLimit
	}
	limit := min(options.MaxRowBytes, options.MaxMemoryBytes-rowHeader)
	owned, used, err := ownRowValues(ctx, row, limit)
	if err != nil {
		return nil, err
	}
	var claim columnar.Allocation
	if options.Allocator != nil {
		claim, err = options.Allocator.Reserve(rowHeader + used)
		if err != nil {
			return nil, err
		}
	}
	return &routineRowIterator{rows: [][]model.Value{owned}, claim: claim}, nil
}

// openSingle executes a non-set routine: PostgreSQL yields the result row once —
// the last row of a multi-row SQL body, or a NULL row when the body produced
// nothing.
func (plan *routineRowSourcePlan) openSingle(ctx context.Context, arguments []model.Value, options builtin.RowSourceOptions) (builtin.RowSourceIterator, error) {
	result, err := plan.program.executeResolved(ctx, arguments, plan.signature)
	if err != nil {
		var sqlErr *model.Error
		if errors.As(err, &sqlErr) && sqlErr.Code == "42804" && sqlErr.Message == "declared value has wrong storage" {
			err = sqlError("42804", "function return row and query-specified return row do not match", nil)
		}
		return nil, err
	}
	var encoded []model.Value
	if len(result.Rows) != 0 {
		encoded = result.Rows[len(result.Rows)-1]
	}
	var row []model.Value
	switch {
	case encoded == nil:
		row = plan.nullRow()
	case len(encoded) != 1 && plan.expand:
		// A multi-column body row supplies the record's fields directly.
		row, err = plan.splitRow([]model.Value{{Type: model.TypeComposite, ArrayVal: encoded}})
	default:
		row, err = plan.splitRow(encoded)
	}
	if err != nil {
		return nil, err
	}
	return materializeSingleRow(ctx, row, options)
}

type routineRowIterator struct {
	rows     [][]model.Value
	position int
	claim    columnar.Allocation
	closed   bool
}

func (iterator *routineRowIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator.closed {
		return nil, false, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if iterator.position >= len(iterator.rows) {
		return nil, false, nil
	}
	row := iterator.rows[iterator.position]
	iterator.position++
	return row, true, nil
}

func (iterator *routineRowIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.closed = true
	for i := range iterator.rows {
		clear(iterator.rows[i])
	}
	clear(iterator.rows)
	iterator.rows = nil
	if iterator.claim != nil {
		iterator.claim.Release()
		iterator.claim = nil
	}
	return nil
}

var _ expr.RowSourceResolver = (*Resolver)(nil)
