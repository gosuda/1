package supersql

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func openPhase5FKDB(t *testing.T) (*Catalog, func(string) outcome, func(string, string)) {
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

	return db, mustExec, requireErrCode
}

func requireIntRows(t *testing.T, out outcome, want [][]int64) {
	t.Helper()
	if len(out.Rows) != len(want) {
		t.Fatalf("row count = %d, want %d (rows=%+v)", len(out.Rows), len(want), out.Rows)
	}
	for i := range want {
		if len(out.Rows[i]) != len(want[i]) {
			t.Fatalf("row %d col count = %d, want %d", i, len(out.Rows[i]), len(want[i]))
		}
		for j := range want[i] {
			if out.Rows[i][j].IsNull {
				t.Fatalf("row %d col %d = NULL, want %d", i, j, want[i][j])
			}
			if out.Rows[i][j].IntVal != want[i][j] {
				t.Fatalf("row %d col %d = %d, want %d", i, j, out.Rows[i][j].IntVal, want[i][j])
			}
		}
	}
}

func TestPhase5FKParentNoActionAndRestrict(t *testing.T) {
	_, mustExec, requireErrCode := openPhase5FKDB(t)

	// Parent has NO outgoing FK of its own; child holds the FK.
	mustExec(`CREATE TABLE parents (id int PRIMARY KEY, label text NOT NULL DEFAULT '')`)
	mustExec(`CREATE TABLE children_na (id int PRIMARY KEY, pid int REFERENCES parents(id) ON DELETE NO ACTION ON UPDATE NO ACTION)`)
	mustExec(`CREATE TABLE children_r (id int PRIMARY KEY, pid int REFERENCES parents(id) ON DELETE RESTRICT ON UPDATE RESTRICT)`)

	mustExec(`INSERT INTO parents (id, label) VALUES (1, 'unreferenced'), (2, 'na_ref'), (3, 'r_ref')`)
	mustExec(`INSERT INTO children_na VALUES (10, 2)`)
	mustExec(`INSERT INTO children_r VALUES (20, 3)`)

	// Immediate NO ACTION: DELETE and key UPDATE on referenced parent must fail with 23503.
	requireErrCode(`DELETE FROM parents WHERE id = 2`, "23503")
	requireErrCode(`UPDATE parents SET id = 99 WHERE id = 2`, "23503")

	// Immediate RESTRICT: DELETE and key UPDATE on referenced parent must fail with 23503.
	requireErrCode(`DELETE FROM parents WHERE id = 3`, "23503")
	requireErrCode(`UPDATE parents SET id = 98 WHERE id = 3`, "23503")

	// Updating a non-referenced column on a referenced parent succeeds.
	mustExec(`UPDATE parents SET label = 'updated' WHERE id = 2`)

	// Deleting an unreferenced parent succeeds.
	mustExec(`DELETE FROM parents WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{2}, {3}})

	// After removing the referencing child, parent deletion succeeds.
	mustExec(`DELETE FROM children_na WHERE id = 10`)
	mustExec(`DELETE FROM parents WHERE id = 2`)
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{3}})

	// Nullable UNIQUE referenced column: deleting a parent row whose key is NULL succeeds.
	mustExec(`CREATE TABLE u_parents (id int PRIMARY KEY, code int UNIQUE)`)
	mustExec(`CREATE TABLE u_children (id int PRIMARY KEY, pcode int REFERENCES u_parents(code))`)
	mustExec(`INSERT INTO u_parents VALUES (1, NULL), (2, 100)`)
	mustExec(`INSERT INTO u_children VALUES (10, 100)`)
	mustExec(`DELETE FROM u_parents WHERE id = 1`)
	requireErrCode(`DELETE FROM u_parents WHERE id = 2`, "23503")
}

func TestPhase5FKParentDeferredConstraints(t *testing.T) {
	_, mustExec, requireErrCode := openPhase5FKDB(t)

	mustExec(`CREATE TABLE parents (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE children (id int PRIMARY KEY, pid int REFERENCES parents(id) DEFERRABLE INITIALLY DEFERRED)`)
	mustExec(`INSERT INTO parents VALUES (1), (2), (3)`)
	mustExec(`INSERT INTO children VALUES (10, 2), (20, 3)`)

	// 1. Autocommit DELETE on referenced parent fails with 23503 at statement commit.
	requireErrCode(`DELETE FROM parents WHERE id = 2`, "23503")
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{1}, {2}, {3}})

	// 2. Explicit transaction: DELETE succeeds inside BEGIN, fails with 23503 on COMMIT.
	mustExec(`BEGIN`)
	mustExec(`DELETE FROM parents WHERE id = 2`)
	requireErrCode(`COMMIT`, "23503")
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{1}, {2}, {3}})

	// 3. Explicit transaction: UPDATE parent key succeeds inside BEGIN, fails on COMMIT if child not updated.
	mustExec(`BEGIN`)
	mustExec(`UPDATE parents SET id = 200 WHERE id = 2`)
	requireErrCode(`COMMIT`, "23503")
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{1}, {2}, {3}})

	// 4. Explicit transaction: DELETE parent succeeds on COMMIT when child is deleted before COMMIT.
	mustExec(`BEGIN`)
	mustExec(`DELETE FROM parents WHERE id = 2`)
	mustExec(`DELETE FROM children WHERE id = 10`)
	mustExec(`COMMIT`)
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{1}, {3}})
	requireIntRows(t, mustExec(`SELECT id, pid FROM children ORDER BY id`), [][]int64{{20, 3}})

	// 5. Explicit transaction: UPDATE parent key succeeds on COMMIT when child is updated to match before COMMIT.
	mustExec(`BEGIN`)
	mustExec(`UPDATE parents SET id = 300 WHERE id = 3`)
	mustExec(`UPDATE children SET pid = 300 WHERE id = 20`)
	mustExec(`COMMIT`)
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{1}, {300}})
	requireIntRows(t, mustExec(`SELECT id, pid FROM children ORDER BY id`), [][]int64{{20, 300}})
}

func TestPhase5FKCascadeDeleteSingleMultiAndSelf(t *testing.T) {
	_, mustExec, _ := openPhase5FKDB(t)

	// 1. Single-hop ON DELETE CASCADE (parent has no outgoing FK).
	mustExec(`CREATE TABLE parents (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE children (id int PRIMARY KEY, pid int REFERENCES parents(id) ON DELETE CASCADE)`)
	mustExec(`INSERT INTO parents VALUES (1), (2)`)
	mustExec(`INSERT INTO children VALUES (10, 1), (11, 1), (20, 2)`)

	mustExec(`DELETE FROM parents WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM parents ORDER BY id`), [][]int64{{2}})
	requireIntRows(t, mustExec(`SELECT id, pid FROM children ORDER BY id`), [][]int64{{20, 2}})

	// 2. Multi-hop ON DELETE CASCADE: grandparent -> parent -> child.
	mustExec(`CREATE TABLE gp (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE p (id int PRIMARY KEY, gpid int REFERENCES gp(id) ON DELETE CASCADE)`)
	mustExec(`CREATE TABLE c (id int PRIMARY KEY, pid int REFERENCES p(id) ON DELETE CASCADE)`)
	mustExec(`INSERT INTO gp VALUES (1), (2)`)
	mustExec(`INSERT INTO p VALUES (10, 1), (20, 2)`)
	mustExec(`INSERT INTO c VALUES (100, 10), (101, 10), (200, 20)`)

	mustExec(`DELETE FROM gp WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM gp ORDER BY id`), [][]int64{{2}})
	requireIntRows(t, mustExec(`SELECT id, gpid FROM p ORDER BY id`), [][]int64{{20, 2}})
	requireIntRows(t, mustExec(`SELECT id, pid FROM c ORDER BY id`), [][]int64{{200, 20}})

	// 3. Self-referencing ON DELETE CASCADE (3-level tree).
	mustExec(`CREATE TABLE nodes (id int PRIMARY KEY, pid int REFERENCES nodes(id) ON DELETE CASCADE)`)
	mustExec(`INSERT INTO nodes VALUES (1, NULL), (2, 1), (3, 2), (4, NULL), (5, 4)`)

	mustExec(`DELETE FROM nodes WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM nodes ORDER BY id`), [][]int64{{4}, {5}})
}

func TestPhase5FKCascadeUpdateAndSetNullDefault(t *testing.T) {
	_, mustExec, _ := openPhase5FKDB(t)

	// 1. ON UPDATE CASCADE (single-hop and multi-hop).
	mustExec(`CREATE TABLE gp_u (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE p_u (id int PRIMARY KEY, gpid int UNIQUE REFERENCES gp_u(id) ON UPDATE CASCADE)`)
	mustExec(`CREATE TABLE c_u (id int PRIMARY KEY, pgpid int REFERENCES p_u(gpid) ON UPDATE CASCADE)`)
	mustExec(`INSERT INTO gp_u VALUES (1), (2)`)
	mustExec(`INSERT INTO p_u VALUES (10, 1), (20, 2)`)
	mustExec(`INSERT INTO c_u VALUES (100, 1), (200, 2)`)

	mustExec(`UPDATE gp_u SET id = 11 WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM gp_u ORDER BY id`), [][]int64{{2}, {11}})
	requireIntRows(t, mustExec(`SELECT id, gpid FROM p_u ORDER BY id`), [][]int64{{10, 11}, {20, 2}})
	requireIntRows(t, mustExec(`SELECT id, pgpid FROM c_u ORDER BY id`), [][]int64{{100, 11}, {200, 2}})

	// 2. ON DELETE SET NULL & ON UPDATE SET NULL.
	mustExec(`CREATE TABLE p_sn (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE c_sn (id int PRIMARY KEY, pid int REFERENCES p_sn(id) ON DELETE SET NULL ON UPDATE SET NULL)`)
	mustExec(`INSERT INTO p_sn VALUES (1), (2), (3)`)
	mustExec(`INSERT INTO c_sn VALUES (10, 1), (20, 2), (30, 3)`)

	mustExec(`DELETE FROM p_sn WHERE id = 1`)
	mustExec(`UPDATE p_sn SET id = 22 WHERE id = 2`)

	out := mustExec(`SELECT id, pid FROM c_sn ORDER BY id`)
	if len(out.Rows) != 3 {
		t.Fatalf("c_sn row count = %d, want 3", len(out.Rows))
	}
	if !out.Rows[0][1].IsNull {
		t.Fatalf("c_sn row 10 pid = %+v, want NULL after ON DELETE SET NULL", out.Rows[0][1])
	}
	if !out.Rows[1][1].IsNull {
		t.Fatalf("c_sn row 20 pid = %+v, want NULL after ON UPDATE SET NULL", out.Rows[1][1])
	}
	if out.Rows[2][1].IsNull || out.Rows[2][1].IntVal != 3 {
		t.Fatalf("c_sn row 30 pid = %+v, want 3", out.Rows[2][1])
	}

	// 3. ON DELETE SET DEFAULT & ON UPDATE SET DEFAULT.
	mustExec(`CREATE TABLE p_sd (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE c_sd (id int PRIMARY KEY, pid int DEFAULT 0 REFERENCES p_sd(id) ON DELETE SET DEFAULT ON UPDATE SET DEFAULT)`)
	mustExec(`INSERT INTO p_sd VALUES (0), (1), (2), (3)`)
	mustExec(`INSERT INTO c_sd VALUES (10, 1), (20, 2), (30, 3)`)

	mustExec(`DELETE FROM p_sd WHERE id = 1`)
	mustExec(`UPDATE p_sd SET id = 22 WHERE id = 2`)
	requireIntRows(t, mustExec(`SELECT id, pid FROM c_sd ORDER BY id`), [][]int64{{10, 0}, {20, 0}, {30, 3}})
}

func TestPhase5FKCascadeBeforeTriggers(t *testing.T) {
	_, mustExec, _ := openPhase5FKDB(t)

	// 1. Self-referencing ON DELETE CASCADE with BEFORE DELETE trigger:
	//    - Returning NULL skips the cascade deletion for that child row (!proceed).
	//    - Returning OLD allows the cascade deletion to proceed (proceed).
	mustExec(`CREATE TABLE trig_nodes_del (id int PRIMARY KEY, pid int REFERENCES trig_nodes_del(id) ON DELETE CASCADE, keep boolean NOT NULL)`)
	mustExec(`CREATE TABLE trig_del_log (cid int PRIMARY KEY)`)
	mustExec(`CREATE FUNCTION trig_node_before_del() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		INSERT INTO trig_del_log VALUES (OLD.id);
		IF OLD.keep THEN
			RETURN NULL;
		END IF;
		RETURN OLD;
	END;
	$$`)
	mustExec(`CREATE TRIGGER trg_node_before_del BEFORE DELETE ON trig_nodes_del FOR EACH ROW EXECUTE FUNCTION trig_node_before_del()`)

	mustExec(`INSERT INTO trig_nodes_del VALUES (1, NULL, false), (10, 1, false), (11, 1, true), (2, NULL, false), (20, 2, false)`)

	mustExec(`DELETE FROM trig_nodes_del WHERE id = 1`)
	// Row 10 (keep=false -> RETURN OLD) must be deleted; row 11 (keep=true -> RETURN NULL) must survive.
	requireIntRows(t, mustExec(`SELECT id FROM trig_nodes_del ORDER BY id`), [][]int64{{2}, {11}, {20}})
	requireIntRows(t, mustExec(`SELECT cid FROM trig_del_log ORDER BY cid`), [][]int64{{1}, {10}, {11}})

	// 2. Self-referencing ON UPDATE CASCADE with BEFORE UPDATE trigger:
	//    - Returning NULL skips the cascade update for that child row (!proceed).
	//    - Returning NEW (with a modification) allows the cascade update to proceed and persists NEW's changes.
	mustExec(`CREATE TABLE trig_nodes_upd (id int PRIMARY KEY, pid int REFERENCES trig_nodes_upd(id) ON UPDATE CASCADE, tag text NOT NULL, locked boolean NOT NULL)`)
	mustExec(`CREATE FUNCTION trig_node_before_upd() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF OLD.locked THEN
			RETURN NULL;
		END IF;
		IF OLD.pid IS NOT NULL THEN
			NEW.tag := 'cascaded';
		END IF;
		RETURN NEW;
	END;
	$$`)
	mustExec(`CREATE TRIGGER trg_node_before_upd BEFORE UPDATE ON trig_nodes_upd FOR EACH ROW EXECUTE FUNCTION trig_node_before_upd()`)

	mustExec(`INSERT INTO trig_nodes_upd VALUES (1, NULL, 'root', false), (10, 1, 'orig', false), (11, 1, 'orig', true), (2, NULL, 'root2', false), (20, 2, 'orig', false)`)

	mustExec(`UPDATE trig_nodes_upd SET id = 99 WHERE id = 1`)

	out := mustExec(`SELECT id, pid, tag FROM trig_nodes_upd WHERE id IN (10, 11, 20) ORDER BY id`)
	if len(out.Rows) != 3 {
		t.Fatalf("trig_nodes_upd child row count = %d, want 3", len(out.Rows))
	}
	checkRow := func(rows [][]model.Value, idx int, wantID, wantPID int64, wantTag string) {
		t.Helper()
		row := rows[idx]
		if row[0].IntVal != wantID || row[1].IntVal != wantPID || row[2].StrVal != wantTag {
			t.Fatalf("row %d = (%d, %d, %q), want (%d, %d, %q)", idx, row[0].IntVal, row[1].IntVal, row[2].StrVal, wantID, wantPID, wantTag)
		}
	}
	checkRow(out.Rows, 0, 10, 99, "cascaded")
	checkRow(out.Rows, 1, 11, 1, "orig")
	checkRow(out.Rows, 2, 20, 2, "orig")

	// 3. Separate parent + child tables with BEFORE DELETE / BEFORE UPDATE triggers
	//    (referencing child in a no-op CTE so routine_execution pins child's trigger routine).
	mustExec(`CREATE TABLE trig_p (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE trig_c (id int PRIMARY KEY, pid int REFERENCES trig_p(id) ON DELETE CASCADE ON UPDATE CASCADE, tag text NOT NULL, keep boolean NOT NULL)`)
	mustExec(`CREATE FUNCTION trig_c_before_del() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF OLD.keep THEN
			RETURN NULL;
		END IF;
		RETURN OLD;
	END;
	$$`)
	mustExec(`CREATE FUNCTION trig_c_before_upd() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF OLD.keep THEN
			RETURN NULL;
		END IF;
		NEW.tag := 'child_cascaded';
		RETURN NEW;
	END;
	$$`)
	mustExec(`CREATE TRIGGER trg_c_before_del BEFORE DELETE ON trig_c FOR EACH ROW EXECUTE FUNCTION trig_c_before_del()`)
	mustExec(`CREATE TRIGGER trg_c_before_upd BEFORE UPDATE ON trig_c FOR EACH ROW EXECUTE FUNCTION trig_c_before_upd()`)

	mustExec(`INSERT INTO trig_p VALUES (1), (2)`)
	mustExec(`INSERT INTO trig_c VALUES (10, 1, 'orig', false), (11, 1, 'orig', true), (20, 2, 'orig', false), (21, 2, 'orig', true)`)

	mustExec(`WITH _pin AS (SELECT id FROM trig_c WHERE false) DELETE FROM trig_p WHERE id = 1`)
	requireIntRows(t, mustExec(`SELECT id FROM trig_c WHERE id IN (10, 11) ORDER BY id`), [][]int64{{11}})

	mustExec(`WITH _pin AS (SELECT id FROM trig_c WHERE false) UPDATE trig_p SET id = 22 WHERE id = 2`)
	out2 := mustExec(`SELECT id, pid, tag FROM trig_c WHERE id IN (20, 21) ORDER BY id`)
	if len(out2.Rows) != 2 {
		t.Fatalf("trig_c updated row count = %d, want 2", len(out2.Rows))
	}
	checkRow(out2.Rows, 0, 20, 22, "child_cascaded")
	checkRow(out2.Rows, 1, 21, 2, "orig")
}

func TestPhase5FKAlterTableAndTempTables(t *testing.T) {
	_, mustExec, requireErrCode := openPhase5FKDB(t)

	// 1. Adding an FK via ALTER TABLE after initial DML on parent must invalidate
	//    the relational graph cache and enforce parent-side DELETE/UPDATE immediately.
	mustExec(`CREATE TABLE late_p (id int PRIMARY KEY)`)
	mustExec(`CREATE TABLE late_c (id int PRIMARY KEY, pid int)`)
	mustExec(`INSERT INTO late_p VALUES (1), (2), (3)`)
	mustExec(`INSERT INTO late_c VALUES (10, 2)`)

	// Warm the cache while late_p has no incoming FK.
	mustExec(`DELETE FROM late_p WHERE id = 1`)

	// Now add incoming FK on late_c referencing late_p.
	mustExec(`ALTER TABLE late_c ADD CONSTRAINT fk_late FOREIGN KEY (pid) REFERENCES late_p(id)`)

	// Parent DELETE and key UPDATE must now be rejected with 23503.
	requireErrCode(`DELETE FROM late_p WHERE id = 2`, "23503")
	requireErrCode(`UPDATE late_p SET id = 99 WHERE id = 2`, "23503")
	mustExec(`DELETE FROM late_p WHERE id = 3`)

	// 2. Temporary tables referencing temporary tables: parent-side DELETE/UPDATE
	//    must enforce incoming FKs and cascades.
	mustExec(`CREATE TEMP TABLE tp (id int PRIMARY KEY)`)
	mustExec(`CREATE TEMP TABLE tc_restrict (id int PRIMARY KEY, pid int REFERENCES tp(id) ON DELETE RESTRICT)`)
	mustExec(`CREATE TEMP TABLE tc_cascade (id int PRIMARY KEY, pid int REFERENCES tp(id) ON DELETE CASCADE ON UPDATE CASCADE)`)
	mustExec(`INSERT INTO tp VALUES (1), (2), (3)`)
	mustExec(`INSERT INTO tc_restrict VALUES (10, 1)`)
	mustExec(`INSERT INTO tc_cascade VALUES (20, 2), (30, 3)`)

	requireErrCode(`DELETE FROM tp WHERE id = 1`, "23503")
	mustExec(`DELETE FROM tp WHERE id = 2`)
	mustExec(`UPDATE tp SET id = 33 WHERE id = 3`)
	requireIntRows(t, mustExec(`SELECT id FROM tp ORDER BY id`), [][]int64{{1}, {33}})
	requireIntRows(t, mustExec(`SELECT id, pid FROM tc_cascade ORDER BY id`), [][]int64{{30, 33}})
}
