package runtime

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestAllocatorZeroClaimsConsumeAdmissionSlots(t *testing.T) {
	q := testQuery(t, nil, QueryConfig{SpillMode: SpillDisabled, MaxReservations: 1})
	allocator := q.Allocator(WorkMemory)
	claim, err := allocator.Reserve(0)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	if _, err := allocator.Reserve(0); !errors.Is(err, ErrBudget) {
		t.Fatalf("second zero claim = %v", err)
	}
	if err := claim.Resize(math.MaxUint64); !errors.Is(err, ErrBudget) {
		t.Fatalf("overflow resize = %v", err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 1 {
		t.Fatalf("rejected resize changed admission: %+v", usage)
	}
	claim.Release()
	claim.Release()
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("released admission: %+v", usage)
	}
}

func TestBuildersShareQueryCap(t *testing.T) {
	q := testQuery(t, nil, QueryConfig{SpillMode: SpillDisabled, WorkBytes: 4096})
	options := columnar.BuilderOptions{MaxRows: 2, MaxBytes: 4096, Allocator: q.Allocator(WorkMemory)}
	layouts := []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	first, err := columnar.NewBuilder(layouts, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := columnar.NewBuilder(layouts, options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	row := []model.Value{{Type: model.TypeBytes, BytesVal: make([]byte, 1800)}}
	if err := first.Append(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if err := second.Append(t.Context(), row); !errors.Is(err, ErrBudget) {
		t.Fatalf("independent builder bypassed query cap: %v", err)
	}
	if second.Rows() != 0 || first.Rows() != 1 {
		t.Fatalf("failed append changed rows: %d, %d", first.Rows(), second.Rows())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Append(t.Context(), row); err != nil {
		t.Fatalf("released capacity unavailable: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 || usage.Peak > usage.Limit {
		t.Fatalf("builder ownership leaked: %+v", usage)
	}
}

func TestBuilderGrowthAdmitsOldAndNewBuffers(t *testing.T) {
	q := testQuery(t, nil, QueryConfig{SpillMode: SpillDisabled, WorkBytes: 4096})
	builder, err := columnar.NewBuilder([]model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes}}}, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 4096, Allocator: q.Allocator(WorkMemory)})
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close()
	if err := builder.Append(t.Context(), []model.Value{{Type: model.TypeBytes, BytesVal: []byte("0123456789abcdef")}}); err != nil {
		t.Fatal(err)
	}
	var published columnar.Batch
	if err := builder.Publish(&published); err != nil {
		t.Fatal(err)
	}
	before := q.Usage().Work.Live
	blocker, err := q.TryReserve(WorkMemory, 4096-before-16)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if err := builder.Append(t.Context(), []model.Value{{Type: model.TypeBytes, BytesVal: []byte("x")}}); !errors.Is(err, ErrBudget) {
		t.Fatalf("growth ignored temporary old buffer: %v", err)
	}
	if got := q.Usage().Work.Live; got != 4080 {
		t.Fatalf("failed resize changed live bytes: %d", got)
	}
	blocker.Release()
	if err := builder.Append(t.Context(), []model.Value{{Type: model.TypeBytes, BytesVal: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if err := builder.Close(); err != nil {
		t.Fatal(err)
	}
	copied, _, err := columnar.CopyValue(t.Context(), &published.Columns[0], 0, 16)
	if err != nil || string(copied.BytesVal) != "0123456789abcdef" {
		t.Fatalf("published earlier row changed: %q, %v", copied.BytesVal, err)
	}
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("growth claims leaked: %+v", usage)
	}
}
