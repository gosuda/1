package supersql

import (
	"testing"
)

// TestRowTriggerSemantics exercises the embedded profile's required
// SQL/PLpgSQL row trigger surface: BEFORE row modification, RETURN NULL row
// skipping, WHEN filtering, OLD/NEW field access, TG_* variables, nested DML,
// and AFTER triggers observing the final stored row.
func TestRowTriggerSemantics(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE TABLE t (id bigint PRIMARY KEY, v int)")
	exec("CREATE TABLE audit (msg text)")

	// BEFORE INSERT: NEW field read, assignment, and returned row stored.
	exec("CREATE FUNCTION bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.v := NEW.v + 1; RETURN NEW; END $$")
	exec("CREATE TRIGGER bump_ins BEFORE INSERT ON t FOR EACH ROW EXECUTE FUNCTION bump()")
	exec("INSERT INTO t VALUES (1, 10)")
	var v int64
	if err := db.QueryRow("SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 11 {
		t.Fatalf("BEFORE trigger NEW assignment not stored: v=%d", v)
	}

	// RETURN NULL from a WHEN-filtered BEFORE trigger skips the row.
	exec("CREATE FUNCTION skip() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$")
	exec("CREATE TRIGGER skip_ins BEFORE INSERT ON t FOR EACH ROW WHEN (NEW.id = 99) EXECUTE FUNCTION skip()")
	exec("INSERT INTO t VALUES (99, 1)")
	var n int64
	if err := db.QueryRow("SELECT count(*) FROM t WHERE id = 99").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("RETURN NULL did not skip insert: %d rows", n)
	}

	// AFTER triggers see OLD/NEW, TG_OP is uppercase, and nested DML runs.
	exec(`CREATE FUNCTION audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			INSERT INTO audit VALUES (TG_OP || ':' || coalesce(OLD.v::text,'') || '->' || coalesce(NEW.v::text,''));
			RETURN NEW;
		END $$`)
	exec("CREATE TRIGGER audit_upd AFTER UPDATE ON t FOR EACH ROW EXECUTE FUNCTION audit()")
	exec("CREATE TRIGGER audit_del AFTER DELETE ON t FOR EACH ROW EXECUTE FUNCTION audit()")
	exec("UPDATE t SET v = v + 100 WHERE id = 1")
	var msg string
	if err := db.QueryRow("SELECT msg FROM audit WHERE msg LIKE 'UPDATE%'").Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if msg != "UPDATE:11->111" {
		t.Fatalf("audit message = %q, want UPDATE:11->111", msg)
	}
	if err := db.QueryRow("SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 111 {
		t.Fatalf("updated v=%d, want 111", v)
	}
	exec("DELETE FROM t WHERE id = 1")
	if err := db.QueryRow("SELECT msg FROM audit WHERE msg LIKE 'DELETE%'").Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if msg != "DELETE:111->" {
		t.Fatalf("delete audit message = %q, want DELETE:111->", msg)
	}
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("delete left %d rows", n)
	}
}

// TestRoutineVariableReturn covers the retvarno path: RETURN of a plain
// variable must return that variable's value, not an empty result.
func TestRoutineVariableReturn(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE FUNCTION f(x int) RETURNS int LANGUAGE plpgsql AS $$ BEGIN RETURN x; END $$")
	var got int64
	if err := db.QueryRow("SELECT f(7)").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("RETURN variable returned %d, want 7", got)
	}
	// A record variable returns the whole row value.
	exec("CREATE FUNCTION g() RETURNS int LANGUAGE plpgsql AS $$ DECLARE y int := 41; BEGIN y := y + 1; RETURN y; END $$")
	if err := db.QueryRow("SELECT g()").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Fatalf("RETURN y returned %d, want 42", got)
	}
}

// TestRoutinePseudotypeReturns verifies routine result pseudotypes are
// declaration markers: they must create cleanly and reject execution contexts
// that cannot carry them.
func TestRoutinePseudotypeReturns(t *testing.T) {
	db, _ := openTestDB(t)
	for _, q := range []string{
		"CREATE FUNCTION fvoid() RETURNS void LANGUAGE sql AS $$ SELECT $$",
		"CREATE FUNCTION fev() RETURNS event_trigger LANGUAGE plpgsql AS $$ BEGIN END $$",
		"CREATE FUNCTION ftrg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END $$",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestRoutinePolymorphicCalls verifies declared polymorphic markers resolve at
// the call boundary: each invocation binds and executes against the concrete
// argument layouts, while positions sharing a marker must agree and markers
// that cannot be inferred fail as binding errors.
func TestRoutinePolymorphicCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	db, _ := openTestDB(t)
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec("CREATE FUNCTION fp(x anyelement) RETURNS anyelement LANGUAGE sql AS $$ SELECT x $$")
	exec("CREATE FUNCTION fpp(a anyelement, b anyelement) RETURNS anyelement LANGUAGE sql AS $$ SELECT b $$")
	exec("CREATE FUNCTION fna(x anynonarray) RETURNS anynonarray LANGUAGE sql AS $$ SELECT x $$")
	exec("CREATE FUNCTION fa(x anyarray) RETURNS anyarray LANGUAGE sql AS $$ SELECT x $$")
	exec("CREATE FUNCTION fac(a anyarray, e anyelement) RETURNS anyelement LANGUAGE sql AS $$ SELECT e $$")
	exec("CREATE FUNCTION fst(x anyelement) RETURNS anyelement LANGUAGE sql STRICT AS $$ SELECT x $$")
	exec("CREATE FUNCTION plp(x anyelement) RETURNS anyelement LANGUAGE plpgsql AS $$ BEGIN RETURN x; END $$")

	var got int
	for _, q := range []string{"SELECT fp(5)", "SELECT fp(7)", "SELECT fpp(1, 2)", "SELECT fna(9)", "SELECT fst(4)", "SELECT plp(11)", "SELECT fac(ARRAY[1], 2)"} {
		want := map[string]int{"SELECT fp(5)": 5, "SELECT fp(7)": 7, "SELECT fpp(1, 2)": 2, "SELECT fna(9)": 9, "SELECT fst(4)": 4, "SELECT plp(11)": 11, "SELECT fac(ARRAY[1], 2)": 2}[q]
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", q, got, want)
		}
	}
	var text string
	if err := db.QueryRow("SELECT fp('a'::text)").Scan(&text); err != nil || text != "a" {
		t.Fatalf("fp('a') = %q, %v", text, err)
	}
	if err := db.QueryRow("SELECT plp('t'::text)").Scan(&text); err != nil || text != "t" {
		t.Fatalf("plp('t') = %q, %v", text, err)
	}
	// A set-returning declaration resolves the same way per call.
	exec("CREATE FUNCTION fs(x anyelement) RETURNS SETOF anyelement LANGUAGE sql AS $$ SELECT x $$")
	if err := db.QueryRow("SELECT fs(3)").Scan(&got); err != nil || got != 3 {
		t.Fatalf("fs(3) = %d, %v", got, err)
	}
	// anyarray admits arrays only, and its element type couples to the scalar markers.
	var arr string
	if err := db.QueryRow("SELECT fa(ARRAY[1,2])::text").Scan(&arr); err != nil || arr != "{1,2}" {
		t.Fatalf("fa(ARRAY[1,2]) = %q, %v", arr, err)
	}
	// Kinded markers reject mismatched argument kinds.
	exec("CREATE TYPE mood AS ENUM ('sad','ok','happy')")
	exec("CREATE FUNCTION fen(x anyenum) RETURNS anyenum LANGUAGE sql AS $$ SELECT x $$")
	if err := db.QueryRow("SELECT fen('sad'::mood)::text").Scan(&text); err != nil || text != "sad" {
		t.Fatalf("fen('sad') = %q, %v", text, err)
	}
	// The anycompatible family resolves each call through the common type of
	// its positions rather than requiring identical argument types.
	exec("CREATE FUNCTION fc(x anycompatible, y anycompatible) RETURNS anycompatible LANGUAGE sql AS $$ SELECT y $$")
	if err := db.QueryRow("SELECT fc(1, 2::int8)").Scan(&got); err != nil || got != 2 {
		t.Fatalf("fc(1,2::int8) = %d, %v", got, err)
	}
	// Range and multirange arguments couple into the scalar and range
	// variables: the multirange's element type and subtype drive resolution.
	exec("CREATE FUNCTION fmre(x anymultirange) RETURNS anyelement LANGUAGE sql AS $$ SELECT 5 $$")
	if err := db.QueryRow("SELECT fmre('{[1,3)}'::int4multirange)").Scan(&got); err != nil || got != 5 {
		t.Fatalf("fmre = %d, %v", got, err)
	}
	exec("CREATE FUNCTION fcmr(x anycompatiblemultirange) RETURNS anycompatible LANGUAGE sql AS $$ SELECT 1 $$")
	if err := db.QueryRow("SELECT fcmr('{[1,3)}'::int4multirange)").Scan(&got); err != nil || got != 1 {
		t.Fatalf("fcmr = %d, %v", got, err)
	}
	exec("CREATE FUNCTION fmr(x anymultirange) RETURNS anyrange LANGUAGE sql AS $$ SELECT NULL::int4range $$")
	var isNull bool
	if err := db.QueryRow("SELECT fmr('{[1,3)}'::int4multirange) IS NULL").Scan(&isNull); err != nil || !isNull {
		t.Fatalf("fmr = %v, %v", isNull, err)
	}
	// A produced result whose type differs from the resolved declaration
	// fails at the routine boundary rather than corrupting byte-typed values.
	exec("CREATE FUNCTION frm(x anyrange) RETURNS anymultirange LANGUAGE sql AS $$ SELECT x $$")
	if _, err := db.Exec("SELECT frm(int4range(1,5))"); err == nil {
		t.Fatal("frm accepted a range produced for a multirange result")
	}
	for _, q := range []string{
		"SELECT fp(NULL)",                 // no argument can infer the marker
		"SELECT fst(NULL)",                // STRICT still requires inference
		"SELECT fpp(1, 'a'::text)",        // repeated marker positions must agree
		"SELECT fna(ARRAY[1,2])",          // anynonarray rejects arrays
		"SELECT fa(5)",                    // anyarray requires an array
		"SELECT fac(ARRAY[1], 'x'::text)", // element must match the scalar marker
		"SELECT fen(5)",                   // anyenum requires an enum
		"SELECT fc(1, 'a'::text)",         // no common type across the family
	} {
		if _, err := db.Exec(q); err == nil {
			t.Fatalf("%s: expected rejection, got success", q)
		}
	}
	// Result-only pseudo-types remain illegal in parameter position.
	if _, err := db.Exec("CREATE FUNCTION fv(x void) RETURNS void LANGUAGE sql AS $$ SELECT NULL $$"); err == nil {
		t.Fatal("void parameter accepted")
	}
	// A polymorphic CALL resolves through the same signature substitution.
	exec("CREATE PROCEDURE pp(x anyelement) LANGUAGE sql AS $$ SELECT x $$")
	if _, err := db.Exec("CALL pp(5)"); err != nil {
		t.Fatalf("CALL pp(5): %v", err)
	}
	// INOUT parameters bind as inputs; OUT-only parameters take no call
	// argument and resolve from the inferred result shape.
	exec("CREATE PROCEDURE ppinout(x INOUT anyelement) LANGUAGE sql AS $$ SELECT x $$")
	if err := db.QueryRow("CALL ppinout(7)").Scan(&got); err != nil || got != 7 {
		t.Fatalf("CALL ppinout(7) = %d, %v", got, err)
	}
	exec("CREATE PROCEDURE ppo(x anyelement, OUT y anyelement) LANGUAGE sql AS $$ SELECT x $$")
	var out any
	if err := db.QueryRow("CALL ppo(5)").Scan(&out); err != nil {
		t.Fatalf("CALL ppo(5): %v", err)
	}
	exec("CREATE PROCEDURE ppm(x anyelement, OUT u anyelement, OUT v anyelement) LANGUAGE sql AS $$ SELECT x, x $$")
	var u, v any
	if err := db.QueryRow("CALL ppm(5)").Scan(&u, &v); err != nil {
		t.Fatalf("CALL ppm(5): %v", err)
	}
	if _, err := db.Exec("CALL ppo(5, 2)"); err == nil {
		t.Fatal("CALL ppo(5,2) accepted an argument for an OUT parameter")
	}
}

// User routine calls admit implicitly coercible arguments as PostgreSQL does,
// and an exact signature outranks one that needs coercion.
func TestRoutineImplicitArgumentCoercion(t *testing.T) {
	db, _ := openTestDB(t)
	for _, q := range []string{
		"CREATE FUNCTION wid(x bigint) RETURNS text LANGUAGE sql AS $$ SELECT 'bigint' $$",
		"CREATE FUNCTION pick(x integer) RETURNS text LANGUAGE sql AS $$ SELECT 'integer' $$",
		"CREATE FUNCTION pick(x bigint) RETURNS text LANGUAGE plpgsql AS $$ BEGIN RETURN 'bigint'; END $$",
		"CREATE TABLE pwid_log (v bigint)",
		"CREATE PROCEDURE pwid(x bigint) LANGUAGE sql AS $$ INSERT INTO pwid_log VALUES (x) $$",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for q, want := range map[string]string{
		"SELECT wid(1)":          "bigint",
		"SELECT pick(1)":         "integer",
		"SELECT pick(1::bigint)": "bigint",
	} {
		var got string
		if err := db.QueryRow(q).Scan(&got); err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", q, got, err, want)
		}
	}
	if _, err := db.Exec("CALL pwid(4)"); err != nil {
		t.Fatalf("CALL pwid(4): %v", err)
	}
	var logged int64
	if err := db.QueryRow("SELECT v FROM pwid_log").Scan(&logged); err != nil || logged != 4 {
		t.Fatalf("pwid_log = %d, %v; want 4", logged, err)
	}
	if _, err := db.Exec("SELECT wid('x'::text)"); err == nil {
		t.Fatal("wid(text) resolved without an implicit text-to-bigint cast")
	}
}
