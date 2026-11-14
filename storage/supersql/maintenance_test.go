package supersql

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// retentionTestClock positions the deterministic test clock at a fixed instant
// far enough past the epoch that the default 24-hour retention cutoff stays
// inside the positive SI nanosecond range model.Timestamp enforces. The plain
// test clock sits microseconds past the epoch, where every cutoff underflows.
func retentionTestClock() *testClock {
	clk := newTestClock()
	clk.mu.Lock()
	clk.now = clock.Instant(1_700_000_000_000_000_000)
	clk.mu.Unlock()
	return clk
}

// commitSegments returns the published commit segment names of one directory.
func commitSegments(t testing.TB, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "commits"))
	if err != nil {
		t.Fatalf("read commits directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// runFileNames lists every run file of one directory, including manifests.
func runFileNames(t testing.TB, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "runs"))
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// runDirectoryBytes reports the resident size of every run file.
func runDirectoryBytes(t testing.TB, directory string) int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "runs"))
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat run file %q: %v", entry.Name(), err)
		}
		total += info.Size()
	}
	return total
}

func insertMaintenanceRows(t testing.TB, ctx context.Context, database *Catalog, table string, count int) {
	t.Helper()
	for id := 1; id <= count; id++ {
		if _, err := database.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (%d, %d)`, table, id, id*10), 0); err != nil {
			t.Fatalf("INSERT %d: %v", id, err)
		}
	}
}

// TestMaintenanceLoopBoundsHistory proves Open starts a loop that retires
// published commit segments and obsolete run files while the database stays
// open and readable.
func TestMaintenanceLoopBoundsHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	directory := testDirectory(t)
	database, err := Open(ctx, OpenOptions{
		Directory: directory, Clock: retentionTestClock(),
		Maintenance: MaintenanceConfig{Interval: MinMaintenanceInterval, WALThreshold: 8, RunThreshold: 8},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := database.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}
	})
	if _, err := database.Exec(ctx, `CREATE TABLE maintenance_items (id bigint PRIMARY KEY, value bigint)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	insertMaintenanceRows(t, ctx, database, "maintenance_items", 40)

	deadline := time.Now().Add(10 * time.Second)
	var segments []string
	for {
		segments = commitSegments(t, directory)
		if len(segments) < 16 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance did not bound commits/: %d segments after 10s", len(segments))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := database.MaintenanceError(); err != nil {
		t.Fatalf("maintenance error while running: %v", err)
	}
	selected, err := database.Exec(ctx, `SELECT count(*) FROM maintenance_items`, 0)
	if err != nil {
		t.Fatalf("SELECT after maintenance: %v", err)
	}
	if len(selected.Rows) != 1 || selected.Rows[0][0].IntVal != 40 {
		t.Fatalf("maintained rows: %+v", selected.Rows)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	if err := database.MaintenanceError(); err != nil {
		t.Fatalf("maintenance error after Close: %v", err)
	}
}

// TestMaintenanceDisabledLeavesHistory pins the explicit opt-out: with the loop
// disabled nothing retires commit segments.
func TestMaintenanceDisabledLeavesHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	directory := testDirectory(t)
	database, err := Open(ctx, OpenOptions{
		Directory: directory, Clock: retentionTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true, Interval: MinMaintenanceInterval, WALThreshold: 1, RunThreshold: 1},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := database.Exec(ctx, `CREATE TABLE disabled_items (id bigint PRIMARY KEY, value bigint)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	insertMaintenanceRows(t, ctx, database, "disabled_items", 40)
	time.Sleep(300 * time.Millisecond)
	if segments := commitSegments(t, directory); len(segments) < 40 {
		t.Fatalf("disabled maintenance retired history: %d segments", len(segments))
	}
	if err := database.MaintenanceError(); err != nil {
		t.Fatalf("disabled maintenance reported an error: %v", err)
	}
}

// TestMaintenanceErrorSurfacesFatalFailure proves the fatal path: a maintenance
// failure that a later pass cannot retry stops the loop, is observable from
// MaintenanceError, and is delivered by Close.
func TestMaintenanceErrorSurfacesFatalFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t), Clock: retentionTestClock(),
		Maintenance: MaintenanceConfig{Interval: MinMaintenanceInterval, WALThreshold: 1, RunThreshold: 1},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil && !errors.Is(err, database.MaintenanceError()) {
			t.Errorf("Close: %v", err)
		}
	})
	if _, err := database.Exec(ctx, `CREATE TABLE fatal_items (id bigint PRIMARY KEY)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	// Closing the backend underneath the loop makes the next probe report
	// ErrClosed, which the loop treats as fatal.
	if err := database.backend.Close(); err != nil {
		t.Fatalf("backend Close: %v", err)
	}
	var reported error
	deadline := time.Now().Add(10 * time.Second)
	for {
		if reported = database.MaintenanceError(); reported != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fatal maintenance failure was not reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := database.Close(); !errors.Is(err, reported) {
		t.Fatalf("Close after a fatal maintenance failure = %v, want %v", err, reported)
	}
}

// TestOpenRejectsInvalidMaintenanceInterval pins the option rejection and the
// absence of side effects: a rejected Open creates no directory.
func TestOpenRejectsInvalidMaintenanceInterval(t *testing.T) {
	for _, interval := range []time.Duration{-time.Second, MinMaintenanceInterval - time.Millisecond} {
		directory := filepath.Join(t.TempDir(), "rejected")
		database, err := Open(t.Context(), OpenOptions{
			Directory: directory, Clock: newTestClock(), Maintenance: MaintenanceConfig{Interval: interval},
		})
		if database != nil {
			if closeErr := database.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			t.Fatalf("Open accepted maintenance interval %s", interval)
		}
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != "22023" || sqlErr.Message != "invalid maintenance configuration" {
			t.Fatalf("Open maintenance interval %s = %v, want 22023", interval, err)
		}
		if _, statErr := os.Stat(directory); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rejected Open created %s: %v", directory, statErr)
		}
	}
	// The boundary value itself is accepted.
	database, err := Open(t.Context(), OpenOptions{
		Directory: testDirectory(t), Clock: newTestClock(),
		Maintenance: MaintenanceConfig{Interval: MinMaintenanceInterval, WALThreshold: 1, RunThreshold: 1},
	})
	if err != nil {
		t.Fatalf("Open at MinMaintenanceInterval: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestMaintenanceSigkillHelper is the child of TestMaintenanceSigkillRecovery.
// It acknowledges every committed row on stdout so the parent can kill it at an
// arbitrary acknowledged boundary.
func TestMaintenanceSigkillHelper(t *testing.T) {
	directory := os.Getenv("SUPERSQL_MAINTENANCE_SIGKILL_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	first, err := strconv.Atoi(os.Getenv("SUPERSQL_MAINTENANCE_SIGKILL_FIRST"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{
		Directory: directory, Clock: retentionTestClock(),
		Maintenance: MaintenanceConfig{Interval: MinMaintenanceInterval, WALThreshold: 4, RunThreshold: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	for id := first; ; id++ {
		if _, err := database.Exec(ctx, fmt.Sprintf(`INSERT INTO sigkill_items VALUES (%d, %d)`, id, id), 0); err != nil {
			t.Fatalf("INSERT %d: %v", id, err)
		}
		fmt.Printf("acked %d\n", id)
	}
}

// TestMaintenanceSigkillRecovery kills a writer mid-maintenance five times and
// requires every acknowledged row to survive, contiguously, after each reopen.
func TestMaintenanceSigkillRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	directory := testDirectory(t)
	database, err := Open(ctx, OpenOptions{Directory: directory, Clock: retentionTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := database.Exec(ctx, `CREATE TABLE sigkill_items (id bigint PRIMARY KEY, value bigint)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	last := 0
	for iteration := 1; iteration <= 5; iteration++ {
		first := last + 1
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMaintenanceSigkillHelper$")
		command.Env = append(os.Environ(),
			"SUPERSQL_MAINTENANCE_SIGKILL_DIRECTORY="+directory,
			"SUPERSQL_MAINTENANCE_SIGKILL_FIRST="+strconv.Itoa(first),
			"TMPDIR="+t.TempDir(),
		)
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(stdout)
		acked := first - 1
		for acked-first+1 < 30 {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("iteration %d: writer stopped before 30 acknowledgements: %v", iteration, err)
			}
			value, ok := strings.CutPrefix(strings.TrimSpace(line), "acked ")
			if !ok {
				continue
			}
			acked, err = strconv.Atoi(value)
			if err != nil {
				t.Fatalf("iteration %d: unreadable acknowledgement %q", iteration, line)
			}
		}
		if err := command.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("iteration %d: kill writer: %v", iteration, err)
		}
		if err := command.Wait(); err == nil {
			t.Fatalf("iteration %d: killed writer exited successfully", iteration)
		}
		// The writer may have committed rows it never acknowledged on stdout, so
		// the reopen reports the true high-water mark for the next iteration.
		last = requireContiguousRows(t, ctx, directory, acked)
	}
}

// requireContiguousRows reopens one directory and requires the surviving rows to
// be exactly ids 1..N with no gap and N >= acknowledged. It returns N.
func requireContiguousRows(t testing.TB, ctx context.Context, directory string, acknowledged int) int {
	t.Helper()
	database, err := Open(ctx, OpenOptions{Directory: directory, Clock: retentionTestClock()})
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	}()
	outcome, err := database.Exec(ctx, `SELECT id FROM sigkill_items ORDER BY id`, 0)
	if err != nil {
		t.Fatalf("SELECT after kill: %v", err)
	}
	if len(outcome.Rows) < acknowledged {
		t.Fatalf("recovered %d rows, want at least the %d acknowledged", len(outcome.Rows), acknowledged)
	}
	for index, row := range outcome.Rows {
		if len(row) != 1 || row[0].IntVal != int64(index+1) {
			t.Fatalf("recovered row %d = %+v, want id %d", index, row, index+1)
		}
	}
	return len(outcome.Rows)
}

// soakInserts bounds the sustained-growth soak. The resident run set holds one
// descriptor per live run file, and a single-row insert publishes about two run
// files, so the workload stops well below the per-process descriptor limit
// (kern.maxfilesperproc is 10240 on macOS). The live run-file count is logged so
// the file-count growth stays visible without exhausting the process.
const soakInserts = 2000

// TestCatalogSoakBoundedGrowth is the env-gated soak: sustained writes through
// sql.DB keep commits/ and runs/ inside fixed bounds, and reopen stays fast.
func TestCatalogSoakBoundedGrowth(t *testing.T) {
	if os.Getenv("SUPERSQL_SOAK") == "" {
		t.Skip("set SUPERSQL_SOAK=1 to run the sustained growth soak")
	}
	ctx := t.Context()
	directory := testDirectory(t)
	connector, err := OpenConnector(ctx, OpenOptions{
		Directory: directory, Clock: retentionTestClock(),
		Maintenance: MaintenanceConfig{Interval: MinMaintenanceInterval, WALThreshold: 64, RunThreshold: 64},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	database := sql.OpenDB(connector)
	if _, err := database.ExecContext(ctx, `CREATE TABLE soak_items (id bigint PRIMARY KEY, value bigint)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	var maxSegments, maxRunBytes, maxRunFiles int64
	for id := 1; id <= soakInserts; id++ {
		if _, err := database.ExecContext(ctx, fmt.Sprintf(`INSERT INTO soak_items VALUES (%d, %d)`, id, id)); err != nil {
			t.Fatalf("INSERT %d: %v", id, err)
		}
		if id%1000 != 0 {
			continue
		}
		segments := int64(len(commitSegments(t, directory)))
		runBytes := runDirectoryBytes(t, directory)
		runFiles := int64(len(runFileNames(t, directory)))
		maxSegments = max(maxSegments, segments)
		maxRunBytes = max(maxRunBytes, runBytes)
		maxRunFiles = max(maxRunFiles, runFiles)
		t.Logf("after %d inserts: commits=%d run_files=%d runs_bytes=%d", id, segments, runFiles, runBytes)
	}
	if maxSegments > 256 {
		t.Fatalf("commits/ reached %d segments, want at most 256", maxSegments)
	}
	if maxRunBytes >= 32<<20 {
		t.Fatalf("runs/ reached %d bytes, want less than 32 MiB", maxRunBytes)
	}
	if maxRunFiles > int64(neosepia.DefaultLimits().MaxFiles) {
		t.Fatalf("live run files reached %d, want at most the resident bound %d", maxRunFiles, neosepia.DefaultLimits().MaxFiles)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("sql.DB close: %v", err)
	}
	if err := connector.Close(); err != nil {
		t.Fatalf("Connector close: %v", err)
	}
	started := time.Now()
	reopened, err := Open(ctx, OpenOptions{Directory: directory, Clock: retentionTestClock(), Maintenance: MaintenanceConfig{Disabled: true}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	elapsed := time.Since(started)
	if err := reopened.Close(); err != nil {
		t.Fatalf("reopen Close: %v", err)
	}
	t.Logf("soak reopen: %s (max commits %d, max run files %d, max runs bytes %d)", elapsed, maxSegments, maxRunFiles, maxRunBytes)
	if elapsed > 5*time.Second {
		t.Fatalf("reopen took %s, want less than 5s", elapsed)
	}
}
