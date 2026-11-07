package model_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestArrayShapeRequiresRepresentableLastSubscript(t *testing.T) {
	for _, tc := range []struct {
		name          string
		lower, length int32
		invalid       bool
	}{
		{"maximum lower bound", math.MaxInt32, 1, false},
		{"multiple elements ending at maximum", math.MaxInt32 - 1, 2, false},
		{"last subscript overflows", math.MaxInt32, 2, true},
		{"lowest lower bound", math.MinInt32, math.MaxInt32, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := model.ValidateArrayShape([]model.ArrayDimension{{LowerBound: tc.lower, Length: tc.length}}, uint64(tc.length))
			if tc.invalid {
				if !errors.Is(err, model.ErrInvalidArrayShape) {
					t.Fatalf("invalid bound admitted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArrayShapeInvariants(t *testing.T) {
	var dimensions [7]model.ArrayDimension
	for i := range dimensions {
		dimensions[i] = model.ArrayDimension{LowerBound: 1, Length: 1}
	}
	for _, tc := range []struct {
		name       string
		dimensions []model.ArrayDimension
		elements   uint64
		invalid    bool
	}{
		{"canonical empty", nil, 0, false},
		{"empty catalog vector", []model.ArrayDimension{{LowerBound: 0, Length: 0}}, 0, false},
		{"empty dimension upper bound underflows", []model.ArrayDimension{{LowerBound: math.MinInt32, Length: 0}}, 0, true},
		{"nonempty without dimensions", nil, 1, true},
		{"empty with dimensions", dimensions[:1], 0, true},
		{"zero length", []model.ArrayDimension{{Length: 0}}, 1, true},
		{"negative length", []model.ArrayDimension{{Length: -1}}, 1, true},
		{"too few elements", []model.ArrayDimension{{Length: 2}}, 1, true},
		{"too many elements", []model.ArrayDimension{{Length: 2}}, 3, true},
		{"six dimensions", dimensions[:6], 1, false},
		{"seven dimensions", dimensions[:], 1, true},
		{"multidimensional product", []model.ArrayDimension{{Length: 2}, {Length: 3}}, 6, false},
		{"product overflow", []model.ArrayDimension{{Length: math.MaxInt32}, {Length: math.MaxInt32}, {Length: math.MaxInt32}}, math.MaxUint64, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := model.ValidateArrayShape(tc.dimensions, tc.elements)
			if tc.invalid {
				if !errors.Is(err, model.ErrInvalidArrayShape) {
					t.Fatalf("dimensions %v with %d elements: got %v, want invalid shape", tc.dimensions, tc.elements, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
