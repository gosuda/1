package neosepia

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func newMutationBlockFixture(t *testing.T, columns []storage.ColumnLayout, buildBytes uint64) *mutationBlockBuilder {
	t.Helper()
	builder, err := newMutationBlockBuilder(t.Context(), storage.ObjectLayout{Target: encTestTarget(), Columns: columns}, 20, mutationBlockLimits{BuildBytes: buildBytes, SortBytes: 1 << 20, Block: encTestLimits()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.Close(); err != nil {
			t.Error(err)
		}
	})
	return builder
}

func TestMutationBlockSortPreservesRangePointPrecedence(t *testing.T) {
	builder := newMutationBlockFixture(t, scanFixtureSpec().Layout, 1<<20)
	mutations := []storage.Mutation{
		scanOwnMutation(storage.MutationUpsert, "z", "", 100),
		scanOwnMutation(storage.MutationUpsert, "a", "", 10),
		scanOwnMutation(storage.MutationDeleteRange, "a", "y", 0),
		scanOwnMutation(storage.MutationUpsert, "b", "", 20),
		scanOwnMutation(storage.MutationUpsert, "a", "", 30),
		scanOwnMutation(storage.MutationDelete, "z", "", 0),
		scanOwnMutation(storage.MutationUpsert, "y", "", 40),
	}
	for i, mutation := range mutations {
		mutation.Ordinal = uint64(i + 1)
		if err := builder.Append(t.Context(), mutation); err != nil {
			t.Fatal(err)
		}
	}
	data, err := builder.Flush(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := openRun(t, writeRun(t, data))
	expected := []visibleVersion{{"a", 30}, {"b", 20}, {"y", 40}}
	if got := visibleRunVersions(t, []*sstFile{run}, 20); !slices.Equal(got, expected) {
		t.Fatalf("sorted mutations changed visibility: got %v, want %v", got, expected)
	}
	if got := visibleRunVersions(t, []*sstFile{run}, 19); len(got) != 0 {
		t.Fatalf("uncommitted block versions became visible: %v", got)
	}
}

func TestMutationBlockFailedAppendPreservesFlushablePrefix(t *testing.T) {
	columns := []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, {ID: 2, Type: model.Type{ID: 25, Storage: model.TypeString}}}
	builder := newMutationBlockFixture(t, columns, 32<<10)
	first := storage.NewUpsertMutation(encTestTarget(), []byte("z"), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(1), model.StringVal("first")})
	first.Ordinal = 1
	if err := builder.Append(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	first.Key[0] = 'x'
	first.Values[1] = model.StringVal("changed")
	second := storage.NewUpsertMutation(encTestTarget(), []byte("a"), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(2), model.StringVal(strings.Repeat("x", 64<<10))})
	second.Ordinal = 2
	if err := builder.Append(t.Context(), second); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("oversized append: %v", err)
	}
	prefix, err := builder.Flush(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second.Values[1] = model.StringVal("second")
	if err := builder.Append(t.Context(), second); err != nil {
		t.Fatalf("retry after flushing prefix: %v", err)
	}
	suffix, err := builder.Flush(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	runs := []*sstFile{openRun(t, writeRun(t, prefix)), openRun(t, writeRun(t, suffix))}
	for _, test := range []struct {
		key    string
		number int64
		text   string
	}{{"a", 2, "second"}, {"z", 1, "first"}} {
		request := storage.ReadRequest{Target: encTestTarget(), Key: []byte(test.key), ColumnIDs: []model.ColumnID{1, 2}, Layout: columns}
		row, err := readPAXPoint(t.Context(), pointSource(t, runs, 20), nil, request, pointReadLimits{ScanBytes: 1 << 20, RowBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		if row.Values[0].IntVal != test.number || row.Values[1].StrVal != test.text {
			t.Fatalf("key %s lost atomic append values: %+v", test.key, row.Values)
		}
	}
}
