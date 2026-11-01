package partition

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type integerEncoder struct{}

func (integerEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maximum uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(values) > len(layout) || uint64(len(values))*9 > maximum {
		return nil, storage.ErrInvalidLayout
	}
	encoded := make([]byte, 0, len(values)*9)
	for i, value := range values {
		if value.Type != layout[i].Type.Storage {
			return nil, storage.ErrInvalidLayout
		}
		if value.IsNull {
			encoded = append(encoded, 0)
			encoded = append(encoded, make([]byte, 8)...)
			continue
		}
		encoded = append(encoded, 1)
		var data [8]byte
		binary.BigEndian.PutUint64(data[:], uint64(value.IntVal)^(uint64(1)<<63))
		encoded = append(encoded, data[:]...)
	}
	return encoded, nil
}

func (integerEncoder) ValidateOperatorClass(context.Context, model.TypeLayout, model.OID) error {
	return nil
}

func partitionDescriptor(t *testing.T, id model.ObjectID, oid model.OID, name string, parent model.ObjectID, bound *model.PartitionBound, key model.KeyElement) *model.Descriptor {
	t.Helper()
	integer := model.TypeLayout{Type: key.Type}
	metadata := &model.TableMetadata{
		ColumnIDHighWatermark: 1,
		Columns:               []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: integer}},
		PrimaryKey:            []model.KeyElement{{Column: 1, Type: integer.Type}},
	}
	if parent == 0 {
		metadata.PartitionStrategy = bound.Strategy
		metadata.PartitionKeys = []model.KeyElement{key}
	} else {
		metadata.Parents = []model.ObjectID{parent}
		metadata.PartitionBound = bound
		metadata.PartitionState = model.PartitionAttached
	}
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: id, OID: oid, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: name},
		Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: metadata,
	}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	return descriptor
}

func TestListRouterReturnsPhysicalChildIdentity(t *testing.T) {
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	operatorClass, ok := builtin.LookupDefaultOperatorClass("btree", integer.ID)
	if !ok {
		t.Fatal("missing integer operator class")
	}
	key := model.KeyElement{Column: 1, Type: integer, OperatorClass: operatorClass.ID}
	parentBound := &model.PartitionBound{Strategy: 'l', Definition: "FOR VALUES IN (0)", ListValues: []model.PartitionDatum{{Kind: model.PartitionDatumValue, Layout: model.TypeLayout{Type: integer}, Value: model.Int64Val(0)}}}
	parent := partitionDescriptor(t, 100, 10100, "events", 0, parentBound, key)
	zero := partitionDescriptor(t, 101, 10101, "events_zero", parent.ID(), parentBound, key)
	oneBound := &model.PartitionBound{Strategy: 'l', Definition: "FOR VALUES IN (1)", ListValues: []model.PartitionDatum{{Kind: model.PartitionDatumValue, Layout: model.TypeLayout{Type: integer}, Value: model.Int64Val(1)}}}
	one := partitionDescriptor(t, 102, 10102, "events_one", parent.ID(), oneBound, key)
	router, err := NewRouter(t.Context(), Options{Parent: parent, Children: []*model.Descriptor{zero, one}, Encoder: integerEncoder{}, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	target, err := router.Route(t.Context(), []model.Value{model.Int64Val(1)})
	if err != nil {
		t.Fatal(err)
	}
	if target.Ref.ObjectID != one.ID() || target.OID != one.OID() {
		t.Fatalf("routed to %+v", target)
	}
	if _, err := router.Route(t.Context(), []model.Value{model.Int64Val(2)}); !errors.Is(err, ErrNoPartition) {
		t.Fatalf("missing partition error = %v", err)
	}
}

func TestRangeRouterPrunesDisjointChildren(t *testing.T) {
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	operatorClass, ok := builtin.LookupDefaultOperatorClass("btree", integer.ID)
	if !ok {
		t.Fatal("missing integer operator class")
	}
	key := model.KeyElement{Column: 1, Type: integer, OperatorClass: operatorClass.ID}
	layout := model.TypeLayout{Type: integer}
	firstBound := &model.PartitionBound{Strategy: 'r', Definition: "FOR VALUES FROM (MINVALUE) TO (10)", Lower: []model.PartitionDatum{{Kind: model.PartitionDatumMin, Layout: layout}}, Upper: []model.PartitionDatum{{Kind: model.PartitionDatumValue, Layout: layout, Value: model.Int64Val(10)}}}
	parent := partitionDescriptor(t, 200, 10200, "measurements", 0, firstBound, key)
	first := partitionDescriptor(t, 201, 10201, "measurements_low", parent.ID(), firstBound, key)
	secondBound := &model.PartitionBound{Strategy: 'r', Definition: "FOR VALUES FROM (10) TO (MAXVALUE)", Lower: []model.PartitionDatum{{Kind: model.PartitionDatumValue, Layout: layout, Value: model.Int64Val(10)}}, Upper: []model.PartitionDatum{{Kind: model.PartitionDatumMax, Layout: layout}}}
	second := partitionDescriptor(t, 202, 10202, "measurements_high", parent.ID(), secondBound, key)
	router, err := NewRouter(t.Context(), Options{Parent: parent, Children: []*model.Descriptor{first, second}, Encoder: integerEncoder{}, Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Values: []model.Value{model.Int64Val(4)}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.Int64Val(4)}, Inclusive: true}}}
	targets, err := router.Prune(t.Context(), spans)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Ref.ObjectID != first.ID() {
		t.Fatalf("pruned targets = %+v", targets)
	}
}
