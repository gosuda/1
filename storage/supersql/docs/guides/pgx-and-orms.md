# Go Drivers, `pgx`, `pgwire` & ORM Integration Guide

SuperSQL provides three integration tiers for Go applications and external PostgreSQL tools—all pure Go (`CGO_ENABLED=0`) over the same embedded database directory.

---

## 1. Choosing an Integration Tier

| Tier | Package | Best For |
| :--- | :--- | :--- |
| **Standard `database/sql`** | `github.com/gosuda/supersql-alpha` | `sqlc` (`database/sql`), `sqlx`, Ent `dialect/sql`, Go stdlib services |
| **In-Process `pgx/v5` API** | `github.com/gosuda/supersql-alpha/pgx` | Apps already using `pgxpool`, `NamedArgs`, `SendBatch`, `CopyFrom`, `CollectRows`, `WaitForNotification` with zero TCP overhead |
| **Optional PG v3.0 TCP Listener** | `github.com/gosuda/supersql-alpha/pgwire` | `psql`, DBeaver, DataGrip, `github.com/jackc/pgx/v5`, GORM (`gorm.io/driver/postgres`), Ent (`pgx/v5`), Bun (`pgdriver`) |

---

## 2. Standard `database/sql`

Construct a connector explicitly (global `sql.Open("supersql", ...)` is intentionally rejected because a certified clock cannot be passed in a DSN):

```go
clk := localclock.New()
connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
	Directory: "/var/lib/myapp/db",
	Clock:     clk,
})
if err != nil {
	return err
}
db := sql.OpenDB(connector)
defer db.Close() // also closes connector & catalog
```

Inspecting PostgreSQL SQLSTATE errors:

```go
var sqlErr *supersql.Error
if errors.As(err, &sqlErr) {
	switch sqlErr.Code {
	case "23505": // unique_violation
	case "40P01": // deadlock_detected
	case "42501": // insufficient_privilege
	}
}
```

---

## 3. Cleanroom `supersql/pgx`

```go
pool, err := pgx.OpenPool(ctx, supersql.OpenOptions{
	Directory: dir,
	Clock:     localclock.New(),
})
if err != nil {
	return err
}
defer pool.Close()

// LISTEN / WaitForNotification on a dedicated connection
conn, err := pool.Acquire(ctx)
if err != nil {
	return err
}
defer conn.Release()

if _, err := conn.Exec(ctx, `LISTEN job_events`); err != nil {
	return err
}
notif, err := conn.WaitForNotification(ctx)
```

- Supports `PoolConfig` lifecycle hooks (`BeforeConnect`, `AfterConnect`, `BeforeAcquire`, `AfterRelease`), `BeginFunc` / `BeginTxFunc`, nested savepoint `tx.Begin`, `Batch` / `SendBatch`, `CopyFrom`, `NamedArgs` (`@param`), `pgtype` wrappers, `*pgx.PgError`, and `CollectRows` / `RowToStructByName`.

---

## 4. Optional `pgwire` TCP Listener (`psql`, GORM, Ent, `jackc/pgx/v5`)

Attach a PostgreSQL v3.0 wire listener to an already-opened `*supersql.Catalog` (for local admin inspection or connecting standard PostgreSQL drivers):

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory: dir,
	Clock:     localclock.New(),
})
if err != nil {
	return err
}
defer db.Close()

srv, err := pgwire.NewServerFromCatalog(db, pgwire.Config{
	AuthFunc: func(ctx context.Context, user, database, password string) error {
		if !verifyPassword(user, password) {
			return errors.New("invalid credentials")
		}
		return nil
	},
	TLSConfig:      tlsCfg, // enables SSLRequest -> 'S' TLS upgrade
	MaxConnections: 64,
	IdleTimeout:    5 * time.Minute,
})
if err != nil {
	return err
}
defer srv.Close()

go func() { _ = srv.ListenAndServe(ctx, "127.0.0.1:5432") }()
```

### Security Notes for `pgwire`
- **Role Binding**: When a connecting `user` matches an existing SQL role in `pg_catalog.pg_roles`, `pgwire` binds `session_user` directly at the engine layer (`is_superuser=off` for non-superusers). Neither `RESET ROLE`, `RESET ALL`, nor `DO $$ BEGIN RESET ROLE; END $$` can escalate back to superuser.
- **Pre-Auth Limits & DoS Protection**: Pre-auth sockets are bounded and tracked for clean `Server.Close()`, repeated `SSLRequest`/`GSSENCRequest` loops are rejected, and binary array/numeric wire decoders validate dimension bounds before allocation.
- **Connecting with `psql` or `jackc/pgx/v5`**:
  ```sh
  psql "postgres://app_user:secret@127.0.0.1:5432/postgres?sslmode=disable"
  ```
