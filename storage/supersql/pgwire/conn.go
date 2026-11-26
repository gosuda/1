package pgwire

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type preparedStmt struct {
	name            string
	sql             string
	raw             *zpgparse.RawStmt
	isEmpty         bool
	returnsRows     bool
	verb            string
	txInfo          txStmtInfo
	clientParamOIDs []uint32
	analysis        paramAnalysis
	driverStmt      driver.Stmt

	described  bool
	paramOIDs  []uint32
	columns    []columnDesc
	hasColumns bool
}

func (ps *preparedStmt) close() {
	if ps != nil && ps.driverStmt != nil {
		_ = ps.driverStmt.Close()
		ps.driverStmt = nil
	}
}

type portal struct {
	name          string
	stmt          *preparedStmt
	args          []driver.NamedValue
	resultFormats []int16

	started     bool
	completed   bool
	rows        driver.Rows
	cols        []columnDesc
	bufferedRow []driver.Value
	hasBuffered bool
	totalRows   int64
	ctx         context.Context
	cancel      context.CancelFunc
}

func (p *portal) resultFormat(colIdx int) int16 {
	if len(p.resultFormats) == 0 {
		return formatText
	}
	if len(p.resultFormats) == 1 {
		return p.resultFormats[0]
	}
	if colIdx >= 0 && colIdx < len(p.resultFormats) {
		return p.resultFormats[colIdx]
	}
	return formatText
}

func (p *portal) close() error {
	if p == nil {
		return nil
	}
	var err error
	if p.rows != nil {
		err = p.rows.Close()
		p.rows = nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.ctx = nil
	p.bufferedRow = nil
	p.hasBuffered = false
	return err
}

type clientConn struct {
	server    *Server
	connMu    sync.Mutex
	rawConn   net.Conn
	closing   bool
	br        *bufio.Reader
	bw        *bufio.Writer
	writeMu   sync.Mutex
	closed    bool
	pid       int32
	secretKey int32

	driverConn  driver.Conn
	user        string
	database    string
	appName     string
	timeZone    string
	boundRole   string
	isSuperuser bool

	txStatus byte
	inError  bool

	stmts   map[string]*preparedStmt
	portals map[string]*portal

	cancelMu     sync.Mutex
	activeCancel context.CancelFunc
}

func (c *clientConn) currentConn() net.Conn {
	c.connMu.Lock()
	rc := c.rawConn
	c.connMu.Unlock()
	return rc
}

func (c *clientConn) setConn(nc net.Conn) error {
	c.connMu.Lock()
	if c.closing {
		c.connMu.Unlock()
		_ = nc.Close()
		return net.ErrClosed
	}
	c.rawConn = nc
	c.connMu.Unlock()
	c.writeMu.Lock()
	c.br = bufio.NewReaderSize(nc, 16*1024)
	c.bw = bufio.NewWriterSize(nc, 16*1024)
	c.writeMu.Unlock()
	return nil
}

func (c *clientConn) forceClose() {
	c.connMu.Lock()
	c.closing = true
	rc := c.rawConn
	c.connMu.Unlock()
	if rc != nil {
		_ = rc.SetDeadline(time.Now())
		_ = rc.Close()
	}
}

func (c *clientConn) writeFrame(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	_, err := c.bw.Write(frame)
	return err
}

func (c *clientConn) flush() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.bw.Flush()
}

func (c *clientConn) writeAndFlush(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if _, err := c.bw.Write(frame); err != nil {
		return err
	}
	return c.bw.Flush()
}

func (c *clientConn) sendNotification(n Notification) error {
	pid := n.PID
	if pid == 0 {
		pid = c.pid
	}
	frame := encodeNotificationResponse(pid, n.Channel, n.Payload)
	return c.writeAndFlush(frame)
}

func (c *clientConn) setActiveCancel(cancel context.CancelFunc) {
	c.cancelMu.Lock()
	c.activeCancel = cancel
	c.cancelMu.Unlock()
}

func (c *clientConn) cancelActiveQuery() {
	c.cancelMu.Lock()
	cancel := c.activeCancel
	c.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *clientConn) closeResources() {
	c.cancelActiveQuery()
	for name, p := range c.portals {
		_ = p.close()
		delete(c.portals, name)
	}
	for name, ps := range c.stmts {
		ps.close()
		delete(c.stmts, name)
	}
	if c.driverConn != nil {
		_ = c.driverConn.Close()
		c.driverConn = nil
	}
	c.writeMu.Lock()
	c.closed = true
	c.writeMu.Unlock()
	c.forceClose()
}

func (c *clientConn) safeSendFatalPanic() {
	defer func() {
		_ = recover()
	}()
	if rc := c.currentConn(); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
	}
	_ = c.writeAndFlush(encodeErrorResponse(wireError{
		Severity: "FATAL",
		Code:     "XX000",
		Message:  "internal server error",
	}))
}

func (c *clientConn) recoverPanic(r any) {
	if c.server != nil {
		c.server.logf("pgwire: recovered panic on connection pid=%d: %v\n%s", c.pid, r, debug.Stack())
	}
	c.safeSendFatalPanic()
}

func (c *clientConn) serve(ctx context.Context) {
	preAuthReleased := false
	releasePreAuth := func() {
		if !preAuthReleased {
			preAuthReleased = true
			c.server.unregisterPreAuth(c)
			c.server.releasePreAuthSlot()
		}
	}
	defer func() {
		releasePreAuth()
		c.closeResources()
	}()
	defer func() {
		if r := recover(); r != nil {
			c.recoverPanic(r)
		}
	}()

	// 1. Startup negotiation (SSLRequest / GSSENCRequest / CancelRequest / StartupMessage).
	params, isCancel, err := c.handshake(ctx)
	if err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			c.server.logf("pgwire: startup error from %s: %v", c.currentConn().RemoteAddr(), err)
		}
		return
	}
	if isCancel {
		return
	}

	// 2. Enforce MaxConnections after CancelRequest so cancellations always succeed.
	if !c.server.acquireSlot() {
		_ = c.writeAndFlush(encodeErrorResponse(wireError{
			Severity: "FATAL",
			Code:     "53300",
			Message:  "sorry, too many clients already",
		}))
		return
	}
	defer c.server.releaseSlot()

	c.user = params["user"]
	if c.user == "" {
		c.user = "postgres"
	}
	c.database = params["database"]
	if c.database == "" {
		c.database = c.user
	}
	c.appName = params["application_name"]
	c.timeZone = "UTC"
	if tz := params["TimeZone"]; tz != "" {
		c.timeZone = tz
	} else if tz := params["timezone"]; tz != "" {
		c.timeZone = tz
	}

	// 3. Authentication.
	if err := c.authenticate(ctx); err != nil {
		return
	}

	// 4. Open isolated SuperSQL session for this TCP connection.
	dc, err := c.server.connector.Connect(ctx)
	if err != nil {
		_ = c.writeAndFlush(encodeErrorResponse(toWireError(err, "FATAL")))
		return
	}
	c.driverConn = dc

	// Fail-closed role authentication and session role binding before executing any startup SQL.
	if err := c.bindAuthenticatedRole(ctx); err != nil {
		_ = c.writeAndFlush(encodeErrorResponse(toWireError(err, "FATAL")))
		return
	}

	// Apply safe startup session parameters (best effort).
	c.applyStartupParams(ctx, params)

	// 5. Promote from pre-auth tracking to active connection registry.
	c.server.registerConn(c)
	releasePreAuth()
	defer c.server.unregisterConn(c.pid)

	// 6. Send Backend startup parameters, BackendKeyData, and ReadyForQuery.
	if err := c.sendStartupParameters(); err != nil {
		return
	}

	// 7. Wire optional LISTEN/NOTIFY hook and automatic session hub push.
	if c.server.cfg.NotifyHandler != nil {
		if cleanup := c.server.cfg.NotifyHandler(ctx, c.pid, c.sendNotification); cleanup != nil {
			defer cleanup()
		}
	}
	if nc, ok := c.driverConn.(notificationDriverConn); ok {
		watchCtx, cancelWatch := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			defer func() {
				if r := recover(); r != nil {
					c.server.logf("pgwire: recovered panic in notification watcher pid=%d: %v\n%s", c.pid, r, debug.Stack())
				}
			}()
			for {
				n, err := nc.WaitForNotification(watchCtx)
				if err != nil {
					return
				}
				pid := int32(n.PID)
				if pid == 0 {
					pid = c.pid
				}
				if err := c.sendNotification(Notification{
					PID:     pid,
					Channel: n.Channel,
					Payload: n.Payload,
				}); err != nil {
					return
				}
			}
		}()
		defer func() {
			cancelWatch()
			// Unblock any write/flush in progress on a stalled TCP peer before waiting.
			c.forceClose()
			<-watchDone
		}()
	}

	// 8. Main protocol command loop.
	c.commandLoop(ctx)
}

func (c *clientConn) handshake(ctx context.Context) (map[string]string, bool, error) {
	var sawSSL, sawGSS, tlsUpgraded bool
	for {
		rc := c.currentConn()
		if c.server.cfg.ReadTimeout > 0 {
			_ = rc.SetReadDeadline(time.Now().Add(c.server.cfg.ReadTimeout))
		}
		pkt, err := readStartupPacket(rc)
		if err != nil {
			return nil, false, err
		}
		switch pkt.Code {
		case sslRequestCode:
			if sawSSL || tlsUpgraded {
				return nil, false, errors.New("repeated SSLRequest during startup")
			}
			sawSSL = true
			if c.server.cfg.TLSConfig != nil {
				if _, err := rc.Write([]byte{'S'}); err != nil {
					return nil, false, err
				}
				tlsConn := tls.Server(rc, c.server.cfg.TLSConfig.Clone())
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					return nil, false, err
				}
				if err := c.setConn(tlsConn); err != nil {
					return nil, false, err
				}
				tlsUpgraded = true
			} else {
				if _, err := rc.Write([]byte{'N'}); err != nil {
					return nil, false, err
				}
			}
		case gssEncRequestCode:
			if sawGSS || tlsUpgraded {
				return nil, false, errors.New("repeated GSSENCRequest during startup")
			}
			sawGSS = true
			if _, err := rc.Write([]byte{'N'}); err != nil {
				return nil, false, err
			}
		case cancelRequestCode:
			c.server.cancelQuery(pkt.PID, pkt.Secret)
			return nil, true, nil
		case protocolVersion30:
			_ = rc.SetReadDeadline(time.Time{})
			return pkt.Params, false, nil
		default:
			_ = c.writeAndFlush(encodeErrorResponse(wireError{
				Severity: "FATAL",
				Code:     "0A000",
				Message:  fmt.Sprintf("unsupported frontend protocol version %d.%d", pkt.Code>>16, pkt.Code&0xFFFF),
			}))
			return nil, false, fmt.Errorf("unsupported protocol code %d", pkt.Code)
		}
	}
}

func (c *clientConn) authenticate(ctx context.Context) error {
	if c.server.cfg.AuthFunc == nil && c.server.cfg.Password == "" {
		return c.writeFrame(encodeAuthMessage(authTypeOk))
	}
	if err := c.writeAndFlush(encodeAuthMessage(authTypeCleartextPassword)); err != nil {
		return err
	}
	if c.server.cfg.ReadTimeout > 0 {
		rc := c.currentConn()
		_ = rc.SetReadDeadline(time.Now().Add(c.server.cfg.ReadTimeout))
		defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	}
	msgType, body, err := readFrontendMessageWithLimit(c.br, maxPasswordMessageBytes)
	if err != nil {
		var we *wireError
		if errors.As(err, &we) {
			_ = c.writeAndFlush(encodeErrorResponse(*we))
		}
		return err
	}
	if msgType != 'p' {
		_ = c.writeAndFlush(encodeErrorResponse(wireError{
			Severity: "FATAL",
			Code:     "08P01",
			Message:  "expected PasswordMessage",
		}))
		return fmt.Errorf("expected password message, got %q", msgType)
	}
	password, _, ok := readCString(body, 0)
	if !ok {
		password = string(body)
	}
	var authErr error
	if c.server.cfg.AuthFunc != nil {
		authErr = c.server.cfg.AuthFunc(ctx, c.user, c.database, password)
	} else if subtle.ConstantTimeCompare([]byte(password), []byte(c.server.cfg.Password)) != 1 {
		authErr = fmt.Errorf("password authentication failed for user %q", c.user)
	}
	if authErr != nil {
		we := wireError{
			Severity: "FATAL",
			Code:     "28P01",
			Message:  fmt.Sprintf("password authentication failed for user %q", c.user),
		}
		var explicit *wireError
		if errors.As(authErr, &explicit) {
			we = *explicit
			we.Severity = "FATAL"
		} else if code := supersql.SQLState(authErr); code != "" {
			we.Code = code
			we.Message = authErr.Error()
		}
		_ = c.writeAndFlush(encodeErrorResponse(we))
		return authErr
	}
	return c.writeFrame(encodeAuthMessage(authTypeOk))
}

func (c *clientConn) applyStartupParams(ctx context.Context, params map[string]string) {
	execer, ok := c.driverConn.(driver.ExecerContext)
	if !ok {
		return
	}
	apply := func(guc, val string) {
		if val == "" {
			return
		}
		escaped := strings.ReplaceAll(val, "'", "''")
		_, _ = execer.ExecContext(ctx, fmt.Sprintf("SET %s = '%s'", guc, escaped), nil)
	}
	if tz := params["TimeZone"]; tz != "" {
		apply("TimeZone", tz)
	} else if tz := params["timezone"]; tz != "" {
		apply("TimeZone", tz)
	}
	if sp := params["search_path"]; sp != "" {
		apply("search_path", sp)
	}
	if app := params["application_name"]; app != "" {
		apply("application_name", app)
	}
}

// bindAuthenticatedRole verifies and binds the SQL session to the authenticated
// username. Built-in embedded superuser aliases ("", "postgres", "supersql",
// "embedded") retain superuser privileges. Every other username is verified
// fail-closed against pg_catalog.pg_roles (must exist and have rolcanlogin = true)
// and pinned on the session so RESET ROLE / RESET ALL / SET ROLE NONE cannot
// escalate back to the default embedded superuser.
func (c *clientConn) bindAuthenticatedRole(ctx context.Context) error {
	c.isSuperuser = false
	c.boundRole = ""
	user := strings.ToLower(strings.TrimSpace(c.user))
	if user == "" || user == "postgres" || user == "supersql" || user == "embedded" {
		c.isSuperuser = true
		return nil
	}
	queryer, ok := c.driverConn.(driver.QueryerContext)
	if !ok {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("cannot authenticate role %q", c.user),
		}
	}
	rows, err := queryer.QueryContext(ctx,
		`SELECT rolsuper, rolcanlogin FROM pg_catalog.pg_roles WHERE lower(rolname) = $1`,
		[]driver.NamedValue{{Ordinal: 1, Value: user}},
	)
	if err != nil {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("failed to authenticate role %q", c.user),
		}
	}
	var found bool
	var rolsuper bool
	var rolcanlogin bool
	dest := make([]driver.Value, 2)
	if nextErr := rows.Next(dest); nextErr == nil {
		found = true
		if b, ok := dest[0].(bool); ok {
			rolsuper = b
		}
		if b, ok := dest[1].(bool); ok {
			rolcanlogin = b
		}
	} else if !errors.Is(nextErr, io.EOF) {
		_ = rows.Close()
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("failed to authenticate role %q", c.user),
		}
	}
	if closeErr := rows.Close(); closeErr != nil {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("failed to authenticate role %q", c.user),
		}
	}
	if !found {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("role %q does not exist", c.user),
		}
	}
	if !rolcanlogin {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("role %q is not permitted to log in", c.user),
		}
	}
	if user == "default" {
		return &wireError{
			Severity: "FATAL",
			Code:     "28000",
			Message:  fmt.Sprintf("role %q is not permitted to log in", c.user),
		}
	}
	type sessionUserSetter interface {
		SetSessionUser(context.Context, string) error
	}
	if setter, ok := c.driverConn.(sessionUserSetter); ok {
		if err := setter.SetSessionUser(ctx, user); err != nil {
			return &wireError{
				Severity: "FATAL",
				Code:     "28000",
				Message:  fmt.Sprintf("failed to set session user %q: %v", user, err),
			}
		}
	} else {
		execer, ok := c.driverConn.(driver.ExecerContext)
		if !ok {
			return &wireError{
				Severity: "FATAL",
				Code:     "28000",
				Message:  fmt.Sprintf("cannot bind session role %q", user),
			}
		}
		if _, err := execer.ExecContext(ctx, fmt.Sprintf("SET ROLE %s", quoteIdent(user)), nil); err != nil {
			return &wireError{
				Severity: "FATAL",
				Code:     "28000",
				Message:  fmt.Sprintf("failed to set session role %q: %v", user, err),
			}
		}
	}
	c.user = user
	c.boundRole = user
	c.isSuperuser = rolsuper
	return nil
}

func (c *clientConn) sendStartupParameters() error {
	superVal := "on"
	if !c.isSuperuser {
		superVal = "off"
	}
	statusPairs := [][2]string{
		{"server_version", c.server.appVersion},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", c.timeZone},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
		{"is_superuser", superVal},
		{"session_authorization", c.user},
		{"application_name", c.appName},
	}
	for _, kv := range statusPairs {
		if err := c.writeFrame(encodeParameterStatus(kv[0], kv[1])); err != nil {
			return err
		}
	}
	if err := c.writeFrame(encodeBackendKeyData(c.pid, c.secretKey)); err != nil {
		return err
	}
	if err := c.writeFrame(encodeReadyForQuery(c.txStatus)); err != nil {
		return err
	}
	return c.flush()
}

func (c *clientConn) commandLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			c.recoverPanic(r)
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		rc := c.currentConn()
		// Apply IdleTimeout while waiting for the first byte of the next message.
		if c.server.cfg.IdleTimeout > 0 {
			_ = rc.SetReadDeadline(time.Now().Add(c.server.cfg.IdleTimeout))
		} else {
			_ = rc.SetReadDeadline(time.Time{})
		}
		msgType, err := c.br.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				var netErr net.Error
				if !errors.As(err, &netErr) || !netErr.Timeout() {
					c.server.logf("pgwire: read error from %s: %v", rc.RemoteAddr(), err)
				}
			}
			return
		}

		// Apply ReadTimeout for the remainder of the incoming message frame.
		if c.server.cfg.ReadTimeout > 0 {
			_ = rc.SetReadDeadline(time.Now().Add(c.server.cfg.ReadTimeout))
		} else {
			_ = rc.SetReadDeadline(time.Time{})
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
			return
		}
		rawLen := binary.BigEndian.Uint32(lenBuf[:])
		limit := uint32(maxMessageBytes)
		if msgType == 'p' {
			limit = maxPasswordMessageBytes
		}
		if rawLen < 4 || rawLen > limit {
			_ = c.writeAndFlush(encodeErrorResponse(wireError{
				Severity: "FATAL",
				Code:     "08P01",
				Message:  fmt.Sprintf("invalid frontend message length: %d", rawLen),
			}))
			return
		}
		body, err := readMessageBody(c.br, int(rawLen)-4)
		if err != nil {
			return
		}
		// Clear socket deadline while executing SQL so long-running queries are
		// governed by cancellation/statement timeouts rather than IdleTimeout.
		_ = rc.SetReadDeadline(time.Time{})

		switch msgType {
		case 'X': // Terminate
			return
		case 'Q': // Simple Query
			c.inError = false
			if err := c.handleSimpleQuery(ctx, body); err != nil {
				return
			}
		case 'S': // Sync
			c.inError = false
			if err := c.drainDriverNotifications(); err != nil {
				return
			}
			if err := c.writeAndFlush(encodeReadyForQuery(c.txStatus)); err != nil {
				return
			}
		case 'H': // Flush
			if err := c.flush(); err != nil {
				return
			}
		case 'P': // Parse
			if c.inError {
				continue
			}
			if err := c.handleParse(ctx, body); err != nil {
				c.inError = true
				if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
					return
				}
			}
		case 'B': // Bind
			if c.inError {
				continue
			}
			if err := c.handleBind(ctx, body); err != nil {
				c.inError = true
				if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
					return
				}
			}
		case 'D': // Describe
			if c.inError {
				continue
			}
			if err := c.handleDescribe(ctx, body); err != nil {
				c.inError = true
				if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
					return
				}
			}
		case 'E': // Execute
			if c.inError {
				continue
			}
			if err := c.handleExecute(ctx, body); err != nil {
				c.inError = true
				if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
					return
				}
			}
		case 'C': // Close
			if c.inError {
				continue
			}
			if err := c.handleClose(body); err != nil {
				c.inError = true
				if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
					return
				}
			}
		default:
			_ = c.writeAndFlush(encodeErrorResponse(wireError{
				Severity: "FATAL",
				Code:     "08P01",
				Message:  fmt.Sprintf("unsupported frontend message type %q", msgType),
			}))
			return
		}
	}
}

// --- Transaction state tracking ---

func (c *clientConn) updateTxStatusOnSuccess(info txStmtInfo, tag string) {
	switch info.kind {
	case txStmtBegin:
		if c.txStatus == txStatusIdle {
			c.txStatus = txStatusInTx
		}
	case txStmtCommit, txStmtRollback:
		if info.chain {
			c.txStatus = txStatusInTx
		} else {
			c.txStatus = txStatusIdle
		}
	case txStmtRollbackTo:
		if c.txStatus == txStatusFailed || c.txStatus == txStatusInTx {
			c.txStatus = txStatusInTx
		}
	case txStmtPrepareTx:
		c.txStatus = txStatusIdle
	default:
		// Cross-check against returned command tag for statements like CALL/DO
		// or edge cases.
		upper := strings.ToUpper(strings.TrimSpace(tag))
		if upper == "BEGIN" || upper == "START TRANSACTION" {
			if c.txStatus == txStatusIdle {
				c.txStatus = txStatusInTx
			}
		} else if upper == "COMMIT" || upper == "ROLLBACK" {
			if info.kind == txStmtNone && c.txStatus != txStatusIdle {
				// Leave unchanged unless it was a transaction statement.
			}
		}
	}
}

func (c *clientConn) updateTxStatusOnError(info txStmtInfo, err error) {
	state := supersql.SQLState(err)
	if state == "25P02" {
		c.txStatus = txStatusFailed
		return
	}
	if info.kind == txStmtCommit {
		// A failed COMMIT (e.g., 40001 serialization error or 25P01 no active
		// transaction) ends the transaction block.
		if state == "40001" || state == "25P01" || state == "40000" || state == "40002" {
			c.txStatus = txStatusIdle
			return
		}
	}
	if info.kind == txStmtRollback && state == "25P01" {
		c.txStatus = txStatusIdle
		return
	}
	if c.txStatus == txStatusInTx {
		c.txStatus = txStatusFailed
	}
}

func isTxRecoveryStmt(info txStmtInfo) bool {
	return info.kind == txStmtRollback || info.kind == txStmtCommit || info.kind == txStmtRollbackTo
}

// --- Simple Query Protocol ('Q') ---

func (c *clientConn) handleSimpleQuery(ctx context.Context, body []byte) error {
	queryStr, _, ok := readCString(body, 0)
	if !ok {
		queryStr = string(body)
	}
	stmts, raws, err := splitSimpleQuery(queryStr)
	if err != nil {
		c.updateTxStatusOnError(txStmtInfo{}, err)
		if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
			return werr
		}
		return c.writeAndFlush(encodeReadyForQuery(c.txStatus))
	}
	if len(stmts) == 0 {
		if werr := c.writeFrame(encodeEmptyQueryResponse()); werr != nil {
			return werr
		}
		return c.writeAndFlush(encodeReadyForQuery(c.txStatus))
	}

	for i, stmtSQL := range stmts {
		raw := raws[i]
		txInfo := classifyTxStmt(raw)
		if c.txStatus == txStatusFailed && !isTxRecoveryStmt(txInfo) {
			err := &wireError{
				Severity: "ERROR",
				Code:     "25P02",
				Message:  "current transaction is aborted, commands ignored until end of transaction block",
			}
			if werr := c.writeFrame(encodeErrorResponse(*err)); werr != nil {
				return werr
			}
			break
		}
		if err := c.execSimpleStatement(ctx, stmtSQL, raw, txInfo); err != nil {
			c.updateTxStatusOnError(txInfo, err)
			if werr := c.writeFrame(encodeErrorResponse(toWireError(err, "ERROR"))); werr != nil {
				return werr
			}
			// Stop executing subsequent statements in the simple query string.
			break
		}
	}
	if err := c.drainDriverNotifications(); err != nil {
		return err
	}
	return c.writeAndFlush(encodeReadyForQuery(c.txStatus))
}

type notificationDriverConn interface {
	WaitForNotification(ctx context.Context) (supersql.Notification, error)
	DrainNotifications() []supersql.Notification
}

func (c *clientConn) drainDriverNotifications() error {
	nc, ok := c.driverConn.(notificationDriverConn)
	if !ok {
		return nil
	}
	for _, n := range nc.DrainNotifications() {
		pid := int32(n.PID)
		if pid == 0 {
			pid = c.pid
		}
		if err := c.writeFrame(encodeNotificationResponse(pid, n.Channel, n.Payload)); err != nil {
			return err
		}
	}
	return nil
}

func (c *clientConn) execSimpleStatement(ctx context.Context, stmtSQL string, raw *zpgparse.RawStmt, txInfo txStmtInfo) error {
	execCtx, cancel := context.WithCancel(ctx)
	c.setActiveCancel(cancel)
	defer func() {
		c.setActiveCancel(nil)
		cancel()
	}()

	if stmtReturnsRows(raw) {
		queryer, ok := c.driverConn.(driver.QueryerContext)
		if !ok {
			return &wireError{Severity: "ERROR", Code: "XX000", Message: "driver connection does not support QueryContext"}
		}
		rows, err := queryer.QueryContext(execCtx, stmtSQL, nil)
		if err != nil {
			return err
		}
		cols := extractRowColumns(rows, nil)
		if err := c.writeFrame(encodeRowDescription(cols)); err != nil {
			_ = rows.Close()
			return err
		}
		dest := make([]driver.Value, len(cols))
		var rowCount int64
		for {
			if err := rows.Next(dest); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				_ = rows.Close()
				return err
			}
			encoded, err := encodeRowFields(cols, dest)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if err := c.writeFrame(encodeDataRow(encoded)); err != nil {
				_ = rows.Close()
				return err
			}
			rowCount++
		}
		if err := rows.Close(); err != nil {
			return err
		}
		tag := formatRowCommandTag(stmtCommandVerb(raw), rowCount)
		c.updateTxStatusOnSuccess(txInfo, tag)
		return c.writeFrame(encodeCommandComplete(tag))
	}

	execer, ok := c.driverConn.(driver.ExecerContext)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "XX000", Message: "driver connection does not support ExecContext"}
	}
	res, err := execer.ExecContext(execCtx, stmtSQL, nil)
	if err != nil {
		return err
	}
	tag := extractCommandTag(res, raw)
	c.updateTxStatusOnSuccess(txInfo, tag)
	return c.writeFrame(encodeCommandComplete(tag))
}

// --- Extended Query Protocol ('P', 'B', 'D', 'E', 'C') ---

func (c *clientConn) handleParse(ctx context.Context, body []byte) error {
	stmtName, pos, ok := readCString(body, 0)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Parse message: missing statement name"}
	}
	queryStr, pos, ok := readCString(body, pos)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Parse message: missing query string"}
	}
	if pos+2 > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Parse message: missing parameter count"}
	}
	numParamTypes := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	if numParamTypes > maxWireParams {
		return tooManyParametersError(numParamTypes)
	}
	pos += 2
	if pos+4*numParamTypes > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Parse message: truncated parameter OIDs"}
	}
	clientOIDs := make([]uint32, numParamTypes)
	for i := range numParamTypes {
		clientOIDs[i] = binary.BigEndian.Uint32(body[pos+4*i : pos+4*i+4])
	}

	if stmtName != "" {
		if _, exists := c.stmts[stmtName]; exists {
			return &wireError{
				Severity: "ERROR",
				Code:     "42P05",
				Message:  fmt.Sprintf("prepared statement %q already exists", stmtName),
			}
		}
	}

	raw, err := parseSingleStatement(queryStr)
	if err != nil {
		c.updateTxStatusOnError(txStmtInfo{}, err)
		return err
	}

	txInfo := classifyTxStmt(raw)
	analysis, err := analyzeParams(raw)
	if err != nil {
		c.updateTxStatusOnError(txInfo, err)
		return err
	}
	if analysis.maxParam > maxWireParams {
		err := tooManyParametersError(analysis.maxParam)
		c.updateTxStatusOnError(txInfo, err)
		return err
	}

	ps := &preparedStmt{
		name:            stmtName,
		sql:             queryStr,
		raw:             raw,
		isEmpty:         raw == nil,
		returnsRows:     stmtReturnsRows(raw),
		verb:            stmtCommandVerb(raw),
		txInfo:          txInfo,
		clientParamOIDs: clientOIDs,
		analysis:        analysis,
	}

	// If in a failed transaction block, only transaction recovery commands may
	// proceed through Parse (PostgreSQL reports 25P02 for all other statements).
	if c.txStatus == txStatusFailed && !ps.isEmpty && !isTxRecoveryStmt(ps.txInfo) {
		return &wireError{
			Severity: "ERROR",
			Code:     "25P02",
			Message:  "current transaction is aborted, commands ignored until end of transaction block",
		}
	}

	if !ps.isEmpty {
		if preparer, ok := c.driverConn.(driver.ConnPrepareContext); ok {
			dStmt, err := preparer.PrepareContext(ctx, queryStr)
			if err != nil {
				c.updateTxStatusOnError(ps.txInfo, err)
				return err
			}
			ps.driverStmt = dStmt
		}
	}

	// Overwrite unnamed statement cleanly.
	if old, exists := c.stmts[stmtName]; exists {
		old.close()
	}
	c.stmts[stmtName] = ps
	return c.writeFrame(encodeParseComplete())
}

func (c *clientConn) handleBind(ctx context.Context, body []byte) error {
	portalName, pos, ok := readCString(body, 0)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: missing portal name"}
	}
	stmtName, pos, ok := readCString(body, pos)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: missing statement name"}
	}
	ps, exists := c.stmts[stmtName]
	if !exists {
		return &wireError{
			Severity: "ERROR",
			Code:     "26000",
			Message:  fmt.Sprintf("prepared statement %q does not exist", stmtName),
		}
	}
	if portalName != "" {
		if _, exists := c.portals[portalName]; exists {
			return &wireError{
				Severity: "ERROR",
				Code:     "42P03",
				Message:  fmt.Sprintf("cursor %q already exists", portalName),
			}
		}
	}

	if pos+2 > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: missing param format count"}
	}
	numParamFormats := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	pos += 2
	if pos+2*numParamFormats > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: truncated param formats"}
	}
	paramFormats := make([]int16, numParamFormats)
	for i := range numParamFormats {
		paramFormats[i] = int16(binary.BigEndian.Uint16(body[pos+2*i : pos+2*i+2]))
	}
	pos += 2 * numParamFormats

	if pos+2 > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: missing param value count"}
	}
	numParams := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	if numParams > maxWireParams {
		return tooManyParametersError(numParams)
	}
	pos += 2
	rawParams := make([][]byte, numParams)
	hasBinaryParam := false
	for i := range numParams {
		if pos+4 > len(body) {
			return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: truncated param length"}
		}
		pLen := int(int32(binary.BigEndian.Uint32(body[pos : pos+4])))
		pos += 4
		if pLen == -1 {
			rawParams[i] = nil
			continue
		}
		if pLen < 0 || pos+pLen > len(body) {
			return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: truncated param payload"}
		}
		rawParams[i] = body[pos : pos+pLen]
		pos += pLen
		if paramFormatForIndex(paramFormats, i) == formatBinary {
			hasBinaryParam = true
		}
	}

	if pos+2 > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: missing result format count"}
	}
	numResultFormats := int(binary.BigEndian.Uint16(body[pos : pos+2]))
	pos += 2
	if pos+2*numResultFormats > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Bind message: truncated result formats"}
	}
	resultFormats := make([]int16, numResultFormats)
	for i := range numResultFormats {
		resultFormats[i] = int16(binary.BigEndian.Uint16(body[pos+2*i : pos+2*i+2]))
	}

	if c.txStatus == txStatusFailed && !ps.isEmpty && !isTxRecoveryStmt(ps.txInfo) {
		return &wireError{
			Severity: "ERROR",
			Code:     "25P02",
			Message:  "current transaction is aborted, commands ignored until end of transaction block",
		}
	}

	// Resolve parameter OIDs when needed (already described, or binary params present).
	paramOIDs := ps.paramOIDs
	if len(paramOIDs) < numParams {
		if hasBinaryParam || !ps.described {
			var err error
			paramOIDs, err = c.resolveParamOIDs(ctx, ps)
			if err != nil {
				c.updateTxStatusOnError(ps.txInfo, err)
				return err
			}
		}
	}

	args := make([]driver.NamedValue, numParams)
	for i := range numParams {
		fmtCode := paramFormatForIndex(paramFormats, i)
		var oid uint32
		if i < len(paramOIDs) {
			oid = paramOIDs[i]
		} else if i < len(ps.clientParamOIDs) {
			oid = ps.clientParamOIDs[i]
		}
		val, err := decodeParam(oid, fmtCode, rawParams[i])
		if err != nil {
			c.updateTxStatusOnError(ps.txInfo, err)
			return err
		}
		args[i] = driver.NamedValue{Ordinal: i + 1, Value: val}
	}

	if old, exists := c.portals[portalName]; exists {
		_ = old.close()
	}
	c.portals[portalName] = &portal{
		name:          portalName,
		stmt:          ps,
		args:          args,
		resultFormats: resultFormats,
	}
	return c.writeFrame(encodeBindComplete())
}

func paramFormatForIndex(formats []int16, idx int) int16 {
	if len(formats) == 0 {
		return formatText
	}
	if len(formats) == 1 {
		return formats[0]
	}
	if idx >= 0 && idx < len(formats) {
		return formats[idx]
	}
	return formatText
}

func (c *clientConn) handleDescribe(ctx context.Context, body []byte) error {
	if len(body) < 2 {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Describe message"}
	}
	targetType := body[0]
	name, _, ok := readCString(body, 1)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Describe message: missing name"}
	}

	switch targetType {
	case 'S':
		ps, exists := c.stmts[name]
		if !exists {
			return &wireError{
				Severity: "ERROR",
				Code:     "26000",
				Message:  fmt.Sprintf("prepared statement %q does not exist", name),
			}
		}
		if err := c.describeStatement(ctx, ps, nil); err != nil {
			c.updateTxStatusOnError(ps.txInfo, err)
			return err
		}
		if err := c.writeFrame(encodeParameterDescription(ps.paramOIDs)); err != nil {
			return err
		}
		if !ps.hasColumns {
			return c.writeFrame(encodeNoData())
		}
		return c.writeFrame(encodeRowDescription(ps.columns))

	case 'P':
		p, exists := c.portals[name]
		if !exists {
			return &wireError{
				Severity: "ERROR",
				Code:     "34000",
				Message:  fmt.Sprintf("portal %q does not exist", name),
			}
		}
		if !p.stmt.described {
			if err := c.describeStatement(ctx, p.stmt, p.args); err != nil {
				c.updateTxStatusOnError(p.stmt.txInfo, err)
				return err
			}
		}
		if !p.stmt.hasColumns {
			return c.writeFrame(encodeNoData())
		}
		cols := make([]columnDesc, len(p.stmt.columns))
		for i, col := range p.stmt.columns {
			col.FormatCode = p.resultFormat(i)
			cols[i] = col
		}
		return c.writeFrame(encodeRowDescription(cols))

	default:
		return &wireError{
			Severity: "ERROR",
			Code:     "08P01",
			Message:  fmt.Sprintf("invalid Describe target type %q", targetType),
		}
	}
}

func (c *clientConn) handleExecute(ctx context.Context, body []byte) error {
	portalName, pos, ok := readCString(body, 0)
	if !ok || pos+4 > len(body) {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Execute message"}
	}
	maxRows := int(int32(binary.BigEndian.Uint32(body[pos : pos+4])))
	p, exists := c.portals[portalName]
	if !exists {
		return &wireError{
			Severity: "ERROR",
			Code:     "34000",
			Message:  fmt.Sprintf("portal %q does not exist", portalName),
		}
	}
	if p.stmt.isEmpty {
		return c.writeFrame(encodeEmptyQueryResponse())
	}
	if p.completed {
		return c.writeFrame(encodeCommandComplete(formatRowCommandTag(p.stmt.verb, 0)))
	}

	if !p.stmt.returnsRows {
		execCtx, cancel := context.WithCancel(ctx)
		c.setActiveCancel(cancel)
		defer func() {
			c.setActiveCancel(nil)
			cancel()
		}()
		var res driver.Result
		var err error
		if stmtExec, ok := p.stmt.driverStmt.(driver.StmtExecContext); ok && p.stmt.driverStmt != nil {
			res, err = stmtExec.ExecContext(execCtx, p.args)
		} else if execer, ok := c.driverConn.(driver.ExecerContext); ok {
			res, err = execer.ExecContext(execCtx, p.stmt.sql, p.args)
		} else {
			return &wireError{Severity: "ERROR", Code: "XX000", Message: "driver connection does not support ExecContext"}
		}
		if err != nil {
			c.updateTxStatusOnError(p.stmt.txInfo, err)
			return err
		}
		p.started = true
		p.completed = true
		tag := extractCommandTag(res, p.stmt.raw)
		c.updateTxStatusOnSuccess(p.stmt.txInfo, tag)
		return c.writeFrame(encodeCommandComplete(tag))
	}

	// Row-returning portal: keep its cancelable context alive across
	// PortalSuspended ('s') calls until the portal finishes or is closed.
	if !p.started {
		p.ctx, p.cancel = context.WithCancel(ctx)
	}
	c.setActiveCancel(p.cancel)
	defer c.setActiveCancel(nil)

	if !p.started {
		var rows driver.Rows
		var err error
		if stmtQuery, ok := p.stmt.driverStmt.(driver.StmtQueryContext); ok && p.stmt.driverStmt != nil {
			rows, err = stmtQuery.QueryContext(p.ctx, p.args)
		} else if queryer, ok := c.driverConn.(driver.QueryerContext); ok {
			rows, err = queryer.QueryContext(p.ctx, p.stmt.sql, p.args)
		} else {
			_ = p.close()
			return &wireError{Severity: "ERROR", Code: "XX000", Message: "driver connection does not support QueryContext"}
		}
		if err != nil {
			_ = p.close()
			c.updateTxStatusOnError(p.stmt.txInfo, err)
			return err
		}
		p.rows = rows
		p.cols = extractRowColumns(rows, p.resultFormats)
		p.started = true
	}

	var emitted int
	nextRow := func() ([]driver.Value, bool, error) {
		if p.hasBuffered {
			row := p.bufferedRow
			p.bufferedRow = nil
			p.hasBuffered = false
			return row, true, nil
		}
		dest := make([]driver.Value, len(p.cols))
		if err := p.rows.Next(dest); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, false, nil
			}
			return nil, false, err
		}
		return dest, true, nil
	}

	for {
		if maxRows > 0 && emitted >= maxRows {
			// Peek one row ahead to distinguish PortalSuspended ('s') from
			// CommandComplete ('C').
			peek, ok, err := nextRow()
			if err != nil {
				_ = p.close()
				p.completed = true
				c.updateTxStatusOnError(p.stmt.txInfo, err)
				return err
			}
			if ok {
				p.bufferedRow = peek
				p.hasBuffered = true
				return c.writeFrame(encodePortalSuspended())
			}
			// Exhausted cleanly.
			break
		}
		row, ok, err := nextRow()
		if err != nil {
			_ = p.close()
			p.completed = true
			c.updateTxStatusOnError(p.stmt.txInfo, err)
			return err
		}
		if !ok {
			break
		}
		encoded, err := encodeRowFields(p.cols, row)
		if err != nil {
			_ = p.close()
			p.completed = true
			c.updateTxStatusOnError(p.stmt.txInfo, err)
			return err
		}
		if err := c.writeFrame(encodeDataRow(encoded)); err != nil {
			_ = p.close()
			p.completed = true
			return err
		}
		emitted++
		p.totalRows++
	}

	closeErr := p.close()
	p.completed = true
	if closeErr != nil {
		c.updateTxStatusOnError(p.stmt.txInfo, closeErr)
		return closeErr
	}
	tag := formatRowCommandTag(p.stmt.verb, p.totalRows)
	c.updateTxStatusOnSuccess(p.stmt.txInfo, tag)
	return c.writeFrame(encodeCommandComplete(tag))
}

func (c *clientConn) handleClose(body []byte) error {
	if len(body) < 2 {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Close message"}
	}
	targetType := body[0]
	name, _, ok := readCString(body, 1)
	if !ok {
		return &wireError{Severity: "ERROR", Code: "08P01", Message: "malformed Close message: missing name"}
	}
	switch targetType {
	case 'S':
		if ps, exists := c.stmts[name]; exists {
			ps.close()
			delete(c.stmts, name)
		}
	case 'P':
		if p, exists := c.portals[name]; exists {
			_ = p.close()
			delete(c.portals, name)
		}
	default:
		return &wireError{
			Severity: "ERROR",
			Code:     "08P01",
			Message:  fmt.Sprintf("invalid Close target type %q", targetType),
		}
	}
	return c.writeFrame(encodeCloseComplete())
}

// --- Statement Describe & Parameter OID Resolution ---

func (c *clientConn) resolveParamOIDs(ctx context.Context, ps *preparedStmt) ([]uint32, error) {
	count := max(len(ps.clientParamOIDs), ps.analysis.maxParam)
	if count == 0 {
		return nil, nil
	}
	if count < 0 || count > maxWireParams {
		return nil, tooManyParametersError(count)
	}
	oids := make([]uint32, count)
	for i := range count {
		if i < len(ps.clientParamOIDs) && ps.clientParamOIDs[i] != 0 {
			oids[i] = ps.clientParamOIDs[i]
			continue
		}
		if explicit, ok := ps.analysis.explicitOIDs[i+1]; ok && explicit != 0 {
			oids[i] = explicit
		}
	}

	// Resolve remaining parameters using table column hints when available.
	if len(ps.analysis.colHints) > 0 && c.txStatus != txStatusFailed {
		tableCache := make(map[string][]columnDesc)
		for pnum, hint := range ps.analysis.colHints {
			if pnum < 1 || pnum > count || oids[pnum-1] != 0 {
				continue
			}
			key := hint.table.qualifiedSQL()
			cols, cached := tableCache[key]
			if !cached {
				cols = c.probeTableColumns(ctx, key)
				tableCache[key] = cols
			}
			if len(cols) == 0 {
				continue
			}
			if hint.colName != "" {
				for _, col := range cols {
					if strings.EqualFold(col.Name, hint.colName) {
						oids[pnum-1] = col.TypeOID
						break
					}
				}
			} else if hint.colIndex >= 0 && hint.colIndex < len(cols) {
				oids[pnum-1] = cols[hint.colIndex].TypeOID
			}
		}
	}
	return oids, nil
}

func (c *clientConn) probeTableColumns(ctx context.Context, qualifiedTable string) []columnDesc {
	cols, err := c.runProtectedProbe(ctx, "SELECT * FROM "+qualifiedTable+" WHERE false", nil)
	if err != nil {
		return nil
	}
	return cols
}

func (c *clientConn) describeStatement(ctx context.Context, ps *preparedStmt, boundArgs []driver.NamedValue) error {
	if ps.isEmpty {
		ps.paramOIDs = nil
		ps.columns = nil
		ps.hasColumns = false
		ps.described = true
		return nil
	}
	if c.txStatus == txStatusFailed && !isTxRecoveryStmt(ps.txInfo) {
		return &wireError{
			Severity: "ERROR",
			Code:     "25P02",
			Message:  "current transaction is aborted, commands ignored until end of transaction block",
		}
	}

	paramOIDs, err := c.resolveParamOIDs(ctx, ps)
	if err != nil {
		return err
	}
	ps.paramOIDs = paramOIDs

	if !ps.returnsRows {
		ps.columns = nil
		ps.hasColumns = false
		ps.described = true
		return nil
	}

	staticCols, probeSQL, ok := buildDescribeProbe(ps.raw, ps.sql)
	if !ok {
		ps.columns = nil
		ps.hasColumns = false
		ps.described = true
		return nil
	}
	if staticCols != nil {
		ps.columns = staticCols
		ps.hasColumns = len(staticCols) > 0
		ps.described = true
		return nil
	}

	// Only pass parameters actually referenced by probeSQL (e.g. a synthetic
	// SELECT <returning> FROM <table> WHERE false probe for INSERT ... RETURNING
	// usually references 0 parameters even though the INSERT has $1..$N).
	probeParamCount := len(ps.paramOIDs)
	if probeRaw, parseErr := parseSingleStatement(probeSQL); parseErr == nil {
		probeAnalysis, err := analyzeParams(probeRaw)
		if err != nil {
			return err
		}
		probeParamCount = probeAnalysis.maxParam
	}
	if probeParamCount > maxWireParams {
		return tooManyParametersError(probeParamCount)
	}
	var probeOIDs []uint32
	if probeParamCount > 0 {
		probeOIDs = make([]uint32, probeParamCount)
		copy(probeOIDs, ps.paramOIDs)
	}
	var probeArgs []driver.NamedValue
	if probeParamCount > 0 {
		if len(boundArgs) >= probeParamCount {
			probeArgs = boundArgs[:probeParamCount]
		} else {
			probeArgs = buildProbeArgs(probeOIDs, false)
		}
	}
	cols, err := c.runProtectedProbe(ctx, probeSQL, probeArgs)
	if err != nil && boundArgs == nil && len(probeOIDs) > 0 {
		// Retry with empty-string fallback for completely untyped parameters
		// (e.g. "SELECT $1") so SuperSQL binds them as text like PostgreSQL.
		fallbackArgs := buildProbeArgs(probeOIDs, true)
		if retryCols, retryErr := c.runProtectedProbe(ctx, probeSQL, fallbackArgs); retryErr == nil {
			cols = retryCols
			err = nil
		}
	}
	if err != nil {
		return err
	}
	ps.columns = cols
	ps.hasColumns = len(cols) > 0
	ps.described = true
	return nil
}

func buildProbeArgs(paramOIDs []uint32, stringFallback bool) []driver.NamedValue {
	args := make([]driver.NamedValue, len(paramOIDs))
	for i, oid := range paramOIDs {
		var val driver.Value
		if stringFallback && oid == 0 {
			val = ""
		} else {
			val = nil
		}
		args[i] = driver.NamedValue{Ordinal: i + 1, Value: val}
	}
	return args
}

type queryDescriber interface {
	DescribeQuery(ctx context.Context, query string, args []driver.NamedValue) (names []string, oids []uint32, mods []int32, err error)
}

// runProtectedProbe extracts exact result column metadata for a Describe probe.
// When the driver connection supports bind-only DescribeQuery, it binds against
// the catalog snapshot without acquiring data-table locks (so Describe never
// blocks on or pollutes row locks in concurrent transactions). Otherwise it
// falls back to a 0-row SELECT probe wrapped in a SAVEPOINT inside active
// transactions.
func (c *clientConn) runProtectedProbe(ctx context.Context, probeSQL string, args []driver.NamedValue) ([]columnDesc, error) {
	if describer, ok := c.driverConn.(queryDescriber); ok {
		names, oids, mods, err := describer.DescribeQuery(ctx, probeSQL, args)
		if err != nil {
			return nil, err
		}
		cols := make([]columnDesc, len(names))
		for i, name := range names {
			var oid uint32 = oidText
			var mod int32 = -1
			if i < len(oids) && oids[i] != 0 {
				oid = oids[i]
				if i < len(mods) {
					mod = mods[i]
				}
			}
			if oid == oidUnknown {
				oid = oidText
			}
			cols[i] = columnDesc{
				Name:       name,
				TypeOID:    oid,
				TypeSize:   typeSize(oid),
				TypeMod:    mod,
				FormatCode: formatText,
			}
		}
		return cols, nil
	}
	queryer, ok := c.driverConn.(driver.QueryerContext)
	if !ok {
		return nil, &wireError{Severity: "ERROR", Code: "XX000", Message: "driver connection does not support QueryContext"}
	}
	execer, hasExec := c.driverConn.(driver.ExecerContext)
	useSavepoint := c.txStatus == txStatusInTx && hasExec
	if useSavepoint {
		if _, err := execer.ExecContext(ctx, "SAVEPOINT _pgwire_desc", nil); err != nil {
			useSavepoint = false
		}
	}
	rows, err := queryer.QueryContext(ctx, probeSQL, args)
	if err != nil {
		if useSavepoint {
			_, _ = execer.ExecContext(ctx, "ROLLBACK TO SAVEPOINT _pgwire_desc", nil)
			_, _ = execer.ExecContext(ctx, "RELEASE SAVEPOINT _pgwire_desc", nil)
		}
		return nil, err
	}
	cols := extractRowColumns(rows, nil)
	closeErr := rows.Close()
	if useSavepoint {
		_, _ = execer.ExecContext(ctx, "ROLLBACK TO SAVEPOINT _pgwire_desc", nil)
		_, _ = execer.ExecContext(ctx, "RELEASE SAVEPOINT _pgwire_desc", nil)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return cols, nil
}

// --- Row & CommandTag Helpers ---

type oidColumnProvider interface {
	ColumnOIDs() ([]uint32, []int32)
}

type singleOIDColumnProvider interface {
	ColumnTypeOID(index int) (uint32, int32)
}

type commandTagProvider interface {
	CommandTag() string
}

func extractRowColumns(rows driver.Rows, resultFormats []int16) []columnDesc {
	names := rows.Columns()
	cols := make([]columnDesc, len(names))
	var oids []uint32
	var mods []int32
	if p, ok := rows.(oidColumnProvider); ok {
		oids, mods = p.ColumnOIDs()
	}
	single, hasSingle := rows.(singleOIDColumnProvider)
	for i, name := range names {
		var oid uint32 = oidText
		var mod int32 = -1
		if i < len(oids) && oids[i] != 0 {
			oid = oids[i]
			if i < len(mods) {
				mod = mods[i]
			}
		} else if hasSingle {
			if sOID, sMod := single.ColumnTypeOID(i); sOID != 0 {
				oid = sOID
				mod = sMod
			}
		}
		if oid == oidUnknown {
			oid = oidText
		}
		fmtCode := formatText
		if len(resultFormats) == 1 {
			fmtCode = resultFormats[0]
		} else if i < len(resultFormats) {
			fmtCode = resultFormats[i]
		}
		cols[i] = columnDesc{
			Name:       name,
			TypeOID:    oid,
			TypeSize:   typeSize(oid),
			TypeMod:    mod,
			FormatCode: fmtCode,
		}
	}
	return cols
}

func encodeRowFields(cols []columnDesc, values []driver.Value) ([][]byte, error) {
	fields := make([][]byte, len(cols))
	for i, col := range cols {
		var v driver.Value
		if i < len(values) {
			v = values[i]
		}
		enc, err := encodeResultValue(col.TypeOID, col.FormatCode, v)
		if err != nil {
			return nil, err
		}
		fields[i] = enc
	}
	return fields, nil
}

func extractCommandTag(res driver.Result, raw *zpgparse.RawStmt) string {
	if res != nil {
		if tp, ok := res.(commandTagProvider); ok {
			if tag := strings.TrimSpace(tp.CommandTag()); tag != "" {
				return tag
			}
		}
		rows, _ := res.RowsAffected()
		verb := stmtCommandVerb(raw)
		return formatRowCommandTag(verb, rows)
	}
	return "OK"
}
