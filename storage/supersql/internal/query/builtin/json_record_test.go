package builtin

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func jsonRecordTestLayout(field model.TypeLayout) model.TypeLayout {
	layout := inputRecordLayout(field)
	layout.FieldNames = []string{"a"}
	return layout
}

func jsonRecordTestCallable(t *testing.T, id model.FunctionID, input model.TypeID, layout model.TypeLayout) Callable {
	t.Helper()
	call, err := ResolveTyped(t.Context(), id, []model.TypeLayout{typedQuoteLayout(input, model.TypeJSONB)}, layout, ResolveOptions{Collation: 950, DefaultCollation: 950}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func TestJSONRecordArrayKeepsArraysAtInferredLeafDepth(t *testing.T) {
	for _, source := range []struct {
		name string
		id model.TypeID
		function model.FunctionID
	}{
		{"json", 114, 3204},
		{"jsonb", 3802, 3490},
	} {
		for _, element := range []struct {
			name string
			id model.TypeID
			storage model.DataType
		}{
			{"json", 114, model.TypeJSONB},
			{"jsonb", 3802, model.TypeJSONB},
			{"text", 25, model.TypeString},
		} {
			t.Run(source.name+"/"+element.name, func(t *testing.T) {
				layout := jsonRecordTestLayout(replacementLayout(t, element.id, element.storage))
				call := jsonRecordTestCallable(t, source.function, source.id, layout)
				got, err := call.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: `{"a":[0,[1,2],{"k":3},null]}`}}, CallBudget{OutputBytes: 1<<16, MemoryBytes: 1<<20})
				if err != nil {
					t.Fatal(err)
				}
				array := got.ArrayVal[0]
				if !slices.Equal(array.Dimensions, []model.ArrayDimension{{LowerBound: 1, Length: 4}}) || len(array.ArrayVal) != 4 {
					t.Fatalf("SQL array shape = %v, elements = %d", array.Dimensions, len(array.ArrayVal))
				}
				want := []string{"0", "[1,2]", `{"k":3}`}
				if source.id == 3802 || element.id == 3802 {
					want[1], want[2] = "[1, 2]", `{"k": 3}`
				}
				for i, text := range want {
					value := array.ArrayVal[i]
					if value.Type != element.storage || value.IsNull || value.StrVal != text {
						t.Errorf("element %d = %v, want %q", i, value, text)
					}
				}
				if !array.ArrayVal[3].IsNull || array.ArrayVal[3].Type != element.storage {
					t.Errorf("JSON null element = %v", array.ArrayVal[3])
				}
			})
		}
	}
}

func TestJSONRecordArrayInfersDepthFromFirstObjectOrEmptyArray(t *testing.T) {
	for _, source := range []struct {
		name string
		id model.TypeID
		function model.FunctionID
	}{{"json", 114, 3204}, {"jsonb", 3802, 3490}} {
		layout := jsonRecordTestLayout(replacementLayout(t, 3802, model.TypeJSONB))
		call := jsonRecordTestCallable(t, source.function, source.id, layout)
		for _, tc := range []struct {
			name, text string
			dimensions []model.ArrayDimension
			values []string
		}{
			{"object leaf", `{"a":[{"k":1},[2]]}`, []model.ArrayDimension{{LowerBound: 1, Length: 2}}, []string{`{"k": 1}`, "[2]"}},
			{"nested leaf arrays", `{"a":[[1,[2]],[3,[4,5]]]}`, []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 2}}, []string{"1", "[2]", "3", "[4, 5]"}},
			{"empty subarrays", `{"a":[[],[]]}`, nil, nil},
			{"deep JSON leaf", `{"a":[0,[[[[[[[1]]]]]]]]}`, []model.ArrayDimension{{LowerBound: 1, Length: 2}}, []string{"0", "[[[[[[[1]]]]]]]"}},
		} {
			t.Run(source.name+"/"+tc.name, func(t *testing.T) {
				got, err := call.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: tc.text}}, CallBudget{OutputBytes: 1<<16, MemoryBytes: 1<<20})
				if err != nil {
					t.Fatal(err)
				}
				array := got.ArrayVal[0]
				if array.IsNull || !slices.Equal(array.Dimensions, tc.dimensions) || len(array.ArrayVal) != len(tc.values) {
					t.Fatalf("array = %v, want dimensions %v and elements %v", array, tc.dimensions, tc.values)
				}
				for i, want := range tc.values {
					if array.ArrayVal[i].IsNull || array.ArrayVal[i].StrVal != want {
						t.Errorf("element %d = %v, want %q", i, array.ArrayVal[i], want)
					}
				}
			})
		}
	}
}

func TestJSONRecordArrayRejectsRaggedDimensions(t *testing.T) {
	for _, source := range []struct {
		name string
		id model.TypeID
		function model.FunctionID
	}{{"json", 114, 3204}, {"jsonb", 3802, 3490}} {
		call := jsonRecordTestCallable(t, source.function, source.id, jsonRecordTestLayout(replacementLayout(t, 3802, model.TypeJSONB)))
		for _, text := range []string{`{"a":[[],[1]]}`, `{"a":[[1],[2,3]]}`, `{"a":[[1],0]}`, `{"a":[[1],null]}`} {
			t.Run(source.name+"/"+text, func(t *testing.T) {
				_, err := call.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: text}}, CallBudget{OutputBytes: 1<<16, MemoryBytes: 1<<20})
				requireReplacementSQLState(t, err, "22P02")
			})
		}
	}
}

func TestJSONRecordArrayEarlierConversionErrorPrecedesLaterShapeError(t *testing.T) {
	call := jsonRecordTestCallable(t, 3490, 3802, jsonRecordTestLayout(replacementLayout(t, 23, model.TypeInt64)))
	for _, text := range []string{`{"a":[[2147483648],[1,2]]}`, `{"a":[[2147483648],null]}`} {
		_, err := call.Call(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: text}}, CallBudget{OutputBytes: 1<<16, MemoryBytes: 1<<20})
		requireReplacementSQLState(t, err, "22003")
	}
}

func TestJSONRecordValidSQLNullSemantics(t *testing.T) {
	layout := jsonRecordTestLayout(typedQuoteLayout(23, model.TypeInt64))
	call, err := ResolveTyped(t.Context(), 6338, []model.TypeLayout{layout, typedQuoteLayout(3802, model.TypeJSONB)}, typedQuoteLayout(16, model.TypeBoolean), ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		base, input model.Value
		null, valid bool
	}{
		{"both SQL null", model.NullVal(model.TypeComposite), model.NullVal(model.TypeJSONB), true, false},
		{"nonnull base SQL null input", model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(7)}}, model.NullVal(model.TypeJSONB), false, true},
		{"null base object input", model.NullVal(model.TypeComposite), model.Value{Type: model.TypeJSONB, StrVal: `{"a":1}`}, false, true},
		{"JSON null input", model.NullVal(model.TypeComposite), model.Value{Type: model.TypeJSONB, StrVal: `null`}, false, false},
		{"invalid field input", model.NullVal(model.TypeComposite), model.Value{Type: model.TypeJSONB, StrVal: `{"a":"bad"}`}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := call.Call(t.Context(), []model.Value{tc.base, tc.input}, CallBudget{OutputBytes: 1, MemoryBytes: 1<<20})
			if err != nil || got.Type != model.TypeBoolean || got.IsNull != tc.null || (!tc.null && (got.IntVal != 0) != tc.valid) {
				t.Fatalf("validity = %v, %v; want null=%t, valid=%t", got, err, tc.null, tc.valid)
			}
		})
	}
}
