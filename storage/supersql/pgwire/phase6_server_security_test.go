package pgwire

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gosuda/supersql-alpha"
)

// --- P1-N5: Whitespace usernames & pg_roles-first authentication ---

func TestPhase6WhitespaceUsernameRejectedAndPgRolesFirst(t *testing.T) {
	ctx := t.Context()

	t.Run("WhitespaceUsernamesFailClosed", func(t *testing.T) {
		ts := startPhase3Server(t, Config{})

		for _, badUser := range []string{"   ", "\t  \n", " postgres ", " supersql "} {
			rawConn, err := net.Dial("tcp", ts.addr)
			if err != nil {
				t.Fatalf("Dial(%q): %v", badUser, err)
			}
			r := bufio.NewReader(rawConn)
			writeP3Startup(t, rawConn, map[string]string{"user": badUser, "database": "postgres"})
			_ = rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))

			var sawError map[byte]string
			for {
				typ, body := readP3Msg(t, r)
				if typ == 'E' {
					sawError = parseWireErrorFields(body)
					break
				}
				if typ == 'Z' {
					_ = rawConn.Close()
					t.Fatalf("user %q unexpectedly reached ReadyForQuery instead of FATAL 28000", badUser)
				}
			}
			_ = rawConn.Close()

			if sawError['S'] != "FATAL" || sawError['C'] != "28000" {
				t.Fatalf("user %q got error %+v, want FATAL 28000", badUser, sawError)
			}
		}

		// Direct unit check on bindAuthenticatedRole for defense in depth.
		c := &clientConn{user: "   "}
		err := c.bindAuthenticatedRole(ctx)
		var we *wireError
		if !errors.As(err, &we) || we.Severity != "FATAL" || we.Code != "28000" {
			t.Fatalf("bindAuthenticatedRole(\"   \") = %v, want FATAL 28000", err)
		}
		if c.isSuperuser {
			t.Fatal("whitespace-only user became superuser")
		}
	})

	t.Run("PgRolesCheckedFirstForBuiltinAliases", func(t *testing.T) {
		ts := startPhase3Server(t, Config{
			AuthFunc: func(_ context.Context, _, _, _ string) error {
				return nil
			},
		})

		// Initially (no postgres row in pg_roles), postgres connects as bootstrap superuser.
		admin, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("bootstrap admin connect: %v", err)
		}
		defer func() { _ = admin.Close(ctx) }()
		if got := admin.PgConn().ParameterStatus("is_superuser"); got != "on" {
			t.Fatalf("bootstrap postgres is_superuser = %q, want on", got)
		}

		if _, err := admin.Exec(ctx, `CREATE TABLE ops_secret (id int PRIMARY KEY, v text)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		if _, err := admin.Exec(ctx, `INSERT INTO ops_secret VALUES (1, 'restricted')`); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		// Operator creates explicit catalog role "postgres" with NOLOGIN -> login must fail with 28000!
		if _, err := admin.Exec(ctx, `CREATE ROLE postgres NOLOGIN`); err != nil {
			t.Fatalf("CREATE ROLE postgres NOLOGIN: %v", err)
		}
		_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
			t.Fatalf("expected 28000 for postgres with NOLOGIN in pg_roles, got %v", err)
		}

		// Operator alters "postgres" to LOGIN NOSUPERUSER -> connects as non-superuser!
		if _, err := admin.Exec(ctx, `ALTER ROLE postgres LOGIN NOSUPERUSER`); err != nil {
			t.Fatalf("ALTER ROLE postgres LOGIN NOSUPERUSER: %v", err)
		}
		demoted, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("connect demoted postgres: %v", err)
		}
		defer func() { _ = demoted.Close(ctx) }()
		if got := demoted.PgConn().ParameterStatus("is_superuser"); got != "off" {
			t.Fatalf("demoted postgres is_superuser = %q, want off", got)
		}
		var v string
		err = demoted.QueryRow(ctx, `SELECT v FROM ops_secret WHERE id = 1`).Scan(&v)
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("demoted postgres expected 42501 on ungranted table, got %v", err)
		}

		// Operator creates explicit catalog role "supersql" with NOLOGIN -> also rejected with 28000!
		if _, err := admin.Exec(ctx, `CREATE ROLE supersql NOLOGIN`); err != nil {
			t.Fatalf("CREATE ROLE supersql NOLOGIN: %v", err)
		}
		_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://supersql:pw@%s/postgres?sslmode=disable", ts.addr))
		if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
			t.Fatalf("expected 28000 for supersql with NOLOGIN in pg_roles, got %v", err)
		}
	})
}

// --- P1-N6: Server.Serve graceful drain/shutdown on ctx.Done() ---

func TestPhase6ServeGracefulDrainOnContextCancel(t *testing.T) {
	dir, err := os.MkdirTemp("", "ssql-pgwire-p6-drain-*")
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
	t.Cleanup(func() { _ = connector.Close() })

	// Disable both IdleTimeout and ReadTimeout so Serve would hang forever if
	// ctx cancellation did not actively wake and close tracked connections.
	srv, err := NewServer(connector, Config{
		IdleTimeout: -1,
		ReadTimeout: -1,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	serveCtx, cancelServe := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(serveCtx, ln)
	}()

	addr := ln.Addr().String()

	// 1. Authenticated idle pgx client (left open, idle in commandLoop).
	idlePgx, err := pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", addr))
	if err != nil {
		cancelServe()
		t.Fatalf("pgx.Connect: %v", err)
	}
	defer func() { _ = idlePgx.Close(context.Background()) }()

	// 2. Stalled pre-auth TCP client (connected, sent nothing).
	preAuthConn, err := net.Dial("tcp", addr)
	if err != nil {
		cancelServe()
		t.Fatalf("Dial preAuthConn: %v", err)
	}
	defer func() { _ = preAuthConn.Close() }()

	if srv.ActiveConnections() != 1 {
		cancelServe()
		t.Fatalf("ActiveConnections = %d, want 1", srv.ActiveConnections())
	}

	// Cancel ctx passed to Serve and verify prompt shutdown (< 1s).
	start := time.Now()
	cancelServe()
	select {
	case serveErr := <-serveDone:
		if !errors.Is(serveErr, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", serveErr)
		}
	case <-time.After(1 * time.Second):
		_ = srv.Close()
		t.Fatal("Server.Serve did not return within 1s after ctx cancellation with idle/pre-auth clients")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("Server.Serve drain took %v, want < 1s", elapsed)
	}
}

// --- P1-N7: Pre-auth total deadline, per-IP pre-auth limit, and WriteTimeout ---

func TestPhase6PreAuthTotalDeadlineNotStretchedBySSLOrGSS(t *testing.T) {
	const readTimeout = 300 * time.Millisecond
	ts := startPhase3Server(t, Config{
		Password:    "secret",
		ReadTimeout: readTimeout,
	})

	conn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	start := time.Now()

	// Wait ~120ms, send SSLRequest -> 'N'.
	time.Sleep(120 * time.Millisecond)
	sslPkt := make([]byte, 8)
	binary.BigEndian.PutUint32(sslPkt[0:4], 8)
	binary.BigEndian.PutUint32(sslPkt[4:8], uint32(sslRequestCode))
	if _, err := conn.Write(sslPkt); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 'N' {
		t.Fatalf("SSLRequest reply = (%q, %v), want 'N'", reply[0], err)
	}

	// Wait another ~120ms (total ~240ms), send GSSENCRequest -> 'N'.
	time.Sleep(120 * time.Millisecond)
	gssPkt := make([]byte, 8)
	binary.BigEndian.PutUint32(gssPkt[0:4], 8)
	binary.BigEndian.PutUint32(gssPkt[4:8], uint32(gssEncRequestCode))
	if _, err := conn.Write(gssPkt); err != nil {
		t.Fatalf("write GSSENCRequest: %v", err)
	}
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 'N' {
		t.Fatalf("GSSENCRequest reply = (%q, %v), want 'N'", reply[0], err)
	}

	// Now stall before StartupMessage. If GSSENCRequest had reset ReadTimeout
	// to +300ms, total lifetime would be ~540ms+. With a single pre-auth deadline,
	// server must close the socket right around ~300ms total.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := conn.Read(reply[:])
	elapsed := time.Since(start)
	if readErr == nil {
		t.Fatal("expected server to close stalled pre-auth connection at total preAuthDeadline")
	}
	if elapsed > readTimeout+200*time.Millisecond {
		t.Fatalf("pre-auth connection stayed open for %v (ReadTimeout=%v); SSL/GSS stretched deadline", elapsed, readTimeout)
	}
}

type fakeAddrConn struct {
	net.Conn
	raddr net.Addr
}

func (f *fakeAddrConn) RemoteAddr() net.Addr { return f.raddr }

func TestPhase6PerIPPreAuthLimit(t *testing.T) {
	t.Run("EndToEndTCP", func(t *testing.T) {
		ts := startPhase3Server(t, Config{
			Password:        "secret",
			MaxPreAuthPerIP: 2,
			ReadTimeout:     3 * time.Second,
		})

		dialAndWaitChallenge := func() net.Conn {
			c, err := net.Dial("tcp", ts.addr)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			r := bufio.NewReader(c)
			writeP3Startup(t, c, map[string]string{"user": "postgres", "database": "postgres"})
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			typ, _ := readP3Msg(t, r)
			if typ != 'R' {
				_ = c.Close()
				t.Fatalf("expected auth challenge 'R', got %q", typ)
			}
			return c
		}

		// First 2 pre-auth connections from 127.0.0.1 succeed.
		c1 := dialAndWaitChallenge()
		defer func() { _ = c1.Close() }()
		c2 := dialAndWaitChallenge()
		defer func() { _ = c2.Close() }()

		// 3rd concurrent pre-auth connection from 127.0.0.1 must be dropped immediately.
		c3, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatalf("Dial c3: %v", err)
		}
		defer func() { _ = c3.Close() }()
		_ = c3.SetReadDeadline(time.Now().Add(1 * time.Second))
		writeP3Startup(t, c3, map[string]string{"user": "postgres", "database": "postgres"})
		var buf [1]byte
		if _, err := c3.Read(buf[:]); err == nil {
			t.Fatal("3rd pre-auth connection from same IP unexpectedly received data instead of being rejected")
		}

		// Closing c1 frees a slot so a subsequent connection from 127.0.0.1 succeeds.
		_ = c1.Close()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ts.server.preAuthCount.Load() < 2 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		c4 := dialAndWaitChallenge()
		_ = c4.Close()
	})

	t.Run("MultiIPIsolationAndCleanup", func(t *testing.T) {
		srv, err := NewServer(&supersql.Connector{}, Config{MaxPreAuthPerIP: 2})
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		if srv.maxPreAuthPerIP != 2 {
			t.Fatalf("maxPreAuthPerIP = %d, want 2", srv.maxPreAuthPerIP)
		}

		makeConn := func(addr string) (*fakeAddrConn, func()) {
			a, b := net.Pipe()
			tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
			if err != nil {
				t.Fatalf("ResolveTCPAddr(%q): %v", addr, err)
			}
			return &fakeAddrConn{Conn: a, raddr: tcpAddr}, func() {
				_ = a.Close()
				_ = b.Close()
			}
		}

		c1, close1 := makeConn("10.0.0.1:1001")
		defer close1()
		c2, close2 := makeConn("10.0.0.1:1002")
		defer close2()
		c3, close3 := makeConn("10.0.0.1:1003")
		defer close3()
		cMapped, closeMapped := makeConn("[::ffff:10.0.0.1]:1004")
		defer closeMapped()
		cOther, closeOther := makeConn("10.0.0.2:2001")
		defer closeOther()

		cc1, _ := srv.acceptPreAuth(c1)
		cc2, _ := srv.acceptPreAuth(c2)
		if cc1 == nil || cc2 == nil {
			t.Fatal("first two pre-auth connections from 10.0.0.1 should be accepted")
		}
		if cc3, _ := srv.acceptPreAuth(c3); cc3 != nil {
			t.Fatal("3rd pre-auth connection from 10.0.0.1 should be rejected")
		}
		if ccMapped, _ := srv.acceptPreAuth(cMapped); ccMapped != nil {
			t.Fatal("IPv4-mapped IPv6 connection from ::ffff:10.0.0.1 should share 10.0.0.1 limit")
		}
		ccOther, _ := srv.acceptPreAuth(cOther)
		if ccOther == nil {
			t.Fatal("connection from distinct IP 10.0.0.2 should be accepted while 10.0.0.1 is capped")
		}

		// Promote cc1 via registerConn and unregister cc2/ccOther -> map must drain cleanly to 0.
		srv.registerConn(cc1)
		srv.unregisterPreAuth(cc2)
		srv.unregisterPreAuth(ccOther)
		srv.unregisterPreAuth(ccOther) // idempotent

		srv.mu.Lock()
		ipMapLen := len(srv.preAuthByIP)
		preMapLen := len(srv.preAuthConns)
		srv.mu.Unlock()
		if ipMapLen != 0 || preMapLen != 0 || srv.preAuthCount.Load() != 0 {
			t.Fatalf("leaked pre-auth state: byIP=%d preAuthConns=%d count=%d", ipMapLen, preMapLen, srv.preAuthCount.Load())
		}
	})
}

func TestPhase6WriteTimeoutUnblocksStalledReader(t *testing.T) {
	// Verify Config.WriteTimeout normalization.
	defSrv, err := NewServer(&supersql.Connector{}, Config{})
	if err != nil {
		t.Fatalf("NewServer default: %v", err)
	}
	if defSrv.writeTimeout != defaultWriteTimeout {
		t.Fatalf("default writeTimeout = %v, want %v", defSrv.writeTimeout, defaultWriteTimeout)
	}
	disabledSrv, err := NewServer(&supersql.Connector{}, Config{WriteTimeout: -1})
	if err != nil {
		t.Fatalf("NewServer disabled: %v", err)
	}
	if disabledSrv.writeTimeout != 0 {
		t.Fatalf("negative WriteTimeout = %v, want 0", disabledSrv.writeTimeout)
	}

	// Unread net.Pipe stalls writes immediately until WriteTimeout fires.
	srv, err := NewServer(&supersql.Connector{}, Config{WriteTimeout: 60 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewServer custom WriteTimeout: %v", err)
	}
	srvPipe, cliPipe := net.Pipe()
	defer func() {
		_ = srvPipe.Close()
		_ = cliPipe.Close()
	}()
	cc := srv.newClientConn(srvPipe, "127.0.0.1")

	start := time.Now()
	err = cc.writeAndFlush(encodeErrorResponse(wireError{
		Severity: "ERROR",
		Code:     "XX000",
		Message:  strings.Repeat("payload", 64),
	}))
	if err == nil {
		t.Fatal("expected writeAndFlush timeout error on stalled reader, got nil")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("writeAndFlush took %v, want ~60ms", elapsed)
	}

	// writeMu must be unlocked and c.closed must be true so subsequent writes fail immediately with net.ErrClosed.
	done := make(chan error, 1)
	go func() {
		done <- cc.writeAndFlush(encodeReadyForQuery(txStatusIdle))
	}()
	select {
	case err2 := <-done:
		if !errors.Is(err2, net.ErrClosed) {
			t.Fatalf("subsequent writeAndFlush after timeout = %v, want net.ErrClosed", err2)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("subsequent writeAndFlush hung on writeMu after WriteTimeout")
	}
}

// --- P1-N9: SCRAM iteration cap & dummySCRAMVerifier PBKDF2 timing parity ---

func TestPhase6SCRAMIterationCapAndDummyVerifierPBKDF2(t *testing.T) {
	if scramMaxIterations != 1<<20 {
		t.Fatalf("scramMaxIterations = %d, want %d", scramMaxIterations, 1<<20)
	}

	// Valid verifier at cap (1<<20) parses; above cap ((1<<20)+1) is rejected.
	salt := base64.StdEncoding.EncodeToString(make([]byte, scramSaltLen))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	atCap := fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramMaxIterations, salt, key, key)
	if v, isV, err := parseSCRAMVerifier(atCap); err != nil || !isV || v.iterations != scramMaxIterations {
		t.Fatalf("parseSCRAMVerifier(atCap) = (%+v, %v, %v), want valid verifier", v, isV, err)
	}
	overCap := fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", scramMaxIterations+1, salt, key, key)
	if _, isV, err := parseSCRAMVerifier(overCap); err == nil || !isV {
		t.Fatalf("parseSCRAMVerifier(overCap) = (isV=%v, err=%v), want error", isV, err)
	}

	// dummySCRAMVerifier runs PBKDF2 work with scramDefaultIterations and produces random salts/keys.
	v1, err := dummySCRAMVerifier()
	if err != nil {
		t.Fatalf("dummySCRAMVerifier 1: %v", err)
	}
	v2, err := dummySCRAMVerifier()
	if err != nil {
		t.Fatalf("dummySCRAMVerifier 2: %v", err)
	}
	if v1.iterations != scramDefaultIterations || len(v1.salt) != scramSaltLen || len(v1.storedKey) != 32 || len(v1.serverKey) != 32 {
		t.Fatalf("unexpected dummy verifier shape: %+v", v1)
	}
	if string(v1.salt) == string(v2.salt) || string(v1.storedKey) == string(v2.storedKey) {
		t.Fatal("dummySCRAMVerifier produced identical salt or storedKey across calls")
	}

	// Timing parity check: dummySCRAMVerifier should take measurable PBKDF2 time
	// comparable to deriveSCRAMVerifier (not sub-microsecond rand-only).
	const samples = 5
	startReal := time.Now()
	for range samples {
		if _, err := deriveSCRAMVerifier("benchmark-password"); err != nil {
			t.Fatalf("deriveSCRAMVerifier: %v", err)
		}
	}
	realDur := time.Since(startReal)

	startDummy := time.Now()
	for range samples {
		if _, err := dummySCRAMVerifier(); err != nil {
			t.Fatalf("dummySCRAMVerifier: %v", err)
		}
	}
	dummyDur := time.Since(startDummy)
	// Dummy work must be at least 20% of real PBKDF2 derivation (vs <1% without PBKDF2).
	if dummyDur*5 < realDur {
		t.Fatalf("dummySCRAMVerifier too fast compared to deriveSCRAMVerifier: dummy=%v real=%v", dummyDur, realDur)
	}

	// End-to-end pgx test: unknown user fails with 28P01 via SCRAM PasswordLookup.
	ts := startPhase3Server(t, Config{
		PasswordLookup: func(_ context.Context, user, _ string) (string, error) {
			if user == "postgres" {
				return "correct-secret", nil
			}
			return "", errors.New("unknown user")
		},
	})
	_, err = pgx.Connect(t.Context(), fmt.Sprintf("postgres://unknown_actor:wrong@%s/postgres?sslmode=disable", ts.addr))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
		t.Fatalf("expected 28P01 for unknown user in SCRAM PasswordLookup, got %v", err)
	}
}
