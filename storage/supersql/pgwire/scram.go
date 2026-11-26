package pgwire

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	scramMechanismSHA256   = "SCRAM-SHA-256"
	scramDefaultIterations = 4096
	scramSaltLen           = 16
	scramServerNonceBytes  = 18
	scramMaxNonceLen       = 256
	scramMaxIterations     = 1 << 24
)

type scramVerifier struct {
	iterations int
	salt       []byte
	storedKey  []byte
	serverKey  []byte
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func deriveSCRAMVerifier(password string) (scramVerifier, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return scramVerifier{}, fmt.Errorf("pgwire: generate SCRAM salt: %w", err)
	}
	saltedPassword, err := pbkdf2.Key(sha256.New, password, salt, scramDefaultIterations, sha256.Size)
	if err != nil {
		return scramVerifier{}, fmt.Errorf("pgwire: derive SCRAM salted password: %w", err)
	}
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKeySum := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	return scramVerifier{
		iterations: scramDefaultIterations,
		salt:       salt,
		storedKey:  storedKeySum[:],
		serverKey:  serverKey,
	}, nil
}

func dummySCRAMVerifier() (scramVerifier, error) {
	salt := make([]byte, scramSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return scramVerifier{}, fmt.Errorf("pgwire: generate dummy SCRAM salt: %w", err)
	}
	storedKey := make([]byte, sha256.Size)
	if _, err := rand.Read(storedKey); err != nil {
		return scramVerifier{}, fmt.Errorf("pgwire: generate dummy SCRAM stored key: %w", err)
	}
	serverKey := make([]byte, sha256.Size)
	if _, err := rand.Read(serverKey); err != nil {
		return scramVerifier{}, fmt.Errorf("pgwire: generate dummy SCRAM server key: %w", err)
	}
	return scramVerifier{
		iterations: scramDefaultIterations,
		salt:       salt,
		storedKey:  storedKey,
		serverKey:  serverKey,
	}, nil
}

// parseSCRAMVerifier parses a PostgreSQL SCRAM-SHA-256 verifier string of the form
// "SCRAM-SHA-256$<iterations>:<b64salt>$<b64storedKey>:<b64serverKey>".
// It returns (verifier, true, nil) when secret is a valid verifier,
// (zero, false, nil) when secret does not have the SCRAM-SHA-256$ prefix
// (indicating a plaintext password), or (zero, true, err) when secret carries
// the prefix but is malformed.
func parseSCRAMVerifier(secret string) (scramVerifier, bool, error) {
	const prefix = scramMechanismSHA256 + "$"
	if !strings.HasPrefix(secret, prefix) {
		return scramVerifier{}, false, nil
	}
	rest := secret[len(prefix):]
	iterSalt, keys, ok := strings.Cut(rest, "$")
	if !ok || strings.Contains(keys, "$") {
		return scramVerifier{}, true, errors.New("malformed SCRAM-SHA-256 verifier")
	}
	iterStr, saltB64, ok := strings.Cut(iterSalt, ":")
	if !ok || strings.Contains(saltB64, ":") {
		return scramVerifier{}, true, errors.New("malformed SCRAM-SHA-256 iteration/salt")
	}
	iterations, err := strconv.Atoi(iterStr)
	if err != nil || iterations < 1 || iterations > scramMaxIterations {
		return scramVerifier{}, true, errors.New("invalid SCRAM-SHA-256 iteration count")
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil || len(salt) == 0 {
		return scramVerifier{}, true, errors.New("invalid SCRAM-SHA-256 salt")
	}
	storedB64, serverB64, ok := strings.Cut(keys, ":")
	if !ok || strings.Contains(serverB64, ":") {
		return scramVerifier{}, true, errors.New("malformed SCRAM-SHA-256 key pair")
	}
	storedKey, err := base64.StdEncoding.DecodeString(storedB64)
	if err != nil || len(storedKey) != sha256.Size {
		return scramVerifier{}, true, errors.New("invalid SCRAM-SHA-256 stored key")
	}
	serverKey, err := base64.StdEncoding.DecodeString(serverB64)
	if err != nil || len(serverKey) != sha256.Size {
		return scramVerifier{}, true, errors.New("invalid SCRAM-SHA-256 server key")
	}
	return scramVerifier{
		iterations: iterations,
		salt:       salt,
		storedKey:  storedKey,
		serverKey:  serverKey,
	}, true, nil
}

func isPrintableSCRAMNonce(s string) bool {
	if len(s) == 0 || len(s) > scramMaxNonceLen {
		return false
	}
	for i := range len(s) {
		b := s[i]
		if b < 0x21 || b > 0x7E || b == ',' {
			return false
		}
	}
	return true
}

func parseSCRAMClientFirst(msg string) (gs2Header, clientFirstBare, clientNonce string, err error) {
	switch {
	case strings.HasPrefix(msg, "n,,"):
		gs2Header = "n,,"
		clientFirstBare = msg[3:]
	case strings.HasPrefix(msg, "y,,"):
		gs2Header = "y,,"
		clientFirstBare = msg[3:]
	case strings.HasPrefix(msg, "p="):
		return "", "", "", errors.New("SCRAM channel binding is not supported")
	default:
		return "", "", "", errors.New("invalid SCRAM GS2 header")
	}

	parts := strings.Split(clientFirstBare, ",")
	if len(parts) < 2 {
		return "", "", "", errors.New("malformed SCRAM client-first-message")
	}
	if strings.HasPrefix(parts[0], "m=") {
		return "", "", "", errors.New("unsupported SCRAM mandatory extension")
	}
	if !strings.HasPrefix(parts[0], "n=") {
		return "", "", "", errors.New("missing SCRAM username attribute")
	}
	if !strings.HasPrefix(parts[1], "r=") {
		return "", "", "", errors.New("missing SCRAM client nonce")
	}
	clientNonce = parts[1][2:]
	if !isPrintableSCRAMNonce(clientNonce) {
		return "", "", "", errors.New("invalid SCRAM client nonce")
	}
	for _, ext := range parts[2:] {
		if len(ext) < 2 || ext[1] != '=' {
			return "", "", "", errors.New("malformed SCRAM extension")
		}
		k := ext[0]
		if !((k >= 'a' && k <= 'z') || (k >= 'A' && k <= 'Z')) {
			return "", "", "", errors.New("invalid SCRAM extension attribute")
		}
		if k == 'm' || k == 'n' || k == 'r' {
			return "", "", "", errors.New("duplicate or unsupported SCRAM attribute")
		}
	}
	return gs2Header, clientFirstBare, clientNonce, nil
}

func parseSCRAMClientFinal(msg string) (cbindB64, nonce, withoutProof string, proof []byte, err error) {
	parts := strings.Split(msg, ",")
	if len(parts) < 3 {
		return "", "", "", nil, errors.New("malformed SCRAM client-final-message")
	}
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "p=") {
		return "", "", "", nil, errors.New("missing SCRAM client proof")
	}
	proof, err = base64.StdEncoding.DecodeString(last[2:])
	if err != nil || len(proof) != sha256.Size {
		return "", "", "", nil, errors.New("invalid SCRAM client proof")
	}
	if !strings.HasPrefix(parts[0], "c=") {
		return "", "", "", nil, errors.New("missing SCRAM channel binding attribute")
	}
	cbindB64 = parts[0][2:]
	if !strings.HasPrefix(parts[1], "r=") {
		return "", "", "", nil, errors.New("missing SCRAM combined nonce")
	}
	nonce = parts[1][2:]
	withoutProof = msg[:len(msg)-len(last)-1]
	return cbindB64, nonce, withoutProof, proof, nil
}

func (c *clientConn) readAuthFrame() (byte, []byte, error) {
	rc := c.currentConn()
	if c.server.readTimeout > 0 && rc != nil {
		_ = rc.SetReadDeadline(time.Now().Add(c.server.readTimeout))
		defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	}
	msgType, body, err := readFrontendMessageWithLimit(c.br, maxPasswordMessageBytes)
	if err != nil {
		var we *wireError
		if errors.As(err, &we) {
			_ = c.writeAndFlush(encodeErrorResponse(*we))
		}
		return 0, nil, err
	}
	return msgType, body, nil
}

func (c *clientConn) failAuth(code, msg string) error {
	_ = c.writeAndFlush(encodeErrorResponse(wireError{
		Severity: "FATAL",
		Code:     code,
		Message:  msg,
	}))
	return errors.New(msg)
}

func (c *clientConn) authenticateSCRAM(ctx context.Context, lookup func(ctx context.Context, user, database string) (string, error)) error {
	// 1. Advertise SCRAM-SHA-256 via AuthenticationSASL.
	if err := c.writeAndFlush(encodeAuthSASL(scramMechanismSHA256)); err != nil {
		return err
	}

	// 2. Read SASLInitialResponse ('p').
	msgType, body, err := c.readAuthFrame()
	if err != nil {
		return err
	}
	if msgType != 'p' {
		return c.failAuth("08P01", "expected SASLInitialResponse")
	}
	mech, pos, ok := readCString(body, 0)
	if !ok {
		return c.failAuth("08P01", "malformed SASLInitialResponse")
	}
	if mech != scramMechanismSHA256 {
		return c.failAuth("08P01", fmt.Sprintf("unsupported SASL authentication mechanism %q", mech))
	}
	if pos+4 > len(body) {
		return c.failAuth("08P01", "malformed SASLInitialResponse length")
	}
	respLen := int(int32(binary.BigEndian.Uint32(body[pos : pos+4])))
	pos += 4
	if respLen <= 0 || pos+respLen != len(body) {
		return c.failAuth("08P01", "malformed SASLInitialResponse payload")
	}

	gs2Header, clientFirstBare, clientNonce, err := parseSCRAMClientFirst(string(body[pos : pos+respLen]))
	if err != nil {
		return c.failAuth("08P01", err.Error())
	}

	// 3. Look up expected credential or synthesize a dummy challenge on lookup
	// failure to prevent user enumeration.
	var (
		verifier     scramVerifier
		lookupFailed bool
	)
	secret, lookupErr := lookup(ctx, c.user, c.database)
	if lookupErr != nil {
		lookupFailed = true
	} else {
		v, isVerifier, parseErr := parseSCRAMVerifier(secret)
		if parseErr != nil {
			lookupFailed = true
		} else if isVerifier {
			verifier = v
		} else {
			v, deriveErr := deriveSCRAMVerifier(secret)
			if deriveErr != nil {
				return c.failAuth("XX000", "internal authentication error")
			}
			verifier = v
		}
	}
	if lookupFailed {
		v, dummyErr := dummySCRAMVerifier()
		if dummyErr != nil {
			return c.failAuth("XX000", "internal authentication error")
		}
		verifier = v
	}

	// 4. Send server-first-message via AuthenticationSASLContinue.
	nonceBytes := make([]byte, scramServerNonceBytes)
	if _, err := rand.Read(nonceBytes); err != nil {
		return c.failAuth("XX000", "internal authentication error")
	}
	combinedNonce := clientNonce + base64.StdEncoding.EncodeToString(nonceBytes)
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d",
		combinedNonce,
		base64.StdEncoding.EncodeToString(verifier.salt),
		verifier.iterations,
	)
	if err := c.writeAndFlush(encodeAuthSASLContinue(serverFirst)); err != nil {
		return err
	}

	// 5. Read SASLResponse ('p').
	msgType, body, err = c.readAuthFrame()
	if err != nil {
		return err
	}
	if msgType != 'p' {
		return c.failAuth("08P01", "expected SASLResponse")
	}
	cbindB64, nonce, clientFinalWithoutProof, clientProof, err := parseSCRAMClientFinal(string(body))
	if err != nil {
		return c.failAuth("08P01", err.Error())
	}
	expectedCBind := base64.StdEncoding.EncodeToString([]byte(gs2Header))
	if cbindB64 != expectedCBind {
		return c.failAuth("08P01", "SCRAM channel-binding mismatch")
	}
	if nonce != combinedNonce {
		return c.failAuth("08P01", "SCRAM nonce mismatch")
	}

	// 6. Verify ClientProof in constant time.
	authMessage := clientFirstBare + "," + serverFirst + "," + clientFinalWithoutProof
	clientSignature := hmacSHA256(verifier.storedKey, []byte(authMessage))
	var recoveredClientKey [sha256.Size]byte
	for i := range recoveredClientKey {
		recoveredClientKey[i] = clientProof[i] ^ clientSignature[i]
	}
	recoveredStoredKey := sha256.Sum256(recoveredClientKey[:])
	proofValid := subtle.ConstantTimeCompare(recoveredStoredKey[:], verifier.storedKey) == 1
	if lookupFailed || !proofValid {
		return c.failAuth("28P01", fmt.Sprintf("password authentication failed for user %q", c.user))
	}

	// 7. Send AuthenticationSASLFinal with ServerSignature followed by AuthenticationOk.
	serverSignature := hmacSHA256(verifier.serverKey, []byte(authMessage))
	serverFinal := "v=" + base64.StdEncoding.EncodeToString(serverSignature)
	if err := c.writeFrame(encodeAuthSASLFinal(serverFinal)); err != nil {
		return err
	}
	return c.writeFrame(encodeAuthMessage(authTypeOk))
}
