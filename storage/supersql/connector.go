package supersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	expr "github.com/gosuda/supersql-alpha/internal/query/exec/expr"
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
	if options.Clock == nil {
		return nil, ErrClockRequired
	}
	database, err := Open(ctx, options)
	if err != nil {
		return nil, err
	}
	return &Connector{database: database}, nil
}

// Connect returns a driver.Conn bound to a new isolated sessionScope. It fails
// with ErrDatabaseClosed after Close or once the owning Catalog is closed.
func (connector *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	if connector == nil || connector.closed.Load() || connector.database == nil {
		return nil, ErrDatabaseClosed
	}
	scope, err := connector.database.newSession(ctx)
	if err != nil {
		return nil, err
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
	if connector == nil || connector.closed.Load() {
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
	return nil
}

// execSQL parses with this session's lexical settings and runs each statement
// through the session pipeline, returning the last statement's outcome.
func (c *connection) execSQL(ctx context.Context, text string, args []driver.NamedValue) (Outcome, error) {
	parsed, err := c.scope.txns.Parse(ctx, text, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return Outcome{}, err
	}
	return c.execParsed(ctx, parsed, args)
}

func (c *connection) execParsed(ctx context.Context, parsed *zpgparse.ExtendedParseResult, args []driver.NamedValue) (Outcome, error) {
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return Outcome{}, ErrMissingStatement
	}
	if len(args) != 0 && len(parsed.ParseResult.Stmts) != 1 {
		return Outcome{}, &model.Error{Code: "42601", Message: "cannot insert multiple commands into a parameterized statement"}
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return Outcome{}, err
	}
	var outcome Outcome
	for index := range parsed.ParseResult.Stmts {
		stmtParsed := c.scope.rewriteCompatIsolation(parsed, index)
		if outcome, err = c.database.execStatement(ctx, c.scope, stmtParsed, index, parameters, 0, nil, nil); err != nil {
			return outcome, err
		}
	}
	return outcome, nil
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
		return nil, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) != 1 {
		return nil, &model.Error{Code: "42601", Message: "a prepared statement must contain exactly one statement"}
	}
	return &statement{conn: c, parsed: parsed}, nil
}

func (c *connection) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(c.done)
	return c.scope.close()
}

func (c *connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *connection) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if err := c.check(); err != nil {
		return nil, err
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
			return nil, &model.Error{Code: "0A000", Message: fmt.Sprintf("isolation level %q is not supported; the engine is serializable", sql.IsolationLevel(options.Isolation))}
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
	case uint:
		nv.Value = int64(v)
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
		return nil
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
	outcome, err := c.execSQL(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return driverResult{
		tag:          formatCommandTag(outcome.Tag, outcome.RowsAffected),
		rowsAffected: int64(outcome.RowsAffected),
	}, nil
}

func (c *connection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	parsed, err := c.scope.txns.Parse(ctx, query, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return nil, err
	}
	return c.queryParsed(ctx, parsed, args)
}

// queryParsed streams the last statement's rows through a pump: the session
// invocation runs on its own goroutine so the driver pulls rows from a bounded
// channel instead of materializing the whole result. Statements before the
// last are drained to completion first.
func (c *connection) queryParsed(ctx context.Context, parsed *zpgparse.ExtendedParseResult, args []driver.NamedValue) (driver.Rows, error) {
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return nil, ErrMissingStatement
	}
	if len(args) != 0 && len(parsed.ParseResult.Stmts) != 1 {
		return nil, &model.Error{Code: "42601", Message: "cannot insert multiple commands into a parameterized statement"}
	}
	parameters, err := driverParameters(args)
	if err != nil {
		return nil, err
	}
	statements := parsed.ParseResult.Stmts
	pumpCtx, cancel := context.WithCancel(ctx)
	rows := &queryRows{
		builtins: c.scope.builtins(),
		incoming: make(chan [][]model.Value, 4),
		ready:    make(chan []bind.ResultColumn, 1),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
		connDone: c.done,
		cancel:   cancel,
	}
	go func() {
		// LIFO: incoming closes after err is set and before done, so a Next
		// that drains the channel observes the terminal error deterministically.
		defer close(rows.done)
		defer close(rows.incoming)
		for index := 0; index < len(statements)-1; index++ {
			stmtParsed := c.scope.rewriteCompatIsolation(parsed, index)
			if _, runErr := c.database.execStatement(pumpCtx, c.scope, stmtParsed, index, nil, 0, drainRows, nil); runErr != nil {
				rows.err = runErr
				return
			}
		}
		lastIndex := len(statements) - 1
		stmtParsed := c.scope.rewriteCompatIsolation(parsed, lastIndex)
		_, runErr := c.database.execStatement(pumpCtx, c.scope, stmtParsed, lastIndex, parameters, 0, rows.stream, nil)
		rows.err = runErr
	}()
	select {
	case columns := <-rows.ready:
		rows.columns = columns
		return rows, nil
	case <-rows.done:
		if rows.err != nil {
			return nil, rows.err
		}
		// done implies stream already ran for row statements; its buffered
		// ready send is still waiting, so collect it without blocking.
		select {
		case rows.columns = <-rows.ready:
		default:
		}
		return rows, nil
	case <-c.done:
		cancel()
		<-rows.done
		return nil, driver.ErrBadConn
	}
}

// drainRows consumes a non-terminal statement's row source without retaining
// it, so earlier statements in a multi-statement query commit normally.
func drainRows(ctx context.Context, _ []bind.ResultColumn, _ builtin.ResolveOptions, source rowSource) error {
	if source == nil {
		return nil
	}
	var batch columnar.Batch
	for {
		err := source.Next(ctx, &batch)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *connection) Ping(ctx context.Context) error {
	if err := c.check(); err != nil {
		return err
	}
	return ctx.Err()
}

// ResetSession replaces a dirty session scope — one holding an open or failed
// transaction — so pooled reuse never inherits transactional state. Clean
// sessions keep their settings, prepared statements and temporary objects,
// matching PostgreSQL session semantics across pool checkout.
func (c *connection) ResetSession(ctx context.Context) error {
	if c.closed.Load() {
		return driver.ErrBadConn
	}
	if !c.scope.txns.Active() {
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

func (c *connection) IsValid() bool { return c != nil && !c.closed.Load() }

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
	return err
}

func (tx *transaction) Rollback() error {
	if !tx.finished.CompareAndSwap(false, true) {
		return errors.New("supersql: transaction already finished")
	}
	_, err := tx.conn.execSQL(tx.ctx, "ROLLBACK", nil)
	return err
}

// statement is one driver.Stmt: a syntax-checked single statement re-planned
// per invocation under the connection's own session.
type statement struct {
	conn   *connection
	parsed *zpgparse.ExtendedParseResult
	closed atomic.Bool
}

var (
	_ driver.Stmt             = (*statement)(nil)
	_ driver.StmtExecContext  = (*statement)(nil)
	_ driver.StmtQueryContext = (*statement)(nil)
)

func (s *statement) Close() error {
	s.closed.Store(true)
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
	outcome, err := s.conn.execParsed(ctx, s.parsed, args)
	if err != nil {
		return nil, err
	}
	return driverResult{
		tag:          formatCommandTag(outcome.Tag, outcome.RowsAffected),
		rowsAffected: int64(outcome.RowsAffected),
	}, nil
}

func (s *statement) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), driverNamedValues(args))
}

func (s *statement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if s.closed.Load() {
		return nil, driver.ErrBadConn
	}
	return s.conn.queryParsed(ctx, s.parsed, args)
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

// queryRows is a streaming driver.Rows: a pump goroutine runs the statement
// invocation and hands copied row batches across a bounded channel, so Next
// pulls rows instead of reading a materialized result. Close stops the row
// feed and waits for the session to unwind its portal; it deliberately does
// not cancel the statement context, because a canceled statement aborts the
// surrounding transaction while a portal closed early must not.
type queryRows struct {
	columns  []bind.ResultColumn
	builtins builtin.ResolveOptions

	incoming  chan [][]model.Value
	ready     chan []bind.ResultColumn
	done      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	connDone  <-chan struct{}
	cancel    context.CancelFunc
	err       error

	current [][]model.Value
	pos     int
	codecs  []builtin.TypeTextCodec
}

var _ driver.Rows = (*queryRows)(nil)

func (rows *queryRows) Columns() []string {
	names := make([]string, len(rows.columns))
	for index, column := range rows.columns {
		names[index] = column.Name
	}
	return names
}

func (rows *queryRows) ColumnOIDs() ([]uint32, []int32) {
	oids := make([]uint32, len(rows.columns))
	mods := make([]int32, len(rows.columns))
	for index, column := range rows.columns {
		oids[index] = uint32(column.Layout.Type.ID)
		mods[index] = column.Layout.Type.Modifier
	}
	return oids, mods
}

// stream is the session-side consume callback: it publishes the result columns
// once, then forwards each batch's copied rows to the driver side. An early
// Close returns nil so the statement completes normally; only a statement
// cancellation or connection teardown propagates as an error.
func (rows *queryRows) stream(ctx context.Context, columns []bind.ResultColumn, builtins builtin.ResolveOptions, source rowSource) error {
	// The statement's own resolve options carry the declared-type resolver, so
	// driver-side rendering of enum and domain columns stays valid after the
	// statement releases its snapshot pin.
	if builtins.Types != nil {
		rows.builtins.Types = builtins.Types
	}
	select {
	case rows.ready <- columns:
	default:
	}
	if source == nil {
		return nil
	}
	var batch columnar.Batch
	for {
		select {
		case <-rows.closed:
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
				value, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(lane), 1<<20)
				if copyErr != nil {
					return copyErr
				}
				row[index] = value
			}
			out = append(out, row)
		}
		select {
		case rows.incoming <- out:
		case <-rows.closed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-rows.connDone:
			return driver.ErrBadConn
		}
	}
}

func (rows *queryRows) Close() error {
	rows.closeOnce.Do(func() { close(rows.closed) })
	<-rows.done
	if rows.cancel != nil {
		rows.cancel()
		rows.cancel = nil
	}
	return nil
}

func (rows *queryRows) Next(dest []driver.Value) error {
	for rows.pos >= len(rows.current) {
		select {
		case batch, ok := <-rows.incoming:
			if !ok {
				<-rows.done
				if rows.err != nil {
					return rows.err
				}
				return io.EOF
			}
			rows.current = batch
			rows.pos = 0
		case <-rows.connDone:
			if rows.cancel != nil {
				rows.cancel()
			}
			<-rows.done
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
			return err
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
	case 18, 19, 25, 705, 1042, 1043:
		return value.StrVal, nil // char, name, text, unknown, bpchar, varchar
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
	rendered, err := codec.Output(context.Background(), []model.Value{value}, builtin.CallBudget{MemoryBytes: 4 << 20, OutputBytes: 4 << 20})
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
