package builtin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var recordComparisonIDs = [...]model.FunctionID{2981, 2982, 2983, 2984, 2985, 2986, 2987}

func recordCompareLeaf(id model.TypeID, storage model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: storage}}
}

func recordCompareLayout(id model.TypeID, fields ...model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: model.TypeComposite}, Children: fields}
}

func recordCompareResult(id model.FunctionID) model.TypeLayout {
	if id == 2987 {
		return recordCompareLeaf(23, model.TypeInt64)
	}
	return recordCompareLeaf(16, model.TypeBoolean)
}

func comparedRecord(fields ...model.Value) model.Value {
	return model.Value{Type: model.TypeComposite, ArrayVal: fields}
}

func recordCompareCallable(t *testing.T, id model.FunctionID, left, right model.TypeLayout, options ResolveOptions) Callable {
	t.Helper()
	function, err := ResolveTyped(t.Context(), id, []model.TypeLayout{left, right}, recordCompareResult(id), options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireRecordCompare(t *testing.T, function Callable, left, right model.Value, order int64) {
	t.Helper()
	var want model.Value
	switch function.Signature().ID {
	case 2981:
		want = model.BoolVal(order == 0)
	case 2982:
		want = model.BoolVal(order != 0)
	case 2983:
		want = model.BoolVal(order < 0)
	case 2984:
		want = model.BoolVal(order > 0)
	case 2985:
		want = model.BoolVal(order <= 0)
	case 2986:
		want = model.BoolVal(order >= 0)
	default:
		want = model.Int64Val(order)
	}
	got, err := function.Call(t.Context(), []model.Value{left, right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != want.Type || got.IntVal != want.IntVal {
		t.Fatalf("record function %d = %v, %v; want %v", function.Signature().ID, got, err, want)
	}
}

func TestRecordComparisonIdentitiesAndNullOrder(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	null := model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name        string
		fields      []model.TypeLayout
		left, right model.Value
		order       int64
	}{
		{"empty records", nil, comparedRecord(), comparedRecord(), 0},
		{"equal null fields", []model.TypeLayout{integer}, comparedRecord(null), comparedRecord(null), 0},
		{"null sorts last", []model.TypeLayout{integer}, comparedRecord(null), comparedRecord(model.Int64Val(math.MaxInt32)), 1},
		{"nonnull precedes null", []model.TypeLayout{integer}, comparedRecord(model.Int64Val(math.MinInt32)), comparedRecord(null), -1},
		{"continue after null pair", []model.TypeLayout{integer, integer}, comparedRecord(null, model.Int64Val(2)), comparedRecord(null, model.Int64Val(1)), 1},
		{"earlier field decides", []model.TypeLayout{integer, integer}, comparedRecord(model.Int64Val(1), model.Int64Val(9)), comparedRecord(model.Int64Val(2), model.Int64Val(0)), -1},
		{"equal nonnull fields", []model.TypeLayout{integer, integer}, comparedRecord(model.Int64Val(1), model.Int64Val(2)), comparedRecord(model.Int64Val(1), model.Int64Val(2)), 0},
	} {
		for _, id := range recordComparisonIDs {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				left := recordCompareLayout(80001, tc.fields...)
				right := recordCompareLayout(80002, tc.fields...)
				function := recordCompareCallable(t, id, left, right, ResolveOptions{})
				requireRecordCompare(t, function, tc.left, tc.right, tc.order)
			})
		}
	}
}

func TestRecordComparisonUsesSelectedSQLSemantics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          model.TypeID
		storage     model.DataType
		left, right model.Value
		order       int64
	}{
		{"numeric scales", 1700, model.TypeNumeric, model.Value{Type: model.TypeNumeric, StrVal: "1.00"}, model.Value{Type: model.TypeNumeric, StrVal: "1"}, 0},
		{"numeric magnitude", 1700, model.TypeNumeric, model.Value{Type: model.TypeNumeric, StrVal: "2"}, model.Value{Type: model.TypeNumeric, StrVal: "10"}, -1},
		{"float NaN high", 701, model.TypeFloat64, model.Float64Val(math.NaN()), model.Float64Val(math.Inf(1)), 1},
		{"bpchar padding", 1042, model.TypeString, model.StringVal("a  "), model.StringVal("a"), 0},
		{"varchar padding significant", 1043, model.TypeString, model.StringVal("a  "), model.StringVal("a"), 1},
		{"C ordering normalized", 25, model.TypeString, model.StringVal("a"), model.StringVal("z"), -1},
	} {
		for _, id := range recordComparisonIDs {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				left, right := recordCompareLeaf(tc.id, tc.storage), recordCompareLeaf(tc.id, tc.storage)
				left.Type.Modifier, right.Type.Modifier = 7, 15
				left.Type.Collation, right.Type.Collation = 950, 950
				function := recordCompareCallable(t, id, recordCompareLayout(2249, left), recordCompareLayout(2249, right), ResolveOptions{})
				requireRecordCompare(t, function, comparedRecord(tc.left), comparedRecord(tc.right), tc.order)
			})
		}
	}
}

func TestRecordComparisonTypeAndCountErrorsFollowEqualPrefix(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	bigint := recordCompareLeaf(20, model.TypeInt64)
	json := recordCompareLeaf(114, model.TypeJSONB)
	text := recordCompareLeaf(25, model.TypeString)
	for _, tc := range []struct {
		name                    string
		leftFields, rightFields []model.TypeLayout
		left, right             model.Value
		state                   string
		order                   int64
	}{
		{"unreached type mismatch", []model.TypeLayout{integer, integer}, []model.TypeLayout{integer, bigint}, comparedRecord(model.Int64Val(1), model.Int64Val(0)), comparedRecord(model.Int64Val(2), model.Int64Val(0)), "", -1},
		{"type mismatch before nulls", []model.TypeLayout{integer, integer}, []model.TypeLayout{integer, bigint}, comparedRecord(model.Int64Val(1), model.NullVal(model.TypeInt64)), comparedRecord(model.Int64Val(1), model.NullVal(model.TypeInt64)), "42804", 0},
		{"type mismatch before absent operator", []model.TypeLayout{json}, []model.TypeLayout{text}, comparedRecord(model.NullVal(model.TypeJSONB)), comparedRecord(model.NullVal(model.TypeString)), "42804", 0},
		{"unreached extra field", []model.TypeLayout{integer}, []model.TypeLayout{integer, json}, comparedRecord(model.Int64Val(2)), comparedRecord(model.Int64Val(1), model.NullVal(model.TypeJSONB)), "", 1},
		{"equal prefix before count mismatch", []model.TypeLayout{integer}, []model.TypeLayout{integer, json}, comparedRecord(model.Int64Val(1)), comparedRecord(model.Int64Val(1), model.NullVal(model.TypeJSONB)), "42804", 0},
		{"no shared fields", nil, []model.TypeLayout{json}, comparedRecord(), comparedRecord(model.NullVal(model.TypeJSONB)), "42804", 0},
		{"lookup error before count mismatch", []model.TypeLayout{json}, []model.TypeLayout{json, integer}, comparedRecord(model.NullVal(model.TypeJSONB)), comparedRecord(model.NullVal(model.TypeJSONB), model.Int64Val(1)), "42883", 0},
	} {
		for _, id := range recordComparisonIDs {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				function := recordCompareCallable(t, id, recordCompareLayout(80001, tc.leftFields...), recordCompareLayout(80002, tc.rightFields...), ResolveOptions{})
				if tc.state == "" {
					requireRecordCompare(t, function, tc.left, tc.right, tc.order)
					return
				}
				_, err := function.Call(t.Context(), []model.Value{tc.left, tc.right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireArrayPositionSQLState(t, err, tc.state)
			})
		}
	}
}

func TestRecordComparisonAbsentOperatorsAreReachedBeforeNulls(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	json := recordCompareLeaf(114, model.TypeJSONB)
	for _, id := range recordComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			layout := recordCompareLayout(2249, integer, json)
			function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
			for _, pair := range [][2]model.Value{
				{model.NullVal(model.TypeJSONB), model.NullVal(model.TypeJSONB)},
				{model.NullVal(model.TypeJSONB), {Type: model.TypeJSONB, StrVal: "{}"}},
				{{Type: model.TypeJSONB, StrVal: "{}"}, {Type: model.TypeJSONB, StrVal: "{}"}},
			} {
				left := comparedRecord(model.Int64Val(1), pair[0])
				right := comparedRecord(model.Int64Val(2), pair[1])
				requireRecordCompare(t, function, left, right, -1)
				right.ArrayVal[0] = model.Int64Val(1)
				_, err := function.Call(t.Context(), []model.Value{left, right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireArrayPositionSQLState(t, err, "42883")
			}
			for _, pair := range [][2]model.Value{
				{model.NullVal(model.TypeComposite), comparedRecord()},
				{comparedRecord(), model.NullVal(model.TypeComposite)},
			} {
				result, err := function.Call(t.Context(), pair[:], CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !result.IsNull || result.Type != recordCompareResult(id).Type.Storage {
					t.Fatalf("strict NULL comparison = %v, %v", result, err)
				}
			}
		})
	}
}

func TestRecordComparisonUsesNestedLayouts(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	text := recordCompareLeaf(25, model.TypeString)
	text.Type.Collation = 950
	inner := recordCompareLayout(80003, text, integer)
	array := model.TypeLayout{Type: model.Type{ID: 80004, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{inner}}
	layout := recordCompareLayout(2249, array, integer)
	left := comparedRecord(replacementArray(comparedRecord(model.StringVal("a"), model.Int64Val(7))), model.Int64Val(9))
	right := comparedRecord(replacementArray(comparedRecord(model.StringVal("a"), model.Int64Val(8))), model.Int64Val(0))
	for _, id := range recordComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
			requireRecordCompare(t, function, left, right, -1)
			requireRecordCompare(t, function, right, right, 0)
		})
	}
}

func TestRecordComparisonNestedErrorsWaitForNestedInvocation(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	leftNested := recordCompareLayout(80003, integer)
	rightNested := recordCompareLayout(80003, recordCompareLeaf(20, model.TypeInt64))
	for _, id := range []model.FunctionID{2981, 2987} {
		function := recordCompareCallable(t, id, recordCompareLayout(2249, leftNested), recordCompareLayout(2249, rightNested), ResolveOptions{})
		null := comparedRecord(model.NullVal(model.TypeComposite))
		requireRecordCompare(t, function, null, null, 0)
		value := comparedRecord(comparedRecord(model.Int64Val(1)))
		_, err := function.Call(t.Context(), []model.Value{value, value}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireArrayPositionSQLState(t, err, "42804")
	}
}

func TestRecordComparisonFieldCollationControlsInvocation(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	for _, id := range []model.FunctionID{2981, 2987} {
		for _, tc := range []struct {
			name        string
			left, right model.CollationID
			options     ResolveOptions
			state       string
		}{
			{"field C ignores outer collation", 950, 950, ResolveOptions{}, ""},
			{"shared default uses database collation", 100, 100, ResolveOptions{DefaultCollation: 950}, ""},
			{"mismatch ignores outer C", 950, 951, ResolveOptions{Collation: 950}, "42P22"},
			{"indeterminate ignores outer C", 0, 0, ResolveOptions{Collation: 950}, "42P22"},
		} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				leftField, rightField := recordCompareLeaf(25, model.TypeString), recordCompareLeaf(25, model.TypeString)
				leftField.Type.Collation, rightField.Type.Collation = tc.left, tc.right
				function := recordCompareCallable(t, id, recordCompareLayout(2249, integer, leftField), recordCompareLayout(2249, integer, rightField), tc.options)
				left := comparedRecord(model.Int64Val(1), model.StringVal("a"))
				right := comparedRecord(model.Int64Val(2), model.StringVal("a"))
				requireRecordCompare(t, function, left, right, -1)
				right.ArrayVal[0] = model.Int64Val(1)
				if tc.state != "" {
					_, err := function.Call(t.Context(), []model.Value{left, right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireArrayPositionSQLState(t, err, tc.state)
				} else {
					requireRecordCompare(t, function, left, right, 0)
				}
				left.ArrayVal[1] = model.NullVal(model.TypeString)
				requireRecordCompare(t, function, left, right, 1)
				right.ArrayVal[1] = model.NullVal(model.TypeString)
				requireRecordCompare(t, function, left, right, 0)
			})
		}
	}
}

func TestRecordComparisonRejectsMalformedValuesWhenReached(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	layout := recordCompareLayout(2249, integer, integer)
	for _, id := range []model.FunctionID{2981, 2987} {
		function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
		valid := comparedRecord(model.Int64Val(1), model.Int64Val(2))
		for _, malformed := range []model.Value{
			model.StringVal("not a record"),
			comparedRecord(),
			comparedRecord(model.Int64Val(1)),
			comparedRecord(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3)),
			{Type: model.TypeComposite, ArrayVal: valid.ArrayVal, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}},
			comparedRecord(model.NullVal(model.TypeString), model.Int64Val(2)),
		} {
			_, err := function.Call(t.Context(), []model.Value{malformed, valid}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireArrayPositionSQLState(t, err, "42804")
		}
		left := comparedRecord(model.Int64Val(0), model.StringVal("bad int4"))
		requireRecordCompare(t, function, left, valid, -1)
		left.ArrayVal[0] = model.Int64Val(1)
		_, err := function.Call(t.Context(), []model.Value{left, valid}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireArrayPositionSQLState(t, err, "42804")
		left.ArrayVal[1] = model.Int64Val(math.MaxInt32 + 1)
		_, err = function.Call(t.Context(), []model.Value{left, valid}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireArrayPositionSQLState(t, err, "22003")
		left.ArrayVal[1] = model.Int64Val(1)
		requireRecordCompare(t, function, left, valid, -1)
	}
}

func TestRecordComparisonRejectsInvalidDeclarationsBeforeSelection(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	for _, tc := range []struct {
		name   string
		mutate func(*model.TypeLayout, *model.TypeLayout)
	}{
		{"noncomposite", func(left, _ *model.TypeLayout) { *left = integer }},
		{"known identity with wrong storage", func(left, _ *model.TypeLayout) { left.Children[0].Type.Storage = model.TypeString }},
		{"invalid trailing field despite count mismatch", func(left, _ *model.TypeLayout) { left.Children = append(left.Children, model.TypeLayout{}) }},
		{"missing nested array element", func(left, _ *model.TypeLayout) { left.Children[1] = recordCompareLeaf(1007, model.TypeArray) }},
		{"contradictory nested array element", func(left, _ *model.TypeLayout) {
			left.Children[1] = replacementLayout(t, 23, model.TypeInt64)
			left.Children[1].Children[0].Type.ID = 20
		}},
		{"scalar with children", func(left, _ *model.TypeLayout) { left.Children[0].Children = []model.TypeLayout{integer} }},
		{"cyclic field layout", func(left, _ *model.TypeLayout) { left.Children[1] = *left }},
		{"wrong result identity", func(_, result *model.TypeLayout) { result.Type.ID = 25 }},
		{"wrong result storage", func(_, result *model.TypeLayout) { result.Type.Storage = model.TypeString }},
	} {
		for _, id := range []model.FunctionID{2981, 2987} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				left := recordCompareLayout(2249, integer, recordCompareLeaf(3802, model.TypeJSONB))
				right := recordCompareLayout(2249, integer, recordCompareLeaf(3802, model.TypeJSONB))
				result := recordCompareResult(id)
				tc.mutate(&left, &result)
				_, err := ResolveTyped(t.Context(), id, []model.TypeLayout{left, right}, result, ResolveOptions{}, 1<<20)
				requireArrayPositionSQLState(t, err, "42804")
			})
		}
	}
}

func TestRecordComparisonMetadataIsOwnedAndBounded(t *testing.T) {
	integer := recordCompareLeaf(23, model.TypeInt64)
	for _, id := range []model.FunctionID{2981, 2987} {
		for _, field := range []model.TypeLayout{
			integer,
			recordCompareLeaf(114, model.TypeJSONB),
			recordCompareLeaf(3802, model.TypeJSONB),
			recordCompareLayout(80003, integer),
		} {
			t.Run(fmt.Sprintf("%d/%d", id, field.Type.ID), func(t *testing.T) {
				layout := recordCompareLayout(2249, field)
				function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
				for _, maximum := range []uint64{0, function.OwnedBytes() - 1} {
					_, err := ResolveTyped(t.Context(), id, []model.TypeLayout{layout, layout}, recordCompareResult(id), ResolveOptions{}, maximum)
					requireArrayPositionSQLState(t, err, "54000")
				}
				_, err := ResolveTyped(t.Context(), id, []model.TypeLayout{layout, layout}, recordCompareResult(id), ResolveOptions{}, function.OwnedBytes())
				if err != nil {
					t.Fatalf("reported metadata bound %d rejected: %v", function.OwnedBytes(), err)
				}
			})
		}
		layout := recordCompareLayout(2249, recordCompareLayout(80003, integer))
		function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
		layout.Children[0].Children[0] = recordCompareLeaf(114, model.TypeString)
		layout.Children = nil
		requireRecordCompare(t, function, comparedRecord(comparedRecord(model.Int64Val(1))), comparedRecord(comparedRecord(model.Int64Val(2))), -1)
		wide := recordCompareLayout(2249, make([]model.TypeLayout, 4096)...)
		for i := range wide.Children {
			wide.Children[i] = integer
		}
		_, err := ResolveTyped(t.Context(), id, []model.TypeLayout{wide, recordCompareLayout(2249)}, recordCompareResult(id), ResolveOptions{}, 1024)
		requireArrayPositionSQLState(t, err, "54000")
	}
}

func TestRecordComparisonCancellation(t *testing.T) {
	layout := recordCompareLayout(2249, make([]model.TypeLayout, 128)...)
	value := comparedRecord(make([]model.Value, 128)...)
	for i := range layout.Children {
		layout.Children[i] = recordCompareLeaf(23, model.TypeInt64)
		value.ArrayVal[i] = model.NullVal(model.TypeInt64)
	}
	for _, id := range []model.FunctionID{2981, 2987} {
		function := recordCompareCallable(t, id, layout, layout, ResolveOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := ResolveTyped(ctx, id, []model.TypeLayout{layout, layout}, recordCompareResult(id), ResolveOptions{}, 1<<20)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled preparation = %v", err)
		}
		_, err = function.Call(ctx, []model.Value{value, value}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled invocation = %v", err)
		}
		ctx, cancel = context.WithCancel(t.Context())
		preparation := &arrayPositionCancelContext{Context: ctx, cancel: cancel, remaining: 20}
		_, err = ResolveTyped(preparation, id, []model.TypeLayout{layout, layout}, recordCompareResult(id), ResolveOptions{}, 1<<20)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during preparation = %v", err)
		}
		ctx, cancel = context.WithCancel(t.Context())
		interrupted := &arrayPositionCancelContext{Context: ctx, cancel: cancel, remaining: 20}
		_, err = function.Call(interrupted, []model.Value{value, value}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during NULL scan = %v", err)
		}
	}
}

func TestRecordComparisonEqualityNullResultIsFalse(t *testing.T) {
	for _, id := range []model.FunctionID{2981, 2982} {
		operation := opEq
		if id == 2982 {
			operation = opNe
		}
		codec := &recordComparison{
			operation: operation, counts: [2]int{1, 2},
			fields: []recordFieldComparison{{
				storage: [2]model.DataType{model.TypeInt64, model.TypeInt64},
				callable: replacementEquality(func(context.Context, []model.Value, CallBudget) (model.Value, error) {
					return model.NullVal(model.TypeBoolean), nil
				}),
			}},
		}
		signature, _ := LookupSignature(id)
		function := Callable{signature: signature, kernel: codec.compare, resultStorage: model.TypeBoolean, argumentStorage: [8]model.DataType{model.TypeComposite, model.TypeComposite}}
		requireRecordCompare(t, function, comparedRecord(model.Int64Val(1)), comparedRecord(model.Int64Val(1), model.Int64Val(0)), 1)
	}
}

func TestRecordComparisonChecksOrderingCallbackResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result model.Value
		state  string
		order  int64
	}{
		{"negative int4 normalized", model.Int64Val(math.MinInt32), "", -1},
		{"positive int4 normalized", model.Int64Val(math.MaxInt32), "", 1},
		{"NULL is invalid", model.NullVal(model.TypeInt64), "XX000", 0},
		{"boolean is invalid", model.BoolVal(true), "XX000", 0},
		{"outside int4", model.Int64Val(math.MaxInt32 + 1), "XX000", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callback := Callable{
				signature:       Signature{ArgumentCount: 2, Arguments: [8]model.TypeID{23, 23}, Result: 23},
				argumentStorage: [8]model.DataType{model.TypeInt64, model.TypeInt64}, resultStorage: model.TypeInt64,
				kernel: func(context.Context, []model.Value, CallBudget) (model.Value, error) { return tc.result, nil },
			}
			codec := &recordComparison{operation: opCmp, counts: [2]int{1, 1}, fields: []recordFieldComparison{{storage: [2]model.DataType{model.TypeInt64, model.TypeInt64}, callable: callback}}}
			signature, _ := LookupSignature(2987)
			function := Callable{signature: signature, kernel: codec.compare, resultStorage: model.TypeInt64, argumentStorage: [8]model.DataType{model.TypeComposite, model.TypeComposite}}
			value := comparedRecord(model.Int64Val(1))
			if tc.state == "" {
				requireRecordCompare(t, function, value, value, tc.order)
				return
			}
			_, err := function.Call(t.Context(), []model.Value{value, value}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireArrayPositionSQLState(t, err, tc.state)
		})
	}
}
