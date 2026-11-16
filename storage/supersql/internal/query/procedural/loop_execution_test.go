package procedural

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// loopTestSnapshot resolves no catalog objects. Every expression these tests
// compile is a literal or an array constructor, so binding never needs one.
type loopTestSnapshot struct{}

var errLoopTestNoCatalog = errors.New("loop test catalog is empty")

func (loopTestSnapshot) Timestamp() model.Timestamp { return 0 }
func (loopTestSnapshot) Resolve(context.Context, model.NamespaceName) (catalog.PublishedDescriptor, error) {
	return catalog.PublishedDescriptor{}, errLoopTestNoCatalog
}
func (loopTestSnapshot) Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error) {
	return catalog.PublishedDescriptor{}, errLoopTestNoCatalog
}
func (loopTestSnapshot) ScanNamespace(context.Context, model.ObjectID, model.ObjectKind, func(catalog.PublishedDescriptor) error) error {
	return errLoopTestNoCatalog
}

func loopTestBindOptions() bind.Options {
	limits := ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: 1 << 20}
	return bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits}
}

// loopTestExecutor answers the literal expressions the compiled loop bodies
// contain and serves canned rows for the dynamically built command. It records
// every request so a test can observe what reached SQL execution.
type loopTestExecutor struct {
	values   map[string]model.Value
	rows     [][]model.Value
	columns  []model.TypeLayout
	requests []SQLRequest
}

func (e *loopTestExecutor) value(request SQLRequest) (model.Value, error) {
	text, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{request.Statement}})
	if err != nil {
		return model.Value{}, err
	}
	value, ok := e.values[text]
	if !ok {
		return model.Value{}, fmt.Errorf("loop test executor has no value for %q", text)
	}
	return value, nil
}

func (e *loopTestExecutor) RunRoutine(context.Context, RoutineInvocation, func(context.Context) (SQLResult, error)) (SQLResult, error) {
	return SQLResult{}, nil
}

func (e *loopTestExecutor) ExecuteSQL(_ context.Context, request SQLRequest) (SQLResult, error) {
	e.requests = append(e.requests, request)
	text, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{request.Statement}})
	if err != nil {
		return SQLResult{}, err
	}
	if _, dynamic := e.values["!"+text]; dynamic {
		return SQLResult{Columns: e.columns, Rows: e.rows}, nil
	}
	value, err := e.value(request)
	if err != nil {
		return SQLResult{}, err
	}
	return SQLResult{Columns: []model.TypeLayout{{Type: model.Type{Storage: value.Type, Modifier: -1}}}, Rows: [][]model.Value{{value}}}, nil
}

func (e *loopTestExecutor) RunSubtransaction(context.Context, func(context.Context) error) error {
	return nil
}
func (e *loopTestExecutor) ValidateRoutineValue(context.Context, model.TypeLayout, model.Value) error {
	return nil
}
func (e *loopTestExecutor) TransactionControl(context.Context, TransactionCommand) error { return nil }
func (e *loopTestExecutor) DeferTrigger(context.Context, DeferredTrigger) error          { return nil }
func (e *loopTestExecutor) TriggerDeferred(context.Context, model.TriggerDescriptor) (bool, error) {
	return false, nil
}
func (e *loopTestExecutor) InvokeNativeTrigger(context.Context, model.FunctionID, TriggerContext) (*model.Row, bool, error) {
	return nil, false, nil
}
func (e *loopTestExecutor) Report(context.Context, model.Error) {}

// loopTestFrame compiles a DO block and returns its frame plus the single loop
// statement it contains, so a test can exercise runLoop without a session.
func loopTestFrame(t *testing.T, body string, executor *loopTestExecutor) (*frame, statement) {
	t.Helper()
	parsed, err := zpgparse.Parse("DO $$ " + body + " $$")
	if err != nil {
		t.Fatalf("parse DO block: %v", err)
	}
	statement := parsed.Stmts[0].Stmt.GetDoStmt()
	if statement == nil {
		t.Fatal("source did not parse as DO")
	}
	program, _, err := (Compiler{Limits: DefaultLimits()}).CompileInline(t.Context(), statement, loopTestSnapshot{}, loopTestBindOptions())
	if err != nil {
		t.Fatalf("compile DO block: %v", err)
	}
	if len(program.statements) != 1 || len(program.statements[0].Body) == 0 {
		t.Fatalf("compiled program shape = %+v", program.statements)
	}
	loop := program.statements[0].Body[0]
	if loop.Kind != stmtLoop {
		t.Fatalf("first statement kind = %d, want loop", loop.Kind)
	}
	executor.columns = []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}}
	return program.newTestFrame(executor), loop
}

func (p *Program) newTestFrame(executor SQLExecutor) *frame {
	f := &frame{program: p, executor: executor, values: make([]model.Value, len(p.datums)), layouts: make([]model.TypeLayout, len(p.datums)), assigned: make([]bool, len(p.datums))}
	for i, d := range p.datums {
		f.layouts[i] = d.Layout
		if d.Layout.Type.Storage.Valid() {
			f.values[i] = model.NullVal(d.Layout.Type.Storage)
		}
		if d.Name == "found" {
			f.values[i], f.assigned[i] = model.BoolVal(false), true
		}
	}
	return f
}

func loopTestDatumIndex(t *testing.T, p *Program, name string) int {
	t.Helper()
	for i, d := range p.datums {
		if d.Name == name {
			return i
		}
	}
	t.Fatalf("no datum named %q in %+v", name, p.datums)
	return -1
}

func loopTestAssertError(t *testing.T, err error, code string) {
	t.Helper()
	var state *model.Error
	if !errors.As(err, &state) {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
	if state.Code != code {
		t.Fatalf("SQLSTATE = %s (%s), want %s", state.Code, state.Message, code)
	}
}

// A dynamic FOR binds its USING parameters positionally and assigns each row of
// the dynamically built command to the record loop variable.
func TestRunLoopDynamicForwardsUsingParametersAndRowTarget(t *testing.T) {
	executor := &loopTestExecutor{
		values: map[string]model.Value{
			"SELECT 'SELECT value FROM t'": model.StringVal("SELECT value FROM t"),
			"SELECT 7":                     model.Int64Val(7),
			"!SELECT value FROM t":         model.Int64Val(0),
		},
		rows: [][]model.Value{{model.Int64Val(4)}, {model.Int64Val(6)}},
	}
	f, loop := loopTestFrame(t, `DECLARE r record; BEGIN FOR r IN EXECUTE 'SELECT value FROM t' USING 7 LOOP END LOOP; END`, executor)
	if loop.LoopKind != "dynfors" {
		t.Fatalf("loop kind = %q, want dynfors", loop.LoopKind)
	}
	if _, err := f.runLoop(t.Context(), loop); err != nil {
		t.Fatalf("runLoop: %v", err)
	}

	var dynamic *SQLRequest
	for i := range executor.requests {
		text, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{executor.requests[i].Statement}})
		if err != nil {
			t.Fatal(err)
		}
		if text == "SELECT value FROM t" {
			dynamic = &executor.requests[i]
		}
	}
	if dynamic == nil {
		t.Fatalf("dynamic command never reached SQL execution: %+v", executor.requests)
	}
	if len(dynamic.Parameters) != 1 || dynamic.Parameters[0].Value.IntVal != 7 {
		t.Fatalf("USING parameters = %+v, want one int64 7", dynamic.Parameters)
	}

	target := loopTestDatumIndex(t, f.program, "r")
	got, err := json.Marshal(f.values[target])
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(6)}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("loop variable after iteration = %s, want last row %s", got, want)
	}
	found := loopTestDatumIndex(t, f.program, "found")
	if !f.assigned[found] || f.values[found].IntVal != 1 {
		t.Fatalf("FOUND = %+v assigned=%v, want true", f.values[found], f.assigned[found])
	}
}

// FOREACH ... SLICE yields sub-arrays of the requested dimensionality, keeping
// the rightmost dimensions' bounds, instead of individual elements.
func TestRunLoopForeachSliceYieldsSubArrays(t *testing.T) {
	array := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4)},
	}
	executor := &loopTestExecutor{values: map[string]model.Value{"SELECT ARRAY[ARRAY[1, 2], ARRAY[3, 4]]": array}}
	f, loop := loopTestFrame(t, `DECLARE x int[]; BEGIN FOREACH x SLICE 1 IN ARRAY ARRAY[[1,2],[3,4]] LOOP END LOOP; END`, executor)
	if loop.LoopKind != "foreach_a" || loop.Slice != 1 {
		t.Fatalf("loop kind=%q slice=%d, want foreach_a slice 1", loop.LoopKind, loop.Slice)
	}
	if _, err := f.runLoop(t.Context(), loop); err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	target := loopTestDatumIndex(t, f.program, "x")
	last := f.values[target]
	if last.Type != model.TypeArray || len(last.ArrayVal) != 2 {
		t.Fatalf("final slice = %+v, want a 2-element array", last)
	}
	if last.ArrayVal[0].IntVal != 3 || last.ArrayVal[1].IntVal != 4 {
		t.Fatalf("final slice elements = %+v, want {3,4}", last.ArrayVal)
	}
	if len(last.Dimensions) != 1 || last.Dimensions[0].Length != 2 || last.Dimensions[0].LowerBound != 1 {
		t.Fatalf("final slice dimensions = %+v, want one dimension length 2 lower bound 1", last.Dimensions)
	}
	found := loopTestDatumIndex(t, f.program, "found")
	if f.values[found].IntVal != 1 {
		t.Fatalf("FOUND = %+v, want true", f.values[found])
	}
}

// SLICE beyond the array's rank is rejected with the array-subscript SQLSTATE.
func TestRunLoopForeachSliceDimensionOutOfRange(t *testing.T) {
	array := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(1), model.Int64Val(2)},
	}
	executor := &loopTestExecutor{values: map[string]model.Value{"SELECT ARRAY[1, 2]": array}}
	f, loop := loopTestFrame(t, `DECLARE x int[]; BEGIN FOREACH x SLICE 2 IN ARRAY ARRAY[1,2] LOOP END LOOP; END`, executor)
	_, err := f.runLoop(t.Context(), loop)
	loopTestAssertError(t, err, "2202E")
	var state *model.Error
	if !errors.As(err, &state) || state.Message != "slice dimension (2) is out of the valid range 0..1" {
		t.Fatalf("message = %v, want PostgreSQL's slice range diagnostic", err)
	}
}

// A SLICE loop needs an array loop variable; PostgreSQL rejects a scalar one.
func TestRunLoopForeachSliceRequiresArrayVariable(t *testing.T) {
	array := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(1), model.Int64Val(2)},
	}
	executor := &loopTestExecutor{values: map[string]model.Value{"SELECT ARRAY[1, 2]": array}}
	f, loop := loopTestFrame(t, `DECLARE x int; BEGIN FOREACH x SLICE 1 IN ARRAY ARRAY[1,2] LOOP END LOOP; END`, executor)
	loopTestAssertError(t, f.mustRunLoopError(t, loop), "42804")
}

func (f *frame) mustRunLoopError(t *testing.T, loop statement) error {
	t.Helper()
	_, err := f.runLoop(t.Context(), loop)
	if err == nil {
		t.Fatal("runLoop succeeded, want an error")
	}
	return err
}

// FOREACH without SLICE still iterates individual elements.
func TestRunLoopForeachWithoutSliceIteratesElements(t *testing.T) {
	array := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3)},
	}
	executor := &loopTestExecutor{values: map[string]model.Value{"SELECT ARRAY[1, 2, 3]": array}}
	f, loop := loopTestFrame(t, `DECLARE x int; BEGIN FOREACH x IN ARRAY ARRAY[1,2,3] LOOP END LOOP; END`, executor)
	if loop.Slice != 0 {
		t.Fatalf("slice = %d, want 0", loop.Slice)
	}
	if _, err := f.runLoop(t.Context(), loop); err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	target := loopTestDatumIndex(t, f.program, "x")
	if f.values[target].Type != model.TypeInt64 || f.values[target].IntVal != 3 {
		t.Fatalf("final element = %+v, want int64 3", f.values[target])
	}
}
