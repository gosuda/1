package builtin

import (
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type jsonRecordRows struct {
	plan       *jsonRecordPlan
	options    RowSourceOptions
	arguments  []model.Value
	text       string
	position   int
	row        []model.Value
	inputClaim columnar.Allocation
	rowClaim   columnar.Allocation
	inputBytes uint64
	closed     bool
	temporal   jsonRecordTemporal
}

func (plan *jsonRecordPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (RowSourceIterator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(arguments) != len(plan.argumentTypes) {
		return nil, sqlError("42883", "JSON record argument count does not match signature")
	}
	for i, argument := range arguments {
		if argument.Type != plan.argumentTypes[i].Storage {
			return nil, sqlError("42804", "JSON record argument storage does not match declared type")
		}
	}
	inputBytes := uint64(0)
	for _, argument := range arguments {
		bytes, err := model.ValueBytes(ctx, argument, options.MaxInputBytes)
		if err != nil {
			return nil, jsonRecordValueError(err)
		}
		if bytes > options.MaxInputBytes || inputBytes > math.MaxUint64-bytes {
			return nil, jsonRecordMemoryError()
		}
		inputBytes += bytes
	}
	if inputBytes > options.MaxInputBytes || inputBytes > options.MaxMemoryBytes {
		return nil, jsonRecordMemoryError()
	}
	headerBytes := uint64(unsafe.Sizeof(jsonRecordRows{}))
	if headerBytes > options.MaxMemoryBytes-inputBytes {
		return nil, jsonRecordMemoryError()
	}
	inputBytes += headerBytes
	var claim columnar.Allocation
	if options.Allocator != nil {
		var err error
		claim, err = options.Allocator.Reserve(inputBytes)
		if err != nil {
			return nil, err
		}
	}
	iterator := &jsonRecordRows{plan: plan, options: options, arguments: arguments, inputBytes: inputBytes, inputClaim: claim, temporal: jsonRecordTemporal{runtime: options.Temporal}}
	fail := func(err error) (RowSourceIterator, error) { _ = iterator.Close(); return nil, err }
	input := arguments[plan.jsonArgument]
	iterator.text = input.StrVal
	if input.IsNull && plan.set {
		iterator.position = -1
		return iterator, nil
	}
	if !input.IsNull {
		if plan.binary {
			limit := options.MaxMemoryBytes - inputBytes
			if iterator.inputClaim != nil {
				if err := iterator.inputClaim.Resize(options.MaxMemoryBytes); err != nil {
					return fail(err)
				}
			}
			canonical, err := canonicalJSONB(ctx, iterator.text, int64(min(limit, math.MaxInt64)), false)
			if err != nil {
				return fail(err)
			}
			iterator.text = canonical
			iterator.inputBytes += uint64(len(canonical))
			if iterator.inputClaim != nil {
				if err := iterator.inputClaim.Resize(iterator.inputBytes); err != nil {
					return fail(err)
				}
			}
		} else if _, _, err := scanJSONTextExtract(ctx, iterator.text, &jsonTextSelector{}); err != nil {
			return fail(err)
		}
		if plan.set {
			scanner := jsonTextScanner{text: iterator.text}
			scanner.space(ctx)
			if scanner.peek() != '[' {
				return fail(sqlError("22023", "cannot call JSON recordset function on a non-array"))
			}
			iterator.position = scanner.position + 1
		}
	}
	// PostgreSQL materializes these functions before exposing the first row.
	// Replay only immutable/stable input codecs, retaining no converted rows.
	start := iterator.position
	for {
		_, ok, err := iterator.next(ctx)
		if err != nil {
			return fail(err)
		}
		if !ok {
			break
		}
	}
	iterator.releaseRow()
	iterator.position = start
	return iterator, nil
}

func (iterator *jsonRecordRows) releaseRow() {
	iterator.row = nil
	if iterator.rowClaim != nil {
		iterator.rowClaim.Release()
		iterator.rowClaim = nil
	}
}

func (iterator *jsonRecordRows) Next(ctx context.Context) ([]model.Value, bool, error) {
	row, ok, err := iterator.next(ctx)
	if err != nil || !ok {
		_ = iterator.Close()
	}
	return row, ok, err
}

func (iterator *jsonRecordRows) next(ctx context.Context) ([]model.Value, bool, error) {
	iterator.releaseRow()
	if iterator.closed {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if iterator.position < 0 {
		return nil, false, nil
	}
	plan := iterator.plan
	text := iterator.text
	var kind jsonTextKind
	if plan.set {
		scanner := jsonTextScanner{text: text, position: iterator.position}
		scanner.space(ctx)
		if scanner.err != nil {
			return nil, false, scanner.err
		}
		if scanner.peek() == ']' {
			iterator.position = -1
			return nil, false, scanner.err
		}
		span, err := scanJSONCollectionValue(ctx, &scanner)
		if err != nil {
			return nil, false, err
		}
		kind = span.kind
		if kind != jsonTextObject {
			return nil, false, sqlError("22023", "argument of JSON recordset function must be an array of objects")
		}
		text = text[span.start:span.end]
		scanner.space(ctx)
		if scanner.peek() == ',' {
			scanner.advance(ctx, 1)
		}
		if scanner.err != nil {
			return nil, false, scanner.err
		}
		iterator.position = scanner.position
	} else {
		iterator.position = -1
	}
	memory := iterator.options.MaxMemoryBytes - iterator.inputBytes
	if iterator.options.Allocator != nil {
		claim, err := iterator.options.Allocator.Reserve(memory)
		if err != nil {
			return nil, false, err
		}
		iterator.rowClaim = claim
	}
	columns := len(plan.root.children)
	if plan.valid {
		columns = 1
	}
	headers := uint64(columns) * uint64(unsafe.Sizeof(model.Value{}))
	if headers > memory || headers > iterator.options.MaxRowBytes {
		return nil, false, jsonRecordMemoryError()
	}
	budget := CallBudget{OutputBytes: int64(min(iterator.options.MaxRowBytes, math.MaxInt64)), MemoryBytes: memory, Temporal: &iterator.temporal}
	var value model.Value
	var err error
	if plan.set {
		base := model.NullVal(model.TypeComposite)
		if plan.jsonArgument == 1 {
			base = iterator.arguments[0]
		}
		value, err = plan.root.convert(ctx, text, kind, base, budget)
	} else {
		// The scalar kernel owns its result. The input is already canonicalized
		// here, but the kernel also handles SQL NULL and validity semantics.
		args := [3]model.Value{}
		copy(args[:], iterator.arguments)
		args[plan.jsonArgument].StrVal = text
		value, err = plan.callPrepared(ctx, args[:len(iterator.arguments)], budget)
	}
	if err != nil {
		return nil, false, err
	}
	row := value.ArrayVal
	if plan.valid {
		row = []model.Value{value}
	} else if value.IsNull {
		headers := uint64(len(plan.root.children)) * uint64(unsafe.Sizeof(model.Value{}))
		if headers > memory || headers > iterator.options.MaxRowBytes {
			return nil, false, jsonRecordMemoryError()
		}
		row = make([]model.Value, len(plan.root.children))
		for i := range row {
			row[i] = model.NullVal(plan.root.children[i].typ.Storage)
		}
	}
	bytes, err := model.ValueBytes(ctx, model.Value{Type: model.TypeComposite, ArrayVal: row}, min(memory, iterator.options.MaxRowBytes))
	if err != nil {
		return nil, false, jsonRecordValueError(err)
	}
	iterator.row = row
	if iterator.rowClaim != nil {
		if err := iterator.rowClaim.Resize(bytes); err != nil {
			return nil, false, err
		}
	}
	return row, true, nil
}

func (iterator *jsonRecordRows) Close() error {
	iterator.releaseRow()
	iterator.arguments, iterator.text = nil, ""
	iterator.temporal = jsonRecordTemporal{}
	iterator.plan = nil
	iterator.options = RowSourceOptions{}
	iterator.closed = true
	if iterator.inputClaim != nil {
		iterator.inputClaim.Release()
		iterator.inputClaim = nil
	}
	return nil
}

// Stable timestamp capabilities are read at most once across preflight/replay.
// Volatile subtype input functions are rejected by the conversion resolver.
type jsonRecordTemporal struct {
	runtime                        model.TemporalRuntime
	transaction, statement         model.SQLTimestamp
	transactionErr, statementErr   error
	haveTransaction, haveStatement bool
}

func (runtime *jsonRecordTemporal) TransactionTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	if !runtime.haveTransaction {
		if runtime.runtime == nil {
			return 0, sqlError("55000", "SQL temporal runtime is unavailable")
		}
		runtime.transaction, runtime.transactionErr = runtime.runtime.TransactionTimestamp(ctx)
		runtime.haveTransaction = true
	}
	return runtime.transaction, runtime.transactionErr
}

func (runtime *jsonRecordTemporal) StatementTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	if !runtime.haveStatement {
		if runtime.runtime == nil {
			return 0, sqlError("55000", "SQL temporal runtime is unavailable")
		}
		runtime.statement, runtime.statementErr = runtime.runtime.StatementTimestamp(ctx)
		runtime.haveStatement = true
	}
	return runtime.statement, runtime.statementErr
}

func (runtime *jsonRecordTemporal) ClockTimestamp(context.Context) (model.SQLTimestamp, error) {
	return 0, sqlError("0A000", "volatile clock input is not supported by JSON record conversion")
}
