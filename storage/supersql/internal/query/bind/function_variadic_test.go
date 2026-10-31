package bind_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func variadicRoot(t *testing.T, statement *bind.BoundStatement) (ir.ScalarID, ir.Scalar, ir.Signature) {
	t.Helper()
	graph := statement.Graph()
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		t.Fatal("missing projected function")
	}
	call, ok := graph.Scalar(binding.Expr)
	if !ok || call.Kind != ir.Call {
		t.Fatalf("projected expression = %+v, want function call", call)
	}
	signature, ok := graph.Signature(call.Signature)
	if !ok {
		t.Fatal("missing call signature")
	}
	return binding.Expr, call, signature
}

func TestVariadicBindingPreservesWideArguments(t *testing.T) {
	for _, function := range []struct {
		name, prefix string
		id           model.FunctionID
	}{
		{"concat", "", 3058},
		{"concat_ws", "','", 3059},
		{"format", "'%s'", 3539},
	} {
		for _, count := range []int{9, 100} {
			t.Run(fmt.Sprintf("%s/%d", function.name, count), func(t *testing.T) {
				args := make([]string, count)
				for i := range args {
					args[i] = strconv.Itoa(i + 1)
				}
				if function.prefix != "" {
					args[0] = function.prefix
				}
				statement, err := bindSQL(t, "SELECT "+function.name+"("+strings.Join(args, ",")+")", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				root, call, signature := variadicRoot(t, statement)
				if call.Function != function.id || signature.ParameterCount() != uint32(count) || call.OperandCount() != uint32(count) || signature.VariadicArray {
					t.Fatalf("call=%+v signature=%+v", call, signature)
				}
				for i := range args {
					if i == 0 && function.prefix != "" {
						continue
					}
					operand, _ := statement.Graph().Operand(root, uint32(i))
					value, err := statement.Graph().CloneConstant(t.Context(), operand, 1024)
					if err != nil || value.IsNull || value.IntVal != int64(i+1) {
						t.Fatalf("argument %d = %+v, %v", i, value, err)
					}
				}
			})
		}
	}
}

func TestVariadicBindingRejectsInvalidCalls(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"concat needs argument", "SELECT concat()", "42883"},
		{"separator needs argument", "SELECT concat_ws(',')", "42883"},
		{"fixed prefix type", "SELECT concat_ws(1,2)", "42883"},
		{"unknown parameter", "SELECT concat($1)", "42P18"},
		{"unknown format parameter", "SELECT format('%s',$1)", "42P18"},
		{"unknown explicit array", "SELECT format('%s',VARIADIC NULL)", "42804"},
		{"scalar explicit array", "SELECT concat(VARIADIC 1)", "42804"},
		{"nonvariadic marker", "SELECT lower(VARIADIC ARRAY['a'])", "42883"},
		{"too many arguments", "SELECT concat(" + strings.Repeat("1,", 100) + "1)", "54023"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, nil, nil)
			requireCode(t, err, scenario.code)
		})
	}
}

func TestVariadicBindingRetainsHeterogeneousTypes(t *testing.T) {
	statement, err := bindSQL(t, "SELECT concat(NULL,'x',true,1.25,ROW(3,'r'),ARRAY[2,4])", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, call, signature := variadicRoot(t, statement)
	want := []model.TypeID{25, 25, 16, 1700, 2249, 1007}
	if signature.ParameterCount() != uint32(len(want)) {
		t.Fatalf("parameters = %d, want %d", signature.ParameterCount(), len(want))
	}
	for i, id := range want {
		layout, _ := statement.Graph().SignatureParameter(call.Signature, uint32(i))
		typ, _ := statement.Graph().Type(layout)
		if typ.ID != id {
			t.Errorf("argument %d type = %d, want %d", i, typ.ID, id)
		}
	}
}

func TestVariadicPrefixInfersSharedParameter(t *testing.T) {
	statement, err := bindSQL(t, "SELECT format($1,$1)", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := statement.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != 25 {
		t.Fatalf("parameters = %+v, want one text parameter", parameters)
	}
	_, _, signature := variadicRoot(t, statement)
	if signature.Function != 3539 || signature.ParameterCount() != 2 {
		t.Fatalf("signature = %+v", signature)
	}
}

func TestFormatWithoutVariadicArgumentsSelectsFixedOverload(t *testing.T) {
	statement, err := bindSQL(t, "SELECT format('abc')", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, call, signature := variadicRoot(t, statement)
	if call.Function != 3540 || signature.ParameterCount() != 1 || signature.VariadicArray {
		t.Fatalf("call=%+v signature=%+v", call, signature)
	}
}

func TestVariadicArrayParameterOwnsNestedLayout(t *testing.T) {
	for _, marker := range []string{"", "VARIADIC "} {
		t.Run(marker, func(t *testing.T) {
			leaf := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 11, Collation: 950}}
			record := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeComposite, Modifier: -1}, Children: []model.TypeLayout{leaf}}
			array := model.TypeLayout{Type: model.Type{ID: 90002, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{record}}
			statement, err := bindSQL(t, "SELECT concat("+marker+"$1)", nil, []model.TypeLayout{array})
			if err != nil {
				t.Fatal(err)
			}
			array.Children[0].Children[0].Type.Modifier = 99
			_, call, signature := variadicRoot(t, statement)
			if signature.VariadicArray != (marker != "") {
				t.Fatalf("spread = %v for %q", signature.VariadicArray, marker)
			}
			layout, _ := statement.Graph().SignatureParameter(call.Signature, 0)
			for _, expected := range []model.Type{array.Type, record.Type, leaf.Type} {
				typ, _ := statement.Graph().Type(layout)
				if typ != expected {
					t.Fatalf("nested type = %+v, want %+v", typ, expected)
				}
				layout, _ = statement.Graph().LayoutChild(layout, 0)
			}
		})
	}
}

func TestVariadicExplicitArrayDoesNotSelectFixedUserOverload(t *testing.T) {
	snapshot := tableSnapshot(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 100}}
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "concat", Signature: "integer[]"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 'user'", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "values", Layout: array, Mode: "in"}}, Result: text, Language: "sql", Volatility: "immutable"})),
	}))
	for _, scenario := range []struct {
		marker   string
		path     []string
		function model.FunctionID
	}{
		{"", []string{"public", "pg_catalog"}, 90012},
		{"VARIADIC ", []string{"public", "pg_catalog"}, 3058},
		{"", []string{"pg_catalog", "public"}, 3058},
	} {
		sql := "SELECT concat(" + scenario.marker + "ARRAY[1,2])"
		parsed, err := zpgparse.Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		options := bindingOptions(sql)
		options.SearchPath = scenario.path
		statement, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, call, _ := variadicRoot(t, statement)
		if call.Function != scenario.function {
			t.Fatalf("%s path %v selected %d, want %d", sql, scenario.path, call.Function, scenario.function)
		}
	}
}

var errVariadicArgument = errors.New("variadic argument failed")

func TestVariadicArgumentsKeepSideEffectAndErrorOrder(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		fail      int64
		want      []int64
	}{
		{"expanded", "SELECT concat(public.observe(1),public.observe(2),public.observe(3),4,5,6,7,8,9)", 0, []int64{1, 2, 3}},
		{"spread", "SELECT concat(VARIADIC ARRAY[public.observe(1),public.observe(2),public.observe(3)])", 0, []int64{1, 2, 3}},
		{"ignored format arguments", "SELECT format('%s',public.observe(1),public.observe(2),public.observe(3))", 0, []int64{1, 2, 3}},
		{"first error", "SELECT concat(public.observe(1),public.observe(2),public.observe(3))", 2, []int64{1, 2}},
		{"typed expanded", "SELECT json_extract_path_text(public.observe(1)::text::json,public.observe(2)::text,public.observe(3)::text)", 0, []int64{1, 2, 3}},
		{"typed explicit array", "SELECT json_extract_path_text(public.observe(1)::text::json,VARIADIC ARRAY[public.observe(2)::text,public.observe(3)::text])", 0, []int64{1, 2, 3}},
		{"typed named array", "SELECT json_extract_path_text(from_json=>public.observe(1)::text::json,VARIADIC path_elems=>ARRAY[public.observe(2)::text,public.observe(3)::text])", 0, []int64{1, 2, 3}},
		{"typed first error", "SELECT json_extract_path_text(public.observe(1)::text::json,public.observe(2)::text,public.observe(3)::text)", 2, []int64{1, 2}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot := tableSnapshot(t)
			integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
			snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
				ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
				Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "observe", Signature: "integer"},
				Routine: new(compiledRoutine(t, snapshot, "SELECT value", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "value", Layout: integer, Mode: "in"}}, Result: integer, Language: "sql", Volatility: "volatile"})),
			}))
			statement, err := bindSQL(t, scenario.sql, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			root, call, _ := variadicRoot(t, statement)
			if call.Effects.Volatility != ir.Volatile || !call.Effects.MayError {
				t.Fatalf("call effects = %+v", call.Effects)
			}
			var observed []int64
			options := expr.Options{Limits: expr.DefaultLimits(), Functions: onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
				if function.ID != 90012 {
					t.Fatalf("unexpected routine %d", function.ID)
				}
				return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
					observed = append(observed, args[0].IntVal)
					if args[0].IntVal == scenario.fail {
						return model.Value{}, errVariadicArgument
					}
					return args[0], nil
				}, nil
			})}
			program, err := expr.Compile(t.Context(), statement.Graph(), []ir.ScalarID{root}, options)
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
			err = state.Eval(t.Context(), &input, nil, &output, expr.Direct)
			unexpectedFailure := scenario.fail == 0 && err != nil
			missingFailure := scenario.fail != 0 && !errors.Is(err, errVariadicArgument)
			if unexpectedFailure || missingFailure {
				t.Fatalf("evaluation error = %v", err)
			}
			if !slices.Equal(observed, scenario.want) {
				t.Fatalf("effects = %v, want %v", observed, scenario.want)
			}
		})
	}
}

func TestWideVariadicCallRetainsOuterCaptures(t *testing.T) {
	statement, err := bindSQL(t, "SELECT (SELECT concat(t.x,1,2,3,4,5,6,7,t.y)) FROM (VALUES(8,9)) t(x,y)", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		call, _ := graph.Scalar(id)
		if call.Kind != ir.Call || call.Function != 3058 {
			continue
		}
		if call.OperandCount() != 9 || graph.CaptureCount(call.Scope) != 2 {
			t.Fatalf("arguments=%d captures=%d", call.OperandCount(), graph.CaptureCount(call.Scope))
		}
		for i, position := range []uint32{0, 8} {
			operand, _ := graph.Operand(id, position)
			value, _ := graph.Scalar(operand)
			capture, ok := graph.Capture(call.Scope, uint32(i))
			if !ok || value.Kind != ir.Slot || capture != value.Slot {
				t.Fatalf("argument %d = %+v, capture=%d", position, value, capture)
			}
		}
		return
	}
	t.Fatal("missing correlated concat call")
}

func TestUserVariadicCallsRemainExplicitGaps(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "many", Signature: "integer[]"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 1", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "values", Layout: array, Mode: "variadic"}}, Result: integer, Language: "sql", Volatility: "immutable"})),
	}))
	for _, sql := range []string{"SELECT public.many()", "SELECT public.many(1,2)", "SELECT public.many(VARIADIC ARRAY[1,2])"} {
		_, err := bindSQL(t, sql, snapshot, nil)
		var missing *bind.MissingBindingError
		if !errors.As(err, &missing) {
			t.Fatalf("%s: error = %v, want missing variadic routine binding", sql, err)
		}
	}
}
