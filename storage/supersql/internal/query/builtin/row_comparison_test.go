package builtin_test

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestRowComparisonInterpretationsUseSelectedFamilySupport(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator model.OperatorID
		want     []builtin.RowComparisonInfo
	}{
		{"int4", 97, []builtin.RowComparisonInfo{{Family: 1976, Strategy: 1, Left: 23, Right: 23, CompareFunction: 351}}},
		{"mixed_integer", 37, []builtin.RowComparisonInfo{{Family: 1976, Strategy: 1, Left: 23, Right: 20, CompareFunction: 2188}}},
		{"mixed_integer_negator", 36, []builtin.RowComparisonInfo{{Family: 1976, Strategy: 6, Left: 23, Right: 20, CompareFunction: 2188}}},
		{"name_text", 255, []builtin.RowComparisonInfo{{Family: 1994, Strategy: 1, Left: 19, Right: 25, CompareFunction: 246}}},
		{"text_name", 261, []builtin.RowComparisonInfo{{Family: 1994, Strategy: 1, Left: 25, Right: 19, CompareFunction: 253}}},
		{"equality_multiple_families", 98, []builtin.RowComparisonInfo{
			{Family: 1994, Strategy: 3, Left: 25, Right: 25, CompareFunction: 360},
			{Family: 2095, Strategy: 3, Left: 25, Right: 25, CompareFunction: 2166},
		}},
		{"negator_multiple_families", 531, []builtin.RowComparisonInfo{
			{Family: 1994, Strategy: 6, Left: 25, Right: 25, CompareFunction: 360},
			{Family: 2095, Strategy: 6, Left: 25, Right: 25, CompareFunction: 2166},
		}},
		{"absent", 0, nil},
		{"unknown", 0xffffffff, nil},
		{"integer_addition", 551, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Collect(builtin.RowComparisonInterpretations(tc.operator))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("operator %d interpretations = %+v; want %+v", tc.operator, got, tc.want)
			}
		})
	}
}

func TestRowComparisonInterpretationIterationStopsWithoutAllocation(t *testing.T) {
	var got builtin.RowComparisonInfo
	allocations := testing.AllocsPerRun(100, func() {
		for info := range builtin.RowComparisonInterpretations(98) {
			got = info
			break
		}
	})
	if got.Family != 1994 || got.CompareFunction != 360 {
		t.Fatalf("first interpretation = %+v", got)
	}
	if allocations != 0 {
		t.Fatalf("row interpretation iteration allocated %g times", allocations)
	}
}
