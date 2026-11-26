package builtin

import (
	"context"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonPathRowsPlan struct {
	id       model.FunctionID
	options  ResolveOptions
	metadata uint64
}

func ResolveJSONPathRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	signature, known := LookupSignature(id)
	if !known { return nil, &UnknownFunctionError{ID: id} }
	if id != 1179 && id != 4006 { return nil, &MissingImplementationError{Signature: signature} }
	if len(arguments) != 4 { return nil, sqlError("42883", "jsonb_path_query argument count does not match signature") }
	expected := [4]struct {
		id      model.TypeID
		storage model.DataType
	}{{3802, model.TypeJSONB}, {4072, model.TypeBytes}, {3802, model.TypeJSONB}, {16, model.TypeBoolean}}
	for index, want := range expected {
		argument := arguments[index]
		if argument.Type.ID != want.id || argument.Type.Storage != want.storage || len(argument.Children) != 0 {
			return nil, sqlError("42804", "jsonb_path_query argument identity mismatch")
		}
	}
	if result.Type.ID != 3802 || result.Type.Storage != model.TypeJSONB || len(result.Children) != 0 {
		return nil, sqlError("42804", "jsonb_path_query result must be jsonb")
	}
	metadata := uint64(unsafe.Sizeof(jsonPathRowsPlan{}))
	if metadata > maximum { return nil, sqlError("54000", "builtin type metadata budget exceeded") }
	return &jsonPathRowsPlan{id: id, options: options, metadata: metadata}, nil
}

func (plan *jsonPathRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type jsonPathStreamValue struct {
	text string
	err  error
}

type jsonPathRowsIterator struct {
	cancel context.CancelFunc
	values <-chan jsonPathStreamValue
	done   <-chan struct{}
	claim  columnar.Allocation
	row    [1]model.Value
	once   sync.Once
	closed bool
}

func (plan *jsonPathRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (_ RowSourceIterator, err error) {
	if err := ctx.Err(); err != nil { return nil, err }
	if len(arguments) != 4 { return nil, sqlError("42883", "jsonb_path_query argument count does not match signature") }
	expected := [4]model.DataType{model.TypeJSONB, model.TypeBytes, model.TypeJSONB, model.TypeBoolean}
	var inputBytes uint64
	for index, argument := range arguments {
		if argument.Type != expected[index] { return nil, sqlError("42804", "jsonb_path_query argument storage mismatch") }
		bytes, valueErr := model.ValueBytes(ctx, argument, options.MaxInputBytes)
		if valueErr != nil || bytes > options.MaxInputBytes-inputBytes { return nil, sqlError("54000", "jsonb_path_query input exceeded byte budget") }
		inputBytes += bytes
	}
	iterator := &jsonPathRowsIterator{}
	fail := func(failure error) (RowSourceIterator, error) { _ = iterator.Close(); return nil, failure }
	for _, argument := range arguments { if argument.IsNull { iterator.closed = true; return iterator, nil } }
	if inputBytes > options.MaxInputBytes { return fail(sqlError("54000", "jsonb_path_query input exceeded byte budget")) }
	limit := options.MaxMemoryBytes
	if options.Allocator != nil {
		iterator.claim, err = options.Allocator.Reserve(0)
		if err != nil { return fail(err) }
		limit, err = iterator.claim.GrowUpTo(limit)
		if err != nil { return fail(err) }
	}
	budget := CallBudget{OutputBytes: int64(min(options.MaxRowBytes, uint64(math.MaxInt64))), MemoryBytes: limit, Temporal: options.Temporal}
	planValue, err := DecodeJSONPathBinary(ctx, arguments[1].BytesVal, limit)
	if err != nil { return fail(err) }
	budget, err = budget.After(planValue.OwnedBytes())
	if err != nil { return fail(err) }
	silent := arguments[3].IntVal != 0
	useTZ := plan.id == 1179
	streamContext, cancel := context.WithCancel(ctx)
	values := make(chan jsonPathStreamValue)
	done := make(chan struct{})
	iterator.cancel, iterator.values, iterator.done = cancel, values, done
	go func() {
		defer close(done)
		defer close(values)
		evaluationErr := ExecuteJSONPath(streamContext, arguments[0].StrVal, planValue, arguments[2].StrVal, useTZ, budget, plan.options, func(value string) error {
			select {
			case values <- jsonPathStreamValue{text: value}:
				return nil
			case <-streamContext.Done():
				return streamContext.Err()
			}
		})
		if evaluationErr == nil || silent && isSuppressibleJSONPathError(evaluationErr) { return }
		select {
		case values <- jsonPathStreamValue{err: evaluationErr}:
		case <-streamContext.Done():
		}
	}()
	return iterator, nil
}

func (iterator *jsonPathRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	iterator.row = [1]model.Value{}
	if iterator.closed { return nil, false, nil }
	select {
	case <-ctx.Done():
		_ = iterator.Close()
		return nil, false, ctx.Err()
	case value, open := <-iterator.values:
		if !open {
			_ = iterator.Close()
			return nil, false, nil
		}
		if value.err != nil {
			_ = iterator.Close()
			return nil, false, value.err
		}
		iterator.row[0] = model.Value{Type: model.TypeJSONB, StrVal: value.text}
		return iterator.row[:], true, nil
	}
}

func (iterator *jsonPathRowsIterator) Close() error {
	iterator.once.Do(func() {
		iterator.closed = true
		iterator.row = [1]model.Value{}
		if iterator.cancel != nil { iterator.cancel() }
		if iterator.done != nil { <-iterator.done }
		if iterator.claim != nil { iterator.claim.Release(); iterator.claim = nil }
	})
	return nil
}

var _ RowSourcePlan = (*jsonPathRowsPlan)(nil)
var _ RowSourceIterator = (*jsonPathRowsIterator)(nil)
