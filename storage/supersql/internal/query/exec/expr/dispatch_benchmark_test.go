package expr_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type dispatchWorkload struct{ name, expression, sqlType string }
type dispatchPacket struct {
	vector columnar.Vector
	owner  *columnar.Builder
	prefix []uint64
}
type dispatchFixture struct {
	graph    *ir.Graph
	root     ir.ScalarID
	options  expr.Options
	packets  [5]dispatchPacket
	checksum uint64
}

func BenchmarkExpressionDispatch(b *testing.B) {
	for _, workload := range []dispatchWorkload{
		{"arithmetic", "(x+7)*3-x/2", "bigint"},
		{"filter", "x%7<3 AND x>=5", "bigint"},
		{"case", "CASE WHEN x=0 THEN 0 ELSE 100/x END", "bigint"},
		{"numeric", "x*1.25::numeric+2.5::numeric", "numeric"},
		{"text", "x||':suffix:'||x", "text"},
	} {
		for _, rows := range []int{1, 1024, 1000000} {
			for _, width := range []int{1, 256, 1024, 2048} {
				for _, nullPercent := range []int{0, 20} {
					name := fmt.Sprintf("%s/rows=%d/width=%d/null=%d", workload.name, rows, width, nullPercent)
					b.Run(name, func(b *testing.B) {
						fixture := newDispatchFixture(b, workload, min(rows, width), width, nullPercent)
						for _, lifecycle := range []string{"prepared", "prepare_execute"} {
							b.Run(lifecycle, func(b *testing.B) {
								b.Run("direct", func(b *testing.B) { benchmarkDispatch(b, fixture, rows, lifecycle == "prepare_execute", expr.Direct) })
								b.Run("treewalk", func(b *testing.B) { benchmarkTreewalk(b, fixture, rows, lifecycle == "prepare_execute") })
							})
						}
					})
				}
			}
		}
	}
}

func newDispatchFixture(b *testing.B, workload dispatchWorkload, rows, width, nullPercent int) *dispatchFixture {
	b.Helper()
	sql := fmt.Sprintf("SELECT %s FROM (VALUES(NULL::%s)) v(x)", workload.expression, workload.sqlType)
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		b.Fatal(err)
	}
	limits := ir.Limits{MaxNodes: 4096, MaxOperands: 16384, MaxSlots: 256, MaxDepth: 64, MaxBytes: 8 << 20, SourceBytes: uint32(len(sql))}
	bound, err := bind.Bind(b.Context(), parsed.Stmts[0], nil, bind.Options{Limits: limits, Collation: 950, DefaultCollation: 950}, nil)
	if err != nil {
		b.Fatal(err)
	}
	graph := bound.Graph()
	input, ok := graph.Input(graph.Root(), 0)
	if !ok {
		b.Fatal("benchmark requires one input relation")
	}
	slot, ok := graph.Output(input, 0)
	if !ok {
		b.Fatal("benchmark requires one input column")
	}
	info, _ := graph.Slot(slot)
	typ, _ := graph.Type(info.Layout)
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		b.Fatal("benchmark requires one projected expression")
	}
	options := expr.Options{Limits: expr.DefaultLimits(), Inputs: []expr.Input{{Slot: slot, Column: 0}}, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}}
	options.Limits.MaxRows = width
	fixture := &dispatchFixture{graph: graph, root: binding.Expr, options: options}
	for phase := range fixture.packets {
		owner, err := columnar.NewBuilder([]model.TypeLayout{{Type: typ}}, columnar.BuilderOptions{MaxRows: rows, MaxBytes: 1 << 20})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() {
			if err := owner.Close(); err != nil {
				b.Error(err)
			}
		})
		prefix := make([]uint64, rows+1)
		for row := range rows {
			number := int64((row + phase*17) % 128)
			input, expected := dispatchValues(workload.name, number)
			if nullPercent != 0 && (row+phase)%5 == 0 {
				input = model.NullVal(input.Type)
				expected = model.NullVal(expected.Type)
			}
			if err := owner.Append(b.Context(), []model.Value{input}); err != nil {
				b.Fatal(err)
			}
			prefix[row+1] = prefix[row] + dispatchValueHash(expected)
		}
		var batch columnar.Batch
		if err := owner.Publish(&batch); err != nil {
			b.Fatal(err)
		}
		fixture.packets[phase] = dispatchPacket{vector: batch.Columns[0], owner: owner, prefix: prefix}
		fixture.checksum += prefix[rows]
	}
	return fixture
}

func dispatchValues(workload string, number int64) (model.Value, model.Value) {
	input := model.Int64Val(number)
	switch workload {
	case "arithmetic":
		return input, model.Int64Val((number+7)*3 - number/2)
	case "filter":
		return input, model.BoolVal(number%7 < 3 && number >= 5)
	case "case":
		if number == 0 {
			return input, model.Int64Val(0)
		}
		return input, model.Int64Val(100 / number)
	case "numeric":
		numerator := 12500*number + 28125
		return model.Value{Type: model.TypeNumeric, StrVal: fmt.Sprintf("%d.25", number)}, model.Value{Type: model.TypeNumeric, StrVal: fmt.Sprintf("%d.%04d", numerator/10000, numerator%10000)}
	case "text":
		text := fmt.Sprintf("row-%04d-abcdefghijklmnopqrstuv", number)
		return model.StringVal(text), model.StringVal(text + ":suffix:" + text)
	default:
		panic("unknown benchmark workload")
	}
}

func dispatchValueHash(value model.Value) uint64 {
	if value.IsNull {
		return 0x9e3779b97f4a7c15
	}
	if value.Type == model.TypeInt64 || value.Type == model.TypeBoolean {
		return uint64(value.IntVal) * 0xbf58476d1ce4e5b9
	}
	hash := uint64(1469598103934665603)
	for i := range len(value.StrVal) {
		hash = (hash ^ uint64(value.StrVal[i])) * 1099511628211
	}
	return hash
}

func dispatchOutputHash(batch *columnar.Batch) uint64 {
	vector := &batch.Columns[0]
	var sum uint64
	for row := range batch.Rows() {
		lane := batch.Lane(row)
		if vector.IsNull(lane) {
			sum += 0x9e3779b97f4a7c15
			continue
		}
		index := vector.ValueIndex(lane)
		if vector.Type.Storage == model.TypeInt64 || vector.Type.Storage == model.TypeBoolean {
			sum += uint64(vector.Int64[index]) * 0xbf58476d1ce4e5b9
			continue
		}
		range_ := vector.Ranges[index]
		hash := uint64(1469598103934665603)
		for _, value := range vector.Data[range_.Offset : range_.Offset+range_.Length] {
			hash = (hash ^ uint64(value)) * 1099511628211
		}
		sum += hash
	}
	return sum
}

func benchmarkDispatch(b *testing.B, fixture *dispatchFixture, rows int, prepare bool, mode expr.Mode) {
	b.Helper()
	compile := func() *expr.Program {
		program, err := expr.Compile(b.Context(), fixture.graph, []ir.ScalarID{fixture.root}, fixture.options)
		if err != nil {
			b.Fatal(err)
		}
		return program
	}
	program := compile()
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if state != nil {
			if err := state.Close(); err != nil {
				b.Error(err)
			}
		}
	})
	var inputColumns [1]columnar.Vector
	input := columnar.Batch{Columns: inputColumns[:]}
	output := columnar.Batch{Columns: make([]columnar.Vector, 0, 1)}
	for _, packet := range fixture.packets {
		inputColumns[0] = packet.vector
		input.Length = packet.vector.Length
		if err := state.Eval(b.Context(), &input, nil, &output, mode); err != nil {
			b.Fatal(err)
		}
		if got := dispatchOutputHash(&output); got != packet.prefix[input.Length] {
			b.Fatalf("warm-up checksum=%016x, want %016x", got, packet.prefix[input.Length])
		}
	}
	output.Reset()
	if err := state.Close(); err != nil {
		b.Fatal(err)
	}
	state, err = expr.NewState(program, expr.StateOptions{})
	if err != nil {
		b.Fatal(err)
	}
	// Both modes start with identical live fixture/program ownership. Retention
	// metrics are process-heap deltas after state Close and GC, not allocation totals.
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if prepare {
			if err := state.Close(); err != nil {
				b.Fatal(err)
			}
			program = compile()
			state, err = expr.NewState(program, expr.StateOptions{})
			if err != nil {
				b.Fatal(err)
			}
		}
		remaining, phase := rows, iteration%len(fixture.packets)
		var actual, expected uint64
		for remaining > 0 {
			packet := &fixture.packets[phase]
			take := min(remaining, packet.vector.Length)
			inputColumns[0] = packet.vector
			inputColumns[0].Length = take
			input.Length = take
			if err := state.Eval(b.Context(), &input, nil, &output, mode); err != nil {
				b.Fatal(err)
			}
			actual += dispatchOutputHash(&output)
			expected += packet.prefix[take]
			remaining -= take
			phase = (phase + 1) % len(fixture.packets)
		}
		if actual != expected {
			b.Fatalf("dispatch checksum=%016x, want %016x", actual, expected)
		}
	}
	b.StopTimer()
	output.Reset()
	if err := state.Close(); err != nil {
		b.Fatal(err)
	}
	state = nil
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(fixture)
	runtime.KeepAlive(program)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-B")
	b.ReportMetric(float64(int64(after.HeapObjects)-int64(before.HeapObjects)), "retained-objects")
	b.ReportMetric(float64(fixture.checksum&((1<<53)-1)), "checksum")
	b.ReportMetric(float64(rows), "rows/op")
}

// treeEvaluator is the tree-walking baseline: it resolves each call's builtin
// kernel once, then walks the bound scalar graph recursively per row with no
// instruction scheduling, register allocation, or lane masking.
type treeEvaluator struct {
	graph   *ir.Graph
	root    ir.ScalarID
	columns map[model.SlotID]int
	calls   map[ir.ScalarID]builtin.Callable
	consts  map[ir.ScalarID]model.Value
	budget  builtin.CallBudget
	input   *columnar.Vector
}

var errTreeWalkUnsupported = fmt.Errorf("treewalk evaluator does not cover this scalar kind")

func treeLayout(graph *ir.Graph, id ir.LayoutID, depth int) (model.TypeLayout, bool) {
	typ, ok := graph.Type(id)
	if !ok || depth > 64 {
		return model.TypeLayout{}, false
	}
	children := make([]model.TypeLayout, graph.LayoutChildCount(id))
	for i := range children {
		child, ok := graph.LayoutChild(id, uint32(i))
		if !ok {
			return model.TypeLayout{}, false
		}
		resolved, ok := treeLayout(graph, child, depth+1)
		if !ok {
			return model.TypeLayout{}, false
		}
		children[i] = resolved
	}
	return model.TypeLayout{Type: typ, Children: children}, true
}

func newTreeEvaluator(ctx context.Context, graph *ir.Graph, root ir.ScalarID, options expr.Options, maxBytes uint64) (*treeEvaluator, error) {
	evaluator := &treeEvaluator{
		graph:   graph,
		root:    root,
		columns: make(map[model.SlotID]int, len(options.Inputs)),
		calls:   make(map[ir.ScalarID]builtin.Callable),
		consts:  make(map[ir.ScalarID]model.Value),
		budget:  builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20},
	}
	for _, input := range options.Inputs {
		evaluator.columns[input.Slot] = input.Column
	}
	pending := []ir.ScalarID{root}
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		scalar, ok := graph.Scalar(id)
		if !ok {
			return nil, expr.ErrInvalid
		}
		for i := range scalar.OperandCount() {
			operand, ok := graph.Operand(id, i)
			if !ok {
				return nil, expr.ErrInvalid
			}
			pending = append(pending, operand)
		}
		switch scalar.Kind {
		case ir.Const:
			value, err := graph.CloneConstant(ctx, id, maxBytes)
			if err != nil {
				return nil, err
			}
			evaluator.consts[id] = value
		case ir.Slot:
			// The dispatch fixture publishes exactly one input vector per packet.
			if column, mapped := evaluator.columns[scalar.Slot]; !mapped || column != 0 {
				return nil, errTreeWalkUnsupported
			}
		case ir.Call, ir.Cast:
			identity := scalar.Function
			if scalar.Operator != 0 {
				op, ok := builtin.LookupOperator(scalar.Operator)
				if !ok {
					return nil, expr.ErrInvalid
				}
				identity = op.Function
			}
			signature, ok := graph.Signature(scalar.Signature)
			if !ok {
				return nil, expr.ErrInvalid
			}
			argumentLayouts := make([]model.TypeLayout, signature.ParameterCount())
			for i := range argumentLayouts {
				layout, ok := graph.SignatureParameter(scalar.Signature, uint32(i))
				if !ok {
					return nil, expr.ErrInvalid
				}
				resolved, ok := treeLayout(graph, layout, 0)
				if !ok {
					return nil, expr.ErrInvalid
				}
				argumentLayouts[i] = resolved
			}
			resultLayout, ok := treeLayout(graph, signature.Result, 0)
			if !ok {
				return nil, expr.ErrInvalid
			}
			var callable builtin.Callable
			var err error
			if signature.VariadicArray {
				callable, err = builtin.ResolveVariadic(ctx, identity, argumentLayouts, resultLayout, options.Builtin, maxBytes)
			} else {
				callable, err = builtin.ResolveTyped(ctx, identity, argumentLayouts, resultLayout, options.Builtin, maxBytes)
			}
			if err != nil {
				return nil, err
			}
			evaluator.calls[id] = callable
		case ir.Case, ir.Coalesce, ir.NullTest, ir.Bool, ir.Relabel, ir.Collate:
		default:
			return nil, errTreeWalkUnsupported
		}
	}
	return evaluator, nil
}

// eval computes one scalar on one input lane. Returned values are borrowed or
// freshly allocated by the invoked kernel; the caller hashes and discards them.
func (t *treeEvaluator) eval(ctx context.Context, id ir.ScalarID, lane int) (model.Value, error) {
	scalar, ok := t.graph.Scalar(id)
	if !ok {
		return model.Value{}, expr.ErrInvalid
	}
	typ, _ := t.graph.Type(scalar.Layout)
	null := model.NullVal(typ.Storage)
	switch scalar.Kind {
	case ir.Const:
		return t.consts[id], nil
	case ir.Slot:
		vector := t.input
		if vector.IsNull(lane) {
			return null, nil
		}
		if vector.Type.Storage == model.TypeInt64 || vector.Type.Storage == model.TypeBoolean || vector.Type.Storage == model.TypeTimestamp {
			return model.Value{Type: vector.Type.Storage, IntVal: vector.Int64[vector.ValueIndex(lane)]}, nil
		}
		if vector.Type.Storage == model.TypeFloat64 {
			return model.Float64Val(vector.Float64[vector.ValueIndex(lane)]), nil
		}
		value, _, err := columnar.CopyValue(ctx, vector, lane, t.budget.MemoryBytes)
		return value, err
	case ir.Relabel, ir.Collate:
		operand, _ := t.graph.Operand(id, 0)
		return t.eval(ctx, operand, lane)
	case ir.NullTest:
		operand, _ := t.graph.Operand(id, 0)
		value, err := t.eval(ctx, operand, lane)
		if err != nil {
			return model.Value{}, err
		}
		return model.BoolVal(value.IsNull != scalar.Negated), nil
	case ir.Coalesce:
		for i := range scalar.OperandCount() {
			operand, _ := t.graph.Operand(id, i)
			value, err := t.eval(ctx, operand, lane)
			if err != nil {
				return model.Value{}, err
			}
			if !value.IsNull {
				return value, nil
			}
		}
		return null, nil
	case ir.Case:
		elseOperand := ir.ScalarID(0)
		count := scalar.OperandCount()
		if count%2 != 0 {
			elseOperand, _ = t.graph.Operand(id, count-1)
			count--
		}
		for i := uint32(0); i < count; i += 2 {
			when, _ := t.graph.Operand(id, i)
			value, err := t.eval(ctx, when, lane)
			if err != nil {
				return model.Value{}, err
			}
			if !value.IsNull && value.IntVal == 1 {
				then, _ := t.graph.Operand(id, i+1)
				return t.eval(ctx, then, lane)
			}
		}
		if elseOperand == 0 {
			return null, nil
		}
		return t.eval(ctx, elseOperand, lane)
	case ir.Bool:
		switch scalar.BoolOp {
		case ir.Not:
			operand, _ := t.graph.Operand(id, 0)
			value, err := t.eval(ctx, operand, lane)
			if err != nil || value.IsNull {
				return value, err
			}
			return model.BoolVal(value.IntVal != 1), nil
		case ir.And, ir.Or:
			result := scalar.BoolOp == ir.And
			nullable := false
			for i := range scalar.OperandCount() {
				operand, _ := t.graph.Operand(id, i)
				value, err := t.eval(ctx, operand, lane)
				if err != nil {
					return model.Value{}, err
				}
				if value.IsNull {
					nullable = true
					continue
				}
				if (value.IntVal == 1) != result {
					return model.BoolVal(!result), nil
				}
			}
			if nullable {
				return null, nil
			}
			return model.BoolVal(result), nil
		}
		return model.Value{}, errTreeWalkUnsupported
	case ir.Call, ir.Cast:
		count := scalar.OperandCount()
		args := make([]model.Value, count)
		nulls := false
		for i := range count {
			operand, _ := t.graph.Operand(id, i)
			value, err := t.eval(ctx, operand, lane)
			if err != nil {
				return model.Value{}, err
			}
			args[i] = value
			nulls = nulls || value.IsNull
		}
		if scalar.Strict && nulls {
			return null, nil
		}
		return t.calls[id].Call(ctx, args, t.budget)
	}
	return model.Value{}, errTreeWalkUnsupported
}

// benchmarkTreewalk runs the identical fixture and kernel set through a direct
// per-row graph walk. It shares the dispatch harness's checksum and retained
// metrics so compiled-vs-walked numbers are directly comparable.
func benchmarkTreewalk(b *testing.B, fixture *dispatchFixture, rows int, prepare bool) {
	b.Helper()
	build := func() *treeEvaluator {
		evaluator, err := newTreeEvaluator(b.Context(), fixture.graph, fixture.root, fixture.options, 1<<20)
		if err != nil {
			b.Fatal(err)
		}
		return evaluator
	}
	evaluator := build()
	warmup := func() {
		var actual, expected uint64
		for i := range fixture.packets {
			packet := &fixture.packets[i]
			evaluator.input = &packet.vector
			for row := range packet.vector.Length {
				value, err := evaluator.eval(b.Context(), fixture.root, row)
				if err != nil {
					b.Fatal(err)
				}
				actual += dispatchValueHash(value)
			}
			expected += packet.prefix[packet.vector.Length]
		}
		if actual != expected {
			b.Fatalf("treewalk checksum=%016x, want %016x", actual, expected)
		}
	}
	warmup()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if prepare {
			evaluator = build()
		}
		remaining, phase := rows, iteration%len(fixture.packets)
		var actual, expected uint64
		for remaining > 0 {
			packet := &fixture.packets[phase]
			take := min(remaining, packet.vector.Length)
			evaluator.input = &packet.vector
			for row := range take {
				value, err := evaluator.eval(b.Context(), fixture.root, row)
				if err != nil {
					b.Fatal(err)
				}
				actual += dispatchValueHash(value)
			}
			expected += packet.prefix[take]
			remaining -= take
			phase = (phase + 1) % len(fixture.packets)
		}
		if actual != expected {
			b.Fatalf("treewalk checksum=%016x, want %016x", actual, expected)
		}
	}
	b.StopTimer()
	evaluator = nil
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(fixture)
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "retained-B")
	b.ReportMetric(float64(int64(after.HeapObjects)-int64(before.HeapObjects)), "retained-objects")
	b.ReportMetric(float64(fixture.checksum&((1<<53)-1)), "checksum")
	b.ReportMetric(float64(rows), "rows/op")
}
