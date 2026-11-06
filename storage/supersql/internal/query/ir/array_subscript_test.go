package ir

import (
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestArrayFetchRejectsInvalidContracts(t *testing.T) {
	for _, kind := range []ScalarKind{ArrayIndex, ArraySlice} {
		for _, defect := range []string{"no source", "scalar source", "nonnull result", "wrong result identity", "wrong result modifier", "wrong result collation", "wrong index type", "missing index", "extra index", "foreign payload"} {
			t.Run(fmt.Sprintf("kind%d/%s", kind, defect), func(t *testing.T) {
				f := newFixture(t)
				element := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1, Collation: 100}}
				array := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray, Modifier: -1, Collation: 100}, Children: []model.TypeLayout{element}}
				addLayout := func(layout model.TypeLayout) LayoutID {
					return arrayFetchTestLayout(t, f.b, layout)
				}
				arrayID := addLayout(array)
				int4 := addLayout(model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}})
				source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: arrayID, Parameter: 1, Nullable: true}})
				index := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: int4}, Value: model.Int64Val(1)})
				result := element
				subscripts := ArraySubscripts{}
				if kind == ArraySlice {
					result = array
					subscripts = ArraySubscripts{Dimensions: 1, Upper: 1}
				}
				switch defect {
				case "wrong result identity":
					result.Type.ID++
				case "wrong result modifier":
					result.Type.Modifier = 7
				case "wrong result collation":
					result.Type.Collation++
				}
				spec := ScalarSpec{Scalar: Scalar{Kind: kind, Scope: f.scope, Layout: addLayout(result), Nullable: true, Subscripts: subscripts}, Operands: []ScalarID{source, index}}
				switch defect {
				case "no source":
					spec.Operands = nil
				case "scalar source":
					spec.Operands[0] = index
				case "nonnull result":
					spec.Nullable = false
				case "wrong index type":
					spec.Operands[1] = f.constant(1)
				case "missing index":
					spec.Operands = spec.Operands[:1]
				case "extra index":
					for range 6 {
						spec.Operands = append(spec.Operands, index)
					}
				case "foreign payload":
					spec.FieldIndex = 1
				}
				_, err := f.b.AddScalar(t.Context(), spec)
				requireInvalid(t, err)
			})
		}
	}
}

func TestArrayFetchRejectsMissingOperandEffects(t *testing.T) {
	for _, kind := range []ScalarKind{ArrayIndex, ArraySlice} {
		f := newFixture(t)
		integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
		int4 := arrayFetchTestLayout(t, f.b, integer)
		array := arrayFetchTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}})
		source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: array, Parameter: 1, Nullable: true}})
		effects := Effects{Volatility: Volatile}
		signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 900000, Result: int4, Nullable: true, Effects: effects}})
		if err != nil {
			t.Fatal(err)
		}
		index := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: int4, Function: 900000, Signature: signature, Nullable: true, Effects: effects}})
		spec := ScalarSpec{Scalar: Scalar{Kind: kind, Scope: f.scope, Layout: int4, Nullable: true}, Operands: []ScalarID{source, index}}
		if kind == ArraySlice {
			spec.Layout, spec.Subscripts = array, ArraySubscripts{Dimensions: 1, Upper: 1}
		}
		_, err = f.b.AddScalar(t.Context(), spec)
		requireInvalid(t, err)
	}
}

func TestArraySliceRejectsInvalidMasks(t *testing.T) {
	for _, subscripts := range []ArraySubscripts{
		{}, {Dimensions: 7}, {Dimensions: 1, Upper: 2}, {Dimensions: 1, Lower: 2},
		{Dimensions: 6, Upper: 64}, {Dimensions: 6, Lower: 128},
		{Dimensions: 1, Upper: 1}, {Dimensions: 1, Lower: 1},
	} {
		f := newFixture(t)
		array, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
		if err != nil {
			t.Fatal(err)
		}
		source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: array, Parameter: 1}})
		_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArraySlice, Scope: f.scope, Layout: array, Nullable: true, Subscripts: subscripts}, Operands: []ScalarID{source}})
		requireInvalid(t, err)
	}
}

func TestArraySubscriptsBelongOnlyToSlices(t *testing.T) {
	f := newFixture(t)
	_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Subscripts: ArraySubscripts{Dimensions: 1}}, Value: model.Int64Val(1)})
	requireInvalid(t, err)
	f = newFixture(t)
	int4, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	array, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
	if err != nil {
		t.Fatal(err)
	}
	source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: array, Parameter: 1}})
	index := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: int4}, Value: model.Int64Val(1)})
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayIndex, Scope: f.scope, Layout: f.integer, Nullable: true, Subscripts: ArraySubscripts{Dimensions: 1}}, Operands: []ScalarID{source, index}})
	requireInvalid(t, err)
}

func TestArrayFetchPreservesNestedElementLayout(t *testing.T) {
	for _, invalidNested := range []bool{false, true} {
		f := newFixture(t)
		element := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 11, Collation: 100}}}}
		array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray}, Children: []model.TypeLayout{element}}
		arrayID, err := f.b.AddLayout(t.Context(), array)
		if err != nil {
			t.Fatal(err)
		}
		elementID, err := f.b.AddLayout(t.Context(), element)
		if err != nil {
			t.Fatal(err)
		}
		int4, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}})
		if err != nil {
			t.Fatal(err)
		}
		source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: arrayID, Parameter: 1, Nullable: true}})
		index := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: int4}, Value: model.Int64Val(1)})
		for rank := uint8(1); rank <= 6; rank++ {
			args := []ScalarID{source}
			for range rank {
				args = append(args, index)
			}
			f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayIndex, Scope: f.scope, Layout: elementID, Nullable: true}, Operands: args})
			f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArraySlice, Scope: f.scope, Layout: arrayID, Nullable: true, Subscripts: ArraySubscripts{Dimensions: rank}}, Operands: []ScalarID{source}})
			args = append(args, args[1:]...)
			mask := uint8(1<<rank) - 1
			f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArraySlice, Scope: f.scope, Layout: arrayID, Nullable: true, Subscripts: ArraySubscripts{Dimensions: rank, Upper: mask, Lower: mask}}, Operands: args})
		}
		if !invalidNested {
			f.freeze(f.scan(1, f.slot(arrayID, true, 1, 1)))
			continue
		}
		element.Children[0].Type.Modifier++
		wrongElement, err := f.b.AddLayout(t.Context(), element)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayIndex, Scope: f.scope, Layout: wrongElement, Nullable: true}, Operands: []ScalarID{source, index}})
		requireInvalid(t, err)
	}
}

func arrayFetchTestLayout(t *testing.T, builder *Builder, layout model.TypeLayout) LayoutID {
	t.Helper()
	id, err := builder.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
