package supersql

import "testing"

func openPhase4DDLOwnershipDB(t *testing.T) (func(string) outcome, func(string, string)) {
	t.Helper()
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	mustExec := func(sql string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sql, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sql, err)
		}
		return out
	}

	requireErrCode := func(sql, code string) {
		t.Helper()
		_, err := db.exec(ctx, sql, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sql, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sql, code, got, err)
		}
	}

	return mustExec, requireErrCode
}

func TestPhase4DDLOwnershipRefreshMatViewReindexAndSetSchema(t *testing.T) {
	mustExec, requireErrCode := openPhase4DDLOwnershipDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE SCHEMA s3`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO alice, bob`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s3 TO alice, bob`)

	// Alice owns table, index, view, materialized view, and sequence in s1.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE s1.t (id int PRIMARY KEY, val text)`)
	mustExec(`INSERT INTO s1.t VALUES (1, 'a'), (2, 'b')`)
	mustExec(`CREATE INDEX idx_t_val ON s1.t (val)`)
	mustExec(`CREATE VIEW s1.v AS SELECT id, val FROM s1.t`)
	mustExec(`CREATE MATERIALIZED VIEW s1.mv AS SELECT id, val FROM s1.t`)
	mustExec(`CREATE SEQUENCE s1.seq`)
	mustExec(`GRANT ALL ON TABLE s1.t, s1.v, s1.mv TO bob`)
	mustExec(`GRANT ALL ON SEQUENCE s1.seq TO bob`)

	// ALL includes MAINTAIN, allowing reindex without ownership. Refresh
	// delegation remains unavailable until stored-query identities are pinned;
	// unrelated SET SCHEMA operations still require the owner.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`REFRESH MATERIALIZED VIEW s1.mv`, "42501")
	requireErrCode(`REFRESH MATERIALIZED VIEW s1.mv WITH DATA`, "42501")
	requireErrCode(`REFRESH MATERIALIZED VIEW s1.mv WITH NO DATA`, "42501")
	mustExec(`REINDEX TABLE s1.t`)
	mustExec(`REINDEX INDEX s1.idx_t_val`)
	requireErrCode(`ALTER TABLE s1.t SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER VIEW s1.v SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER MATERIALIZED VIEW s1.mv SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.seq SET SCHEMA s3`, "42501")

	// Removing MAINTAIN restores reindex denial despite the remaining ACL.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	mustExec(`REVOKE MAINTAIN ON s1.t FROM bob`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`REINDEX TABLE s1.t`, "42501")
	requireErrCode(`REINDEX INDEX s1.idx_t_val`, "42501")

	// Alice (owner) can refresh and reindex, is denied SET SCHEMA without destination CREATE (s2),
	// and succeeds on SET SCHEMA with destination CREATE (s3).
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	mustExec(`REFRESH MATERIALIZED VIEW s1.mv WITH NO DATA`)
	mustExec(`REFRESH MATERIALIZED VIEW s1.mv WITH DATA`)
	mustExec(`REINDEX INDEX s1.idx_t_val`)
	mustExec(`REINDEX TABLE s1.t`)

	requireErrCode(`ALTER TABLE s1.t SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER VIEW s1.v SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER MATERIALIZED VIEW s1.mv SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.seq SET SCHEMA s2`, "42501")

	mustExec(`ALTER TABLE s1.t SET SCHEMA s3`)
	mustExec(`ALTER VIEW s1.v SET SCHEMA s3`)
	mustExec(`ALTER MATERIALIZED VIEW s1.mv SET SCHEMA s3`)
	mustExec(`ALTER SEQUENCE s1.seq SET SCHEMA s3`)
}

func TestPhase4DDLOwnershipTypesAndDomains(t *testing.T) {
	mustExec, requireErrCode := openPhase4DDLOwnershipDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE SCHEMA s3`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO alice`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s3 TO alice, bob`)

	// Alice cannot create types/domains in s2 (no CREATE privilege).
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TYPE s2.e_denied AS ENUM ('a')`, "42501")
	requireErrCode(`CREATE DOMAIN s2.d_denied AS int`, "42501")
	requireErrCode(`CREATE TYPE s2.c_denied AS (a int)`, "42501")
	requireErrCode(`CREATE TYPE s2.r_denied AS RANGE (SUBTYPE = int4)`, "42501")
	requireErrCode(`CREATE TYPE s2.sh_denied`, "42501")

	// Alice creates enum, domain, composite, range, and shell type in s1.
	mustExec(`CREATE TYPE s1.my_enum AS ENUM ('red', 'green')`)
	mustExec(`CREATE DOMAIN s1.my_dom AS int CONSTRAINT pos_chk CHECK (VALUE > 0)`)
	mustExec(`CREATE TYPE s1.my_comp AS (a int, b text)`)
	mustExec(`CREATE TYPE s1.my_range AS RANGE (SUBTYPE = int4)`)
	mustExec(`CREATE TYPE s1.my_shell`)

	// Grant bob CREATE on s1 too so shell-completion test isolates shell ownership.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT CREATE ON SCHEMA s1 TO bob`)

	// Bob (non-owner) must be denied on all ALTER/RENAME/SET SCHEMA/DROP and shell completion.
	mustExec(`SET ROLE bob`)
	requireErrCode(`CREATE TYPE s1.my_shell AS RANGE (SUBTYPE = int4)`, "42501")
	requireErrCode(`ALTER TYPE s1.my_enum ADD VALUE 'blue'`, "42501")
	requireErrCode(`ALTER TYPE s1.my_enum ADD VALUE IF NOT EXISTS 'red'`, "42501")
	requireErrCode(`ALTER TYPE s1.my_enum RENAME VALUE 'red' TO 'crimson'`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom SET DEFAULT 1`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom SET NOT NULL`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom ADD CONSTRAINT max_chk CHECK (VALUE < 100)`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom RENAME CONSTRAINT pos_chk TO pos_chk2`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom DROP CONSTRAINT pos_chk`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp ADD ATTRIBUTE c bool`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp DROP ATTRIBUTE b`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp ALTER ATTRIBUTE a TYPE bigint`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp RENAME ATTRIBUTE a TO a2`, "42501")
	requireErrCode(`ALTER TYPE s1.my_enum RENAME TO my_enum_stolen`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom RENAME TO my_dom_stolen`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp RENAME TO my_comp_stolen`, "42501")
	requireErrCode(`ALTER TYPE s1.my_range RENAME TO my_range_stolen`, "42501")
	requireErrCode(`ALTER TYPE s1.my_enum SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER TYPE s1.my_range SET SCHEMA s3`, "42501")
	requireErrCode(`DROP TYPE s1.my_enum`, "42501")
	requireErrCode(`DROP DOMAIN s1.my_dom`, "42501")
	requireErrCode(`DROP TYPE s1.my_comp`, "42501")
	requireErrCode(`DROP TYPE s1.my_range`, "42501")
	requireErrCode(`DROP TYPE s1.my_shell`, "42501")

	// Alice (owner) fails SET SCHEMA to s2 (no CREATE), succeeds on all ALTER/RENAME/SET SCHEMA s3/DROP.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`ALTER TYPE s1.my_enum SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER DOMAIN s1.my_dom SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER TYPE s1.my_comp SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER TYPE s1.my_range SET SCHEMA s2`, "42501")

	mustExec(`CREATE TYPE s1.my_shell AS RANGE (SUBTYPE = int4)`)
	mustExec(`ALTER TYPE s1.my_enum ADD VALUE 'blue'`)
	mustExec(`ALTER TYPE s1.my_enum RENAME VALUE 'red' TO 'crimson'`)
	mustExec(`ALTER DOMAIN s1.my_dom SET DEFAULT 1`)
	mustExec(`ALTER DOMAIN s1.my_dom SET NOT NULL`)
	mustExec(`ALTER DOMAIN s1.my_dom RENAME CONSTRAINT pos_chk TO pos_chk2`)
	mustExec(`ALTER TYPE s1.my_comp ADD ATTRIBUTE c bool`)
	mustExec(`ALTER TYPE s1.my_comp RENAME ATTRIBUTE a TO a2`)
	mustExec(`ALTER TYPE s1.my_enum RENAME TO my_enum2`)
	mustExec(`ALTER DOMAIN s1.my_dom RENAME TO my_dom2`)
	mustExec(`ALTER TYPE s1.my_comp RENAME TO my_comp2`)
	mustExec(`ALTER TYPE s1.my_range RENAME TO my_range2`)

	mustExec(`ALTER TYPE s1.my_enum2 SET SCHEMA s3`)
	mustExec(`ALTER DOMAIN s1.my_dom2 SET SCHEMA s3`)
	mustExec(`ALTER TYPE s1.my_comp2 SET SCHEMA s3`)
	mustExec(`ALTER TYPE s1.my_range2 SET SCHEMA s3`)
	mustExec(`ALTER TYPE s1.my_shell SET SCHEMA s3`)

	mustExec(`DROP TYPE s3.my_enum2`)
	mustExec(`DROP DOMAIN s3.my_dom2`)
	mustExec(`DROP TYPE s3.my_comp2`)
	mustExec(`DROP TYPE s3.my_range2`)
	mustExec(`DROP TYPE s3.my_shell`)
}

func TestPhase4DDLOwnershipOperatorsAggregatesAndCasts(t *testing.T) {
	mustExec, requireErrCode := openPhase4DDLOwnershipDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE SCHEMA s3`)
	mustExec(`CREATE SCHEMA priv_fn`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO alice`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s3 TO alice, bob`)

	// Admin creates restricted helper functions in priv_fn.
	mustExec(`CREATE FUNCTION priv_fn.secret_add(a int, b int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT a + b'`)
	mustExec(`CREATE FUNCTION priv_fn.secret_neg(a int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT -a'`)
	mustExec(`REVOKE ALL ON FUNCTION priv_fn.secret_add(int, int) FROM PUBLIC`)
	mustExec(`REVOKE ALL ON FUNCTION priv_fn.secret_neg(int) FROM PUBLIC`)

	// Alice creates her own helper functions and types in s1.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE FUNCTION s1.my_add(a int, b int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT a + b'`)
	mustExec(`CREATE FUNCTION s1.my_neg(a int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT -a'`)
	mustExec(`CREATE TYPE s1.alice_enum AS ENUM ('one', 'two')`)
	mustExec(`CREATE TYPE s1.alice_enum2 AS ENUM ('x', 'y')`)
	mustExec(`CREATE TYPE s1.alice_comp AS (x int)`)
	mustExec(`CREATE FUNCTION s1.enum_to_text(v s1.alice_enum) RETURNS text LANGUAGE sql IMMUTABLE AS 'SELECT ''val''::text'`)
	mustExec(`CREATE FUNCTION s1.int_to_comp(v int) RETURNS s1.alice_comp LANGUAGE sql IMMUTABLE AS 'SELECT ROW(v)::s1.alice_comp'`)

	// 1. CREATE OPERATOR / CREATE AGGREGATE schema CREATE and support-function privilege checks.
	requireErrCode(`CREATE OPERATOR s2.##+ (LEFTARG = int, RIGHTARG = int, FUNCTION = s1.my_add)`, "42501")
	requireErrCode(`CREATE AGGREGATE s2.my_sum(int) (SFUNC = s1.my_add, STYPE = int)`, "42501")
	// Denied without USAGE on priv_fn schema.
	requireErrCode(`CREATE OPERATOR s1.##+ (LEFTARG = int, RIGHTARG = int, FUNCTION = priv_fn.secret_add)`, "42501")
	requireErrCode(`CREATE AGGREGATE s1.my_sum(int) (SFUNC = priv_fn.secret_add, STYPE = int)`, "42501")

	// Grant USAGE on priv_fn without EXECUTE on function -> still denied.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT USAGE ON SCHEMA priv_fn TO alice`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE OPERATOR s1.##+ (LEFTARG = int, RIGHTARG = int, FUNCTION = priv_fn.secret_add)`, "42501")
	requireErrCode(`CREATE AGGREGATE s1.my_sum(int) (SFUNC = priv_fn.secret_add, STYPE = int)`, "42501")

	// Grant EXECUTE on priv_fn.secret_add -> succeeds.
	mustExec(`RESET ROLE`)
	mustExec(`GRANT EXECUTE ON FUNCTION priv_fn.secret_add(int, int) TO alice`)
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE OPERATOR s1.##+ (LEFTARG = int, RIGHTARG = int, FUNCTION = priv_fn.secret_add)`)
	mustExec(`CREATE OPERATOR s1.##~ (RIGHTARG = int, FUNCTION = s1.my_neg)`)
	mustExec(`CREATE AGGREGATE s1.my_sum(int) (SFUNC = priv_fn.secret_add, STYPE = int)`)

	// 2. Non-owner (bob) denied on ALTER/RENAME/SET SCHEMA/DROP for operator and aggregate.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`ALTER OPERATOR s1.##+ (int, int) SET (HASHES = true)`, "42501")
	requireErrCode(`ALTER OPERATOR s1.##~ (NONE, int) SET (HASHES = true)`, "42501")
	requireErrCode(`ALTER OPERATOR s1.##+ (int, int) SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER OPERATOR s1.##~ (NONE, int) SET SCHEMA s3`, "42501")
	requireErrCode(`DROP OPERATOR s1.##+ (int, int)`, "42501")
	requireErrCode(`DROP OPERATOR s1.##~ (NONE, int)`, "42501")
	requireErrCode(`ALTER AGGREGATE s1.my_sum(int) RENAME TO my_sum_stolen`, "42501")
	requireErrCode(`ALTER AGGREGATE s1.my_sum(int) SET SCHEMA s3`, "42501")
	requireErrCode(`DROP AGGREGATE s1.my_sum(int)`, "42501")

	// 3. Owner (alice) denied SET SCHEMA to s2 (no CREATE), succeeds on ALTER/RENAME/SET SCHEMA s3/DROP.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`ALTER OPERATOR s1.##+ (int, int) SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER AGGREGATE s1.my_sum(int) SET SCHEMA s2`, "42501")

	mustExec(`ALTER OPERATOR s1.##+ (int, int) SET (HASHES = true)`)
	mustExec(`ALTER OPERATOR s1.##~ (NONE, int) SET (HASHES = true)`)
	mustExec(`ALTER OPERATOR s1.##+ (int, int) SET SCHEMA s3`)
	mustExec(`ALTER OPERATOR s1.##~ (NONE, int) SET SCHEMA s3`)
	mustExec(`DROP OPERATOR s3.##+ (int, int)`)
	mustExec(`DROP OPERATOR s3.##~ (NONE, int)`)

	mustExec(`ALTER AGGREGATE s1.my_sum(int) RENAME TO my_sum2`)
	mustExec(`ALTER AGGREGATE s1.my_sum2(int) SET SCHEMA s3`)
	mustExec(`DROP AGGREGATE s3.my_sum2(int)`)

	// 4. CREATE CAST / DROP CAST authorization.
	// Binary-coercible cast (WITHOUT FUNCTION) is superuser-only even when alice owns both types.
	requireErrCode(`CREATE CAST (s1.alice_enum AS s1.alice_enum2) WITHOUT FUNCTION`, "42501")

	// Non-owner bob cannot create cast between alice's types or between builtins.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`CREATE CAST (s1.alice_enum AS text) WITH FUNCTION s1.enum_to_text(s1.alice_enum)`, "42501")
	requireErrCode(`CREATE CAST (s1.alice_enum AS text) WITH INOUT`, "42501")
	requireErrCode(`CREATE CAST (int4 AS s1.alice_comp) WITH FUNCTION s1.int_to_comp(int4)`, "42501")
	requireErrCode(`CREATE CAST (bool AS bytea) WITH INOUT`, "42501")

	// Alice owns source type (alice_enum -> text) and target type (int4 -> alice_comp).
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE CAST (s1.alice_enum AS text) WITH FUNCTION s1.enum_to_text(s1.alice_enum)`)
	mustExec(`CREATE CAST (int4 AS s1.alice_comp) WITH FUNCTION s1.int_to_comp(int4)`)
	mustExec(`CREATE CAST (s1.alice_enum2 AS text) WITH INOUT`)

	// Bob cannot drop alice's casts.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`DROP CAST (s1.alice_enum AS text)`, "42501")
	requireErrCode(`DROP CAST (int4 AS s1.alice_comp)`, "42501")
	requireErrCode(`DROP CAST (s1.alice_enum2 AS text)`, "42501")

	// Superuser can create binary-coercible cast, and alice cannot drop superuser's cast.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE CAST (s1.alice_enum AS s1.alice_enum2) WITHOUT FUNCTION`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`DROP CAST (s1.alice_enum AS s1.alice_enum2)`, "42501")
	// Alice can drop her own casts.
	mustExec(`DROP CAST (s1.alice_enum AS text)`)
	mustExec(`DROP CAST (int4 AS s1.alice_comp)`)
	mustExec(`DROP CAST (s1.alice_enum2 AS text)`)
	mustExec(`RESET ROLE`)
	mustExec(`DROP CAST (s1.alice_enum AS s1.alice_enum2)`)
}

func TestPhase4DDLOwnershipTextSearch(t *testing.T) {
	mustExec, requireErrCode := openPhase4DDLOwnershipDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA s2`)
	mustExec(`CREATE SCHEMA s3`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)
	mustExec(`GRANT USAGE ON SCHEMA s2 TO alice`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s3 TO alice, bob`)

	// Alice cannot create text search objects in s2 (no CREATE).
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE TEXT SEARCH DICTIONARY s2.d_denied (TEMPLATE = simple)`, "42501")
	requireErrCode(`CREATE TEXT SEARCH CONFIGURATION s2.c_denied (PARSER = default)`, "42501")

	// Alice creates dictionary and configuration in s1.
	mustExec(`CREATE TEXT SEARCH DICTIONARY s1.my_dict (TEMPLATE = simple)`)
	mustExec(`CREATE TEXT SEARCH CONFIGURATION s1.my_cfg (PARSER = default)`)

	// Bob (non-owner) is denied on ALTER, RENAME, SET SCHEMA, and DROP.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`ALTER TEXT SEARCH DICTIONARY s1.my_dict (Accept = false)`, "42501")
	requireErrCode(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg ADD MAPPING FOR asciiword WITH s1.my_dict`, "42501")
	requireErrCode(`ALTER TEXT SEARCH DICTIONARY s1.my_dict RENAME TO my_dict_stolen`, "42501")
	requireErrCode(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg RENAME TO my_cfg_stolen`, "42501")
	requireErrCode(`ALTER TEXT SEARCH DICTIONARY s1.my_dict SET SCHEMA s3`, "42501")
	requireErrCode(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg SET SCHEMA s3`, "42501")
	requireErrCode(`DROP TEXT SEARCH CONFIGURATION s1.my_cfg`, "42501")
	requireErrCode(`DROP TEXT SEARCH DICTIONARY s1.my_dict`, "42501")

	// Alice (owner) is denied SET SCHEMA without destination CREATE (s2), succeeds on all operations with s3.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	requireErrCode(`ALTER TEXT SEARCH DICTIONARY s1.my_dict SET SCHEMA s2`, "42501")
	requireErrCode(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg SET SCHEMA s2`, "42501")

	mustExec(`ALTER TEXT SEARCH DICTIONARY s1.my_dict (Accept = false)`)
	mustExec(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg ADD MAPPING FOR asciiword WITH s1.my_dict`)
	mustExec(`ALTER TEXT SEARCH DICTIONARY s1.my_dict RENAME TO my_dict2`)
	mustExec(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg RENAME TO my_cfg2`)
	mustExec(`ALTER TEXT SEARCH DICTIONARY s1.my_dict2 SET SCHEMA s3`)
	mustExec(`ALTER TEXT SEARCH CONFIGURATION s1.my_cfg2 SET SCHEMA s3`)
	mustExec(`DROP TEXT SEARCH CONFIGURATION s3.my_cfg2`)
	mustExec(`DROP TEXT SEARCH DICTIONARY s3.my_dict2`)
}

func TestPhase4DDLOwnershipCommentOn(t *testing.T) {
	mustExec, requireErrCode := openPhase4DDLOwnershipDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1 AUTHORIZATION alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)

	// Alice creates one of every commentable object kind in s1.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE s1.t (id int PRIMARY KEY, val text)`)
	mustExec(`CREATE INDEX idx_t_val ON s1.t (val)`)
	mustExec(`CREATE VIEW s1.v AS SELECT id, val FROM s1.t`)
	mustExec(`CREATE MATERIALIZED VIEW s1.mv AS SELECT id, val FROM s1.t`)
	mustExec(`CREATE SEQUENCE s1.seq`)
	mustExec(`CREATE TYPE s1.e AS ENUM ('a', 'b')`)
	mustExec(`CREATE DOMAIN s1.d AS int CONSTRAINT d_pos CHECK (VALUE > 0)`)
	mustExec(`CREATE FUNCTION s1.fn(x int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT x + 1'`)
	mustExec(`CREATE FUNCTION s1.add2(a int, b int) RETURNS int LANGUAGE sql IMMUTABLE AS 'SELECT a + b'`)
	mustExec(`CREATE OPERATOR s1.##+ (LEFTARG = int, RIGHTARG = int, FUNCTION = s1.add2)`)
	mustExec(`CREATE AGGREGATE s1.agg(int) (SFUNC = s1.add2, STYPE = int)`)
	mustExec(`CREATE CAST (s1.e AS text) WITH INOUT`)
	mustExec(`CREATE STATISTICS s1.st (ndistinct) ON id, val FROM s1.t`)
	mustExec(`CREATE TEXT SEARCH DICTIONARY s1.dict (TEMPLATE = simple)`)
	mustExec(`CREATE TEXT SEARCH CONFIGURATION s1.cfg (PARSER = default)`)

	// Bob (non-owner) is denied COMMENT ON for every object kind.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	for _, stmt := range []string{
		`COMMENT ON SCHEMA s1 IS 'bob'`,
		`COMMENT ON TABLE s1.t IS 'bob'`,
		`COMMENT ON COLUMN s1.t.val IS 'bob'`,
		`COMMENT ON INDEX s1.idx_t_val IS 'bob'`,
		`COMMENT ON VIEW s1.v IS 'bob'`,
		`COMMENT ON MATERIALIZED VIEW s1.mv IS 'bob'`,
		`COMMENT ON SEQUENCE s1.seq IS 'bob'`,
		`COMMENT ON TYPE s1.e IS 'bob'`,
		`COMMENT ON DOMAIN s1.d IS 'bob'`,
		`COMMENT ON CONSTRAINT d_pos ON DOMAIN s1.d IS 'bob'`,
		`COMMENT ON FUNCTION s1.fn(int) IS 'bob'`,
		`COMMENT ON OPERATOR s1.##+ (int, int) IS 'bob'`,
		`COMMENT ON AGGREGATE s1.agg(int) IS 'bob'`,
		`COMMENT ON CAST (s1.e AS text) IS 'bob'`,
		`COMMENT ON STATISTICS s1.st IS 'bob'`,
		`COMMENT ON TEXT SEARCH DICTIONARY s1.dict IS 'bob'`,
		`COMMENT ON TEXT SEARCH CONFIGURATION s1.cfg IS 'bob'`,
	} {
		requireErrCode(stmt, "42501")
	}

	// Alice (owner) succeeds on COMMENT ON for every object kind.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	for _, stmt := range []string{
		`COMMENT ON SCHEMA s1 IS 'alice'`,
		`COMMENT ON TABLE s1.t IS 'alice'`,
		`COMMENT ON COLUMN s1.t.val IS 'alice'`,
		`COMMENT ON INDEX s1.idx_t_val IS 'alice'`,
		`COMMENT ON VIEW s1.v IS 'alice'`,
		`COMMENT ON MATERIALIZED VIEW s1.mv IS 'alice'`,
		`COMMENT ON SEQUENCE s1.seq IS 'alice'`,
		`COMMENT ON TYPE s1.e IS 'alice'`,
		`COMMENT ON DOMAIN s1.d IS 'alice'`,
		`COMMENT ON CONSTRAINT d_pos ON DOMAIN s1.d IS 'alice'`,
		`COMMENT ON FUNCTION s1.fn(int) IS 'alice'`,
		`COMMENT ON OPERATOR s1.##+ (int, int) IS 'alice'`,
		`COMMENT ON AGGREGATE s1.agg(int) IS 'alice'`,
		`COMMENT ON CAST (s1.e AS text) IS 'alice'`,
		`COMMENT ON STATISTICS s1.st IS 'alice'`,
		`COMMENT ON TEXT SEARCH DICTIONARY s1.dict IS 'alice'`,
		`COMMENT ON TEXT SEARCH CONFIGURATION s1.cfg IS 'alice'`,
	} {
		mustExec(stmt)
	}
}
