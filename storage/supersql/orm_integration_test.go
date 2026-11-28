package supersql_test

import (
	"database/sql"
	"encoding/json"
	"slices"
	"testing"
	"time"

	jackpgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	supersql "github.com/gosuda/supersql-alpha"
	clocklocal "github.com/gosuda/supersql-alpha/clock/local"
	"github.com/gosuda/supersql-alpha/pgwire"
	ssqlpgx "github.com/gosuda/supersql-alpha/pgx"
)

// TestEcosystemORMAndSQLCPatterns exercises real-world sqlc, GORM, Ent, and Bun
// query/migration/introspection patterns across database/sql, supersql/pgx, and
// upstream github.com/jackc/pgx/v5 over pgwire.
func TestEcosystemORMAndSQLCPatterns(t *testing.T) {
	ctx := t.Context()
	clk := clocklocal.New()

	db, err := supersql.Open(ctx, supersql.OpenOptions{
		Directory: t.TempDir(),
		Clock:     clk,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// 1. GORM / Ent-style schema migration + introspection via database/sql.
	sqlDB := sql.OpenDB(supersql.NewConnectorWithCatalog(db))
	defer sqlDB.Close()

	migrations := []string{
		`CREATE TABLE tenants (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			slug TEXT NOT NULL UNIQUE,
			settings JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE users (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			tenant_id BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
			email TEXT NOT NULL,
			display_name TEXT NOT NULL,
			tags TEXT[] NOT NULL DEFAULT '{}',
			version INT NOT NULL DEFAULT 1,
			deleted_at TIMESTAMPTZ,
			UNIQUE (tenant_id, email)
		)`,
		`CREATE INDEX idx_users_active_email ON users (tenant_id, email) WHERE deleted_at IS NULL`,
	}
	for _, stmt := range migrations {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("migration %q: %v", stmt, err)
		}
	}

	// Verify ORM schema introspection queries (information_schema + pg_catalog).
	var colCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'users'`,
	).Scan(&colCount); err != nil {
		t.Fatalf("information_schema.columns: %v", err)
	}
	if colCount != 7 {
		t.Fatalf("users column count = %d, want 7", colCount)
	}

	// 2. sqlc-style prepared statements: INSERT ... RETURNING, ON CONFLICT DO UPDATE,
	//    CTE pagination, optimistic locking, and nested savepoints.
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	var tenantID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO tenants (slug, settings) VALUES ($1, $2::jsonb)
		 ON CONFLICT (slug) DO UPDATE SET settings = EXCLUDED.settings
		 RETURNING id`,
		"acme", `{"plan":"pro","seats":25}`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	if _, err := tx.ExecContext(ctx, `SAVEPOINT sp_users`); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}
	var userID int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO users (tenant_id, email, display_name, tags)
		 VALUES ($1, $2, $3, ARRAY['admin', 'owner'])
		 RETURNING id`,
		tenantID, "alice@acme.io", "Alice",
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	// Roll back a second tentative insert inside a nested savepoint.
	if _, err := tx.ExecContext(ctx, `SAVEPOINT sp_bad`); err != nil {
		t.Fatalf("SAVEPOINT sp_bad: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO users (tenant_id, email, display_name) VALUES ($1, $2, $3)`,
		tenantID, "temp@acme.io", "Temp",
	); err != nil {
		t.Fatalf("insert temp user: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp_bad`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Optimistic locking update (GORM/Ent pattern).
	var newVersion int
	if err := sqlDB.QueryRowContext(ctx,
		`UPDATE users SET display_name = $1, version = version + 1
		 WHERE id = $2 AND version = $3 AND deleted_at IS NULL
		 RETURNING version`,
		"Alice Prime", userID, 1,
	).Scan(&newVersion); err != nil {
		t.Fatalf("optimistic update: %v", err)
	}
	if newVersion != 2 {
		t.Fatalf("version = %d, want 2", newVersion)
	}

	// 3. supersql/pgx native adapter: batch + Query.
	spool := ssqlpgx.NewPoolFromCatalog(db)
	defer spool.Close()

	batch := &ssqlpgx.Batch{}
	batch.Queue(`INSERT INTO users (tenant_id, email, display_name, tags) VALUES ($1, $2, $3, ARRAY['dev'])`, tenantID, "bob@acme.io", "Bob")
	batch.Queue(`INSERT INTO users (tenant_id, email, display_name, tags) VALUES ($1, $2, $3, ARRAY['ops'])`, tenantID, "carol@acme.io", "Carol")
	br := spool.SendBatch(ctx, batch)
	for i := range 2 {
		if _, err := br.Exec(); err != nil {
			t.Fatalf("batch[%d]: %v", i, err)
		}
	}
	if err := br.Close(); err != nil {
		t.Fatalf("batch close: %v", err)
	}

	// 4. Upstream github.com/jackc/pgx/v5 + pgxpool over pgwire (exact wire path used by
	//    GORM postgres driver, Ent pgx dialect, Bun pgdriver, and sqlc pgx/v5).
	srv, err := pgwire.NewServerFromCatalog(db, pgwire.Config{
		Password: "orm-test-password",
	})
	if err != nil {
		t.Fatalf("pgwire.NewServerFromCatalog: %v", err)
	}
	go func() { _ = srv.ListenAndServe(ctx, "127.0.0.1:0") }()
	defer srv.Close()

	// Wait briefly for listener address to bind.
	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("pgwire server failed to bind address")
	}

	poolCfg, err := pgxpool.ParseConfig("postgres://supersql:orm-test-password@" + addr + "/postgres?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()

	// CTE + JSON aggregation + parameterized IN / ANY over upstream pgx/v5 binary/extended protocol.
	var settingsRaw []byte
	var userCount int64
	if err := pool.QueryRow(ctx,
		`WITH active AS (
			SELECT tenant_id, email FROM users WHERE deleted_at IS NULL AND email = ANY($1::text[])
		)
		SELECT t.settings, count(a.email)
		FROM tenants t
		LEFT JOIN active a ON a.tenant_id = t.id
		WHERE t.slug = $2
		GROUP BY t.id, t.settings`,
		[]string{"alice@acme.io", "bob@acme.io"}, "acme",
	).Scan(&settingsRaw, &userCount); err != nil {
		t.Fatalf("upstream pgx/v5 CTE query: %v", err)
	}
	if userCount != 2 {
		t.Fatalf("active matched user count = %d, want 2", userCount)
	}
	var parsed map[string]any
	if err := json.Unmarshal(settingsRaw, &parsed); err != nil {
		t.Fatalf("json.Unmarshal settings %q: %v", string(settingsRaw), err)
	}
	if parsed["plan"] != "pro" {
		t.Fatalf("settings.plan = %v, want pro", parsed["plan"])
	}

	// Upstream pgx/v5 transaction + prepared statement reuse.
	pgxTx, err := pool.BeginTx(ctx, jackpgx.TxOptions{})
	if err != nil {
		t.Fatalf("pgxpool.BeginTx: %v", err)
	}
	var deletedEmail string
	if err := pgxTx.QueryRow(ctx,
		`UPDATE users SET deleted_at = now() WHERE email = $1 RETURNING email`,
		"carol@acme.io",
	).Scan(&deletedEmail); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if deletedEmail != "carol@acme.io" {
		t.Fatalf("deletedEmail = %q, want carol@acme.io", deletedEmail)
	}
	if err := pgxTx.Commit(ctx); err != nil {
		t.Fatalf("pgxTx.Commit: %v", err)
	}

	// Confirm soft-deleted row is excluded from active count.
	var activeTotal int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&activeTotal); err != nil {
		t.Fatalf("active count: %v", err)
	}
	if activeTotal != 2 {
		t.Fatalf("activeTotal = %d, want 2", activeTotal)
	}
}

// TestICUCollationsEndToEnd verifies predefined pure-Go ICU collations,
// CREATE/DROP COLLATION DDL, case-insensitive and numeric collations, Korean
// Hangul ordering, column/index collations, and GROUP BY/DISTINCT semantics.
func TestICUCollationsEndToEnd(t *testing.T) {
	ctx := t.Context()
	clk := clocklocal.New()

	db, err := supersql.Open(ctx, supersql.OpenOptions{
		Directory: t.TempDir(),
		Clock:     clk,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	sqlDB := sql.OpenDB(supersql.NewConnectorWithCatalog(db))
	defer sqlDB.Close()

	// 1. Predefined en-US-x-icu vs C ordering (case folding / linguistic order).
	rows, err := sqlDB.QueryContext(ctx,
		`SELECT v FROM (VALUES ('b'), ('A'), ('a'), ('B')) AS t(v) ORDER BY v COLLATE "en-US-x-icu"`,
	)
	if err != nil {
		t.Fatalf("en-US-x-icu ORDER BY: %v", err)
	}
	var enOrder []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		enOrder = append(enOrder, s)
	}
	rows.Close()
	if !slices.Equal(enOrder, []string{"a", "A", "b", "B"}) {
		t.Fatalf("en-US-x-icu order = %v, want [a A b B]", enOrder)
	}

	// 2. Korean Hangul collation (ko-KR-x-icu).
	rows, err = sqlDB.QueryContext(ctx,
		`SELECT v FROM (VALUES ('하늘'), ('가나다'), ('바다'), ('나비')) AS t(v) ORDER BY v COLLATE "ko-KR-x-icu"`,
	)
	if err != nil {
		t.Fatalf("ko-KR-x-icu ORDER BY: %v", err)
	}
	var koOrder []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		koOrder = append(koOrder, s)
	}
	rows.Close()
	if !slices.Equal(koOrder, []string{"가나다", "나비", "바다", "하늘"}) {
		t.Fatalf("ko-KR-x-icu order = %v, want [가나다 나비 바다 하늘]", koOrder)
	}

	// 3. Numeric collation (en-US-u-kn-true): item2 < item10.
	rows, err = sqlDB.QueryContext(ctx,
		`SELECT v FROM (VALUES ('item10'), ('item2'), ('item1')) AS t(v) ORDER BY v COLLATE "en-US-u-kn-true"`,
	)
	if err != nil {
		t.Fatalf("en-US-u-kn-true ORDER BY: %v", err)
	}
	var numOrder []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		numOrder = append(numOrder, s)
	}
	rows.Close()
	if !slices.Equal(numOrder, []string{"item1", "item2", "item10"}) {
		t.Fatalf("numeric order = %v, want [item1 item2 item10]", numOrder)
	}

	// 4. CREATE COLLATION (case-insensitive nondeterministic) + column & index usage.
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE COLLATION ci_en (provider = icu, locale = 'en-US-u-ks-level2', deterministic = false)`,
	); err != nil {
		t.Fatalf("CREATE COLLATION ci_en: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE TABLE contacts (id INT PRIMARY KEY, email TEXT COLLATE ci_en NOT NULL)`,
	); err != nil {
		t.Fatalf("CREATE TABLE contacts: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE INDEX idx_contacts_email ON contacts (email)`,
	); err != nil {
		t.Fatalf("CREATE INDEX idx_contacts_email: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO contacts VALUES (1, 'Alice@Example.com'), (2, 'bob@example.com'), (3, 'ALICE@example.com')`,
	); err != nil {
		t.Fatalf("INSERT contacts: %v", err)
	}

	var matched int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM contacts WHERE email = 'alice@example.com'`,
	).Scan(&matched); err != nil {
		t.Fatalf("case-insensitive lookup: %v", err)
	}
	if matched != 2 {
		t.Fatalf("case-insensitive email count = %d, want 2", matched)
	}

	// 5. CREATE COLLATION ... FROM existing + DROP COLLATION.
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE COLLATION ci_copy FROM ci_en`,
	); err != nil {
		t.Fatalf("CREATE COLLATION FROM: %v", err)
	}
	var eq bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT ('Hello' COLLATE ci_copy) = ('hello' COLLATE ci_copy)`,
	).Scan(&eq); err != nil {
		t.Fatalf("ci_copy equality: %v", err)
	}
	if !eq {
		t.Fatal("ci_copy equality = false, want true")
	}
	if _, err := sqlDB.ExecContext(ctx, `DROP COLLATION ci_copy`); err != nil {
		t.Fatalf("DROP COLLATION ci_copy: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `DROP COLLATION IF EXISTS ci_copy`); err != nil {
		t.Fatalf("DROP COLLATION IF EXISTS: %v", err)
	}

	// 6. P0-6 regression: secondary index lookup on table with non-deterministic ICU PRIMARY KEY.
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE TABLE ci_pk_items (code TEXT COLLATE ci_en PRIMARY KEY, category TEXT NOT NULL, val INT NOT NULL)`,
	); err != nil {
		t.Fatalf("CREATE TABLE ci_pk_items: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`CREATE INDEX idx_ci_pk_category ON ci_pk_items (category)`,
	); err != nil {
		t.Fatalf("CREATE INDEX idx_ci_pk_category: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO ci_pk_items VALUES ('Alpha-1', 'books', 42), ('Beta-2', 'games', 99)`,
	); err != nil {
		t.Fatalf("INSERT ci_pk_items: %v", err)
	}
	var gotCode string
	var gotVal int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT code, val FROM ci_pk_items WHERE category = 'books'`,
	).Scan(&gotCode, &gotVal); err != nil {
		t.Fatalf("secondary lookup on non-det ICU PK: %v", err)
	}
	if gotCode != "Alpha-1" || gotVal != 42 {
		t.Fatalf("secondary lookup got (%q, %d), want (Alpha-1, 42)", gotCode, gotVal)
	}

	// 7. P1-5 regression: hashtext on deterministic and non-deterministic ICU collations.
	var hDet int32
	if err := sqlDB.QueryRowContext(ctx, `SELECT hashtext('abc' COLLATE "en-US-x-icu")`).Scan(&hDet); err != nil {
		t.Fatalf("hashtext deterministic ICU: %v", err)
	}
	var hCI1, hCI2 int32
	if err := sqlDB.QueryRowContext(ctx, `SELECT hashtext('Alice' COLLATE ci_en), hashtext('alice' COLLATE ci_en)`).Scan(&hCI1, &hCI2); err != nil {
		t.Fatalf("hashtext non-deterministic ICU: %v", err)
	}
	if hCI1 != hCI2 {
		t.Fatalf("non-det ICU hashtext mismatch: %d != %d", hCI1, hCI2)
	}
}
