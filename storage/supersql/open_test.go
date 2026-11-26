package supersql

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// testClock is a deterministic certified clock. It never reads wall time: every
// read returns the current instant and every certified wait advances past the
// requested boundary, which is what the commit sequencer requires.
type testClock struct {
	mu    sync.Mutex
	now   clock.Instant
	scale clock.ScaleDescriptor
	epoch clock.Epoch
}

func newTestClock() *testClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identifier := sha256.Sum256(history)
	return &testClock{
		now:   1000,
		scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identifier, CanonicalLeapHistory: history},
		epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: identifier},
	}
}

func (c *testClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += 1000
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, nil
}

func (c *testClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if value < 0 || value == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.now = max(c.now, (value/1000+1)*1000)
	return nil
}

func (c *testClock) Scale() clock.ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

func (c *testClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}

func (c *testClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(int64(value), 0).UTC()}, nil
}

// TestOpenExecutesStatementsEndToEnd is the acceptance path for the root
// composition: a real database in a fresh directory executes DDL and DML through
// the session stack and reads the committed rows back.
func TestOpenExecutesStatementsEndToEnd(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	created, err := database.Exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, value bigint)`, 0)
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if created.Tag != "CREATE TABLE" {
		t.Fatalf("CREATE TABLE tag = %q", created.Tag)
	}

	inserted, err := database.Exec(ctx, `INSERT INTO items VALUES (1, 10), (2, 20), (3, 30)`, 0)
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if inserted.RowsAffected != 3 || !inserted.Committed {
		t.Fatalf("INSERT outcome = %+v", inserted)
	}

	selected, err := database.Exec(ctx, `SELECT id, value FROM items ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if len(selected.Columns) != 2 || selected.Columns[0].Name != "id" || selected.Columns[1].Name != "value" {
		t.Fatalf("SELECT columns = %+v", selected.Columns)
	}
	got := readInt64Rows(t, selected.Rows)
	want := [][2]int64{{1, 10}, {2, 20}, {3, 30}}
	if len(got) != len(want) {
		t.Fatalf("SELECT rows = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("SELECT rows = %v, want %v", got, want)
		}
	}

	// Reopening the same directory must load the durable catalog and keep the rows.
	directory := database.Directory()
	if err := database.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	reopened, err := Open(ctx, OpenOptions{Directory: directory, Clock: newTestClock()})
	if err != nil {
		t.Fatalf("reopen Open: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("reopen Close: %v", err)
		}
	}()
	after, err := reopened.Exec(ctx, `SELECT count(*) FROM items`, 0)
	if err != nil {
		t.Fatalf("SELECT after reopen: %v", err)
	}
	if counts := readInt64Column(t, after.Rows); len(counts) != 1 || counts[0] != 3 {
		t.Fatalf("count after reopen = %v", counts)
	}
}

// TestOpenRequiresClock proves the typed error for a missing and a typed-nil
// clock, and that no directory is created when the clock is missing.
func TestOpenRequiresClock(t *testing.T) {
	ctx := t.Context()
	directory := filepath.Join(t.TempDir(), "never-created")
	for name, source := range map[string]clock.Clock{
		"nil":           nil,
		"typed-pointer": (*testClock)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Open(ctx, OpenOptions{Directory: directory, Clock: source})
			if !errors.Is(err, ErrClockRequired) {
				t.Fatalf("Open error = %v, want ErrClockRequired", err)
			}
			if _, statErr := os.Stat(directory); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("directory was created despite the clock error: %v", statErr)
			}
		})
	}
}

// TestOpenRejectsLegacyDirectoryWithoutWriting proves the upgrade gate: a
// directory holding legacy files yields a typed UpgradeRequired error and is
// left byte-for-byte unchanged.
func TestOpenRejectsLegacyDirectoryWithoutWriting(t *testing.T) {
	ctx := t.Context()
	directory := t.TempDir()
	files := map[string]string{
		"catalog_manifest.json":  `{"legacy":true}`,
		"heap_sequences.json":    `{}`,
		"descriptor_latest.json": `{}`,
		"wal.log":                "legacy write-ahead log",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := directorySnapshot(t, directory)

	_, err := Open(ctx, OpenOptions{Directory: directory, Clock: newTestClock()})
	var upgrade *UpgradeRequired
	if !errors.As(err, &upgrade) {
		t.Fatalf("Open error = %v, want UpgradeRequired", err)
	}
	if upgrade.Directory != directory {
		t.Fatalf("UpgradeRequired.Directory = %q, want %q", upgrade.Directory, directory)
	}
	if !errors.Is(err, neosepia.ErrIncompatibleFormat) {
		t.Fatalf("UpgradeRequired cause = %v, want ErrIncompatibleFormat", upgrade.Cause)
	}
	after := directorySnapshot(t, directory)
	if before != after {
		t.Fatalf("legacy directory changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestCloseIsIdempotent proves repeated Close returns the same first error and
// that statements fail with the typed closed error afterwards.
func TestCloseIsIdempotent(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first := database.Close()
	if first != nil {
		t.Fatalf("first Close: %v", first)
	}
	if second := database.Close(); second != first {
		t.Fatalf("second Close = %v, want %v", second, first)
	}
	if _, err := database.Exec(ctx, `SELECT 1`, 1); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("Exec after Close = %v, want ErrDatabaseClosed", err)
	}
}

// readInt64Rows projects materialized result rows onto their integer values.
func readInt64Rows(t *testing.T, rows [][]model.Value) [][2]int64 {
	t.Helper()
	result := make([][2]int64, 0, len(rows))
	for _, row := range rows {
		if len(row) != 2 {
			t.Fatalf("row %v has %d columns, want 2", row, len(row))
		}
		result = append(result, [2]int64{row[0].IntVal, row[1].IntVal})
	}
	return result
}

// readInt64Column projects a single-integer-column result.
func readInt64Column(t *testing.T, rows [][]model.Value) []int64 {
	t.Helper()
	result := make([]int64, 0, len(rows))
	for _, row := range rows {
		if len(row) != 1 {
			t.Fatalf("row %v has %d columns, want 1", row, len(row))
		}
		result = append(result, row[0].IntVal)
	}
	return result
}

func directorySnapshot(t *testing.T, directory string) string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		builder.WriteString(entry.Name())
		builder.WriteString("|")
		builder.WriteString(info.Mode().String())
		builder.WriteString("|")
		builder.Write(content)
		builder.WriteString("\n")
	}
	return builder.String()
}
