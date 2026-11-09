package bind_test

import (
	"context"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func freezeRoutineDefault(t *testing.T, sql string, target model.TypeLayout, snapshot metadataSnapshot, options bind.Options) *model.ExpressionDefinition {
	t.Helper()
	parsed, err := zpgparse.Parse("SELECT " + sql)
	if err != nil {
		t.Fatal(err)
	}
	options.Limits.SourceBytes = uint32(len(sql) + 7)
	node := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
	result, err := bind.BindRoutineDefault(t.Context(), node, target, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	return &result.Definition
}

func defaultRoutineDescriptor(t *testing.T, id model.ObjectID, oid model.OID, parent model.ObjectID, name string, parameters []model.RoutineParameter, result model.TypeLayout, volatility string) *model.Descriptor {
	t.Helper()
	return testDescriptor(t, model.DescriptorSpec{ID: id, OID: oid, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: parent, Kind: model.ObjectRoutine, Name: name}, Routine: new(compiledRoutine(t, tableSnapshot(t), "SELECT 0", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: parameters, Result: result, Language: "sql", Volatility: volatility}))})
}

func TestRoutineDefaultsDistinguishOmissionFromExplicitNull(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	fallback := freezeRoutineDefault(t, "5", integer, snapshot, bindingOptions("SELECT 5"))
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "combine", []model.RoutineParameter{{Name: "x", Layout: integer, Mode: "in"}, {Name: "y", Layout: integer, Mode: "in", Default: fallback}}, integer, "immutable"))
	statement, err := bindSQL(t, `SELECT combine(1),combine(y=>3,x=>2),combine(4,y=>NULL)`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := evaluateRecordExpression(t, statement, onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
		if function.ID != 90040 {
			return nil, &builtin.UnknownFunctionError{ID: function.ID}
		}
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			if args[1].IsNull {
				return model.Int64Val(-1), nil
			}
			return model.Int64Val(args[0].IntVal*10 + args[1].IntVal), nil
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{15, 23, -1} {
		if values[i].IntVal != want {
			t.Fatalf("call %d=%v, want %d", i, values[i], want)
		}
	}
}

func TestRoutineDefaultFreezesLiteralInputSettings(t *testing.T) {
	snapshot := tableSnapshot(t)
	interval := model.TypeLayout{Type: model.Type{ID: 1186, Storage: model.TypeInterval, Modifier: -1}}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
	options := bindingOptions("SELECT interval '-1 2:00'")
	options.Builtin.IntervalStyle = builtin.IntervalSQLStandard
	fallback := freezeRoutineDefault(t, "interval '-1 2:00'", interval, snapshot, options)
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "negative_time", []model.RoutineParameter{{Name: "value", Layout: interval, Mode: "in", Default: fallback}}, boolean, "immutable"))
	statement, err := bindSQL(t, `SELECT negative_time()`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := evaluateRecordExpression(t, statement, onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
		if function.ID != 90040 {
			return nil, &builtin.UnknownFunctionError{ID: function.ID}
		}
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			return model.BoolVal(args[0].IntervalVal.Microseconds < 0), nil
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if values[0].IsNull || values[0].IntVal != 1 {
		t.Fatalf("frozen SQL-standard interval predicate=%v", values[0])
	}
	options.Builtin.IntervalStyle = builtin.IntervalPostgres
	bound, err := bind.BindColumnDefault(t.Context(), fallback, interval, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	program, err := expr.Compile(t.Context(), bound.Graph, []ir.ScalarID{bound.Root}, expr.Options{Limits: expr.DefaultLimits(), Builtin: bound.Builtin})
	if err != nil {
		t.Fatal(err)
	}
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	input, output := columnar.Batch{Length: 1}, columnar.Batch{}
	if err := state.Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		t.Fatal(err)
	}
	value, _, err := columnar.CopyValue(t.Context(), &output.Columns[0], 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull || value.IntervalVal.Microseconds != -7200000000 {
		t.Fatalf("frozen column default=%v, want negative two hours", value)
	}
}

func TestRoutineDefaultKeepsCallableIdentityAcrossSearchPaths(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 41, 90041, 10, "next_value", nil, integer, "volatile"))
	fallback := freezeRoutineDefault(t, "next_value()", integer, snapshot, bindingOptions("SELECT next_value()"))
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "combine", []model.RoutineParameter{{Name: "x", Layout: integer, Mode: "in"}, {Name: "y", Layout: integer, Mode: "in", Default: fallback}}, integer, "immutable"))
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{ID: 50, OID: 10050, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "shadow"}, Schema: &model.SchemaMetadata{Owner: 50}}), defaultRoutineDescriptor(t, 51, 90051, 50, "next_value", nil, integer, "volatile"))
	const sql = `SELECT combine(1),combine(2)`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	options.SearchPath = []string{"shadow", "public"}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := int64(0)
	values, err := evaluateRecordExpression(t, statement, onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
		switch function.ID {
		case 90040:
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				return model.Int64Val(args[0].IntVal*10 + args[1].IntVal), nil
			}, nil
		case 90041:
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				count++
				return model.Int64Val(count), nil
			}, nil
		case 90051:
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				return model.Int64Val(99), nil
			}, nil
		default:
			return nil, &builtin.UnknownFunctionError{ID: function.ID}
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if values[0].IntVal != 11 || values[1].IntVal != 22 {
		t.Fatalf("frozen callable results=%v, want 11 and 22", values)
	}
}

func TestRoutineDefaultConstantEncodingPreservesNaN(t *testing.T) {
	snapshot := tableSnapshot(t)
	floating := model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64, Modifier: -1}}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
	fallback := freezeRoutineDefault(t, `'NaN'::float8`, floating, snapshot, bindingOptions(`SELECT 'NaN'::float8`))
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "default_is_nan", []model.RoutineParameter{{Name: "value", Layout: floating, Mode: "in", Default: fallback}}, boolean, "immutable"))
	statement, err := bindSQL(t, `SELECT default_is_nan()`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := evaluateRecordExpression(t, statement, onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
		if function.ID != 90040 {
			return nil, &builtin.UnknownFunctionError{ID: function.ID}
		}
		return func(_ context.Context, arguments []model.Value, _ builtin.CallBudget) (model.Value, error) {
			return model.BoolVal(math.IsNaN(arguments[0].FloatVal)), nil
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if values[0].IntVal != 1 {
		t.Fatalf("encoded NaN predicate=%v", values[0])
	}
}

func TestRoutineDefaultRejectsTruncatedEncoding(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	fallback := freezeRoutineDefault(t, "1", integer, snapshot, bindingOptions("SELECT 1"))
	fallback.Encoding = fallback.Encoding[:len(fallback.Encoding)-1]
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "broken_default", []model.RoutineParameter{{Name: "value", Layout: integer, Mode: "in", Default: fallback}}, integer, "immutable"))
	_, err := bindSQL(t, `SELECT broken_default()`, snapshot, nil)
	requireCode(t, err, "22023")
	if _, err := bindSQL(t, `SELECT broken_default(2)`, snapshot, nil); err != nil {
		t.Fatalf("supplied argument reached omitted default: %v", err)
	}
}

func TestRoutineDefaultRetainsExplicitCollationConflicts(t *testing.T) {
	snapshot := tableSnapshot(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1, Collation: 950}}
	left := freezeRoutineDefault(t, `'a' COLLATE "POSIX"`, text, snapshot, bindingOptions(`SELECT 'a' COLLATE "POSIX"`))
	right := freezeRoutineDefault(t, `'b' COLLATE "C"`, text, snapshot, bindingOptions(`SELECT 'b' COLLATE "C"`))
	snapshot.descriptors = append(snapshot.descriptors, defaultRoutineDescriptor(t, 40, 90040, 10, "collated_defaults", []model.RoutineParameter{{Name: "a", Layout: text, Mode: "in", Default: left}, {Name: "b", Layout: text, Mode: "in", Default: right}}, text, "immutable"))
	_, err := bindSQL(t, `SELECT collated_defaults()`, snapshot, nil)
	requireCode(t, err, "42P21")
}

func TestFrozenRegclassDefaultPreservesIdentityAcrossSequenceAlter(t *testing.T) {
	snapshot := sequenceSnapshot(t)
	original := snapshot.descriptors[len(snapshot.descriptors)-1]
	layout := model.TypeLayout{Type: model.Type{ID: 2205, Modifier: -1, Storage: model.TypeInt64}}
	options := bindingOptions(`SELECT 'seq'::regclass`)
	definition := freezeRoutineDefault(t, `'seq'::regclass`, layout, snapshot, options)
	spec, err := original.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Version++
	spec.Sequence.Start = 9
	spec.Sequence.Incarnation = 901
	snapshot.descriptors[len(snapshot.descriptors)-1] = testDescriptor(t, spec)
	bound, err := bind.BindColumnDefault(t.Context(), definition, layout, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	program, err := expr.Compile(t.Context(), bound.Graph, []ir.ScalarID{bound.Root}, expr.Options{Limits: expr.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	}()
	input := columnar.Batch{Length: 1}
	var output columnar.Batch
	if err := state.Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		t.Fatal(err)
	}
	value, _, err := columnar.CopyValue(t.Context(), &output.Columns[0], 0, 1024)
	if err != nil || value.IntVal != int64(original.OID()) {
		t.Fatalf("frozen relation OID = %v, %v; want %d", value, err, original.OID())
	}
	spec.Version = 1
	spec.Sequence.Identity, spec.Sequence.Incarnation = 902, 902
	snapshot.descriptors[len(snapshot.descriptors)-1] = testDescriptor(t, spec)
	_, err = bind.BindColumnDefault(t.Context(), definition, layout, snapshot, options)
	requireCode(t, err, "0A000")
}
