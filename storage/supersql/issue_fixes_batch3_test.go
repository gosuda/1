package supersql

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBatch3TypesParsingOperatorsTailIssueFixes(t *testing.T) {
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
	execRaw := func(sql string) (outcome, error) {
		return db.execIn(ctx, scope, sql, 10000)
	}
	mustFailState := func(t *testing.T, sql, wantCode string) {
		t.Helper()
		_, err := db.execIn(ctx, scope, sql, 10000)
		if err == nil {
			t.Fatalf("exec %q succeeded, want SQLSTATE %s", sql, wantCode)
		}
		var diagnostic *model.Error
		if !errors.As(err, &diagnostic) || diagnostic.Code != wantCode {
			t.Fatalf("exec %q error = %v, want SQLSTATE %s", sql, err, wantCode)
		}
	}

	// Issue #33: typmod domains, coalesce domain unwrapping, composite/array domain checks,
	// ALTER DOMAIN ADD NOT NULL, ALTER TYPE -> domain cascade, to_number RN fixes.
	t.Run("Issue33_DomainsCompositesAndRoman", func(t *testing.T) {
		mustExec(t, `CREATE DOMAIN d_vc5 AS varchar(5)`)
		res := mustExec(t, `SELECT 'hello'::d_vc5::text`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal != "hello" {
			t.Fatalf("d_vc5 = %+v", res.Rows)
		}
		mustFailState(t, `SELECT 'toolong'::d_vc5`, "22001")

		mustExec(t, `CREATE DOMAIN d_int4 AS int4`)
		res = mustExec(t, `SELECT coalesce(4::d_int4, 7)`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 4 {
			t.Fatalf("coalesce domain = %+v", res.Rows)
		}
		mustExec(t, `ALTER DOMAIN d_int4 ADD CONSTRAINT d_int4_nn NOT NULL`)
		mustFailState(t, `SELECT NULL::d_int4`, "23502")

		mustExec(t, `CREATE DOMAIN d_ord AS int[] CHECK (VALUE[1] < VALUE[2])`)
		mustFailState(t, `SELECT ARRAY[2, 1]::d_ord`, "23514")
		mustExec(t, `SELECT ARRAY[1, 2]::d_ord`)

		mustExec(t, `CREATE TYPE comp33 AS (r float8, i float8)`)
		mustExec(t, `CREATE DOMAIN dcomp33 AS comp33`)
		mustExec(t, `ALTER DOMAIN dcomp33 ADD CONSTRAINT c1 CHECK ((VALUE).r > 0)`)
		mustFailState(t, `SELECT ROW(0, 1)::dcomp33`, "23514")
		mustExec(t, `SELECT ROW(1, 2)::dcomp33`)
		if _, err := execRaw(`ALTER TYPE comp33 ALTER ATTRIBUTE r TYPE varchar`); err == nil {
			t.Fatalf("expected ALTER ATTRIBUTE r TYPE varchar to fail against domain check")
		} else {
			var diagnostic *model.Error
			if !errors.As(err, &diagnostic) {
				t.Fatalf("ALTER ATTRIBUTE r TYPE varchar error = %v, want SQL error", err)
			}
		}
		mustExec(t, `ALTER TYPE comp33 ALTER ATTRIBUTE r TYPE bigint`)
		mustFailState(t, `SELECT ROW(0::bigint, 1)::dcomp33`, "23514")
		mustExec(t, `SELECT ROW(1::bigint, 2)::dcomp33`)
		if _, err := execRaw(`ALTER TYPE comp33 DROP ATTRIBUTE r`); err == nil {
			t.Fatalf("expected DROP ATTRIBUTE r to fail against domain check")
		} else {
			var diagnostic *model.Error
			if !errors.As(err, &diagnostic) {
				t.Fatalf("DROP ATTRIBUTE r error = %v, want SQL error", err)
			}
		}
		mustExec(t, `ALTER TYPE comp33 DROP ATTRIBUTE i`)
		mustFailState(t, `SELECT ROW(0::bigint)::dcomp33`, "23514")
		mustExec(t, `SELECT ROW(1::bigint)::dcomp33`)

		res = mustExec(t, `SELECT to_number('  XIV', '  RN')::int, to_number('M CC', 'RN')::int`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 14 || res.Rows[0][1].IntVal != 1000 {
			t.Fatalf("to_number RN = %+v", res.Rows)
		}
		mustFailState(t, `SELECT to_number('CM', 'RNRN')`, "22023")
	})

	// Issue #36: ISO compact datetime T + negative TZ, to_timestamp TZ/FF/separators, large scientific numeric.
	t.Run("Issue36_DatetimePictureAndNumeric", func(t *testing.T) {
		mustExec(t, `SELECT timestamp with time zone '20011227T040506-08'`)
		mustExec(t, `SELECT timestamp with time zone 'J2452271T040506-08'`)

		mustExec(t, `SELECT to_timestamp('2011-12-18 11:38 -05', 'YYYY-MM-DD HH12:MI TZ')`)
		mustExec(t, `SELECT to_timestamp('2011-12-18 11:38 +01:30', 'YYYY-MM-DD HH12:MI TZ')`)
		mustExec(t, `SELECT to_timestamp('2011-12-18 11:38ESTFOO24', 'YYYY-MM-DD HH12:MITZFOOSS')`)
		mustExec(t, `SELECT to_timestamp('2011-12-18 11:38-05FOO24', 'YYYY-MM-DD HH12:MITZFOOSS')`)
		mustFailState(t, `SELECT to_timestamp('2011-12-18 11:38 JUNK', 'YYYY-MM-DD HH12:MI TZ')`, "22007")
		mustExec(t, `SELECT to_timestamp('2018-11-02 12:34:56', 'YYYY-MM-DD HH24:MI:SS.FF3')`)
		mustExec(t, `SELECT to_timestamp('2018-11-02 12:34:56.12', 'YYYY-MM-DD HH24:MI:SS.FF1')`)
		mustExec(t, `SELECT to_timestamp('2000 + JUN', 'YYYY MON')`)
		mustExec(t, `SELECT to_timestamp('2000 ++ JUN', 'YYYY  MON')`)
		mustFailState(t, `SELECT to_timestamp('2000 + + JUN', 'YYYY  MON')`, "22007")
		mustExec(t, `SELECT to_timestamp('2000 + + JUN', 'YYYY   MON')`)
		mustExec(t, `SELECT to_timestamp('2000 -10', 'YYYY TZH')`)

		res := mustExec(t, `SELECT round(4.4e131071, -131071) = 4e131071, trunc(9.9e131071, -131071) = 9e131071`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal == 0 || res.Rows[0][1].IntVal == 0 {
			t.Fatalf("large scientific round/trunc = %+v", res.Rows)
		}
		mustFailState(t, `SELECT round(5.5e131071, -131072)`, "22003")
	})

	// Issue #37: builtin-backed user operators, ALTER OPERATOR, RETURNS record star expansion,
	// overload tie-breaking, and binary-coercible casts.
	t.Run("Issue37_OperatorsOverloadsAndCasts", func(t *testing.T) {
		mustExec(t, `CREATE OPERATOR ===# (LEFTARG = int4, RIGHTARG = int4, PROCEDURE = int4eq)`)
		mustExec(t, `CREATE OPERATOR !==# (LEFTARG = int4, RIGHTARG = int4, PROCEDURE = int4ne)`)
		res := mustExec(t, `SELECT 5 ===# 5, 5 !==# 6`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal == 0 || res.Rows[0][1].IntVal == 0 {
			t.Fatalf("builtin-backed user operator = %+v", res.Rows)
		}
		mustExec(t, `ALTER OPERATOR ===# (int4, int4) SET (MERGES)`)
		mustExec(t, `ALTER OPERATOR ===# (int4, int4) SET (HASHES)`)
		mustExec(t, `ALTER OPERATOR ===# (int4, int4) SET (COMMUTATOR = ===#)`)
		mustExec(t, `ALTER OPERATOR ===# (int4, int4) SET (NEGATOR = !==#)`)
		mustFailState(t, `ALTER OPERATOR ===# (int4, int4) SET (NEGATOR = ===#)`, "42P13")

		mustExec(t, `CREATE FUNCTION dfunc37(a int, b int, OUT x int, OUT y int) RETURNS record LANGUAGE sql AS $$ SELECT a + b, a - b $$`)
		res = mustExec(t, `SELECT (dfunc37(10, 3)).*`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 13 || res.Rows[0][1].IntVal != 7 {
			t.Fatalf("(dfunc37).* = %+v", res.Rows)
		}

		mustExec(t, `CREATE FUNCTION ovl37(text) RETURNS int LANGUAGE sql AS $$ SELECT 1 $$`)
		mustExec(t, `CREATE FUNCTION ovl37(int) RETURNS int LANGUAGE sql AS $$ SELECT 2 $$`)
		res = mustExec(t, `SELECT ovl37('abc'), ovl37(42)`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 1 || res.Rows[0][1].IntVal != 2 {
			t.Fatalf("overload resolution = %+v", res.Rows)
		}
	})

	// Issue #39: JSON_ARRAYAGG, JSON_ARRAY(SELECT ...), JSON_OBJECTAGG, NULL arg, non-scalar keys.
	t.Run("Issue39_SQLJSONAggregatesAndKeys", func(t *testing.T) {
		mustExec(t, `CREATE TABLE j39 (k text, v int)`)
		mustExec(t, `INSERT INTO j39 VALUES ('a', 1), ('b', 2)`)
		res := mustExec(t, `SELECT JSON_ARRAYAGG(v ORDER BY v)::text, JSON_OBJECTAGG(k VALUE v)::text FROM j39`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal != "[1, 2]" {
			t.Fatalf("JSON_ARRAYAGG/JSON_OBJECTAGG = %+v", res.Rows)
		}
		res = mustExec(t, `SELECT JSON_ARRAYAGG(NULL)::text, JSON_ARRAYAGG(NULL NULL ON NULL)::text`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal != "[]" || res.Rows[0][1].StrVal != "[null]" {
			t.Fatalf("JSON_ARRAYAGG(NULL) = %+v", res.Rows)
		}
		res = mustExec(t, `SELECT JSON_ARRAY(SELECT v FROM j39 ORDER BY v)::text`)
		if len(res.Rows) != 1 || res.Rows[0][0].StrVal != "[1, 2]" {
			t.Fatalf("JSON_ARRAY(SELECT ...) = %+v", res.Rows)
		}
		mustFailState(t, `SELECT JSON_OBJECT(json '[1]': 123)`, "22023")
		mustFailState(t, `SELECT JSON_OBJECT(ARRAY[1,2,3]: 'aaa')`, "22023")
	})

	// Issue #40: ANALYZE owner/pg_catalog warn+skip, BUFFER_USAGE_LIMIT 0, transactional TABLESAMPLE, 2202G, 42809.
	t.Run("Issue40_AnalyzeAndTablesample", func(t *testing.T) {
		t.Cleanup(func() { _, _ = execRaw("ROLLBACK") })
		mustExec(t, `CREATE TABLE ts40 (id int primary key, v text)`)
		mustExec(t, `INSERT INTO ts40 VALUES (1, 'a'), (2, 'b'), (3, 'c')`)
		mustExec(t, `ANALYZE (BUFFER_USAGE_LIMIT 0) ts40`)
		mustExec(t, `ANALYZE pg_class`)

		mustExec(t, `CREATE ROLE nonowner40 LOGIN`)
		mustExec(t, `SET SESSION AUTHORIZATION nonowner40`)
		mustFailState(t, `ANALYZE ts40`, "42501")
		mustExec(t, `ANALYZE`)
		mustExec(t, `RESET SESSION AUTHORIZATION`)

		mustExec(t, `BEGIN`)
		res := mustExec(t, `SELECT count(*) FROM ts40 TABLESAMPLE BERNOULLI (100)`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 {
			t.Fatalf("transactional TABLESAMPLE BERNOULLI = %+v", res.Rows)
		}
		res = mustExec(t, `SELECT count(*) FROM ts40 TABLESAMPLE SYSTEM (100) REPEATABLE (1)`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 {
			t.Fatalf("transactional TABLESAMPLE SYSTEM = %+v", res.Rows)
		}
		mustExec(t, `COMMIT`)

		mustFailState(t, `SELECT * FROM ts40 TABLESAMPLE BERNOULLI (50) REPEATABLE (NULL)`, "2202G")
		mustExec(t, `CREATE VIEW v40 AS SELECT * FROM ts40`)
		mustFailState(t, `SELECT * FROM v40 TABLESAMPLE BERNOULLI (50)`, "42809")
		mustFailState(t, `WITH c AS (SELECT * FROM ts40) SELECT * FROM c TABLESAMPLE BERNOULLI (50)`, "42809")
	})

	// Issue #42: pg_indexes opclass 55000, partition identity, ADD IDENTITY nullable check,
	// GENERATED/MultiAssignRef DEFAULT, and WHERE CURRENT OF.
	t.Run("Issue42_OpclassIdentityGeneratedAndCurrentOf", func(t *testing.T) {
		t.Cleanup(func() { _, _ = execRaw("ROLLBACK") })
		mustExec(t, `CREATE TABLE idx42 (id int primary key, a int unique, b text)`)
		mustExec(t, `CREATE INDEX idx42_b ON idx42 (b)`)
		res := mustExec(t, `SELECT count(*) FROM pg_indexes WHERE tablename = 'idx42'`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 3 {
			t.Fatalf("pg_indexes count = %+v, want 3", res.Rows)
		}

		mustExec(t, `CREATE TABLE idp42 (id int GENERATED ALWAYS AS IDENTITY, v text) PARTITION BY RANGE (id)`)
		mustExec(t, `CREATE TABLE idp42_1 PARTITION OF idp42 FOR VALUES FROM (1) TO (100)`)
		mustExec(t, `INSERT INTO idp42_1 (v) VALUES ('one')`)
		res = mustExec(t, `SELECT id, v FROM idp42`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 1 || res.Rows[0][1].StrVal != "one" {
			t.Fatalf("partition identity insert = %+v", res.Rows)
		}

		mustExec(t, `CREATE TABLE null_id42 (a int)`)
		mustFailState(t, `ALTER TABLE null_id42 ALTER COLUMN a ADD GENERATED ALWAYS AS IDENTITY`, "55000")

		mustExec(t, `CREATE TABLE gen42 (a int primary key, b int GENERATED ALWAYS AS (a * 2) STORED)`)
		mustExec(t, `INSERT INTO gen42 VALUES (1, DEFAULT)`)
		mustExec(t, `UPDATE gen42 SET (a, b) = (5, DEFAULT) WHERE a = 1`)
		res = mustExec(t, `SELECT a, b FROM gen42`)
		if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 5 || res.Rows[0][1].IntVal != 10 {
			t.Fatalf("MultiAssignRef DEFAULT generated = %+v", res.Rows)
		}

		mustExec(t, `CREATE TABLE cur42 (id int primary key, val text)`)
		mustExec(t, `INSERT INTO cur42 VALUES (1, 'a'), (2, 'b'), (3, 'c')`)
		mustExec(t, `BEGIN`)
		mustExec(t, `DECLARE c42 CURSOR FOR SELECT id, val FROM cur42 ORDER BY id`)
		mustFailState(t, `UPDATE cur42 SET val = 'bad' WHERE CURRENT OF c42`, "55000")
		mustExec(t, `ROLLBACK`)

		mustExec(t, `BEGIN`)
		mustExec(t, `DECLARE c42 CURSOR FOR SELECT id, val FROM cur42 ORDER BY id`)
		mustExec(t, `FETCH 1 FROM c42`)
		mustExec(t, `UPDATE cur42 SET val = 'updated' WHERE CURRENT OF c42`)
		mustExec(t, `FETCH 1 FROM c42`)
		mustExec(t, `DELETE FROM cur42 WHERE CURRENT OF c42`)
		mustExec(t, `CLOSE c42`)
		mustExec(t, `COMMIT`)

		res = mustExec(t, `SELECT id, val FROM cur42 ORDER BY id`)
		if len(res.Rows) != 2 || res.Rows[0][0].IntVal != 1 || res.Rows[0][1].StrVal != "updated" || res.Rows[1][0].IntVal != 3 {
			t.Fatalf("after WHERE CURRENT OF = %+v", res.Rows)
		}
	})

	// Issue #14: CREATE COLLATION default libc, to_regnamespace/to_regrole, partitioned MERGE.
	t.Run("Issue14_CollationRegnamespaceAndInheritedMerge", func(t *testing.T) {
		mustFailState(t, `CREATE COLLATION c14_libc (locale = 'C')`, "0A000")

		res := mustExec(t, `SELECT to_regnamespace('foo.bar'), to_regrole('foo.bar')`)
		if len(res.Rows) != 1 || !res.Rows[0][0].IsNull || !res.Rows[0][1].IsNull {
			t.Fatalf("to_regnamespace/to_regrole foo.bar = %+v, want NULLs", res.Rows)
		}

		mustExec(t, `CREATE TABLE mp14 (id int primary key, v text) PARTITION BY RANGE (id)`)
		mustExec(t, `CREATE TABLE mp14_1 PARTITION OF mp14 FOR VALUES FROM (1) TO (10)`)
		mustExec(t, `INSERT INTO mp14 VALUES (1, 'old')`)
		mustExec(t, `MERGE INTO mp14 t USING (VALUES (1, 'new'), (2, 'ins')) AS s(id, v) ON t.id = s.id WHEN MATCHED THEN UPDATE SET v = s.v WHEN NOT MATCHED THEN INSERT VALUES (s.id, s.v)`)
		res = mustExec(t, `SELECT id, v FROM mp14 ORDER BY id`)
		if len(res.Rows) != 2 || res.Rows[0][1].StrVal != "new" || res.Rows[1][1].StrVal != "ins" {
			t.Fatalf("partitioned MERGE = %+v", res.Rows)
		}
	})
}
