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
	remaining := options.MaxInputBytes
	for _, value := range arguments {
		bytes, err := model.ValueBytes(ctx, value, remaining)
		if err != nil || bytes > remaining {
			return nil, ErrLimit
		}
		remaining -= bytes
	}
	call, cancel := context.WithCancel(ctx)
	iterator := &routineRowIterator{cancel: cancel, packets: make(chan routineRowPacket, 1), done: make(chan struct{})}
	strictNull := false
	if plan.strict {
		for _, value := range arguments {
			strictNull = strictNull || value.IsNull
		}
	}
	go func() {
		defer close(iterator.done)
		defer close(iterator.packets)
		if strictNull {
			// A strict set routine yields no rows on NULL input; a strict
			// non-set routine still produces its single NULL result.
			if !plan.set {
				_ = iterator.send(call, routineRowPacket{row: plan.nullRow()})
			}
			return
		}
		if !plan.set {
			iterator.runSingle(call, plan, arguments, options)
			return
		}
		err := plan.program.executeRowsResolved(call, arguments, func(encoded []model.Value) error {
			row, err := plan.splitRow(encoded)
			if err != nil {
				return err
			}
			owned, claim, err := ownRow(call, row, options)
			if err != nil {
				return err
			}
			return iterator.send(call, routineRowPacket{row: owned, claim: claim})
		}, plan.signature)
		if err != nil && call.Err() == nil {
			iterator.send(call, routineRowPacket{err: err})
		}
	}()
	return iterator, nil
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

// ownRow copies a delivered row's payloads and reserves the copy under the
// caller's allocator so a packet owns its bytes independently of the routine
// frame that produced them.
func ownRow(call context.Context, row []model.Value, options builtin.RowSourceOptions) ([]model.Value, columnar.Allocation, error) {
	owned := make([]model.Value, len(row))
	limit := min(options.MaxRowBytes, options.MaxMemoryBytes)
	used := uint64(len(row)) * uint64(unsafe.Sizeof(model.Value{}))
	if used > limit {
		return nil, nil, ErrLimit
	}
	for i, value := range row {
		copy, bytes, err := model.CopyValue(call, value, limit-used)
		if err != nil || bytes > limit-used {
			return nil, nil, ErrLimit
		}
		owned[i], used = copy, used+bytes
	}
	if options.Allocator == nil {
		return owned, nil, nil
	}
	claim, err := options.Allocator.Reserve(used)
	if err != nil {
		return nil, nil, err
	}
	return owned, claim, nil
}

// runSingle executes a non-set routine: PostgreSQL yields the result row once —
// the last row of a multi-row SQL body, or a NULL row when the body produced
// nothing.
func (iterator *routineRowIterator) runSingle(call context.Context, plan *routineRowSourcePlan, arguments []model.Value, options builtin.RowSourceOptions) {
	result, err := plan.program.executeResolved(call, arguments, plan.signature)
	if err != nil {
		var sqlErr *model.Error
		if errors.As(err, &sqlErr) && sqlErr.Code == "42804" && sqlErr.Message == "declared value has wrong storage" {
			err = sqlError("42804", "function return row and query-specified return row do not match", nil)
		}
	}
	var encoded []model.Value
	if err == nil && len(result.Rows) != 0 {
		encoded = result.Rows[len(result.Rows)-1]
	}
	var row []model.Value
	var claim columnar.Allocation
	if err == nil {
		switch {
		case encoded == nil:
			row = plan.nullRow()
		case len(encoded) != 1 && plan.expand:
			// A multi-column body row supplies the record's fields directly.
			row, err = plan.splitRow([]model.Value{{Type: model.TypeComposite, ArrayVal: encoded}})
		default:
			row, err = plan.splitRow(encoded)
		}
	}
	if err == nil {
		row, claim, err = ownRow(call, row, options)
	}
	if err != nil && call.Err() == nil {
		_ = iterator.send(call, routineRowPacket{err: err})
		return
	}
	if row != nil {
		_ = iterator.send(call, routineRowPacket{row: row, claim: claim})
	}
}

// send delivers a packet; call cancellation reports it back to the program so
// an abandoned iterator stops routine execution instead of leaking it.
func (iterator *routineRowIterator) send(call context.Context, packet routineRowPacket) error {
	select {
	case iterator.packets <- packet:
		return nil
	case <-call.Done():
		if packet.claim != nil {
			packet.claim.Release()
		}
		return call.Err()
	}
}

type routineRowPacket struct {
	row   []model.Value
	claim columnar.Allocation
	err   error
}

type routineRowIterator struct {
	cancel  context.CancelFunc
	packets chan routineRowPacket
	done    chan struct{}
	current columnar.Allocation
	closed  bool
}

func (iterator *routineRowIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator.closed {
		return nil, false, io.EOF
	}
	if iterator.current != nil {
		iterator.current.Release()
		iterator.current = nil
	}
	select {
	case packet, ok := <-iterator.packets:
		if !ok {
			return nil, false, nil
		}
		if packet.err != nil {
			return nil, false, packet.err
		}
		iterator.current = packet.claim
		return packet.row, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (iterator *routineRowIterator) Close() error {
	if iterator.closed {
		return nil
	}
	iterator.closed = true
	iterator.cancel()
	if iterator.current != nil {
		iterator.current.Release()
		iterator.current = nil
	}
	for packet := range iterator.packets {
		if packet.claim != nil {
			packet.claim.Release()
		}
	}
	<-iterator.done
	return nil
}

var _ expr.RowSourceResolver = (*Resolver)(nil)
