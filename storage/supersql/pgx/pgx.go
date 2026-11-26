// Package pgx provides a cleanroom, zero-external-dependency, in-process
// pgx/v5-compatible API over supersql.
package pgx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var (
	// ErrNoRows occurs when rows are expected but none are returned.
	ErrNoRows = sql.ErrNoRows

	// ErrTxClosed occurs when an operation is performed on a closed transaction.
	ErrTxClosed = sql.ErrTxDone

	// ErrTxCommitRollback occurs when a commit fails and the transaction is rolled back.
	ErrTxCommitRollback = errors.New("pgx: commit unexpectedly resulted in rollback")
)

// PgError represents a PostgreSQL server error compatible with pgconn.PgError.
type PgError struct {
	Severity       string
	Code           string
	Message        string
	Detail         string
	Hint           string
	Position       int32
	ConstraintName string
	TableName      string
	ColumnName     string
	cause          error
}

// Error returns the formatted PostgreSQL diagnostic string.
func (e *PgError) Error() string {
	if e == nil {
		return ""
	}
	if e.Severity != "" && e.Code != "" {
		return fmt.Sprintf("%s: %s (SQLSTATE %s)", e.Severity, e.Message, e.Code)
	}
	if e.Message != "" {
		return e.Message
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return e.Code
}

// Unwrap returns the underlying error wrapped by this PgError.
func (e *PgError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// SQLState returns the five-character PostgreSQL SQLSTATE code.
func (e *PgError) SQLState() string {
	if e == nil {
		return ""
	}
	return e.Code
}

func sqlState(err error) string {
	var pgErr *PgError
	if errors.As(err, &pgErr) && pgErr.Code != "" {
		return pgErr.Code
	}
	return supersql.SQLState(err)
}

func wrapPgError(err error) error {
	if err == nil {
		return nil
	}
	var existing *PgError
	if errors.As(err, &existing) {
		return err
	}
	var sqlErr *supersql.Error
	if !errors.As(err, &sqlErr) {
		return err
	}
	return &PgError{
		Severity:       "ERROR",
		Code:           sqlErr.Code,
		Message:        sqlErr.Message,
		Detail:         sqlErr.Detail,
		Hint:           sqlErr.Hint,
		Position:       int32(sqlErr.Position),
		ConstraintName: sqlErr.Constraint,
		TableName:      sqlErr.Table,
		ColumnName:     sqlErr.Column,
		cause:          err,
	}
}

// CommandTag is the result tag returned by an Exec command.
type CommandTag struct {
	s            string
	rowsAffected int64
}

// NewCommandTag parses a command tag string.
func NewCommandTag(s string) CommandTag {
	s = strings.TrimSpace(s)
	var rows int64
	if idx := strings.LastIndexByte(s, ' '); idx >= 0 {
		if n, err := strconv.ParseInt(s[idx+1:], 10, 64); err == nil {
			rows = n
		}
	}
	return CommandTag{s: s, rowsAffected: rows}
}

func newCommandTagFromResult(res sql.Result, defaultVerb string) CommandTag {
	if res == nil {
		return CommandTag{s: defaultVerb}
	}
	rows, _ := res.RowsAffected()
	if tagger, ok := res.(interface{ CommandTag() string }); ok {
		if tag := tagger.CommandTag(); tag != "" {
			return CommandTag{s: tag, rowsAffected: rows}
		}
	}
	return formatVerbCommandTag(defaultVerb, rows)
}

func formatVerbCommandTag(verb string, rows int64) CommandTag {
	verb = strings.ToUpper(strings.TrimSpace(verb))
	switch verb {
	case "INSERT":
		return CommandTag{s: fmt.Sprintf("INSERT 0 %d", rows), rowsAffected: rows}
	case "UPDATE", "DELETE", "SELECT", "MOVE", "FETCH", "COPY", "MERGE":
		return CommandTag{s: fmt.Sprintf("%s %d", verb, rows), rowsAffected: rows}
	default:
		return CommandTag{s: verb, rowsAffected: rows}
	}
}

// String returns the command tag string.
func (ct CommandTag) String() string { return ct.s }

// RowsAffected returns the number of rows affected by the command.
func (ct CommandTag) RowsAffected() int64 { return ct.rowsAffected }

// Insert reports whether the command tag is an INSERT tag.
func (ct CommandTag) Insert() bool { return strings.HasPrefix(ct.s, "INSERT") }

// Update reports whether the command tag is an UPDATE tag.
func (ct CommandTag) Update() bool { return strings.HasPrefix(ct.s, "UPDATE") }

// Delete reports whether the command tag is a DELETE tag.
func (ct CommandTag) Delete() bool { return strings.HasPrefix(ct.s, "DELETE") }

// Select reports whether the command tag is a SELECT tag.
func (ct CommandTag) Select() bool { return strings.HasPrefix(ct.s, "SELECT") }

// Empty reports whether the command tag is empty.
func (ct CommandTag) Empty() bool { return ct.s == "" }

// TxIsoLevel represents the transaction isolation level.
type TxIsoLevel string

// Transaction isolation levels supported by TxOptions.
const (
	Serializable    TxIsoLevel = "serializable"
	RepeatableRead  TxIsoLevel = "repeatable read"
	ReadCommitted   TxIsoLevel = "read committed"
	ReadUncommitted TxIsoLevel = "read uncommitted"
)

// TxAccessMode represents the transaction access mode.
type TxAccessMode string

// Transaction access modes supported by TxOptions.
const (
	ReadWrite TxAccessMode = "read write"
	ReadOnly  TxAccessMode = "read only"
)

// TxDeferrableMode represents the transaction deferrable mode.
type TxDeferrableMode string

// Transaction deferrable modes supported by TxOptions.
const (
	Deferrable    TxDeferrableMode = "deferrable"
	NotDeferrable TxDeferrableMode = "not deferrable"
)

// TxOptions configures a transaction started with BeginTx.
type TxOptions struct {
	IsoLevel       TxIsoLevel
	AccessMode     TxAccessMode
	DeferrableMode TxDeferrableMode
	BeginQuery     string
}

func (txOptions TxOptions) toSQLTxOptions() *sql.TxOptions {
	opts := &sql.TxOptions{
		ReadOnly: txOptions.AccessMode == ReadOnly,
	}
	switch txOptions.IsoLevel {
	case "":
		opts.Isolation = sql.LevelDefault
	case Serializable:
		opts.Isolation = sql.LevelSerializable
	case RepeatableRead:
		opts.Isolation = sql.LevelRepeatableRead
	case ReadCommitted:
		opts.Isolation = sql.LevelReadCommitted
	case ReadUncommitted:
		opts.Isolation = sql.LevelReadUncommitted
	default:
		opts.Isolation = sql.LevelDefault
	}
	return opts
}

// FieldDescription describes a single result column.
type FieldDescription struct {
	Name         string
	DataTypeOID  uint32
	TypeModifier int32
}

// Row is a convenience wrapper over Rows for QueryRow.
type Row interface {
	Scan(dest ...any) error
}

// CollectableRow is the subset of Rows methods accessible to a RowToFunc.
type CollectableRow interface {
	Scan(dest ...any) error
	FieldDescriptions() []FieldDescription
	Values() ([]any, error)
	RawValues() [][]byte
}

// Rows is the result set returned from Query.
type Rows interface {
	Close()
	Err() error
	CommandTag() CommandTag
	FieldDescriptions() []FieldDescription
	Next() bool
	Scan(dest ...any) error
	Values() ([]any, error)
	RawValues() [][]byte
	Conn() *Conn
}

// QueryRewriter rewrites a query and its arguments before execution.
type QueryRewriter interface {
	RewriteQuery(ctx context.Context, conn *Conn, sql string, args []any) (newSQL string, newArgs []any, err error)
}

// NamedArgs maps named parameters (@name) in a SQL query to values.
type NamedArgs map[string]any

var _ QueryRewriter = NamedArgs(nil)

// RewriteQuery replaces @name placeholders with positional $1, $2, ... parameters.
func (na NamedArgs) RewriteQuery(_ context.Context, _ *Conn, query string, _ []any) (string, []any, error) {
	return rewriteNamedArgs(query, na)
}

func rewriteNamedArgs(query string, na NamedArgs) (string, []any, error) {
	var b strings.Builder
	b.Grow(len(query))
	var args []any
	ordinals := make(map[string]int, len(na))

	i := 0
	n := len(query)
	for i < n {
		ch := query[i]
		// Single-quoted literal
		if ch == '\'' {
			b.WriteByte(ch)
			i++
			for i < n {
				c := query[i]
				b.WriteByte(c)
				i++
				if c == '\'' {
					if i < n && query[i] == '\'' {
						b.WriteByte(query[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}
		// Double-quoted identifier
		if ch == '"' {
			b.WriteByte(ch)
			i++
			for i < n {
				c := query[i]
				b.WriteByte(c)
				i++
				if c == '"' {
					if i < n && query[i] == '"' {
						b.WriteByte(query[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}
		// Line comment --
		if ch == '-' && i+1 < n && query[i+1] == '-' {
			for i < n && query[i] != '\n' {
				b.WriteByte(query[i])
				i++
			}
			continue
		}
		// Block comment /* ... */
		if ch == '/' && i+1 < n && query[i+1] == '*' {
			b.WriteString("/*")
			i += 2
			for i < n {
				if query[i] == '*' && i+1 < n && query[i+1] == '/' {
					b.WriteString("*/")
					i += 2
					break
				}
				b.WriteByte(query[i])
				i++
			}
			continue
		}
		// Dollar-quoted literal $tag$...$tag$
		if ch == '$' && (i+1 < n && (query[i+1] == '$' || unicode.IsLetter(rune(query[i+1])) || query[i+1] == '_')) {
			endTag := strings.IndexByte(query[i+1:], '$')
			if endTag >= 0 {
				tag := query[i : i+1+endTag+1]
				b.WriteString(tag)
				i += len(tag)
				closeIdx := strings.Index(query[i:], tag)
				if closeIdx >= 0 {
					b.WriteString(query[i : i+closeIdx+len(tag)])
					i += closeIdx + len(tag)
				} else {
					b.WriteString(query[i:])
					i = n
				}
				continue
			}
		}
		// Named argument @ident (skip @@ operator)
		if ch == '@' {
			if i+1 < n && query[i+1] == '@' {
				b.WriteString("@@")
				i += 2
				continue
			}
			start := i + 1
			end := start
			for end < n {
				r := rune(query[end])
				if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
					end++
				} else {
					break
				}
			}
			if end > start {
				name := query[start:end]
				val, ok := na[name]
				if !ok {
					return "", nil, fmt.Errorf("pgx: missing named argument %q", name)
				}
				ord, exists := ordinals[name]
				if !exists {
					args = append(args, val)
					ord = len(args)
					ordinals[name] = ord
				}
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(ord))
				i = end
				continue
			}
		}
		b.WriteByte(ch)
		i++
	}
	return b.String(), args, nil
}

func prepareQueryAndArgs(ctx context.Context, conn *Conn, query string, args []any) (string, []any, error) {
	if len(args) == 1 {
		if rewriter, ok := args[0].(QueryRewriter); ok {
			return rewriter.RewriteQuery(ctx, conn, query, args)
		}
	}
	return query, args, nil
}

// skipSQLSpaceAndComments advances pos past whitespace, line comments (--),
// and nested block comments (/* ... */).
func skipSQLSpaceAndComments(s string, pos int) int {
	n := len(s)
	for pos < n {
		if unicode.IsSpace(rune(s[pos])) {
			pos++
			continue
		}
		if s[pos] == '-' && pos+1 < n && s[pos+1] == '-' {
			pos += 2
			for pos < n && s[pos] != '\n' {
				pos++
			}
			continue
		}
		if s[pos] == '/' && pos+1 < n && s[pos+1] == '*' {
			depth := 1
			pos += 2
			for pos < n && depth > 0 {
				if s[pos] == '/' && pos+1 < n && s[pos+1] == '*' {
					depth++
					pos += 2
					continue
				}
				if s[pos] == '*' && pos+1 < n && s[pos+1] == '/' {
					depth--
					pos += 2
					continue
				}
				pos++
			}
			continue
		}
		break
	}
	return pos
}

// nextSQLWord reads the next SQL token (word, punctuation, or skipped literal)
// starting at pos. It returns the uppercased word (or single punctuation byte)
// and the position immediately following the token.
func nextSQLWord(s string, pos int) (tok string, next int) {
	pos = skipSQLSpaceAndComments(s, pos)
	n := len(s)
	if pos >= n {
		return "", n
	}
	ch := s[pos]
	// Single-quoted string literal
	if ch == '\'' {
		pos++
		for pos < n {
			if s[pos] == '\'' {
				pos++
				if pos < n && s[pos] == '\'' {
					pos++
					continue
				}
				break
			}
			pos++
		}
		return "'", pos
	}
	// Double-quoted identifier
	if ch == '"' {
		start := pos
		pos++
		for pos < n {
			if s[pos] == '"' {
				pos++
				if pos < n && s[pos] == '"' {
					pos++
					continue
				}
				break
			}
			pos++
		}
		return s[start:pos], pos
	}
	// Dollar-quoted literal $tag$...$tag$
	if ch == '$' && pos+1 < n && (s[pos+1] == '$' || unicode.IsLetter(rune(s[pos+1])) || s[pos+1] == '_') {
		if endTag := strings.IndexByte(s[pos+1:], '$'); endTag >= 0 {
			tag := s[pos : pos+1+endTag+1]
			pos += len(tag)
			if closeIdx := strings.Index(s[pos:], tag); closeIdx >= 0 {
				pos += closeIdx + len(tag)
			} else {
				pos = n
			}
			return "$", pos
		}
	}
	if unicode.IsLetter(rune(ch)) || ch == '_' {
		start := pos
		pos++
		for pos < n && (unicode.IsLetter(rune(s[pos])) || unicode.IsDigit(rune(s[pos])) || s[pos] == '_' || s[pos] == '$') {
			pos++
		}
		return strings.ToUpper(s[start:pos]), pos
	}
	return string(ch), pos + 1
}

func skipBalancedParens(s string, pos int) int {
	depth := 1
	for pos < len(s) && depth > 0 {
		tok, next := nextSQLWord(s, pos)
		pos = next
		switch tok {
		case "(":
			depth++
		case ")":
			depth--
		case "":
			return pos
		}
	}
	return pos
}

func parseStatementVerb(sqlText string) string {
	verb, pos := nextSQLWord(sqlText, 0)
	if verb == "" {
		return ""
	}
	if verb == "WITH" {
		// Advance past WITH [RECURSIVE] cte1 [(cols)] AS [NOT] [MATERIALIZED] (...) [, ...]
		tok, next := nextSQLWord(sqlText, pos)
		if tok == "RECURSIVE" {
			pos = next
		}
		for pos < len(sqlText) {
			tok, next = nextSQLWord(sqlText, pos)
			pos = next
			if tok == "" {
				break
			}
			if tok == "(" {
				pos = skipBalancedParens(sqlText, pos)
				continue
			}
			// After closing a CTE body, look for a top-level statement keyword.
			switch tok {
			case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "VALUES", "TABLE":
				if tok == "VALUES" || tok == "TABLE" {
					return "SELECT"
				}
				return tok
			}
		}
		return "SELECT"
	}
	switch verb {
	case "VALUES", "TABLE":
		return "SELECT"
	case "CREATE", "ALTER", "DROP":
		obj, next := nextSQLWord(sqlText, pos)
		for obj == "OR" || obj == "REPLACE" || obj == "TEMP" || obj == "TEMPORARY" || obj == "UNLOGGED" || obj == "UNIQUE" || obj == "RECURSIVE" {
			if obj == "OR" {
				// skip following REPLACE
				_, next = nextSQLWord(sqlText, next)
			}
			obj, next = nextSQLWord(sqlText, next)
		}
		if obj == "MATERIALIZED" {
			w2, _ := nextSQLWord(sqlText, next)
			if w2 == "VIEW" {
				return verb + " MATERIALIZED VIEW"
			}
		}
		if obj != "" && unicode.IsLetter(rune(obj[0])) {
			return verb + " " + obj
		}
		return verb
	default:
		return verb
	}
}

// Identifier represents a PostgreSQL schema/table/column identifier path.
type Identifier []string

// Sanitize quotes each component of the identifier for safe inclusion in SQL.
func (ident Identifier) Sanitize() string {
	parts := make([]string, len(ident))
	for i, part := range ident {
		parts[i] = `"` + strings.ReplaceAll(part, `"`, `""`) + `"`
	}
	return strings.Join(parts, ".")
}

// PoolConfig configures a Pool.
type PoolConfig struct {
	OpenOptions     supersql.OpenOptions
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	BeforeConnect   func(context.Context, *PoolConfig) error
	AfterConnect    func(context.Context, *Conn) error
	BeforeAcquire   func(context.Context, *Conn) bool
	AfterRelease    func(*Conn) bool
}

// hookConnector wraps a driver.Connector to support PoolConfig lifecycle hooks.
type hookConnector struct {
	base          driver.Connector
	cfg           PoolConfig
	beforeConnect func(context.Context, *PoolConfig) error
}

func (hc *hookConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if hc.beforeConnect != nil {
		cfgCopy := hc.cfg
		if err := hc.beforeConnect(ctx, &cfgCopy); err != nil {
			return nil, err
		}
	}
	raw, err := hc.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	conn := &hookConn{inner: raw}
	conn.isNew.Store(true)
	return conn, nil
}

func (hc *hookConnector) Driver() driver.Driver {
	return hc.base.Driver()
}

type hookConn struct {
	inner     driver.Conn
	isNew     atomic.Bool
	discarded atomic.Bool
}

func (c *hookConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(query)
}

func (c *hookConn) Close() error {
	return c.inner.Close()
}

func (c *hookConn) Begin() (driver.Tx, error) {
	return c.inner.Begin()
}

func (c *hookConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.inner.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.inner.Prepare(query)
}

func (c *hookConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.inner.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

func (c *hookConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := c.inner.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *hookConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.inner.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *hookConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *hookConn) ResetSession(ctx context.Context) error {
	if c.discarded.Load() {
		return driver.ErrBadConn
	}
	if sr, ok := c.inner.(driver.SessionResetter); ok {
		return sr.ResetSession(ctx)
	}
	return nil
}

func (c *hookConn) IsValid() bool {
	if c.discarded.Load() {
		return false
	}
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *hookConn) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := c.inner.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// PoolStat reports connection pool statistics compatible with pgxpool.Stat.
type PoolStat struct {
	stats sql.DBStats
}

// TotalConns returns the total number of open connections in the pool.
func (s PoolStat) TotalConns() int32 { return int32(s.stats.OpenConnections) }

// AcquiredConns returns the number of connections currently checked out of the pool.
func (s PoolStat) AcquiredConns() int32 { return int32(s.stats.InUse) }

// IdleConns returns the number of idle connections currently held in the pool.
func (s PoolStat) IdleConns() int32 { return int32(s.stats.Idle) }

// Pool is a concurrency-safe connection pool compatible with pgxpool.Pool.
type Pool struct {
	connector     *supersql.Connector
	db            *sql.DB
	ownsConnector bool
	closed        atomic.Bool
	afterConnect  func(context.Context, *Conn) error
	beforeAcquire func(context.Context, *Conn) bool
	afterRelease  func(*Conn) bool
}

// OpenPool opens a supersql database and returns a Pool over it.
func OpenPool(ctx context.Context, opts supersql.OpenOptions) (*Pool, error) {
	return OpenPoolWithConfig(ctx, PoolConfig{OpenOptions: opts})
}

// OpenPoolWithConfig opens a supersql database with pool sizing and lifecycle hooks.
func OpenPoolWithConfig(ctx context.Context, cfg PoolConfig) (*Pool, error) {
	connector, err := supersql.OpenConnector(ctx, cfg.OpenOptions)
	if err != nil {
		return nil, wrapPgError(err)
	}
	var dbConnector driver.Connector = connector
	if cfg.BeforeConnect != nil || cfg.AfterConnect != nil || cfg.BeforeAcquire != nil || cfg.AfterRelease != nil {
		dbConnector = &hookConnector{
			base:          connector,
			cfg:           cfg,
			beforeConnect: cfg.BeforeConnect,
		}
	}
	pool := &Pool{
		connector:     connector,
		db:            sql.OpenDB(dbConnector),
		ownsConnector: true,
		afterConnect:  cfg.AfterConnect,
		beforeAcquire: cfg.BeforeAcquire,
		afterRelease:  cfg.AfterRelease,
	}
	if cfg.MaxConns > 0 {
		pool.db.SetMaxOpenConns(int(cfg.MaxConns))
	}
	if cfg.MinConns > 0 {
		pool.db.SetMaxIdleConns(int(cfg.MinConns))
	}
	if cfg.MaxConnLifetime > 0 {
		pool.db.SetConnMaxLifetime(cfg.MaxConnLifetime)
	}
	if cfg.MaxConnIdleTime > 0 {
		pool.db.SetConnMaxIdleTime(cfg.MaxConnIdleTime)
	}
	return pool, nil
}

// NewPoolFromConnector creates a Pool over an existing supersql.Connector.
// Calling Close on the returned Pool closes its sql.DB pool; the caller retains
// ownership of closing the Connector unless OpenPool constructed it.
func NewPoolFromConnector(connector *supersql.Connector) *Pool {
	db := sql.OpenDB(connector)
	return &Pool{
		connector: connector,
		db:        db,
	}
}

// NewPoolFromCatalog creates a Pool over an already-opened supersql.Catalog.
func NewPoolFromCatalog(catalog *supersql.Catalog) *Pool {
	connector := supersql.NewConnectorWithCatalog(catalog)
	return NewPoolFromConnector(connector)
}

// Close closes all connections in the pool and, if opened via OpenPool, closes
// the underlying Catalog.
func (p *Pool) Close() {
	if p == nil || !p.closed.CompareAndSwap(false, true) {
		return
	}
	_ = p.db.Close()
	if p.ownsConnector && p.connector != nil {
		_ = p.connector.Close()
	}
}

// StdDB returns the underlying *sql.DB handle.
func (p *Pool) StdDB() *sql.DB { return p.db }

// Catalog returns the underlying *supersql.Catalog.
func (p *Pool) Catalog() *supersql.Catalog {
	if p == nil || p.connector == nil {
		return nil
	}
	return p.connector.Catalog()
}

// Stat returns pool connection statistics.
func (p *Pool) Stat() PoolStat {
	return PoolStat{stats: p.db.Stats()}
}

// Ping verifies the pool can acquire a session and communicate with the engine.
func (p *Pool) Ping(ctx context.Context) error {
	return wrapPgError(p.db.PingContext(ctx))
}

// Acquire checks out an isolated session connection from the pool.
func (p *Pool) Acquire(ctx context.Context) (*PoolConn, error) {
	for attempt := 0; attempt < 4; attempt++ {
		sqlConn, err := p.db.Conn(ctx)
		if err != nil {
			return nil, wrapPgError(err)
		}
		c := &Conn{sqlConn: sqlConn, pool: p}
		var hc *hookConn
		if p.afterConnect != nil || p.beforeAcquire != nil || p.afterRelease != nil {
			_ = sqlConn.Raw(func(driverConn any) error {
				hc, _ = driverConn.(*hookConn)
				return nil
			})
		}
		isNew := hc != nil && hc.isNew.CompareAndSwap(true, false)
		if isNew && p.afterConnect != nil {
			if err := p.afterConnect(ctx, c); err != nil {
				if hc != nil {
					hc.discarded.Store(true)
				}
				_ = sqlConn.Close()
				return nil, wrapPgError(err)
			}
		}
		if p.beforeAcquire != nil && !p.beforeAcquire(ctx, c) {
			if hc != nil {
				hc.discarded.Store(true)
			}
			_ = sqlConn.Close()
			if !isNew && ctx.Err() == nil {
				continue
			}
			return nil, errors.New("pgx: connection rejected by BeforeAcquire")
		}
		return &PoolConn{conn: c}, nil
	}
	return nil, errors.New("pgx: connection rejected by BeforeAcquire")
}

// AcquireFunc acquires a connection, runs fn, and releases the connection.
func (p *Pool) AcquireFunc(ctx context.Context, fn func(*PoolConn) error) error {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pc.Release()
	return fn(pc)
}

// Exec executes a SQL statement on a pooled connection and returns its CommandTag.
func (p *Pool) Exec(ctx context.Context, sqlText string, arguments ...any) (CommandTag, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return CommandTag{}, err
	}
	defer pc.Release()
	return pc.Exec(ctx, sqlText, arguments...)
}

// Query executes a query on a pooled connection and returns streaming Rows.
// The acquired connection is automatically released when Rows.Close() is called.
func (p *Pool) Query(ctx context.Context, sqlText string, args ...any) (Rows, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return newClosedErrorRows(nil, err), err
	}
	rows, err := pc.conn.queryWithRelease(ctx, pc.Release, sqlText, args...)
	if err != nil {
		pc.Release()
		return rows, err
	}
	return rows, nil
}

// QueryRow executes a query expected to return at most one row.
func (p *Pool) QueryRow(ctx context.Context, sqlText string, args ...any) Row {
	rows, _ := p.Query(ctx, sqlText, args...)
	return &pgxRow{rows: rows}
}

// Begin starts a transaction with default (SERIALIZABLE) isolation on an acquired connection.
func (p *Pool) Begin(ctx context.Context) (Tx, error) {
	return p.BeginTx(ctx, TxOptions{})
}

// BeginTx starts a transaction with TxOptions on an acquired connection.
// Calling Commit or Rollback automatically releases the connection back to the pool.
func (p *Pool) BeginTx(ctx context.Context, txOptions TxOptions) (Tx, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := pc.conn.beginTxInternal(ctx, txOptions, pc.Release)
	if err != nil {
		pc.Release()
		return nil, err
	}
	return tx, nil
}

// BeginFunc starts a transaction and runs fn, committing on nil error or rolling back otherwise.
func (p *Pool) BeginFunc(ctx context.Context, fn func(Tx) error) error {
	return BeginFunc(ctx, p, fn)
}

// SendBatch sends a batch of queries over a pooled connection inside an implicit transaction.
func (p *Pool) SendBatch(ctx context.Context, b *Batch) BatchResults {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return &batchResults{err: wrapPgError(err), closed: true}
	}
	return pc.conn.sendBatchWithRelease(ctx, b, pc.Release)
}

// CopyFrom performs a bulk insert of rows into tableName on an acquired connection.
func (p *Pool) CopyFrom(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer pc.Release()
	return pc.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

// PoolConn is an acquired connection from a Pool.
type PoolConn struct {
	conn     *Conn
	released atomic.Bool
}

// Release returns the connection to the Pool.
func (pc *PoolConn) Release() {
	if pc == nil || !pc.released.CompareAndSwap(false, true) {
		return
	}
	if pc.conn != nil && pc.conn.sqlConn != nil {
		if pc.conn.pool != nil && pc.conn.pool.afterRelease != nil {
			if !pc.conn.pool.afterRelease(pc.conn) {
				_ = pc.conn.sqlConn.Raw(func(driverConn any) error {
					if hc, ok := driverConn.(*hookConn); ok {
						hc.discarded.Store(true)
					}
					return nil
				})
			}
		}
		_ = pc.conn.sqlConn.Close()
	}
}

// Conn returns the underlying *Conn.
func (pc *PoolConn) Conn() *Conn { return pc.conn }

// Ping verifies the connection is still alive.
func (pc *PoolConn) Ping(ctx context.Context) error { return pc.conn.Ping(ctx) }

// Exec executes a SQL statement on this connection.
func (pc *PoolConn) Exec(ctx context.Context, sqlText string, arguments ...any) (CommandTag, error) {
	return pc.conn.Exec(ctx, sqlText, arguments...)
}

// Query executes a SQL query on this connection.
func (pc *PoolConn) Query(ctx context.Context, sqlText string, args ...any) (Rows, error) {
	return pc.conn.Query(ctx, sqlText, args...)
}

// QueryRow executes a query returning at most one row on this connection.
func (pc *PoolConn) QueryRow(ctx context.Context, sqlText string, args ...any) Row {
	return pc.conn.QueryRow(ctx, sqlText, args...)
}

// Begin starts a transaction on this connection.
func (pc *PoolConn) Begin(ctx context.Context) (Tx, error) {
	return pc.conn.Begin(ctx)
}

// BeginTx starts a transaction with options on this connection.
func (pc *PoolConn) BeginTx(ctx context.Context, txOptions TxOptions) (Tx, error) {
	return pc.conn.BeginTx(ctx, txOptions)
}

// BeginFunc starts a transaction on this connection and executes fn.
func (pc *PoolConn) BeginFunc(ctx context.Context, fn func(Tx) error) error {
	return BeginFunc(ctx, pc.conn, fn)
}

// SendBatch sends a batch of queries on this connection.
func (pc *PoolConn) SendBatch(ctx context.Context, b *Batch) BatchResults {
	return pc.conn.SendBatch(ctx, b)
}

// CopyFrom performs a bulk insert of rows on this connection.
func (pc *PoolConn) CopyFrom(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	return pc.conn.CopyFrom(ctx, tableName, columnNames, rowSrc)
}

// Conn represents a single isolated SQL session compatible with pgx.Conn.
type Conn struct {
	sqlConn   *sql.Conn
	pool      *Pool
	ownsPool  bool
	savepoint atomic.Uint64
	mu        sync.Mutex
	prepared  map[string]string
}

// OpenConn opens a single dedicated session connection over supersql.OpenOptions.
func OpenConn(ctx context.Context, opts supersql.OpenOptions) (*Conn, error) {
	pool, err := OpenPool(ctx, opts)
	if err != nil {
		return nil, err
	}
	sqlConn, err := pool.db.Conn(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	return &Conn{
		sqlConn:  sqlConn,
		pool:     pool,
		ownsPool: true,
	}, nil
}

// Close closes the connection.
func (c *Conn) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	err := c.sqlConn.Close()
	if c.ownsPool && c.pool != nil {
		c.pool.Close()
	}
	return wrapPgError(err)
}

// Ping verifies the connection is alive.
func (c *Conn) Ping(ctx context.Context) error {
	return wrapPgError(c.sqlConn.PingContext(ctx))
}

// Prepare registers a named prepared query on this session.
func (c *Conn) Prepare(ctx context.Context, name, sqlText string) error {
	stmt, err := c.sqlConn.PrepareContext(ctx, sqlText)
	if err != nil {
		return wrapPgError(err)
	}
	_ = stmt.Close()
	c.mu.Lock()
	if c.prepared == nil {
		c.prepared = make(map[string]string)
	}
	c.prepared[name] = sqlText
	c.mu.Unlock()
	return nil
}

// Deallocate removes a named prepared query from this session.
func (c *Conn) Deallocate(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.prepared, name)
	c.mu.Unlock()
	return nil
}

// DeallocateAll removes all named prepared queries from this session.
func (c *Conn) DeallocateAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	clear(c.prepared)
	c.mu.Unlock()
	return nil
}

func (c *Conn) resolvePrepared(sqlText string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepared != nil {
		if resolved, ok := c.prepared[sqlText]; ok {
			return resolved
		}
	}
	return sqlText
}

// Exec executes a statement and returns its CommandTag.
func (c *Conn) Exec(ctx context.Context, sqlText string, arguments ...any) (CommandTag, error) {
	sqlText = c.resolvePrepared(sqlText)
	rewritten, args, err := prepareQueryAndArgs(ctx, c, sqlText, arguments)
	if err != nil {
		return CommandTag{}, wrapPgError(err)
	}
	res, err := c.sqlConn.ExecContext(ctx, rewritten, args...)
	if err != nil {
		return CommandTag{}, wrapPgError(err)
	}
	return newCommandTagFromResult(res, parseStatementVerb(rewritten)), nil
}

// Query executes a query and returns streaming Rows.
func (c *Conn) Query(ctx context.Context, sqlText string, args ...any) (Rows, error) {
	return c.queryWithRelease(ctx, nil, sqlText, args...)
}

func (c *Conn) queryWithRelease(ctx context.Context, release func(), sqlText string, arguments ...any) (Rows, error) {
	sqlText = c.resolvePrepared(sqlText)
	rewritten, args, err := prepareQueryAndArgs(ctx, c, sqlText, arguments)
	if err != nil {
		err = wrapPgError(err)
		return newClosedErrorRows(c, err), err
	}
	sqlRows, err := c.sqlConn.QueryContext(ctx, rewritten, args...)
	if err != nil {
		err = wrapPgError(err)
		return newClosedErrorRows(c, err), err
	}
	return newPgxRows(c, sqlRows, release, parseStatementVerb(rewritten)), nil
}

// QueryRow executes a query expected to return at most one row.
func (c *Conn) QueryRow(ctx context.Context, sqlText string, args ...any) Row {
	rows, _ := c.Query(ctx, sqlText, args...)
	return &pgxRow{rows: rows}
}

// Begin starts a transaction on this connection.
func (c *Conn) Begin(ctx context.Context) (Tx, error) {
	return c.BeginTx(ctx, TxOptions{})
}

// BeginTx starts a transaction with TxOptions on this connection.
func (c *Conn) BeginTx(ctx context.Context, txOptions TxOptions) (Tx, error) {
	return c.beginTxInternal(ctx, txOptions, nil)
}

func (txOptions TxOptions) beginSQL() (string, error) {
	var parts []string
	parts = append(parts, "BEGIN")
	switch txOptions.IsoLevel {
	case "":
	case Serializable:
		parts = append(parts, "ISOLATION LEVEL SERIALIZABLE")
	case RepeatableRead:
		parts = append(parts, "ISOLATION LEVEL REPEATABLE READ")
	case ReadCommitted:
		parts = append(parts, "ISOLATION LEVEL READ COMMITTED")
	case ReadUncommitted:
		parts = append(parts, "ISOLATION LEVEL READ UNCOMMITTED")
	default:
		return "", fmt.Errorf("pgx: unknown isolation level %q", txOptions.IsoLevel)
	}
	switch txOptions.AccessMode {
	case "":
	case ReadWrite:
		parts = append(parts, "READ WRITE")
	case ReadOnly:
		parts = append(parts, "READ ONLY")
	default:
		return "", fmt.Errorf("pgx: unknown access mode %q", txOptions.AccessMode)
	}
	switch txOptions.DeferrableMode {
	case "":
	case Deferrable:
		parts = append(parts, "DEFERRABLE")
	case NotDeferrable:
		parts = append(parts, "NOT DEFERRABLE")
	default:
		return "", fmt.Errorf("pgx: unknown deferrable mode %q", txOptions.DeferrableMode)
	}
	return strings.Join(parts, " "), nil
}

func (c *Conn) checkNoActiveTx(ctx context.Context, allowHistoricalReadOnly bool) error {
	probeTx, err := c.sqlConn.BeginTx(ctx, nil)
	if err != nil {
		if allowHistoricalReadOnly && sqlState(err) == "0A000" {
			return nil
		}
		return wrapPgError(err)
	}
	if rbErr := probeTx.Rollback(); rbErr != nil {
		return wrapPgError(rbErr)
	}
	return nil
}

func (c *Conn) beginTxInternal(ctx context.Context, txOptions TxOptions, release func()) (Tx, error) {
	if txOptions.BeginQuery != "" {
		if err := c.checkNoActiveTx(ctx, true); err != nil {
			return nil, err
		}
		if _, err := c.sqlConn.ExecContext(ctx, txOptions.BeginQuery); err != nil {
			return nil, wrapPgError(err)
		}
		return &pgxTx{conn: c, rawBegin: true, release: release}, nil
	}
	if txOptions.DeferrableMode != "" {
		beginSQL, err := txOptions.beginSQL()
		if err != nil {
			return nil, err
		}
		if err := c.checkNoActiveTx(ctx, false); err != nil {
			return nil, err
		}
		if _, err := c.sqlConn.ExecContext(ctx, beginSQL); err != nil {
			return nil, wrapPgError(err)
		}
		return &pgxTx{conn: c, rawBegin: true, release: release}, nil
	}
	sqlTx, err := c.sqlConn.BeginTx(ctx, txOptions.toSQLTxOptions())
	if err != nil {
		return nil, wrapPgError(err)
	}
	return &pgxTx{conn: c, sqlTx: sqlTx, release: release}, nil
}

// BeginFunc starts a transaction on this connection and runs fn.
func (c *Conn) BeginFunc(ctx context.Context, fn func(Tx) error) error {
	return BeginFunc(ctx, c, fn)
}

// SendBatch sends a batch of queries on this connection inside an implicit transaction.
func (c *Conn) SendBatch(ctx context.Context, b *Batch) BatchResults {
	return c.sendBatchWithRelease(ctx, b, nil)
}

func (c *Conn) sendBatchWithRelease(ctx context.Context, b *Batch, release func()) BatchResults {
	tx, err := c.beginTxInternal(ctx, TxOptions{}, nil)
	if err != nil {
		// If the connection is already inside a manual BEGIN block (SQLSTATE 25001),
		// run the batch inside the active transaction without starting a new one.
		if sqlState(err) == "25001" {
			return &batchResults{
				ctx:     ctx,
				execer:  c,
				conn:    c,
				batch:   b,
				release: release,
			}
		}
		if release != nil {
			release()
		}
		return &batchResults{err: wrapPgError(err), closed: true}
	}
	return &batchResults{
		ctx:        ctx,
		execer:     tx,
		implicitTx: tx,
		conn:       c,
		batch:      b,
		release:    release,
	}
}

// CopyFrom performs an atomic bulk insert of rows into tableName.
func (c *Conn) CopyFrom(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	tx, err := c.beginTxInternal(ctx, TxOptions{}, nil)
	if err != nil {
		if sqlState(err) == "25001" {
			return c.copyFromInSavepoint(ctx, tableName, columnNames, rowSrc)
		}
		return 0, wrapPgError(err)
	}
	return c.copyFromWithTx(ctx, tx, tableName, columnNames, rowSrc)
}

func (c *Conn) copyFromWithTx(ctx context.Context, tx Tx, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (total int64, err error) {
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			total = 0
		} else if commitErr := tx.Commit(ctx); commitErr != nil {
			err = wrapPgError(commitErr)
			total = 0
		}
	}()
	total, err = copyFromExecer(ctx, tx, tableName, columnNames, rowSrc)
	return total, wrapPgError(err)
}

func (c *Conn) copyFromInSavepoint(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (total int64, err error) {
	spID := c.savepoint.Add(1)
	spName := fmt.Sprintf("pgx_sp_%d", spID)
	if _, err := c.Exec(ctx, "SAVEPOINT "+spName); err != nil {
		return 0, wrapPgError(err)
	}
	defer func() {
		if p := recover(); p != nil {
			rollbackCtx := context.Background()
			if ctx != nil {
				rollbackCtx = context.WithoutCancel(ctx)
			}
			_, _ = c.Exec(rollbackCtx, "ROLLBACK TO SAVEPOINT "+spName)
			_, _ = c.Exec(rollbackCtx, "RELEASE SAVEPOINT "+spName)
			panic(p)
		}
		if err != nil {
			rollbackCtx := context.Background()
			if ctx != nil {
				rollbackCtx = context.WithoutCancel(ctx)
			}
			_, _ = c.Exec(rollbackCtx, "ROLLBACK TO SAVEPOINT "+spName)
			_, _ = c.Exec(rollbackCtx, "RELEASE SAVEPOINT "+spName)
			total = 0
		} else if _, relErr := c.Exec(ctx, "RELEASE SAVEPOINT "+spName); relErr != nil {
			err = wrapPgError(relErr)
			total = 0
		}
	}()
	total, err = copyFromExecer(ctx, c, tableName, columnNames, rowSrc)
	return total, wrapPgError(err)
}

// Tx represents a database transaction or nested savepoint transaction.
type Tx interface {
	Begin(ctx context.Context) (Tx, error)
	BeginFunc(ctx context.Context, fn func(Tx) error) error
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	CopyFrom(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error)
	SendBatch(ctx context.Context, b *Batch) BatchResults
	Prepare(ctx context.Context, name, sql string) error
	Exec(ctx context.Context, sql string, arguments ...any) (commandTag CommandTag, err error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Conn() *Conn
}

type queryExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

type pgxTx struct {
	conn      *Conn
	sqlTx     *sql.Tx
	rawBegin  bool
	savepoint string
	release   func()
	closed    atomic.Bool
}

func (tx *pgxTx) Conn() *Conn { return tx.conn }

func (tx *pgxTx) Begin(ctx context.Context) (Tx, error) {
	if tx.closed.Load() {
		return nil, ErrTxClosed
	}
	spID := tx.conn.savepoint.Add(1)
	spName := fmt.Sprintf("pgx_sp_%d", spID)
	if _, err := tx.Exec(ctx, "SAVEPOINT "+spName); err != nil {
		return nil, err
	}
	return &pgxTx{
		conn:      tx.conn,
		sqlTx:     tx.sqlTx,
		rawBegin:  tx.rawBegin,
		savepoint: spName,
	}, nil
}

func (tx *pgxTx) BeginFunc(ctx context.Context, fn func(Tx) error) error {
	return BeginFunc(ctx, tx, fn)
}

func (tx *pgxTx) Commit(ctx context.Context) error {
	if !tx.closed.CompareAndSwap(false, true) {
		return ErrTxClosed
	}
	if tx.release != nil {
		defer tx.release()
	}
	if tx.savepoint != "" {
		_, err := tx.execUnderlying(ctx, "RELEASE SAVEPOINT "+tx.savepoint)
		return wrapPgError(err)
	}
	if tx.rawBegin {
		_, err := tx.conn.Exec(ctx, "COMMIT")
		if err != nil && !errors.Is(err, storage.ErrCommitIndeterminate) {
			rollbackCtx := context.Background()
			if ctx != nil {
				rollbackCtx = context.WithoutCancel(ctx)
			}
			_, _ = tx.conn.Exec(rollbackCtx, "ROLLBACK")
		}
		return wrapPgError(err)
	}
	return wrapPgError(tx.sqlTx.Commit())
}

func (tx *pgxTx) Rollback(ctx context.Context) error {
	if !tx.closed.CompareAndSwap(false, true) {
		return ErrTxClosed
	}
	if tx.release != nil {
		defer tx.release()
	}
	rollbackCtx := context.Background()
	if ctx != nil {
		rollbackCtx = context.WithoutCancel(ctx)
	}
	if tx.savepoint != "" {
		if _, err := tx.execUnderlying(rollbackCtx, "ROLLBACK TO SAVEPOINT "+tx.savepoint); err != nil {
			return wrapPgError(err)
		}
		_, err := tx.execUnderlying(rollbackCtx, "RELEASE SAVEPOINT "+tx.savepoint)
		return wrapPgError(err)
	}
	if tx.rawBegin {
		_, err := tx.conn.Exec(rollbackCtx, "ROLLBACK")
		return wrapPgError(err)
	}
	return wrapPgError(tx.sqlTx.Rollback())
}

func (tx *pgxTx) Prepare(ctx context.Context, name, sqlText string) error {
	if tx.closed.Load() {
		return ErrTxClosed
	}
	return tx.conn.Prepare(ctx, name, sqlText)
}

func (tx *pgxTx) execUnderlying(ctx context.Context, sqlText string, arguments ...any) (CommandTag, error) {
	sqlText = tx.conn.resolvePrepared(sqlText)
	rewritten, args, err := prepareQueryAndArgs(ctx, tx.conn, sqlText, arguments)
	if err != nil {
		return CommandTag{}, wrapPgError(err)
	}
	if tx.sqlTx != nil {
		res, err := tx.sqlTx.ExecContext(ctx, rewritten, args...)
		if err != nil {
			return CommandTag{}, wrapPgError(err)
		}
		return newCommandTagFromResult(res, parseStatementVerb(rewritten)), nil
	}
	return tx.conn.Exec(ctx, rewritten, args...)
}

func (tx *pgxTx) Exec(ctx context.Context, sqlText string, arguments ...any) (CommandTag, error) {
	if tx.closed.Load() {
		return CommandTag{}, ErrTxClosed
	}
	return tx.execUnderlying(ctx, sqlText, arguments...)
}

func (tx *pgxTx) Query(ctx context.Context, sqlText string, arguments ...any) (Rows, error) {
	if tx.closed.Load() {
		return newClosedErrorRows(tx.conn, ErrTxClosed), ErrTxClosed
	}
	sqlText = tx.conn.resolvePrepared(sqlText)
	rewritten, args, err := prepareQueryAndArgs(ctx, tx.conn, sqlText, arguments)
	if err != nil {
		err = wrapPgError(err)
		return newClosedErrorRows(tx.conn, err), err
	}
	if tx.sqlTx != nil {
		sqlRows, err := tx.sqlTx.QueryContext(ctx, rewritten, args...)
		if err != nil {
			err = wrapPgError(err)
			return newClosedErrorRows(tx.conn, err), err
		}
		return newPgxRows(tx.conn, sqlRows, nil, parseStatementVerb(rewritten)), nil
	}
	return tx.conn.Query(ctx, rewritten, args...)
}

func (tx *pgxTx) QueryRow(ctx context.Context, sqlText string, args ...any) Row {
	rows, _ := tx.Query(ctx, sqlText, args...)
	return &pgxRow{rows: rows}
}

func (tx *pgxTx) SendBatch(ctx context.Context, b *Batch) BatchResults {
	return &batchResults{
		ctx:    ctx,
		execer: tx,
		conn:   tx.conn,
		batch:  b,
	}
}

func (tx *pgxTx) CopyFrom(ctx context.Context, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	var total int64
	err := tx.BeginFunc(ctx, func(subTx Tx) error {
		n, err := copyFromExecer(ctx, subTx, tableName, columnNames, rowSrc)
		total = n
		return err
	})
	return total, wrapPgError(err)
}

// BeginFunc begins a transaction on db and executes fn.
func BeginFunc(ctx context.Context, db interface {
	Begin(ctx context.Context) (Tx, error)
}, fn func(Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
	}()
	return fn(tx)
}

// BeginTxFunc begins a transaction with txOptions on db and executes fn.
func BeginTxFunc(ctx context.Context, db interface {
	BeginTx(ctx context.Context, txOptions TxOptions) (Tx, error)
}, txOptions TxOptions, fn func(Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, txOptions)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
	}()
	return fn(tx)
}

// pgxRows implements Rows over *sql.Rows.
type pgxRows struct {
	conn      *Conn
	sqlRows   *sql.Rows
	release   func()
	verb      string
	fields    []FieldDescription
	rowCount  int64
	err       error
	closed    atomic.Bool
	lastVals  []any
	hasValues bool
}

func newClosedErrorRows(conn *Conn, err error) *pgxRows {
	r := &pgxRows{conn: conn, err: wrapPgError(err)}
	r.closed.Store(true)
	return r
}

func pgTypeNameToOID(typeName string) uint32 {
	name := strings.ToUpper(strings.TrimSpace(typeName))
	if strings.HasPrefix(name, "OID(") && strings.HasSuffix(name, ")") {
		if parsed, err := strconv.ParseUint(name[4:len(name)-1], 10, 32); err == nil {
			return uint32(parsed)
		}
	}
	switch name {
	case "BOOL", "BOOLEAN":
		return 16
	case "BYTEA":
		return 17
	case "CHAR":
		return 18
	case "NAME":
		return 19
	case "INT8", "BIGINT":
		return 20
	case "INT2", "SMALLINT":
		return 21
	case "INT4", "INTEGER", "INT":
		return 23
	case "TEXT":
		return 25
	case "OID":
		return 26
	case "JSON":
		return 114
	case "FLOAT4", "REAL":
		return 700
	case "FLOAT8", "DOUBLE PRECISION":
		return 701
	case "UNKNOWN":
		return 705
	case "_BOOL":
		return 1000
	case "_BYTEA":
		return 1001
	case "_CHAR":
		return 1002
	case "_NAME":
		return 1003
	case "_INT2":
		return 1005
	case "_INT4":
		return 1007
	case "_TEXT":
		return 1009
	case "_BPCHAR":
		return 1014
	case "_VARCHAR":
		return 1015
	case "_INT8":
		return 1016
	case "_FLOAT4":
		return 1021
	case "_FLOAT8":
		return 1022
	case "_OID":
		return 1028
	case "BPCHAR", "CHARACTER":
		return 1042
	case "VARCHAR", "CHARACTER VARYING":
		return 1043
	case "DATE":
		return 1082
	case "TIME", "TIME WITHOUT TIME ZONE":
		return 1083
	case "TIMESTAMP", "TIMESTAMP WITHOUT TIME ZONE":
		return 1114
	case "TIMESTAMPTZ", "TIMESTAMP WITH TIME ZONE":
		return 1184
	case "INTERVAL":
		return 1186
	case "_NUMERIC":
		return 1231
	case "TIMETZ", "TIME WITH TIME ZONE":
		return 1266
	case "BIT":
		return 1560
	case "VARBIT", "BIT VARYING":
		return 1562
	case "NUMERIC", "DECIMAL":
		return 1700
	case "UUID":
		return 2950
	case "_UUID":
		return 2951
	case "JSONB":
		return 3802
	case "_JSONB":
		return 3807
	default:
		return 25
	}
}

func newPgxRows(conn *Conn, sqlRows *sql.Rows, release func(), verb string) *pgxRows {
	var fields []FieldDescription
	if colTypes, err := sqlRows.ColumnTypes(); err == nil && len(colTypes) > 0 {
		fields = make([]FieldDescription, len(colTypes))
		for i, ct := range colTypes {
			fields[i] = FieldDescription{
				Name:         ct.Name(),
				DataTypeOID:  pgTypeNameToOID(ct.DatabaseTypeName()),
				TypeModifier: -1,
			}
		}
	} else {
		cols, _ := sqlRows.Columns()
		fields = make([]FieldDescription, len(cols))
		for i, name := range cols {
			fields[i] = FieldDescription{Name: name, DataTypeOID: 25, TypeModifier: -1}
		}
	}
	return &pgxRows{
		conn:    conn,
		sqlRows: sqlRows,
		release: release,
		verb:    verb,
		fields:  fields,
	}
}

func (r *pgxRows) Conn() *Conn { return r.conn }

func (r *pgxRows) Close() {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}
	if r.sqlRows != nil {
		if err := r.sqlRows.Close(); err != nil && r.err == nil {
			r.err = wrapPgError(err)
		}
		if err := r.sqlRows.Err(); err != nil && r.err == nil {
			r.err = wrapPgError(err)
		}
	}
	if r.release != nil {
		r.release()
		r.release = nil
	}
}

func (r *pgxRows) Err() error {
	if r.err != nil {
		return r.err
	}
	if r.sqlRows != nil {
		return wrapPgError(r.sqlRows.Err())
	}
	return nil
}

func (r *pgxRows) CommandTag() CommandTag {
	verb := r.verb
	if verb == "" {
		verb = "SELECT"
	}
	return formatVerbCommandTag(verb, r.rowCount)
}

func (r *pgxRows) FieldDescriptions() []FieldDescription {
	return r.fields
}

func (r *pgxRows) Next() bool {
	if r.closed.Load() || r.sqlRows == nil {
		return false
	}
	r.hasValues = false
	if r.sqlRows.Next() {
		r.rowCount++
		return true
	}
	r.Close()
	return false
}

func (r *pgxRows) Values() ([]any, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.hasValues {
		out := make([]any, len(r.lastVals))
		copy(out, r.lastVals)
		return out, nil
	}
	raw := make([]any, len(r.fields))
	ptrs := make([]any, len(r.fields))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := r.sqlRows.Scan(ptrs...); err != nil {
		r.err = wrapPgError(err)
		return nil, r.err
	}
	r.lastVals = raw
	r.hasValues = true
	out := make([]any, len(raw))
	copy(out, raw)
	return out, nil
}

func (r *pgxRows) RawValues() [][]byte {
	vals, err := r.Values()
	if err != nil {
		return nil
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if v == nil {
			continue
		}
		switch tv := v.(type) {
		case []byte:
			out[i] = tv
		case string:
			out[i] = []byte(tv)
		default:
			out[i] = []byte(fmt.Sprint(tv))
		}
	}
	return out
}

func (r *pgxRows) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.sqlRows == nil {
		return ErrNoRows
	}
	needsCustom := r.hasValues
	if !needsCustom {
		for _, d := range dest {
			if isCustomScanDest(d) {
				needsCustom = true
				break
			}
		}
	}
	if !needsCustom {
		if err := r.sqlRows.Scan(dest...); err != nil {
			r.err = wrapPgError(err)
			return r.err
		}
		return nil
	}
	vals, err := r.Values()
	if err != nil {
		return err
	}
	if len(dest) != len(vals) {
		return fmt.Errorf("pgx: Scan received %d destinations, but row has %d columns", len(dest), len(vals))
	}
	for i, d := range dest {
		if err := assignScanValue(vals[i], d); err != nil {
			return fmt.Errorf("pgx: cannot scan column %d (%s): %w", i, r.fields[i].Name, err)
		}
	}
	return nil
}

type pgxRow struct {
	rows Rows
}

func (r *pgxRow) Scan(dest ...any) error {
	if r.rows == nil {
		return ErrNoRows
	}
	defer r.rows.Close()
	if err := r.rows.Err(); err != nil {
		r.rows.Close()
		return wrapPgError(err)
	}
	if !r.rows.Next() {
		r.rows.Close()
		if err := r.rows.Err(); err != nil {
			return wrapPgError(err)
		}
		return ErrNoRows
	}
	scanErr := r.rows.Scan(dest...)
	r.rows.Close()
	if scanErr != nil {
		return wrapPgError(scanErr)
	}
	return wrapPgError(r.rows.Err())
}

var byteSliceType = reflect.TypeFor[[]byte]()

func isCustomScanDest(d any) bool {
	if d == nil {
		return true
	}
	if _, ok := d.(sql.Scanner); ok {
		return false
	}
	switch d.(type) {
	case *[16]byte, *json.RawMessage, *map[string]any, *[]any,
		*uint, *uint8, *uint16, *uint32, *uint64, *int8, *int16, *int32:
		return true
	}
	rv := reflect.ValueOf(d)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return false
	}
	elem := rv.Elem()
	switch elem.Kind() {
	case reflect.Slice:
		return elem.Type() != byteSliceType
	case reflect.Struct:
		_, isTime := d.(*time.Time)
		return !isTime
	case reflect.Pointer:
		inner := elem.Type().Elem()
		return inner.Kind() == reflect.Slice && inner != byteSliceType
	}
	return false
}

func assignScanValue(src any, dest any) error {
	if dest == nil {
		return nil
	}
	if scanner, ok := dest.(sql.Scanner); ok {
		return scanner.Scan(src)
	}
	switch d := dest.(type) {
	case *any:
		*d = src
		return nil
	case *string:
		if src == nil {
			return errors.New("cannot scan NULL into *string")
		}
		switch s := src.(type) {
		case string:
			*d = s
		case []byte:
			*d = string(s)
		default:
			*d = fmt.Sprint(s)
		}
		return nil
	case *int64:
		v, err := toInt64(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	case *int:
		v, err := toInt64(src)
		if err != nil {
			return err
		}
		if strconv.IntSize == 32 && (v < math.MinInt32 || v > math.MaxInt32) {
			return fmt.Errorf("value %d overflows int", v)
		}
		*d = int(v)
		return nil
	case *int32:
		v, err := toInt64(src)
		if err != nil {
			return err
		}
		if v < math.MinInt32 || v > math.MaxInt32 {
			return fmt.Errorf("value %d overflows int32", v)
		}
		*d = int32(v)
		return nil
	case *int16:
		v, err := toInt64(src)
		if err != nil {
			return err
		}
		if v < math.MinInt16 || v > math.MaxInt16 {
			return fmt.Errorf("value %d overflows int16", v)
		}
		*d = int16(v)
		return nil
	case *int8:
		v, err := toInt64(src)
		if err != nil {
			return err
		}
		if v < math.MinInt8 || v > math.MaxInt8 {
			return fmt.Errorf("value %d overflows int8", v)
		}
		*d = int8(v)
		return nil
	case *uint64:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	case *uint:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		if v > uint64(^uint(0)) {
			return fmt.Errorf("value %d overflows uint", v)
		}
		*d = uint(v)
		return nil
	case *uint32:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		if v > math.MaxUint32 {
			return fmt.Errorf("value %d overflows uint32", v)
		}
		*d = uint32(v)
		return nil
	case *uint16:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		if v > math.MaxUint16 {
			return fmt.Errorf("value %d overflows uint16", v)
		}
		*d = uint16(v)
		return nil
	case *uint8:
		v, err := toUint64(src)
		if err != nil {
			return err
		}
		if v > math.MaxUint8 {
			return fmt.Errorf("value %d overflows uint8", v)
		}
		*d = uint8(v)
		return nil
	case *float64:
		v, err := toFloat64(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	case *float32:
		v, err := toFloat64(src)
		if err != nil {
			return err
		}
		*d = float32(v)
		return nil
	case *bool:
		v, err := toBool(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	case *[]byte:
		if src == nil {
			*d = nil
			return nil
		}
		switch b := src.(type) {
		case []byte:
			cp := make([]byte, len(b))
			copy(cp, b)
			*d = cp
		case string:
			*d = []byte(b)
		default:
			return fmt.Errorf("cannot scan %T into *[]byte", src)
		}
		return nil
	case *time.Time:
		if src == nil {
			return errors.New("cannot scan NULL into *time.Time")
		}
		parsed, err := parseTimeValue(src)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	case *[16]byte:
		if src == nil {
			return errors.New("cannot scan NULL into *[16]byte")
		}
		return parseUUIDInto(src, d)
	case *json.RawMessage:
		if src == nil {
			*d = nil
			return nil
		}
		switch s := src.(type) {
		case []byte:
			cp := make([]byte, len(s))
			copy(cp, s)
			*d = json.RawMessage(cp)
		case string:
			*d = json.RawMessage(s)
		default:
			*d = json.RawMessage(fmt.Sprint(src))
		}
		return nil
	case *map[string]any:
		if src == nil {
			*d = nil
			return nil
		}
		return json.Unmarshal([]byte(fmt.Sprint(src)), d)
	case *[]any:
		if src == nil {
			*d = nil
			return nil
		}
		raw := strings.TrimSpace(fmt.Sprint(src))
		if strings.HasPrefix(raw, "{") {
			node, err := parsePGArrayTree(raw)
			if err != nil {
				return err
			}
			*d = pgArrayNodeToAnySlice(node)
			return nil
		}
		return json.Unmarshal([]byte(raw), d)
	}

	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", dest)
	}
	elem := rv.Elem()
	if src == nil {
		if elem.Kind() == reflect.Pointer || elem.Kind() == reflect.Slice || elem.Kind() == reflect.Map || elem.Kind() == reflect.Interface {
			elem.Set(reflect.Zero(elem.Type()))
			return nil
		}
		return fmt.Errorf("cannot scan NULL into %T", dest)
	}
	if elem.Kind() == reflect.Pointer {
		ptr := reflect.New(elem.Type().Elem())
		if err := assignScanValue(src, ptr.Interface()); err != nil {
			return err
		}
		elem.Set(ptr)
		return nil
	}
	if elem.Kind() == reflect.Slice && elem.Type() != byteSliceType {
		var literal string
		switch s := src.(type) {
		case string:
			literal = s
		case []byte:
			literal = string(s)
		default:
			literal = fmt.Sprint(s)
		}
		root, err := parsePGArrayTree(literal)
		if err != nil {
			return err
		}
		return assignPGArrayNode(root, elem)
	}
	if elem.Kind() == reflect.Struct {
		return json.Unmarshal([]byte(fmt.Sprint(src)), dest)
	}
	sv := reflect.ValueOf(src)
	if sv.Type().AssignableTo(elem.Type()) {
		elem.Set(sv)
		return nil
	}
	if sv.Type().ConvertibleTo(elem.Type()) {
		elem.Set(sv.Convert(elem.Type()))
		return nil
	}
	return fmt.Errorf("cannot scan %T into %T", src, dest)
}

func toInt64(src any) (int64, error) {
	if src == nil {
		return 0, errors.New("cannot scan NULL into integer")
	}
	switch v := src.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return 0, fmt.Errorf("uint64 value %d overflows int64", v)
		}
		return int64(v), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	case []byte:
		return strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
	default:
		return 0, fmt.Errorf("cannot scan %T as int64", src)
	}
}

func toUint64(src any) (uint64, error) {
	if src == nil {
		return 0, errors.New("cannot scan NULL into unsigned integer")
	}
	switch v := src.(type) {
	case uint64:
		return v, nil
	case uint:
		return uint64(v), nil
	case uint32:
		return uint64(v), nil
	case uint16:
		return uint64(v), nil
	case uint8:
		return uint64(v), nil
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("cannot scan negative value %d into unsigned integer", v)
		}
		return uint64(v), nil
	case int:
		if v < 0 {
			return 0, fmt.Errorf("cannot scan negative value %d into unsigned integer", v)
		}
		return uint64(v), nil
	case int32:
		if v < 0 {
			return 0, fmt.Errorf("cannot scan negative value %d into unsigned integer", v)
		}
		return uint64(v), nil
	case int16:
		if v < 0 {
			return 0, fmt.Errorf("cannot scan negative value %d into unsigned integer", v)
		}
		return uint64(v), nil
	case int8:
		if v < 0 {
			return 0, fmt.Errorf("cannot scan negative value %d into unsigned integer", v)
		}
		return uint64(v), nil
	case string:
		s := strings.TrimSpace(v)
		if strings.HasPrefix(s, "-") {
			return 0, fmt.Errorf("cannot scan negative value %q into unsigned integer", s)
		}
		return strconv.ParseUint(s, 10, 64)
	case []byte:
		s := strings.TrimSpace(string(v))
		if strings.HasPrefix(s, "-") {
			return 0, fmt.Errorf("cannot scan negative value %q into unsigned integer", s)
		}
		return strconv.ParseUint(s, 10, 64)
	default:
		return 0, fmt.Errorf("cannot scan %T as uint64", src)
	}
}

func toFloat64(src any) (float64, error) {
	if src == nil {
		return 0, errors.New("cannot scan NULL into float")
	}
	switch v := src.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case int:
		return float64(v), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	case []byte:
		return strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
	default:
		return 0, fmt.Errorf("cannot scan %T as float64", src)
	}
}

func toBool(src any) (bool, error) {
	if src == nil {
		return false, errors.New("cannot scan NULL into bool")
	}
	switch v := src.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "t", "true", "1", "yes", "on":
			return true, nil
		case "f", "false", "0", "no", "off":
			return false, nil
		default:
			return false, fmt.Errorf("cannot parse %q as bool", v)
		}
	case []byte:
		return toBool(string(v))
	default:
		return false, fmt.Errorf("cannot scan %T as bool", src)
	}
}

func parseUUIDInto(src any, dest *[16]byte) error {
	switch v := src.(type) {
	case [16]byte:
		*dest = v
		return nil
	case []byte:
		if len(v) == 16 {
			copy(dest[:], v)
			return nil
		}
		return parseUUIDString(string(v), dest)
	case string:
		return parseUUIDString(v, dest)
	default:
		return fmt.Errorf("cannot scan %T into UUID", src)
	}
}

func parseUUIDString(s string, dest *[16]byte) error {
	clean := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(clean) != 32 {
		return fmt.Errorf("invalid UUID string %q", s)
	}
	b, err := hex.DecodeString(clean)
	if err != nil {
		return err
	}
	copy(dest[:], b)
	return nil
}

type pgArrayNode struct {
	isArray  bool
	isNull   bool
	text     string
	elements []pgArrayNode
}

// parsePGArrayTree parses a PostgreSQL array literal (including optional
// dimension prefixes like [1:2][1:2]=, nested braces for multi-dimensional
// arrays, quoted strings with escapes, and unquoted NULL vs quoted "NULL").
func parsePGArrayTree(raw string) (pgArrayNode, error) {
	s := strings.TrimSpace(raw)
	// Strip optional dimension prefix such as [1:2][1:3]={...}
	if strings.HasPrefix(s, "[") {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return pgArrayNode{}, fmt.Errorf("invalid PostgreSQL array literal %q", raw)
		}
		s = strings.TrimSpace(s[eq+1:])
	}
	if len(s) < 2 || s[0] != '{' {
		return pgArrayNode{}, fmt.Errorf("invalid PostgreSQL array literal %q", raw)
	}
	node, next, err := parsePGArraySub(s, 0)
	if err != nil {
		return pgArrayNode{}, err
	}
	if strings.TrimSpace(s[next:]) != "" {
		return pgArrayNode{}, fmt.Errorf("invalid PostgreSQL array literal %q: trailing characters", raw)
	}
	return node, nil
}

func parsePGArraySub(s string, pos int) (pgArrayNode, int, error) {
	n := len(s)
	if pos >= n || s[pos] != '{' {
		return pgArrayNode{}, pos, fmt.Errorf("expected '{' in array literal %q at %d", s, pos)
	}
	pos++
	node := pgArrayNode{isArray: true}
	if pos < n && s[pos] == '}' {
		return node, pos + 1, nil
	}
	for pos < n {
		switch s[pos] {
		case '{':
			child, next, err := parsePGArraySub(s, pos)
			if err != nil {
				return pgArrayNode{}, pos, err
			}
			node.elements = append(node.elements, child)
			pos = next
		case '"':
			pos++
			var elem strings.Builder
			closed := false
			for pos < n {
				if s[pos] == '\\' && pos+1 < n {
					pos++
					elem.WriteByte(s[pos])
					pos++
					continue
				}
				if s[pos] == '"' {
					pos++
					closed = true
					break
				}
				elem.WriteByte(s[pos])
				pos++
			}
			if !closed {
				return pgArrayNode{}, pos, fmt.Errorf("unterminated quoted element in array literal %q", s)
			}
			node.elements = append(node.elements, pgArrayNode{text: elem.String()})
		default:
			start := pos
			for pos < n && s[pos] != ',' && s[pos] != '}' {
				pos++
			}
			token := s[start:pos]
			if strings.EqualFold(token, "NULL") {
				node.elements = append(node.elements, pgArrayNode{isNull: true})
			} else {
				node.elements = append(node.elements, pgArrayNode{text: token})
			}
		}
		if pos >= n {
			return pgArrayNode{}, pos, fmt.Errorf("unterminated array literal %q", s)
		}
		if s[pos] == ',' {
			pos++
			continue
		}
		if s[pos] == '}' {
			return node, pos + 1, nil
		}
		return pgArrayNode{}, pos, fmt.Errorf("unexpected character %q in array literal %q", s[pos], s)
	}
	return pgArrayNode{}, pos, fmt.Errorf("unterminated array literal %q", s)
}

func assignPGArrayNode(node pgArrayNode, sliceVal reflect.Value) error {
	if !node.isArray {
		return fmt.Errorf("cannot scan scalar %q into %s", node.text, sliceVal.Type())
	}
	elemType := sliceVal.Type().Elem()
	out := reflect.MakeSlice(sliceVal.Type(), len(node.elements), len(node.elements))
	for i, child := range node.elements {
		slot := out.Index(i)
		if child.isArray {
			if elemType.Kind() != reflect.Slice || elemType == byteSliceType {
				return fmt.Errorf("cannot scan nested array into %s", elemType)
			}
			if err := assignPGArrayNode(child, slot); err != nil {
				return err
			}
			continue
		}
		var scalar any
		if !child.isNull {
			scalar = child.text
		}
		if err := assignScanValue(scalar, slot.Addr().Interface()); err != nil {
			return fmt.Errorf("array element %d: %w", i, err)
		}
	}
	sliceVal.Set(out)
	return nil
}

func pgArrayNodeToAnySlice(node pgArrayNode) []any {
	out := make([]any, len(node.elements))
	for i, child := range node.elements {
		if child.isArray {
			out[i] = pgArrayNodeToAnySlice(child)
		} else if child.isNull {
			out[i] = nil
		} else {
			out[i] = child.text
		}
	}
	return out
}

func parsePGArrayLiteral(s string) ([]string, error) {
	tree, err := parsePGArrayTree(s)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(tree.elements))
	for i, child := range tree.elements {
		if child.isArray {
			return nil, fmt.Errorf("cannot scan multi-dimensional array into []string")
		}
		if child.isNull {
			return nil, errors.New("cannot scan NULL array element into string")
		}
		out[i] = child.text
	}
	return out, nil
}

// Batch queues multiple queries to be executed sequentially.
type Batch struct {
	items []*QueuedQuery
}

// QueuedQuery represents a single queued item in a Batch.
type QueuedQuery struct {
	SQL       string
	Arguments []any
	fn        func(br BatchResults) error
}

// Queue adds a query to the batch.
func (b *Batch) Queue(query string, arguments ...any) *QueuedQuery {
	q := &QueuedQuery{SQL: query, Arguments: arguments}
	b.items = append(b.items, q)
	return q
}

// Len returns the number of queued queries in the batch.
func (b *Batch) Len() int {
	if b == nil {
		return 0
	}
	return len(b.items)
}

// Query registers a callback to process the rows returned by this queued query when BatchResults.Close() is called.
func (q *QueuedQuery) Query(fn func(rows Rows) error) {
	q.fn = func(br BatchResults) error {
		rows, err := br.Query()
		if err != nil {
			return err
		}
		defer rows.Close()
		if err := fn(rows); err != nil {
			return err
		}
		rows.Close()
		return rows.Err()
	}
}

// QueryRow registers a callback to process the single row returned by this queued query.
func (q *QueuedQuery) QueryRow(fn func(row Row) error) {
	q.fn = func(br BatchResults) error {
		return fn(br.QueryRow())
	}
}

// Exec registers a callback to process the CommandTag returned by this queued query.
func (q *QueuedQuery) Exec(fn func(ct CommandTag) error) {
	q.fn = func(br BatchResults) error {
		ct, err := br.Exec()
		if err != nil {
			return err
		}
		return fn(ct)
	}
}

// BatchResults reads the results of a Batch.
type BatchResults interface {
	Exec() (CommandTag, error)
	Query() (Rows, error)
	QueryRow() Row
	Close() error
}

type batchResults struct {
	ctx        context.Context
	execer     queryExecer
	implicitTx Tx
	conn       *Conn
	batch      *Batch
	release    func()
	activeRows Rows
	idx        int
	err        error
	closed     bool
}

func (br *batchResults) finishActiveRows() {
	if br.activeRows == nil {
		return
	}
	br.activeRows.Close()
	if err := br.activeRows.Err(); err != nil && br.err == nil {
		br.err = wrapPgError(err)
	}
	br.activeRows = nil
}

func (br *batchResults) nextItem() (*QueuedQuery, error) {
	br.finishActiveRows()
	if br.err != nil {
		return nil, br.err
	}
	if br.closed {
		return nil, errors.New("pgx: batch already closed")
	}
	if br.batch == nil || br.idx >= len(br.batch.items) {
		return nil, errors.New("pgx: no more results in batch")
	}
	item := br.batch.items[br.idx]
	br.idx++
	return item, nil
}

func (br *batchResults) Exec() (CommandTag, error) {
	item, err := br.nextItem()
	if err != nil {
		return CommandTag{}, err
	}
	ct, err := br.execer.Exec(br.ctx, item.SQL, item.Arguments...)
	if err != nil {
		err = wrapPgError(err)
		br.err = err
	}
	return ct, err
}

func (br *batchResults) Query() (Rows, error) {
	item, err := br.nextItem()
	if err != nil {
		return newClosedErrorRows(br.conn, err), err
	}
	rows, err := br.execer.Query(br.ctx, item.SQL, item.Arguments...)
	if err != nil {
		err = wrapPgError(err)
		br.err = err
		return rows, err
	}
	br.activeRows = rows
	return rows, nil
}

func (br *batchResults) QueryRow() Row {
	rows, _ := br.Query()
	return &pgxRow{rows: rows}
}

func (br *batchResults) Close() error {
	if br.closed {
		return br.err
	}
	defer func() {
		br.closed = true
		if br.release != nil {
			br.release()
			br.release = nil
		}
	}()
	br.finishActiveRows()
	for br.batch != nil && br.idx < len(br.batch.items) && br.err == nil {
		item := br.batch.items[br.idx]
		if item.fn != nil {
			if err := item.fn(br); err != nil {
				br.err = wrapPgError(err)
				break
			}
			br.finishActiveRows()
		} else {
			if _, err := br.Exec(); err != nil {
				br.err = wrapPgError(err)
				break
			}
		}
	}
	br.finishActiveRows()
	if br.implicitTx != nil {
		tx := br.implicitTx
		br.implicitTx = nil
		if br.err != nil {
			rollbackCtx := context.Background()
			if br.ctx != nil {
				rollbackCtx = context.WithoutCancel(br.ctx)
			}
			_ = tx.Rollback(rollbackCtx)
		} else if err := tx.Commit(br.ctx); err != nil {
			br.err = wrapPgError(err)
		}
	}
	return br.err
}

// CopyFromSource is the data source for CopyFrom.
type CopyFromSource interface {
	Next() bool
	Values() ([]any, error)
	Err() error
}

type copyFromRowsSource struct {
	rows [][]any
	idx  int
}

// CopyFromRows returns a CopyFromSource over a slice of row value slices.
func CopyFromRows(rows [][]any) CopyFromSource {
	return &copyFromRowsSource{rows: rows, idx: -1}
}

func (s *copyFromRowsSource) Next() bool {
	s.idx++
	return s.idx < len(s.rows)
}

func (s *copyFromRowsSource) Values() ([]any, error) {
	return s.rows[s.idx], nil
}

func (s *copyFromRowsSource) Err() error { return nil }

type copyFromSliceSource struct {
	length int
	next   func(int) ([]any, error)
	idx    int
	err    error
}

// CopyFromSlice returns a CopyFromSource backed by an indexed generator function.
func CopyFromSlice(length int, next func(int) ([]any, error)) CopyFromSource {
	return &copyFromSliceSource{length: length, next: next, idx: -1}
}

func (s *copyFromSliceSource) Next() bool {
	if s.err != nil {
		return false
	}
	s.idx++
	return s.idx < s.length
}

func (s *copyFromSliceSource) Values() ([]any, error) {
	vals, err := s.next(s.idx)
	if err != nil {
		s.err = err
	}
	return vals, err
}

func (s *copyFromSliceSource) Err() error { return s.err }

func copyFromExecer(ctx context.Context, execer queryExecer, tableName Identifier, columnNames []string, rowSrc CopyFromSource) (int64, error) {
	if len(tableName) == 0 {
		return 0, errors.New("pgx: CopyFrom requires a non-empty tableName")
	}
	if len(columnNames) == 0 {
		return 0, errors.New("pgx: CopyFrom requires at least one column")
	}
	cols := make([]string, len(columnNames))
	for i, c := range columnNames {
		cols[i] = Identifier{c}.Sanitize()
	}
	prefix := fmt.Sprintf("INSERT INTO %s (%s) VALUES ", tableName.Sanitize(), strings.Join(cols, ", "))
	numCols := len(columnNames)
	maxRowsPerBatch := max(1, min(200, 60000/numCols))

	var total int64
	var batchArgs []any
	var rowsInBatch int

	flush := func() error {
		if rowsInBatch == 0 {
			return nil
		}
		var b strings.Builder
		b.WriteString(prefix)
		paramIdx := 1
		for r := 0; r < rowsInBatch; r++ {
			if r > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('(')
			for c := 0; c < numCols; c++ {
				if c > 0 {
					b.WriteString(", ")
				}
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(paramIdx))
				paramIdx++
			}
			b.WriteByte(')')
		}
		ct, err := execer.Exec(ctx, b.String(), batchArgs...)
		if err != nil {
			return err
		}
		total += ct.RowsAffected()
		batchArgs = batchArgs[:0]
		rowsInBatch = 0
		return nil
	}

	for rowSrc.Next() {
		vals, err := rowSrc.Values()
		if err != nil {
			return total, err
		}
		if len(vals) != numCols {
			return total, fmt.Errorf("pgx: CopyFrom expected %d values, got %d", numCols, len(vals))
		}
		batchArgs = append(batchArgs, vals...)
		rowsInBatch++
		if rowsInBatch >= maxRowsPerBatch {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rowSrc.Err(); err != nil {
		return total, err
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, nil
}

// RowToFunc maps a single CollectableRow to a value of type T.
type RowToFunc[T any] func(row CollectableRow) (T, error)

// CollectRows iterates through rows, calling fn for each row and collecting the results into a slice.
func CollectRows[T any](rows Rows, fn RowToFunc[T]) ([]T, error) {
	return AppendRows([]T(nil), rows, fn)
}

// AppendRows iterates through rows, calling fn for each row and appending the results to slice.
func AppendRows[T any, S ~[]T](slice S, rows Rows, fn RowToFunc[T]) (S, error) {
	defer rows.Close()
	for rows.Next() {
		v, err := fn(rows)
		if err != nil {
			return nil, err
		}
		slice = append(slice, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return slice, nil
}

// CollectOneRow calls fn for the first row in rows and closes rows. Returns ErrNoRows if no rows are present.
func CollectOneRow[T any](rows Rows, fn RowToFunc[T]) (T, error) {
	defer rows.Close()
	var zero T
	if !rows.Next() {
		rows.Close()
		if err := rows.Err(); err != nil {
			return zero, err
		}
		return zero, ErrNoRows
	}
	v, err := fn(rows)
	rows.Close()
	if err != nil {
		return zero, err
	}
	if err := rows.Err(); err != nil {
		return zero, err
	}
	return v, nil
}

// CollectExactlyOneRow calls fn for the single row in rows and returns an error if 0 or >1 rows are returned.
func CollectExactlyOneRow[T any](rows Rows, fn RowToFunc[T]) (T, error) {
	defer rows.Close()
	var zero T
	if !rows.Next() {
		rows.Close()
		if err := rows.Err(); err != nil {
			return zero, err
		}
		return zero, ErrNoRows
	}
	v, err := fn(rows)
	if err != nil {
		rows.Close()
		return zero, err
	}
	if rows.Next() {
		rows.Close()
		return zero, errors.New("pgx: too many rows")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return zero, err
	}
	return v, nil
}

// ForEachRow iterates over rows, scanning each row into scans and calling fn.
func ForEachRow(rows Rows, scans []any, fn func() error) (CommandTag, error) {
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(scans...); err != nil {
			return CommandTag{}, err
		}
		if err := fn(); err != nil {
			return CommandTag{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return CommandTag{}, err
	}
	return rows.CommandTag(), nil
}

// RowTo scans a single column row into a value of type T.
func RowTo[T any](row CollectableRow) (T, error) {
	var v T
	err := row.Scan(&v)
	return v, err
}

// RowToAddrOf scans a single column row into the address of a new value of type T.
func RowToAddrOf[T any](row CollectableRow) (*T, error) {
	var v T
	err := row.Scan(&v)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// RowToMap scans a row into a map[string]any keyed by column name.
func RowToMap(row CollectableRow) (map[string]any, error) {
	vals, err := row.Values()
	if err != nil {
		return nil, err
	}
	fields := row.FieldDescriptions()
	m := make(map[string]any, len(fields))
	for i, f := range fields {
		m[f.Name] = vals[i]
	}
	return m, nil
}

// RowToStructByPos scans a row into a struct of type T by field position.
func RowToStructByPos[T any](row CollectableRow) (T, error) {
	var v T
	rv := reflect.ValueOf(&v).Elem()
	if rv.Kind() != reflect.Struct {
		return v, fmt.Errorf("pgx: RowToStructByPos requires a struct type, got %T", v)
	}
	fields := exportedStructFieldPtrs(rv)
	if len(fields) != len(row.FieldDescriptions()) {
		return v, fmt.Errorf("pgx: struct %T has %d exported fields, but query returned %d columns", v, len(fields), len(row.FieldDescriptions()))
	}
	err := row.Scan(fields...)
	return v, err
}

// RowToAddrOfStructByPos returns a pointer to a struct of type T scanned by field position.
func RowToAddrOfStructByPos[T any](row CollectableRow) (*T, error) {
	v, err := RowToStructByPos[T](row)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// RowToStructByName scans a row into a struct of type T by matching column names to `db` tags or field names.
func RowToStructByName[T any](row CollectableRow) (T, error) {
	return rowToStructByName[T](row, false)
}

// RowToStructByNameLax scans a row into a struct of type T by name, ignoring extra columns or unmapped fields.
func RowToStructByNameLax[T any](row CollectableRow) (T, error) {
	return rowToStructByName[T](row, true)
}

// RowToAddrOfStructByName returns a pointer to a struct of type T scanned by column name.
func RowToAddrOfStructByName[T any](row CollectableRow) (*T, error) {
	v, err := RowToStructByName[T](row)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// RowToAddrOfStructByNameLax returns a pointer to a struct of type T scanned by column name (lax).
func RowToAddrOfStructByNameLax[T any](row CollectableRow) (*T, error) {
	v, err := RowToStructByNameLax[T](row)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func rowToStructByName[T any](row CollectableRow, lax bool) (T, error) {
	var v T
	rv := reflect.ValueOf(&v).Elem()
	if rv.Kind() != reflect.Struct {
		return v, fmt.Errorf("pgx: RowToStructByName requires a struct type, got %T", v)
	}
	fieldMap := buildStructFieldNameMap(rv)
	fds := row.FieldDescriptions()
	dests := make([]any, len(fds))
	for i, fd := range fds {
		key := normalizeFieldKey(fd.Name)
		if ptr, ok := fieldMap[key]; ok {
			dests[i] = ptr
			delete(fieldMap, key)
		} else if lax {
			var discard any
			dests[i] = &discard
		} else {
			return v, fmt.Errorf("pgx: cannot find field for column %q in struct %T", fd.Name, v)
		}
	}
	if !lax && len(fieldMap) > 0 {
		for missing := range fieldMap {
			return v, fmt.Errorf("pgx: struct %T field %q not present in query columns", v, missing)
		}
	}
	err := row.Scan(dests...)
	return v, err
}

func exportedStructFieldPtrs(rv reflect.Value) []any {
	var out []any
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		if sf.Tag.Get("db") == "-" {
			continue
		}
		fv := rv.Field(i)
		if sf.Anonymous && fv.Kind() == reflect.Struct {
			out = append(out, exportedStructFieldPtrs(fv)...)
			continue
		}
		out = append(out, fv.Addr().Interface())
	}
	return out
}

func buildStructFieldNameMap(rv reflect.Value) map[string]any {
	m := make(map[string]any)
	populateStructFieldNameMap(rv, m)
	return m
}

func populateStructFieldNameMap(rv reflect.Value, m map[string]any) {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get("db")
		if tag == "-" {
			continue
		}
		fv := rv.Field(i)
		if sf.Anonymous && fv.Kind() == reflect.Struct && tag == "" {
			populateStructFieldNameMap(fv, m)
			continue
		}
		if tag != "" {
			if comma := strings.IndexByte(tag, ','); comma >= 0 {
				tag = tag[:comma]
			}
			if tag != "" {
				m[normalizeFieldKey(tag)] = fv.Addr().Interface()
				continue
			}
		}
		m[normalizeFieldKey(sf.Name)] = fv.Addr().Interface()
	}
}

func normalizeFieldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '_' {
			continue
		}
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		b.WriteByte(ch)
	}
	return b.String()
}
