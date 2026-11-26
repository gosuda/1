package supersql

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
)

func TestPhase5PLpgSQLMultiSRFAndScalarNoDeadlock(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	mustExec(`CREATE FUNCTION srf() RETURNS SETOF int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN NEXT 1;
			RETURN NEXT 2;
			RETURN NEXT 3;
		END $$`)
	mustExec(`CREATE FUNCTION srf2() RETURNS SETOF int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN NEXT 10;
			RETURN NEXT 20;
		END $$`)
	mustExec(`CREATE FUNCTION add_one(x int) RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN x + 1;
		END $$`)
	mustExec(`CREATE FUNCTION srf_nested() RETURNS SETOF int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN QUERY SELECT add_one(x) FROM srf() AS x;
		END $$`)

	type intPair struct {
		a      int64
		aValid bool
		b      int64
		bValid bool
	}
	queryPairs := func(q string) []intPair {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var got []intPair
		for rows.Next() {
			var a, b *int64
			if err := rows.Scan(&a, &b); err != nil {
				t.Fatalf("%s scan: %v", q, err)
			}
			var p intPair
			if a != nil {
				p.a, p.aValid = *a, true
			}
			if b != nil {
				p.b, p.bValid = *b, true
			}
			got = append(got, p)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s rows err: %v", q, err)
		}
		return got
	}
	queryInts := func(q string) []int64 {
		t.Helper()
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var got []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("%s scan: %v", q, err)
			}
			got = append(got, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s rows err: %v", q, err)
		}
		return got
	}

	// 1. Multiple SRFs in target list (projectset).
	wantSame := []intPair{
		{a: 1, aValid: true, b: 1, bValid: true},
		{a: 2, aValid: true, b: 2, bValid: true},
		{a: 3, aValid: true, b: 3, bValid: true},
	}
	if got := queryPairs("SELECT srf(), srf()"); !reflect.DeepEqual(got, wantSame) {
		t.Fatalf("SELECT srf(), srf() = %+v, want %+v", got, wantSame)
	}

	wantMismatch := []intPair{
		{a: 1, aValid: true, b: 10, bValid: true},
		{a: 2, aValid: true, b: 20, bValid: true},
		{a: 3, aValid: true, bValid: false},
	}
	if got := queryPairs("SELECT srf(), srf2()"); !reflect.DeepEqual(got, wantMismatch) {
		t.Fatalf("SELECT srf(), srf2() = %+v, want %+v", got, wantMismatch)
	}

	// 2. ROWS FROM with multiple PL/pgSQL SRFs.
	if got := queryPairs("SELECT * FROM ROWS FROM (srf(), srf())"); !reflect.DeepEqual(got, wantSame) {
		t.Fatalf("ROWS FROM (srf(), srf()) = %+v, want %+v", got, wantSame)
	}
	if got := queryPairs("SELECT * FROM ROWS FROM (srf(), srf2())"); !reflect.DeepEqual(got, wantMismatch) {
		t.Fatalf("ROWS FROM (srf(), srf2()) = %+v, want %+v", got, wantMismatch)
	}

	// 3. CROSS JOIN of two PL/pgSQL SRFs.
	wantCross := []intPair{
		{a: 1, aValid: true, b: 1, bValid: true},
		{a: 1, aValid: true, b: 2, bValid: true},
		{a: 1, aValid: true, b: 3, bValid: true},
		{a: 2, aValid: true, b: 1, bValid: true},
		{a: 2, aValid: true, b: 2, bValid: true},
		{a: 2, aValid: true, b: 3, bValid: true},
		{a: 3, aValid: true, b: 1, bValid: true},
		{a: 3, aValid: true, b: 2, bValid: true},
		{a: 3, aValid: true, b: 3, bValid: true},
	}
	if got := queryPairs("SELECT a, b FROM srf() AS a CROSS JOIN srf() AS b ORDER BY a, b"); !reflect.DeepEqual(got, wantCross) {
		t.Fatalf("srf() CROSS JOIN srf() = %+v, want %+v", got, wantCross)
	}

	// 4. Scalar PL/pgSQL function evaluated over PL/pgSQL SRF output.
	wantAdded := []int64{2, 3, 4}
	if got := queryInts("SELECT add_one(x) FROM srf() AS x ORDER BY 1"); !reflect.DeepEqual(got, wantAdded) {
		t.Fatalf("SELECT add_one(x) FROM srf() AS x = %+v, want %+v", got, wantAdded)
	}

	// 5. Nested SRF invoking another SRF and scalar PL/pgSQL function.
	if got := queryInts("SELECT * FROM srf_nested() ORDER BY 1"); !reflect.DeepEqual(got, wantAdded) {
		t.Fatalf("SELECT * FROM srf_nested() = %+v, want %+v", got, wantAdded)
	}
}

func TestPhase5PLpgSQLSharedCallTreeStepBudget(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Define stub first so self-recursive body compilation resolves fib_dos.
	// Branching recursion at depth 60 stays below MaxDepth (64) but would
	// execute 2^60 frames if step budgets reset per frame.
	mustExec(`CREATE FUNCTION fib_dos(n int) RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN 0;
		END $$`)
	mustExec(`CREATE OR REPLACE FUNCTION fib_dos(n int) RETURNS int LANGUAGE plpgsql AS $$
		DECLARE i int;
		BEGIN
			FOR i IN 1..2000 LOOP
			END LOOP;
			IF n <= 1 THEN
				RETURN n;
			END IF;
			RETURN fib_dos(n - 1) + fib_dos(n - 2);
		END $$`)

	// Small input succeeds normally.
	var fib6 int64
	if err := db.QueryRowContext(ctx, "SELECT fib_dos(6)").Scan(&fib6); err != nil {
		t.Fatalf("SELECT fib_dos(6): %v", err)
	}
	if fib6 != 8 {
		t.Fatalf("fib_dos(6) = %d, want 8", fib6)
	}

	// Branching recursion at n = 60 must hit the cumulative call-tree step limit promptly.
	start := time.Now()
	var got int64
	err := db.QueryRowContext(ctx, "SELECT fib_dos(60)").Scan(&got)
	elapsed := time.Since(start)
	if !errors.Is(err, procedural.ErrLimit) {
		t.Fatalf("SELECT fib_dos(60) err = %v (got=%d, elapsed=%v), want procedural.ErrLimit", err, got, elapsed)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("SELECT fib_dos(60) took %v to hit step budget, want prompt termination", elapsed)
	}

	// WHEN OTHERS must not be able to swallow step-budget exhaustion.
	mustExec(`CREATE FUNCTION fib_dos_swallow() RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				RETURN fib_dos(60);
			EXCEPTION WHEN OTHERS THEN
				RETURN -1;
			END;
		END $$`)
	err = db.QueryRowContext(ctx, "SELECT fib_dos_swallow()").Scan(&got)
	if !errors.Is(err, procedural.ErrLimit) {
		t.Fatalf("SELECT fib_dos_swallow() err = %v (got=%d), want procedural.ErrLimit", err, got)
	}

	// Cumulative MaxResultBytes across recursive frames: each frame reads ~2 MiB
	// (< 16 MiB per-frame cap), so 10 recursive frames exceed 16 MiB cumulatively.
	mustExec(`CREATE FUNCTION bytes_dos(depth int) RETURNS int LANGUAGE plpgsql AS $$
		BEGIN
			RETURN 0;
		END $$`)
	mustExec(`CREATE OR REPLACE FUNCTION bytes_dos(depth int) RETURNS int LANGUAGE plpgsql AS $$
		DECLARE
			s text;
			i int;
		BEGIN
			FOR i IN 1..8 LOOP
				SELECT repeat('a', 250000) INTO s;
			END LOOP;
			IF depth <= 1 THEN
				RETURN length(s);
			END IF;
			RETURN bytes_dos(depth - 1);
		END $$`)
	if err := db.QueryRowContext(ctx, "SELECT bytes_dos(2)").Scan(&got); err != nil {
		t.Fatalf("SELECT bytes_dos(2): %v", err)
	}
	if got != 250000 {
		t.Fatalf("bytes_dos(2) = %d, want 250000", got)
	}
	err = db.QueryRowContext(ctx, "SELECT bytes_dos(10)").Scan(&got)
	if !errors.Is(err, procedural.ErrLimit) {
		t.Fatalf("SELECT bytes_dos(10) err = %v (got=%d), want procedural.ErrLimit", err, got)
	}
}

func TestPhase5PLpgSQLWhenOthersExclusions(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// 1. WHEN OTHERS must NOT catch ASSERT failure (P0004).
	mustExec(`CREATE FUNCTION catch_assert_others() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				ASSERT false, 'boom assert';
			EXCEPTION WHEN OTHERS THEN
				RETURN 'caught';
			END;
			RETURN 'ok';
		END $$`)
	var out string
	err := db.QueryRowContext(ctx, "SELECT catch_assert_others()").Scan(&out)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "P0004" {
		t.Fatalf("catch_assert_others: err=%v (out=%q), want SQLSTATE P0004", err, out)
	}

	// Explicit WHEN assert_failure still catches P0004 per PostgreSQL rules.
	mustExec(`CREATE FUNCTION catch_assert_explicit() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				ASSERT false, 'boom assert';
			EXCEPTION WHEN assert_failure THEN
				RETURN SQLSTATE;
			END;
			RETURN 'ok';
		END $$`)
	if err := db.QueryRowContext(ctx, "SELECT catch_assert_explicit()").Scan(&out); err != nil {
		t.Fatalf("catch_assert_explicit: %v", err)
	}
	if out != "P0004" {
		t.Fatalf("catch_assert_explicit = %q, want P0004", out)
	}

	// 2. WHEN OTHERS must NOT catch query_canceled (57014).
	mustExec(`CREATE FUNCTION catch_cancel_code_others() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				RAISE EXCEPTION 'canceled' USING ERRCODE = '57014';
			EXCEPTION WHEN OTHERS THEN
				RETURN 'caught';
			END;
			RETURN 'ok';
		END $$`)
	err = db.QueryRowContext(ctx, "SELECT catch_cancel_code_others()").Scan(&out)
	sqlErr = nil
	if !errors.As(err, &sqlErr) || sqlErr.Code != "57014" {
		t.Fatalf("catch_cancel_code_others: err=%v (out=%q), want SQLSTATE 57014", err, out)
	}

	// 3. WHEN OTHERS must NOT catch actual query context cancellation.
	mustExec(`CREATE FUNCTION spin_catch_others() RETURNS text LANGUAGE plpgsql AS $$
		DECLARE i int := 0;
		BEGIN
			BEGIN
				LOOP
					i := i + 1;
				END LOOP;
			EXCEPTION WHEN OTHERS THEN
				RETURN 'caught';
			END;
			RETURN 'ok';
		END $$`)
	cancelCtx, cancelSpin := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancelSpin()
	err = db.QueryRowContext(cancelCtx, "SELECT spin_catch_others()").Scan(&out)
	if err == nil {
		t.Fatalf("spin_catch_others succeeded with %q, want cancellation error", out)
	}
	sqlErr = nil
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && (!errors.As(err, &sqlErr) || sqlErr.Code != "57014") {
		t.Fatalf("spin_catch_others err = %v, want context cancellation or 57014", err)
	}

	// 4. WHEN OTHERS still catches ordinary errors like RAISE EXCEPTION (P0001).
	mustExec(`CREATE FUNCTION catch_raise_others() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				RAISE EXCEPTION 'ordinary error';
			EXCEPTION WHEN OTHERS THEN
				RETURN SQLSTATE;
			END;
		END $$`)
	if err := db.QueryRowContext(ctx, "SELECT catch_raise_others()").Scan(&out); err != nil {
		t.Fatalf("catch_raise_others: %v", err)
	}
	if out != "P0001" {
		t.Fatalf("catch_raise_others = %q, want P0001", out)
	}
}

func TestPhase5PLpgSQLStandardConditionNames(t *testing.T) {
	db, _ := openTestDB(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	mustExec := func(q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// 1. numeric_value_out_of_range (22003) catches 22003.
	mustExec(`CREATE FUNCTION catch_overflow() RETURNS text LANGUAGE plpgsql AS $$
		DECLARE s smallint;
		BEGIN
			BEGIN
				SELECT 40000::smallint INTO s;
			EXCEPTION WHEN numeric_value_out_of_range THEN
				RETURN 'caught:' || SQLSTATE;
			END;
			RETURN 'ok';
		END $$`)
	var out string
	if err := db.QueryRowContext(ctx, "SELECT catch_overflow()").Scan(&out); err != nil {
		t.Fatalf("catch_overflow: %v", err)
	}
	if out != "caught:22003" {
		t.Fatalf("catch_overflow = %q, want caught:22003", out)
	}

	// 2. numeric_value_out_of_range MUST NOT falsely catch RAISE EXCEPTION (P0001).
	mustExec(`CREATE FUNCTION overflow_does_not_catch_raise() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				RAISE EXCEPTION 'plain raise';
			EXCEPTION WHEN numeric_value_out_of_range THEN
				RETURN 'wrongly_caught';
			END;
			RETURN 'ok';
		END $$`)
	err := db.QueryRowContext(ctx, "SELECT overflow_does_not_catch_raise()").Scan(&out)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "P0001" {
		t.Fatalf("overflow_does_not_catch_raise: err=%v (out=%q), want SQLSTATE P0001", err, out)
	}

	// 3. undefined_table (42P01) catches missing table and does NOT catch P0001.
	mustExec(`CREATE FUNCTION catch_undef_table(should_raise boolean) RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				IF should_raise THEN
					RAISE EXCEPTION 'not a table error';
				END IF;
				EXECUTE 'SELECT * FROM no_such_table_phase5';
			EXCEPTION WHEN undefined_table THEN
				RETURN 'caught:' || SQLSTATE;
			END;
			RETURN 'ok';
		END $$`)
	if err := db.QueryRowContext(ctx, "SELECT catch_undef_table(false)").Scan(&out); err != nil {
		t.Fatalf("catch_undef_table(false): %v", err)
	}
	if out != "caught:42P01" {
		t.Fatalf("catch_undef_table(false) = %q, want caught:42P01", out)
	}
	err = db.QueryRowContext(ctx, "SELECT catch_undef_table(true)").Scan(&out)
	sqlErr = nil
	if !errors.As(err, &sqlErr) || sqlErr.Code != "P0001" {
		t.Fatalf("catch_undef_table(true): err=%v (out=%q), want SQLSTATE P0001", err, out)
	}

	// 4. RAISE USING ERRCODE with standard PG condition name + category matching (data_exception = 22000).
	mustExec(`CREATE FUNCTION raise_named_condition() RETURNS text LANGUAGE plpgsql AS $$
		BEGIN
			BEGIN
				RAISE EXCEPTION 'range overflow' USING ERRCODE = 'numeric_value_out_of_range';
			EXCEPTION WHEN data_exception THEN
				RETURN 'class22:' || SQLSTATE;
			END;
		END $$`)
	if err := db.QueryRowContext(ctx, "SELECT raise_named_condition()").Scan(&out); err != nil {
		t.Fatalf("raise_named_condition: %v", err)
	}
	if out != "class22:22003" {
		t.Fatalf("raise_named_condition = %q, want class22:22003", out)
	}
}
