package supersql

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestBatch2CatalogPartitionRLSCopyIssueFixes(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	scope, err := db.newSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	}()

	mustExec := func(t *testing.T, sql string) outcome {
		t.Helper()
		out, err := db.execIn(ctx, scope, sql, 10000)
		if err != nil {
			t.Fatalf("exec %q failed: %v", sql, err)
		}
		return out
	}
	execCopy := func(sql string, streams *qcopy.Streams) (outcome, error) {
		parsed, err := zpgparse.ParseExtended(sql, zpgparse.ParseOptions{})
		if err != nil {
			return outcome{}, err
		}
		if streams != nil && streams.MaxRecordBytes == 0 {
			streams.MaxRecordBytes = 1 << 20
		}
		return db.execStatement(ctx, scope, parsed, 0, nil, 0, nil, streams)
	}
	mustFailState := func(t *testing.T, sql, wantState string) {
		t.Helper()
		_, err := db.execIn(ctx, scope, sql, 10000)
		if err == nil {
			t.Fatalf("exec %q succeeded, want SQLSTATE %s", sql, wantState)
		}
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != wantState {
			t.Fatalf("exec %q error = %v, want SQLSTATE %s", sql, err, wantState)
		}
	}

	// Issue #25: DROP-CASCADE cycles (serial <-> table), non-cascade owned sequence drop,
	// temporary table CASCADE, and DROP COLUMN CASCADE with views/FKs.
	t.Run("Issue25_DropCascadeAndOwnedSequences", func(t *testing.T) {
		mustExec(t, `CREATE TABLE ser_tbl (id serial primary key, v text)`)
		mustExec(t, `CREATE VIEW ser_v AS SELECT id, v FROM ser_tbl`)
		mustExec(t, `DROP TABLE ser_tbl CASCADE`)
		// Re-creating immediately proves no lingering 42P07 table or sequence.
		mustExec(t, `CREATE TABLE ser_tbl (id serial primary key, v text)`)
		mustExec(t, `DROP TABLE ser_tbl`)
		mustExec(t, `CREATE TABLE ser_tbl (id serial primary key, v text)`)
		mustExec(t, `DROP TABLE ser_tbl CASCADE`)

		mustExec(t, `CREATE TEMP TABLE tmp_base (a int)`)
		mustExec(t, `CREATE TEMP VIEW tmp_v AS SELECT a FROM tmp_base`)
		mustFailState(t, `DROP TABLE tmp_base`, "2BP01")
		mustExec(t, `DROP TABLE tmp_base CASCADE`)

		mustExec(t, `CREATE TABLE col_base (id int primary key, x int unique, y int)`)
		mustExec(t, `CREATE VIEW col_v AS SELECT x FROM col_base`)
		mustExec(t, `CREATE TABLE col_ref (rx int REFERENCES col_base(x))`)
		mustFailState(t, `ALTER TABLE col_base DROP COLUMN x`, "2BP01")
		mustExec(t, `ALTER TABLE col_base DROP COLUMN x CASCADE`)
		mustFailState(t, `SELECT * FROM col_v`, "42P01")
	})

	// Issue #26/#10: pg_partition_root, INVALID sub-partitioned index attach,
	// mixed column + expression partition key, PARTITION OF column options, reordered ATTACH PARTITION.
	t.Run("Issue26_PartitionsRoutingAndAttach", func(t *testing.T) {
		mustExec(t, `CREATE TABLE prt_root (a int, b text) PARTITION BY RANGE (a)`)
		mustExec(t, `CREATE TABLE prt_sub PARTITION OF prt_root FOR VALUES FROM (0) TO (100) PARTITION BY RANGE (a)`)
		mustExec(t, `CREATE TABLE prt_leaf PARTITION OF prt_sub FOR VALUES FROM (0) TO (50)`)
		res := mustExec(t, `SELECT pg_partition_root('prt_leaf'::regclass)::text`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal != "prt_root" {
			t.Fatalf("pg_partition_root = %+v, want prt_root", res.Rows)
		}
		mustExec(t, `CREATE INDEX prt_sub_a_idx ON ONLY prt_sub (a)`)
		mustExec(t, `CREATE INDEX prt_root_a_idx ON ONLY prt_root (a)`)
		mustExec(t, `ALTER INDEX prt_root_a_idx ATTACH PARTITION prt_sub_a_idx`)

		// Mixed column + expression partition keys on keyless table.
		mustExec(t, `CREATE TABLE mix_part (a int, b int) PARTITION BY RANGE (a, (b + 1))`)
		mustExec(t, `CREATE TABLE mix_part_1 PARTITION OF mix_part FOR VALUES FROM (0, 0) TO (10, 10)`)
		mustExec(t, `INSERT INTO mix_part VALUES (1, 1)`)

		// PARTITION OF column options + ATTACH PARTITION with reordered columns.
		mustExec(t, `CREATE TABLE pconf (a int unique, b text) PARTITION BY LIST (a)`)
		mustExec(t, `CREATE TABLE pconf_1 PARTITION OF pconf (b unique) FOR VALUES IN (1, 2)`)
		mustExec(t, `CREATE TABLE pconf_2 (b text, a int unique)`)
		mustExec(t, `ALTER TABLE pconf ATTACH PARTITION pconf_2 FOR VALUES IN (3)`)
		mustExec(t, `INSERT INTO pconf VALUES (3, 'x')`)
		res = mustExec(t, `SELECT a, b FROM pconf WHERE a = 3`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 || res.Rows[0][1].StrVal != "x" {
			t.Fatalf("reordered attach select = %+v", res.Rows)
		}
	})

	// Issue #29/#12 & #38: scalar/record builtins in FROM (pg_input_error_info, to_tsquery)
	// and 2-arg ts_rewrite(tsquery, text).
	t.Run("Issue29And38_TableFunctionsAndTSRewrite", func(t *testing.T) {
		res := mustExec(t, `SELECT message, sql_error_code FROM pg_input_error_info('notanint', 'int4')`)
		if len(res.Rows) != 1 || res.Rows[0][0].IsNull || res.Rows[0][1].StrVal != "22P02" {
			t.Fatalf("pg_input_error_info invalid = %+v", res.Rows)
		}
		res = mustExec(t, `SELECT message, sql_error_code FROM pg_input_error_info('42', 'int4')`)
		if len(res.Rows) != 1 || !res.Rows[0][0].IsNull || !res.Rows[0][1].IsNull {
			t.Fatalf("pg_input_error_info valid = %+v, want 1 row of NULLs", res.Rows)
		}
		res = mustExec(t, `SELECT q::text FROM to_tsquery('simple', 'cat & dog') AS q`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal == "" {
			t.Fatalf("FROM to_tsquery = %+v", res.Rows)
		}

		mustExec(t, `CREATE TABLE ts_aliases (t tsquery, s tsquery)`)
		mustExec(t, `INSERT INTO ts_aliases VALUES ('supernovae'::tsquery, 'sn'::tsquery)`)
		res = mustExec(t, `SELECT ts_rewrite('supernovae & star'::tsquery, 'SELECT t, s FROM ts_aliases')::text`)
		if len(res.Rows) != 1 || !strings.Contains(res.Rows[0][0].StrVal, "sn") {
			t.Fatalf("ts_rewrite 2-arg = %+v, want sn", res.Rows)
		}
	})

	// Issue #32 & #35: Keyless table PL/pgSQL trigger arity, COPY DML without RETURNING,
	// COPY HEADER true on empty input, HEADER MATCH \N rejection, CSV literal \., SQL_ASCII.
	t.Run("Issue32And35_KeylessTriggersAndCopy", func(t *testing.T) {
		mustExec(t, `CREATE TABLE wslot_test (slotname char(20), roomno char(8), slotlink char(20), backlink char(20))`)
		mustExec(t, `CREATE FUNCTION wslot_trig() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				NEW.slotname := trim(NEW.slotname);
				RETURN NEW;
			END;
		$$`)
		mustExec(t, `CREATE TRIGGER wslot_bi BEFORE INSERT OR UPDATE ON wslot_test FOR EACH ROW EXECUTE FUNCTION wslot_trig()`)
		mustExec(t, `INSERT INTO wslot_test VALUES ('WS.001', '001', '', '')`)
		mustExec(t, `UPDATE wslot_test SET roomno = '002' WHERE slotname = 'WS.001'`)
		mustExec(t, `DELETE FROM wslot_test WHERE slotname = 'WS.001'`)

		mustExec(t, `CREATE TABLE copy_dml_t (id int, v text)`)
		for _, sql := range []string{
			`COPY (INSERT INTO copy_dml_t VALUES (1, 'a')) TO STDOUT`,
			`COPY (UPDATE copy_dml_t SET v = 'b') TO STDOUT`,
			`COPY (DELETE FROM copy_dml_t) TO STDOUT`,
		} {
			var dmlBuf bytes.Buffer
			_, dmlErr := execCopy(sql, &qcopy.Streams{Writer: &dmlBuf})
			var se *model.Error
			if !errors.As(dmlErr, &se) || se.Code != "0A000" {
				t.Fatalf("exec %q err = %v, want 0A000", sql, dmlErr)
			}
		}

		// Empty input with HEADER true succeeds with 0 rows.
		if _, err := execCopy(`COPY copy_dml_t FROM STDIN WITH (HEADER true)`, &qcopy.Streams{Reader: strings.NewReader("")}); err != nil {
			t.Fatalf("COPY empty HEADER true: %v", err)
		}
		// HEADER MATCH rejects \N.
		if _, err := execCopy(`COPY copy_dml_t (id, v) FROM STDIN WITH (HEADER MATCH)`, &qcopy.Streams{Reader: strings.NewReader("\\N\tv\n1\ta\n")}); err == nil {
			t.Fatalf("COPY HEADER MATCH with \\N unexpectedly succeeded")
		}
		// CSV treats unquoted \. as literal data in PG 18.
		mustExec(t, `CREATE TABLE csv_dot (a text)`)
		if _, err := execCopy(`COPY csv_dot FROM STDIN WITH (FORMAT csv)`, &qcopy.Streams{Reader: strings.NewReader("\\.\nhello\n")}); err != nil {
			t.Fatalf("COPY CSV literal \\.: %v", err)
		}
		res := mustExec(t, `SELECT count(*) FROM csv_dot`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 2 {
			t.Fatalf("CSV literal \\. row count = %+v, want 2", res.Rows)
		}
		// ENCODING 'SQL_ASCII' accepted.
		var outBuf bytes.Buffer
		if _, err := execCopy(`COPY csv_dot TO STDOUT WITH (ENCODING 'SQL_ASCII')`, &qcopy.Streams{Writer: &outBuf}); err != nil {
			t.Fatalf("COPY ENCODING SQL_ASCII: %v", err)
		}

		// Harness splitRegressionSQL COPY stdin must skip trailing line/comment.
		stmts, err := splitRegressionSQL("COPY t FROM stdin; -- comment\n1\ta\n\\.\nSELECT 1;\n")
		if err != nil {
			t.Fatal(err)
		}
		if len(stmts) != 2 || stmts[0].kind != regCopyIn || stmts[0].data != "1\ta\n" {
			t.Fatalf("splitRegressionSQL COPY data = %+v, want \"1\\ta\\n\"", stmts)
		}
	})

	// Issue #31 & #34: SET SESSION AUTHORIZATION, row_security GUC, policy dependencies,
	// COPY TO RLS rejection, TRIGGER privilege, WITH ADMIN OPTION, predefined roles.
	t.Run("Issue31And34_SessionAuthRLSAndRoles", func(t *testing.T) {
		mustExec(t, `CREATE ROLE rls_u1`)
		mustExec(t, `CREATE ROLE rls_u2`)
		mustExec(t, `CREATE ROLE rls_u3`)
		mustExec(t, `CREATE TABLE rls_dep (k int)`)
		mustExec(t, `CREATE TABLE rls_tbl (id int, owner_name text)`)
		mustExec(t, `INSERT INTO rls_tbl VALUES (1, 'rls_u1'), (2, 'rls_u2')`)
		mustExec(t, `GRANT SELECT ON rls_tbl TO rls_u1, rls_u2`)
		mustExec(t, `ALTER TABLE rls_tbl ENABLE ROW LEVEL SECURITY`)
		mustExec(t, `CREATE POLICY p_dep ON rls_tbl FOR SELECT USING (owner_name = current_user AND EXISTS (SELECT 1 FROM rls_dep))`)
		// Policy expression dependency blocks DROP TABLE rls_dep.
		mustFailState(t, `DROP TABLE rls_dep`, "2BP01")
		mustExec(t, `DROP POLICY p_dep ON rls_tbl`)
		mustExec(t, `CREATE POLICY p_own ON rls_tbl FOR SELECT USING (owner_name = current_user)`)

		// SET SESSION AUTHORIZATION switches user and enforces RLS + row_security=off + PG 18 COPY TO RLS.
		mustExec(t, `SET SESSION AUTHORIZATION rls_u1`)
		res := mustExec(t, `SELECT id FROM rls_tbl`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 1 {
			t.Fatalf("rls_u1 rows = %+v, want [1]", res.Rows)
		}
		mustExec(t, `SET row_security = off`)
		mustFailState(t, `SELECT id FROM rls_tbl`, "42501")
		var copyBuf bytes.Buffer
		_, copyErr := execCopy(`COPY rls_tbl TO STDOUT`, &qcopy.Streams{Writer: &copyBuf})
		var sqlErr *model.Error
		if !errors.As(copyErr, &sqlErr) || sqlErr.Code != "42501" {
			t.Fatalf("COPY TO under RLS with row_security=off err = %v, want 42501", copyErr)
		}
		mustExec(t, `SET row_security = on`)
		copyBuf.Reset()
		if _, err := execCopy(`COPY rls_tbl TO STDOUT`, &qcopy.Streams{Writer: &copyBuf}); err != nil {
			t.Fatalf("COPY TO under RLS with row_security=on failed: %v", err)
		}
		if strings.TrimSpace(copyBuf.String()) != "1\trls_u1" {
			t.Fatalf("COPY TO under RLS output = %q, want %q", copyBuf.String(), "1\trls_u1")
		}
		// Switch directly to another role (authenticated user is superuser) and reset.
		mustExec(t, `SET SESSION AUTHORIZATION rls_u2`)
		res = mustExec(t, `SELECT id FROM rls_tbl`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 2 {
			t.Fatalf("rls_u2 rows = %+v, want [2]", res.Rows)
		}
		mustExec(t, `RESET SESSION AUTHORIZATION`)

		// Predefined role pg_read_all_data + WITH ADMIN OPTION delegation.
		mustExec(t, `CREATE TABLE priv_secret (v int)`)
		mustExec(t, `INSERT INTO priv_secret VALUES (99)`)
		mustExec(t, `GRANT pg_read_all_data TO rls_u1`)
		mustExec(t, `GRANT rls_u2 TO rls_u1 WITH ADMIN OPTION`)
		mustExec(t, `SET SESSION AUTHORIZATION rls_u1`)
		res = mustExec(t, `SELECT v FROM priv_secret`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 99 {
			t.Fatalf("pg_read_all_data select = %+v, want [99]", res.Rows)
		}
		// Non-superuser with ADMIN OPTION can grant rls_u2 to rls_u3.
		mustExec(t, `GRANT rls_u2 TO rls_u3`)
		mustExec(t, `RESET SESSION AUTHORIZATION`)
		// Revoking rls_u2 from rls_u1 RESTRICT fails due to downstream grant to rls_u3.
		mustFailState(t, `REVOKE rls_u2 FROM rls_u1 RESTRICT`, "2BP01")
		mustExec(t, `REVOKE rls_u2 FROM rls_u1 CASCADE`)
	})
}
