package supersql

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestPLpgSQLProcedureOutParams exercises the embedded profile's required
// procedure parameter surface: OUT results, INOUT read-modify-write, and
// mixed IN/OUT ordering. The anonymous row datum that carries OUT values
// must compile without a resolvable type name.
func TestPLpgSQLProcedureOutParams(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE PROCEDURE p(OUT x int) LANGUAGE plpgsql AS $$ BEGIN x := 42; END $$")
	var x int64
	if err := db.QueryRow("CALL p()").Scan(&x); err != nil {
		t.Fatalf("CALL p(): %v", err)
	}
	if x != 42 {
		t.Fatalf("OUT parameter result=%d, want 42", x)
	}

	exec("CREATE PROCEDURE pio(INOUT x int, IN y int) LANGUAGE plpgsql AS $$ BEGIN x := x + y; END $$")
	if err := db.QueryRow("CALL pio(10, 5)").Scan(&x); err != nil {
		t.Fatalf("CALL pio(10, 5): %v", err)
	}
	if x != 15 {
		t.Fatalf("INOUT parameter result=%d, want 15", x)
	}
}

// TestPLpgSQLMultiOutFunction exercises functions whose result row is the
// anonymous OUT-parameter row datum rather than a single $0 variable.
func TestPLpgSQLMultiOutFunction(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE FUNCTION mf(OUT a int, OUT b text) RETURNS record LANGUAGE plpgsql AS $$ BEGIN a := 7; b := 'hi'; END $$")
	var row string
	if err := db.QueryRow("SELECT mf()").Scan(&row); err != nil {
		t.Fatalf("SELECT mf(): %v", err)
	}
	if row != "(7,hi)" {
		t.Fatalf("multi-OUT result=%q, want (7,hi)", row)
	}
}

// TestPLpgSQLForLoops exercises row-target loops: a single scalar target
// (which the parser wraps in an anonymous row datum), multi-variable row
// targets, and integer range loops. Declared variables with := defaults must
// bind their initializers against the complete datum set.
func TestPLpgSQLForLoops(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE FUNCTION for_scalar() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE a int; total int := 0;
		BEGIN
			FOR a IN SELECT 1 LOOP total := total + a; END LOOP;
			RETURN total;
		END $$`)
	var total int64
	if err := db.QueryRow("SELECT for_scalar()").Scan(&total); err != nil {
		t.Fatalf("for_scalar: %v", err)
	}
	if total != 1 {
		t.Fatalf("for_scalar=%d, want 1", total)
	}

	exec(`CREATE FUNCTION for_multi() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE a int; b int; total int := 0;
		BEGIN
			FOR a, b IN SELECT 1 AS x, 2 AS y UNION ALL SELECT 3, 4 LOOP
				total := total + a * 10 + b;
			END LOOP;
			RETURN total;
		END $$`)
	if err := db.QueryRow("SELECT for_multi()").Scan(&total); err != nil {
		t.Fatalf("for_multi: %v", err)
	}
	if total != 46 {
		t.Fatalf("for_multi=%d, want 46", total)
	}

	exec(`CREATE FUNCTION for_int() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE total int := 0;
		BEGIN
			FOR i IN 1..3 LOOP total := total + i; END LOOP;
			RETURN total;
		END $$`)
	if err := db.QueryRow("SELECT for_int()").Scan(&total); err != nil {
		t.Fatalf("for_int: %v", err)
	}
	if total != 6 {
		t.Fatalf("for_int=%d, want 6", total)
	}
}

// TestPLpgSQLRecordFields exercises record variables whose fields resolve
// dynamically: a single undeclared FOR target is implicitly a record, query
// column names become the record's fields, and field datums carry no static
// layout, so statements that never reference them must still bind and execute.
func TestPLpgSQLRecordFields(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE FUNCTION for_record() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE x int := 0;
		BEGIN
			SELECT 5 INTO x;
			FOR rec IN SELECT 3 AS a, 4 AS b LOOP
				RETURN rec.a * 10 + rec.b + x;
			END LOOP;
			RETURN 0;
		END $$`)
	var total int64
	if err := db.QueryRow("SELECT for_record()").Scan(&total); err != nil {
		t.Fatalf("for_record: %v", err)
	}
	if total != 39 {
		t.Fatalf("for_record=%d, want 39", total)
	}

	exec(`CREATE FUNCTION into_record() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE rec record;
		BEGIN
			SELECT 3 AS a, 4 AS b INTO rec;
			RETURN rec.a * 10 + rec.b;
		END $$`)
	if err := db.QueryRow("SELECT into_record()").Scan(&total); err != nil {
		t.Fatalf("into_record: %v", err)
	}
	if total != 34 {
		t.Fatalf("into_record=%d, want 34", total)
	}

	exec(`CREATE FUNCTION for_record_bare() RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			FOR rec IN SELECT 3 AS a LOOP
				RETURN rec.a;
			END LOOP;
			RETURN 0;
		END $$`)
	if err := db.QueryRow("SELECT for_record_bare()").Scan(&total); err != nil {
		t.Fatalf("for_record_bare: %v", err)
	}
	if total != 3 {
		t.Fatalf("for_record_bare=%d, want 3", total)
	}

	exec(`CREATE FUNCTION for_record_dynamic() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE total int := 0;
		BEGIN
			FOR rec IN EXECUTE 'SELECT 2 AS a UNION ALL SELECT 5' LOOP
				total := total + rec.a;
			END LOOP;
			RETURN total;
		END $$`)
	if err := db.QueryRow("SELECT for_record_dynamic()").Scan(&total); err != nil {
		t.Fatalf("for_record_dynamic: %v", err)
	}
	if total != 7 {
		t.Fatalf("for_record_dynamic=%d, want 7", total)
	}

	exec(`CREATE FUNCTION for_record_iterations() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE total int := 0;
		BEGIN
			FOR rec IN SELECT 1 AS a UNION ALL SELECT 2 LOOP
				total := total + rec.a;
			END LOOP;
			RETURN total;
		END $$`)
	if err := db.QueryRow("SELECT for_record_iterations()").Scan(&total); err != nil {
		t.Fatalf("for_record_iterations: %v", err)
	}
	if total != 3 {
		t.Fatalf("for_record_iterations=%d, want 3", total)
	}

	exec(`CREATE FUNCTION record_missing_field() RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			FOR rec IN SELECT 1 AS a LOOP
				RETURN rec.missing;
			END LOOP;
			RETURN 0;
		END $$`)
	var missing string
	err := db.QueryRow("SELECT record_missing_field()").Scan(&missing)
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != "42703" {
		t.Fatalf("record_missing_field: %v", err)
	}
}

// TestPLpgSQLIntoTargets exercises multi-variable SELECT INTO, where the
// parser's anonymous row datum must distribute the composite into the named
// scalars rather than discarding it.
func TestPLpgSQLIntoTargets(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE FUNCTION into_multi() RETURNS int LANGUAGE plpgsql AS $$
		DECLARE a int; b int;
		BEGIN
			SELECT 3, 4 INTO a, b;
			RETURN a * 10 + b;
		END $$`)
	var v int64
	if err := db.QueryRow("SELECT into_multi()").Scan(&v); err != nil {
		t.Fatalf("into_multi: %v", err)
	}
	if v != 34 {
		t.Fatalf("into_multi=%d, want 34", v)
	}
}

// TestDomainCheckScaling guards the domain-CHECK compile path: every schema
// DDL statement revalidates declared types, so the per-statement cost must
// stay bounded as the domain count grows rather than scaling with the
// catalog namespace size.
func TestDomainCheckScaling(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	db, _ := openTestDB(t)
	const domains = 24
	var worst time.Duration
	for i := 0; i < domains; i++ {
		start := time.Now()
		if _, err := db.Exec("CREATE DOMAIN d" + strconv.Itoa(i) + " AS int CHECK (VALUE > " + strconv.Itoa(i) + " AND VALUE < " + strconv.Itoa(i+1000) + ")"); err != nil {
			t.Fatalf("domain %d: %v", i, err)
		}
		if elapsed := time.Since(start); elapsed > worst {
			worst = elapsed
		}
	}
	if worst > 10*time.Second {
		t.Fatalf("single CREATE DOMAIN took %v; catalog resolution is not bounded", worst)
	}
	if _, err := db.Exec("CREATE FUNCTION f(x int) RETURNS int LANGUAGE sql AS $$ SELECT x $$"); err != nil {
		t.Fatalf("routine DDL with %d domains: %v", domains, err)
	}
}
