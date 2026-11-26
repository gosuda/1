package builtin_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestOIDVectorKeyIdentityIgnoresLowerBounds(t *testing.T) {
	encoder := keyEncoder(t)
	layout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 30, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{{Type: model.Type{ID: 26, Storage: model.TypeInt64, Modifier: -1}}}}}
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}, Dimensions: []model.ArrayDimension{{LowerBound: 0, Length: 2}}}
	first, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{value}, 23)
	if err != nil {
		t.Fatal(err)
	}
	value.Dimensions[0].LowerBound = 7
	second, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{value}, 23)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("lower-bound-dependent key: %x/%x, %v", first, second, err)
	}
	decoded, err := encoder.DecodeKey(t.Context(), layout, first, 4096)
	if err != nil {
		t.Fatal(err)
	}
	value.Dimensions[0].LowerBound = 0
	if !reflect.DeepEqual(decoded, []model.Value{value}) {
		t.Fatalf("canonical vector key decoded %v", decoded)
	}
}
