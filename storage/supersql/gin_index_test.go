package supersql

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func execGINTextRows(t *testing.T, db *Catalog, sqlText string) []string {
	t.Helper()
	outcome, err := db.exec(t.Context(), sqlText, 0)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	var out []string
	for _, vals := range outcome.Rows {
		if len(vals) > 0 && !vals[0].IsNull {
			out = append(out, vals[0].StrVal)
		}
	}
	return out
}

func execGINInt64IDs(t *testing.T, db *Catalog, sqlText string) []int64 {
	t.Helper()
	outcome, err := db.exec(t.Context(), sqlText, 0)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	var ids []int64
	for _, vals := range outcome.Rows {
		if len(vals) == 0 || vals[0].IsNull {
			t.Fatalf("unexpected row in %s: %+v", sqlText, vals)
		}
		ids = append(ids, vals[0].IntVal)
	}
	return ids
}

func explainPlanText(t *testing.T, db *Catalog, query string) string {
	t.Helper()
	lines := execGINTextRows(t, db, "EXPLAIN "+query)
	return strings.Join(lines, "\n")
}

func mustExecGIN(t *testing.T, db *Catalog, stmts ...string) {
	t.Helper()
	ctx := t.Context()
	for _, stmt := range stmts {
		if _, err := db.exec(ctx, stmt, 0); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func TestGINValidationRejections(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	mustExecGIN(t, db, `CREATE TABLE bad_gin (id bigint PRIMARY KEY, doc jsonb, tags int[], body text)`)

	invalidDDL := []string{
		`CREATE UNIQUE INDEX idx_gin_uniq ON bad_gin USING gin (doc)`,
		`CREATE INDEX idx_gin_multi ON bad_gin USING gin (doc, tags)`,
		`CREATE INDEX idx_gin_include ON bad_gin USING gin (doc) INCLUDE (id)`,
		`CREATE INDEX idx_gin_expr ON bad_gin USING gin ((doc -> 'a'))`,
		`CREATE INDEX idx_gin_badtype ON bad_gin USING gin (id)`,
		`CREATE INDEX idx_gin_badopclass ON bad_gin USING gin (doc array_ops)`,
		`CREATE INDEX idx_gin_badopclass2 ON bad_gin USING gin (tags jsonb_ops)`,
		`CREATE INDEX idx_gin_desc ON bad_gin USING gin (doc DESC)`,
	}
	for _, ddl := range invalidDDL {
		if _, err := db.exec(ctx, ddl, 0); err == nil {
			t.Fatalf("expected error for invalid GIN DDL %q, got nil", ddl)
		}
	}
}

func TestGINJSONBOpsAndPathOps(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Pre-populate rows BEFORE creating GIN indexes to test backfill!
	mustExecGIN(t, db,
		`CREATE TABLE docs (id bigint PRIMARY KEY, doc jsonb, pdoc jsonb)`,
		`INSERT INTO docs VALUES
			(1, '{"a": 1, "b": "hello", "tags": ["go", "sql"]}'::jsonb, '{"a": 1, "nested": {"k": true}}'::jsonb),
			(2, '{"a": 2, "c": 1.0, "tags": ["rust"]}'::jsonb, '{"a": 2, "nested": {"k": false}}'::jsonb),
			(3, '{}'::jsonb, '{}'::jsonb),
			(4, NULL, NULL),
			(5, '{"a": 1, "b": "world", "d": null}'::jsonb, '{"a": 1, "nested": {"k": true, "x": 42}}'::jsonb)`,
		`CREATE INDEX idx_docs_gin ON docs USING gin (doc)`,
		`CREATE INDEX idx_docs_path_gin ON docs USING gin (pdoc jsonb_path_ops)`,
	)

	// Verify EXPLAIN selects GIN Index Scan for jsonb_ops operators @>, ?, ?|, ?&
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{`SELECT id FROM docs WHERE doc @> '{"a": 1}'::jsonb ORDER BY id`, []int64{1, 5}},
		{`SELECT id FROM docs WHERE doc @> '{"tags": ["go"]}'::jsonb ORDER BY id`, []int64{1}},
		{`SELECT id FROM docs WHERE doc @> '{"c": 1}'::jsonb ORDER BY id`, []int64{2}},
		{`SELECT id FROM docs WHERE doc @> '{}'::jsonb ORDER BY id`, []int64{1, 2, 3, 5}},
		{`SELECT id FROM docs WHERE doc ? 'b' ORDER BY id`, []int64{1, 5}},
		{`SELECT id FROM docs WHERE doc ?| ARRAY['b', 'c'] ORDER BY id`, []int64{1, 2, 5}},
		{`SELECT id FROM docs WHERE doc ?& ARRAY['a', 'b'] ORDER BY id`, []int64{1, 5}},
		{`SELECT id FROM docs WHERE doc ?& ARRAY['a', 'c'] ORDER BY id`, []int64{2}},
	} {
		plan := explainPlanText(t, db, tc.query)
		if !strings.Contains(plan, "GIN Index Scan using idx_docs_gin on docs") {
			t.Fatalf("query %q did not use GIN Index Scan:\n%s", tc.query, plan)
		}
		got := execGINInt64IDs(t, db, tc.query)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("query %q = %v, want %v", tc.query, got, tc.want)
		}
	}

	// Verify EXPLAIN FORMAT JSON / YAML / XML include GIN Index Scan as well!
	for _, fmtOpt := range []string{"(FORMAT JSON)", "(FORMAT YAML)", "(FORMAT XML)"} {
		lines := execGINTextRows(t, db, "EXPLAIN "+fmtOpt+` SELECT id FROM docs WHERE doc @> '{"a": 1}'::jsonb`)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "GIN Index Scan using idx_docs_gin on docs") {
			t.Fatalf("EXPLAIN %s missing GIN Index Scan:\n%s", fmtOpt, joined)
		}
	}

	// jsonb_path_ops MUST use GIN for @>, and MUST NOT use GIN for ? / ?| / ?&!
	pathContainment := `SELECT id FROM docs WHERE pdoc @> '{"nested": {"k": true}}'::jsonb ORDER BY id`
	if plan := explainPlanText(t, db, pathContainment); !strings.Contains(plan, "GIN Index Scan using idx_docs_path_gin on docs") {
		t.Fatalf("jsonb_path_ops @> did not use GIN Index Scan:\n%s", plan)
	}
	if got := execGINInt64IDs(t, db, pathContainment); !slices.Equal(got, []int64{1, 5}) {
		t.Fatalf("jsonb_path_ops @> = %v, want [1 5]", got)
	}

	for _, q := range []string{
		`SELECT id FROM docs WHERE pdoc ? 'nested' ORDER BY id`,
		`SELECT id FROM docs WHERE pdoc ?| ARRAY['nested', 'a'] ORDER BY id`,
		`SELECT id FROM docs WHERE pdoc ?& ARRAY['nested', 'a'] ORDER BY id`,
	} {
		if plan := explainPlanText(t, db, q); strings.Contains(plan, "GIN Index Scan") {
			t.Fatalf("jsonb_path_ops MUST NOT use GIN for %q:\n%s", q, plan)
		}
		if got := execGINInt64IDs(t, db, q); !slices.Equal(got, []int64{1, 2, 5}) {
			t.Fatalf("jsonb_path_ops fallback %q = %v, want [1 2 5]", q, got)
		}
	}

	// Predicate-less full scan must NOT select GIN Index Scan.
	if plan := explainPlanText(t, db, `SELECT id FROM docs ORDER BY id`); strings.Contains(plan, "GIN Index Scan") {
		t.Fatalf("predicate-less scan falsely used GIN:\n%s", plan)
	}
}

func TestGINArraysAndTSVector(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	mustExecGIN(t, db,
		`CREATE TABLE articles (id bigint PRIMARY KEY, nums int[], labels text[], tsv tsvector)`,
		`INSERT INTO articles VALUES
			(1, ARRAY[1, 2, 3], ARRAY['go', 'db', 'gin'], to_tsvector('simple', 'The quick brown fox jumps over the lazy dog')),
			(2, ARRAY[2, 4, 6], ARRAY['rust', 'db'], to_tsvector('simple', 'Database inverted indexes accelerate search queries')),
			(3, ARRAY[]::int[], ARRAY[]::text[], to_tsvector('simple', '')),
			(4, NULL, NULL, NULL),
			(5, ARRAY[3, 5, 7], ARRAY['go', 'search'], to_tsvector('simple', 'Quick fox and database search'))`,
		`CREATE INDEX idx_articles_nums ON articles USING gin (nums)`,
		`CREATE INDEX idx_articles_labels ON articles USING gin (labels array_ops)`,
		`CREATE INDEX idx_articles_tsv ON articles USING gin (tsv tsvector_ops)`,
	)

	cases := []struct {
		query string
		idx   string
		want  []int64
	}{
		{`SELECT id FROM articles WHERE nums @> ARRAY[2, 3] ORDER BY id`, "idx_articles_nums", []int64{1}},
		{`SELECT id FROM articles WHERE nums && ARRAY[4, 5] ORDER BY id`, "idx_articles_nums", []int64{2, 5}},
		{`SELECT id FROM articles WHERE ARRAY[2] <@ nums ORDER BY id`, "idx_articles_nums", []int64{1, 2}},
		{`SELECT id FROM articles WHERE nums @> ARRAY[]::int[] ORDER BY id`, "idx_articles_nums", []int64{1, 2, 3, 5}},
		{`SELECT id FROM articles WHERE labels @> ARRAY['go'] ORDER BY id`, "idx_articles_labels", []int64{1, 5}},
		{`SELECT id FROM articles WHERE labels && ARRAY['rust', 'search'] ORDER BY id`, "idx_articles_labels", []int64{2, 5}},
		{`SELECT id FROM articles WHERE tsv @@ to_tsquery('simple', 'quick & fox') ORDER BY id`, "idx_articles_tsv", []int64{1, 5}},
		{`SELECT id FROM articles WHERE tsv @@ to_tsquery('simple', 'jumps | accelerate') ORDER BY id`, "idx_articles_tsv", []int64{1, 2}},
		{`SELECT id FROM articles WHERE to_tsquery('simple', 'database & search') @@ tsv ORDER BY id`, "idx_articles_tsv", []int64{2, 5}},
		// Negated tsquery falls back safely to base scan and still returns exact results!
		{`SELECT id FROM articles WHERE tsv @@ to_tsquery('simple', '!fox') ORDER BY id`, "idx_articles_tsv", []int64{2, 3}},
	}
	for _, tc := range cases {
		plan := explainPlanText(t, db, tc.query)
		if !strings.Contains(plan, "GIN Index Scan using "+tc.idx+" on articles") {
			t.Fatalf("query %q did not use GIN index %s:\n%s", tc.query, tc.idx, plan)
		}
		got := execGINInt64IDs(t, db, tc.query)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("query %q = %v, want %v", tc.query, got, tc.want)
		}
	}
}

func TestGINCRUDRollbackDropAndReindex(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	mustExecGIN(t, db,
		`CREATE TABLE kv (id bigint PRIMARY KEY, meta jsonb, tags text[])`,
		`CREATE INDEX idx_kv_meta ON kv USING gin (meta)`,
		`CREATE INDEX idx_kv_tags ON kv USING gin (tags)`,
		`INSERT INTO kv VALUES (1, '{"env": "dev", "tier": 1}'::jsonb, ARRAY['alpha', 'beta'])`,
		`INSERT INTO kv VALUES (2, '{"env": "prod", "tier": 2}'::jsonb, ARRAY['beta', 'gamma'])`,
	)

	// UPDATE: old token must no longer match, new token must match!
	mustExecGIN(t, db,
		`UPDATE kv SET meta = '{"env": "staging", "tier": 1}'::jsonb, tags = ARRAY['beta', 'delta'] WHERE id = 1`,
	)
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "dev"}'::jsonb ORDER BY id`); len(got) != 0 {
		t.Fatalf("stale jsonb token still matched after UPDATE: %v", got)
	}
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "staging"}'::jsonb ORDER BY id`); !slices.Equal(got, []int64{1}) {
		t.Fatalf("new jsonb token after UPDATE = %v, want [1]", got)
	}
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE tags @> ARRAY['alpha'] ORDER BY id`); len(got) != 0 {
		t.Fatalf("stale array token still matched after UPDATE: %v", got)
	}
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE tags @> ARRAY['delta'] ORDER BY id`); !slices.Equal(got, []int64{1}) {
		t.Fatalf("new array token after UPDATE = %v, want [1]", got)
	}
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE tags @> ARRAY['beta'] ORDER BY id`); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("retained array token after UPDATE = %v, want [1 2]", got)
	}

	// UPDATE non-NULL -> NULL -> non-NULL.
	mustExecGIN(t, db, `UPDATE kv SET meta = NULL, tags = NULL WHERE id = 2`)
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "prod"}'::jsonb ORDER BY id`); len(got) != 0 {
		t.Fatalf("NULL update left stale postings: %v", got)
	}
	mustExecGIN(t, db, `UPDATE kv SET meta = '{"env": "prod"}'::jsonb, tags = ARRAY['omega'] WHERE id = 2`)
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE tags @> ARRAY['omega'] ORDER BY id`); !slices.Equal(got, []int64{2}) {
		t.Fatalf("non-NULL restore = %v, want [2]", got)
	}

	// DELETE: postings removed!
	mustExecGIN(t, db, `DELETE FROM kv WHERE id = 1`)
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "staging"}'::jsonb ORDER BY id`); len(got) != 0 {
		t.Fatalf("deleted row still matched GIN scan: %v", got)
	}

	// Transaction ROLLBACK: uncommitted postings must not survive rollback!
	mustExecGIN(t, db,
		`BEGIN`,
		`INSERT INTO kv VALUES (3, '{"env": "ephemeral"}'::jsonb, ARRAY['phantom'])`,
		`UPDATE kv SET meta = '{"env": "rolledback"}'::jsonb WHERE id = 2`,
		`ROLLBACK`,
	)
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "ephemeral"}'::jsonb ORDER BY id`); len(got) != 0 {
		t.Fatalf("rolled-back insert matched GIN scan: %v", got)
	}
	if got := execGINInt64IDs(t, db, `SELECT id FROM kv WHERE meta @> '{"env": "prod"}'::jsonb ORDER BY id`); !slices.Equal(got, []int64{2}) {
		t.Fatalf("rolled-back update lost original GIN posting: %v", got)
	}

	// REINDEX TABLE rebuilds GIN indexes cleanly!
	mustExecGIN(t, db, `REINDEX TABLE kv`)
	q := `SELECT id FROM kv WHERE meta @> '{"env": "prod"}'::jsonb ORDER BY id`
	if plan := explainPlanText(t, db, q); !strings.Contains(plan, "GIN Index Scan using idx_kv_meta on kv") {
		t.Fatalf("REINDEX lost GIN Index Scan:\n%s", plan)
	}
	if got := execGINInt64IDs(t, db, q); !slices.Equal(got, []int64{2}) {
		t.Fatalf("after REINDEX = %v, want [2]", got)
	}

	// DROP INDEX removes GIN cleanly and queries fall back to sequential scan.
	mustExecGIN(t, db, `DROP INDEX idx_kv_meta`)
	if plan := explainPlanText(t, db, q); strings.Contains(plan, "GIN Index Scan") {
		t.Fatalf("dropped GIN index still in plan:\n%s", plan)
	}
	if got := execGINInt64IDs(t, db, q); !slices.Equal(got, []int64{2}) {
		t.Fatalf("after DROP INDEX = %v, want [2]", got)
	}
}

func TestGINParityAgainstSequentialScan(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	mustExecGIN(t, db,
		`CREATE TABLE base_plain (id bigint PRIMARY KEY, doc jsonb, arr int[], tsv tsvector)`,
		`CREATE TABLE base_gin   (id bigint PRIMARY KEY, doc jsonb, arr int[], tsv tsvector)`,
		`CREATE INDEX idx_parity_doc ON base_gin USING gin (doc)`,
		`CREATE INDEX idx_parity_arr ON base_gin USING gin (arr)`,
		`CREATE INDEX idx_parity_tsv ON base_gin USING gin (tsv)`,
	)

	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"}
	for i := 1; i <= 40; i++ {
		var docSQL, arrSQL, tsvSQL string
		switch i % 8 {
		case 0:
			docSQL, arrSQL, tsvSQL = "NULL", "NULL", "NULL"
		case 1:
			docSQL, arrSQL, tsvSQL = "'{}'::jsonb", "ARRAY[]::int[]", "to_tsvector('simple', '')"
		default:
			w1 := words[i%len(words)]
			w2 := words[(i*3)%len(words)]
			docSQL = fmt.Sprintf(`'{"k%d": %d, "grp": "%s", "nested": {"flag": %t, "vals": [%d, %d]}}'::jsonb`, i%5, i%4, w1, i%2 == 0, i%7, (i+2)%7)
			arrSQL = fmt.Sprintf(`ARRAY[%d, %d, %d]`, i%7, (i*2)%7, (i+3)%7)
			tsvSQL = fmt.Sprintf(`to_tsvector('simple', '%s %s item%d')`, w1, w2, i%5)
		}
		for _, tbl := range []string{"base_plain", "base_gin"} {
			stmt := fmt.Sprintf(`INSERT INTO %s VALUES (%d, %s, %s, %s)`, tbl, i, docSQL, arrSQL, tsvSQL)
			mustExecGIN(t, db, stmt)
		}
	}
	// Mutate a subset on both tables identically.
	for _, tbl := range []string{"base_plain", "base_gin"} {
		mustExecGIN(t, db,
			fmt.Sprintf(`UPDATE %s SET doc = '{"grp": "omega", "k0": 99}'::jsonb, arr = ARRAY[99, 100], tsv = to_tsvector('simple', 'omega special') WHERE id IN (3, 11, 27)`, tbl),
			fmt.Sprintf(`DELETE FROM %s WHERE id IN (5, 19, 33)`, tbl),
		)
	}

	predicates := []string{
		`doc @> '{"grp": "alpha"}'::jsonb`,
		`doc @> '{"nested": {"flag": true}}'::jsonb`,
		`doc @> '{"nested": {"vals": [3]}}'::jsonb`,
		`doc @> '{"grp": "omega", "k0": 99}'::jsonb`,
		`doc @> '{}'::jsonb`,
		`doc ? 'k2'`,
		`doc ?| ARRAY['k1', 'k3']`,
		`doc ?& ARRAY['grp', 'nested']`,
		`arr @> ARRAY[1, 4]`,
		`arr && ARRAY[2, 99]`,
		`ARRAY[3] <@ arr`,
		`tsv @@ to_tsquery('simple', 'alpha & delta')`,
		`tsv @@ to_tsquery('simple', 'beta | omega')`,
		`tsv @@ to_tsquery('simple', 'alpha & !beta')`,
		`tsv @@ to_tsquery('simple', 'eps:*')`,
	}

	for _, pred := range predicates {
		plainSQL := fmt.Sprintf(`SELECT id FROM base_plain WHERE %s ORDER BY id`, pred)
		ginSQL := fmt.Sprintf(`SELECT id FROM base_gin WHERE %s ORDER BY id`, pred)

		plan := explainPlanText(t, db, ginSQL)
		if !strings.Contains(plan, "GIN Index Scan") {
			t.Fatalf("parity query %q did not select GIN Index Scan:\n%s", pred, plan)
		}
		want := execGINInt64IDs(t, db, plainSQL)
		got := execGINInt64IDs(t, db, ginSQL)
		if !slices.Equal(got, want) {
			t.Fatalf("parity mismatch for WHERE %s:\n  gin   = %v\n  plain = %v", pred, got, want)
		}
	}
	_ = context.Background()
}

func TestGINJSONBTopLevelArrayAndStringExists(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	mustExecGIN(t, db,
		`CREATE TABLE jexists (id bigint PRIMARY KEY, doc jsonb)`,
		`INSERT INTO jexists VALUES
			(1, '["foo", "bar"]'::jsonb),
			(2, '"foo"'::jsonb),
			(3, '{"foo": 1, "bar": 2}'::jsonb),
			(4, '[["foo"]]'::jsonb),
			(5, '{"nested": ["foo", "bar"]}'::jsonb),
			(6, '["baz"]'::jsonb),
			(7, '[]'::jsonb),
			(8, NULL)`,
		`CREATE INDEX idx_jexists_gin ON jexists USING gin (doc)`,
	)

	cases := []struct {
		query string
		want  []int64
	}{
		{`SELECT id FROM jexists WHERE doc ? 'foo' ORDER BY id`, []int64{1, 2, 3}},
		{`SELECT id FROM jexists WHERE doc ?| ARRAY['foo', 'baz'] ORDER BY id`, []int64{1, 2, 3, 6}},
		{`SELECT id FROM jexists WHERE doc ?& ARRAY['foo', 'bar'] ORDER BY id`, []int64{1, 3}},
		{`SELECT id FROM jexists WHERE doc ? 'bar' ORDER BY id`, []int64{1, 3}},
	}
	for _, tc := range cases {
		plan := explainPlanText(t, db, tc.query)
		if !strings.Contains(plan, "GIN Index Scan using idx_jexists_gin on jexists") {
			t.Fatalf("query %q did not use GIN Index Scan:\n%s", tc.query, plan)
		}
		got := execGINInt64IDs(t, db, tc.query)
		if !slices.Equal(got, tc.want) {
			t.Fatalf("query %q = %v, want %v", tc.query, got, tc.want)
		}
	}
}
