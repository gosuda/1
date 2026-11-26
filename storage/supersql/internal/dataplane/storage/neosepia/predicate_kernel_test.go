package neosepia

// Focused contract tests for the private predicate kernel. They exercise the
// compiled semantics directly against columnar.Batch views; they are neither
// a query-language suite nor a replacement for the integration pushdown plan
// checks owned by Main.

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	predBoolType        = model.Type{ID: 16, Storage: model.TypeBoolean}
	predInt4Type        = model.Type{ID: 23, Storage: model.TypeInt64}
	predInt8Type        = model.Type{ID: 20, Storage: model.TypeInt64}
	predTextColType     = model.Type{ID: 25, Storage: model.TypeString}
	predVarcharType     = model.Type{ID: 1043, Storage: model.TypeString, Collation: 100}
	predFloat8Type      = model.Type{ID: 701, Storage: model.TypeFloat64}
	predTimestamptzType = model.Type{ID: 1184, Storage: model.TypeTimestamp}
)

func predTestLayout() []storage.ColumnLayout {
	return []storage.ColumnLayout{
		{ID: 1, Type: predInt4Type},
		{ID: 2, Type: predTextColType},
		{ID: 3, Type: predFloat8Type},
		{ID: 4, Type: predBoolType},
		{ID: 5, Type: predTimestamptzType},
		{ID: 6, Type: predInt8Type},
	}
}

type predTestSpec struct {
	cols   []model.ColumnID
	values [][]model.Value
}

func buildPredKernelBatch(t *testing.T, colTypes map[model.ColumnID]model.Type, spec predTestSpec) columnar.Batch {
	t.Helper()
	layouts := make([]model.TypeLayout, len(spec.cols))
	for i, id := range spec.cols {
		layouts[i] = model.TypeLayout{Type: colTypes[id]}
	}
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: len(spec.values), MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	ctx := context.Background()
	for _, row := range spec.values {
		if err := b.Append(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	var out columnar.Batch
	if err := b.Publish(&out); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(len(spec.values)); err != nil {
		t.Fatal(err)
	}
	return out
}

func predColNode(id model.ColumnID) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: id, Type: predColType(id)}
}

func predColType(id model.ColumnID) model.Type {
	switch id {
	case 1:
		return predInt4Type
	case 2:
		return predTextColType
	case 3:
		return predFloat8Type
	case 4:
		return predBoolType
	case 5:
		return predTimestamptzType
	case 6:
		return predInt8Type
	}
	panic("unknown test column")
}

func predConstNode(ci uint32, typ model.Type) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: ci}
}

func predCmpNode(op model.OperatorID, left, right uint32) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateComparison, Type: predBoolType, Operator: op, Left: left, Right: right}
}

func predCmpNodeCollation(op model.OperatorID, coll model.CollationID, left, right uint32) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateComparison, Type: predBoolType, Operator: op, Collation: coll, Left: left, Right: right}
}

func predInNode(op model.OperatorID, probe uint32, first, count uint32, negated bool) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateIn, Type: predBoolType, Operator: op, Left: probe, FirstArgument: first, ArgumentCount: count, Negated: negated}
}

func predNullNode(child uint32, negated bool) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateNullTest, Type: predBoolType, Left: child, Negated: negated}
}

func predAndNode(l, r uint32) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateAnd, Type: predBoolType, Left: l, Right: r}
}

func predOrNode(l, r uint32) storage.PredicateNode {
	return storage.PredicateNode{Kind: storage.PredicateOr, Type: predBoolType, Left: l, Right: r}
}

func compilePredKernel(t *testing.T, nodes []storage.PredicateNode, edges []uint32, consts []model.Value, layout []storage.ColumnLayout) (*predicateProgram, bool) {
	t.Helper()
	pred, err := storage.NewPredicate(nodes, edges, consts)
	if err != nil {
		t.Fatal(err)
	}
	prog, ok, err := compilePredicate(context.Background(), pred, layout, 4096, 1<<24)
	if err != nil {
		t.Fatal(err)
	}
	return prog, ok
}

func runPredKernelFilter(t *testing.T, prog *predicateProgram, batch *columnar.Batch) []uint32 {
	t.Helper()
	var scratch predicateScratch
	t.Cleanup(func() { _ = scratch.Close() })
	if err := prog.Filter(context.Background(), batch, &scratch, 1<<24); err != nil {
		t.Fatal(err)
	}
	if batch.Selection == nil {
		t.Fatal("Filter left a nil selection")
	}
	return append([]uint32(nil), batch.Selection...)
}

func expectPredSelection(t *testing.T, got, want []uint32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("selection = %v, want %v", got, want)
		}
	}
}

func TestPredicateNullNotInSemantics(t *testing.T) {
	layout := predTestLayout()
	// Members: 2, NULL.
	nodes2 := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predConstNode(1, predInt4Type),
		predInNode(96, 0, 0, 2, false),
	}
	consts := []model.Value{{Type: model.TypeInt64, IntVal: 2}, {Type: model.TypeInt64, IsNull: true}}
	prog, ok := compilePredKernel(t, nodes2, []uint32{1, 2}, consts, layout)
	if !ok {
		t.Fatal("supported IN declined")
	}
	colTypes := map[model.ColumnID]model.Type{1: predInt4Type}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{1},
		values: [][]model.Value{
			{{Type: model.TypeInt64, IntVal: 2}},
			{{Type: model.TypeInt64, IntVal: 3}},
			{{Type: model.TypeInt64, IsNull: true}},
		},
	})
	got := runPredKernelFilter(t, prog, &batch)
	// x IN (2, NULL): row 0 true; rows 1 and 2 unknown -> filtered out.
	expectPredSelection(t, got, []uint32{0})

	// x NOT IN (2, NULL): row 0 false; rows 1 and 2 unknown -> filtered out.
	nodes3 := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predConstNode(1, predInt4Type),
		predInNode(96, 0, 0, 2, true),
	}
	prog3, ok := compilePredKernel(t, nodes3, []uint32{1, 2}, []model.Value{{Type: model.TypeInt64, IntVal: 2}, {Type: model.TypeInt64, IsNull: true}}, layout)
	if !ok {
		t.Fatal("supported NOT IN declined")
	}
	batch3 := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{1},
		values: [][]model.Value{
			{{Type: model.TypeInt64, IntVal: 2}},
			{{Type: model.TypeInt64, IntVal: 3}},
			{{Type: model.TypeInt64, IsNull: true}},
			{{Type: model.TypeInt64, IntVal: 9}},
		},
	})
	got3 := runPredKernelFilter(t, prog3, &batch3)
	// Every row is false or unknown under three-valued NOT IN with a NULL
	// member, so the selection is empty but non-nil.
	expectPredSelection(t, got3, []uint32{})
	if batch3.Selection == nil {
		t.Fatal("empty result must be non-nil")
	}

	// x NOT IN (2) without NULLs: rows 3 and 9 survive.
	nodes4 := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predInNode(96, 0, 0, 1, true),
	}
	prog4, ok := compilePredKernel(t, nodes4, []uint32{1}, []model.Value{{Type: model.TypeInt64, IntVal: 2}}, layout)
	if !ok {
		t.Fatal("supported NOT IN declined")
	}
	batch4 := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{1},
		values: [][]model.Value{
			{{Type: model.TypeInt64, IntVal: 2}},
			{{Type: model.TypeInt64, IntVal: 3}},
			{{Type: model.TypeInt64, IsNull: true}},
		},
	})
	expectPredSelection(t, runPredKernelFilter(t, prog4, &batch4), []uint32{1})
}

func TestPredicateNaNOrderingAndEquality(t *testing.T) {
	layout := predTestLayout()
	nan := math.NaN()
	nodes := []storage.PredicateNode{
		predColNode(3),
		predConstNode(0, predFloat8Type),
		predCmpNode(672, 0, 1), // x < 1.0
	}
	prog, ok := compilePredKernel(t, nodes, nil, []model.Value{{Type: model.TypeFloat64, FloatVal: 1}}, layout)
	if !ok {
		t.Fatal("supported float comparison declined")
	}
	colTypes := map[model.ColumnID]model.Type{3: predFloat8Type}
	vals := [][]model.Value{
		{{Type: model.TypeFloat64, FloatVal: 0.5}},
		{{Type: model.TypeFloat64, FloatVal: nan}},
		{{Type: model.TypeFloat64, IsNull: true}},
		{{Type: model.TypeFloat64, FloatVal: 2}},
	}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{cols: []model.ColumnID{3}, values: vals})
	// NaN exceeds 1.0, so only row 0 qualifies.
	expectPredSelection(t, runPredKernelFilter(t, prog, &batch), []uint32{0})

	// NaN = NaN must hold under PostgreSQL ordering.
	nodesEq := []storage.PredicateNode{
		predColNode(3),
		predConstNode(0, predFloat8Type),
		predCmpNode(670, 0, 1),
	}
	progEq, ok := compilePredKernel(t, nodesEq, nil, []model.Value{{Type: model.TypeFloat64, FloatVal: nan}}, layout)
	if !ok {
		t.Fatal("supported float equality declined")
	}
	batchEq := buildPredKernelBatch(t, colTypes, predTestSpec{cols: []model.ColumnID{3}, values: vals})
	expectPredSelection(t, runPredKernelFilter(t, progEq, &batchEq), []uint32{1})
}

func TestPredicateDuplicateReorderedSelection(t *testing.T) {
	layout := predTestLayout()
	nodes := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predCmpNode(523, 0, 1), // x <= 10
	}
	prog, ok := compilePredKernel(t, nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 10}}, layout)
	if !ok {
		t.Fatal("supported integer comparison declined")
	}
	if got := prog.Columns(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Columns() = %v, want [1]", got)
	}
	colTypes := map[model.ColumnID]model.Type{1: predInt4Type}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{1},
		values: [][]model.Value{
			{{Type: model.TypeInt64, IntVal: 5}},
			{{Type: model.TypeInt64, IntVal: 50}},
			{{Type: model.TypeInt64, IntVal: 7}},
			{{Type: model.TypeInt64, IntVal: 50}},
			{{Type: model.TypeInt64, IsNull: true}},
		},
	})
	// Incoming selection is reordered with duplicates; Filter intersects it
	// while preserving lane order and duplicates.
	batch.Selection = []uint32{3, 0, 2, 0, 1, 4}
	got := runPredKernelFilter(t, prog, &batch)
	expectPredSelection(t, got, []uint32{0, 2, 0})
}

func TestPredicateUnsupportedIdentityAndCollation(t *testing.T) {
	layout := predTestLayout()
	// Unknown operator OID: structurally valid, semantically unknown.
	nodes := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predCmpNode(999999, 0, 1),
	}
	pred, err := storage.NewPredicate(nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := compilePredicate(context.Background(), pred, layout, 4096, 1<<24); err != nil || ok {
		t.Fatalf("unknown operator: ok=%v err=%v, want (false, nil)", ok, err)
	}
	// Valid text operator with a non-C collation stays residual.
	nodesText := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 2, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 811}},
		predConstNode(0, predTextColType),
		predCmpNodeCollation(98, 811, 0, 1),
	}
	predText, err := storage.NewPredicate(nodesText, nil, []model.Value{{Type: model.TypeString, StrVal: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	textLayout := []storage.ColumnLayout{{ID: 2, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 811}}}
	if _, ok, err := compilePredicate(context.Background(), predText, textLayout, 4096, 1<<24); err != nil || ok {
		t.Fatalf("non-C collation: ok=%v err=%v, want (false, nil)", ok, err)
	}
	// Cross timestamp/timestamptz operators are declined, not guessed.
	nodesMix := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 5, Type: predTimestamptzType},
		storage.PredicateNode{Kind: storage.PredicateConstant, Type: model.Type{ID: 1114, Storage: model.TypeTimestamp}, ConstantIndex: 0},
		predCmpNode(2542, 0, 1),
	}
	predMix, err := storage.NewPredicate(nodesMix, nil, []model.Value{{Type: model.TypeTimestamp, IntVal: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := compilePredicate(context.Background(), predMix, layout, 4096, 1<<24); err != nil || ok {
		t.Fatalf("mixed timestamp operator: ok=%v err=%v, want (false, nil)", ok, err)
	}
	// varchar uses the pinned text operators with bytewise semantics.
	nodesVarchar := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 2, Type: model.Type{ID: 1043, Storage: model.TypeString, Collation: 950}},
		storage.PredicateNode{Kind: storage.PredicateConstant, Type: model.Type{ID: 1043, Storage: model.TypeString, Collation: 950}, ConstantIndex: 0},
		predCmpNodeCollation(98, 950, 0, 1),
	}
	// Rebind: the test helper colByID stamps text; construct layout to match.
	varcharLayout := []storage.ColumnLayout{{ID: 2, Type: model.Type{ID: 1043, Storage: model.TypeString, Collation: 950}}}
	predVarchar, err := storage.NewPredicate(nodesVarchar, nil, []model.Value{{Type: model.TypeString, StrVal: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	progV, ok, err := compilePredicate(context.Background(), predVarchar, varcharLayout, 4096, 1<<24)
	if err != nil || !ok {
		t.Fatalf("varchar equality: ok=%v err=%v, want accepted", ok, err)
	}
	colTypes := map[model.ColumnID]model.Type{2: {ID: 1043, Storage: model.TypeString, Collation: 950}}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{2},
		values: [][]model.Value{
			{{Type: model.TypeString, StrVal: "a"}},
			{{Type: model.TypeString, StrVal: "b"}},
		},
	})
	expectPredSelection(t, runPredKernelFilter(t, progV, &batch), []uint32{0})
}

func TestPredicateUnreachableUnsupportedIgnored(t *testing.T) {
	layout := predTestLayout()
	// NewPredicate validates every node structurally, so the unreachable
	// unknown-operator node must still be well-formed: reference only
	// earlier nodes.
	unreachable := []storage.PredicateNode{
		predColNode(1),                 // 0
		predConstNode(0, predInt4Type), // 1
		predCmpNode(999999, 0, 1),      // 2 unreachable unknown operator
		predCmpNode(96, 0, 1),          // 3 root
	}
	prog, ok := compilePredKernel(t, unreachable, nil, []model.Value{{Type: model.TypeInt64, IntVal: 4}}, layout)
	if !ok {
		t.Fatal("reachable program declined because of an unreachable node")
	}
	colTypes := map[model.ColumnID]model.Type{1: predInt4Type}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols: []model.ColumnID{1},
		values: [][]model.Value{
			{{Type: model.TypeInt64, IntVal: 4}},
			{{Type: model.TypeInt64, IntVal: 5}},
		},
	})
	expectPredSelection(t, runPredKernelFilter(t, prog, &batch), []uint32{0})
}

func TestPredicateInvalidColumnReference(t *testing.T) {
	layout := predTestLayout()
	nodes := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 77, Type: predInt4Type},
		predConstNode(0, predInt4Type),
		predCmpNode(96, 0, 1),
	}
	pred, err := storage.NewPredicate(nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := compilePredicate(context.Background(), pred, layout, 4096, 1<<24); !errors.Is(err, storage.ErrInvalidPredicate) {
		t.Fatalf("unbound column err = %v, want ErrInvalidPredicate", err)
	}
	// Node type storage inconsistent with the resolved operator family.
	badType := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 1, Type: predFloat8Type},
		predConstNode(0, predFloat8Type),
		predCmpNode(96, 0, 1),
	}
	predBad, err := storage.NewPredicate(badType, nil, []model.Value{{Type: model.TypeFloat64, FloatVal: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := compilePredicate(context.Background(), predBad, layout, 4096, 1<<24); !errors.Is(err, storage.ErrInvalidPredicate) {
		t.Fatalf("inconsistent operand err = %v, want ErrInvalidPredicate", err)
	}
}

func TestPredicateCancellationAndBudgets(t *testing.T) {
	layout := predTestLayout()
	nodes := []storage.PredicateNode{
		predColNode(1),
		predConstNode(0, predInt4Type),
		predCmpNode(96, 0, 1),
	}
	pred, err := storage.NewPredicate(nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 1}})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := compilePredicate(canceled, pred, layout, 4096, 1<<24); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled compile err = %v, want context.Canceled", err)
	}
	// Node budget smaller than the reachable set.
	if _, _, err := compilePredicate(context.Background(), pred, layout, 1, 1<<24); !errors.Is(err, errPredicateBudget) {
		t.Fatalf("node budget err = %v, want budget error", err)
	}
	// Byte budget smaller than the program.
	if _, _, err := compilePredicate(context.Background(), pred, layout, 4096, 1); !errors.Is(err, errPredicateBudget) {
		t.Fatalf("byte budget err = %v, want budget error", err)
	}
	prog, ok := compilePredKernel(t, nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 1}}, layout)
	if !ok {
		t.Fatal("supported comparison declined")
	}
	colTypes := map[model.ColumnID]model.Type{1: predInt4Type}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{
		cols:   []model.ColumnID{1},
		values: [][]model.Value{{{Type: model.TypeInt64, IntVal: 1}}},
	})
	var scratch predicateScratch
	if err := prog.Filter(canceled, &batch, &scratch, 1<<24); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled filter err = %v, want context.Canceled", err)
	}
	_ = scratch.Close()
	if err := prog.Filter(context.Background(), &batch, &scratch, 1<<24); !errors.Is(err, errPredicateScratchClosed) {
		t.Fatalf("closed scratch err = %v, want scratch-closed", err)
	}
	var scratch2 predicateScratch
	defer scratch2.Close()
	if err := prog.Filter(context.Background(), &batch, &scratch2, 1); !errors.Is(err, errPredicateBudget) {
		t.Fatalf("evaluation budget err = %v, want budget error", err)
	}
}

func TestPredicateThreeValuedAndOr(t *testing.T) {
	layout := predTestLayout()
	// (x = 1) AND (y IS NULL), (x = 1) OR (y IS NULL).
	for _, or := range []bool{false, true} {
		nodes := []storage.PredicateNode{
			storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 1, Type: predInt4Type}, // 0
			predConstNode(0, predInt4Type), // 1
			predCmpNode(96, 0, 1),          // 2
			storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 2, Type: predTextColType}, // 3
			predNullNode(3, false), // 4
		}
		if or {
			nodes = append(nodes, predOrNode(2, 4))
		} else {
			nodes = append(nodes, predAndNode(2, 4))
		}
		prog, ok := compilePredKernel(t, nodes, nil, []model.Value{{Type: model.TypeInt64, IntVal: 1}}, layout)
		if !ok {
			t.Fatal("supported AND/OR declined")
		}
		if got := prog.Columns(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatalf("Columns() = %v, want [1 2]", got)
		}
		colTypes := map[model.ColumnID]model.Type{1: predInt4Type, 2: predTextColType}
		batch := buildPredKernelBatch(t, colTypes, predTestSpec{
			cols: []model.ColumnID{1, 2},
			values: [][]model.Value{
				{{Type: model.TypeInt64, IntVal: 1}, {Type: model.TypeString, IsNull: true}},
				{{Type: model.TypeInt64, IntVal: 1}, {Type: model.TypeString, StrVal: "a"}},
				{{Type: model.TypeInt64, IntVal: 2}, {Type: model.TypeString, IsNull: true}},
				{{Type: model.TypeInt64, IntVal: 2}, {Type: model.TypeString, StrVal: "a"}},
				{{Type: model.TypeInt64, IsNull: true}, {Type: model.TypeString, IsNull: true}},
			},
		})
		var want []uint32
		if or {
			// TRUE OR anything is TRUE; nonmatching rows with NULL y are
			// unknown and filtered out.
			want = []uint32{0, 1, 2, 4}
		} else {
			want = []uint32{0}
		}
		expectPredSelection(t, runPredKernelFilter(t, prog, &batch), want)
	}
}

func TestPredicateBooleanColumnAndScratchReuse(t *testing.T) {
	layout := predTestLayout()
	nodes := []storage.PredicateNode{
		storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 4, Type: predBoolType},
	}
	prog, ok := compilePredKernel(t, nodes, nil, nil, layout)
	if !ok {
		t.Fatal("boolean column declined")
	}
	colTypes := map[model.ColumnID]model.Type{4: predBoolType}
	vals := [][]model.Value{
		{model.BoolVal(true)},
		{model.BoolVal(false)},
		{{Type: model.TypeBoolean, IsNull: true}},
	}
	batch := buildPredKernelBatch(t, colTypes, predTestSpec{cols: []model.ColumnID{4}, values: vals})
	var scratch predicateScratch
	defer scratch.Close()
	if err := prog.Filter(context.Background(), &batch, &scratch, 1<<24); err != nil {
		t.Fatal(err)
	}
	expectPredSelection(t, append([]uint32(nil), batch.Selection...), []uint32{0})
	// Reusing one scratch across batches must not retain the first result.
	batch2 := buildPredKernelBatch(t, colTypes, predTestSpec{cols: []model.ColumnID{4}, values: [][]model.Value{{model.BoolVal(false)}, {model.BoolVal(true)}}})
	if err := prog.Filter(context.Background(), &batch2, &scratch, 1<<24); err != nil {
		t.Fatal(err)
	}
	expectPredSelection(t, append([]uint32(nil), batch2.Selection...), []uint32{1})
}
