package pgwire_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/pgwire"
)

type testClock struct {
	mu    sync.Mutex
	now   clock.Instant
	scale clock.ScaleDescriptor
	epoch clock.Epoch
}

func newTestClock() *testClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identifier := sha256.Sum256(history)
	return &testClock{
		now:   1000,
		scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identifier, CanonicalLeapHistory: history},
		epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: identifier},
	}
}

func (c *testClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += 1000
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, nil
}

func (c *testClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if value < 0 || value == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.now = max(c.now, (value/1000+1)*1000)
	return nil
}

func (c *testClock) Scale() clock.ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

func (c *testClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}

func (c *testClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(int64(value), 0).UTC()}, nil
}

type testServer struct {
	connector *supersql.Connector
	server    *pgwire.Server
	addr      string
	serveErr  chan error
}

func startTestServer(t *testing.T, cfg pgwire.Config) *testServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "ssql-pgwire-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	connector, err := supersql.OpenConnector(t.Context(), supersql.OpenOptions{
		Directory:   dir,
		Clock:       newTestClock(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}

	srv, err := pgwire.NewServer(connector, cfg)
	if err != nil {
		_ = connector.Close()
		t.Fatalf("NewServer: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = connector.Close()
		t.Fatalf("net.Listen: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(context.Background(), ln)
	}()

	ts := &testServer{
		connector: connector,
		server:    srv,
		addr:      ln.Addr().String(),
		serveErr:  serveErr,
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Server.Close: %v", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Server.Serve returned error: %v", err)
		}
		if err := connector.Close(); err != nil {
			t.Errorf("Connector.Close: %v", err)
		}
	})
	return ts
}

func (ts *testServer) connString(extra string) string {
	base := fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", ts.addr)
	if extra != "" {
		return base + "&" + extra
	}
	return base
}

func generateSelfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// --- Raw wire protocol test helpers ---

type rawMsg struct {
	typ  byte
	body []byte
}

func writeRawMsg(t *testing.T, w io.Writer, typ byte, body []byte) {
	t.Helper()
	buf := make([]byte, 5+len(body))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(body)))
	copy(buf[5:], body)
	if _, err := w.Write(buf); err != nil {
		t.Fatalf("writeRawMsg(%q): %v", typ, err)
	}
}

func writeStartup(t *testing.T, w io.Writer, params map[string]string) {
	t.Helper()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, int32(196608))
	for k, v := range params {
		body.WriteString(k)
		body.WriteByte(0)
		body.WriteString(v)
		body.WriteByte(0)
	}
	body.WriteByte(0)
	pkt := make([]byte, 4+body.Len())
	binary.BigEndian.PutUint32(pkt[0:4], uint32(len(pkt)))
	copy(pkt[4:], body.Bytes())
	if _, err := w.Write(pkt); err != nil {
		t.Fatalf("writeStartup: %v", err)
	}
}

func readRawMsg(t *testing.T, r *bufio.Reader) rawMsg {
	t.Helper()
	typ, err := r.ReadByte()
	if err != nil {
		t.Fatalf("readRawMsg type: %v", err)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		t.Fatalf("readRawMsg len: %v", err)
	}
	msgLen := int(binary.BigEndian.Uint32(lenBuf[:]))
	body := make([]byte, msgLen-4)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("readRawMsg body: %v", err)
	}
	return rawMsg{typ: typ, body: body}
}

func readUntilReady(t *testing.T, r *bufio.Reader) ([]rawMsg, byte) {
	t.Helper()
	var msgs []rawMsg
	for {
		m := readRawMsg(t, r)
		msgs = append(msgs, m)
		if m.typ == 'Z' {
			return msgs, m.body[0]
		}
	}
}

// --- Tests ---

func TestPgxSimpleAndExtendedTypes(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	conn, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Verify server metadata reported during startup.
	if v := conn.PgConn().ParameterStatus("server_version"); v != "18.4 (SuperSQL)" {
		t.Fatalf("server_version = %q, want 18.4 (SuperSQL)", v)
	}
	if ts.server.ActiveConnections() != 1 {
		t.Fatalf("ActiveConnections = %d, want 1", ts.server.ActiveConnections())
	}

	_, err = conn.Exec(ctx, `
		CREATE TABLE all_types (
			id          bigint PRIMARY KEY,
			s_i2        smallint,
			s_i4        integer,
			s_i8        bigint,
			s_f4        real,
			s_f8        double precision,
			s_bool      boolean,
			s_text      text,
			s_bytea     bytea,
			s_uuid      uuid,
			s_date      date,
			s_ts        timestamp,
			s_tstz      timestamptz,
			s_num       numeric(12, 4),
			s_jsonb     jsonb,
			s_i4arr     integer[],
			s_txtarr    text[],
			s_nullable  text
		)
	`)
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	wantUUID := [16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0x4d, 0xef, 0x80, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77}
	wantDate := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	wantTS := time.Date(2026, 6, 15, 14, 30, 45, 123456000, time.UTC)
	wantTSTZ := time.Date(2026, 6, 15, 14, 30, 45, 654321000, time.UTC)
	wantBytes := []byte{0x00, 0x01, 0xfe, 0xff, 'a', 'b'}
	wantJSON := map[string]any{"k": "v", "n": float64(7)}
	wantI4Arr := []int32{10, 20, 30}
	wantTxtArr := []string{"alpha", "beta,gamma", `quote"here`}

	var num pgtype.Numeric
	if err := num.Scan("12345.6789"); err != nil {
		t.Fatalf("num.Scan: %v", err)
	}

	// Test Extended Query (default CacheStatement mode -> Parse + Describe + Bind + Execute with binary params & results).
	var returnedID int64
	err = conn.QueryRow(ctx, `
		INSERT INTO all_types (
			id, s_i2, s_i4, s_i8, s_f4, s_f8, s_bool, s_text, s_bytea,
			s_uuid, s_date, s_ts, s_tstz, s_num, s_jsonb, s_i4arr, s_txtarr, s_nullable
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9,
			$10, $11, $12, $13, $14, $15, $16, $17, $18
		) RETURNING id
	`,
		int64(1), int16(42), int32(-1000), int64(9000000000),
		float32(3.5), float64(6.25), true, "hello supersql", wantBytes,
		pgtype.UUID{Bytes: wantUUID, Valid: true},
		pgtype.Date{Time: wantDate, Valid: true},
		pgtype.Timestamp{Time: wantTS, Valid: true},
		wantTSTZ, num, wantJSON, wantI4Arr, wantTxtArr, nil,
	).Scan(&returnedID)
	if err != nil {
		t.Fatalf("INSERT RETURNING: %v", err)
	}
	if returnedID != 1 {
		t.Fatalf("returnedID = %d, want 1", returnedID)
	}

	// Read back in BOTH CacheStatement (binary results) AND SimpleProtocol (text results) AND Exec mode.
	for _, mode := range []pgx.QueryExecMode{
		pgx.QueryExecModeCacheStatement,
		pgx.QueryExecModeExec,
		pgx.QueryExecModeSimpleProtocol,
	} {
		var (
			id       int64
			i2       int16
			i4       int32
			i8       int64
			f4       float32
			f8       float64
			b        bool
			txt      string
			rawBytes []byte
			gotUUID  pgtype.UUID
			gotDate  pgtype.Date
			gotTS    pgtype.Timestamp
			gotTSTZ  time.Time
			gotNum   pgtype.Numeric
			gotJSON  map[string]any
			gotI4Arr []int32
			gotTArr  []string
			gotNull  *string
		)
		err := conn.QueryRow(ctx, `
			SELECT id, s_i2, s_i4, s_i8, s_f4, s_f8, s_bool, s_text, s_bytea,
			       s_uuid, s_date, s_ts, s_tstz, s_num, s_jsonb, s_i4arr, s_txtarr, s_nullable
			FROM all_types WHERE id = $1
		`, mode, int64(1)).Scan(
			&id, &i2, &i4, &i8, &f4, &f8, &b, &txt, &rawBytes,
			&gotUUID, &gotDate, &gotTS, &gotTSTZ, &gotNum, &gotJSON, &gotI4Arr, &gotTArr, &gotNull,
		)
		if err != nil {
			t.Fatalf("SELECT (mode=%v): %v", mode, err)
		}
		if id != 1 || i2 != 42 || i4 != -1000 || i8 != 9000000000 {
			t.Fatalf("mode=%v ints mismatch: %d %d %d %d", mode, id, i2, i4, i8)
		}
		if f4 != 3.5 || f8 != 6.25 || !b || txt != "hello supersql" {
			t.Fatalf("mode=%v scalars mismatch: %v %v %v %q", mode, f4, f8, b, txt)
		}
		if !bytes.Equal(rawBytes, wantBytes) {
			t.Fatalf("mode=%v bytea = %x, want %x", mode, rawBytes, wantBytes)
		}
		if !gotUUID.Valid || gotUUID.Bytes != wantUUID {
			t.Fatalf("mode=%v uuid = %+v, want %x", mode, gotUUID, wantUUID)
		}
		if !gotDate.Valid || !gotDate.Time.UTC().Equal(wantDate) {
			t.Fatalf("mode=%v date = %+v, want %v", mode, gotDate, wantDate)
		}
		if !gotTS.Valid || !gotTS.Time.UTC().Equal(wantTS) {
			t.Fatalf("mode=%v ts = %+v, want %v", mode, gotTS, wantTS)
		}
		if !gotTSTZ.UTC().Equal(wantTSTZ) {
			t.Fatalf("mode=%v tstz = %v, want %v", mode, gotTSTZ.UTC(), wantTSTZ)
		}
		gotNumVal, _ := gotNum.Float64Value()
		if !gotNumVal.Valid || math.Abs(gotNumVal.Float64-12345.6789) > 1e-6 {
			t.Fatalf("mode=%v numeric = %+v", mode, gotNum)
		}
		if gotJSON["k"] != "v" || gotJSON["n"] != float64(7) {
			t.Fatalf("mode=%v jsonb = %#v", mode, gotJSON)
		}
		if !slices.Equal(gotI4Arr, wantI4Arr) {
			t.Fatalf("mode=%v i4arr = %v, want %v", mode, gotI4Arr, wantI4Arr)
		}
		if !slices.Equal(gotTArr, wantTxtArr) {
			t.Fatalf("mode=%v txtarr = %v, want %v", mode, gotTArr, wantTxtArr)
		}
		if gotNull != nil {
			t.Fatalf("mode=%v nullable = %q, want nil", mode, *gotNull)
		}
	}
}

func TestPgxTransactionsSavepointsAndErrorRecovery(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	conn, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, `CREATE TABLE accounts (id int PRIMARY KEY, balance int NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// 1. BEGIN + INSERT + COMMIT.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if conn.PgConn().TxStatus() != 'T' {
		t.Fatalf("TxStatus after BEGIN = %q, want 'T'", conn.PgConn().TxStatus())
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts VALUES (1, 100)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if conn.PgConn().TxStatus() != 'I' {
		t.Fatalf("TxStatus after COMMIT = %q, want 'I'", conn.PgConn().TxStatus())
	}

	// 2. BEGIN + SAVEPOINT + error ('E') + ROLLBACK TO SAVEPOINT ('T') + COMMIT ('I').
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 2: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts VALUES (2, 200)`); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	if _, err := tx.Exec(ctx, `SAVEPOINT sp1`); err != nil {
		t.Fatalf("SAVEPOINT: %v", err)
	}
	// Trigger unique constraint violation (23505) -> enters failed tx state 'E'.
	_, err = tx.Exec(ctx, `INSERT INTO accounts VALUES (1, 999)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "23505" {
		t.Fatalf("SQLSTATE = %q, want 23505", pgErr.Code)
	}
	if conn.PgConn().TxStatus() != 'E' {
		t.Fatalf("TxStatus after failed statement = %q, want 'E'", conn.PgConn().TxStatus())
	}

	// Subsequent statement while in 'E' must fail with 25P02.
	_, err = tx.Exec(ctx, `SELECT 1`)
	if !errors.As(err, &pgErr) || pgErr.Code != "25P02" {
		t.Fatalf("expected 25P02 in failed tx, got %v", err)
	}
	if conn.PgConn().TxStatus() != 'E' {
		t.Fatalf("TxStatus still = %q, want 'E'", conn.PgConn().TxStatus())
	}

	// ROLLBACK TO SAVEPOINT recovers to 'T'!
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT sp1`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT: %v", err)
	}
	if conn.PgConn().TxStatus() != 'T' {
		t.Fatalf("TxStatus after ROLLBACK TO SAVEPOINT = %q, want 'T'", conn.PgConn().TxStatus())
	}
	if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT sp1`); err != nil {
		t.Fatalf("RELEASE SAVEPOINT: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	// 3. Explicit ROLLBACK discards uncommitted writes.
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 3: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO accounts VALUES (3, 300)`); err != nil {
		t.Fatalf("INSERT 3: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if conn.PgConn().TxStatus() != 'I' {
		t.Fatalf("TxStatus after ROLLBACK = %q, want 'I'", conn.PgConn().TxStatus())
	}

	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&count); err != nil {
		t.Fatalf("SELECT count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	// 4. Failed tx recovered by full ROLLBACK.
	tx, err = conn.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin 4: %v", err)
	}
	_, err = tx.Exec(ctx, `SELECT 1 / 0`)
	if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
		t.Fatalf("expected 22012 division by zero, got %v", err)
	}
	if conn.PgConn().TxStatus() != 'E' {
		t.Fatalf("TxStatus = %q, want 'E'", conn.PgConn().TxStatus())
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("Rollback after error: %v", err)
	}
	if conn.PgConn().TxStatus() != 'I' {
		t.Fatalf("TxStatus after ROLLBACK = %q, want 'I'", conn.PgConn().TxStatus())
	}
}

func TestPgxSQLStatePropagation(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	conn, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	cases := []struct {
		name     string
		sql      string
		wantCode string
	}{
		{"undefined_table", "SELECT * FROM no_such_table_xyz", "42P01"},
		{"syntax_error", "SELECT FROM WHERE", "42601"},
		{"division_by_zero", "SELECT 1 / 0", "22012"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := conn.Exec(ctx, tc.sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("expected *pgconn.PgError, got %T: %v", err, err)
			}
			if pgErr.Code != tc.wantCode {
				t.Fatalf("SQLSTATE = %q, want %q (msg=%q)", pgErr.Code, tc.wantCode, pgErr.Message)
			}
		})
	}
}

func TestAuthenticationModes(t *testing.T) {
	ctx := t.Context()

	t.Run("CleartextPassword", func(t *testing.T) {
		ts := startTestServer(t, pgwire.Config{Password: "s3cr3t!"})

		// Wrong password -> 28P01.
		_, err := pgx.Connect(ctx, fmt.Sprintf("postgres://alice:wrong@%s/mydb?sslmode=disable", ts.addr))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("expected 28P01 on wrong password, got %v", err)
		}

		// Correct password -> succeeds.
		conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://alice:s3cr3t!@%s/mydb?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("expected auth success, got %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var n int
		if err := conn.QueryRow(ctx, "SELECT 7").Scan(&n); err != nil || n != 7 {
			t.Fatalf("SELECT 7 = (%d, %v)", n, err)
		}
	})

	t.Run("CustomAuthFunc", func(t *testing.T) {
		ts := startTestServer(t, pgwire.Config{
			AuthFunc: func(_ context.Context, user, database, password string) error {
				if user == "admin" && database == "prod" && password == "token42" {
					return nil
				}
				return errors.New("denied")
			},
		})

		_, err := pgx.Connect(ctx, fmt.Sprintf("postgres://admin:bad@%s/prod?sslmode=disable", ts.addr))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("expected 28P01 from AuthFunc reject, got %v", err)
		}

		conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://admin:token42@%s/prod?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("expected AuthFunc accept, got %v", err)
		}
		_ = conn.Close(ctx)
	})
}

func TestCancelRequest(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	holder, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("pgx.Connect holder: %v", err)
	}
	defer func() { _ = holder.Close(ctx) }()

	waiter, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("pgx.Connect waiter: %v", err)
	}
	defer func() { _ = waiter.Close(ctx) }()

	if _, err := holder.Exec(ctx, `CREATE TABLE lock_target (id int PRIMARY KEY, v int)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := holder.Exec(ctx, `INSERT INTO lock_target VALUES (1, 0)`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Holder acquires an exclusive row lock in an older transaction so waiter blocks.
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE lock_target SET v = 1 WHERE id = 1`); err != nil {
		t.Fatalf("holder UPDATE: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(ctx, `UPDATE lock_target SET v = 2 WHERE id = 1`)
		errCh <- err
	}()

	// Give the waiter query a brief moment to block on the lock manager.
	time.Sleep(150 * time.Millisecond)

	if err := waiter.PgConn().CancelRequest(ctx); err != nil {
		t.Fatalf("CancelRequest: %v", err)
	}

	select {
	case err := <-errCh:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected *pgconn.PgError after cancel, got %T: %v", err, err)
		}
		if pgErr.Code != "57014" {
			t.Fatalf("SQLSTATE = %q, want 57014 (msg=%q)", pgErr.Code, pgErr.Message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled query to return")
	}

	// Verify connection remains usable after cancellation.
	var ok int
	if err := waiter.QueryRow(ctx, "SELECT 123").Scan(&ok); err != nil || ok != 123 {
		t.Fatalf("post-cancel query = (%d, %v), want (123, nil)", ok, err)
	}
}

func TestTLSUpgrade(t *testing.T) {
	ctx := t.Context()
	tlsCfg := generateSelfSignedTLS(t)
	ts := startTestServer(t, pgwire.Config{TLSConfig: tlsCfg})

	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=require", ts.addr))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.TLSConfig = &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("TLS pgx.ConnectConfig: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var v string
	if err := conn.QueryRow(ctx, "SELECT 'encrypted'").Scan(&v); err != nil || v != "encrypted" {
		t.Fatalf("TLS query = (%q, %v)", v, err)
	}
}

func TestConcurrentPgxPool(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{MaxConnections: 32})

	poolCfg, err := pgxpool.ParseConfig(ts.connString(""))
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	poolCfg.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, worker int, label text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const workers = 4
	const perWorker = 10
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	isSerializationErr := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
	}

	for w := range workers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := range perWorker {
				id := int64(workerID*1000 + i)
				wantLabel := fmt.Sprintf("w%d-%d", workerID, i)
				for attempt := 0; ; attempt++ {
					tx, err := pool.Begin(ctx)
					if err != nil {
						errCh <- fmt.Errorf("worker %d begin: %w", workerID, err)
						return
					}
					if _, err := tx.Exec(ctx, `INSERT INTO items (id, worker, label) VALUES ($1, $2, $3)`,
						id, workerID, wantLabel); err != nil {
						_ = tx.Rollback(ctx)
						if isSerializationErr(err) && attempt < 20 {
							continue
						}
						errCh <- fmt.Errorf("worker %d insert %d: %w", workerID, id, err)
						return
					}
					if err := tx.Commit(ctx); err != nil {
						if isSerializationErr(err) && attempt < 20 {
							continue
						}
						errCh <- fmt.Errorf("worker %d commit %d: %w", workerID, id, err)
						return
					}
					break
				}
				var gotLabel string
				for attempt := 0; ; attempt++ {
					err := pool.QueryRow(ctx, `SELECT label FROM items WHERE id = $1`, id).Scan(&gotLabel)
					if err == nil {
						break
					}
					if isSerializationErr(err) && attempt < 20 {
						continue
					}
					errCh <- fmt.Errorf("worker %d select %d: %w", workerID, id, err)
					return
				}
				if gotLabel != wantLabel {
					errCh <- fmt.Errorf("worker %d label = %q, want %q", workerID, gotLabel, wantLabel)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent worker error: %v", err)
	}

	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != workers*perWorker {
		t.Fatalf("total rows = %d, want %d", total, workers*perWorker)
	}
}

func TestRawProtocolMultiStatementPortalSuspendedAndNotify(t *testing.T) {
	var notifyPID atomic.Int32
	notifyReady := make(chan pgwire.NotifySender, 1)
	ts := startTestServer(t, pgwire.Config{
		NotifyHandler: func(_ context.Context, pid int32, send pgwire.NotifySender) func() {
			notifyPID.Store(pid)
			select {
			case notifyReady <- send:
			default:
			}
			return nil
		},
	})

	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	r := bufio.NewReader(rawConn)

	// 1. SSLRequest when TLSConfig == nil -> must receive 'N'.
	sslPkt := make([]byte, 8)
	binary.BigEndian.PutUint32(sslPkt[0:4], 8)
	binary.BigEndian.PutUint32(sslPkt[4:8], 80877103)
	if _, err := rawConn.Write(sslPkt); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	sslReply, err := r.ReadByte()
	if err != nil || sslReply != 'N' {
		t.Fatalf("SSLRequest reply = (%q, %v), want 'N'", sslReply, err)
	}

	// 2. StartupMessage -> AuthOk + ParameterStatus + BackendKeyData + ReadyForQuery('I').
	writeStartup(t, rawConn, map[string]string{"user": "tester", "database": "testdb"})
	msgs, status := readUntilReady(t, r)
	if status != 'I' {
		t.Fatalf("initial ReadyForQuery status = %q, want 'I'", status)
	}
	var sawAuth, sawKey bool
	for _, m := range msgs {
		if m.typ == 'R' && binary.BigEndian.Uint32(m.body) == 0 {
			sawAuth = true
		}
		if m.typ == 'K' && len(m.body) == 8 {
			sawKey = true
		}
	}
	if !sawAuth || !sawKey {
		t.Fatalf("missing startup frames: sawAuth=%v sawKey=%v", sawAuth, sawKey)
	}

	// 3. Empty Simple Query -> EmptyQueryResponse ('I') + ReadyForQuery ('Z').
	writeRawMsg(t, rawConn, 'Q', []byte("   \x00"))
	msgs, status = readUntilReady(t, r)
	if status != 'I' || len(msgs) != 2 || msgs[0].typ != 'I' {
		t.Fatalf("empty query responses = %+v, status=%q", msgs, status)
	}

	// 4. Multi-statement Simple Query.
	writeRawMsg(t, rawConn, 'Q', []byte("CREATE TABLE nums (n int); INSERT INTO nums VALUES (1),(2),(3),(4),(5); SELECT n FROM nums ORDER BY n;\x00"))
	msgs, status = readUntilReady(t, r)
	if status != 'I' {
		t.Fatalf("multi-stmt status = %q, want 'I'", status)
	}
	var cmdTags []string
	var dataRows int
	for _, m := range msgs {
		switch m.typ {
		case 'C':
			cmdTags = append(cmdTags, string(bytes.TrimSuffix(m.body, []byte{0})))
		case 'D':
			dataRows++
		case 'E':
			t.Fatalf("unexpected error in multi-stmt: %q", string(m.body))
		}
	}
	if dataRows != 5 {
		t.Fatalf("dataRows = %d, want 5", dataRows)
	}
	if !slices.Equal(cmdTags, []string{"CREATE TABLE", "INSERT 0 5", "SELECT 5"}) {
		t.Fatalf("cmdTags = %v, want [CREATE TABLE, INSERT 0 5, SELECT 5]", cmdTags)
	}

	// 5. Extended Query with maxRows=2 -> PortalSuspended ('s'), PortalSuspended ('s'), CommandComplete ('C').
	var parseBody bytes.Buffer
	parseBody.WriteString("s_nums\x00SELECT n FROM nums ORDER BY n\x00")
	_ = binary.Write(&parseBody, binary.BigEndian, int16(0))
	writeRawMsg(t, rawConn, 'P', parseBody.Bytes())

	// Describe Statement 'S'
	writeRawMsg(t, rawConn, 'D', []byte("Ss_nums\x00"))

	// Bind named portal p_nums
	var bindBody bytes.Buffer
	bindBody.WriteString("p_nums\x00s_nums\x00")
	_ = binary.Write(&bindBody, binary.BigEndian, int16(0)) // param formats
	_ = binary.Write(&bindBody, binary.BigEndian, int16(0)) // params
	_ = binary.Write(&bindBody, binary.BigEndian, int16(0)) // result formats (text)
	writeRawMsg(t, rawConn, 'B', bindBody.Bytes())

	// Describe Portal 'P'
	writeRawMsg(t, rawConn, 'D', []byte("Pp_nums\x00"))

	// Execute maxRows=2
	execMsg := func(maxRows int32) []byte {
		var b bytes.Buffer
		b.WriteString("p_nums\x00")
		_ = binary.Write(&b, binary.BigEndian, maxRows)
		return b.Bytes()
	}
	writeRawMsg(t, rawConn, 'E', execMsg(2))
	writeRawMsg(t, rawConn, 'H', nil) // Flush

	// Read ParseComplete, ParameterDescription, RowDescription, BindComplete, RowDescription, 2xDataRow, PortalSuspended.
	expectTypes := []byte{'1', 't', 'T', '2', 'T', 'D', 'D', 's'}
	for i, want := range expectTypes {
		got := readRawMsg(t, r)
		if got.typ != want {
			t.Fatalf("batch1 msg[%d] = %q, want %q (body=%q)", i, got.typ, want, string(got.body))
		}
	}

	// Second Execute maxRows=2 -> 2xDataRow + PortalSuspended ('s').
	writeRawMsg(t, rawConn, 'E', execMsg(2))
	writeRawMsg(t, rawConn, 'H', nil)
	for i, want := range []byte{'D', 'D', 's'} {
		got := readRawMsg(t, r)
		if got.typ != want {
			t.Fatalf("batch2 msg[%d] = %q, want %q", i, got.typ, want)
		}
	}

	// Third Execute maxRows=2 -> 1xDataRow + CommandComplete ("SELECT 5").
	writeRawMsg(t, rawConn, 'E', execMsg(2))
	writeRawMsg(t, rawConn, 'C', []byte("Pp_nums\x00"))
	writeRawMsg(t, rawConn, 'C', []byte("Ss_nums\x00"))
	writeRawMsg(t, rawConn, 'S', nil)
	msgs, status = readUntilReady(t, r)
	if status != 'I' {
		t.Fatalf("final status = %q, want 'I'", status)
	}
	gotTypes := make([]byte, len(msgs))
	for i, m := range msgs {
		gotTypes[i] = m.typ
	}
	if !slices.Equal(gotTypes, []byte{'D', 'C', '3', '3', 'Z'}) {
		t.Fatalf("batch3 msg types = %q, want \"DC33Z\"", string(gotTypes))
	}

	// 6. Extended query error recovery: error skips until Sync.
	var badParse bytes.Buffer
	badParse.WriteString("\x00SELECT * FROM does_not_exist\x00")
	_ = binary.Write(&badParse, binary.BigEndian, int16(0))
	writeRawMsg(t, rawConn, 'P', badParse.Bytes())
	writeRawMsg(t, rawConn, 'D', []byte("S\x00"))
	// This Bind must be skipped because Describe errored.
	writeRawMsg(t, rawConn, 'B', []byte("\x00\x00\x00\x00\x00\x00\x00\x00"))
	writeRawMsg(t, rawConn, 'S', nil)
	msgs, status = readUntilReady(t, r)
	if status != 'I' {
		t.Fatalf("error recovery status = %q, want 'I'", status)
	}
	gotTypes = make([]byte, len(msgs))
	for i, m := range msgs {
		gotTypes[i] = m.typ
	}
	// ParseComplete ('1'), ErrorResponse ('E') on Describe, Bind skipped, ReadyForQuery ('Z').
	if !slices.Equal(gotTypes, []byte{'1', 'E', 'Z'}) {
		t.Fatalf("error recovery types = %q, want \"1EZ\"", string(gotTypes))
	}

	// 7. Asynchronous NotificationResponse ('A') via NotifyHandler & Server.Notify.
	sendFn := <-notifyReady
	if err := sendFn(pgwire.Notification{PID: 777, Channel: "orders", Payload: "created:42"}); err != nil {
		t.Fatalf("sendFn: %v", err)
	}
	notif := readRawMsg(t, r)
	if notif.typ != 'A' {
		t.Fatalf("expected 'A' notification frame, got %q", notif.typ)
	}
	if pid := int32(binary.BigEndian.Uint32(notif.body[0:4])); pid != 777 {
		t.Fatalf("notif pid = %d, want 777", pid)
	}
	if !bytes.Equal(notif.body[4:], []byte("orders\x00created:42\x00")) {
		t.Fatalf("notif payload = %q", string(notif.body[4:]))
	}

	if err := ts.server.Notify(notifyPID.Load(), "alerts", "ping"); err != nil {
		t.Fatalf("Server.Notify: %v", err)
	}
	notif2 := readRawMsg(t, r)
	if notif2.typ != 'A' || !bytes.Contains(notif2.body[4:], []byte("alerts\x00ping\x00")) {
		t.Fatalf("unexpected Server.Notify frame: %q %q", notif2.typ, string(notif2.body))
	}

	// 8. Clean Terminate ('X').
	writeRawMsg(t, rawConn, 'X', nil)
}

func TestMaxConnectionsLimit(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{MaxConnections: 2})

	c1, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("connect 1: %v", err)
	}
	defer func() { _ = c1.Close(ctx) }()

	c2, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("connect 2: %v", err)
	}
	defer func() { _ = c2.Close(ctx) }()

	if ts.server.ActiveConnections() != 2 {
		t.Fatalf("ActiveConnections = %d, want 2", ts.server.ActiveConnections())
	}

	// Third connection must be rejected with 53300 too_many_connections.
	_, err = pgx.Connect(ctx, ts.connString(""))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "53300" {
		t.Fatalf("expected 53300 too_many_connections, got %v", err)
	}
}

func TestPgxWaitForNotificationAndListenAndServe(t *testing.T) {
	ctx := t.Context()
	dir, err := os.MkdirTemp("", "ssql-pgwire-ls-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
		Directory:   dir,
		Clock:       newTestClock(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	defer func() { _ = connector.Close() }()

	srv, err := pgwire.NewServer(connector, pgwire.Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	serveCtx, stopServe := context.WithCancel(ctx)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.ListenAndServe(serveCtx, "127.0.0.1:0")
	}()

	var addr net.Addr
	for range 100 {
		addr = srv.Addr()
		if addr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == nil {
		stopServe()
		t.Fatal("Server.Addr() never became non-nil")
	}

	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", addr.String()))
	if err != nil {
		stopServe()
		t.Fatalf("pgx.Connect: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		srv.BroadcastNotify(9001, "events", "hello-pgx")
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWait()
	notif, err := conn.WaitForNotification(waitCtx)
	if err != nil {
		_ = conn.Close(ctx)
		stopServe()
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notif.PID != 9001 || notif.Channel != "events" || notif.Payload != "hello-pgx" {
		t.Fatalf("unexpected notification: %+v", notif)
	}

	_ = conn.Close(ctx)
	stopServe()
	if err := <-doneCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("ListenAndServe returned unexpected error: %v", err)
	}
}

func TestPgwireSQLListenNotifyEndToEnd(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	connA, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("connect A: %v", err)
	}
	defer func() { _ = connA.Close(ctx) }()

	connB, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("connect B: %v", err)
	}
	defer func() { _ = connB.Close(ctx) }()

	if ct, err := connA.Exec(ctx, `LISTEN events`); err != nil {
		t.Fatalf("LISTEN events: %v", err)
	} else if ct.String() != "LISTEN" {
		t.Fatalf("LISTEN tag = %q, want LISTEN", ct.String())
	}

	// Verify pg_listening_channels() over the wire.
	var chName string
	if err := connA.QueryRow(ctx, `SELECT * FROM pg_listening_channels()`).Scan(&chName); err != nil {
		t.Fatalf("pg_listening_channels: %v", err)
	}
	if chName != "events" {
		t.Fatalf("pg_listening_channels = %q, want events", chName)
	}

	// Send rolled-back NOTIFY (must NOT arrive), committed NOTIFY, and pg_notify from Conn B.
	errCh := make(chan error, 1)
	go func() {
		time.Sleep(40 * time.Millisecond)
		txRoll, err := connB.Begin(ctx)
		if err != nil {
			errCh <- err
			return
		}
		if _, err := txRoll.Exec(ctx, `NOTIFY events, 'never-delivered'`); err != nil {
			errCh <- err
			return
		}
		if err := txRoll.Rollback(ctx); err != nil {
			errCh <- err
			return
		}

		txCommit, err := connB.Begin(ctx)
		if err != nil {
			errCh <- err
			return
		}
		if _, err := txCommit.Exec(ctx, `NOTIFY events, 'hello-tx'`); err != nil {
			errCh <- err
			return
		}
		if err := txCommit.Commit(ctx); err != nil {
			errCh <- err
			return
		}

		if _, err := connB.Exec(ctx, `SELECT pg_notify('events', 'hello-fn')`); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	n1, err := connA.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification 1: %v", err)
	}
	if n1.Channel != "events" || n1.Payload != "hello-tx" {
		t.Fatalf("n1 = %+v, want events/hello-tx", n1)
	}

	n2, err := connA.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("WaitForNotification 2: %v", err)
	}
	if n2.Channel != "events" || n2.Payload != "hello-fn" {
		t.Fatalf("n2 = %+v, want events/hello-fn", n2)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("sender goroutine error: %v", err)
	}
}

func TestPreAuthShutdownAndRepeatedSSLReject(t *testing.T) {
	dir, err := os.MkdirTemp("", "ssql-pgwire-preauth-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	connector, err := supersql.OpenConnector(t.Context(), supersql.OpenOptions{
		Directory:   dir,
		Clock:       newTestClock(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	defer func() { _ = connector.Close() }()

	srv, err := pgwire.NewServer(connector, pgwire.Config{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(context.Background(), ln) }()

	addr := ln.Addr().String()

	// 1. Repeated SSLRequest loop must be rejected by closing the connection.
	sslConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial sslConn: %v", err)
	}
	sslPkt := make([]byte, 8)
	binary.BigEndian.PutUint32(sslPkt[0:4], 8)
	binary.BigEndian.PutUint32(sslPkt[4:8], 80877103)
	if _, err := sslConn.Write(sslPkt); err != nil {
		t.Fatalf("write SSLRequest 1: %v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(sslConn, reply[:]); err != nil || reply[0] != 'N' {
		t.Fatalf("SSLRequest 1 reply = (%q, %v), want 'N'", reply[0], err)
	}
	// Second SSLRequest must cause server to drop the connection (EOF on read).
	_, _ = sslConn.Write(sslPkt)
	_ = sslConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sslConn.Read(reply[:]); err == nil {
		t.Fatal("expected connection close on repeated SSLRequest, got data")
	}
	_ = sslConn.Close()

	// 2. Idle pre-auth TCP client must NOT hang Server.Close().
	idleConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("Dial idleConn: %v", err)
	}
	defer func() { _ = idleConn.Close() }()
	time.Sleep(50 * time.Millisecond)

	closeDone := make(chan error, 1)
	go func() { closeDone <- srv.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Server.Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Server.Close hung with idle pre-auth TCP client connected")
	}
	<-serveDone
}

func TestDescribeTrailingLineComment(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{})

	conn, err := pgx.Connect(ctx, ts.connString(""))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var answer int
	err = conn.QueryRow(ctx, "SELECT 42 AS answer -- trailing comment").Scan(&answer)
	if err != nil {
		t.Fatalf("QueryRow with trailing -- comment: %v", err)
	}
	if answer != 42 {
		t.Fatalf("answer = %d, want 42", answer)
	}
}

func TestAuthenticatedNonSuperuserRoleBinding(t *testing.T) {
	ctx := t.Context()
	ts := startTestServer(t, pgwire.Config{
		AuthFunc: func(_ context.Context, user, database, password string) error {
			return nil
		},
	})

	admin, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("admin Connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, `CREATE TABLE secret_tbl (id int PRIMARY KEY, secret text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO secret_tbl VALUES (1, 'top-secret')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE ROLE lowpriv`); err != nil {
		t.Fatalf("CREATE ROLE: %v", err)
	}

	// Connect as non-superuser SQL role lowpriv.
	low, err := pgx.Connect(ctx, fmt.Sprintf("postgres://lowpriv:pw@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("lowpriv Connect: %v", err)
	}
	defer func() { _ = low.Close(ctx) }()

	if got := low.PgConn().ParameterStatus("is_superuser"); got != "off" {
		t.Fatalf("is_superuser = %q, want off", got)
	}

	// Ungranted table read must fail with 42501 insufficient_privilege.
	var s string
	err = low.QueryRow(ctx, `SELECT secret FROM secret_tbl WHERE id = 1`).Scan(&s)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501 permission denied for lowpriv, got %v", err)
	}

	// RESET ROLE must NOT escalate lowpriv back to superuser because session_user is bound.
	if _, err := low.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE: %v", err)
	}
	err = low.QueryRow(ctx, `SELECT secret FROM secret_tbl WHERE id = 1`).Scan(&s)
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501 after RESET ROLE, got %v", err)
	}

	// DO $$ BEGIN RESET ROLE; END $$ must NOT escalate lowpriv to superuser.
	if _, err := low.Exec(ctx, `DO $$ BEGIN RESET ROLE; END $$;`); err != nil {
		t.Fatalf("DO RESET ROLE: %v", err)
	}
	var curUser, sessUser string
	if err := low.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&curUser, &sessUser); err != nil {
		t.Fatalf("SELECT current_user, session_user: %v", err)
	}
	if curUser != "lowpriv" || sessUser != "lowpriv" {
		t.Fatalf("got current_user=%q session_user=%q, want lowpriv/lowpriv", curUser, sessUser)
	}
	if _, err := low.Exec(ctx, `CREATE ROLE pwn SUPERUSER`); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501 on CREATE ROLE SUPERUSER after DO RESET ROLE, got %v", err)
	}

	// Legitimate SET ROLE to a granted group role must work cleanly.
	if _, err := admin.Exec(ctx, `CREATE ROLE readers`); err != nil {
		t.Fatalf("CREATE ROLE readers: %v", err)
	}
	if _, err := admin.Exec(ctx, `GRANT readers TO lowpriv`); err != nil {
		t.Fatalf("GRANT readers TO lowpriv: %v", err)
	}
	if _, err := admin.Exec(ctx, `GRANT SELECT ON secret_tbl TO readers`); err != nil {
		t.Fatalf("GRANT SELECT TO readers: %v", err)
	}
	if _, err := low.Exec(ctx, `SET ROLE readers`); err != nil {
		t.Fatalf("SET ROLE readers: %v", err)
	}
	if err := low.QueryRow(ctx, `SELECT secret FROM secret_tbl WHERE id = 1`).Scan(&s); err != nil || s != "top-secret" {
		t.Fatalf("readers role read = (%q, %v), want (top-secret, nil)", s, err)
	}
	if _, err := low.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatalf("RESET ROLE back to lowpriv: %v", err)
	}

	// After direct GRANT SELECT, lowpriv can also read directly.
	if _, err := admin.Exec(ctx, `GRANT SELECT ON secret_tbl TO lowpriv`); err != nil {
		t.Fatalf("GRANT SELECT: %v", err)
	}
	if err := low.QueryRow(ctx, `SELECT secret FROM secret_tbl WHERE id = 1`).Scan(&s); err != nil || s != "top-secret" {
		t.Fatalf("granted read = (%q, %v), want (top-secret, nil)", s, err)
	}
}
