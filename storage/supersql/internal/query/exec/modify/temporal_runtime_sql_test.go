package modify_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func temporalSessionFixture(t *testing.T) *sessionExecutionFixture {
	t.Helper()
	fixture := newSessionExecutionFixture(t)
	if err := fixture.session.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.session, err = session.NewTransactions(t.Context(), session.TransactionConfig{Manager: fixture.store.manager, Settings: fixture.settings, Options: fixture.transactionOptions, TemporalClock: fixture.store.clock})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func temporalClockAt(fixture *sessionExecutionFixture, instant time.Time) int64 {
	fixture.store.clock.mu.Lock()
	fixture.store.clock.now = max(fixture.store.clock.now, clock.Instant(instant.UnixNano()))
	fixture.store.clock.mu.Unlock()
	return instant.UnixMicro() - 946684800000000
}

func temporalRows(t *testing.T, fixture *sessionExecutionFixture, sql string) ([][]int64, error) {
	t.Helper()
	var result [][]int64
	_, err := runStatementSQL(t, fixture, sql, fixture.statementOptions(t, sql), func(ctx context.Context, _ *session.Prepared, rows *session.Rows) error {
		var err error
		result, err = collectWritableRows(ctx, rows)
		return err
	})
	return result, err
}

func TestSQLTemporalFramesRespectTransactionAndStatementLifetimes(t *testing.T) {
	fixture := temporalSessionFixture(t)
	start := time.Date(2021, 11, 7, 5, 30, 12, 123456000, time.UTC)
	transactionTime := temporalClockAt(fixture, start)
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	const sql = `SELECT now(),statement_timestamp(),clock_timestamp()`
	for _, seconds := range []int{0, 2} {
		statementTime := temporalClockAt(fixture, start.Add(time.Duration(seconds)*time.Second))
		got, err := temporalRows(t, fixture, sql)
		want := [][]int64{{transactionTime, statementTime, statementTime}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("clock frame = %v, %v; want %v", got, err, want)
		}
	}
	if err := fixture.session.Savepoint(t.Context(), "clock_savepoint"); err != nil {
		t.Fatal(err)
	}
	later := temporalClockAt(fixture, start.Add(4*time.Second))
	if err := fixture.session.RollbackTo(t.Context(), "clock_savepoint"); err != nil {
		t.Fatal(err)
	}
	got, err := temporalRows(t, fixture, sql)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{transactionTime, later, later}}) {
		t.Fatalf("savepoint reset transaction time: %v, %v", got, err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	fresh := temporalClockAt(fixture, start.Add(6*time.Second))
	got, err = temporalRows(t, fixture, sql)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{fresh, fresh, fresh}}) {
		t.Fatalf("new transaction reused old clock frame: %v, %v", got, err)
	}
}

func TestSQLVolatileClockChangesWithoutChangingStatementTime(t *testing.T) {
	fixture := temporalSessionFixture(t)
	start := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
	fixed := temporalClockAt(fixture, start)
	const sql = `SELECT now(),statement_timestamp(),clock_timestamp() FROM generate_series(1,4)`
	_, err := runStatementSQL(t, fixture, sql, fixture.statementOptions(t, sql), func(ctx context.Context, _ *session.Prepared, rows *session.Rows) error {
		for index := range 2 {
			live := temporalClockAt(fixture, start.Add(time.Duration(index)*time.Second))
			batch := columnar.Batch{MaxRows: 1}
			if err := rows.Next(ctx, &batch); err != nil {
				return err
			}
			if batch.Rows() != 1 {
				t.Fatalf("clock fetch rows=%d", batch.Rows())
			}
			for column, want := range []int64{fixed, fixed, live} {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[column], batch.Lane(0), 128)
				if err != nil {
					return err
				}
				if value.IntVal != want {
					t.Fatalf("row %d clock %d=%d; want %d", index, column, value.IntVal, want)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreparedRelativeLiteralFreezesButCurrentTimestampDoesNot(t *testing.T) {
	fixture := temporalSessionFixture(t)
	start := time.Date(2021, 1, 2, 3, 4, 5, 123456000, time.UTC)
	frozen := temporalClockAt(fixture, start)
	const sql = `SELECT timestamp 'now',CURRENT_TIMESTAMP`
	var prepared *session.Prepared
	_, err := runStatementSQL(t, fixture, sql, fixture.statementOptions(t, sql), func(ctx context.Context, bound *session.Prepared, rows *session.Rows) error {
		prepared = bound
		_, err := collectWritableRows(ctx, rows)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	current := temporalClockAt(fixture, start.Add(time.Hour))
	var got [][]int64
	_, err = fixture.session.RunPrepared(t.Context(), prepared, fixture.invocation(t), func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	if err != nil || !reflect.DeepEqual(got, [][]int64{{frozen, current}}) {
		t.Fatalf("prepared temporal values = %v, %v", got, err)
	}
}

func TestFixedSnapshotQueriesDoNotRequireAnUnusedTemporalClock(t *testing.T) {
	fixture := temporalSessionFixture(t)
	instant := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
	fixed := temporalClockAt(fixture, instant)
	readOnly := true
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{Characteristics: session.BeginOptions{ReadOnly: &readOnly}}); err != nil {
		t.Fatal(err)
	}
	if _, err := temporalRows(t, fixture, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	fixture.store.clock.mu.Lock()
	fixture.store.clock.unavailable = true
	fixture.store.clock.mu.Unlock()
	defer func() {
		fixture.store.clock.mu.Lock()
		fixture.store.clock.unavailable = false
		fixture.store.clock.mu.Unlock()
	}()
	got, err := temporalRows(t, fixture, `SELECT 1,now()`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{1, fixed}}) {
		t.Fatalf("unused live clock blocked retained snapshot: %v, %v", got, err)
	}
	_, err = temporalRows(t, fixture, `SELECT statement_timestamp()`)
	requireUniqueSQLState(t, err, "55000")
	if err := fixture.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedTemporalDefaultsGeneratedValuesAndChecksKeepDefinitionTime(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	start := time.Date(2021, 1, 2, 3, 4, 5, 123456000, time.UTC)
	frozen := temporalClockAt(fixture, start)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE temporal_defs(
		frozen timestamp DEFAULT timestamp 'now',
		live timestamptz DEFAULT CURRENT_TIMESTAMP,
		generated timestamp GENERATED ALWAYS AS (timestamp 'now') STORED,
		times timestamp[] DEFAULT ARRAY[timestamp 'now',timestamp '2000-01-01'],
		memo text DEFAULT 'now',
		CHECK (generated=timestamp 'now' AND times[1]=timestamp 'now')
	)`)
	if err != nil {
		t.Fatal(err)
	}
	// Each implicit transaction resolves the committed policy from catalog data.
	for _, advance := range []time.Duration{time.Hour, 2 * time.Hour} {
		current := temporalClockAt(fixture, start.Add(advance))
		requireIdentityRows(t, fixture, sequences, dependencies, table,
			`INSERT INTO temporal_defs DEFAULT VALUES RETURNING frozen,live,generated,times[1],CASE WHEN memo='now' THEN 1 ELSE 0 END`,
			[][]int64{{frozen, current, frozen, frozen, 1}})
	}
}
