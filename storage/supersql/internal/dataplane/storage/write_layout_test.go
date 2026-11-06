package storage_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func emptyArrayBatch() storage.MutationBatch {
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	m := storage.NewUpsertMutation(target, []byte("k"), []model.ColumnID{1}, []model.Value{{Type: model.TypeArray}})
	m.Ordinal = 1
	return storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 1, EncodingVersion: storage.MutationEncodingVersion, Mutations: []storage.Mutation{m}, Layouts: []storage.ObjectLayout{{Target: target, Columns: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}}}}}}}
}

func sealLayoutBatch(t *testing.T, b *storage.MutationBatch) {
	t.Helper()
	var err error
	b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizedBatchPreservesEmptyArrayTypeIdentity(t *testing.T) {
	b := emptyArrayBatch()
	sealLayoutBatch(t, &b)
	first, err := b.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b.Layouts[0].Columns[0].Type.ID = 1016
	b.Layouts[0].Columns[0].Children[0].Type.ID = 20
	sealLayoutBatch(t, &b)
	second, err := b.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("empty arrays with different declared element types have identical commit identities")
	}
	b.Layouts = nil
	sealLayoutBatch(t, &b)
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrInvalidLayout) {
		t.Fatalf("missing declaration: %v", err)
	}
}

func TestNormalizedBatchRejectsNestedRepresentationMismatch(t *testing.T) {
	b := emptyArrayBatch()
	b.Mutations[0].Values[0].Dimensions = []model.ArrayDimension{{Length: 1, LowerBound: 1}}
	b.Mutations[0].Values[0].ArrayVal = []model.Value{model.StringVal("not an integer")}
	sealLayoutBatch(t, &b)
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrUnnormalizedBatch) {
		t.Fatalf("mistyped array element: %v", err)
	}
}

func TestNormalizedBatchRejectsPartialRowImage(t *testing.T) {
	b := emptyArrayBatch()
	b.Mutations[0].ColumnIDs = nil
	b.Mutations[0].Values = nil
	sealLayoutBatch(t, &b)
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrUnnormalizedBatch) {
		t.Fatalf("partial upsert image: %v", err)
	}
}

func TestDeclaredMissingValueRejectsWrongArrayElementType(t *testing.T) {
	b := emptyArrayBatch()
	column := &b.Layouts[0].Columns[0]
	column.HasMissingValue = true
	column.MissingValue = model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.StringVal("wrong element type")}}
	if err := b.Layouts[0].Validate(t.Context()); !errors.Is(err, storage.ErrInvalidLayout) {
		t.Fatalf("invalid historical column default: %v", err)
	}
}
