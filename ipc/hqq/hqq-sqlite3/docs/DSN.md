# DSN Reference and Control Knobs (`gosuda.org/sqlite3`)

`gosuda.org/sqlite3` accepts both standard SQLite file / `:memory:` DSN strings (`app.db?_journal=WAL`, `file:app.db?mode=ro`, `:memory:`) and explicit listener URIs (`hqq://<listener>?db=<alias-or-path>`). Query parameters are translated into session transport settings, transaction lock modes, timezone locations, and `PRAGMA` statements executed when each `database/sql` connection opens. Both explicit driver keys (`busy`, `txlock`, `pragma.<name>`) and standard SQLite driver underscore-prefixed control knobs (`_busy_timeout`, `_txlock`, `_fk`, `_journal_mode`, `_synchronous`, `_loc`, and others) are supported so existing `github.com/mattn/go-sqlite3` applications can migrate without rewriting connection strings.

---

## 1. DSN Syntax

### 1.1 Drop-In File and `:memory:` DSNs (Auto-Launched Server)

When `sql.Open("sqlite3", dsn)` or `sqlite.ConnectorFromDSN(dsn)` is called with a file path, `file:` URI, or `:memory:` (without the `hqq://` scheme), the driver automatically extracts the embedded `hqq-sqlite-server` binary for the current OS/Arch, verifies its SHA-256 digest via `secureexec`, and starts a background shared-memory server scoped to the database file's directory:

```text
<path-or-:memory:>[?<key>=<value>...]
file:<path-or-:memory:>[?<key>=<value>...]
```

```go
// File path with standard control knobs:
db, err := sql.Open("sqlite3", "app.db?_journal=WAL&_sync=NORMAL&_fk=1&_busy_timeout=5000&_txlock=immediate")

// In-memory database:
memDB, err := sql.Open("sqlite3", ":memory:")
```

### 1.2 Explicit Listener URI (`hqq://`)

To connect to a specific `hqq-sqlite-server` listener started via `launcher.Start` or externally, use the `hqq://` scheme:

```text
hqq://<listener>?db=<alias-or-path>[&<key>=<value>...]
```

| URI Component | Requirement | Example |
| :--- | :--- | :--- |
| Scheme | Must be `hqq`. | `hqq://` |
| Host (`<listener>`) | HQQ listener name of the running `hqq-sqlite-server` instance. No port, path, userinfo, or fragment is permitted. | `sqlite-svc` |
| Query (`?<key>=<value>`) | URL-encoded key-value pairs. Must include `db`. Unknown keys, empty values, or duplicate/conflicting keys fail during `ParseDSN`. | `?db=main&_journal=WAL&_busy_timeout=5000` |

```go
db, err := sql.Open(
	"sqlite3",
	"hqq://sqlite-svc?db=main&mode=rwc&_journal=WAL&_sync=NORMAL&_fk=1&_busy_timeout=5000&_txlock=immediate&_loc=UTC",
)
```

---

## 2. Connection and Driver Parameters

Connection parameters configure the HQQ client session, `sqlite3_open_v2` flags, lock wait timeouts, batching bounds, and timestamp handling:

| Query Key(s) | Accepted Values | Default | `sqlite.Config` Field | Behavior |
| :--- | :--- | :--- | :--- | :--- |
| `db` | Non-empty alias name or relative path inside `--allow-dir` (`1..4096` bytes) | *(Required)* | `Database` | Target database passed to `sqlite3_open_v2` after server policy resolution. |
| `mode` | `rwc`, `rw`, `ro` | `rwc` | `Mode` | Selects `SQLITE_OPEN_READWRITE\|SQLITE_OPEN_CREATE` (`rwc`), `SQLITE_OPEN_READWRITE` (`rw`), or `SQLITE_OPEN_READONLY` (`ro`). |
| `timeout` | Go duration (`5s`, `500ms`) `> 0` | `5s` | `Timeout` | Per-operation RPC deadline when the caller's `context.Context` has no deadline. |
| `fetch` | Integer `1..4096` | `256` | `FetchSize` | Maximum rows fetched per `fetch_rows` batch during `Query` / `QueryContext`. |
| `busy`, `_busy_timeout`, `_timeout` | Go duration (`5s`, `250ms`) or integer milliseconds (`5000`, `0`) `>= 0` | `5s` | `BusyTimeout` | Configures `sqlite3_busy_timeout(db, ms)`. Passing `0` (`0s` or `0`) disables the busy handler (`BusyTimeout < 0` in `Config`) so lock conflicts return `SQLITE_BUSY` immediately. |
| `txlock`, `_txlock` | `deferred`, `immediate`, `exclusive` (case-insensitive for `_txlock`) | `deferred` | `TxMode` | Default transaction mode for read-write `Begin` / `BeginTx` calls (`BEGIN DEFERRED`, `BEGIN IMMEDIATE`, or `BEGIN EXCLUSIVE`). |
| `loc`, `_loc` | `auto` or any IANA timezone name accepted by `time.LoadLocation` (`UTC`, `Local`, `Asia/Seoul`) | `nil` (preserve `time.Time` zone; parse offset-free strings as UTC) | `Location` | Converts bound `time.Time` parameters via `t.In(loc)` before formatting, and parses offset-free `DATE`/`DATETIME`/`TIMESTAMP` column strings in `loc` (`auto` selects `time.Local`). |

---

## 3. Underscore-Prefixed PRAGMA Control Knobs

Underscore-prefixed control knobs translate into validated `PRAGMA <name> = <value>;` statements executed in deterministic alphabetical order immediately after `sqlite3_open_v2` and `sqlite3_busy_timeout`:

| Query Key(s) | Target SQLite Pragma | Accepted Values | Canonical Wire Value |
| :--- | :--- | :--- | :--- |
| `_fk`, `_foreign_keys` | `PRAGMA foreign_keys` | `true`, `false`, `1`, `0`, `on`, `off`, `yes`, `no` (case-insensitive) | `1` or `0` |
| `_defer_fk`, `_defer_foreign_keys` | `PRAGMA defer_foreign_keys` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_journal`, `_journal_mode` | `PRAGMA journal_mode` | `DELETE`, `TRUNCATE`, `PERSIST`, `MEMORY`, `WAL`, `OFF` (case-insensitive) | Uppercase mode (`WAL`, `DELETE`, ...) |
| `_sync`, `_synchronous` | `PRAGMA synchronous` | `0`, `1`, `2`, `3`, `OFF`, `NORMAL`, `FULL`, `EXTRA` (case-insensitive) | Uppercase name or digit |
| `_vacuum`, `_auto_vacuum` | `PRAGMA auto_vacuum` | `0`, `1`, `2`, `NONE`, `FULL`, `INCREMENTAL` (case-insensitive) | Uppercase name or digit |
| `_cache_size` | `PRAGMA cache_size` | Signed 64-bit integer (`-2000` for KiB, `2000` for pages) | Base-10 integer |
| `_cslike`, `_case_sensitive_like` | `PRAGMA case_sensitive_like` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_ignore_check_constraints` | `PRAGMA ignore_check_constraints` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_locking`, `_locking_mode` | `PRAGMA locking_mode` | `NORMAL`, `EXCLUSIVE` (case-insensitive) | `NORMAL` or `EXCLUSIVE` |
| `_query_only` | `PRAGMA query_only` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_rt`, `_recursive_triggers` | `PRAGMA recursive_triggers` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_secure_delete` | `PRAGMA secure_delete` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) or `FAST` | `1`, `0`, or `FAST` |
| `_temp_store` | `PRAGMA temp_store` | `0`, `1`, `2`, `DEFAULT`, `FILE`, `MEMORY` (case-insensitive) | Uppercase name or digit |
| `_writable_schema` | `PRAGMA writable_schema` | Boolean (`true`/`false`, `1`/`0`, `on`/`off`, `yes`/`no`) | `1` or `0` |
| `_mmap_size` | `PRAGMA mmap_size` | Signed 64-bit integer `>= 0` | Base-10 integer |
| `_page_size` | `PRAGMA page_size` | Power-of-two integer in `512..65536` | Base-10 integer |
| `_wal_autocheckpoint` | `PRAGMA wal_autocheckpoint` | Signed 64-bit integer `>= 0` | Base-10 integer |

---

## 4. Arbitrary Pragmas (`pragma.<name>`)

Any SQLite pragma not listed in Section 3 can be passed via `pragma.<name>=<value>`:

```text
hqq://sqlite-svc?db=main&pragma.busy_timeout=5000&pragma.analysis_limit=400
```

To prevent SQL injection during connection bootstrap, both the driver and the C server validate pragma identifiers and values against strict character-set bounds:

- **Pragma name (`<name>`)**: must match `^[a-z_][a-z0-9_]{0,63}$`.
- **Pragma value (`<value>`)**: must match `^[A-Za-z0-9_.+-]{1,255}$`.
- **Count bound**: at most `32` pragmas (`wire.MaxPragmas`) per connection.

---

## 5. Duplicate and Alias Conflict Rules

`ParseDSN` fails closed on ambiguous query strings:

1. **Repeated query keys**: passing the same key twice (`?db=a&db=b` or `?_fk=1&_fk=0`) is rejected with an error.
2. **Alias collisions**: mixing synonymous driver options in the same DSN (`busy=5s&_busy_timeout=5000`, `txlock=immediate&_txlock=exclusive`, or `loc=UTC&_loc=Local`) is rejected with `option "<key>" conflicts with another option for the same setting`.
3. **Pragma collisions**: specifying the same underlying pragma via two aliases (`_fk=1&_foreign_keys=1`) or via both a shorthand knob and `pragma.<name>` (`_journal=WAL&pragma.journal_mode=WAL`) is rejected with `duplicate pragma "<name>"`.

---

## 6. Programmatic Configuration (`sqlite.Config`)

Applications that construct a connector via `sqlite.NewConnector(listener, cfg)` instead of a DSN string can configure the exact same parameters on `sqlite.Config`:

```go
loc, err := time.LoadLocation("Asia/Seoul")
if err != nil {
	return err
}

conn, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
	Database:    "main",
	Mode:        sqlite.ModeReadWriteCreate,
	Timeout:     5 * time.Second,
	FetchSize:   256,
	BusyTimeout: 5 * time.Second, // set < 0 to disable busy waiting (0 ms)
	TxMode:      sqlite.TxImmediate,
	Location:    loc,
	Pragmas: map[string]string{
		"journal_mode": "WAL",
		"synchronous":  "NORMAL",
		"foreign_keys": "1",
	},
})
if err != nil {
	return err
}
db := sql.OpenDB(conn)
defer db.Close()
```

---

## 7. Type Mapping and Timestamp Formatting

### 7.1 Bound Query Parameters (Go -> SQLite)

| Go Argument Type | SQLite Storage Class | Conversion Rules |
| :--- | :--- | :--- |
| `nil` | `NULL` | Bound via `sqlite3_bind_null`. |
| `int`, `int8`, `int16`, `int32`, `int64`, `uint`, `uint8`, `uint16`, `uint32` | `INTEGER` | Sign-extended or zero-extended to `int64`. |
| `uint64` | `INTEGER` | Values `<= math.MaxInt64` bind as `int64`; values `> math.MaxInt64` fail before RPC transmission. |
| `bool` | `INTEGER` | `true` -> `1`, `false` -> `0`. |
| `float32`, `float64` | `REAL` | IEEE-754 binary64. `NaN` is rejected with an error; `+Inf` and `-Inf` are preserved. |
| `string` | `TEXT` | Raw bytes bound with `SQLITE_TRANSIENT` (embedded NUL bytes are preserved via `bind_text`). |
| `[]byte`, `json.RawMessage` | `BLOB` | Bound as `BLOB` (`nil` or `len == 0` binds a 0-byte blob). Convert `json.RawMessage` to `string` to store `TEXT`. |
| `time.Time` | `TEXT` | If `Config.Location != nil` (or `_loc` is set), converted via `t.In(loc)` first. Formatted with layout `"2006-01-02 15:04:05.999999999-07:00"`. |
| `driver.Valuer` (`sql.Null*`) | Resolved value | `Value()` is evaluated before applying the rules above. |

### 7.2 Result Columns (SQLite -> Go)

| SQLite Storage Class | Column Declared Type | Returned `driver.Value` |
| :--- | :--- | :--- |
| `INTEGER` | Any | `int64` |
| `FLOAT` | Any | `float64` |
| `BLOB` | Any | `[]byte` (copied; safe to retain after ` rows.Next()`) |
| `NULL` | Any | `nil` |
| `TEXT` | Any type other than `DATE`, `DATETIME`, `TIMESTAMP` | `string` |
| `TEXT` | `DATE`, `DATETIME`, `TIMESTAMP` (matched case-insensitively on the leading type token) | Parsed `time.Time` when the string matches an ISO-8601 / RFC 3339 date or timestamp layout; falls back to `string` if parsing fails. Strings with an explicit offset (`Z` or `±HH:MM`) keep that zone; offset-free strings (`"2006-01-02 15:04:05"`) are interpreted in `Config.Location` (or `time.UTC` when `Location` is `nil`). |
