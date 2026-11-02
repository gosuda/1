# hqq-sqlite3 (`gosuda.org/sqlite3`)

`gosuda.org/sqlite3` is a **zero-`cgo` (`CGO_ENABLED=0`) SQLite `database/sql` driver** powered by HQQ shared-memory IPC. Your Go application compiles as a pure Go binary without linking C objects, while executing the official SQLite C engine in an isolated shared-memory worker process (`hqq-sqlite-server`).

SLSA-attested prebuilt `hqq-sqlite-server` binaries are embedded per target platform (`//go:build` + `//go:embed`), extracted to the user cache on first use, verified against pinned SHA-256 digests via `gosuda.org/hqq/secureexec`, and bound to the Go parent process lifetime via `--parent-stdin`. No external binary download or C toolchain is required.

```bash
go get gosuda.org/sqlite3
```

## Supported Platforms

| OS | Architectures | Embedded & SLSA-Attested Release Binary |
| :--- | :--- | :--- |
| **Linux** | `amd64`, `arm64` | `hqq-sqlite-server_linux_amd64`, `hqq-sqlite-server_linux_arm64` (embedded) |
| **macOS** | `arm64` (Apple Silicon), `amd64` | `hqq-sqlite-server_darwin_arm64`, `hqq-sqlite-server_darwin_amd64` (embedded) |
| **Windows** | `amd64`, `arm64` | `hqq-sqlite-server_windows_amd64.exe`, `hqq-sqlite-server_windows_arm64.exe` (embedded) |

---

## Quickstart

### 1. Drop-In `database/sql` (`"sqlite3"`)

Import `gosuda.org/sqlite3` to register the `"sqlite3"` driver (drop-in replacement for `github.com/mattn/go-sqlite3`). Opening a file path, `file:` URI, or `:memory:` automatically verifies and launches the embedded `hqq-sqlite-server` worker in the background:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	_ "gosuda.org/sqlite3" // registers "sqlite3"
)

func main() {
	ctx := context.Background()

	db, err := sql.Open(
		"sqlite3",
		"app.db?_journal=WAL&_sync=NORMAL&_fk=1&_busy_timeout=5000&_txlock=immediate",
	)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		log.Fatal(err)
	}
	fmt.Println("SQLite version:", version)
}
```

### 2. Managed Server + `*sql.DB` (`launcher.OpenManaged`)

For explicit directory sandboxing (`AllowDir`) and alias mappings, `launcher.OpenManaged` starts a dedicated worker process (using the embedded binary automatically when `Binary` is omitted) and returns a connected `*sql.DB`:

```go
m, err := launcher.OpenManaged(ctx, launcher.Config{
	AllowDir: "/var/lib/myapp/db",
	Aliases:  map[string]string{"main": "/var/lib/myapp/db/app.db"},
},
	launcher.WithDatabase("main"),
	launcher.WithSQLiteConfig(sqlite.Config{
		Timeout:     5 * time.Second,
		BusyTimeout: 5 * time.Second,
		Pragmas: map[string]string{
			"journal_mode": "WAL",
			"synchronous":  "NORMAL",
			"foreign_keys": "1",
		},
	}),
)
if err != nil {
	return err
}
defer m.Close()
```

### 3. Explicit Listener DSN or Structured Connector (`hqq://`, `sqlite.NewConnector`)

When connecting to a pre-started `hqq-sqlite-server` listener:

```go
// Via hqq:// DSN:
db, err := sql.Open(
	"sqlite3",
	"hqq://sqlite-svc?db=main&_journal=WAL&_sync=NORMAL&_fk=1&_busy_timeout=5000",
)

// Or via structured Config:
conn, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
	Database:    "main",
	Mode:        sqlite.ModeReadWriteCreate,
	Timeout:     5 * time.Second,
	FetchSize:   256,
	BusyTimeout: 5 * time.Second,
	TxMode:      sqlite.TxImmediate,
	Location:    time.UTC,
	Pragmas: map[string]string{
		"journal_mode": "WAL",
		"foreign_keys": "1",
	},
})
db := sql.OpenDB(conn)
```

---

## Public API Summary

### Driver & Configuration (`package sqlite`)

```go
const (
	DriverName          = "sqlite3"
	Version             = "v0.3.3540100"
	SQLiteVersionNumber = 3540100
)

type Mode uint8
const (
	ModeReadWriteCreate Mode = iota // default: rwc
	ModeReadWrite                   // rw
	ModeReadOnly                    // ro
)

type TxMode uint8
const (
	TxDeferred  TxMode = iota // BEGIN DEFERRED
	TxImmediate               // BEGIN IMMEDIATE
	TxExclusive               // BEGIN EXCLUSIVE
)

type Config struct {
	Database    string
	Mode        Mode
	Timeout     time.Duration
	FetchSize   int
	BusyTimeout time.Duration // < 0 disables busy waiting (0 ms)
	TxMode      TxMode
	Location    *time.Location
	Pragmas     map[string]string
	Session     rt.Options
}

func NewConnector(listener string, cfg Config) (*Connector, error)
func ConnectorFromDSN(dsn string) (*Connector, error)
func ParseDSN(dsn string) (listener string, cfg Config, err error)
func WithTxMode(ctx context.Context, mode TxMode) context.Context
func ServerBinary() (path string, hash secureexec.Hash, err error)
func ExpectedHash(goos, goarch string) (secureexec.Hash, bool)
```

### Error Inspection (`package sqlite`)

```go
type Error struct {
	Code         int    // primary SQLite result code (e.g. CodeConstraint)
	ExtendedCode int    // extended result code (e.g. CodeConstraintUnique)
	Message      string // sqlite3_errmsg()
}

var (
	ErrBusy           error
	ErrLocked         error
	ErrConstraint     error
	ErrReadOnly       error
	ErrInterrupted    error
	ErrOutcomeUnknown error
)

func IsBusy(err error) bool
func IsLocked(err error) bool
func IsConstraint(err error) bool
func IsReadOnly(err error) bool
func IsInterrupted(err error) bool
```

### Process Launcher (`package launcher`)

```go
func Start(ctx context.Context, cfg Config) (*Server, error)
func Open(ctx context.Context, cfg Config, opts ...OpenOption) (*sql.DB, *Server, error)
func OpenManaged(ctx context.Context, cfg Config, opts ...OpenOption) (*Managed, error)

func WithDatabase(db string) OpenOption
func WithSQLiteConfig(cfg sqlite.Config) OpenOption
```

---

## Documentation

| Document | Description |
| :--- | :--- |
| [**`docs/DSN.md`**](docs/DSN.md) | `hqq://` DSN syntax, underscore-prefixed PRAGMA control knobs (`_journal`, `_sync`, `_fk`, `_busy_timeout`, `_txlock`, `_loc`, ...), and type conversion rules. |
| [**`docs/ARCHITECTURE.md`**](docs/ARCHITECTURE.md) | Shared-memory session architecture, handle lifecycles, batch shims (`fetch_rows`, `column_chunk`), retry safety, and benchmarks. |
| [**`launcher/README.md`**](launcher/README.md) | Child-process verification (`secureexec`), signed release manifests, and `--parent-stdin` lifeline details. |
| [**`sqlite3/README.md`**](sqlite3/README.md) | Low-level `hqq-bindgen` IDL, C server hooks, and path-sandboxing policy. |
| [**`PROTOCOL.md`**](PROTOCOL.md) | Wire protocol specification and resource limits. |
