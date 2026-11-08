package neosepia

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestSourceBudgetFollowsRetainedSelection(t *testing.T) {
	file := openRun(t, writeRun(t,
		runBlock(t, strings.Repeat("a", 16<<10), "", 10, storage.MutationUpsert, 1),
		runBlock(t, strings.Repeat("b", 16<<10), "", 10, storage.MutationUpsert, 2)))
	memory := &sourceMemory{limit: 24 << 10}
	cursor, err := newRunVersionCursor(file, 0, 2, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	row, err := cursor.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	selected := row.lease.retain()
	defer func() {
		if selected != nil {
			if err := selected.release(); err != nil {
				t.Error(err)
			}
		}
	}()
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.ReadBlock(t.Context(), 1, 1<<20, memory); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("closed cursor released a still-selected block's budget: %v", err)
	}
	value, err := selected.block.Column(t.Context(), 1)
	if err != nil || value.Int64[row.row] != 1 {
		t.Fatalf("retained selection: %v, %v", value.Int64, err)
	}
	if err := selected.release(); err != nil {
		t.Fatal(err)
	}
	selected = nil
	replacement, err := file.ReadBlock(t.Context(), 1, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}
