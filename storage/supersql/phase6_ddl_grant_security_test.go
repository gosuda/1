package supersql

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func openPhase6DDLGrantDB(t *testing.T) (func(string) outcome, func(string, string)) {
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

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}

	requireErrCode := func(sqlText, code string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, code, got, err)
		}
	}

	return mustExec, requireErrCode
}

func TestPhase6DDLGrantDropIndexConcurrently(t *testing.T) {
	mustExec, requireErrCode := openPhase6DDLGrantDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1 AUTHORIZATION alice`)
	mustExec(`CREATE SCHEMA priv AUTHORIZATION alice`)
	mustExec(`GRANT USAGE ON SCHEMA s1 TO bob`)

	// Alice owns tables, a regular index, and a standalone UNIQUE index in s1 and priv.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE s1.reg_tbl (id int PRIMARY KEY, tag int)`)
	mustExec(`CREATE INDEX idx_reg_tag ON s1.reg_tbl (tag)`)
	mustExec(`CREATE TABLE s1.uniq_tbl (id int PRIMARY KEY, code int)`)
	mustExec(`CREATE UNIQUE INDEX uidx_uniq_code ON s1.uniq_tbl (code)`)
	mustExec(`CREATE TABLE priv.hidden (id int PRIMARY KEY, v int)`)
	mustExec(`CREATE INDEX idx_hidden_v ON priv.hidden (v)`)
	mustExec(`GRANT ALL ON TABLE s1.reg_tbl, s1.uniq_tbl TO bob`)

	// Bob (non-owner) must fail 42501 on DROP INDEX CONCURRENTLY for both regular and UNIQUE indexes.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`DROP INDEX CONCURRENTLY s1.idx_reg_tag`, "42501")
	requireErrCode(`DROP INDEX CONCURRENTLY s1.uidx_uniq_code`, "42501")
	// Without USAGE on priv, qualified DROP INDEX CONCURRENTLY fails 42501 even for nonexistent indexes (anti-probing).
	requireErrCode(`DROP INDEX CONCURRENTLY priv.idx_hidden_v`, "42501")
	requireErrCode(`DROP INDEX CONCURRENTLY priv.does_not_exist`, "42501")
	requireErrCode(`DROP INDEX CONCURRENTLY IF EXISTS priv.does_not_exist`, "42501")

	// Alice (owner) succeeds dropping both regular and standalone UNIQUE indexes concurrently.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE alice`)
	mustExec(`DROP INDEX CONCURRENTLY s1.idx_reg_tag`)
	mustExec(`DROP INDEX CONCURRENTLY s1.uidx_uniq_code`)
	mustExec(`DROP INDEX CONCURRENTLY priv.idx_hidden_v`)
}

func TestPhase6DDLGrantAlterSequenceAndOwnedBy(t *testing.T) {
	mustExec, requireErrCode := openPhase6DDLGrantDB(t)

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE SCHEMA s1`)
	mustExec(`CREATE SCHEMA priv AUTHORIZATION alice`)
	mustExec(`GRANT USAGE, CREATE ON SCHEMA s1 TO alice, bob`)

	// Alice creates a sequence and table in s1 and a sequence in priv.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE s1.alice_tbl (id int PRIMARY KEY)`)
	mustExec(`CREATE SEQUENCE s1.alice_seq START 1`)
	mustExec(`CREATE SEQUENCE priv.secret_seq START 1`)
	mustExec(`GRANT ALL ON SEQUENCE s1.alice_seq TO bob`)

	// Bob creates his own table and sequence in s1.
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	mustExec(`CREATE TABLE s1.bob_tbl (id int PRIMARY KEY)`)
	mustExec(`CREATE SEQUENCE s1.bob_seq START 1`)

	// Non-owner bob cannot ALTER alice's sequence with any option.
	requireErrCode(`ALTER SEQUENCE s1.alice_seq RESTART WITH 10`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.alice_seq MAXVALUE 1000`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.alice_seq INCREMENT BY 5`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.alice_seq AS integer`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.alice_seq OWNED BY NONE`, "42501")
	requireErrCode(`ALTER SEQUENCE s1.alice_seq OWNED BY s1.bob_tbl.id`, "42501")

	// Without USAGE on priv, ALTER SEQUENCE fails 42501 even with IF EXISTS on a nonexistent name.
	requireErrCode(`ALTER SEQUENCE priv.secret_seq RESTART`, "42501")
	requireErrCode(`ALTER SEQUENCE IF EXISTS priv.no_such_seq RESTART`, "42501")

	// Owner of sequence (bob) cannot attach OWNED BY to alice's table -> 42501.
	requireErrCode(`ALTER SEQUENCE s1.bob_seq OWNED BY s1.alice_tbl.id`, "42501")
	// Without USAGE on priv, OWNED BY referencing priv fails 42501.
	requireErrCode(`ALTER SEQUENCE s1.bob_seq OWNED BY priv.no_such_tbl.id`, "42501")

	// Owner of both bob_seq and bob_tbl succeeds on all ALTER SEQUENCE operations and OWNED BY.
	mustExec(`ALTER SEQUENCE s1.bob_seq AS integer INCREMENT BY 2 MAXVALUE 500 RESTART WITH 10`)
	mustExec(`ALTER SEQUENCE s1.bob_seq OWNED BY s1.bob_tbl.id`)
	mustExec(`ALTER SEQUENCE s1.bob_seq OWNED BY NONE`)

	// Even superuser cannot link a sequence owned by bob to a table owned by alice -> 55000.
	mustExec(`RESET ROLE`)
	requireErrCode(`ALTER SEQUENCE s1.bob_seq OWNED BY s1.alice_tbl.id`, "55000")

	// Alice (owner of both alice_seq and alice_tbl) succeeds.
	mustExec(`SET ROLE alice`)
	mustExec(`ALTER SEQUENCE s1.alice_seq AS integer INCREMENT BY 3 MAXVALUE 900 RESTART WITH 5`)
	mustExec(`ALTER SEQUENCE s1.alice_seq OWNED BY s1.alice_tbl.id`)
	mustExec(`ALTER SEQUENCE s1.alice_seq OWNED BY NONE`)
}

func TestPhase6DDLGrantRevokeCascadeRestrict(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sqlText string) outcome {
		t.Helper()
		out, err := db.exec(ctx, sqlText, 0)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", sqlText, err)
		}
		return out
	}

	requireErrCode := func(sqlText, code string) {
		t.Helper()
		_, err := db.exec(ctx, sqlText, 0)
		if err == nil {
			t.Fatalf("%s: expected SQLSTATE %s, got nil", sqlText, code)
		}
		if got := SQLState(err); got != code {
			t.Fatalf("%s: expected SQLSTATE %s, got %q (%v)", sqlText, code, got, err)
		}
	}

	mustExec(`CREATE ROLE alice`)
	mustExec(`CREATE ROLE bob`)
	mustExec(`CREATE ROLE carol`)
	mustExec(`CREATE TABLE t (id int PRIMARY KEY)`)
	mustExec(`INSERT INTO t VALUES (1)`)

	// Build delegation chain: owner -> alice (WGO) -> bob (WGO) -> carol.
	mustExec(`GRANT SELECT ON t TO alice WITH GRANT OPTION`)
	mustExec(`SET ROLE alice`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	mustExec(`GRANT SELECT ON t TO carol`)
	mustExec(`RESET ROLE`)

	// Verify hint and 2BP01 on RESTRICT and default (unspecified) REVOKE.
	for _, stmt := range []string{
		`REVOKE SELECT ON t FROM alice RESTRICT`,
		`REVOKE SELECT ON t FROM alice`,
		`REVOKE GRANT OPTION FOR SELECT ON t FROM alice RESTRICT`,
		`REVOKE GRANT OPTION FOR SELECT ON t FROM alice`,
	} {
		_, err := db.exec(ctx, stmt, 0)
		var mErr *model.Error
		if !errors.As(err, &mErr) || mErr.Code != "2BP01" {
			t.Fatalf("%s: got %v, want SQLSTATE 2BP01", stmt, err)
		}
		if mErr.Hint != "Use CASCADE to revoke them too." {
			t.Fatalf("%s: hint = %q, want %q", stmt, mErr.Hint, "Use CASCADE to revoke them too.")
		}
	}

	// All three still hold SELECT after failed RESTRICT revokes.
	for _, role := range []string{"alice", "bob", "carol"} {
		mustExec(`SET ROLE ` + role)
		mustExec(`SELECT count(*) FROM t`)
		mustExec(`RESET ROLE`)
	}

	// REVOKE GRANT OPTION FOR SELECT ... CASCADE revokes bob and carol while keeping alice's base SELECT.
	mustExec(`REVOKE GRANT OPTION FOR SELECT ON t FROM alice CASCADE`)
	mustExec(`SET ROLE alice`)
	mustExec(`SELECT count(*) FROM t`)
	requireErrCode(`GRANT SELECT ON t TO bob`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	requireErrCode(`SELECT count(*) FROM t`, "42501")
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE carol`)
	requireErrCode(`SELECT count(*) FROM t`, "42501")
	mustExec(`RESET ROLE`)

	// Re-establish chain and test REVOKE SELECT ... CASCADE revokes alice, bob, AND carol cleanly.
	mustExec(`GRANT SELECT ON t TO alice WITH GRANT OPTION`)
	mustExec(`SET ROLE alice`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	mustExec(`GRANT SELECT ON t TO carol`)
	mustExec(`RESET ROLE`)

	mustExec(`REVOKE SELECT ON t FROM alice CASCADE`)
	for _, role := range []string{"alice", "bob", "carol"} {
		mustExec(`SET ROLE ` + role)
		requireErrCode(`SELECT count(*) FROM t`, "42501")
		mustExec(`RESET ROLE`)
	}

	// Independent grantor: bob holds GrantOption from BOTH owner and alice, and delegates to carol.
	// When alice revokes her grant to bob with RESTRICT, bob still holds GrantOption from owner -> succeeds!
	mustExec(`GRANT SELECT ON t TO alice WITH GRANT OPTION`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`SET ROLE alice`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	mustExec(`GRANT SELECT ON t TO carol`)
	mustExec(`RESET ROLE`)

	mustExec(`SET ROLE alice`)
	mustExec(`REVOKE SELECT ON t FROM bob RESTRICT`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE carol`)
	mustExec(`SELECT count(*) FROM t`)
	mustExec(`RESET ROLE`)

	// Clean up before cycle test.
	mustExec(`REVOKE SELECT ON t FROM alice, bob, carol CASCADE`)

	// Cycle test: owner -> alice (WGO) -> bob (WGO) -> carol (WGO) -> bob (WGO).
	mustExec(`GRANT SELECT ON t TO alice WITH GRANT OPTION`)
	mustExec(`SET ROLE alice`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE bob`)
	mustExec(`GRANT SELECT ON t TO carol WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)
	mustExec(`SET ROLE carol`)
	mustExec(`GRANT SELECT ON t TO bob WITH GRANT OPTION`)
	mustExec(`RESET ROLE`)

	requireErrCode(`REVOKE SELECT ON t FROM alice RESTRICT`, "2BP01")
	mustExec(`REVOKE SELECT ON t FROM alice CASCADE`)
	for _, role := range []string{"alice", "bob", "carol"} {
		mustExec(`SET ROLE ` + role)
		requireErrCode(`SELECT count(*) FROM t`, "42501")
		mustExec(`RESET ROLE`)
	}
}

func TestPhase6DDLGrantCreateSchemaPrivilege(t *testing.T) {
	mustExec, requireErrCode := openPhase6DDLGrantDB(t)

	mustExec(`CREATE ROLE alice`)

	// Non-superuser cannot CREATE SCHEMA (plain, IF NOT EXISTS, or AUTHORIZATION).
	mustExec(`SET ROLE alice`)
	requireErrCode(`CREATE SCHEMA rogue_schema`, "42501")
	requireErrCode(`CREATE SCHEMA IF NOT EXISTS public`, "42501")
	requireErrCode(`CREATE SCHEMA AUTHORIZATION alice`, "42501")

	// Superuser succeeds on CREATE SCHEMA and CREATE SCHEMA ... AUTHORIZATION.
	mustExec(`RESET ROLE`)
	mustExec(`CREATE SCHEMA admin_schema`)
	mustExec(`CREATE SCHEMA alice_schema AUTHORIZATION alice`)

	// Verify alice owns alice_schema.
	mustExec(`SET ROLE alice`)
	mustExec(`CREATE TABLE alice_schema.t (id int PRIMARY KEY)`)
}
