package builtin

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestTypeBinaryCodecRoundTripsNestedDeclaredValues(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	domainLayout := model.TypeLayout{Type: model.Type{ID: 8151, Modifier: -1, Storage: model.TypeInt64}}
	enumLayout := model.TypeLayout{Type: model.Type{ID: 8152, Modifier: -1, Storage: model.TypeInt64}}
	domainArrayLayout := model.TypeLayout{Type: model.Type{ID: 8251, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{domainLayout}}
	enumArrayLayout := model.TypeLayout{Type: model.Type{ID: 8252, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{enumLayout}}
	domain := declaredTypeTestDescriptor(t, 15, 8151, 1, "positive_int", &model.TypeMetadata{
		Kind: model.TypeKindDomain, Layout: domainLayout, Array: 8251, BaseLayout: integer,
		Constraints: []model.ConstraintDescriptor{{
			ID: 105, Name: "positive", Kind: model.ConstraintCheck,
			Expression: &model.ExpressionDefinition{SQL: "VALUE", SemanticVersion: 1, Encoding: []byte{1}},
			Enforced: true, Validated: true,
		}},
	})
	enum := declaredTypeTestDescriptor(t, 16, 8152, 1, "color", &model.TypeMetadata{
		Kind: model.TypeKindEnum, Layout: enumLayout, Array: 8252,
		Elements: []string{"red", "blue"}, ElementOIDs: []model.OID{411, 412},
	})
	compositeLayout := model.TypeLayout{
		Type: model.Type{ID: 8153, Modifier: -1, Storage: model.TypeComposite},
		Children: []model.TypeLayout{enumLayout, domainLayout},
	}
	composite := declaredTypeTestDescriptor(t, 17, 8153, 1, "swatch", &model.TypeMetadata{
		Kind: model.TypeKindComposite, Layout: compositeLayout, Array: 8253, FieldIDHighWatermark: 2,
		Fields: []model.ColumnDescriptor{
			{ID: 1, Name: "color", State: model.StatePublic, Layout: enumLayout},
			{ID: 2, Name: "weight", State: model.StatePublic, Layout: domainLayout},
		},
	})
	compositeArrayLayout := model.TypeLayout{Type: model.Type{ID: 8253, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{compositeLayout}}
	domainArray := declaredTypeTestDescriptor(t, 25, 8251, 1, "_positive_int", &model.TypeMetadata{Kind: model.TypeKindArray, Layout: domainArrayLayout, Base: 15, BaseLayout: domainLayout})
	enumArray := declaredTypeTestDescriptor(t, 26, 8252, 1, "_color", &model.TypeMetadata{Kind: model.TypeKindArray, Layout: enumArrayLayout, Base: 16, BaseLayout: enumLayout})
	compositeArray := declaredTypeTestDescriptor(t, 27, 8253, 1, "_swatch", &model.TypeMetadata{Kind: model.TypeKindArray, Layout: compositeArrayLayout, Base: 17, BaseLayout: compositeLayout})
	resolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{
		{Descriptor: domain, Published: true},
		{Descriptor: enum, Published: true},
		{Descriptor: composite, Published: true},
		{Descriptor: domainArray, Published: true},
		{Descriptor: enumArray, Published: true},
		{Descriptor: compositeArray, Published: true},
	}, declaredTypeTestCompiler{}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	layout := compositeArrayLayout
	value := model.Value{
		Type: model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -1}},
		ArrayVal: []model.Value{
			{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(411), model.Int64Val(7)}},
			model.NullVal(model.TypeComposite),
		},
	}
	codec, err := ResolveTypeBinaryCodec(t.Context(), layout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	budget := CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20}
	encoded, err := codec.Output(t.Context(), []model.Value{value}, budget)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.Input(t.Context(), []model.Value{encoded}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, value) {
		t.Fatalf("nested binary round trip = %#v, want %#v", decoded, value)
	}
}

func TestTypeBinaryCodecRoundTripsRangeFraming(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	rangeLayout := model.TypeLayout{Type: model.Type{ID: 3904, Modifier: -1, Storage: model.TypeBytes}}
	constructor, err := ResolveTyped(t.Context(), 3840, []model.TypeLayout{integer, integer}, rangeLayout, ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	budget := CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20}
	value, err := constructor.Call(t.Context(), []model.Value{model.Int64Val(3), model.Int64Val(8)}, budget)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := ResolveTypeBinaryCodec(t.Context(), rangeLayout, ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := codec.Output(t.Context(), []model.Value{value}, budget)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.Input(t.Context(), []model.Value{encoded}, budget)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Type != model.TypeBytes || !bytes.Equal(decoded.BytesVal, value.BytesVal) {
		t.Fatalf("range binary round trip = %#v, want %#v", decoded, value)
	}
}
