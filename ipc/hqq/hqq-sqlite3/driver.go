package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gosuda.org/hqq"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/sqlite3/internal/dsn"
	"gosuda.org/sqlite3/internal/wire"
)

// DriverName is the name the driver is registered under with database/sql.
const DriverName = "sqlite3"

// Compile-time checks of the database/sql interfaces the driver implements.
var (
	_ driver.Driver        = (*Driver)(nil)
	_ driver.DriverContext = (*Driver)(nil)
	_ driver.Connector     = (*Connector)(nil)
	_ io.Closer            = (*Connector)(nil)
)

func init() {
	sql.Register(DriverName, &Driver{})
}

// Driver is the database/sql driver "sqlite3". It is registered by the
// package's init function.
type Driver struct{}

// Open connects to the service named by dsn and returns one connection.
// database/sql never calls Open: it uses OpenConnector.
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	c, err := ConnectorFromDSN(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector parses dsn and returns a Connector for it. database/sql uses it
// for sql.Open.
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return ConnectorFromDSN(dsn)
}

// ConnectorFromDSN parses dsn and returns a Connector for it. Both explicit
// HQQ service URIs (hqq://<listener>?db=...) and standard SQLite file or
// :memory: DSNs (such as "app.db?_journal=WAL", "file:app.db?_fk=1", or
// ":memory:") are supported; file and :memory: DSNs automatically verify and
// start the embedded hqq-sqlite-server worker on first connection.
func ConnectorFromDSN(s string) (*Connector, error) {
	if isExplicitHQQDSN(s) {
		listener, cfg, err := ParseDSN(s)
		if err != nil {
			return nil, err
		}
		return NewConnector(listener, cfg)
	}
	o, err := dsn.ParseFile(s)
	if err != nil {
		return nil, err
	}
	cfg := configFromOptions(o)
	var allowDir, cleanupDir string
	if o.Database == ":memory:" {
		tmp, err := os.MkdirTemp("", "hqq-sqlite-mem-*")
		if err != nil {
			return nil, fmt.Errorf("sqlite: create temp directory for :memory: database: %w", err)
		}
		allowDir = tmp
		cleanupDir = tmp
		cfg.Database = "memory.db"
	} else {
		abs, err := filepath.Abs(o.Database)
		if err != nil {
			return nil, fmt.Errorf("sqlite: resolve database path %q: %w", o.Database, err)
		}
		allowDir = filepath.Dir(abs)
		cfg.Database = filepath.Base(abs)
	}
	cfg, err = cfg.normalized()
	if err != nil {
		if cleanupDir != "" {
			_ = os.RemoveAll(cleanupDir)
		}
		return nil, err
	}
	return &Connector{
		cfg:          cfg,
		autoAllowDir: allowDir,
		cleanupDir:   cleanupDir,
	}, nil
}

func isExplicitHQQDSN(s string) bool {
	rawPath, _, _ := strings.Cut(s, "?")
	if strings.HasPrefix(strings.ToLower(rawPath), "hqq:") {
		return true
	}
	if idx := strings.Index(rawPath, "://"); idx >= 0 {
		return !strings.EqualFold(rawPath[:idx], "file")
	}
	return false
}

// Mode is the open mode of the database.
type Mode uint8

// Open modes. The zero value is ModeReadWriteCreate.
const (
	ModeReadWriteCreate Mode = iota // mode=rwc: open, creating the database if needed
	ModeReadWrite                   // mode=rw: open an existing database for writing
	ModeReadOnly                    // mode=ro: open read-only
)

// TxMode is the BEGIN mode of a transaction.
type TxMode = wire.TxMode

// Transaction modes. TxDeferred is the zero value.
const (
	TxDeferred  = wire.TxDeferred
	TxImmediate = wire.TxImmediate
	TxExclusive = wire.TxExclusive
)

// Pragma is a PRAGMA the driver applies when a session opens.
type Pragma struct {
	Name  string
	Value string
}

// Config configures a Connector. The zero value is valid once Database is set.
type Config struct {
	// Database is the database alias or path the server's open hook resolves,
	// required.
	Database string

	// Mode is the open mode. Default: ModeReadWriteCreate.
	Mode Mode

	// Timeout bounds connecting (the bootstrap, the handshake, and opening
	// the database) and the requests that run outside a caller context
	// (Commit, Rollback, closing cursors and statements, closing the
	// session). Default: 10s.
	Timeout time.Duration

	// FetchSize is the number of rows requested per batch. Default: 256, at
	// most wire.MaxFetchRows (4096). The server may return fewer rows to stay
	// within the size bound of one batch.
	FetchSize int

	// BusyTimeout is how long SQLite waits for a lock held by another
	// session. Zero selects 5s; a negative value disables the wait.
	BusyTimeout time.Duration

	// TxMode is the BEGIN mode of transactions. A context value set with
	// WithTxMode overrides it per transaction. Default: TxDeferred.
	TxMode TxMode

	// Location is the time zone used when formatting time.Time parameters
	// and when parsing DATE, DATETIME, or TIMESTAMP column values that carry
	// no explicit UTC offset. When nil (the default), parameters keep their
	// own zone offset and offset-free column values parse in UTC.
	Location *time.Location

	// Pragmas are applied by the driver when a session opens, in order.
	Pragmas []Pragma

	// Session configures the session of the generated client. Zero fields
	// select the driver's values. CallTimeout is not set by the driver:
	// context deadlines are honored by interrupting and closing the session;
	// a CallTimeout set here applies to every call in addition.
	Session rt.Options

	// Link configures the HQQ link when Session.Link is not set.
	Link hqq.Config
}

// Defaults of Config.
const (
	defaultTimeout     = 10 * time.Second
	defaultBusyTimeout = 5 * time.Second
	defaultFetchSize   = wire.DefaultFetchRows
)

// normalized returns cfg with the defaults applied and its values checked.
func (cfg Config) normalized() (Config, error) {
	if cfg.Database == "" || len(cfg.Database) > wire.MaxPathBytes {
		return cfg, fmt.Errorf("sqlite: database must be 1..%d bytes", wire.MaxPathBytes)
	}
	switch cfg.Mode {
	case ModeReadWriteCreate, ModeReadWrite, ModeReadOnly:
	default:
		return cfg, fmt.Errorf("sqlite: invalid mode %d", cfg.Mode)
	}
	switch cfg.TxMode {
	case TxDeferred, TxImmediate, TxExclusive:
	default:
		return cfg, fmt.Errorf("sqlite: invalid transaction mode %d", cfg.TxMode)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.FetchSize == 0 {
		cfg.FetchSize = defaultFetchSize
	}
	if cfg.FetchSize < 1 || cfg.FetchSize > wire.MaxFetchRows {
		return cfg, fmt.Errorf("sqlite: fetch size must be 1..%d", wire.MaxFetchRows)
	}
	if cfg.BusyTimeout == 0 {
		cfg.BusyTimeout = defaultBusyTimeout
	}
	for _, p := range cfg.Pragmas {
		if !wire.ValidPragmaName(p.Name) || !wire.ValidPragmaValue(p.Value) {
			return cfg, fmt.Errorf("sqlite: invalid pragma %q=%q", p.Name, p.Value)
		}
	}
	return cfg, nil
}

// ParseDSN parses a DSN of the form
//
//	hqq://<listener>?db=...&mode=...&timeout=...&fetch=...&busy=...&txlock=...&pragma.<name>=<value>
//
// (plus standard underscore-prefixed PRAGMA and driver control knobs; see
// docs/DSN.md) and returns the listener name and the Config. Unknown or
// duplicate options are errors.
func ParseDSN(s string) (string, Config, error) {
	o, err := dsn.Parse(s)
	if err != nil {
		return "", Config{}, err
	}
	return o.Listener, configFromOptions(o), nil
}

func configFromOptions(o dsn.Options) Config {
	cfg := Config{
		Database:    o.Database,
		Mode:        Mode(o.Mode),
		Timeout:     o.Timeout,
		FetchSize:   o.Fetch,
		BusyTimeout: o.Busy,
		TxMode:      TxMode(o.TxLock),
		Location:    o.Location,
	}
	for _, p := range o.Pragmas {
		cfg.Pragmas = append(cfg.Pragmas, Pragma{Name: p.Name, Value: p.Value})
	}
	return cfg
}

// Connector connects to one service. Every connection is its own session of
// the generated client, with its own shared-memory link. Connector implements
// io.Closer; sql.DB calls Close when the DB is closed.
type Connector struct {
	listener     string
	cfg          Config
	autoAllowDir string
	cleanupDir   string
	autoRelease  func() error

	mu     sync.Mutex
	closed bool
}

var errConnectorClosed = errors.New("sqlite: connector closed")

// NewConnector returns a Connector for the listener name and configuration.
func NewConnector(listener string, cfg Config) (*Connector, error) {
	if listener == "" {
		return nil, errors.New("sqlite: listener name is empty")
	}
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	return &Connector{listener: listener, cfg: cfg}, nil
}

// Driver returns the driver of the connector.
func (c *Connector) Driver() driver.Driver { return &Driver{} }

// Close marks the connector closed: further Connect calls fail. Connections
// already open stay usable until database/sql closes them. When the connector
// auto-started an embedded server, Close releases its reference and stops the
// server once the last reference closes.
func (c *Connector) Close() error {
	c.mu.Lock()
	c.closed = true
	rel := c.autoRelease
	c.autoRelease = nil
	cleanDir := c.cleanupDir
	c.cleanupDir = ""
	c.mu.Unlock()

	var err error
	if rel != nil {
		err = rel()
	}
	if cleanDir != "" {
		_ = os.RemoveAll(cleanDir)
	}
	return err
}

func (c *Connector) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Connector) ensureListener(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", errConnectorClosed
	}
	if c.listener != "" {
		return c.listener, nil
	}
	if c.autoAllowDir == "" {
		return "", errors.New("sqlite: listener name is empty")
	}
	listener, rel, err := acquireAutoServer(ctx, c.autoAllowDir)
	if err != nil {
		return "", err
	}
	c.listener = listener
	c.autoRelease = rel
	return listener, nil
}

type txModeKey struct{}

// WithTxMode returns a context whose transactions begin in mode, overriding
// Config.TxMode. The context is the one passed to BeginTx.
func WithTxMode(ctx context.Context, mode TxMode) context.Context {
	return context.WithValue(ctx, txModeKey{}, mode)
}

func txModeFrom(ctx context.Context, def TxMode) TxMode {
	if m, ok := ctx.Value(txModeKey{}).(TxMode); ok {
		return m
	}
	return def
}
