package supersql

import (
	"database/sql"
	"fmt"
	"testing"
)

// TestTypeAlterAdmitsCatalogSizedNamespace is the failing-before regression for
// "type dependency namespace limit exceeded": the cascade walk charged every
// namespace row against a fixed 64-descriptor allowance, so ALTER TYPE ADD VALUE
// failed once a schema held more than 64 objects. 150 enums must still alter.
//
// It opens its own connector so that it can assert the close error of a session
// this long directly.
func TestTypeAlterAdmitsCatalogSizedNamespace(t *testing.T) {
	ctx := t.Context()
	connector, err := OpenConnector(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	pool := sql.OpenDB(connector)
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		if err := pool.Close(); err != nil {
			t.Errorf("pool close after long session: %v", err)
		}
	})
	for i := range 150 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TYPE e%d AS ENUM ('a')", i)); err != nil {
			t.Fatalf("create e%d: %v", i, err)
		}
	}
	if _, err := conn.ExecContext(ctx, "ALTER TYPE e0 ADD VALUE 'z'"); err != nil {
		t.Fatalf("ALTER TYPE e0 ADD VALUE: %v", err)
	}
	var got string
	if err := conn.QueryRowContext(ctx, "SELECT 'z'::e0::text").Scan(&got); err != nil {
		t.Fatalf("SELECT new value: %v", err)
	}
	if got != "z" {
		t.Fatalf("new value = %q, want z", got)
	}
}

// TestDropTypeEveryNameFormEndToEnd runs each DROP TYPE name form the bug report
// lists, plus RESTRICT with a dependent column, against the real engine.
func TestDropTypeEveryNameFormEndToEnd(t *testing.T) {
	ctx := t.Context()
	_, _, conn := declaredTypeTestSession(t)
	mustExec := func(statement string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%q: %v", statement, err)
		}
	}
	for _, statement := range []string{
		"CREATE TYPE d1 AS ENUM ('a')",
		"CREATE TYPE public_d2 AS ENUM ('a')",
		"CREATE TYPE d3 AS ENUM ('a')",
		"CREATE TYPE d4 AS ENUM ('a')",
	} {
		mustExec(statement)
	}
	mustExec("DROP TYPE d1")
	mustExec("DROP TYPE public.public_d2")
	mustExec("DROP TYPE IF EXISTS d3")
	mustExec("DROP TYPE d4 CASCADE")
	mustExec("DROP TYPE IF EXISTS d1")
}

// TestDropTypeRestrictRejectsDependentColumn checks that RESTRICT keeps a type a
// column still uses and fails 2BP01.
func TestDropTypeRestrictRejectsDependentColumn(t *testing.T) {
	ctx := t.Context()
	_, _, conn := declaredTypeTestSession(t)
	if _, err := conn.ExecContext(ctx, "CREATE TYPE held AS ENUM ('a')"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "CREATE TABLE holder (c held)"); err != nil {
		t.Fatal(err)
	}
	_, err := conn.ExecContext(ctx, "DROP TYPE held")
	if err == nil {
		t.Fatal("DROP TYPE held succeeded while a column depends on it")
	}
	if got := sqlStateOf(err); got != "2BP01" {
		t.Fatalf("DROP TYPE held: SQLSTATE %s (%v), want 2BP01", got, err)
	}
}
