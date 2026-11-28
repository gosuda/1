package portalite

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	registerLeaseSIWEStatement  = "Register a portal lease"
	maxRegisterSIWEMessageBytes = 4 << 10
	maxRegisterSIWETimeSkew     = time.Minute
	maxRegisterSIWELifetime     = 5 * time.Minute

	siweVersion = "1"
	siweChainID = 1
)

var errInvalidRegisterSIWEChallenge = errors.New("relay returned an invalid registration SIWE challenge")

type siweMessage struct {
	Domain    string
	Address   string
	URI       string
	Statement string
	Nonce     string
	RequestID string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// validateRegisterSIWEChallenge binds a relay challenge to the registration
// signing purpose, relying party, identity, and destination before personal
// signing so a malicious relay cannot trick the identity key into signing an
// arbitrary off-protocol SIWE message.
func validateRegisterSIWEChallenge(challenge challengeResponse, relayURL *url.URL, address string, now time.Time) error {
	if strings.TrimSpace(challenge.ChallengeID) == "" || !now.Before(challenge.ExpiresAt) {
		return errInvalidRegisterSIWEChallenge
	}
	if relayURL == nil || relayURL.Hostname() == "" || relayURL.User != nil ||
		(relayURL.Scheme != "https" && relayURL.Scheme != "http") {
		return errInvalidRegisterSIWEChallenge
	}
	if len(challenge.SIWEMessage) > maxRegisterSIWEMessageBytes {
		return errInvalidRegisterSIWEChallenge
	}

	lines := strings.Split(challenge.SIWEMessage, "\n")
	if len(lines) != 12 {
		return errInvalidRegisterSIWEChallenge
	}
	nonce, ok := strings.CutPrefix(lines[8], "Nonce: ")
	if !ok {
		return errInvalidRegisterSIWEChallenge
	}
	issuedAtText, ok := strings.CutPrefix(lines[9], "Issued At: ")
	if !ok {
		return errInvalidRegisterSIWEChallenge
	}
	issuedAt, err := time.Parse(time.RFC3339, issuedAtText)
	if err != nil {
		return errInvalidRegisterSIWEChallenge
	}
	expiresAt := challenge.ExpiresAt.Truncate(time.Second)
	if issuedAt.After(now.Add(maxRegisterSIWETimeSkew)) ||
		expiresAt.Before(issuedAt) ||
		expiresAt.Sub(issuedAt) > maxRegisterSIWELifetime ||
		expiresAt.After(now.Add(maxRegisterSIWELifetime)) {
		return errInvalidRegisterSIWEChallenge
	}

	registerURI := (&url.URL{
		Scheme: relayURL.Scheme,
		Host:   relayURL.Host,
		Path:   pathRegister,
	}).String()
	expected, err := formatSIWEMessage(siweMessage{
		Domain:    relayURL.Host,
		Address:   address,
		URI:       registerURI,
		Statement: registerLeaseSIWEStatement,
		Nonce:     nonce,
		RequestID: challenge.ChallengeID,
		IssuedAt:  issuedAt,
		ExpiresAt: challenge.ExpiresAt,
	})
	if err != nil || expected != challenge.SIWEMessage {
		return errInvalidRegisterSIWEChallenge
	}
	return nil
}

func formatSIWEMessage(m siweMessage) (string, error) {
	if err := validateSIWEDomain(m.Domain); err != nil {
		return "", err
	}
	parsedURI, err := url.Parse(m.URI)
	if err != nil || !parsedURI.IsAbs() {
		return "", errors.New("invalid siwe uri")
	}
	address, err := normalizeEthereumAddress(m.Address)
	if err != nil {
		return "", err
	}
	if err := validateSIWEFields(m); err != nil {
		return "", err
	}
	header := fmt.Sprintf("%s wants you to sign in with your Ethereum account:\n%s\n\n", m.Domain, address)
	if m.Statement != "" {
		header += m.Statement + "\n"
	}
	return fmt.Sprintf("%s\nURI: %s\nVersion: %s\nChain ID: %d\nNonce: %s\nIssued At: %s\nExpiration Time: %s\nRequest ID: %s",
		header,
		parsedURI.String(),
		siweVersion,
		siweChainID,
		m.Nonce,
		m.IssuedAt.UTC().Format(time.RFC3339),
		m.ExpiresAt.UTC().Format(time.RFC3339),
		m.RequestID,
	), nil
}

func validateSIWEDomain(raw string) error {
	domain, err := url.Parse("https://" + raw)
	if err != nil || domain.Host == "" {
		return errors.New("invalid siwe domain")
	}
	authority := domain.Host
	if domain.User != nil {
		authority = domain.User.String() + "@" + authority
	}
	if authority != raw {
		return errors.New("invalid siwe domain")
	}
	return nil
}

func validateSIWEFields(m siweMessage) error {
	if strings.IndexFunc(m.Statement, func(r rune) bool { return r < ' ' || r > '~' }) >= 0 {
		return errors.New("siwe statement must be a single ASCII line")
	}
	if len(m.Nonce) < 8 || strings.IndexFunc(m.Nonce, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) >= 0 {
		return errors.New("siwe nonce must contain at least 8 alphanumeric characters")
	}
	if strings.TrimSpace(m.RequestID) == "" || strings.IndexFunc(m.RequestID, func(r rune) bool { return r < '!' || r > '~' }) >= 0 {
		return errors.New("invalid siwe request ID")
	}
	if m.IssuedAt.IsZero() || m.ExpiresAt.IsZero() || !m.ExpiresAt.After(m.IssuedAt) {
		return errors.New("invalid siwe timestamps")
	}
	return nil
}
