package pgwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type phase3Clock struct {
	mu    sync.Mutex
	now   clock.Instant
	scale clock.ScaleDescriptor
	epoch clock.Epoch
}

func newPhase3Clock() *phase3Clock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identifier := sha256.Sum256(history)
	return &phase3Clock{
		now:   1000,
		scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identifier, CanonicalLeapHistory: history},
		epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: identifier},
	}
}

func (c *phase3Clock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += 1000
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, nil
}

func (c *phase3Clock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
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

func (c *phase3Clock) Scale() clock.ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

func (c *phase3Clock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}

func (c *phase3Clock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(int64(value), 0).UTC()}, nil
}

type phase3Server struct {
	connector *supersql.Connector
	server    *Server
	addr      string
}

func startPhase3Server(t *testing.T, cfg Config) *phase3Server {
	t.Helper()
	dir, err := os.MkdirTemp("", "ssql-pgwire-p3-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	connector, err := supersql.OpenConnector(t.Context(), supersql.OpenOptions{
		Directory:   dir,
		Clock:       newPhase3Clock(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}

	srv, err := NewServer(connector, cfg)
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
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Server.Close: %v", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Server.Serve: %v", err)
		}
		if err := connector.Close(); err != nil {
			t.Errorf("Connector.Close: %v", err)
		}
	})
	return &phase3Server{
		connector: connector,
		server:    srv,
		addr:      ln.Addr().String(),
	}
}

func writeP3Startup(t *testing.T, w io.Writer, params map[string]string) {
	t.Helper()
	var body bytes.Buffer
	_ = binary.Write(&body, binary.BigEndian, protocolVersion30)
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
		t.Fatalf("writeP3Startup: %v", err)
	}
}

func writeP3Msg(t *testing.T, w io.Writer, typ byte, body []byte) {
	t.Helper()
	buf := make([]byte, 5+len(body))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(body)))
	copy(buf[5:], body)
	if _, err := w.Write(buf); err != nil {
		t.Fatalf("writeP3Msg(%q): %v", typ, err)
	}
}

func readP3Msg(t *testing.T, r *bufio.Reader) (byte, []byte) {
	t.Helper()
	typ, err := r.ReadByte()
	if err != nil {
		t.Fatalf("readP3Msg type: %v", err)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		t.Fatalf("readP3Msg len: %v", err)
	}
	msgLen := int(binary.BigEndian.Uint32(lenBuf[:]))
	body := make([]byte, msgLen-4)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("readP3Msg body: %v", err)
	}
	return typ, body
}

func parseWireErrorFields(body []byte) map[byte]string {
	fields := make(map[byte]string)
	pos := 0
	for pos < len(body) && body[pos] != 0 {
		code := body[pos]
		val, next, ok := readCString(body, pos+1)
		if !ok {
			break
		}
		fields[code] = val
		pos = next
	}
	return fields
}

// --- P0 #9: Fail-closed role auth & rolcanlogin ---

func TestPhase3FailClosedRoleAuthAndCanLogin(t *testing.T) {
	ctx := t.Context()
	ts := startPhase3Server(t, Config{
		AuthFunc: func(_ context.Context, user, database, password string) error {
			return nil
		},
	})

	// 1. Non-existent role must be rejected fail-closed with 28000 (never superuser!).
	_, err := pgx.Connect(ctx, fmt.Sprintf("postgres://nonexistent_user:pw@%s/postgres?sslmode=disable", ts.addr))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
		t.Fatalf("expected 28000 for nonexistent role, got %v", err)
	}

	// Built-in superuser aliases connect as superuser.
	for _, builtin := range []string{"postgres", "supersql", "embedded"} {
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:pw@%s/postgres?sslmode=disable", builtin, ts.addr))
		if err != nil {
			t.Fatalf("connect built-in %q: %v", builtin, err)
		}
		if got := c.PgConn().ParameterStatus("is_superuser"); got != "on" {
			_ = c.Close(ctx)
			t.Fatalf("built-in %q is_superuser = %q, want on", builtin, got)
		}
		_ = c.Close(ctx)
	}

	admin, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	if _, err := admin.Exec(ctx, `CREATE TABLE vault (id int PRIMARY KEY, secret text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO vault VALUES (1, 'classified')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 2. CREATE ROLE nologin_u defaults to NOLOGIN -> rejected with 28000.
	if _, err := admin.Exec(ctx, `CREATE ROLE nologin_u`); err != nil {
		t.Fatalf("CREATE ROLE nologin_u: %v", err)
	}
	var canLogin bool
	if err := admin.QueryRow(ctx, `SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname = 'nologin_u'`).Scan(&canLogin); err != nil {
		t.Fatalf("query pg_roles nologin_u: %v", err)
	}
	if canLogin {
		t.Fatal("CREATE ROLE nologin_u had rolcanlogin = true, want false")
	}
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://nologin_u:pw@%s/postgres?sslmode=disable", ts.addr))
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
		t.Fatalf("expected 28000 for NOLOGIN role, got %v", err)
	}

	// 3. CREATE ROLE login_u LOGIN and CREATE USER u2 both persist CanLogin = true and connect as non-superuser.
	if _, err := admin.Exec(ctx, `CREATE ROLE login_u LOGIN`); err != nil {
		t.Fatalf("CREATE ROLE login_u LOGIN: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE USER u2`); err != nil {
		t.Fatalf("CREATE USER u2: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE USER u_denied NOLOGIN`); err != nil {
		t.Fatalf("CREATE USER u_denied NOLOGIN: %v", err)
	}

	// Verify pg_roles and pg_user reflect CanLogin accurately.
	for _, tc := range []struct {
		role string
		want bool
	}{
		{"nologin_u", false},
		{"login_u", true},
		{"u2", true},
		{"u_denied", false},
	} {
		var got bool
		if err := admin.QueryRow(ctx, `SELECT rolcanlogin FROM pg_catalog.pg_roles WHERE rolname = $1`, tc.role).Scan(&got); err != nil {
			t.Fatalf("pg_roles(%s): %v", tc.role, err)
		}
		if got != tc.want {
			t.Fatalf("pg_roles(%s).rolcanlogin = %v, want %v", tc.role, got, tc.want)
		}
		var cnt int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_user WHERE usename = $1`, tc.role).Scan(&cnt); err != nil {
			t.Fatalf("pg_user(%s): %v", tc.role, err)
		}
		wantCnt := 0
		if tc.want {
			wantCnt = 1
		}
		if cnt != wantCnt {
			t.Fatalf("pg_user(%s) count = %d, want %d", tc.role, cnt, wantCnt)
		}
	}

	// u_denied (CREATE USER ... NOLOGIN) must be rejected with 28000.
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://u_denied:pw@%s/postgres?sslmode=disable", ts.addr))
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
		t.Fatalf("expected 28000 for CREATE USER ... NOLOGIN, got %v", err)
	}

	// Both login_u and u2 connect as non-superusers and cannot read ungranted vault.
	for _, role := range []string{"login_u", "u2"} {
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:pw@%s/postgres?sslmode=disable", role, ts.addr))
		if err != nil {
			t.Fatalf("connect %q: %v", role, err)
		}
		if got := c.PgConn().ParameterStatus("is_superuser"); got != "off" {
			_ = c.Close(ctx)
			t.Fatalf("%s is_superuser = %q, want off", role, got)
		}
		var curUser, sessUser string
		if err := c.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&curUser, &sessUser); err != nil {
			_ = c.Close(ctx)
			t.Fatalf("%s current_user/session_user: %v", role, err)
		}
		if curUser != role || sessUser != role {
			_ = c.Close(ctx)
			t.Fatalf("%s got current_user=%q session_user=%q", role, curUser, sessUser)
		}
		var secret string
		err = c.QueryRow(ctx, `SELECT secret FROM vault WHERE id = 1`).Scan(&secret)
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			_ = c.Close(ctx)
			t.Fatalf("%s expected 42501 on vault, got %v", role, err)
		}
		_ = c.Close(ctx)
	}

	// 4. ALTER ROLE login_u NOLOGIN subsequently rejects login.
	if _, err := admin.Exec(ctx, `ALTER ROLE login_u NOLOGIN`); err != nil {
		t.Fatalf("ALTER ROLE login_u NOLOGIN: %v", err)
	}
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://login_u:pw@%s/postgres?sslmode=disable", ts.addr))
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
		t.Fatalf("expected 28000 after ALTER ROLE login_u NOLOGIN, got %v", err)
	}
	// ALTER ROLE login_u LOGIN re-enables login.
	if _, err := admin.Exec(ctx, `ALTER ROLE login_u LOGIN`); err != nil {
		t.Fatalf("ALTER ROLE login_u LOGIN: %v", err)
	}
	if c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://login_u:pw@%s/postgres?sslmode=disable", ts.addr)); err != nil {
		t.Fatalf("expected login_u reconnect after ALTER ROLE LOGIN, got %v", err)
	} else {
		_ = c.Close(ctx)
	}

	// 5. Case-folded role name ("Alice" / "ALICE") binds to alice as non-superuser (never fails open).
	if _, err := admin.Exec(ctx, `CREATE USER alice`); err != nil {
		t.Fatalf("CREATE USER alice: %v", err)
	}
	for _, variant := range []string{"Alice", "ALICE", "aLiCe"} {
		c, err := pgx.Connect(ctx, fmt.Sprintf("postgres://%s:pw@%s/postgres?sslmode=disable", variant, ts.addr))
		if err != nil {
			t.Fatalf("connect %q: %v", variant, err)
		}
		if got := c.PgConn().ParameterStatus("is_superuser"); got != "off" {
			_ = c.Close(ctx)
			t.Fatalf("%q is_superuser = %q, want off (failed open!)", variant, got)
		}
		var curUser, sessUser string
		if err := c.QueryRow(ctx, `SELECT current_user, session_user`).Scan(&curUser, &sessUser); err != nil {
			_ = c.Close(ctx)
			t.Fatalf("%q current_user/session_user: %v", variant, err)
		}
		if curUser != "alice" || sessUser != "alice" {
			_ = c.Close(ctx)
			t.Fatalf("%q got current_user=%q session_user=%q, want alice/alice", variant, curUser, sessUser)
		}
		var secret string
		err = c.QueryRow(ctx, `SELECT secret FROM vault WHERE id = 1`).Scan(&secret)
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			_ = c.Close(ctx)
			t.Fatalf("%q expected 42501 on vault, got %v", variant, err)
		}
		_ = c.Close(ctx)
	}
}

type failingQueryDriverConn struct {
	queryErr error
}

func (f *failingQueryDriverConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (f *failingQueryDriverConn) Close() error                        { return nil }
func (f *failingQueryDriverConn) Begin() (driver.Tx, error)           { return nil, errors.New("unused") }
func (f *failingQueryDriverConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, f.queryErr
}

func TestPhase3BindAuthenticatedRoleFailClosedOnQueryError(t *testing.T) {
	c := &clientConn{
		driverConn:  &failingQueryDriverConn{queryErr: errors.New("catalog unavailable")},
		user:        "alice",
		isSuperuser: false,
	}
	err := c.bindAuthenticatedRole(t.Context())
	if err == nil {
		t.Fatal("expected error when pg_roles lookup fails, got nil")
	}
	var we *wireError
	if !errors.As(err, &we) || we.Code != "28000" || we.Severity != "FATAL" {
		t.Fatalf("expected FATAL 28000 wireError, got %v", err)
	}
	if c.isSuperuser {
		t.Fatal("c.isSuperuser became true after failed role lookup")
	}
}

// --- P0 #16: Incremental frame reading & $N parameter cap ---

func TestPhase3IncrementalFrameReadNoEager64MiBAlloc(t *testing.T) {
	// 5-byte fake 64 MiB frame header followed immediately by EOF.
	fakeHeader := []byte{'Q', 0x04, 0x00, 0x00, 0x00} // length = 67108864 (64 MiB)

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	_, _, err := readFrontendMessage(bufio.NewReader(bytes.NewReader(fakeHeader)))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
	}

	runtime.ReadMemStats(&after)
	allocDelta := after.TotalAlloc - before.TotalAlloc
	if allocDelta > 1024*1024 {
		t.Fatalf("fake 64 MiB frame header allocated %d bytes (> 1 MiB), eager allocation not fixed!", allocDelta)
	}

	// End-to-end TCP server test: send fake 64 MiB frame header and close, then verify server stays healthy.
	ts := startPhase3Server(t, Config{ReadTimeout: 2 * time.Second})
	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := bufio.NewReader(rawConn)
	writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
	for {
		typ, _ := readP3Msg(t, r)
		if typ == 'Z' {
			break
		}
	}
	if _, err := rawConn.Write(fakeHeader); err != nil {
		t.Fatalf("write fakeHeader: %v", err)
	}
	_ = rawConn.Close()

	// Verify server stays healthy for a normal client.
	conn, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("healthy client Connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var n int
	if err := conn.QueryRow(t.Context(), "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("SELECT 1 = (%d, %v)", n, err)
	}
}

func TestPhase3PasswordMessageLengthCap(t *testing.T) {
	ts := startPhase3Server(t, Config{Password: "secret", ReadTimeout: 2 * time.Second})
	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	r := bufio.NewReader(rawConn)

	writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
	typ, body := readP3Msg(t, r)
	if typ != 'R' || binary.BigEndian.Uint32(body) != uint32(authTypeSASL) {
		t.Fatalf("expected SASL auth challenge, got %q %x", typ, body)
	}

	// Send PasswordMessage claiming maxPasswordMessageBytes + 1 bytes (header only).
	var hdr [5]byte
	hdr[0] = 'p'
	binary.BigEndian.PutUint32(hdr[1:5], uint32(maxPasswordMessageBytes+1))
	if _, err := rawConn.Write(hdr[:]); err != nil {
		t.Fatalf("write oversized password hdr: %v", err)
	}
	_ = rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, body = readP3Msg(t, r)
	if typ != 'E' {
		t.Fatalf("expected ErrorResponse 'E' for oversized PasswordMessage, got %q", typ)
	}
	fields := parseWireErrorFields(body)
	if fields['C'] != "08P01" || fields['S'] != "FATAL" {
		t.Fatalf("expected FATAL 08P01, got %+v", fields)
	}
}

func TestPhase3WireParamCapRejectsHugeParamWithoutOOM(t *testing.T) {
	ts := startPhase3Server(t, Config{})
	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	r := bufio.NewReader(rawConn)

	writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
	for {
		typ, _ := readP3Msg(t, r)
		if typ == 'Z' {
			break
		}
	}

	for _, query := range []string{"SELECT $2147483647", "SELECT $65536"} {
		var parseBody bytes.Buffer
		parseBody.WriteString("\x00")
		parseBody.WriteString(query)
		parseBody.WriteByte(0)
		_ = binary.Write(&parseBody, binary.BigEndian, int16(0))
		writeP3Msg(t, rawConn, 'P', parseBody.Bytes())
		writeP3Msg(t, rawConn, 'D', []byte("S\x00"))
		writeP3Msg(t, rawConn, 'S', nil)

		var sawErrCode string
		for {
			typ, body := readP3Msg(t, r)
			if typ == 'E' {
				sawErrCode = parseWireErrorFields(body)['C']
			}
			if typ == 'Z' {
				break
			}
		}
		if sawErrCode != "54000" {
			t.Fatalf("%q: expected wire error SQLSTATE 54000, got %q", query, sawErrCode)
		}
	}

	// Verify resolveParamOIDs itself rejects > maxWireParams directly without allocating.
	c := &clientConn{}
	if _, err := c.resolveParamOIDs(t.Context(), &preparedStmt{
		analysis: paramAnalysis{maxParam: 2147483647},
	}); err == nil {
		t.Fatal("resolveParamOIDs allowed maxParam=2147483647")
	}

	// Connection remains usable after Sync.
	writeP3Msg(t, rawConn, 'Q', []byte("SELECT 1\x00"))
	var gotData bool
	for {
		typ, _ := readP3Msg(t, r)
		if typ == 'D' {
			gotData = true
		}
		if typ == 'Z' {
			break
		}
	}
	if !gotData {
		t.Fatal("connection failed to execute SELECT 1 after parameter cap error")
	}
}

// --- P0 #17: Iterative walkProtoMessage & recover() panic boundary ---

func TestPhase3DeeplyNestedQueryAndIterativeWalk(t *testing.T) {
	// 1. Unit test: synthetic AST deeper than maxProtoWalkDepth returns errProtoWalkDepth cleanly.
	deep := &zpgparse.Node{
		Node: &zpgparse.Node_AConst{
			AConst: &zpgparse.A_Const{
				Val: &zpgparse.A_Const_Ival{Ival: &zpgparse.Integer{Ival: 1}},
			},
		},
	}
	for range maxProtoWalkDepth + 100 {
		deep = &zpgparse.Node{
			Node: &zpgparse.Node_AExpr{
				AExpr: &zpgparse.A_Expr{Lexpr: deep},
			},
		}
	}
	err := walkProtoMessage(deep.ProtoReflect(), func(_ protoreflect.Message) bool { return true })
	if !errors.Is(err, errProtoWalkDepth) {
		t.Fatalf("expected errProtoWalkDepth for deep AST, got %v", err)
	}

	// 2. Unit test: wide AST exceeding maxProtoWalkNodes returns errProtoWalkNodes cleanly.
	items := make([]*zpgparse.Node, maxProtoWalkNodes+10)
	for i := range items {
		items[i] = &zpgparse.Node{
			Node: &zpgparse.Node_ParamRef{ParamRef: &zpgparse.ParamRef{Number: 1}},
		}
	}
	wide := &zpgparse.List{Items: items}
	err = walkProtoMessage(wide.ProtoReflect(), func(_ protoreflect.Message) bool { return true })
	if !errors.Is(err, errProtoWalkNodes) {
		t.Fatalf("expected errProtoWalkNodes for wide AST, got %v", err)
	}

	// 3. End-to-end test: deeply nested SQL (> 1024 levels) must not crash the server.
	ts := startPhase3Server(t, Config{})
	conn, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	deepParens := "SELECT " + strings.Repeat("(", 1200) + "1" + strings.Repeat(")", 1200)
	_, _ = conn.Exec(t.Context(), deepParens)

	deepNot := "SELECT " + strings.Repeat("NOT (", 1200) + "true" + strings.Repeat(")", 1200)
	_, err = conn.Exec(t.Context(), deepNot)
	if err == nil {
		t.Log("deepNot succeeded without crashing server")
	}

	// Server and connection must still work cleanly.
	var v int
	if err := conn.QueryRow(t.Context(), "SELECT 42").Scan(&v); err != nil || v != 42 {
		t.Fatalf("post-deep-query SELECT 42 = (%d, %v), want (42, nil)", v, err)
	}
}

type panickingDriverConn struct{}

func (p *panickingDriverConn) Prepare(string) (driver.Stmt, error) { panic("boom in prepare") }
func (p *panickingDriverConn) Close() error                        { return nil }
func (p *panickingDriverConn) Begin() (driver.Tx, error)           { panic("boom in begin") }
func (p *panickingDriverConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	panic("boom in query")
}
func (p *panickingDriverConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	panic("boom in exec")
}

func TestPhase3PanicRecoveryBoundary(t *testing.T) {
	ctx := t.Context()

	// 1. End-to-end server test: panic inside AuthFunc is recovered, client gets FATAL XX000,
	// and a second client continues to work normally.
	var loggedPanic sync.Mutex
	var panicLogs []string
	ts := startPhase3Server(t, Config{
		AuthFunc: func(_ context.Context, user, database, password string) error {
			if user == "panicker" {
				panic("simulated auth handler panic")
			}
			return nil
		},
		Logger: func(format string, args ...any) {
			loggedPanic.Lock()
			panicLogs = append(panicLogs, fmt.Sprintf(format, args...))
			loggedPanic.Unlock()
		},
	})

	_, err := pgx.Connect(ctx, fmt.Sprintf("postgres://panicker:pw@%s/postgres?sslmode=disable", ts.addr))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "XX000" || pgErr.Severity != "FATAL" {
		t.Fatalf("expected FATAL XX000 on panicking connection, got %v", err)
	}
	if strings.Contains(pgErr.Message, "simulated auth handler panic") {
		t.Fatalf("panic detail leaked over wire: %q", pgErr.Message)
	}

	// Second client must connect and execute queries normally.
	conn2, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("second client Connect after panic: %v", err)
	}
	var got int
	if err := conn2.QueryRow(ctx, "SELECT 99").Scan(&got); err != nil || got != 99 {
		_ = conn2.Close(ctx)
		t.Fatalf("second client SELECT 99 = (%d, %v), want (99, nil)", got, err)
	}
	_ = conn2.Close(ctx)

	// 2. Direct commandLoop panic boundary test over net.Pipe: panic inside query execution
	// delivers FATAL XX000 and unwinds cleanly without crashing the goroutine.
	srvConn, cliConn := net.Pipe()
	defer func() {
		_ = srvConn.Close()
		_ = cliConn.Close()
	}()
	cc := &clientConn{
		server:     ts.server,
		rawConn:    srvConn,
		br:         bufio.NewReader(srvConn),
		bw:         bufio.NewWriter(srvConn),
		pid:        4242,
		driverConn: &panickingDriverConn{},
		txStatus:   txStatusIdle,
		stmts:      make(map[string]*preparedStmt),
		portals:    make(map[string]*portal),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cc.commandLoop(ctx)
		cc.closeResources()
	}()

	writeP3Msg(t, cliConn, 'Q', []byte("SELECT 1\x00"))
	_ = cliConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, body := readP3Msg(t, bufio.NewReader(cliConn))
	if typ != 'E' {
		t.Fatalf("expected ErrorResponse 'E' on commandLoop panic, got %q", typ)
	}
	fields := parseWireErrorFields(body)
	if fields['S'] != "FATAL" || fields['C'] != "XX000" {
		t.Fatalf("expected FATAL XX000 from commandLoop panic, got %+v", fields)
	}
	if strings.Contains(fields['M'], "boom in query") {
		t.Fatalf("internal panic text leaked over wire: %q", fields['M'])
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("commandLoop did not exit after panic recovery")
	}

	loggedPanic.Lock()
	logCount := len(panicLogs)
	loggedPanic.Unlock()
	if logCount == 0 {
		t.Fatal("expected recovered panic to be logged via Config.Logger")
	}
}
