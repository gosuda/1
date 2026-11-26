package supersql

import (
	"database/sql"
	"fmt"
	"testing"
)

// TestCloseSucceedsAfterLongSessionWithRealisticClock is the end-to-end guard for
// "Close() returns storage corruption after a longer session". A maintenance
// pass that re-selected an already rewritten destination linked two overlapping
// level-1 fragments of one source, runGeneration.validateLevels rejected them,
// and the maintenance goroutine's fatal error surfaced from sql.DB.Close (which
// runs the connector's io.Closer). The clock reads like a deployed one
// (nanoseconds since the Unix epoch), so retention actually collects; with the
// default test clock the collection is skipped as younger than the retention
// window. It starts from an empty directory on purpose: the shared test template
// is bootstrapped with the default (young) clock, and a clone of it hides the
// defect because its stored timestamps no longer agree with this clock. The
// failure depends on when background maintenance runs (2 of 3 runs on unfixed
// code), so the deterministic regression test is
// TestCompactionExcludesRewrittenDestinationFromLaterIntervals in neosepia.
func TestCloseSucceedsAfterLongSessionWithRealisticClock(t *testing.T) {
	ctx := t.Context()
	clk := newTestClock()
	clk.now = 1_700_000_000_000_000_000
	connector, err := OpenConnector(ctx, OpenOptions{Directory: t.TempDir(), Clock: clk})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	pool := sql.OpenDB(connector)
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	for i := range 150 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TYPE e%d AS ENUM ('a')", i)); err != nil {
			t.Fatalf("create e%d: %v", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "ALTER TYPE e0 ADD VALUE 'z'"); err != nil {
		t.Fatalf("ALTER TYPE: %v", err)
	}
	var got string
	if err := conn.QueryRowContext(ctx, "SELECT 'z'::e0::text").Scan(&got); err != nil || got != "z" {
		t.Fatalf("SELECT new value = %q, %v", got, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("conn close: %v", err)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("pool close after long session: %v", err)
	}
}
