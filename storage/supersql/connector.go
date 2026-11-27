package supersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	expr "github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (scope *sessionScope) compatIsolationEnabled() bool {
	if scope == nil || scope.settings == nil {
		return true
	}
	if val, err := scope.settings.CurrentSetting("supersql.strict_isolation", true); err == nil && !val.IsNull {
		switch strings.ToLower(strings.TrimSpace(val.StrVal)) {
		case "on", "true", "1", "yes":
			return false
		}
	}
	for _, key := range []string{"supersql.compat_isolation", "app.allow_weaker_isolation"} {
		if val, err := scope.settings.CurrentSetting(key, true); err == nil && !val.IsNull {
			switch strings.ToLower(strings.TrimSpace(val.StrVal)) {
			case "off", "false", "0", "no":
				return false
			case "on", "true", "1", "yes":
				return true
			}
		}
	}
	return true
}

func isWeakerIsolationString(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "read committed", "repeatable read", "read uncommitted":
		return true
	}
	return false
}

func hasWeakerIsolationDefElem(nodes []*zpgparse.Node) bool {
	for _, node := range nodes {
		def := node.GetDefElem()
		if def == nil {
			continue
		}
		switch strings.ToLower(def.Defname) {
		case "transaction_isolation", "isolation_level":
			if c := def.Arg.GetAConst(); c != nil && !c.Isnull && c.GetSval() != nil {
				if isWeakerIsolationString(c.GetSval().Sval) {
					return true
				}
			}
		}
	}
	return false
}

func rewriteWeakerIsolationDefElem(nodes []*zpgparse.Node) {
	for _, node := range nodes {
		def := node.GetDefElem()
		if def == nil {
			continue
		}
		switch strings.ToLower(def.Defname) {
		case "transaction_isolation", "isolation_level":
			if c := def.Arg.GetAConst(); c != nil && !c.Isnull && c.GetSval() != nil {
				if isWeakerIsolationString(c.GetSval().Sval) {
					c.GetSval().Sval = "serializable"
				}
			}
		}
	}
}

func (scope *sessionScope) rewriteCompatIsolation(parsed *zpgparse.ExtendedParseResult, index int) *zpgparse.ExtendedParseResult {
	if !scope.compatIsolationEnabled() || parsed == nil || parsed.ParseResult == nil || index < 0 || index >= len(parsed.ParseResult.Stmts) {
		return parsed
	}
	raw := parsed.ParseResult.Stmts[index]
	if raw == nil || raw.Stmt == nil {
		return parsed
	}
	needsRewrite := false
	if txStmt := raw.Stmt.GetTransactionStmt(); txStmt != nil {
		needsRewrite = hasWeakerIsolationDefElem(txStmt.Options)
	} else if vsStmt := raw.Stmt.GetVariableSetStmt(); vsStmt != nil {
		if hasWeakerIsolationDefElem(vsStmt.Args) {
			needsRewrite = true
		} else if strings.EqualFold(vsStmt.Name, "transaction_isolation") || strings.EqualFold(vsStmt.Name, "default_transaction_isolation") {
			for _, arg := range vsStmt.Args {
				if c := arg.GetAConst(); c != nil && !c.Isnull && c.GetSval() != nil && isWeakerIsolationString(c.GetSval().Sval) {
					needsRewrite = true
					break
				}
			}
		}
	}
	if !needsRewrite {
		return parsed
	}
	clonedRaw := proto.Clone(raw).(*zpgparse.RawStmt)
	if txStmt := clonedRaw.Stmt.GetTransactionStmt(); txStmt != nil {
		rewriteWeakerIsolationDefElem(txStmt.Options)
	} else if vsStmt := clonedRaw.Stmt.GetVariableSetStmt(); vsStmt != nil {
		rewriteWeakerIsolationDefElem(vsStmt.Args)
		if strings.EqualFold(vsStmt.Name, "transaction_isolation") || strings.EqualFold(vsStmt.Name, "default_transaction_isolation") {
			for _, arg := range vsStmt.Args {
				if c := arg.GetAConst(); c != nil && !c.Isnull && c.GetSval() != nil && isWeakerIsolationString(c.GetSval().Sval) {
					c.GetSval().Sval = "serializable"
				}
			}
		}
	}
	clonedStmts := slices.Clone(parsed.ParseResult.Stmts)
	clonedStmts[index] = clonedRaw
	clonedExtended := *parsed
	clonedExtended.ParseResult = &zpgparse.ParseResult{Version: parsed.ParseResult.Version, Stmts: clonedStmts}
	return &clonedExtended
}

// Driver cannot construct a connection by itself: a certified clock is not
// expressible in a DSN, and no global provider lookup exists. Connections come
// only from OpenConnector, which owns the clock's Catalog.
type Driver struct{}

var _ driver.Driver = Driver{}
var _ driver.DriverContext = Driver{}

// Open always fails with ErrClockRequired: a certified clock is not expressible
// in a DSN. Use OpenConnector.
func (Driver) Open(string) (driver.Conn, error) { return nil, ErrClockRequired }

// OpenConnector always fails with ErrClockRequired. Use OpenConnector.
func (Driver) OpenConnector(string) (driver.Connector, error) { return nil, ErrClockRequired }

// Connector is a driver.Connector that owns one opened Catalog. Every Connect
// creates a connection with its own session, so settings, prepared statements,
// cursors, advisory locks, temporary relations and active transactions never
// cross connections. The host owns close order: sql.DB first, then the
// Connector, then the clock provider. Close releases the database, so no live
// connection may outlive it.
type Connector struct {
	database *Catalog
	closed   atomic.Bool
}

var _ driver.Connector = (*Connector)(nil)

// OpenConnector opens the native database like Open and returns the connector
// that serves driver.Conn sessions over it.
func OpenConnector(ctx context.Context, options OpenOptions) (*Connector, error) {
	if clockUnavailable(options.Clock) {
		return nil, ErrClockRequired
	}
	database, err := Open(ctx, options)
	if err != nil {
		return nil, wrapError(err)
	}
	return &Connector{database: database}, nil
}

// Connect returns a driver.Conn bound to a new isolated sessionScope. It fails
// with ErrDatabaseClosed after Close or once the owning Catalog is closed.
func (connector *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if connector == nil || connector.closed.Load() || connector.database == nil {
		return nil, ErrDatabaseClosed
	}
	if err := connector.database.checkOpen(); err != nil {
		return nil, err
	}
	scope, err := connector.database.newSession(ctx)
	if err != nil {
		return nil, wrapError(err)
	}
	return &connection{database: connector.database, scope: scope, done: make(chan struct{})}, nil
}

// NewConnectorWithCatalog returns a Connector bound to an already opened
// Catalog. Close on the returned Connector closes the underlying Catalog.
func NewConnectorWithCatalog(database *Catalog) *Connector {
	return &Connector{database: database}
}

// Catalog returns the underlying Catalog owned by this connector.
func (connector *Connector) Catalog() *Catalog {
	if connector == nil || connector.closed.Load() || connector.database == nil || connector.database.checkOpen() != nil {
		return nil
	}
	return connector.database
}

// Driver returns the shared Driver value that produced this connector.
func (connector *Connector) Driver() driver.Driver { return Driver{} }

// Close releases the owning Catalog. The host must have closed its sql.DB
// first so no live connection outlives the database.
func (connector *Connector) Close() error {
	if connector == nil || !connector.closed.CompareAndSwap(false, true) {
		return nil
	}
	if connector.database == nil {
		return nil
	}
	return connector.database.Close()
}

// connection is one driver.Conn bound to its own sessionScope. done closes on
// Close so an abandoned streaming Rows cannot park its pump goroutine forever.
type connection struct {
	database *Catalog
	scope    *sessionScope
	closed   atomic.Bool
	done     chan struct{}
}

var (
	_ driver.Conn               = (*connection)(nil)
	_ driver.ConnPrepareContext = (*connection)(nil)
	_ driver.ConnBeginTx        = (*connection)(nil)
	_ driver.ExecerContext      = (*connection)(nil)
	_ driver.QueryerContext     = (*connection)(nil)
	_ driver.Pinger             = (*connection)(nil)
	_ driver.SessionResetter    = (*connection)(nil)
	_ driver.Validator          = (*connection)(nil)
	_ driver.NamedValueChecker  = (*connection)(nil)
)

func (c *connection) check() error {
	if c == nil || c.closed.Load() {
		return driver.ErrBadConn
	}
	if c.database == nil {
		return ErrDatabaseClosed
	}
	if err := c.database.checkOpen(); err != nil {
		return err
	}
	return nil
}

// execSQL parses with this session's lexical settings and runs each statement
// through the session pipeline, returning the last statement's outcome.
func (c *connection) execSQL(ctx context.Context, text string, args []driver.NamedValue) (outcome, error) {
	if err := c.check(); err != nil {
		return outcome{}, err
	}
	parsed, err := c.scope.txns.Parse(ctx, text, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return outcome{}, wrapError(err)
	}
	return c.execParsed(ctx, parsed, args)
}

func (c *connection) execParsed(ctx context.Context, parsed *zpgparse.ExtendedParseResult, args []driver.NamedValue) (outcome, error) {
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return outcome{}, ErrMissingStatement
	}
	if len(args) != 0 && len(parsed.ParseResult.Stmts) != 1 {
		return outcome{}, wrapError(&model.Error{Code: "42601", Message: "cannot insert multiple commands into a parameterized statement"})
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return outcome{}, wrapError(err)
	}
	var out outcome
	for index := range parsed.ParseResult.Stmts {
		stmtParsed := c.scope.rewriteCompatIsolation(parsed, index)
		if out, err = c.database.execStatement(ctx, c.scope, stmtParsed, index, parameters, 0, nil, nil); err != nil {
			return out, wrapError(err)
		}
	}
	return out, nil
}

func (c *connection) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *connection) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	parsed, err := c.scope.txns.Parse(ctx, query, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return nil, wrapError(err)
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return nil, wrapError(&model.Error{Code: "42601", Message: "a prepared statement must contain exactly one statement"})
	}
	return &statement{conn: c, parsed: parsed}, nil
}

func (c *connection) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.done)
	return wrapError(c.scope.close())
}

func (c *connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *connection) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if c.scope != nil && c.scope.txns != nil && c.scope.txns.Active() {
		return nil, wrapError(&model.Error{Code: "25001", Message: "there is already a transaction in progress"})
	}
	var begin string
	switch sql.IsolationLevel(options.Isolation) {
	case sql.LevelDefault:
		begin = "BEGIN"
	case sql.LevelSerializable:
		begin = "BEGIN ISOLATION LEVEL SERIALIZABLE"
	default:
		if c.scope.compatIsolationEnabled() {
			switch sql.IsolationLevel(options.Isolation) {
			case sql.LevelReadCommitted, sql.LevelRepeatableRead, sql.LevelReadUncommitted,
				sql.LevelSnapshot, sql.LevelWriteCommitted, sql.LevelLinearizable:
				begin = "BEGIN ISOLATION LEVEL SERIALIZABLE"
			}
		}
		if begin == "" {
			return nil, wrapError(&model.Error{Code: "0A000", Message: fmt.Sprintf("isolation level %q is not supported; the engine is serializable", sql.IsolationLevel(options.Isolation))})
		}
	}
	if options.ReadOnly {
		begin += " READ ONLY"
	}
	if _, err := c.execSQL(ctx, begin, nil); err != nil {
		return nil, err
	}
	return &transaction{conn: c, ctx: ctx}, nil
}

func (c *connection) CheckNamedValue(nv *driver.NamedValue) error {
	if nv == nil || nv.Value == nil {
		return nil
	}
	if valuer, ok := nv.Value.(driver.Valuer); ok {
		v, err := valuer.Value()
		if err != nil {
			return err
		}
		nv.Value = v
		if v == nil {
			return nil
		}
	}
	switch v := nv.Value.(type) {
	case int64, float64, bool, string, []byte, time.Time:
		return nil
	case int:
		nv.Value = int64(v)
		return nil
	case int32:
		nv.Value = int64(v)
		return nil
	case int16:
		nv.Value = int64(v)
		return nil
	case int8:
		nv.Value = int64(v)
		return nil
	case uint64:
		if v <= math.MaxInt64 {
			nv.Value = int64(v)
		} else {
			nv.Value = strconv.FormatUint(v, 10)
		}
		return nil
	case uint:
		if uint64(v) <= math.MaxInt64 {
			nv.Value = int64(v)
		} else {
			nv.Value = strconv.FormatUint(uint64(v), 10)
		}
		return nil
	case uint32:
		nv.Value = int64(v)
		return nil
	case uint16:
		nv.Value = int64(v)
		return nil
	case uint8:
		nv.Value = int64(v)
		return nil
	case float32:
		nv.Value = float64(v)
		return nil
	case [16]byte:
		nv.Value = fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
		return nil
	case []string:
		nv.Value = formatPGStringArray(v)
		return nil
	case []int:
		parts := make([]string, len(v))
		for i, elem := range v {
			parts[i] = strconv.Itoa(elem)
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case []int64:
		parts := make([]string, len(v))
		for i, elem := range v {
			parts[i] = strconv.FormatInt(elem, 10)
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case []int32:
		parts := make([]string, len(v))
		for i, elem := range v {
			parts[i] = strconv.FormatInt(int64(elem), 10)
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case []int16:
		parts := make([]string, len(v))
		for i, elem := range v {
			parts[i] = strconv.FormatInt(int64(elem), 10)
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case []float64:
		parts := make([]string, len(v))
		for i, elem := range v {
			parts[i] = strconv.FormatFloat(elem, 'g', -1, 64)
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case []bool:
		parts := make([]string, len(v))
		for i, elem := range v {
			if elem {
				parts[i] = "t"
			} else {
				parts[i] = "f"
			}
		}
		nv.Value = "{" + strings.Join(parts, ",") + "}"
		return nil
	case json.RawMessage:
		nv.Value = string(v)
		return nil
	case map[string]any:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		nv.Value = string(raw)
		return nil
	default:
		// Delegate custom Go scalar types (e.g. type UserID int64, type Status
		// string) and pointers to database/sql's DefaultParameterConverter.
		return driver.ErrSkip
	}
}

func formatPGStringArray(elems []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for j := 0; j < len(s); j++ {
			ch := s[j]
			if ch == '"' || ch == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(ch)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func formatCommandTag(tag string, rowsAffected uint64) string {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.Contains(tag, " ") {
		return tag
	}
	switch strings.ToUpper(tag) {
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", rowsAffected)
	case "UPDATE", "DELETE", "SELECT", "MOVE", "FETCH", "COPY":
		return fmt.Sprintf("%s %d", strings.ToUpper(tag), rowsAffected)
	default:
		return tag
	}
}

func (c *connection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	out, err := c.execSQL(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return driverResult{
		tag:          formatCommandTag(out.Tag, out.RowsAffected),
		rowsAffected: int64(out.RowsAffected),
	}, nil
}

func (c *connection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	parsed, err := c.scope.txns.Parse(ctx, query, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return nil, wrapError(err)
	}
	return c.queryParsed(ctx, parsed, args)
}

// DescribeQuery binds a single statement against the session's current catalog
// snapshot without acquiring data-table locks or executing scans, returning its
// result column names, PostgreSQL type OIDs, and type modifiers.
func (c *connection) DescribeQuery(ctx context.Context, query string, args []driver.NamedValue) ([]string, []uint32, []int32, error) {
	if err := c.check(); err != nil {
		return nil, nil, nil, err
	}
	parsed, err := c.scope.txns.Parse(ctx, query, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return nil, nil, nil, wrapError(err)
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return nil, nil, nil, ErrMissingStatement
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return nil, nil, nil, wrapError(err)
	}
	builtins := c.scope.builtins()
	execution := session.PreparedExecution{
		Catalog:     c.database.catalog,
		Statistics:  ptrTo(c.scope.statisticsCommandOptions(builtins)),
		TypeRuntime: c.scope.typeRuntimeOptions(builtins),
		Routines:    c.scope.routineExecutionOptions(builtins),
	}
	execution.Environment.Builtin = builtins
	cols, err := c.scope.txns.DescribeStatement(ctx, parsed.ParseResult.Stmts[0], session.StatementExecution{
		Execution:  execution,
		Parameters: declaredParameterLayouts(parameters),
		Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
			return c.scope.prepareOptions(ctx, snapshot, reader, builtins)
		},
	})
	if err != nil {
		return nil, nil, nil, wrapError(err)
	}
	names := make([]string, len(cols))
	oids := make([]uint32, len(cols))
	mods := make([]int32, len(cols))
	for i, col := range cols {
		names[i] = col.Name
		oids[i] = uint32(col.Layout.Type.ID)
		mods[i] = col.Layout.Type.Modifier
	}
	return names, oids, mods, nil
}

// queryParsed streams statements through a per-statement pump. Single-statement
// queries stream directly; multi-statement queries advance through result sets
// via driver.RowsNextResultSet while skipping intermediate non-row statements.
func (c *connection) queryParsed(ctx context.Context, parsed *zpgparse.ExtendedParseResult, args []driver.NamedValue) (driver.Rows, error) {
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return nil, ErrMissingStatement
	}
	if len(args) != 0 && len(parsed.ParseResult.Stmts) != 1 {
		return nil, wrapError(&model.Error{Code: "42601", Message: "cannot insert multiple commands into a parameterized statement"})
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return nil, wrapError(err)
	}
	rows := &queryRows{
		conn:       c,
		ctx:        ctx,
		parsed:     parsed,
		parameters: parameters,
		builtins:   c.scope.builtins(),
		connDone:   c.done,
	}
	if err := rows.advanceToNextResultSet(true); err != nil {
		return nil, wrapError(err)
	}
	return rows, nil
}

// queryPrepared streams a cached *session.Prepared directly through queryRows.
func (c *connection) queryPrepared(ctx context.Context, prepared *session.Prepared, parameters []expr.ParameterValue) (*queryRows, error) {
	rows := &queryRows{
		conn:     c,
		ctx:      ctx,
		builtins: c.scope.builtins(),
		connDone: c.done,
	}
	if err := rows.startStream(func(pumpCtx context.Context, stream rowStream) error {
		_, runErr := c.database.runPreparedCore(pumpCtx, c.scope, prepared, parameters, 0, stream)
		return runErr
	}); err != nil {
		return nil, wrapError(err)
	}
	return rows, nil
}

func (c *connection) Ping(ctx context.Context) error {
	if err := c.check(); err != nil {
		return err
	}
	return ctx.Err()
}

// ResetSession sanitizes a pooled connection before reuse: an active or failed
// transaction replaces the entire session scope, while a clean session resets
// all session settings, custom GUCs, active role, session_user, sequence
// currval/lastval state, held cursors, SQL prepared statements, temporary
// objects, and session advisory locks so state never leaks across callers.
func (c *connection) ResetSession(ctx context.Context) error {
	if c == nil || c.closed.Load() || c.database == nil || c.database.checkOpen() != nil {
		return driver.ErrBadConn
	}
	if !c.scope.txns.Active() {
		if err := c.scope.settings.ResetAllSessionState(ctx); err != nil {
			return driver.ErrBadConn
		}
		if c.scope.sequences != nil {
			if err := c.scope.sequences.Discard(ctx); err != nil {
				return driver.ErrBadConn
			}
		}
		if err := c.scope.txns.ResetSessionObjects(ctx); err != nil {
			return driver.ErrBadConn
		}
		return nil
	}
	fresh, err := c.database.newSession(ctx)
	if err != nil {
		return driver.ErrBadConn
	}
	replaced := c.scope
	c.scope = fresh
	_ = replaced.close()
	return nil
}

func (c *connection) IsValid() bool {
	return c != nil && !c.closed.Load() && c.database != nil && c.database.checkOpen() == nil
}

// transaction is one driver.Tx over the connection's session.
type transaction struct {
	conn     *connection
	ctx      context.Context
	finished atomic.Bool
}

var _ driver.Tx = (*transaction)(nil)

func (tx *transaction) Commit() error {
	if !tx.finished.CompareAndSwap(false, true) {
		return errors.New("supersql: transaction already finished")
	}
	_, err := tx.conn.execSQL(tx.ctx, "COMMIT", nil)
	if err != nil && tx.conn != nil && tx.conn.scope != nil && tx.conn.scope.txns != nil &&
		!errors.Is(err, storage.ErrCommitIndeterminate) && !errors.Is(err, ErrDatabaseClosed) && !errors.Is(err, driver.ErrBadConn) &&
		tx.conn.scope.txns.Active() && !tx.conn.scope.txns.Indeterminate() {
		rollbackCtx := context.Background()
		if tx.ctx != nil {
			rollbackCtx = context.WithoutCancel(tx.ctx)
		}
		if _, rbErr := tx.conn.execSQL(rollbackCtx, "ROLLBACK", nil); rbErr != nil && !errors.Is(rbErr, storage.ErrCommitIndeterminate) {
			err = errors.Join(err, rbErr)
		}
	}
	return wrapError(err)
}

func (tx *transaction) Rollback() error {
	if !tx.finished.CompareAndSwap(false, true) {
		return errors.New("supersql: transaction already finished")
	}
	rollbackCtx := context.Background()
	if tx.ctx != nil {
		// Detach cancellation so an already-canceled or timed-out BeginTx
		// context still executes ROLLBACK and releases locks and snapshot pins.
		rollbackCtx = context.WithoutCancel(tx.ctx)
	}
	_, err := tx.conn.execSQL(rollbackCtx, "ROLLBACK", nil)
	return wrapError(err)
}

type cachedStatementPlan struct {
	prepared     *session.Prepared
	identity     txn.CatalogReadIdentity
	ddlSeq       uint64
	tempVersion  uint64
	currentRole  string
	sessionUser  string
	searchPath   []string
	builtins     builtin.ResolveOptions
	paramLayouts []model.TypeLayout
}

// statement is one driver.Stmt: a syntax-checked single statement that caches
// bound/planned state keyed by CatalogReadIdentity, role, search_path, semantic
// settings, and declared parameter types, invalidating automatically on DDL or
// session context changes.
type statement struct {
	conn   *connection
	parsed *zpgparse.ExtendedParseResult
	closed atomic.Bool

	mu                sync.Mutex
	cached            *cachedStatementPlan
	parameterizedScan bool
	planCacheHits     atomic.Uint64
	planCacheMisses   atomic.Uint64
}

var (
	_ driver.Stmt             = (*statement)(nil)
	_ driver.StmtExecContext  = (*statement)(nil)
	_ driver.StmtQueryContext = (*statement)(nil)
)

func (s *statement) Close() error {
	s.closed.Store(true)
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
	return nil
}

// NumInput reports unknown arity: the binder resolves parameter types from
// context and rejects a wrong count with its own diagnostic.
func (s *statement) NumInput() int { return -1 }

func (s *statement) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), driverNamedValues(args))
}

func (s *statement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if s.closed.Load() {
		return nil, driver.ErrBadConn
	}
	if err := s.conn.check(); err != nil {
		return nil, err
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return nil, wrapError(err)
	}
	if prepared, ok := s.resolveCachedPlan(ctx, parameters); ok {
		out, runErr := s.conn.database.runPreparedCore(ctx, s.conn.scope, prepared, parameters, 0, nil)
		if runErr == nil {
			return driverResult{
				tag:          formatCommandTag(out.Tag, out.RowsAffected),
				rowsAffected: int64(out.RowsAffected),
			}, nil
		}
		if !isStalePreparedError(runErr) {
			return nil, wrapError(runErr)
		}
		s.evictCachedPlan()
	}
	out, err := s.conn.execParsed(ctx, s.parsed, args)
	if err != nil {
		return nil, wrapError(err)
	}
	return driverResult{
		tag:          formatCommandTag(out.Tag, out.RowsAffected),
		rowsAffected: int64(out.RowsAffected),
	}, nil
}

func (s *statement) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), driverNamedValues(args))
}

func (s *statement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.closed.Load() {
		return nil, driver.ErrBadConn
	}
	if err := s.conn.check(); err != nil {
		return nil, err
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return nil, wrapError(err)
	}
	if prepared, ok := s.resolveCachedPlan(ctx, parameters); ok {
		rows, runErr := s.conn.queryPrepared(ctx, prepared, parameters)
		if runErr == nil {
			return rows, nil
		}
		if !isStalePreparedError(runErr) {
			return nil, wrapError(runErr)
		}
		s.evictCachedPlan()
	}
	return s.conn.queryParsed(ctx, s.parsed, args)
}

func (s *statement) evictCachedPlan() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

func cacheablePreparedStmt(parsed *zpgparse.ExtendedParseResult) bool {
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return false
	}
	if len(parsed.TemporalClauses) != 0 || len(parsed.OnlineDDLClauses) != 0 {
		return false
	}
	raw := parsed.ParseResult.Stmts[0]
	if raw == nil || raw.Stmt == nil {
		return false
	}
	node := raw.Stmt
	return node.GetSelectStmt() != nil ||
		node.GetInsertStmt() != nil ||
		node.GetUpdateStmt() != nil ||
		node.GetDeleteStmt() != nil ||
		node.GetMergeStmt() != nil
}

func preparedHasScans(prepared *session.Prepared) bool {
	if prepared == nil || prepared.Graph() == nil || prepared.Graph().Plan() == nil {
		return false
	}
	plan := prepared.Graph().Plan()
	for index := uint32(1); index <= plan.NodeCount(); index++ {
		node, ok := plan.Node(physical.NodeID(index))
		if !ok {
			continue
		}
		alg := node.Metadata().Algorithm
		if alg == physical.TableScan || alg == physical.SecondaryScan {
			return true
		}
	}
	return false
}

func equalTypeLayouts(a, b []model.TypeLayout) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !model.TypeLayoutsEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func isStalePreparedError(err error) bool {
	if SQLState(err) != "0A000" {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "prepared query") && strings.Contains(msg, "prepare again")
}

// resolveCachedPlan returns a cached *session.Prepared when the statement is
// eligible and the catalog identity, temporary catalog version, role,
// search_path, semantic settings, and declared parameter layouts match.
// Parameterized queries containing table or index scans bypass the generic
// cache so runtime parameters specialize index spans on every invocation.
func (s *statement) resolveCachedPlan(ctx context.Context, parameters []expr.ParameterValue) (*session.Prepared, bool) {
	c := s.conn
	if c == nil || c.database == nil || c.scope == nil || c.database.readOnly || c.database.checkOpen() != nil {
		return nil, false
	}
	active, tempDescriptors, tempVersion, searchPath := c.scope.txns.TemporaryCatalogSnapshot(ctx)
	if active {
		return nil, false
	}
	if !cacheablePreparedStmt(s.parsed) {
		return nil, false
	}
	s.mu.Lock()
	if len(parameters) > 0 && s.parameterizedScan {
		s.mu.Unlock()
		return nil, false
	}
	cached := s.cached
	s.mu.Unlock()

	view, err := c.database.manager.ClosedReadView(ctx)
	if err != nil {
		return nil, false
	}
	defer func() { _ = view.Close() }()

	var identity txn.CatalogReadIdentity
	if identified, ok := view.(interface{ CatalogIdentity() txn.CatalogReadIdentity }); ok {
		identity = identified.CatalogIdentity()
	}
	if identity == (txn.CatalogReadIdentity{}) {
		return nil, false
	}

	ddlSeq := c.scope.ddlSeq.Load()
	currentRole := c.scope.settings.CurrentRole()
	sessionUser := c.scope.settings.SessionUser()
	baseBuiltins := c.scope.builtins()
	declaredLayouts := declaredParameterLayouts(parameters)

	if cached != nil &&
		cached.identity == identity &&
		cached.ddlSeq == ddlSeq &&
		cached.tempVersion == tempVersion &&
		cached.currentRole == currentRole &&
		cached.sessionUser == sessionUser &&
		slices.Equal(cached.searchPath, searchPath) &&
		baseBuiltins.SameSemantics(cached.builtins) &&
		equalTypeLayouts(cached.paramLayouts, declaredLayouts) {
		s.planCacheHits.Add(1)
		return cached.prepared, true
	}

	snapshot, err := c.database.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, false
	}
	defer func() { _ = snapshot.Close() }()

	if err := snapshot.AddSessionOverlay(tempDescriptors); err != nil {
		return nil, false
	}

	types, err := c.scope.txns.DeclaredTypeRuntime(ctx, snapshot, *c.database.typeRuntimeOptions(baseBuiltins))
	if err != nil {
		return nil, false
	}
	planBuiltins := baseBuiltins
	planBuiltins.Types = types

	prepareOpts, err := c.scope.prepareOptions(ctx, snapshot, view, planBuiltins)
	if err != nil {
		return nil, false
	}
	prepareOpts.Normalize.Builtin = planBuiltins
	prepareOpts.Bind.Builtin = planBuiltins
	prepareOpts.Bind.SearchPath = searchPath
	if effectiveWork := c.scope.settings.Resources().EffectiveWorkMemoryBytes; effectiveWork > 0 && prepareOpts.Optimize.WorkingMemory > 0 {
		prepareOpts.Optimize.WorkingMemory = uint64(effectiveWork)
	}
	prepareOpts.Optimize.StatisticsKeyEncoder = c.database.keys.WithResolveOptions(planBuiltins)

	rawStmt := s.parsed.ParseResult.Stmts[0]
	prepared, err := session.Prepare(ctx, rawStmt, snapshot, prepareOpts, declaredLayouts)
	if err != nil || prepared == nil || prepared.Graph() == nil {
		return nil, false
	}
	if len(parameters) > 0 && preparedHasScans(prepared) {
		// Parameterized scan queries benefit from per-invocation span narrowing
		// via Optimize.Parameters rather than a generic full-scan plan.
		s.mu.Lock()
		s.parameterizedScan = true
		s.mu.Unlock()
		return nil, false
	}

	entry := &cachedStatementPlan{
		prepared:     prepared,
		identity:     identity,
		ddlSeq:       ddlSeq,
		tempVersion:  tempVersion,
		currentRole:  currentRole,
		sessionUser:  sessionUser,
		searchPath:   slices.Clone(searchPath),
		builtins:     baseBuiltins,
		paramLayouts: slices.Clone(declaredLayouts),
	}
	s.mu.Lock()
	s.cached = entry
	s.mu.Unlock()
	s.planCacheMisses.Add(1)
	return prepared, true
}

func driverNamedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, value := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
	}
	return named
}

type driverResult struct {
	tag          string
	rowsAffected int64
}

var _ driver.Result = driverResult{}

func (r driverResult) LastInsertId() (int64, error) {
	return 0, errors.New("supersql: no generated integer key is associated with the statement")
}

func (r driverResult) RowsAffected() (int64, error) { return r.rowsAffected, nil }

func (r driverResult) CommandTag() string { return r.tag }

// rowStreamState owns the channel lifecycle for one streaming statement inside
// queryRows, keeping multi-statement NextResultSet transitions race-free.
type rowStreamState struct {
	incoming     chan [][]model.Value
	ready        chan []bind.ResultColumn
	done         chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
	cancel       context.CancelFunc
	err          error
	errDelivered atomic.Bool
}

func (s *rowStreamState) finish() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
		<-s.done
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// queryRows is a streaming driver.Rows: a pump goroutine runs the statement
// invocation and hands copied row batches across a bounded channel, so Next
// pulls rows instead of reading a materialized result. Close stops the row
// feed, waits for the session to unwind its portal, and surfaces any deferred
// statement or write-draining error (e.g. from INSERT ... RETURNING).
type queryRows struct {
	conn       *connection
	ctx        context.Context
	parsed     *zpgparse.ExtendedParseResult
	parameters []expr.ParameterValue
	nextStmt   int

	columns  []bind.ResultColumn
	builtins builtin.ResolveOptions
	connDone <-chan struct{}

	mu        sync.Mutex
	state     *rowStreamState
	closedAll bool
	closeErr  error

	current [][]model.Value
	pos     int
	codecs  []builtin.TypeTextCodec
}

var (
	_ driver.Rows                           = (*queryRows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*queryRows)(nil)
	_ driver.RowsColumnTypeScanType         = (*queryRows)(nil)
	_ driver.RowsColumnTypeNullable         = (*queryRows)(nil)
	_ driver.RowsNextResultSet              = (*queryRows)(nil)
)

func (rows *queryRows) Columns() []string {
	names := make([]string, len(rows.columns))
	for index, col := range rows.columns {
		names[index] = col.Name
	}
	return names
}

func (rows *queryRows) ColumnOIDs() ([]uint32, []int32) {
	oids := make([]uint32, len(rows.columns))
	mods := make([]int32, len(rows.columns))
	for index, col := range rows.columns {
		oids[index] = uint32(col.Layout.Type.ID)
		mods[index] = col.Layout.Type.Modifier
	}
	return oids, mods
}

// ColumnTypeOID returns the PostgreSQL type OID and type modifier for column index.
func (rows *queryRows) ColumnTypeOID(index int) (uint32, int32) {
	if index < 0 || index >= len(rows.columns) {
		return 0, -1
	}
	return uint32(rows.columns[index].Layout.Type.ID), rows.columns[index].Layout.Type.Modifier
}

func (rows *queryRows) ColumnTypeDatabaseTypeName(index int) string {
	if index < 0 || index >= len(rows.columns) {
		return ""
	}
	return pgDatabaseTypeName(rows.columns[index].Layout.Type.ID)
}

func (rows *queryRows) ColumnTypeScanType(index int) reflect.Type {
	if index < 0 || index >= len(rows.columns) {
		return reflect.TypeFor[any]()
	}
	switch rows.columns[index].Layout.Type.ID {
	case 16:
		return reflect.TypeFor[bool]()
	case 17:
		return reflect.TypeFor[[]byte]()
	case 20, 21, 23, 26:
		return reflect.TypeFor[int64]()
	case 700, 701:
		return reflect.TypeFor[float64]()
	case 1082, 1114, 1184:
		return reflect.TypeFor[time.Time]()
	default:
		return reflect.TypeFor[string]()
	}
}

func (rows *queryRows) ColumnTypeNullable(index int) (nullable, ok bool) {
	if index < 0 || index >= len(rows.columns) {
		return false, false
	}
	return rows.columns[index].Nullable, true
}

func pgDatabaseTypeName(id model.TypeID) string {
	switch id {
	case 16:
		return "BOOL"
	case 17:
		return "BYTEA"
	case 18:
		return "CHAR"
	case 19:
		return "NAME"
	case 20:
		return "INT8"
	case 21:
		return "INT2"
	case 23:
		return "INT4"
	case 25:
		return "TEXT"
	case 26:
		return "OID"
	case 114:
		return "JSON"
	case 700:
		return "FLOAT4"
	case 701:
		return "FLOAT8"
	case 705:
		return "UNKNOWN"
	case 1000:
		return "_BOOL"
	case 1001:
		return "_BYTEA"
	case 1005:
		return "_INT2"
	case 1007:
		return "_INT4"
	case 1009:
		return "_TEXT"
	case 1014:
		return "_BPCHAR"
	case 1015:
		return "_VARCHAR"
	case 1016:
		return "_INT8"
	case 1021:
		return "_FLOAT4"
	case 1022:
		return "_FLOAT8"
	case 1028:
		return "_OID"
	case 1042:
		return "BPCHAR"
	case 1043:
		return "VARCHAR"
	case 1082:
		return "DATE"
	case 1083:
		return "TIME"
	case 1114:
		return "TIMESTAMP"
	case 1184:
		return "TIMESTAMPTZ"
	case 1186:
		return "INTERVAL"
	case 1231:
		return "_NUMERIC"
	case 1266:
		return "TIMETZ"
	case 1560:
		return "BIT"
	case 1562:
		return "VARBIT"
	case 1700:
		return "NUMERIC"
	case 2950:
		return "UUID"
	case 2951:
		return "_UUID"
	case 3802:
		return "JSONB"
	case 3807:
		return "_JSONB"
	default:
		return fmt.Sprintf("OID(%d)", id)
	}
}

func (rows *queryRows) startStream(run func(pumpCtx context.Context, stream rowStream) error) error {
	pumpCtx, cancel := context.WithCancel(rows.ctx)
	state := &rowStreamState{
		incoming: make(chan [][]model.Value, 4),
		ready:    make(chan []bind.ResultColumn, 1),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		cancel:   cancel,
	}
	rows.mu.Lock()
	rows.state = state
	rows.mu.Unlock()
	rows.current = nil
	rows.pos = 0
	rows.codecs = nil

	go func() {
		// LIFO: incoming closes after err is set and before done, so a Next
		// that drains the channel observes the terminal error deterministically.
		defer close(state.done)
		defer close(state.incoming)
		state.err = run(pumpCtx, func(ctx context.Context, columns []bind.ResultColumn, builtins builtin.ResolveOptions, source rowSource) error {
			return rows.streamToState(state, ctx, columns, builtins, source)
		})
	}()

	select {
	case columns := <-state.ready:
		rows.columns = columns
		return nil
	case <-state.done:
		if state.err != nil {
			cancel()
			return state.err
		}
		select {
		case rows.columns = <-state.ready:
		default:
			rows.columns = nil
		}
		return nil
	case <-rows.connDone:
		cancel()
		<-state.done
		return driver.ErrBadConn
	}
}

func (rows *queryRows) advanceToNextResultSet(initial bool) error {
	if rows.parsed == nil || rows.parsed.ParseResult == nil {
		return io.EOF
	}
	stmts := rows.parsed.ParseResult.Stmts
	for rows.nextStmt < len(stmts) {
		index := rows.nextStmt
		rows.nextStmt++
		var params []expr.ParameterValue
		if len(stmts) == 1 {
			params = rows.parameters
		}
		stmtParsed := rows.conn.scope.rewriteCompatIsolation(rows.parsed, index)
		err := rows.startStream(func(pumpCtx context.Context, stream rowStream) error {
			_, runErr := rows.conn.database.execStatement(pumpCtx, rows.conn.scope, stmtParsed, index, params, 0, stream, nil)
			return runErr
		})
		if err != nil {
			return err
		}
		// Skip intermediate statements that produce no columns (e.g. SET/DDL
		// preceding a SELECT), stopping on the first result set with columns or
		// on the final statement for an initial call.
		if len(rows.columns) > 0 || (initial && rows.nextStmt >= len(stmts)) {
			return nil
		}
		rows.mu.Lock()
		state := rows.state
		rows.mu.Unlock()
		state.finish()
		if state != nil && state.err != nil {
			return state.err
		}
	}
	return io.EOF
}

func (rows *queryRows) HasNextResultSet() bool {
	rows.mu.Lock()
	closed := rows.closedAll
	rows.mu.Unlock()
	if closed || rows.parsed == nil || rows.parsed.ParseResult == nil {
		return false
	}
	return rows.nextStmt < len(rows.parsed.ParseResult.Stmts)
}

func (rows *queryRows) NextResultSet() error {
	rows.mu.Lock()
	closed := rows.closedAll
	prev := rows.state
	rows.mu.Unlock()
	if closed {
		return io.EOF
	}
	if prev != nil {
		prev.finish()
		if prev.err != nil && !prev.errDelivered.Swap(true) {
			return wrapError(prev.err)
		}
	}
	if !rows.HasNextResultSet() {
		return io.EOF
	}
	err := rows.advanceToNextResultSet(false)
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	return wrapError(err)
}

// streamToState is the session-side consume callback: it publishes the result
// columns once, then forwards each batch's copied rows to the driver side. An
// early Close returns nil so SELECT completes cleanly while write statements
// (e.g. INSERT ... RETURNING) still drain remaining mutations to completion.
func (rows *queryRows) streamToState(state *rowStreamState, ctx context.Context, columns []bind.ResultColumn, builtins builtin.ResolveOptions, source rowSource) error {
	// The statement's own resolve options carry the declared-type resolver, so
	// driver-side rendering of enum and domain columns stays valid after the
	// statement releases its snapshot pin.
	if builtins.Types != nil {
		rows.builtins.Types = builtins.Types
	}
	select {
	case state.ready <- columns:
	default:
	}
	if source == nil {
		return nil
	}
	var batch columnar.Batch
	for {
		select {
		case <-state.closed:
			return nil
		default:
		}
		if err := source.Next(ctx, &batch); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		out := make([][]model.Value, 0, batch.Rows())
		for lane := range batch.Rows() {
			row := make([]model.Value, len(batch.Columns))
			for index := range batch.Columns {
				value, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(lane), 32<<20)
				if copyErr != nil {
					return copyErr
				}
				row[index] = value
			}
			out = append(out, row)
		}
		select {
		case state.incoming <- out:
		case <-state.closed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-rows.connDone:
			return driver.ErrBadConn
		}
	}
}

func (rows *queryRows) Close() error {
	rows.mu.Lock()
	if rows.closedAll {
		err := rows.closeErr
		rows.mu.Unlock()
		return err
	}
	rows.closedAll = true
	state := rows.state
	rows.mu.Unlock()

	var closeErr error
	if state != nil {
		state.finish()
		if state.err != nil && !state.errDelivered.Swap(true) {
			closeErr = wrapError(state.err)
		}
	}

	rows.mu.Lock()
	rows.closeErr = closeErr
	rows.mu.Unlock()
	return closeErr
}

func (rows *queryRows) Next(dest []driver.Value) error {
	rows.mu.Lock()
	state := rows.state
	rows.mu.Unlock()
	if rows.ctx != nil {
		if err := rows.ctx.Err(); err != nil {
			if state != nil {
				state.finish()
				state.errDelivered.Store(true)
			}
			return err
		}
	}
	if state == nil {
		return io.EOF
	}
	var ctxDone <-chan struct{}
	if rows.ctx != nil {
		ctxDone = rows.ctx.Done()
	}
	for rows.pos >= len(rows.current) {
		select {
		case batch, ok := <-state.incoming:
			if !ok {
				<-state.done
				if state.err != nil {
					state.errDelivered.Store(true)
					return wrapError(state.err)
				}
				return io.EOF
			}
			rows.current = batch
			rows.pos = 0
		case <-ctxDone:
			state.finish()
			state.errDelivered.Store(true)
			return rows.ctx.Err()
		case <-rows.connDone:
			if state.cancel != nil {
				state.cancel()
			}
			<-state.done
			return driver.ErrBadConn
		}
	}
	row := rows.current[rows.pos]
	rows.pos++
	for index, value := range row {
		if index >= len(dest) {
			break
		}
		converted, err := rows.driverValue(index, value)
		if err != nil {
			return wrapError(err)
		}
		dest[index] = converted
	}
	return nil
}

// postgresEpochMicros is 2000-01-01 UTC in Unix microseconds: SQL timestamp
// values count microseconds from it, never certified clock units.
const postgresEpochMicros = int64(946684800000000)

var postgresEpoch = time.UnixMicro(postgresEpochMicros).UTC()

// driverValue converts one stored datum for database/sql. The whitelisted
// PostgreSQL OIDs map to their canonical Go type; every other type renders
// through its registered typoutput function as text, like a wire-protocol
// text-format column.
func (rows *queryRows) driverValue(index int, value model.Value) (driver.Value, error) {
	if value.IsNull {
		return nil, nil
	}
	var layout model.TypeLayout
	if index < len(rows.columns) {
		layout = rows.columns[index].Layout
	}
	switch layout.Type.ID {
	case 16:
		return value.IntVal != 0, nil // bool
	case 17:
		return value.BytesVal, nil // bytea
	case 18, 19, 25, 705, 1042, 1043, 2278:
		return value.StrVal, nil // char, name, text, unknown, bpchar, varchar, void
	case 20, 21, 23, 26:
		return value.IntVal, nil // int8, int2, int4, oid
	case 700, 701:
		return value.FloatVal, nil // float4, float8
	case 1082:
		return postgresEpoch.AddDate(0, 0, int(value.IntVal)), nil // date: days since epoch
	case 1114, 1184:
		return time.UnixMicro(value.IntVal + postgresEpochMicros).UTC(), nil // timestamp[tz]: micros
	}
	codec, err := rows.textCodec(index, layout)
	if err != nil {
		return nil, err
	}
	rendered, err := codec.Output(context.Background(), []model.Value{value}, builtin.CallBudget{MemoryBytes: 32 << 20, OutputBytes: 32 << 20})
	if err != nil {
		return nil, err
	}
	return rendered.StrVal, nil
}

func (rows *queryRows) textCodec(index int, layout model.TypeLayout) (builtin.TypeTextCodec, error) {
	if rows.codecs == nil {
		rows.codecs = make([]builtin.TypeTextCodec, len(rows.columns))
	}
	if codec := rows.codecs[index]; codec.Output != nil {
		return codec, nil
	}
	codec, err := builtin.ResolveTypeTextCodec(context.Background(), layout, rows.builtins, 4<<20)
	if err != nil {
		return builtin.TypeTextCodec{}, err
	}
	rows.codecs[index] = codec
	return codec, nil
}

// driverParameters converts driver values into typed parameters. Values whose
// Go type maps to exactly one SQL type declare that type so binding plans the
// casts PostgreSQL would apply to a typed wire parameter; date, time,
// timestamp, and timestamptz share storage with different units, so context
// inference cannot safely pick between them. Strings and NULLs stay
// undeclared: strings keep context inference plus input coercion, and NULLs
// adopt the bound layout's storage tag.
func driverParameters(args []driver.NamedValue) ([]expr.ParameterValue, error) {
	if len(args) == 0 {
		return nil, nil
	}
	parameters := make([]expr.ParameterValue, len(args))
	for index, argument := range args {
		value, layout, err := driverParameterValue(argument.Value)
		if err != nil {
			return nil, err
		}
		parameters[index] = expr.ParameterValue{Number: uint32(argument.Ordinal), Layout: layout, Value: value}
	}
	return parameters, nil
}

func driverParameterValue(value driver.Value) (model.Value, model.TypeLayout, error) {
	switch typed := value.(type) {
	case nil:
		return model.Value{IsNull: true}, model.TypeLayout{}, nil
	case int64:
		return model.Int64Val(typed), declaredParameterLayout(20, model.TypeInt64), nil
	case float64:
		return model.Float64Val(typed), declaredParameterLayout(701, model.TypeFloat64), nil
	case bool:
		return model.BoolVal(typed), declaredParameterLayout(16, model.TypeBoolean), nil
	case string:
		return model.StringVal(typed), model.TypeLayout{}, nil
	case []byte:
		return model.BytesVal(typed), declaredParameterLayout(17, model.TypeBytes), nil
	case time.Time:
		return model.Value{Type: model.TypeTimestamp, IntVal: typed.UTC().UnixMicro() - postgresEpochMicros}, declaredParameterLayout(1184, model.TypeTimestamp), nil
	default:
		return model.Value{}, model.TypeLayout{}, &model.Error{Code: "0A000", Message: fmt.Sprintf("unsupported parameter type %T", value)}
	}
}

// declaredParameterLayout is the wire-declared type for one driver value: an
// unmodified scalar layout whose storage tag matches the value's encoding.
func declaredParameterLayout(id model.TypeID, storage model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: storage}}
}
