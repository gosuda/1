package supersql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// diffStep is one statement in the differential script. query steps compare
// result columns and rows; exec steps compare affected counts. ordered marks
// row order contractual (the statement carries ORDER BY); otherwise rows are
// compared as multisets. oursCode requires our side to fail with that exact
// SQLSTATE — the exclusion contract — while the reference may behave
// differently and is only recorded.
type diffStep struct {
	sql      string
	query    bool
	ordered  bool
	oursCode string
}

// TestPG184Differential replays a required-profile script against an isolated
// PostgreSQL 18.4 reference (SUPERSQL_PG184_DSN) and this engine, comparing
// SQLSTATEs, result column counts, typed values, and affected counts. Type
// identity is compared through pg_typeof/format_type names and the
// information_schema presentation; typmods travel with those names.
// Unexplained mismatches fail the test.
func TestPG184Differential(t *testing.T) {
	dsn := os.Getenv("SUPERSQL_PG184_DSN")
	if dsn == "" {
		t.Skip("SUPERSQL_PG184_DSN is not set; differential gate unperformed")
	}
	ctx := t.Context()

	referenceDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open reference DSN: %v", err)
	}
	defer referenceDB.Close()
	// The script runs on one pinned connection so both sides see the same
	// session state; the pool admits a second connection for the deferred
	// schema drop, which would otherwise wait forever for the pinned slot.
	referenceDB.SetMaxOpenConns(2)
	reference, err := referenceDB.Conn(ctx)
	if err != nil {
		t.Fatalf("reference connect: %v", err)
	}
	defer reference.Close()

	var versionNum string
	if err := reference.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&versionNum); err != nil {
		t.Fatalf("SHOW server_version_num: %v", err)
	}
	if versionNum != "180004" {
		t.Fatalf("reference server_version_num = %q, want 180004", versionNum)
	}

	schema := fmt.Sprintf("supersql_diff_%d", time.Now().UnixNano())
	for _, setup := range []string{
		`CREATE SCHEMA ` + schema,
		`SET search_path = ` + schema,
		`SET default_transaction_isolation = 'serializable'`,
		`SET TimeZone = 'UTC'`,
	} {
		if _, err := reference.ExecContext(ctx, setup); err != nil {
			t.Fatalf("reference setup %q: %v", setup, err)
		}
	}
	defer func() {
		// The schema belongs to this run; drop it through a fresh connection so
		// cleanup is not affected by the pinned connection's transaction state.
		if _, err := referenceDB.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("drop reference schema: %v", err)
		}
	}()

	embedded := sql.OpenDB(mustConnector(t, ctx))
	defer embedded.Close()
	embedded.SetMaxOpenConns(1)
	ours, err := embedded.Conn(ctx)
	if err != nil {
		t.Fatalf("embedded connect: %v", err)
	}
	defer ours.Close()
	if _, err := ours.ExecContext(ctx, `SET TimeZone = 'UTC'`); err != nil {
		t.Fatalf("embedded TimeZone: %v", err)
	}
	if _, err := ours.ExecContext(ctx, `SET supersql.compat_isolation = 'off'`); err != nil {
		t.Fatalf("embedded compat_isolation: %v", err)
	}
	var isolation string
	if err := ours.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatalf("SHOW transaction_isolation: %v", err)
	}
	if isolation != "serializable" {
		t.Fatalf("embedded transaction_isolation = %q, want serializable", isolation)
	}

	mismatches := 0
	for i, step := range diffScript {
		referenceOutcome := runDiffStep(ctx, reference, step)
		ourOutcome := runDiffStep(ctx, ours, step)
		if step.oursCode != "" {
			if ourOutcome.state != step.oursCode {
				t.Errorf("step %d %q: embedded SQLSTATE = %q, want %q", i, step.sql, ourOutcome.state, step.oursCode)
			}
			t.Logf("step %d exclusion %q: embedded=%q reference=%q", i, step.sql, ourOutcome.state, referenceOutcome.state)
			// Exclusion steps such as BEGIN ISOLATION LEVEL succeed on the
			// reference and leave a transaction open; reset it so later steps
			// see the same baseline as the embedded side.
			_, _ = reference.ExecContext(ctx, `ROLLBACK`)
			continue
		}
		if reason := compareDiffOutcome(referenceOutcome, ourOutcome, step.ordered); reason != "" {
			mismatches++
			t.Errorf("step %d %q mismatch: %s\n  reference: %+v\n  embedded:  %+v", i, step.sql, reason, referenceOutcome.summary(), ourOutcome.summary())
		}
	}
	t.Logf("differential script: %d steps, %d mismatches", len(diffScript), mismatches)
}

type diffOutcome struct {
	state    string // SQLSTATE, "" on success
	errText  string
	columns  int
	rows     [][]string
	affected int64
}

func (o diffOutcome) summary() string {
	if o.state != "" {
		return fmt.Sprintf("error %s %s", o.state, o.errText)
	}
	return fmt.Sprintf("columns=%d affected=%d rows=%v", o.columns, o.affected, o.rows)
}

func runDiffStep(ctx context.Context, conn *sql.Conn, step diffStep) diffOutcome {
	var outcome diffOutcome
	if step.query {
		rows, err := conn.QueryContext(ctx, step.sql)
		if err != nil {
			outcome.state, outcome.errText = diffSQLState(err), err.Error()
			return outcome
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			outcome.state, outcome.errText = diffSQLState(err), err.Error()
			return outcome
		}
		outcome.columns = len(columns)
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				outcome.state, outcome.errText = diffSQLState(err), err.Error()
				return outcome
			}
			row := make([]string, len(values))
			for i, value := range values {
				row[i] = normalizeDiffValue(value)
			}
			outcome.rows = append(outcome.rows, row)
		}
		if err := rows.Err(); err != nil {
			outcome.state, outcome.errText = diffSQLState(err), err.Error()
		}
		return outcome
	}
	result, err := conn.ExecContext(ctx, step.sql)
	if err != nil {
		outcome.state, outcome.errText = diffSQLState(err), err.Error()
		return outcome
	}
	affected, err := result.RowsAffected()
	if err == nil {
		outcome.affected = affected
	}
	return outcome
}

// normalizeDiffValue renders a scanned driver value in a canonical form so
// both drivers compare byte-for-byte: bools as t/f, floats in PostgreSQL
// presentation, timestamps as UTC, bytea as \x-hex.
func normalizeDiffValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "\x00"
	case bool:
		if typed {
			return "t"
		}
		return "f"
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		switch {
		case math.IsNaN(typed):
			return "NaN"
		case math.IsInf(typed, 1):
			return "Infinity"
		case math.IsInf(typed, -1):
			return "-Infinity"
		}
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case string:
		return typed
	case []byte:
		return `\x` + hex.EncodeToString(typed)
	case time.Time:
		return typed.UTC().Format("2006-01-02 15:04:05.999999999")
	default:
		return fmt.Sprintf("%v", typed)
	}
}

func diffSQLState(err error) string {
	if err == nil {
		return ""
	}
	var ours *model.Error
	if errors.As(err, &ours) {
		if code := ours.SQLState(); code != "" {
			return code
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	// An error with no SQLSTATE is an internal failure, never a successful
	// step: mapping it to XX000 keeps a bare regression from matching the
	// reference's success.
	return "XX000"
}

func compareDiffOutcome(reference, ours diffOutcome, ordered bool) string {
	if reference.state != ours.state {
		return fmt.Sprintf("SQLSTATE %q vs %q", reference.state, ours.state)
	}
	if reference.state != "" {
		return ""
	}
	if reference.columns != ours.columns {
		return fmt.Sprintf("column count %d vs %d", reference.columns, ours.columns)
	}
	if reference.affected != ours.affected {
		return fmt.Sprintf("affected %d vs %d", reference.affected, ours.affected)
	}
	left := reference.rowStrings()
	right := ours.rowStrings()
	if ordered {
		if !slices.Equal(left, right) {
			return "ordered rows differ"
		}
		return ""
	}
	slices.Sort(left)
	slices.Sort(right)
	if !slices.Equal(left, right) {
		return "row multiset differs"
	}
	return ""
}

func (o diffOutcome) rowStrings() []string {
	rows := make([]string, len(o.rows))
	for i, row := range o.rows {
		rows[i] = strings.Join(row, "\x1f")
	}
	return rows
}

var diffScript = []diffStep{
	{sql: `CREATE TABLE diff_t (id int PRIMARY KEY, v int, t text, vc varchar(12), f float8, b bool, n numeric(12,2), d date, ts timestamptz, j jsonb, r int4range, a int4[])`},
	{sql: `INSERT INTO diff_t VALUES (1, 10, 'alpha', 'a1', 1.5, true, 12.34, '2024-01-02', '2024-01-02 03:04:05+00', '{"x":1}', '[1,5)', '{1,2}'), (2, 20, 'beta', 'b2', -2.25, false, -0.01, '2024-01-03', '2024-01-03 00:00:00+00', '{"x":[2,3]}', '[3,7)', '{3,4}'), (3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)`},
	{sql: `SELECT id, v, t, vc, f, b, n::text, d::text, ts, j::text, r::text, a::text FROM diff_t ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT count(*), sum(v), min(v), max(v), avg(v)::text FROM diff_t`, query: true},
	{sql: `SELECT v % 3, abs(-v), v * 2, v / 4, 7 / 2, (7.0 / 2)::text, mod(7, 3), power(2, 10)::text, round(2.5)::text, floor(2.7)::text, ceil(2.2)::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT upper(t), lower(t), t || '-' || v::text, length(t), left(t, 2), right(t, 2), substr(t, 2, 2), position('a' in t), trim(' x '), btrim('  y  '), repeat('ab', 3), lpad(t, 6, '.'), rpad(t, 6, '.'), initcap('hello there'), reverse(t), ascii('A'), chr(66), translate(t, 'a', 'o'), split_part('x,y,z', ',', 2), strpos(t, 'a') FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT CASE WHEN v > 15 THEN 'big' WHEN v > 5 THEN 'mid' ELSE 'small' END, coalesce(NULL, 42), nullif(v, 10), greatest(1, v, 7), least(1, v, 7) FROM diff_t ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT '42'::int + 1, '3.5'::numeric::text, '2024-01-02'::date::text, 1::text, 1.5::float8::text, 't'::boolean, cast('17' as int8), 23::varchar(5)::text, 25::char(3)::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT pg_typeof(1 + 1)::text, pg_typeof('x'::text)::text, pg_typeof(1.5)::text, pg_typeof(now())::text, pg_typeof('{}'::jsonb)::text, pg_typeof('{1,2}'::int4[])::text, pg_typeof(int4range(1, 2))::text, pg_typeof('2024-01-02'::date)::text`, query: true},
	{sql: `SELECT format_type(23, -1)::text, format_type(1043, 14)::text, format_type(1700, 425990)::text`, query: true},
	{sql: `SELECT (n + 0.01::numeric(12,2))::text, (n * 2)::text, (n - 1)::text, (n / 3)::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT (d + 3)::text, (d - 1)::text, (ts + interval '1 day 02:00:00')::text, (ts - interval '30 minutes')::text, extract(year from ts)::text, extract(month from ts)::text, extract(dow from d)::text, date_trunc('day', ts)::text, age(ts::timestamp, '2024-01-01'::timestamp)::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT (j -> 'x')::text, (j ->> 'x')::text, j ? 'x', j @> '{"x":1}'::jsonb, jsonb_build_object('k', 1)::text, jsonb_array_length('[1,2]'::jsonb), jsonb_typeof(j -> 'x')::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT r::text, lower(r), upper(r), r && int4range(2, 6), r @> 4, isempty(r), int4range(1, 5, '[]')::text, (r * int4range(3, 9))::text, (r + int4range(7, 9))::text FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT a::text, a[1], array_length(a, 1), cardinality(a), array_append(a, 9)::text, array_prepend(0, a)::text, array_position(a, 2), a && '{2,9}'::int4[], a @> '{2}'::int4[], array_to_string(a, ',') FROM diff_t WHERE id = 1`, query: true},
	{sql: `SELECT v FROM diff_t WHERE v IS NULL OR v IS NOT NULL ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT id FROM diff_t WHERE v IN (10, 20, 30) ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT id FROM diff_t WHERE EXISTS (SELECT 1 WHERE v > 15) AND id < 3 ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT CASE WHEN EXISTS (SELECT 1 FROM diff_t WHERE id = 9) THEN 1 ELSE 0 END`, query: true},
	{sql: `SELECT t FROM diff_t WHERE t LIKE 'a%' OR t LIKE '%t_' ORDER BY t`, query: true, ordered: true},
	{sql: `SELECT t ~ '^a', t ~* '^B', t !~ 'z' FROM diff_t WHERE t IS NOT NULL ORDER BY t`, query: true, ordered: true},
	{sql: `SELECT id, v FROM diff_t WHERE (v, id) > (15, 0) ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT DISTINCT b FROM diff_t ORDER BY b`, query: true, ordered: true},
	{sql: `SELECT b, count(*), sum(v) FROM diff_t GROUP BY b ORDER BY b`, query: true, ordered: true},
	{sql: `SELECT b, count(*) FROM diff_t GROUP BY b HAVING count(*) >= 1 ORDER BY b`, query: true, ordered: true},
	{sql: `SELECT count(DISTINCT b), string_agg(t, ',' ORDER BY t), array_agg(v ORDER BY v)::text FROM diff_t`, query: true},
	{sql: `SELECT id, sum(v) OVER (ORDER BY id), rank() OVER (ORDER BY v DESC NULLS LAST), lag(v) OVER (ORDER BY id), count(*) OVER () FROM diff_t ORDER BY id`, query: true, ordered: true},
	{sql: `WITH x AS (SELECT max(v) AS m FROM diff_t) SELECT m FROM x`, query: true},
	{sql: `WITH RECURSIVE series(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM series WHERE n < 5) SELECT n FROM series ORDER BY n`, query: true, ordered: true},
	{sql: `SELECT id FROM diff_t UNION SELECT id + 100 FROM diff_t WHERE id = 1 ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT id FROM diff_t INTERSECT SELECT 1 ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT id FROM diff_t EXCEPT SELECT 2 ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT a.id, b.v FROM diff_t a JOIN diff_t b ON a.id = b.id ORDER BY a.id`, query: true, ordered: true},
	{sql: `SELECT a.id, b.v FROM diff_t a LEFT JOIN diff_t b ON a.id = b.id + 10 ORDER BY a.id`, query: true, ordered: true},
	{sql: `SELECT * FROM generate_series(1, 5) AS g(n) ORDER BY n`, query: true, ordered: true},
	{sql: `SELECT * FROM generate_series(1, 5) g`, query: true},
	{sql: `SELECT * FROM (VALUES (1, 'x'), (2, 'y')) AS q(n, s) ORDER BY n`, query: true, ordered: true},
	{sql: `SELECT v FROM diff_t ORDER BY id LIMIT 1 OFFSET 1`, query: true, ordered: true},
	{sql: `SELECT x.n FROM diff_t, LATERAL (SELECT v + 1 AS n) x ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT * FROM JSON_TABLE('[{"a":1},{"a":2}]', '$[*]' COLUMNS (id FOR ORDINALITY, a int PATH '$.a')) jt ORDER BY id`, query: true, ordered: true},
	{sql: `SELECT (1, 'x') = (1, 'x'), ROW(1, 'x') < ROW(2, 'y'), (1, 'x')::text`, query: true},
	{sql: `SELECT 1 = ALL(ARRAY[1, 1]), 2 = ANY(ARRAY[1, 2]), 3 <> ALL(ARRAY[1, 2])`, query: true},
	{sql: `SELECT 'a' < 'b' COLLATE "C", 'B' < 'a' COLLATE "C", 'z' < 'Z' COLLATE "C"`, query: true},
	{sql: `SELECT column_name::text, data_type::text, is_nullable::text FROM information_schema.columns WHERE table_name::text = 'diff_t' AND table_schema::text = current_schema()::text ORDER BY ordinal_position`, query: true, ordered: true},
	{sql: `CREATE SEQUENCE diff_seq START 10`},
	{sql: `SELECT nextval('diff_seq'), nextval('diff_seq')`, query: true},
	{sql: `SELECT lastval()`, query: true},
	{sql: `SELECT setval('diff_seq', 42), nextval('diff_seq')`, query: true},
	{sql: `CREATE INDEX diff_t_v ON diff_t (v)`},
	{sql: `CREATE INDEX diff_t_partial ON diff_t (v) WHERE v > 15`},
	{sql: `ANALYZE diff_t`},
	{sql: `SELECT id, v FROM diff_t WHERE v > 15 ORDER BY id`, query: true, ordered: true},
	{sql: `CREATE VIEW diff_v AS SELECT id, v * 2 AS vv FROM diff_t`},
	{sql: `SELECT id, vv FROM diff_v ORDER BY id`, query: true, ordered: true},
	{sql: `CREATE TABLE diff_log (id int, op text)`},
	{sql: `CREATE FUNCTION diff_trg() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO diff_log VALUES (NEW.id, TG_OP); RETURN NEW; END $$`},
	{sql: `CREATE TRIGGER diff_t_trg BEFORE INSERT ON diff_t FOR EACH ROW EXECUTE FUNCTION diff_trg()`},
	{sql: `INSERT INTO diff_t (id, v) VALUES (4, 40)`},
	{sql: `SELECT id, op FROM diff_log ORDER BY id`, query: true, ordered: true},
	{sql: `DO $$ DECLARE x int := 0; BEGIN WHILE x < 3 LOOP INSERT INTO diff_log VALUES (100 + x, 'do'); x := x + 1; END LOOP; END $$`},
	{sql: `SELECT id, op FROM diff_log WHERE op = 'do' ORDER BY id`, query: true, ordered: true},
	{sql: `UPDATE diff_t SET v = v + 1 WHERE id = 2`},
	{sql: `UPDATE diff_t SET v = v + 1 WHERE id = 1 RETURNING id, v`, query: true, ordered: true},
	{sql: `DELETE FROM diff_t WHERE id = 3 RETURNING id`, query: true},
	{sql: `INSERT INTO diff_t (id) VALUES (5) ON CONFLICT (id) DO UPDATE SET v = excluded.v RETURNING id, v`, query: true},
	{sql: `INSERT INTO diff_t (id) VALUES (5) ON CONFLICT (id) DO NOTHING`},
	{sql: `INSERT INTO diff_t (id) VALUES (5) ON CONFLICT (id) DO UPDATE SET v = 50`},
	{sql: `SELECT id, v FROM diff_t WHERE id = 5`, query: true},
	{sql: `PREPARE dp AS SELECT v FROM diff_t WHERE id = $1`},
	{sql: `EXECUTE dp(5)`, query: true},
	{sql: `DEALLOCATE dp`},
	{sql: `BEGIN`},
	{sql: `INSERT INTO diff_log VALUES (7, 'txn')`},
	{sql: `SAVEPOINT sp1`},
	{sql: `INSERT INTO diff_log VALUES (8, 'rolled')`},
	{sql: `ROLLBACK TO SAVEPOINT sp1`},
	{sql: `COMMIT`},
	{sql: `SELECT id, op FROM diff_log WHERE id IN (7, 8) ORDER BY id`, query: true, ordered: true},
	{sql: `BEGIN`},
	{sql: `INSERT INTO diff_log VALUES (9, 'gone')`},
	{sql: `ROLLBACK`},
	{sql: `SELECT count(*) FROM diff_log WHERE id = 9`, query: true},
	{sql: `CREATE TYPE diff_mood AS ENUM ('sad', 'ok', 'happy')`},
	{sql: `CREATE TABLE diff_enum (id int PRIMARY KEY, m diff_mood)`},
	{sql: `INSERT INTO diff_enum VALUES (1, 'ok'), (2, 'happy')`},
	{sql: `SELECT id, m::text FROM diff_enum ORDER BY id`, query: true, ordered: true},
	{sql: `CREATE DOMAIN diff_posint AS int CHECK (VALUE > 0)`},
	{sql: `CREATE TABLE diff_dom (id int PRIMARY KEY, p diff_posint)`},
	{sql: `INSERT INTO diff_dom VALUES (1, 5)`},
	{sql: `SELECT id, p FROM diff_dom ORDER BY id`, query: true, ordered: true},
	{sql: `CREATE TYPE diff_pair AS (x int, y text)`},
	{sql: `SELECT ROW(1, 'a')::diff_pair::text`, query: true},
	{sql: `ALTER TABLE diff_t ADD COLUMN extra int DEFAULT 7`},
	{sql: `SELECT id, extra FROM diff_t WHERE id = 1`, query: true},
	{sql: `ALTER TABLE diff_t RENAME COLUMN extra TO extra2`},
	{sql: `SELECT extra2 FROM diff_t WHERE id = 1`, query: true},
	{sql: `COMMENT ON TABLE diff_t IS 'differential'`},
	{sql: `SELECT obj_description('diff_t'::regclass)::text`, query: true},
	// Error parity: both sides must fail with the same SQLSTATE.
	{sql: `SELECT 1/0`, query: true},
	{sql: `INSERT INTO diff_t (id) VALUES (1)`},
	{sql: `SELECT * FROM diff_missing_table`, query: true},
	{sql: `SELECT nosuchfunction(1)`, query: true},
	{sql: `SELECT 'notanint'::int`, query: true},
	{sql: `SELEC bogus`, query: true},
	{sql: `INSERT INTO diff_dom VALUES (2, -1)`},
	{sql: `SELECT m::text FROM diff_enum WHERE m = 'happy'`, query: true},
	// Exclusion contract: embedded must reject with 0A000; the reference result
	// is recorded, never mapped onto it.
	{sql: `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`, oursCode: "0A000"},
	{sql: `BEGIN ISOLATION LEVEL REPEATABLE READ`, oursCode: "0A000"},
	{sql: `GRANT SELECT ON diff_t TO PUBLIC`, oursCode: "0A000"},
	{sql: `VACUUM diff_t`, oursCode: "0A000"},
	{sql: `LISTEN diff_chan`, oursCode: "0A000"},
	{sql: `CREATE EXTENSION hstore`, oursCode: "0A000"},
	{sql: `SELECT xmin FROM diff_t`, query: true, oursCode: "0A000"},
	{sql: `PREPARE TRANSACTION 'diff_xact'`, oursCode: "0A000"},
}
