package supersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// openTestDB opens one connector-backed sql.DB over a fresh directory.
func openTestDB(t *testing.T) (*sql.DB, *Connector) {
	t.Helper()
	connector, err := OpenConnector(t.Context(), OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	database := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("sql.DB close: %v", err)
		}
		if err := connector.Close(); err != nil {
			t.Errorf("Connector close: %v", err)
		}
	})
	return database, connector
}

// TestConnectorRequiresClock pins the typed errors: OpenConnector without a
// clock, and every bare-Driver construction path, all fail with ErrClockRequired.
func TestConnectorRequiresClock(t *testing.T) {
	if _, err := OpenConnector(t.Context(), OpenOptions{Directory: t.TempDir()}); !errors.Is(err, ErrClockRequired) {
		t.Fatalf("OpenConnector without clock = %v, want ErrClockRequired", err)
	}
	var d driver.Driver = Driver{}
	if _, err := d.Open("anything"); !errors.Is(err, ErrClockRequired) {
		t.Fatalf("Driver.Open = %v, want ErrClockRequired", err)
	}
	if _, err := (Driver{}).OpenConnector("anything"); !errors.Is(err, ErrClockRequired) {
		t.Fatalf("Driver.OpenConnector = %v, want ErrClockRequired", err)
	}
}

// TestConnectorEndToEnd exercises the prescribed public smoke path:
// sql.OpenDB over OpenConnector, DDL, typed parameters, a transaction through
// BeginTx, and typed results out of QueryContext.
func TestConnectorEndToEnd(t *testing.T) {
	ctx := t.Context()
	db, connector := openTestDB(t)

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, label text, price float8, note bytea)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items VALUES ($1, $2, $3, $4)`, 1, "alpha", 1.5, []byte{0xde, 0xad}); err != nil {
		t.Fatalf("INSERT params: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items VALUES ($1, $2, $3, $4)`, 2, nil, 2.5, nil); err != nil {
		t.Fatalf("INSERT NULL params: %v", err)
	}

	// Explicit transaction through database/sql.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO items VALUES ($1, $2, $3, $4)`, 3, "gamma", 3.5, nil); err != nil {
		t.Fatalf("tx INSERT: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("tx Commit: %v", err)
	}

	var label string
	var price float64
	var note []byte
	row := db.QueryRowContext(ctx, `SELECT label, price, note FROM items WHERE id = $1`, 1)
	if err := row.Scan(&label, &price, &note); err != nil {
		t.Fatalf("SELECT scan: %v", err)
	}
	if label != "alpha" || price != 1.5 || string(note) != string([]byte{0xde, 0xad}) {
		t.Fatalf("row = (%q, %v, %v)", label, price, note)
	}
	row = db.QueryRowContext(ctx, `SELECT label FROM items WHERE id = $1`, 2)
	var nullable sql.NullString
	if err := row.Scan(&nullable); err != nil {
		t.Fatalf("NULL scan: %v", err)
	}
	if nullable.Valid {
		t.Fatalf("NULL label scanned as %q", nullable.String)
	}

	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	// The connector's own Driver still cannot open anything.
	if _, err := connector.Driver().Open("x"); !errors.Is(err, ErrClockRequired) {
		t.Fatalf("connector Driver().Open = %v, want ErrClockRequired", err)
	}
}

// TestConnectorSessionIsolation proves per-connection session state never
// crosses connections: named prepared statements, transaction visibility and
// advisory-lock ownership are all scoped to one connection.
func TestConnectorSessionIsolation(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	connA, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn A: %v", err)
	}
	defer connA.Close()
	connB, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn B: %v", err)
	}
	defer connB.Close()

	if _, err := connA.ExecContext(ctx, `CREATE TABLE shared (n bigint)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := connA.ExecContext(ctx, `PREPARE iso_p AS SELECT 42`); err != nil {
		t.Fatalf("PREPARE on A: %v", err)
	}
	var answer int64
	if err := connA.QueryRowContext(ctx, `EXECUTE iso_p`).Scan(&answer); err != nil || answer != 42 {
		t.Fatalf("EXECUTE on A = %d, %v", answer, err)
	}
	if err := connB.QueryRowContext(ctx, `EXECUTE iso_p`).Scan(&answer); err == nil {
		t.Fatalf("EXECUTE on B succeeded; prepared statements leaked across sessions")
	}

	// A rolled-back transaction leaves nothing visible to B, and an unrelated
	// table stays readable while A's transaction is still open.
	if _, err := connA.ExecContext(ctx, `CREATE TABLE untouched (n bigint)`); err != nil {
		t.Fatalf("CREATE TABLE untouched: %v", err)
	}
	if _, err := connA.ExecContext(ctx, `INSERT INTO untouched VALUES (5)`); err != nil {
		t.Fatalf("seed INSERT: %v", err)
	}
	tx, err := connA.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO shared VALUES (1)`); err != nil {
		t.Fatalf("tx INSERT: %v", err)
	}
	var visible int64
	if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM untouched`).Scan(&visible); err != nil || visible != 1 {
		t.Fatalf("B unrelated read = %d, %v", visible, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM shared`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("B count after rollback = %d, %v", visible, err)
	}

	// Advisory locks are owned per session: while A holds the lock, B's
	// non-blocking acquire must report it taken rather than succeed as the
	// same owner.
	var acquired bool
	if err := connA.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(7)`).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("advisory lock on A = %v, %v", acquired, err)
	}
	if err := connB.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(7)`).Scan(&acquired); err != nil {
		t.Fatalf("advisory lock on B: %v", err)
	}
	if acquired {
		t.Fatalf("B acquired the advisory lock A holds; session lock identity leaked")
	}
	if err := connA.QueryRowContext(ctx, `SELECT pg_advisory_unlock(7)`).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("advisory unlock on A = %v, %v", acquired, err)
	}
	if err := connB.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(7)`).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("advisory lock on B after release = %v, %v", acquired, err)
	}
}

// TestConnectorCloseOrdering pins the close contract: sql.DB before Connector,
// and statements fail with a typed error after the connector closes.
func TestConnectorCloseOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	connector, err := OpenConnector(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	if _, err := db.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	if err := connector.Close(); err != nil {
		t.Fatalf("Connector.Close: %v", err)
	}
	if _, err := connector.Connect(ctx); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("Connect after close = %v, want ErrDatabaseClosed", err)
	}
	if err := connector.Close(); err != nil {
		t.Fatalf("second Connector.Close = %v", err)
	}
}

// TestConnectorRowsStreamAndEarlyClose exercises the streaming row path:
// pulling rows through Next, closing mid-stream, and cancelling mid-stream all
// unwind the session invocation and leave the pool usable.
func TestConnectorRowsStreamAndEarlyClose(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	ctx := t.Context()
	db, _ := openTestDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE big (id bigint PRIMARY KEY, payload text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	// Keep the seed inside one statement's mutation-batch budget; several
	// batches of INSERT still exercise streaming across batch boundaries.
	for batch := 0; batch < 2; batch++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO big SELECT g, 'x' FROM generate_series($1, $2) AS g`, batch*120+1, batch*120+120); err != nil {
			t.Fatalf("seed batch %d: %v", batch, err)
		}
	}

	// Full drain.
	rows, err := db.QueryContext(ctx, `SELECT id FROM big ORDER BY id`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var seen int
	var last int64
	for rows.Next() {
		if err := rows.Scan(&last); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if seen != 240 || last != 240 {
		t.Fatalf("drained %d rows, last=%d", seen, last)
	}

	// Early close mid-stream unwinds cleanly.
	rows, err = db.QueryContext(ctx, `SELECT id FROM big ORDER BY id`)
	if err != nil {
		t.Fatalf("Query 2: %v", err)
	}
	if !rows.Next() {
		t.Fatal("no first row")
	}
	var first int64
	if err := rows.Scan(&first); err != nil || first != 1 {
		t.Fatalf("first = %d, %v", first, err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early Close: %v", err)
	}

	// Context cancellation mid-stream surfaces an error and unwinds.
	cancelCtx, cancel := context.WithCancel(ctx)
	rows, err = db.QueryContext(cancelCtx, `SELECT id FROM big ORDER BY id`)
	if err != nil {
		t.Fatalf("Query 3: %v", err)
	}
	if !rows.Next() {
		t.Fatal("no first row on cancel query")
	}
	cancel()
	for rows.Next() {
	}
	if err := rows.Err(); err == nil {
		t.Fatal("expected cancellation error from rows.Err()")
	}
	_ = rows.Close()

	// The pool stays healthy after all the unwinding.
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM big`).Scan(&count); err != nil || count != 240 {
		t.Fatalf("post-cancel count = %d, %v", count, err)
	}
}

// TestConnectorTimestampParameters covers time.Time binding and timestamp/date
// result typing through the driver conversion.
func TestConnectorTimestampParameters(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE events (id bigint PRIMARY KEY, at timestamptz, day date)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	moment := time.Date(2024, 3, 14, 15, 9, 26, 531000000, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO events VALUES ($1, $2, $3)`, 1, moment, "2024-03-14"); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	var got time.Time
	if err := db.QueryRowContext(ctx, `SELECT at FROM events WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("SELECT timestamptz: %v", err)
	}
	if !got.Equal(moment) {
		t.Fatalf("timestamptz round trip = %v, want %v", got, moment)
	}
}

// TestConnectorDeclaredParameterTypes pins the typed-parameter contract. A
// time.Time declares timestamptz, so date/time/timestamp targets receive real
// unit-conversion casts instead of reinterpreting the shared TypeTimestamp
// storage; cross-type numeric targets receive their assignment casts; and
// combinations PostgreSQL would reject stay rejected rather than corrupting.
func TestConnectorDeclaredParameterTypes(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE typed (id bigint PRIMARY KEY, day date, at time, stamp timestamp, zone timestamptz, amount numeric, weight float8, small int4, raw bytea)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	moment := time.Date(2024, 3, 14, 15, 9, 26, 531000000, time.UTC)
	if _, err := db.ExecContext(ctx, `INSERT INTO typed VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		int64(1), moment, moment, moment, moment, int64(42), int64(7), int64(9), []byte{0xca, 0xfe}); err != nil {
		t.Fatalf("INSERT typed params: %v", err)
	}

	var day time.Time
	var at, amount string
	var stamp, zone time.Time
	var weight float64
	var small int32
	var raw []byte
	row := db.QueryRowContext(ctx, `SELECT day, at, stamp, zone, amount, weight, small, raw FROM typed WHERE id = 1`)
	if err := row.Scan(&day, &at, &stamp, &zone, &amount, &weight, &small, &raw); err != nil {
		t.Fatalf("SELECT typed: %v", err)
	}
	if day.Year() != moment.Year() || day.Month() != moment.Month() {
		t.Fatalf("date column = %v; microseconds were stored as days", day)
	}
	if !zone.Equal(moment) {
		t.Fatalf("timestamptz = %v, want %v", zone, moment)
	}
	if stamp.Year() != 2024 || stamp.Hour() < 0 || stamp.Hour() > 23 {
		t.Fatalf("timestamp = %v", stamp)
	}
	if amount != "42" {
		t.Fatalf("numeric = %q, want 42", amount)
	}
	if weight != 7 || small != 9 || string(raw) != string([]byte{0xca, 0xfe}) {
		t.Fatalf("weight/small/raw = %v/%v/%v", weight, small, raw)
	}

	// The stored date/time equal the engine's own casts of the same instant,
	// under whatever session zone applies to both sides.
	var same bool
	if err := db.QueryRowContext(ctx, `SELECT day = CAST($1 AS date) AND at = CAST($1 AS time) AND stamp = CAST($1 AS timestamp) FROM typed WHERE id = 1`, moment).Scan(&same); err != nil {
		t.Fatalf("consistency check: %v", err)
	}
	if !same {
		t.Fatalf("stored date/time/timestamp disagree with CAST of the same parameter")
	}

	// Typed parameters still resolve in predicate position. date = timestamptz
	// promotes the date to midnight, so the day side compares against the
	// parameter's date projection while the timestamptz side matches exactly.
	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM typed WHERE day = CAST($1 AS date) AND zone = $1`, moment).Scan(&count); err != nil {
		t.Fatalf("predicate params: %v", err)
	}
	if count != 1 {
		t.Fatalf("predicate count = %d, want 1", count)
	}

	// PostgreSQL's assignment rule coerces any type into a string-category
	// target through its output function, so bytea lands as its hex text form;
	// a non-string target with no cast (int8 into bool) stays a loud error.
	if _, err := db.ExecContext(ctx, `CREATE TABLE rejections (id bigint PRIMARY KEY, label text, flag bool)`); err != nil {
		t.Fatalf("CREATE TABLE rejections: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO rejections (id, label) VALUES ($1, $2)`, int64(1), []byte{0x61}); err != nil {
		t.Fatalf("bytea into text column: %v", err)
	}
	var label string
	if err := db.QueryRowContext(ctx, `SELECT label FROM rejections WHERE id = 1`).Scan(&label); err != nil || label != `\x61` {
		t.Fatalf("bytea->text label = %q, %v; want `\\x61`", label, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO rejections VALUES ($1, $2, $3)`, int64(2), "x", int64(1)); err == nil {
		t.Fatalf("int64 into bool column succeeded; want a coercion error")
	}
	var empty int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM rejections WHERE flag IS NOT NULL`).Scan(&empty); err != nil || empty != 0 {
		t.Fatalf("rejections flag count = %d, %v; want 0", empty, err)
	}

	// Undeclared strings keep context inference through the input functions.
	if _, err := db.ExecContext(ctx, `INSERT INTO typed (id, day, amount) VALUES ($1, $2, $3)`, int64(2), "2024-03-15", "3.25"); err != nil {
		t.Fatalf("string params: %v", err)
	}
	var inferredDay string
	if err := db.QueryRowContext(ctx, `SELECT day::text FROM typed WHERE id = 2`).Scan(&inferredDay); err != nil || inferredDay != "2024-03-15" {
		t.Fatalf("inferred date = %q, %v; want 2024-03-15", inferredDay, err)
	}
}
