package builtin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var declaredTypeTestLimits = model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 64, MaxDepth: 16}

func declaredTypeTestDescriptor(t *testing.T, object model.ObjectID, oid model.OID, version uint64, name string, metadata *model.TypeMetadata) *model.Descriptor {
	t.Helper()
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: object, OID: oid, Version: version, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 1, Kind: model.ObjectType, Name: name}, Type: metadata,
	}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

type declaredTypeTestCompiler struct{}

func (declaredTypeTestCompiler) CompileDomainCheck(_ context.Context, _ TypeResolver, _ *model.Descriptor, constraint model.ConstraintDescriptor, _ model.TypeLayout) (TypeScalarCall, error) {
	return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		value := arguments[0].IntVal
		if constraint.Name == "positive" {
			return model.BoolVal(value > 0), nil
		}
		return model.BoolVal(value < 10), nil
	}, nil
}

func (declaredTypeTestCompiler) ResolveTypeFunction(_ context.Context, types TypeResolver, _ model.ObjectID, _ model.FunctionID, _ []model.TypeLayout, result model.TypeLayout) (TypeScalarCall, error) {
	if result.Type.Storage == model.TypeFloat64 {
		return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
			return model.Float64Val(float64(arguments[0].IntVal - arguments[1].IntVal)), nil
		}, nil
	}
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		resolved, _ := types.ResolveType(result.Type.ID)
		family, _ := resolved.RangeFamily()
		value, err := decodeRange(ctx, family, arguments[0])
		if err != nil {
			return model.Value{}, err
		}
		value.lower.integer++
		return encodeRange(ctx, family, value, budget)
	}, nil
}

func TestDeclaredDomainNestedInputRunsEveryConstraint(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	innerLayout := model.TypeLayout{Type: model.Type{ID: 8101, Modifier: -1, Storage: model.TypeInt64}}
	outerLayout := model.TypeLayout{Type: model.Type{ID: 8102, Modifier: -1, Storage: model.TypeInt64}}
	definition := &model.ExpressionDefinition{SQL: "VALUE", SemanticVersion: 1, Encoding: []byte{1}}
	inner := declaredTypeTestDescriptor(t, 11, 8101, 1, "positive_int", &model.TypeMetadata{
		Kind: model.TypeKindDomain, Layout: innerLayout, Array: 1011, BaseLayout: integer,
		Constraints: []model.ConstraintDescriptor{{ID: 101, Name: "positive", Kind: model.ConstraintCheck, Expression: definition, Enforced: true, Validated: true}},
	})
	outer := declaredTypeTestDescriptor(t, 12, 8102, 1, "small_positive_int", &model.TypeMetadata{
		Kind: model.TypeKindDomain, Layout: outerLayout, Array: 1012, Base: inner.ID(), BaseLayout: innerLayout,
		Constraints: []model.ConstraintDescriptor{{ID: 102, Name: "small", Kind: model.ConstraintCheck, Expression: definition, Enforced: true, Validated: true}},
	})
	resolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{{Descriptor: inner, Published: true}, {Descriptor: outer, Published: true}}, declaredTypeTestCompiler{}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.TypeLayout{
		{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}},
		{Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}},
		integer,
	}
	input, err := ResolveTyped(t.Context(), 2597, arguments, outerLayout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	call := func(text string) error {
		_, err := input.Call(t.Context(), []model.Value{model.StringVal(text), model.Int64Val(8102), model.Int64Val(-1)}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
		return err
	}
	if err := call("2"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"0", "10"} {
		var sqlErr *model.Error
		if err := call(text); !errors.As(err, &sqlErr) || sqlErr.Code != "23514" {
			t.Fatalf("domain input %q error = %v", text, err)
		}
	}
}

func TestDeclaredCompositeTextIORunsNestedDeclaredCodecs(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	domainLayout := model.TypeLayout{Type: model.Type{ID: 8151, Modifier: -1, Storage: model.TypeInt64}}
	enumLayout := model.TypeLayout{Type: model.Type{ID: 8152, Modifier: -1, Storage: model.TypeInt64}}
	domain := declaredTypeTestDescriptor(t, 15, 8151, 1, "positive_int", &model.TypeMetadata{
		Kind: model.TypeKindDomain, Layout: domainLayout, Array: 1015, BaseLayout: integer,
		Constraints: []model.ConstraintDescriptor{{
			ID: 105, Name: "positive", Kind: model.ConstraintCheck,
			Expression: &model.ExpressionDefinition{SQL: "VALUE", SemanticVersion: 1, Encoding: []byte{1}},
			Enforced: true, Validated: true,
		}},
	})
	enum := declaredTypeTestDescriptor(t, 16, 8152, 1, "color", &model.TypeMetadata{
		Kind: model.TypeKindEnum, Layout: enumLayout, Array: 1016,
		Elements: []string{"red", "blue"}, ElementOIDs: []model.OID{411, 412},
	})
	compositeLayout := model.TypeLayout{
		Type: model.Type{ID: 8153, Modifier: -1, Storage: model.TypeComposite},
		Children: []model.TypeLayout{enumLayout, domainLayout},
	}
	composite := declaredTypeTestDescriptor(t, 17, 8153, 1, "swatch", &model.TypeMetadata{
		Kind: model.TypeKindComposite, Layout: compositeLayout, Array: 1017, FieldIDHighWatermark: 2,
		Fields: []model.ColumnDescriptor{
			{ID: 1, Name: "color", State: model.StatePublic, Layout: enumLayout},
			{ID: 2, Name: "weight", State: model.StatePublic, Layout: domainLayout},
		},
	})
	resolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{
		{Descriptor: domain, Published: true},
		{Descriptor: enum, Published: true},
		{Descriptor: composite, Published: true},
	}, declaredTypeTestCompiler{}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	input, err := ResolveTyped(t.Context(), 2290, []model.TypeLayout{
		{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}},
		{Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}},
		integer,
	}, compositeLayout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	value, err := input.Call(t.Context(), []model.Value{model.StringVal("(red,7)"), model.Int64Val(8153), model.Int64Val(-1)}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	output, err := ResolveTyped(t.Context(), 2291, []model.TypeLayout{compositeLayout}, model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	text, err := output.Call(t.Context(), []model.Value{value}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil || text.StrVal != "(red,7)" {
		t.Fatalf("composite text = %v, %v", text, err)
	}
}

func TestDeclaredEnumUsesLabelOrderAndTransactionSafety(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 8201, Modifier: -1, Storage: model.TypeInt64}}
	descriptor := declaredTypeTestDescriptor(t, 21, 8201, 3, "priority", &model.TypeMetadata{
		Kind: model.TypeKindEnum, Layout: layout, Array: 1021,
		Elements: []string{"low", "middle", "high"}, ElementOIDs: []model.OID{301, 299, 302}, UnsafeElementOIDs: []model.OID{302},
	})
	resolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{{Descriptor: descriptor, Published: false}}, nil, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	less, err := ResolveTyped(t.Context(), 3510, []model.TypeLayout{layout, layout}, model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}, ResolveOptions{Types: resolver}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := less.Call(t.Context(), []model.Value{model.Int64Val(301), model.Int64Val(299)}, CallBudget{})
	if err != nil || ordered.IntVal != 1 {
		t.Fatalf("enum order = %v, %v", ordered, err)
	}
	encoder, err := NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	encoder = encoder.WithTypes(resolver)
	keyLayout := []storage.KeyColumn{{ColumnID: 1, Type: layout.Type}}
	lowKey, err := encoder.EncodePrefix(t.Context(), keyLayout, []model.Value{model.Int64Val(301)}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	middleKey, err := encoder.EncodePrefix(t.Context(), keyLayout, []model.Value{model.Int64Val(299)}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(lowKey, middleKey) >= 0 {
		t.Fatal("enum key order used element OIDs instead of label order")
	}
	decodedKey, err := encoder.DecodeKey(t.Context(), keyLayout, middleKey, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(decodedKey) != 1 || decodedKey[0].IntVal != 299 {
		t.Fatalf("decoded enum key = %+v", decodedKey)
	}
	var sqlErr *model.Error
	if _, err := encoder.EncodePrefix(t.Context(), keyLayout, []model.Value{model.Int64Val(302)}, 1024); !errors.As(err, &sqlErr) || sqlErr.Code != "55P04" {
		t.Fatalf("unsafe enum key error = %v", err)
	}
	input, err := ResolveTyped(t.Context(), 3506, []model.TypeLayout{{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}, {Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}}}, layout, ResolveOptions{Types: resolver}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = input.Call(t.Context(), []model.Value{model.StringVal("high"), model.Int64Val(8201)}, CallBudget{})
	if !errors.As(err, &sqlErr) || sqlErr.Code != "55P04" {
		t.Fatalf("unsafe enum error = %v", err)
	}
	committed, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{{Descriptor: descriptor, Published: true}}, nil, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	input, err = ResolveTyped(t.Context(), 3506, []model.TypeLayout{{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}, {Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}}}, layout, ResolveOptions{Types: committed}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = input.Call(t.Context(), []model.Value{model.StringVal("high"), model.Int64Val(8201)}, CallBudget{}); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredRangeCanonicalAndSnapshotIdentity(t *testing.T) {
	subtype := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	rangeLayout := model.TypeLayout{Type: model.Type{ID: 8301, Modifier: -1, Storage: model.TypeBytes}}
	multiLayout := model.TypeLayout{Type: model.Type{ID: 8302, Modifier: -1, Storage: model.TypeBytes}}
	rangeArrayLayout := model.TypeLayout{Type: model.Type{ID: 8303, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{rangeLayout}}
	metadata := &model.RangeTypeMetadata{Subtype: subtype, OperatorClass: 1978, Canonical: 41, CanonicalFunction: 8401, Difference: 42, DifferenceFunction: 8402, Range: 31, Multirange: 32}
	rangeType := declaredTypeTestDescriptor(t, 31, 8301, 5, "stepped", &model.TypeMetadata{Kind: model.TypeKindRange, Layout: rangeLayout, Array: 33, Range: metadata})
	multirangeType := declaredTypeTestDescriptor(t, 32, 8302, 5, "stepped_multi", &model.TypeMetadata{Kind: model.TypeKindMultirange, Layout: multiLayout, Array: 1032, Range: metadata})
	rangeArray := declaredTypeTestDescriptor(t, 33, 8303, 5, "_stepped", &model.TypeMetadata{Kind: model.TypeKindArray, Layout: rangeArrayLayout, Base: 31, BaseLayout: rangeLayout})
	resolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{{Descriptor: rangeType, Published: true}, {Descriptor: multirangeType, Published: true}, {Descriptor: rangeArray, Published: true}}, declaredTypeTestCompiler{}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	inputArguments := []model.TypeLayout{
		{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}},
		{Type: model.Type{ID: 26, Modifier: -1, Storage: model.TypeInt64}},
		{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}},
	}
	input, err := ResolveTyped(t.Context(), 3834, inputArguments, rangeLayout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := input.Call(t.Context(), []model.Value{model.StringVal("[1,5]"), model.Int64Val(8301), model.Int64Val(-1)}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := resolver.ResolveType(8301)
	family, _ := resolved.RangeFamily()
	decoded, err := decodeRange(t.Context(), family, encoded)
	if err != nil || decoded.lower.integer != 2 {
		t.Fatalf("canonical range = %+v, %v", decoded, err)
	}
	constructor, err := ResolveTyped(t.Context(), 3840, []model.TypeLayout{subtype, subtype}, rangeLayout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	constructed, err := constructor.Call(t.Context(), []model.Value{model.Int64Val(3), model.Int64Val(8)}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err = decodeRange(t.Context(), family, constructed)
	if err != nil || decoded.lower.integer != 4 {
		t.Fatalf("constructed range = %+v, %v", decoded, err)
	}
	encoder, err := NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	encoder = encoder.WithTypes(resolver)
	keyLayout := []storage.KeyColumn{{ColumnID: 1, Type: rangeLayout.Type}}
	rangeKey, err := encoder.EncodePrefix(t.Context(), keyLayout, []model.Value{constructed}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	decodedKey, err := encoder.DecodeKey(t.Context(), keyLayout, rangeKey, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(decodedKey) != 1 || !bytes.Equal(decodedKey[0].BytesVal, constructed.BytesVal) {
		t.Fatalf("decoded range key = %+v", decodedKey)
	}
	multirange, err := ResolveTyped(t.Context(), 4282, []model.TypeLayout{rangeArrayLayout}, multiLayout, ResolveOptions{Types: resolver}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	multirangeValue, err := multirange.Call(t.Context(), []model.Value{{
		Type: model.TypeArray, ArrayVal: []model.Value{constructed},
		Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}},
	}}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := newMultirangeCursor(family, multirangeValue)
	if err != nil {
		t.Fatal(err)
	}
	member, more, err := cursor.next(t.Context())
	if err != nil || !more || member.lower.integer != 4 {
		t.Fatalf("constructed multirange member = %+v, %v, %v", member, more, err)
	}
	difference, err := resolved.SubtypeDifference(t.Context(), model.Int64Val(9), model.Int64Val(4), CallBudget{})
	if err != nil || difference != 5 {
		t.Fatalf("subtype difference = %v, %v", difference, err)
	}
	newer := declaredTypeTestDescriptor(t, 31, 8301, 6, "stepped", &model.TypeMetadata{Kind: model.TypeKindRange, Layout: rangeLayout, Array: 33, Range: metadata})
	newerResolver, err := NewDeclaredTypeResolver(t.Context(), []TypeDefinition{{Descriptor: newer, Published: true}, {Descriptor: multirangeType, Published: true}, {Descriptor: rangeArray, Published: true}}, declaredTypeTestCompiler{}, declaredTypeTestLimits)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.SemanticFingerprint() == newerResolver.SemanticFingerprint() {
		t.Fatal("type snapshot fingerprint ignored descriptor version")
	}
}

// TestDeclaredTypeResolverAdmitsCatalogSizedPopulation pins the resolver's
// aggregate allowance to the declared-type population. A catalog that
// accumulates types across sessions retains one resolved entry plus a bounded
// descriptor copy per type, so the whole catalog legitimately needs more than a
// single descriptor's MaxBytes. Before the allowance scaled with the
// population, the 1 MiB cap admitted only a few hundred small enums and every
// later statement failed with ErrDescriptorLimit ("descriptor exceeds metadata
// limits") or 54000 "declared type metadata budget exceeded" — the M1 and M2
// postgres-corpus clusters.
func TestDeclaredTypeResolverAdmitsCatalogSizedPopulation(t *testing.T) {
	const population = 1000
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	definitions := make([]TypeDefinition, 0, population)
	for i := range population {
		oid := model.OID(90000 + i)
		elements := make([]string, 8)
		elementOIDs := make([]model.OID, 8)
		for j := range elements {
			elements[j] = fmt.Sprintf("label_%d", j)
			elementOIDs[j] = model.OID(900000 + i*8 + j)
		}
		definitions = append(definitions, TypeDefinition{
			Descriptor: declaredTypeTestDescriptor(t, model.ObjectID(90000+i), oid, 1, fmt.Sprintf("enum_%d", i), &model.TypeMetadata{
				Kind:   model.TypeKindEnum,
				Layout: model.TypeLayout{Type: model.Type{ID: model.TypeID(oid), Modifier: -1, Storage: model.TypeInt64}},
				Array:  model.ObjectID(100000 + i), Elements: elements, ElementOIDs: elementOIDs,
			}),
			Published: true,
		})
	}
	resolver, err := NewDeclaredTypeResolver(t.Context(), definitions, declaredTypeTestCompiler{}, limits)
	if err != nil {
		t.Fatalf("catalog-sized declared type population rejected: %v", err)
	}
	if _, ok := resolver.ResolveType(model.TypeID(90000 + population - 1)); !ok {
		t.Fatal("last declared type is not resolvable")
	}
}

// TestDeclaredTypeResolverAdmitsPopulationOverhead pins the second trip point of
// the same accounting: the per-type map entries and resolver structs alone must
// fit the aggregate allowance. With the allowance fixed at one descriptor's
// MaxBytes, a modest population overruns it and reports 54000 "declared type
// metadata budget exceeded" (postgres corpus cluster M2: CREATE TEMPORARY TABLE
// and REINDEX after the catalog accumulated types).
func TestDeclaredTypeResolverAdmitsPopulationOverhead(t *testing.T) {
	const population = 200
	limits := model.DescriptorLimits{MaxBytes: 4096, MaxItems: 1024, MaxDepth: 64}
	definitions := make([]TypeDefinition, 0, population)
	for i := range population {
		oid := model.OID(70000 + i)
		definitions = append(definitions, TypeDefinition{
			Descriptor: declaredTypeTestDescriptor(t, model.ObjectID(70000+i), oid, 1, fmt.Sprintf("enum_%d", i), &model.TypeMetadata{
				Kind:   model.TypeKindEnum,
				Layout: model.TypeLayout{Type: model.Type{ID: model.TypeID(oid), Modifier: -1, Storage: model.TypeInt64}},
				Array:  model.ObjectID(80000 + i), Elements: []string{"only"}, ElementOIDs: []model.OID{model.OID(800000 + i)},
			}),
			Published: true,
		})
	}
	if _, err := NewDeclaredTypeResolver(t.Context(), definitions, declaredTypeTestCompiler{}, limits); err != nil {
		t.Fatalf("population overhead rejected: %v", err)
	}
}
