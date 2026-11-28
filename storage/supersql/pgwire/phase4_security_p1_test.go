package pgwire

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gosuda/supersql-alpha"
)

func generateP4SelfSignedTLS(t *testing.T) *tls.Config {
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
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		MinVersion:   tls.VersionTLS12,
	}
}

func makeSCRAMVerifierString(t *testing.T, password string, iterations int) string {
	t.Helper()
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		t.Fatalf("pbkdf2.Key: %v", err)
	}
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s",
		iterations,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(storedKey[:]),
		base64.StdEncoding.EncodeToString(serverKey),
	)
}

func writeP4Parse(t *testing.T, w io.Writer, stmtName, query string) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(stmtName)
	b.WriteByte(0)
	b.WriteString(query)
	b.WriteByte(0)
	_ = binary.Write(&b, binary.BigEndian, int16(0))
	writeP3Msg(t, w, 'P', b.Bytes())
}

func writeP4Bind(t *testing.T, w io.Writer, portalName, stmtName string) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(portalName)
	b.WriteByte(0)
	b.WriteString(stmtName)
	b.WriteByte(0)
	_ = binary.Write(&b, binary.BigEndian, int16(0)) // param formats
	_ = binary.Write(&b, binary.BigEndian, int16(0)) // param values
	_ = binary.Write(&b, binary.BigEndian, int16(0)) // result formats
	writeP3Msg(t, w, 'B', b.Bytes())
}

func readP4UntilReady(t *testing.T, r *bufio.Reader) (types []byte, errCodes []string) {
	t.Helper()
	for {
		typ, body := readP3Msg(t, r)
		types = append(types, typ)
		if typ == 'E' {
			errCodes = append(errCodes, parseWireErrorFields(body)['C'])
		}
		if typ == 'Z' {
			return types, errCodes
		}
	}
}

// --- 1. TLS Enforcement & RequireTLS ---

func TestPhase4TLSEnforcementAndRequireTLS(t *testing.T) {
	// 1a. RequireTLS: true with TLSConfig: nil must fail NewServer.
	if _, err := NewServer(&supersql.Connector{}, Config{RequireTLS: true}); err == nil {
		t.Fatal("expected NewServer to fail when RequireTLS=true and TLSConfig=nil")
	}

	// 1b. TLSConfig != nil rejects plaintext StartupMessage with FATAL 28000.
	tlsCfg := generateP4SelfSignedTLS(t)
	ts := startPhase3Server(t, Config{
		TLSConfig:   tlsCfg,
		RequireTLS:  true,
		ReadTimeout: 300 * time.Millisecond,
	})

	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	r := bufio.NewReader(rawConn)

	writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
	_ = rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, body := readP3Msg(t, r)
	if typ != 'E' {
		t.Fatalf("expected ErrorResponse 'E' for plaintext StartupMessage when TLS is configured, got %q", typ)
	}
	fields := parseWireErrorFields(body)
	if fields['S'] != "FATAL" || fields['C'] != "28000" {
		t.Fatalf("expected FATAL 28000, got %+v", fields)
	}
	if !strings.Contains(fields['M'], "SSL off") {
		t.Fatalf("expected SSL off error message, got %q", fields['M'])
	}

	// 1c. Plaintext rejection also applies when TLSConfig != nil even if RequireTLS is false.
	tsImplicit := startPhase3Server(t, Config{TLSConfig: tlsCfg})
	_, err = pgx.Connect(t.Context(), fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", tsImplicit.addr))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "28000" {
		t.Fatalf("expected pgErr 28000 for sslmode=disable against TLSConfig != nil, got %v", err)
	}

	// 1d. Stalled TLS handshake times out on ReadTimeout instead of hanging.
	stallConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial stallConn: %v", err)
	}
	defer func() { _ = stallConn.Close() }()
	sslPkt := make([]byte, 8)
	binary.BigEndian.PutUint32(sslPkt[0:4], 8)
	binary.BigEndian.PutUint32(sslPkt[4:8], uint32(sslRequestCode))
	if _, err := stallConn.Write(sslPkt); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(stallConn, ack[:]); err != nil || ack[0] != 'S' {
		t.Fatalf("SSLRequest ack = (%q, %v), want 'S'", ack[0], err)
	}
	// Do not send TLS ClientHello; server must close the connection after ReadTimeout (300ms).
	start := time.Now()
	_ = stallConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, readErr := stallConn.Read(ack[:])
	if readErr == nil {
		t.Fatal("expected stalled TLS handshake to be closed by server")
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("stalled TLS handshake took too long to time out: %v", elapsed)
	}

	// 1e. Properly upgraded TLS connection succeeds.
	cfg, err := pgx.ParseConfig(fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=require", ts.addr))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.TLSConfig = &tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("TLS pgx.ConnectConfig: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var got string
	if err := conn.QueryRow(t.Context(), "SELECT 'tls-ok'").Scan(&got); err != nil || got != "tls-ok" {
		t.Fatalf("TLS SELECT = (%q, %v), want ('tls-ok', nil)", got, err)
	}
}

// --- 2. SCRAM-SHA-256 Authentication ---

func TestPhase4SCRAMSHA256Authentication(t *testing.T) {
	ctx := t.Context()

	t.Run("ConfigPasswordUsesSCRAMNotCleartext", func(t *testing.T) {
		ts := startPhase3Server(t, Config{Password: "s3cr3t-scram!"})

		// Verify raw wire sends authTypeSASL (10) with SCRAM-SHA-256, NEVER cleartext (3).
		rawConn, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = rawConn.Close() }()
		r := bufio.NewReader(rawConn)

		writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
		_ = rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		typ, body := readP3Msg(t, r)
		if typ != 'R' {
			t.Fatalf("expected 'R' auth challenge, got %q", typ)
		}
		authType := int32(binary.BigEndian.Uint32(body[0:4]))
		if authType != authTypeSASL {
			t.Fatalf("authType = %d, want authTypeSASL (%d) (must never send cleartext %d)",
				authType, authTypeSASL, authTypeCleartextPassword)
		}
		if !bytes.Equal(body[4:], []byte("SCRAM-SHA-256\x00\x00")) {
			t.Fatalf("SASL mechanisms = %q, want SCRAM-SHA-256\\x00\\x00", body[4:])
		}

		// Reject channel-binding "p=..." GS2 header cleanly.
		var initResp bytes.Buffer
		initResp.WriteString("SCRAM-SHA-256\x00")
		cf := "p=tls-unique,,n=,r=clientnonce123"
		_ = binary.Write(&initResp, binary.BigEndian, int32(len(cf)))
		initResp.WriteString(cf)
		writeP3Msg(t, rawConn, 'p', initResp.Bytes())
		typ, body = readP3Msg(t, r)
		if typ != 'E' {
			t.Fatalf("expected ErrorResponse for p= channel binding, got %q", typ)
		}
		if fields := parseWireErrorFields(body); fields['S'] != "FATAL" || fields['C'] != "08P01" {
			t.Fatalf("expected FATAL 08P01 for p= channel binding, got %+v", fields)
		}

		// pgx with wrong password -> 28P01.
		_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:wrong@%s/postgres?sslmode=disable", ts.addr))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("expected 28P01 on wrong password, got %v", err)
		}

		// pgx with right password -> succeeds.
		conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:s3cr3t-scram!@%s/postgres?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("SCRAM pgx.Connect with Config.Password: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		var n int
		if err := conn.QueryRow(ctx, "SELECT 42").Scan(&n); err != nil || n != 42 {
			t.Fatalf("SELECT 42 = (%d, %v)", n, err)
		}
	})

	t.Run("PasswordLookupPlaintextAndPreHashedVerifier", func(t *testing.T) {
		preHashed := makeSCRAMVerifierString(t, "hashed-pw-99", 4096)

		ts := startPhase3Server(t, Config{
			// Also set AuthFunc to verify PasswordLookup takes precedence over AuthFunc!
			AuthFunc: func(_ context.Context, _, _, _ string) error {
				t.Error("AuthFunc must not be called when PasswordLookup is set")
				return nil
			},
			PasswordLookup: func(_ context.Context, user, database string) (string, error) {
				switch user {
				case "postgres":
					return "plain-pw-11", nil
				case "supersql":
					return preHashed, nil
				default:
					return "", errors.New("user not found")
				}
			},
		})

		// Plaintext lookup succeeds.
		c1, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:plain-pw-11@%s/postgres?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("PasswordLookup plaintext connect: %v", err)
		}
		_ = c1.Close(ctx)

		// Pre-hashed SCRAM-SHA-256$... verifier succeeds.
		c2, err := pgx.Connect(ctx, fmt.Sprintf("postgres://supersql:hashed-pw-99@%s/postgres?sslmode=disable", ts.addr))
		if err != nil {
			t.Fatalf("PasswordLookup pre-hashed verifier connect: %v", err)
		}
		var one int
		if err := c2.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
			_ = c2.Close(ctx)
			t.Fatalf("SELECT 1 = (%d, %v)", one, err)
		}
		_ = c2.Close(ctx)

		// Wrong password against pre-hashed verifier -> 28P01.
		_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://supersql:wrong@%s/postgres?sslmode=disable", ts.addr))
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
			t.Fatalf("expected 28P01 on wrong password for pre-hashed user, got %v", err)
		}

		// Unknown user (lookup failure): server runs fake challenge (SASLContinue) and fails at proof with 28P01.
		rawConn, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatalf("Dial unknown user: %v", err)
		}
		defer func() { _ = rawConn.Close() }()
		r := bufio.NewReader(rawConn)
		writeP3Startup(t, rawConn, map[string]string{"user": "ghost", "database": "postgres"})
		_ = rawConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		typ, body := readP3Msg(t, r)
		if typ != 'R' || int32(binary.BigEndian.Uint32(body[0:4])) != authTypeSASL {
			t.Fatalf("unknown user: expected authTypeSASL, got %q %x", typ, body)
		}
		var initResp bytes.Buffer
		initResp.WriteString("SCRAM-SHA-256\x00")
		cf := "n,,n=,r=ghostnonce123"
		_ = binary.Write(&initResp, binary.BigEndian, int32(len(cf)))
		initResp.WriteString(cf)
		writeP3Msg(t, rawConn, 'p', initResp.Bytes())

		// Must receive AuthenticationSASLContinue (fake challenge) rather than immediate error.
		typ, body = readP3Msg(t, r)
		if typ != 'R' || int32(binary.BigEndian.Uint32(body[0:4])) != authTypeSASLContinue {
			t.Fatalf("unknown user: expected fake AuthenticationSASLContinue (anti-enumeration), got %q %x", typ, body)
		}
		serverFirst := string(body[4:])
		if !strings.Contains(serverFirst, ",i=4096") {
			t.Fatalf("unknown user fake challenge missing i=4096: %q", serverFirst)
		}
		// Extract combined nonce and send dummy proof -> must fail with 28P01.
		firstParts := strings.Split(serverFirst, ",")
		combinedNonce := strings.TrimPrefix(firstParts[0], "r=")
		dummyProof := base64.StdEncoding.EncodeToString(make([]byte, sha256.Size))
		clientFinal := fmt.Sprintf("c=%s,r=%s,p=%s",
			base64.StdEncoding.EncodeToString([]byte("n,,")),
			combinedNonce,
			dummyProof,
		)
		writeP3Msg(t, rawConn, 'p', []byte(clientFinal))
		typ, body = readP3Msg(t, r)
		if typ != 'E' {
			t.Fatalf("unknown user final: expected ErrorResponse, got %q", typ)
		}
		if fields := parseWireErrorFields(body); fields['S'] != "FATAL" || fields['C'] != "28P01" {
			t.Fatalf("unknown user final: expected FATAL 28P01, got %+v", fields)
		}
	})
}

// --- 3. Pre-Auth MaxConnections Starvation & Default Timeouts ---

func TestPhase4DefaultTimeoutsNormalization(t *testing.T) {
	conn := &supersql.Connector{}
	defaultSrv, err := NewServer(conn, Config{})
	if err != nil {
		t.Fatalf("NewServer default: %v", err)
	}
	if defaultSrv.readTimeout != defaultReadTimeout {
		t.Fatalf("default readTimeout = %v, want %v", defaultSrv.readTimeout, defaultReadTimeout)
	}
	if defaultSrv.idleTimeout != defaultIdleTimeout {
		t.Fatalf("default idleTimeout = %v, want %v", defaultSrv.idleTimeout, defaultIdleTimeout)
	}
	if defaultSrv.maxPreparedStmts != defaultMaxPreparedStmts {
		t.Fatalf("default maxPreparedStmts = %d, want %d", defaultSrv.maxPreparedStmts, defaultMaxPreparedStmts)
	}
	if defaultSrv.maxPortals != defaultMaxPortals {
		t.Fatalf("default maxPortals = %d, want %d", defaultSrv.maxPortals, defaultMaxPortals)
	}

	disabledSrv, err := NewServer(conn, Config{ReadTimeout: -1, IdleTimeout: -1})
	if err != nil {
		t.Fatalf("NewServer negative timeouts: %v", err)
	}
	if disabledSrv.readTimeout != 0 || disabledSrv.idleTimeout != 0 {
		t.Fatalf("negative timeouts should disable: got read=%v idle=%v", disabledSrv.readTimeout, disabledSrv.idleTimeout)
	}

	customSrv, err := NewServer(conn, Config{
		ReadTimeout:           5 * time.Second,
		IdleTimeout:           10 * time.Second,
		MaxPreparedStatements: 16,
		MaxPortals:            8,
	})
	if err != nil {
		t.Fatalf("NewServer custom timeouts: %v", err)
	}
	if customSrv.readTimeout != 5*time.Second || customSrv.idleTimeout != 10*time.Second {
		t.Fatalf("custom timeouts mismatch: read=%v idle=%v", customSrv.readTimeout, customSrv.idleTimeout)
	}
	if customSrv.maxPreparedStmts != 16 || customSrv.maxPortals != 8 {
		t.Fatalf("custom caps mismatch: stmts=%d portals=%d", customSrv.maxPreparedStmts, customSrv.maxPortals)
	}
}

func TestPhase4PreAuthDoesNotStarveMaxConnectionsAndTimesOut(t *testing.T) {
	ctx := t.Context()
	ts := startPhase3Server(t, Config{
		MaxConnections: 2,
		Password:       "good-secret",
		ReadTimeout:    400 * time.Millisecond,
	})

	// Open 4 unauthenticated TCP clients that send StartupMessage and stall before completing SCRAM.
	const stalledClients = 4
	stalled := make([]net.Conn, 0, stalledClients)
	for i := range stalledClients {
		c, err := net.Dial("tcp", ts.addr)
		if err != nil {
			t.Fatalf("Dial stalled %d: %v", i, err)
		}
		stalled = append(stalled, c)
		r := bufio.NewReader(c)
		writeP3Startup(t, c, map[string]string{"user": "postgres", "database": "postgres"})
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		typ, _ := readP3Msg(t, r)
		if typ != 'R' {
			t.Fatalf("stalled %d: expected auth challenge 'R', got %q", i, typ)
		}
	}
	defer func() {
		for _, c := range stalled {
			_ = c.Close()
		}
	}()

	// Stalled pre-auth clients must NOT consume active MaxConnections slots.
	if active := ts.server.ActiveConnections(); active != 0 {
		t.Fatalf("ActiveConnections with %d stalled pre-auth clients = %d, want 0", stalledClients, active)
	}

	// Authenticated clients must be able to fill all MaxConnections (2) slots immediately.
	connStr := fmt.Sprintf("postgres://postgres:good-secret@%s/postgres?sslmode=disable", ts.addr)
	auth1, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("auth1 Connect blocked/failed despite pre-auth only clients: %v", err)
	}
	defer func() { _ = auth1.Close(ctx) }()

	auth2, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatalf("auth2 Connect blocked/failed despite pre-auth only clients: %v", err)
	}
	defer func() { _ = auth2.Close(ctx) }()

	if active := ts.server.ActiveConnections(); active != 2 {
		t.Fatalf("ActiveConnections = %d, want 2", active)
	}

	// 3rd authenticated client must be rejected with 53300 too_many_connections.
	_, err = pgx.Connect(ctx, connStr)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "53300" {
		t.Fatalf("expected 53300 for 3rd authenticated client, got %v", err)
	}

	// Verify stalled pre-auth clients are closed by ReadTimeout (400ms).
	var buf [1]byte
	for i, c := range stalled {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := c.Read(buf[:])
		if err == nil {
			t.Fatalf("stalled pre-auth client %d did not time out / close", i)
		}
	}
}

// --- 4. Per-Connection MaxPreparedStatements & MaxPortals Caps ---

func TestPhase4PreparedStatementAndPortalCaps(t *testing.T) {
	ts := startPhase3Server(t, Config{
		MaxPreparedStatements: 2,
		MaxPortals:            2,
	})

	rawConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rawConn.Close() }()
	r := bufio.NewReader(rawConn)

	writeP3Startup(t, rawConn, map[string]string{"user": "postgres", "database": "postgres"})
	readP4UntilReady(t, r)

	// 1. Parse unnamed "" (count=1) and "s1" (count=2) -> both succeed.
	writeP4Parse(t, rawConn, "", "SELECT 1")
	writeP4Parse(t, rawConn, "s1", "SELECT 2")
	writeP3Msg(t, rawConn, 'S', nil)
	types, errCodes := readP4UntilReady(t, r)
	if len(errCodes) != 0 || string(types) != "11Z" {
		t.Fatalf("initial parses got types=%q errCodes=%v, want 11Z / none", string(types), errCodes)
	}

	// 2. Overwriting unnamed "" at cap=2 MUST succeed.
	writeP4Parse(t, rawConn, "", "SELECT 3")
	writeP3Msg(t, rawConn, 'S', nil)
	types, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 0 || string(types) != "1Z" {
		t.Fatalf("overwrite unnamed stmt got types=%q errCodes=%v, want 1Z / none", string(types), errCodes)
	}

	// 3. Parsing a new statement "s2" exceeding MaxPreparedStatements=2 -> SQLSTATE 54000.
	writeP4Parse(t, rawConn, "s2", "SELECT 4")
	writeP3Msg(t, rawConn, 'S', nil)
	_, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 1 || errCodes[0] != "54000" {
		t.Fatalf("exceeding MaxPreparedStatements got errCodes=%v, want [54000]", errCodes)
	}

	// 4. Bind unnamed portal "" (count=1) and "p1" (count=2) -> both succeed.
	writeP4Bind(t, rawConn, "", "s1")
	writeP4Bind(t, rawConn, "p1", "s1")
	writeP3Msg(t, rawConn, 'S', nil)
	types, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 0 || string(types) != "22Z" {
		t.Fatalf("initial binds got types=%q errCodes=%v, want 22Z / none", string(types), errCodes)
	}

	// 5. Overwriting unnamed portal "" at cap=2 MUST succeed.
	writeP4Bind(t, rawConn, "", "")
	writeP3Msg(t, rawConn, 'S', nil)
	types, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 0 || string(types) != "2Z" {
		t.Fatalf("overwrite unnamed portal got types=%q errCodes=%v, want 2Z / none", string(types), errCodes)
	}

	// 6. Binding a new portal "p2" exceeding MaxPortals=2 -> SQLSTATE 54000.
	writeP4Bind(t, rawConn, "p2", "s1")
	writeP3Msg(t, rawConn, 'S', nil)
	_, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 1 || errCodes[0] != "54000" {
		t.Fatalf("exceeding MaxPortals got errCodes=%v, want [54000]", errCodes)
	}

	// 7. Closing "p1" and "s1" frees slots so "p2" and "s2" succeed.
	writeP3Msg(t, rawConn, 'C', []byte("Pp1\x00"))
	writeP3Msg(t, rawConn, 'C', []byte("Ss1\x00"))
	writeP4Parse(t, rawConn, "s2", "SELECT 4")
	writeP4Bind(t, rawConn, "p2", "s2")
	writeP3Msg(t, rawConn, 'S', nil)
	types, errCodes = readP4UntilReady(t, r)
	if len(errCodes) != 0 || string(types) != "3312Z" {
		t.Fatalf("post-close parse/bind got types=%q errCodes=%v, want 3312Z / none", string(types), errCodes)
	}
}

// --- 5. Non-Blocking Deadline-Bounded BroadcastNotify ---

func TestPhase4BroadcastNotifyNonBlockingAndDeadlineBounded(t *testing.T) {
	ctx := t.Context()

	// 5a. Direct unit test of writeAndFlushWithDeadline on an unread net.Pipe.
	srvPipe, cliPipe := net.Pipe()
	defer func() {
		_ = srvPipe.Close()
		_ = cliPipe.Close()
	}()
	cc := &clientConn{
		rawConn: srvPipe,
		br:      bufio.NewReader(srvPipe),
		bw:      bufio.NewWriter(srvPipe),
		pid:     1234,
	}
	start := time.Now()
	err := cc.writeAndFlushWithDeadline(encodeNotificationResponse(1, "ch", strings.Repeat("x", 1024)), 60*time.Millisecond)
	if err == nil {
		t.Fatal("expected write timeout error on unread net.Pipe, got nil")
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Fatalf("writeAndFlushWithDeadline took %v, expected ~60ms", elapsed)
	}
	// Subsequent write must fail immediately with net.ErrClosed (no partial frame corruption).
	if err2 := cc.writeAndFlushWithDeadline(encodeNotificationResponse(1, "ch", "next"), 60*time.Millisecond); !errors.Is(err2, net.ErrClosed) {
		t.Fatalf("subsequent write after timeout = %v, want net.ErrClosed", err2)
	}

	// 5b. End-to-end TCP server test: a stalled TCP reader must NOT block
	// BroadcastNotify callers or delivery to a healthy peer.
	ts := startPhase3Server(t, Config{})

	// Stalled raw TCP peer: completes startup, shrinks recv buffer, then never reads again.
	stalledConn, err := net.Dial("tcp", ts.addr)
	if err != nil {
		t.Fatalf("Dial stalledConn: %v", err)
	}
	defer func() { _ = stalledConn.Close() }()
	if tc, ok := stalledConn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	stalledReader := bufio.NewReader(stalledConn)
	writeP3Startup(t, stalledConn, map[string]string{"user": "postgres", "database": "postgres"})
	readP4UntilReady(t, stalledReader)

	// Healthy pgx peer.
	healthy, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres@%s/postgres?sslmode=disable", ts.addr))
	if err != nil {
		t.Fatalf("Connect healthy: %v", err)
	}
	defer func() { _ = healthy.Close(ctx) }()

	// Drain notifications on healthy peer concurrently while flooding broadcasts
	// large enough to saturate both stalledConn's TCP buffer and broadcastCh (64).
	receivedHealth := make(chan string, 1)
	stopHealthy := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopHealthy:
				return
			default:
			}
			wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			n, err := healthy.WaitForNotification(wctx)
			cancel()
			if err != nil {
				return
			}
			if n.Channel == "health" {
				select {
				case receivedHealth <- n.Payload:
				default:
				}
				return
			}
		}
	}()
	defer close(stopHealthy)

	bigPayload := strings.Repeat("A", 32*1024)
	broadcastStart := time.Now()
	for range 200 {
		ts.server.BroadcastNotify(42, "flood", bigPayload)
	}
	broadcastElapsed := time.Since(broadcastStart)
	if broadcastElapsed > 500*time.Millisecond {
		t.Fatalf("200 BroadcastNotify calls blocked for %v (> 500ms) due to stalled peer!", broadcastElapsed)
	}

	// Send sentinel notification and verify healthy peer receives it promptly
	// (long before stalled peer's 2s notify write timeout expires).
	// Give healthy peer a brief drain window if its 64-entry queue was saturated by the flood.
	delivered := false
	deadline := time.Now().Add(1500 * time.Millisecond)
	for !delivered && time.Now().Before(deadline) {
		ts.server.BroadcastNotify(99, "health", "still-alive")
		select {
		case got := <-receivedHealth:
			if got != "still-alive" {
				t.Fatalf("health payload = %q, want still-alive", got)
			}
			delivered = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !delivered {
		t.Fatal("healthy peer did not receive BroadcastNotify promptly while another peer was stalled")
	}
}
