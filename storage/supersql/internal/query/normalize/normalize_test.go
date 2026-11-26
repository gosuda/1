package normalize_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

var errDeadArm = errors.New("dead arm")

func options() normalize.Options {
	return normalize.Options{Limits: ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: 10000}, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, ExtraFloatDigits: 1}}
}

func bound(t *testing.T, sql string) *ir.Graph {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: options().Limits}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return statement.Graph()
}

func normalized(t *testing.T, graph *ir.Graph) *ir.Graph {
	t.Helper()
	result, err := normalize.Normalize(t.Context(), graph, options())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func expression(t *testing.T, graph *ir.Graph) ir.ScalarID {
	t.Helper()
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		t.Fatal("missing projected expression")
	}
	return binding.Expr
}

func evaluate(t *testing.T, graph *ir.Graph, roots []ir.ScalarID, functions expr.Resolver, rows int) ([][]model.Value, error) {
	t.Helper()
	program, err := expr.Compile(t.Context(), graph, roots, expr.Options{Limits: expr.DefaultLimits(), Builtin: options().Builtin, Functions: functions})
	if err != nil {
		return nil, err
	}
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	input, output := columnar.Batch{Length: rows}, columnar.Batch{}
	if err := state.Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		return nil, err
	}
	values := make([][]model.Value, len(roots))
	for i := range values {
		values[i] = make([]model.Value, rows)
		for lane := range rows {
			values[i][lane], _, err = columnar.CopyValue(t.Context(), &output.Columns[i], lane, 1<<20)
			if err != nil {
				return nil, err
			}
		}
	}
	return values, nil
}

func TestBoundConstantsFoldThroughSQLKernels(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		want      model.Value
		typeID    model.TypeID
	}{
		{"integer widening", "SELECT 2147483647::int8 + 1::int8", model.Int64Val(2147483648), 20},
		{"exact decimal", "SELECT 9007199254740993.00 + 0.01", model.Value{Type: model.TypeNumeric, StrVal: "9007199254740993.01"}, 1700},
		{"masked case error", "SELECT CASE WHEN false THEN 1/0 ELSE 7 END", model.Int64Val(7), 23},
		{"masked coalesce error", "SELECT COALESCE(7, 1/0)", model.Int64Val(7), 23},
		{"masked boolean error", "SELECT false AND (1/0 = 0)", model.BoolVal(false), 16},
		{"nullable truth", "SELECT NULL::boolean OR false", model.NullVal(model.TypeBoolean), 16},
		{"null with decisive truth", "SELECT NULL::boolean AND false", model.BoolVal(false), 16},
		{"resolved collation", "SELECT 'A'::text < 'a'::text", model.BoolVal(true), 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := bound(t, tc.sql)
			graph := normalized(t, original)
			root := expression(t, graph)
			node, _ := graph.Scalar(root)
			typ, _ := graph.Type(node.Layout)
			if node.Kind != ir.Const || typ.ID != tc.typeID || node.Nullable != tc.want.IsNull {
				t.Fatalf("folded node=%+v type=%+v", node, typ)
			}
			values, err := evaluate(t, graph, []ir.ScalarID{root}, nil, 17)
			if err != nil {
				t.Fatal(err)
			}
			for lane, got := range values[0] {
				wrongPayload := !got.IsNull && (got.IntVal != tc.want.IntVal || got.StrVal != tc.want.StrVal)
				if got.Type != tc.want.Type || got.IsNull != tc.want.IsNull || wrongPayload {
					t.Fatalf("lane %d = %+v, want %+v", lane, got, tc.want)
				}
			}
		})
	}
}

func TestNormalizationCancellationAndBudgets(t *testing.T) {
	graph := bound(t, "SELECT 1 + 2")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := normalize.Normalize(ctx, graph, options()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*normalize.Options)
		target error
	}{
		{"work", func(o *normalize.Options) { o.MaxWork = 1 }, normalize.ErrLimit},
		{"allocation", func(o *normalize.Options) { o.MaxBytes = 1 }, normalize.ErrLimit},
		{"graph allocation", func(o *normalize.Options) { o.Limits.MaxBytes = 1 }, ir.ErrLimit},
		{"zero work", func(o *normalize.Options) { o.MaxWork = 0 }, normalize.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options()
			tc.change(&o)
			got, err := normalize.Normalize(t.Context(), graph, o)
			if got != nil || !errors.Is(err, tc.target) {
				t.Fatalf("graph=%v error=%v", got, err)
			}
		})
	}
}

func TestPredicateSubqueryRebindingHonorsSlotBudget(t *testing.T) {
	graph := bound(t, "SELECT x FROM (VALUES(1),(2)) t(x) WHERE x IN (SELECT y FROM (VALUES(2)) r(y))")
	o := options()
	o.Limits.MaxSlots = graph.SlotCount()
	result, err := normalize.Normalize(t.Context(), graph, o)
	if result != nil || !errors.Is(err, ir.ErrLimit) {
		t.Fatalf("slot-limited graph=%v error=%v, want resource rejection", result, err)
	}
}

type resolver func(expr.Function) (expr.ScalarCall, error)

func (r resolver) Resolve(f expr.Function) (expr.ScalarCall, error) { return r(f) }

type fixture struct {
	t                *testing.T
	b                *ir.Builder
	scope            ir.ScopeID
	integer, boolean ir.LayoutID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	b, err := ir.NewBuilder(options().Limits)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	integer, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, b: b, scope: scope, integer: integer, boolean: boolean}
}
func (f *fixture) scalar(spec ir.ScalarSpec) ir.ScalarID {
	f.t.Helper()
	id, err := f.b.AddScalar(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *fixture) rel(spec ir.RelSpec) ir.RelID {
	f.t.Helper()
	id, err := f.b.AddRel(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *fixture) slot(layout ir.LayoutID, nullable bool) model.SlotID {
	f.t.Helper()
	id, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: layout, Nullable: nullable})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f *fixture) constant(layout ir.LayoutID, value model.Value) ir.ScalarID {
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: layout, Nullable: value.IsNull}, Value: value})
}
func (f *fixture) freeze(root ir.RelID) *ir.Graph {
	f.t.Helper()
	graph, err := f.b.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	return graph
}
func (f *fixture) project(scalar ir.ScalarID, layout ir.LayoutID, effects ir.Effects, nullable bool) *ir.Graph {
	input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}})
	output := f.slot(layout, nullable)
	return f.freeze(f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{output}, Bindings: []ir.Binding{{Slot: output, Expr: scalar}}}))
}

func TestSharedVolatileOnceIsNotDuplicatedOrFolded(t *testing.T) {
	f := newFixture(t)
	effects := ir.Effects{Volatility: ir.Volatile}
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990001, Result: f.boolean, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Function: 990001, Signature: signature, Effects: effects}})
	once := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: f.scope, Layout: f.boolean, Effects: effects}, Operands: []ir.ScalarID{call}})
	root := once
	for range 7 {
		root = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, BoolOp: ir.And, Effects: effects}, Operands: []ir.ScalarID{root, root}})
	}
	original := f.project(root, f.boolean, effects, false)
	graph := normalized(t, original)
	if graph.ScalarCount() != original.ScalarCount() {
		t.Fatalf("shared DAG expanded: %d to %d", original.ScalarCount(), graph.ScalarCount())
	}
	calls := 0
	values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, resolver(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			return model.BoolVal(true), nil
		}, nil
	}), 17)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 17 {
		t.Fatalf("volatile calls=%d, want one per row", calls)
	}
	for _, value := range values[0] {
		if value.IsNull || value.IntVal != 1 {
			t.Fatalf("result=%+v", value)
		}
	}
}

func TestFoldedConstantOwnsNestedPayload(t *testing.T) {
	f := newFixture(t)
	bytesLayout := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	rowLayout := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{bytesLayout}}
	layout, err := f.b.AddLayout(t.Context(), rowLayout)
	if err != nil {
		t.Fatal(err)
	}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.BytesVal([]byte{1, 2, 3})}}
	constant := f.constant(layout, value)
	truth := f.constant(f.boolean, model.BoolVal(true))
	choice := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Case, Scope: f.scope, Layout: layout}, Operands: []ir.ScalarID{truth, constant, constant}})
	graph := normalized(t, f.project(choice, layout, ir.Effects{}, false))
	root := expression(t, graph)
	copy, err := graph.CloneConstant(t.Context(), root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	copy.ArrayVal[0].BytesVal[0] = 99
	value.ArrayVal[0].BytesVal[1] = 99
	again, err := graph.CloneConstant(t.Context(), root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if again.ArrayVal[0].BytesVal[0] != 1 || again.ArrayVal[0].BytesVal[1] != 2 {
		t.Fatalf("constant aliased: %+v", again)
	}
	node, _ := graph.Scalar(root)
	child, ok := graph.LayoutChild(node.Layout, 0)
	if !ok {
		t.Fatal("composite child lost")
	}
	typ, _ := graph.Type(child)
	if typ.ID != 17 {
		t.Fatalf("child type=%+v", typ)
	}
}

func TestNaNComparisonUsesPostgreSQLKernel(t *testing.T) {
	f := newFixture(t)
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 701, Modifier: -1, Storage: model.TypeFloat64}})
	if err != nil {
		t.Fatal(err)
	}
	var identity model.FunctionID
	for sig := range builtin.SignaturesNamed("float8eq") {
		identity = sig.ID
	}
	if identity == 0 {
		t.Fatal("missing float8eq inventory")
	}
	sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: identity, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{layout, layout}})
	if err != nil {
		t.Fatal(err)
	}
	nan := f.constant(layout, model.Float64Val(math.NaN()))
	call := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Function: identity, Signature: sig, Strict: true}, Operands: []ir.ScalarID{nan, nan}})
	graph := normalized(t, f.project(call, f.boolean, ir.Effects{}, false))
	value, err := graph.CloneConstant(t.Context(), expression(t, graph), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull || value.IntVal != 1 {
		t.Fatalf("NaN equality=%+v", value)
	}
}

func TestDeadVolatileOnceArmIsNeverInvoked(t *testing.T) {
	f := newFixture(t)
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990004, Result: f.integer, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	call := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.integer, Function: 990004, Signature: signature, Effects: effects}})
	once := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: f.scope, Layout: f.integer, Effects: effects}, Operands: []ir.ScalarID{call}})
	falsity, seven := f.constant(f.boolean, model.BoolVal(false)), f.constant(f.integer, model.Int64Val(7))
	choice := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Case, Scope: f.scope, Layout: f.integer, Effects: effects}, Operands: []ir.ScalarID{falsity, once, seven}})
	graph := normalized(t, f.project(choice, f.integer, effects, false))
	values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, resolver(func(expr.Function) (expr.ScalarCall, error) {
		t.Fatal("dead arm resolved")
		return nil, errDeadArm
	}), 17)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values[0] {
		if value.IsNull || value.IntVal != 7 {
			t.Fatalf("CASE result=%+v", value)
		}
	}
}

func TestNullableOnceMetadataTracksSimplifiedOperand(t *testing.T) {
	f := newFixture(t)
	one := f.constant(f.integer, model.Int64Val(1))
	null := f.constant(f.integer, model.NullVal(model.TypeInt64))
	truth := f.constant(f.boolean, model.BoolVal(true))
	choice := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Case, Scope: f.scope, Layout: f.integer, Nullable: true}, Operands: []ir.ScalarID{truth, one, null}})
	once := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: f.scope, Layout: f.integer, Nullable: true}, Operands: []ir.ScalarID{choice}})
	graph := normalized(t, f.project(once, f.integer, ir.Effects{}, true))
	node, _ := graph.Scalar(expression(t, graph))
	if node.Kind != ir.Once || node.Nullable {
		t.Fatalf("Once metadata=%+v", node)
	}
	values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, nil, 1)
	if err != nil || values[0][0].IsNull || values[0][0].IntVal != 1 {
		t.Fatalf("Once result=%+v, %v", values, err)
	}
}

func TestLargeConstantCloneHonorsScratchBudget(t *testing.T) {
	f := newFixture(t)
	layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}})
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1<<20)
	payload[0], payload[len(payload)-1] = 17, 23
	constant := f.constant(layout, model.BytesVal(payload))
	original := f.project(constant, layout, ir.Effects{}, false)
	o := options()
	o.MaxBytes = 32768
	graph, err := normalize.Normalize(t.Context(), original, o)
	var sql *model.Error
	if graph != nil || !errors.As(err, &sql) || sql.Code != "54000" {
		t.Fatalf("oversized clone=%v, %v", graph, err)
	}
	value, err := original.CloneConstant(t.Context(), constant, 2<<20)
	if err != nil || value.BytesVal[0] != 17 || value.BytesVal[len(value.BytesVal)-1] != 23 {
		t.Fatalf("failed normalization changed original: %v", err)
	}
}

func TestNestedLayoutReconstructionHonorsDepthBudget(t *testing.T) {
	f := newFixture(t)
	layout := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	for range 12 {
		layout = model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{layout}}
	}
	id, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	null := f.constant(id, model.NullVal(model.TypeComposite))
	original := f.project(null, id, ir.Effects{}, true)
	o := options()
	o.Limits.MaxDepth = 4
	graph, err := normalize.Normalize(t.Context(), original, o)
	if graph != nil || !errors.Is(err, normalize.ErrLimit) {
		t.Fatalf("nested layout=%v, %v", graph, err)
	}
}

func TestQuantifiedSubqueryRewritesToMarkJoin(t *testing.T) {
	for _, sql := range []string{
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x = ANY(SELECT y FROM (VALUES(2)) r(y))`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x > ALL(SELECT y FROM (VALUES(2)) r(y))`,
		`SELECT x FROM (VALUES(1,2)) t(x,y) WHERE x = ANY(SELECT z FROM (VALUES(2)) r(z) WHERE z < y)`,
	} {
		t.Run(sql, func(t *testing.T) {
			graph := normalized(t, bound(t, sql))
			markJoin := false
			for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
				rel, _ := graph.Rel(id)
				if rel.Kind != ir.Join || rel.JoinKind != ir.MarkJoin {
					continue
				}
				markJoin = true
				predicate, _ := graph.Scalar(rel.Predicate)
				if predicate.Kind != ir.Call || predicate.Signature == 0 {
					t.Fatalf("mark predicate=%+v, want a resolved comparison call", predicate)
				}
				signature, ok := graph.Signature(predicate.Signature)
				if !ok || signature.Operator == 0 || signature.Function != 0 {
					t.Fatalf("mark signature=%+v ok=%v", signature, ok)
				}
			}
			if !markJoin {
				t.Fatal("quantified subquery did not rewrite to a mark join")
			}
		})
	}
}
