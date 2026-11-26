package supersql

import (
	"testing"
)

// TestPhase5ModifyTriggerMergeInsteadOfView verifies that MERGE targeting a
// view with INSTEAD OF INSERT and INSTEAD OF UPDATE triggers preserves the
// MERGE action's assigned values in NEW rather than dropping them due to empty
// statement-level assignment columns.
func TestPhase5ModifyTriggerMergeInsteadOfView(t *testing.T) {
	db, _ := openTestDB(t)
	exec := func(q string) int64 {
		t.Helper()
		res, err := db.Exec(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("%s: RowsAffected: %v", q, err)
		}
		return n
	}

	exec(`CREATE TABLE base_items (id int PRIMARY KEY, val int NOT NULL)`)
	exec(`CREATE TABLE view_audit (seq int PRIMARY KEY, msg text NOT NULL)`)
	exec(`CREATE VIEW items_view AS SELECT id, val FROM base_items`)
	exec(`CREATE FUNCTION items_view_ins() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE next_seq int;
		BEGIN
			INSERT INTO base_items (id, val) VALUES (NEW.id, NEW.val);
			SELECT COALESCE(MAX(seq), 0) + 1 INTO next_seq FROM view_audit;
			INSERT INTO view_audit VALUES (next_seq, 'INS:' || NEW.id::text || '=' || NEW.val::text);
			RETURN NEW;
		END $$`)
	exec(`CREATE FUNCTION items_view_upd() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE next_seq int;
		BEGIN
			UPDATE base_items SET val = NEW.val WHERE id = OLD.id;
			SELECT COALESCE(MAX(seq), 0) + 1 INTO next_seq FROM view_audit;
			INSERT INTO view_audit VALUES (next_seq, 'UPD:' || OLD.id::text || ':' || OLD.val::text || '->' || NEW.val::text);
			RETURN NEW;
		END $$`)
	exec(`CREATE TRIGGER trg_items_view_ins INSTEAD OF INSERT ON items_view FOR EACH ROW EXECUTE FUNCTION items_view_ins()`)
	exec(`CREATE TRIGGER trg_items_view_upd INSTEAD OF UPDATE ON items_view FOR EACH ROW EXECUTE FUNCTION items_view_upd()`)

	// WHEN NOT MATCHED THEN INSERT must deliver NEW.id=1, NEW.val=42 to INSTEAD OF INSERT.
	if n := exec(`MERGE INTO items_view v
		USING (VALUES (1, 42)) AS s(id, val)
		ON v.id = s.id
		WHEN NOT MATCHED THEN INSERT (id, val) VALUES (s.id, s.val)
		WHEN MATCHED THEN UPDATE SET val = s.val`); n != 1 {
		t.Fatalf("MERGE INSERT rows affected = %d, want 1", n)
	}
	var val int
	if err := db.QueryRow(`SELECT val FROM base_items WHERE id = 1`).Scan(&val); err != nil {
		t.Fatalf("select inserted row: %v", err)
	}
	if val != 42 {
		t.Fatalf("inserted val = %d, want 42", val)
	}

	// WHEN MATCHED THEN UPDATE must deliver OLD.val=42, NEW.val=99 to INSTEAD OF UPDATE.
	if n := exec(`MERGE INTO items_view v
		USING (VALUES (1, 99)) AS s(id, val)
		ON v.id = s.id
		WHEN NOT MATCHED THEN INSERT (id, val) VALUES (s.id, s.val)
		WHEN MATCHED THEN UPDATE SET val = s.val`); n != 1 {
		t.Fatalf("MERGE UPDATE rows affected = %d, want 1", n)
	}
	if err := db.QueryRow(`SELECT val FROM base_items WHERE id = 1`).Scan(&val); err != nil {
		t.Fatalf("select updated row: %v", err)
	}
	if val != 99 {
		t.Fatalf("updated val = %d, want 99", val)
	}

	// Mixed MERGE executing both INSERT and UPDATE in one statement.
	if n := exec(`MERGE INTO items_view v
		USING (VALUES (1, 123), (2, 77)) AS s(id, val)
		ON v.id = s.id
		WHEN NOT MATCHED THEN INSERT (id, val) VALUES (s.id, s.val)
		WHEN MATCHED THEN UPDATE SET val = s.val`); n != 2 {
		t.Fatalf("mixed MERGE rows affected = %d, want 2", n)
	}
	for _, want := range []struct {
		id, val int
	}{{1, 123}, {2, 77}} {
		if err := db.QueryRow(`SELECT val FROM base_items WHERE id = $1`, want.id).Scan(&val); err != nil {
			t.Fatalf("select row id=%d: %v", want.id, err)
		}
		if val != want.val {
			t.Fatalf("row id=%d val = %d, want %d", want.id, val, want.val)
		}
	}

	// Verify audit trail recorded exact OLD/NEW values delivered to triggers.
	wantAudit := map[string]bool{
		"INS:1=42":      false,
		"UPD:1:42->99":  false,
		"UPD:1:99->123": false,
		"INS:2=77":      false,
	}
	rows, err := db.Query(`SELECT msg FROM view_audit ORDER BY seq`)
	if err != nil {
		t.Fatalf("select view_audit: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var msg string
		if err := rows.Scan(&msg); err != nil {
			t.Fatalf("scan view_audit: %v", err)
		}
		if _, ok := wantAudit[msg]; !ok {
			t.Fatalf("unexpected audit entry %q", msg)
		}
		wantAudit[msg] = true
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("view_audit rows.Err: %v", err)
	}
	if count != len(wantAudit) {
		t.Fatalf("audit count = %d, want %d (%v)", count, len(wantAudit), wantAudit)
	}
}

// TestPhase5ModifyTriggerOnConflictBeforeUpdate verifies that INSERT ... ON
// CONFLICT DO UPDATE handles BEFORE UPDATE FOR EACH ROW triggers cleanly:
// returning NULL skips the update without leaking errTriggerSkipped or falsely
// recording conflict identity, while returning a modified NEW writes the
// modified value.
func TestPhase5ModifyTriggerOnConflictBeforeUpdate(t *testing.T) {
	t.Run("return_null_skips_cleanly", func(t *testing.T) {
		db, _ := openTestDB(t)
		exec := func(q string) int64 {
			t.Helper()
			res, err := db.Exec(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				t.Fatalf("%s: RowsAffected: %v", q, err)
			}
			return n
		}

		exec(`CREATE TABLE conflict_skip (id int PRIMARY KEY, val int)`)
		exec(`INSERT INTO conflict_skip VALUES (1, 10)`)
		exec(`CREATE FUNCTION skip_upd() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RETURN NULL;
			END $$`)
		exec(`CREATE TRIGGER trg_skip_upd BEFORE UPDATE ON conflict_skip FOR EACH ROW EXECUTE FUNCTION skip_upd()`)

		if n := exec(`INSERT INTO conflict_skip (id, val) VALUES (1, 20) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`); n != 0 {
			t.Fatalf("ON CONFLICT DO UPDATE skipped by BEFORE UPDATE affected = %d, want 0", n)
		}

		var val int
		if err := db.QueryRow(`SELECT val FROM conflict_skip WHERE id = 1`).Scan(&val); err != nil {
			t.Fatalf("select row: %v", err)
		}
		if val != 10 {
			t.Fatalf("row val = %d, want 10 (unchanged)", val)
		}

		// RETURNING must also yield zero rows and no error when skipped.
		rows, err := db.Query(`INSERT INTO conflict_skip (id, val) VALUES (1, 30) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val RETURNING id, val`)
		if err != nil {
			t.Fatalf("ON CONFLICT RETURNING skipped: %v", err)
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatalf("expected 0 rows from skipped ON CONFLICT RETURNING")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows.Err: %v", err)
		}

		// Still enforces 21000 when hitting a row inserted earlier in the same command.
		_, err = db.Exec(`INSERT INTO conflict_skip (id, val) VALUES (2, 10), (2, 20) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`)
		if err == nil {
			t.Fatalf("expected SQLSTATE 21000 for row inserted earlier in same command, got nil")
		}
		if got := SQLState(err); got != "21000" {
			t.Fatalf("SQLSTATE = %q (%v), want 21000", got, err)
		}
	})

	t.Run("skipped_conflict_does_not_block_later_update_in_same_command", func(t *testing.T) {
		db, _ := openTestDB(t)
		exec := func(q string) int64 {
			t.Helper()
			res, err := db.Exec(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				t.Fatalf("%s: RowsAffected: %v", q, err)
			}
			return n
		}

		exec(`CREATE TABLE conflict_selective (id int PRIMARY KEY, val int)`)
		exec(`INSERT INTO conflict_selective VALUES (1, 10)`)
		exec(`CREATE FUNCTION skip_twenty() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.val = 20 THEN
					RETURN NULL;
				END IF;
				RETURN NEW;
			END $$`)
		exec(`CREATE TRIGGER trg_skip_twenty BEFORE UPDATE ON conflict_selective FOR EACH ROW EXECUTE FUNCTION skip_twenty()`)

		// First conflicting tuple (1, 20) is skipped by BEFORE UPDATE, so the
		// second conflicting tuple (1, 30) must succeed without a false 21000.
		if n := exec(`INSERT INTO conflict_selective (id, val) VALUES (1, 20), (1, 30) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`); n != 1 {
			t.Fatalf("selective skip rows affected = %d, want 1", n)
		}
		var val int
		if err := db.QueryRow(`SELECT val FROM conflict_selective WHERE id = 1`).Scan(&val); err != nil {
			t.Fatalf("select row: %v", err)
		}
		if val != 30 {
			t.Fatalf("row val = %d, want 30", val)
		}

		// Two non-skipped conflicting tuples still trigger 21000.
		_, err := db.Exec(`INSERT INTO conflict_selective (id, val) VALUES (1, 40), (1, 50) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`)
		if err == nil {
			t.Fatalf("expected SQLSTATE 21000 for two applied updates on same row, got nil")
		}
		if got := SQLState(err); got != "21000" {
			t.Fatalf("SQLSTATE = %q (%v), want 21000", got, err)
		}
	})

	t.Run("modified_new_is_written", func(t *testing.T) {
		db, _ := openTestDB(t)
		exec := func(q string) int64 {
			t.Helper()
			res, err := db.Exec(q)
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				t.Fatalf("%s: RowsAffected: %v", q, err)
			}
			return n
		}

		exec(`CREATE TABLE conflict_mod (id int PRIMARY KEY, val int)`)
		exec(`INSERT INTO conflict_mod VALUES (1, 10)`)
		exec(`CREATE FUNCTION bump_upd() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				NEW.val := NEW.val + 100;
				RETURN NEW;
			END $$`)
		exec(`CREATE TRIGGER trg_bump_upd BEFORE UPDATE ON conflict_mod FOR EACH ROW EXECUTE FUNCTION bump_upd()`)

		if n := exec(`INSERT INTO conflict_mod (id, val) VALUES (1, 50) ON CONFLICT (id) DO UPDATE SET val = EXCLUDED.val`); n != 1 {
			t.Fatalf("ON CONFLICT DO UPDATE modified by BEFORE UPDATE affected = %d, want 1", n)
		}
		var val int
		if err := db.QueryRow(`SELECT val FROM conflict_mod WHERE id = 1`).Scan(&val); err != nil {
			t.Fatalf("select row: %v", err)
		}
		if val != 150 {
			t.Fatalf("row val = %d, want 150", val)
		}
	})
}
