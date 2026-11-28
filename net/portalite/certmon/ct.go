package certmon

import (
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	certSpotterDefaultEndpoint = "https://api.certspotter.com/v1/issuances"
	crtShDefaultEndpoint       = "https://crt.sh/"
	maxCTResponseBytes         = 2 << 20
	maxCTPagesPerPoll          = 8
	maxCTRetryDelay            = 24 * time.Hour
	maxCrtShCertFetchesPerPoll = 16
)

var (
	precertPoisonOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 3}

	errCTResponseLimit = errors.New("certmon: CT response exceeds limit")
	errCTPageInvalid   = errors.New("certmon: invalid CT response page")
	errCTCursorStalled = errors.New("certmon: CT pagination cursor stalled")
	errCTPageBudget    = errors.New("certmon: CT page budget reached")
)

// CTEntry is one raw certificate issuance returned by a CTSource.
type CTEntry struct {
	ID        string
	TBSSHA256 string
	CertDER   []byte
}

// CTPage is one bounded page returned by CTSource.Poll.
type CTPage struct {
	Entries    []CTEntry
	NextCursor string
	Complete   bool
	RetryAfter time.Time
}

// CTSource abstracts a Certificate Transparency feed (e.g. CertSpotter, crt.sh,
// or a self-hosted CT watcher).
type CTSource interface {
	Name() string
	Poll(ctx context.Context, hostname string, cursor string) (CTPage, error)
}

func (m *monitor) pollCTSources(ctx context.Context, ev *monitorEvidence) error {
	if len(m.sources) == 0 {
		return nil
	}
	var errs []error
	for _, src := range m.sources {
		if src == nil {
			continue
		}
		err := m.pollOneCTSource(ctx, src, ev)
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
	}
	return errors.Join(errs...)
}

func (m *monitor) pollOneCTSource(ctx context.Context, src CTSource, ev *monitorEvidence) (result error) {
	name := src.Name()
	state := ev.Sources[name]
	now := time.Now().UTC()
	if !state.NextCheck.IsZero() && now.Before(state.NextCheck) {
		return fmt.Errorf("rate-limited until %s", state.NextCheck.Format(time.RFC3339))
	}
	defer func() {
		if result != nil {
			state.CheckedAt = time.Now().UTC()
			state.Error = result.Error()
			ev.Sources[name] = state
			_ = m.saveEvidence(*ev)
		}
	}()

	seenCursors := map[string]bool{}
	for range maxCTPagesPerPoll {
		seenCursors[state.Cursor] = true
		page, err := src.Poll(ctx, m.hostname, state.Cursor)
		if err != nil {
			if !page.RetryAfter.IsZero() {
				state.NextCheck = boundRetryAfter(page.RetryAfter, time.Now().UTC())
			}
			return err
		}
		pollNow := time.Now().UTC()
		nextCerts := make(map[string]CTObservation, len(ev.Certificates)+len(page.Entries))
		for k, v := range ev.Certificates {
			nextCerts[k] = v
		}
		for _, entry := range page.Entries {
			obs, err := decodeCTEntry(name, m.hostname, entry, pollNow)
			if err != nil {
				return err
			}
			if pollNow.Before(obs.NotAfter) {
				if old, ok := nextCerts[obs.SHA256]; ok {
					obs.FirstSeen = old.FirstSeen
				}
				nextCerts[obs.SHA256] = obs
			}
		}

		nextState := state
		nextState.CheckedAt = pollNow
		nextState.Error = ""
		nextState.NextCheck = boundRetryAfter(page.RetryAfter, pollNow)
		if page.Complete || len(page.Entries) == 0 {
			nextState.Cursor = ""
			nextState.LastComplete = pollNow
		} else {
			if page.NextCursor == "" || seenCursors[page.NextCursor] {
				return errCTCursorStalled
			}
			nextState.Cursor = page.NextCursor
		}

		nextEv := *ev
		nextEv.Certificates = nextCerts
		nextEv.Sources = make(map[string]ctSourceState, len(ev.Sources)+1)
		for k, v := range ev.Sources {
			nextEv.Sources[k] = v
		}
		nextEv.Sources[name] = nextState
		if err := m.saveEvidence(nextEv); err != nil {
			return err
		}
		*ev = nextEv
		state = nextState

		if page.Complete || len(page.Entries) == 0 || (!state.NextCheck.IsZero() && pollNow.Before(state.NextCheck)) {
			return nil
		}
	}
	return errCTPageBudget
}

func decodeCTEntry(source, hostname string, entry CTEntry, now time.Time) (CTObservation, error) {
	if entry.ID == "" || len(entry.ID) > 256 {
		return CTObservation{}, errCTPageInvalid
	}
	cert, err := x509.ParseCertificate(entry.CertDER)
	if err != nil {
		return CTObservation{}, err
	}
	covers := cert.VerifyHostname(hostname) == nil
	precert := false
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(precertPoisonOID) {
			precert = true
			break
		}
	}
	return CTObservation{
		Source:         source,
		SourceID:       entry.ID,
		TBSSHA256:      entry.TBSSHA256,
		CertificateDER: append([]byte(nil), entry.CertDER...),
		SHA256:         sha256Hex(cert.Raw),
		SPKISHA256:     sha256Hex(cert.RawSubjectPublicKeyInfo),
		DNSNames:       append([]string(nil), cert.DNSNames...),
		CoversHostname: covers,
		Precertificate: precert,
		NotBefore:      cert.NotBefore,
		NotAfter:       cert.NotAfter,
		FirstSeen:      now,
		LastSeen:       now,
	}, nil
}

// CertSpotterSource queries SSLMate's CertSpotter v1 issuances API.
type CertSpotterSource struct {
	Endpoint string
	Token    string
	Client   *http.Client
}

// NewCertSpotterSource returns a CertSpotter CTSource with an optional Bearer token.
func NewCertSpotterSource(client *http.Client, token string) *CertSpotterSource {
	return &CertSpotterSource{
		Endpoint: certSpotterDefaultEndpoint,
		Token:    strings.TrimSpace(token),
		Client:   client,
	}
}

func (s *CertSpotterSource) Name() string { return "certspotter" }

type certSpotterIssuance struct {
	ID        string `json:"id"`
	TBSSHA256 string `json:"tbs_sha256"`
	CertDER   []byte `json:"cert_der"`
}

func (s *CertSpotterSource) Poll(ctx context.Context, hostname string, cursor string) (CTPage, error) {
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = certSpotterDefaultEndpoint
	}
	q := url.Values{
		"domain":          {hostname},
		"match_wildcards": {"true"},
		"expand":          {"cert_der"},
	}
	if cursor != "" {
		q.Set("after", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return CTPage{}, err
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return CTPage{}, err
	}
	defer resp.Body.Close()
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
	if resp.StatusCode != http.StatusOK {
		return CTPage{RetryAfter: retryAfter}, fmt.Errorf("certspotter returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCTResponseBytes+1))
	if err != nil {
		return CTPage{RetryAfter: retryAfter}, err
	}
	if len(data) > maxCTResponseBytes {
		return CTPage{RetryAfter: retryAfter}, errCTResponseLimit
	}
	var issuances []certSpotterIssuance
	if err := json.Unmarshal(data, &issuances); err != nil {
		return CTPage{RetryAfter: retryAfter}, err
	}
	if issuances == nil || len(issuances) > 1000 {
		return CTPage{RetryAfter: retryAfter}, errCTPageInvalid
	}
	entries := make([]CTEntry, 0, len(issuances))
	lastID := ""
	for _, item := range issuances {
		entries = append(entries, CTEntry{
			ID:        item.ID,
			TBSSHA256: item.TBSSHA256,
			CertDER:   item.CertDER,
		})
		lastID = item.ID
	}
	return CTPage{
		Entries:    entries,
		NextCursor: lastID,
		Complete:   len(entries) == 0,
		RetryAfter: retryAfter,
	}, nil
}

// CrtShSource queries crt.sh JSON index as a fallback CT source and fetches DER
// for newly observed unexpired certificate IDs.
type CrtShSource struct {
	Endpoint string
	Client   *http.Client
}

// NewCrtShSource returns a crt.sh fallback CTSource.
func NewCrtShSource(client *http.Client) *CrtShSource {
	return &CrtShSource{
		Endpoint: crtShDefaultEndpoint,
		Client:   client,
	}
}

func (s *CrtShSource) Name() string { return "crt.sh" }

type crtShRow struct {
	ID       int64  `json:"id"`
	NotAfter string `json:"not_after"`
}

func (s *CrtShSource) Poll(ctx context.Context, hostname string, cursor string) (CTPage, error) {
	endpoint := strings.TrimRight(s.Endpoint, "/")
	if endpoint == "" {
		endpoint = strings.TrimRight(crtShDefaultEndpoint, "/")
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	q := url.Values{
		"q":       {hostname},
		"output":  {"json"},
		"exclude": {"expired"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/?"+q.Encode(), nil)
	if err != nil {
		return CTPage{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return CTPage{}, err
	}
	defer resp.Body.Close()
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
	if resp.StatusCode != http.StatusOK {
		return CTPage{RetryAfter: retryAfter}, fmt.Errorf("crt.sh returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxCTResponseBytes+1))
	if err != nil {
		return CTPage{RetryAfter: retryAfter}, err
	}
	if len(data) > maxCTResponseBytes {
		return CTPage{RetryAfter: retryAfter}, errCTResponseLimit
	}
	var rows []crtShRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return CTPage{RetryAfter: retryAfter}, err
	}

	var minID int64
	if cursor != "" {
		minID, _ = strconv.ParseInt(cursor, 10, 64)
	}
	seen := map[int64]bool{}
	var maxID int64 = minID
	entries := make([]CTEntry, 0, min(len(rows), maxCrtShCertFetchesPerPoll))
	for _, row := range rows {
		if row.ID <= minID || seen[row.ID] {
			continue
		}
		seen[row.ID] = true
		if len(entries) >= maxCrtShCertFetchesPerPoll {
			break
		}
		der, err := s.fetchDER(ctx, client, endpoint, row.ID)
		if err != nil {
			return CTPage{RetryAfter: retryAfter}, err
		}
		entries = append(entries, CTEntry{
			ID:      strconv.FormatInt(row.ID, 10),
			CertDER: der,
		})
		if row.ID > maxID {
			maxID = row.ID
		}
	}
	return CTPage{
		Entries:    entries,
		NextCursor: strconv.FormatInt(maxID, 10),
		Complete:   true,
		RetryAfter: retryAfter,
	}, nil
}

func (s *CrtShSource) fetchDER(ctx context.Context, client *http.Client, endpoint string, id int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/?d=%d", endpoint, id), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crt.sh download %d returned HTTP %d", id, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalDERBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLocalDERBytes {
		return nil, errCTResponseLimit
	}
	// crt.sh /?d=<id> returns PEM; decode to DER if PEM-encoded.
	if len(raw) > 0 && raw[0] == '-' {
		return decodePEMCert(raw)
	}
	return raw, nil
}
func decodePEMCert(pemData []byte) ([]byte, error) {
	for len(pemData) > 0 {
		block, rest := pem.Decode(pemData)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			return block.Bytes, nil
		}
		pemData = rest
	}
	return nil, errCTPageInvalid
}
func parseRetryAfter(header string, now time.Time) time.Time {
	h := strings.TrimSpace(header)
	if h == "" {
		return time.Time{}
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return now.Add(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(h); err == nil && t.After(now) {
		return t.UTC()
	}
	return time.Time{}
}

func boundRetryAfter(when, checked time.Time) time.Time {
	if when.IsZero() {
		return time.Time{}
	}
	maxAt := checked.Add(maxCTRetryDelay)
	if when.After(maxAt) {
		return maxAt
	}
	return when
}
