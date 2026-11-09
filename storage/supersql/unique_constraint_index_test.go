package supersql

import "testing"

// TestAddUniqueConstraintOnIndexedTable covers ALTER TABLE ... ADD UNIQUE and
// ADD PRIMARY KEY on a table that already carries an index.
func TestAddUniqueConstraintOnIndexedTable(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	for _, statement := range []string{
		`CREATE TABLE tbl_include_unique1 (c1 int, c2 int, c3 int)`,
		`INSERT INTO tbl_include_unique1 SELECT x, 2*x, 3*x FROM generate_series(1,10) AS x`,
		`CREATE INDEX tbl_include_unique1_c3 ON tbl_include_unique1 (c3)`,
		`ALTER TABLE tbl_include_unique1 ADD UNIQUE (c1, c2)`,
		`ALTER TABLE tbl_include_unique1 ADD PRIMARY KEY (c1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var constraints int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'tbl_include_unique1'::regclass AND contype IN ('u', 'p')`).Scan(&constraints); err != nil {
		t.Fatalf("pg_constraint query: %v", err)
	}
	if constraints != 2 {
		t.Fatalf("pg_constraint count = %d, want 2", constraints)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tbl_include_unique1 VALUES (11, 2, 33)`); err != nil {
		t.Fatalf("distinct (c1, c2) insert: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO tbl_include_unique1 VALUES (1, 2, 99)`); SQLState(err) != "23505" {
		t.Fatalf("duplicate (c1, c2) insert err = %v (SQLSTATE %q), want 23505", err, SQLState(err))
	}
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tbl_include_unique1 WHERE c3 = 9`).Scan(&rows); err != nil {
		t.Fatalf("read after ADD UNIQUE: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows with c3 = 9: %d, want 1", rows)
	}
}
