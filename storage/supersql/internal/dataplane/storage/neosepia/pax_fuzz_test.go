package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func FuzzPAXColumnDecoding(f *testing.F) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString}}
	array := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	composite := model.TypeLayout{Type: model.Type{ID: 40000, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}, array, {Type: model.Type{ID: 40001, Storage: model.TypeComposite}}}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{composite}, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 65536})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		if err := builder.Close(); err != nil {
			f.Error(err)
		}
	})
	row := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(11), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 3}}, ArrayVal: []model.Value{model.StringVal("x"), model.StringVal("")}}, {Type: model.TypeComposite}}}
	if err := builder.Append(context.Background(), []model.Value{row}); err != nil {
		f.Fatal(err)
	}
	if err := builder.Append(context.Background(), []model.Value{model.NullVal(model.TypeComposite)}); err != nil {
		f.Fatal(err)
	}
	var batch columnar.Batch
	if err := builder.Publish(&batch); err != nil {
		f.Fatal(err)
	}
	versions := blockVersions{Keys: columnar.Vector{Type: model.Type{ID: 17, Storage: model.TypeBytes}, Length: 2, Data: []byte("ab"), Ranges: []columnar.Range{{Length: 1}, {Offset: 1, Length: 1}}}, CommitTS: []model.Timestamp{1, 2}, Ordinals: []uint64{1, 1}, Kinds: []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert}}
	limits := blockLimits{MaxRows: 1024, MaxBytes: 65536, MaxDecodedBytes: 65536}
	seed, err := encodeBlock(context.Background(), storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, versions, &batch, []model.ColumnID{70000}, limits)
	if err != nil {
		f.Fatal(err)
	}
	original, err := openBlock(seed, limits)
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() {
		if err := original.Close(); err != nil {
			f.Error(err)
		}
	})
	vector, err := original.Column(context.Background(), 70000)
	if err != nil {
		f.Fatal(err)
	}
	if err := vector.Validate(); err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) < paxHeaderSize || len(input) > 65536 {
			return
		}
		data := bytes.Clone(input)
		binary.LittleEndian.PutUint32(data[56:], paxChecksum(data))
		block, err := openBlock(data, limits)
		if err != nil {
			return
		}
		t.Cleanup(func() {
			if err := block.Close(); err != nil {
				t.Error(err)
			}
		})
		for i := range block.columns {
			id := model.ColumnID(binary.LittleEndian.Uint32(data[paxHeaderSize+i*paxColumnSize:]))
			v, err := block.Column(t.Context(), id)
			if err != nil {
				if !errors.Is(err, errPAXBlock) && !errors.Is(err, columnar.ErrBatchBudget) {
					t.Fatalf("unexpected decode error: %v", err)
				}
				return
			}
			if err := v.Validate(); err != nil {
				t.Fatalf("decoder exposed invalid vector: %v", err)
			}
		}
	})
}
