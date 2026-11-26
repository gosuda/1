package supersql

import (
	"database/sql"
	"strconv"
	"testing"
)

// These tests pin the differential-gate mismatches found against PostgreSQL
// 18.4 (pg184_differential_test.go) at the driver surface, so each regression
// fails without a live reference server.

// TestDriverProjectsWideOuterJoinKeyExpression covers the LEFT JOIN whose join
// key is an expression over a wide relation. The join materializes its outer
// side through the shared row spool, whose metadata budget must admit every
// column of a relation the catalog accepts.
func TestDriverProjectsWideOuterJoinKeyExpression(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	exec := func(query string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`CREATE TABLE wide_t (id int PRIMARY KEY, v int, t text, vc varchar(12), f float8, b bool, n numeric(12,2), d date, ts timestamptz, j jsonb, r int4range, a int4[])`)
	// Row 11 matches the outer side's row 1 through the computed key b.id + 10;
	// rows 1 and 2 stay unmatched, which is what forces the outer replay spool.
	exec(`INSERT INTO wide_t (id, v) VALUES (1, 10), (2, 20), (11, 110)`)
	rows, err := db.QueryContext(ctx, `SELECT a.id, b.v FROM wide_t a LEFT JOIN wide_t b ON a.id = b.id + 10 ORDER BY a.id`)
	if err != nil {
		t.Fatalf("left join: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, value sql.NullInt64
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if value.Valid {
			got = append(got, strconv.FormatInt(value.Int64, 10))
		} else {
			got = append(got, "NULL")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 3 || got[0] != "NULL" || got[1] != "NULL" || got[2] != "10" {
		t.Fatalf("outer join values = %v, want [NULL NULL 10]", got)
	}
}

// TestDriverWritesWideRows covers inserts and conflict updates on a relation
// wide enough that one row's normalization workspace dominates the write path's
// record budget.
func TestDriverWritesWideRows(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	exec := func(query string) (int64, error) {
		t.Helper()
		result, err := db.ExecContext(ctx, query)
		if err != nil {
			return 0, err
		}
		return result.RowsAffected()
	}
	if _, err := exec(`CREATE TABLE wide_t (id int PRIMARY KEY, v int, t text, vc varchar(12), f float8, b bool, n numeric(12,2), d date, ts timestamptz, j jsonb, r int4range, a int4[])`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := exec(`ALTER TABLE wide_t ADD COLUMN extra int DEFAULT 7`); err != nil {
		t.Fatalf("add column: %v", err)
	}
	affected, err := exec(`INSERT INTO wide_t (id) VALUES (1)`)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if affected != 1 {
		t.Fatalf("insert affected = %d, want 1", affected)
	}
	if _, err := exec(`INSERT INTO wide_t (id) VALUES (5) ON CONFLICT (id) DO UPDATE SET v = excluded.v RETURNING id, v`); err != nil {
		t.Fatalf("conflict insert: %v", err)
	}
	affected, err = exec(`INSERT INTO wide_t (id) VALUES (5) ON CONFLICT (id) DO UPDATE SET v = 50`)
	if err != nil {
		t.Fatalf("conflict update: %v", err)
	}
	if affected != 1 {
		t.Fatalf("conflict update affected = %d, want 1", affected)
	}
	var value sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT v FROM wide_t WHERE id = 5`).Scan(&value); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !value.Valid || value.Int64 != 50 {
		t.Fatalf("conflict update left v = %v, want 50", value)
	}
}

// TestDriverReadsDeclaredTypeColumns covers enum and domain columns, whose
// values render through the declared type's output metadata: the driver must
// resolve it against the statement's own snapshot, not the session defaults.
func TestDriverReadsDeclaredTypeColumns(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	for _, statement := range []string{
		`CREATE TYPE mood AS ENUM ('sad', 'ok', 'happy')`,
		`CREATE TABLE enum_t (id int PRIMARY KEY, m mood)`,
		`INSERT INTO enum_t VALUES (1, 'ok'), (2, 'happy')`,
		`CREATE DOMAIN posint AS int CHECK (VALUE > 0)`,
		`CREATE TABLE dom_t (id int PRIMARY KEY, p posint)`,
		`INSERT INTO dom_t VALUES (1, 5)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	enumRows, err := db.QueryContext(ctx, `SELECT id, m FROM enum_t ORDER BY id`)
	if err != nil {
		t.Fatalf("enum select: %v", err)
	}
	defer enumRows.Close()
	var labels []string
	for enumRows.Next() {
		var id int64
		var label string
		if err := enumRows.Scan(&id, &label); err != nil {
			t.Fatalf("enum scan: %v", err)
		}
		labels = append(labels, label)
	}
	if err := enumRows.Err(); err != nil {
		t.Fatalf("enum iterate: %v", err)
	}
	if len(labels) != 2 || labels[0] != "ok" || labels[1] != "happy" {
		t.Fatalf("enum labels = %v, want [ok happy]", labels)
	}
	var id, value int64
	if err := db.QueryRowContext(ctx, `SELECT id, p FROM dom_t ORDER BY id`).Scan(&id, &value); err != nil {
		t.Fatalf("domain select: %v", err)
	}
	if id != 1 || value != 5 {
		t.Fatalf("domain row = (%d, %d), want (1, 5)", id, value)
	}
}

// TestDriverCatalogTypePresentation covers the type-identity functions and the
// information_schema presentation of a modified type: numeric typmods decode
// their signed 11-bit scale, an unadorned numeric literal keeps its inferred
// type, and a domain column relabels to its base without narrowing the base
// modifier.
func TestDriverCatalogTypePresentation(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	if _, err := db.ExecContext(ctx, `CREATE TABLE typ_t (id int PRIMARY KEY, n numeric(6,2))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	var formatted, typeof, precision, scale, nullable string
	if err := db.QueryRowContext(ctx, `SELECT format_type(23, -1)::text, format_type(1043, 14)::text, format_type(1700, 425990)::text`).
		Scan(new(string), new(string), &formatted); err != nil {
		t.Fatalf("format_type: %v", err)
	}
	if formatted != "numeric(6,2)" {
		t.Fatalf("format_type(1700, 425990) = %q, want numeric(6,2)", formatted)
	}
	if err := db.QueryRowContext(ctx, `SELECT pg_typeof(1.5)::text`).Scan(&typeof); err != nil {
		t.Fatalf("pg_typeof: %v", err)
	}
	if typeof != "numeric" {
		t.Fatalf("pg_typeof(1.5) = %q, want numeric", typeof)
	}
	if err := db.QueryRowContext(ctx, `SELECT numeric_precision::text, numeric_scale::text FROM information_schema.columns WHERE table_name = 'typ_t' AND column_name = 'n'`).
		Scan(&precision, &scale); err != nil {
		t.Fatalf("numeric presentation: %v", err)
	}
	if precision != "6" || scale != "2" {
		t.Fatalf("numeric(6,2) presentation = (%s, %s), want (6, 2)", precision, scale)
	}
	if err := db.QueryRowContext(ctx, `SELECT is_nullable::text FROM information_schema.columns WHERE table_name = 'typ_t' ORDER BY ordinal_position LIMIT 1`).Scan(&nullable); err != nil {
		t.Fatalf("domain column cast: %v", err)
	}
	if nullable != "NO" {
		t.Fatalf("is_nullable::text = %q, want NO", nullable)
	}
}
