// Package certmon implements independent, warning-only CAA (RFC 8659) and
// Certificate Transparency (CT) monitoring for portalite exposures.
//
// It plugs into portalite.ExposeConfig.CertObserver without coupling to
// private keys, TLS handshakes, or relay credentials.
package certmon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gosuda.org/portalite"
)

const (
	defaultDNSResolver   = "1.1.1.1:53"
	defaultPollInterval  = time.Hour
	maxLocalDERBytes     = 256 << 10
	maxLocalHistoryFiles = 4096
	maxBaselineBytes     = 64 << 10
	maxEvidenceBytes     = 64 << 20
	maxEvidenceCerts     = 10000
	maxCAAObservations   = 168
)

var (
	errStateDirRequired    = errors.New("certmon: state directory is required")
	errResolverInvalid     = errors.New("certmon: DNS resolver must be an IP:port")
	errHostnameInvalid     = errors.New("certmon: invalid DNS hostname")
	errBaselineUnconfigured = errors.New("certmon: CAA baseline is unconfigured")
	errBaselineRead        = errors.New("certmon: cannot read bounded baseline file")
	errBaselineInvalid     = errors.New("certmon: baseline must name the hostname and specify caa")
	errBaselineSPKIInvalid = errors.New("certmon: baseline SPKI hashes must be lowercase SHA-256 hex")
	errEvidenceLimit       = errors.New("certmon: evidence exceeds size or entry limit")
	errEvidenceInvalid     = errors.New("certmon: invalid evidence file")
	errLocalHistoryLimit   = errors.New("certmon: local certificate history exceeds limit")
	errLocalHistoryInvalid = errors.New("certmon: invalid local certificate history entry")
)

// WarningKind classifies observations emitted via Config.OnWarning.
type WarningKind string

const (
	WarningUnrecognizedSPKI WarningKind = "unrecognized_spki"
	WarningCAADrift         WarningKind = "caa_drift"
	WarningCAAUnavailable   WarningKind = "caa_unavailable"
	WarningCTUnavailable    WarningKind = "ct_unavailable"
	WarningBaseline         WarningKind = "baseline"
	WarningStorage          WarningKind = "storage"
)

// Warning is a structured observation delivered asynchronously to Config.OnWarning.
type Warning struct {
	Kind              WarningKind `json:"kind"`
	Hostname          string      `json:"hostname"`
	Source            string      `json:"source,omitempty"`
	Message           string      `json:"message"`
	SPKISHA256        string      `json:"spki_sha256,omitempty"`
	CertificateSHA256 string      `json:"certificate_sha256,omitempty"`
	Precertificate    bool        `json:"precertificate,omitempty"`
	Err               error       `json:"-"`
	ObservedAt        time.Time   `json:"observed_at"`
}

// Config configures CAA and CT monitoring.
type Config struct {
	// StateDir persists public evidence and local leaf DER history (never private keys).
	StateDir string
	// BaselineFile is an optional operator-maintained JSON policy reloaded every poll.
	// When empty, CAA drift detection is skipped and only raw observations are recorded.
	BaselineFile string
	// DNSResolver is an independent resolver IP:port (defaults to 1.1.1.1:53).
	DNSResolver string
	// CertSpotterToken is an optional Bearer token for the default CertSpotter CT source.
	CertSpotterToken string
	// CTSources overrides the default CT source chain (CertSpotter primary + crt.sh fallback).
	CTSources []CTSource
	// PollInterval controls how often CAA and CT are polled (defaults to 1 hour).
	PollInterval time.Duration
	// HTTPClient is an optional HTTP client used by default CT sources.
	HTTPClient *http.Client
	// OnWarning receives structured warning events. Must be non-blocking or fast.
	OnWarning func(Warning)
}

// Validate checks configuration without touching the filesystem or network.
func (c Config) Validate() error {
	if strings.TrimSpace(c.StateDir) == "" {
		return errStateDirRequired
	}
	if strings.TrimSpace(c.DNSResolver) != "" {
		addr, err := netip.ParseAddrPort(strings.TrimSpace(c.DNSResolver))
		if err != nil || addr.Port() == 0 {
			return errResolverInvalid
		}
	}
	return nil
}

// CAARecord represents one normalized RFC 8659 CAA resource record.
type CAARecord struct {
	Flags uint8  `json:"flags"`
	Tag   string `json:"tag"`
	Value string `json:"value"`
}

// Baseline is the operator-maintained expected CAA and SPKI policy file schema.
type Baseline struct {
	Hostname           string      `json:"hostname"`
	CAA                []CAARecord `json:"caa"`
	ApprovedSPKISHA256 []string    `json:"approved_spki_sha256,omitempty"`
}

// CAAObservation records one RFC 8659 CAA lookup attempt.
type CAAObservation struct {
	ObservedAt time.Time   `json:"observed_at"`
	Resolver   string      `json:"resolver"`
	Owner      string      `json:"owner,omitempty"`
	Records    []CAARecord `json:"records"`
	Error      string      `json:"error,omitempty"`
}

// CTObservation records one DER-verified certificate observed from a CT source.
type CTObservation struct {
	Source         string    `json:"source"`
	SourceID       string    `json:"source_id"`
	TBSSHA256      string    `json:"tbs_sha256,omitempty"`
	CertificateDER []byte    `json:"certificate_der"`
	SHA256         string    `json:"certificate_sha256"`
	SPKISHA256     string    `json:"spki_sha256"`
	DNSNames       []string  `json:"dns_names"`
	CoversHostname bool      `json:"covers_hostname"`
	Precertificate bool      `json:"precertificate"`
	NotBefore      time.Time `json:"not_before"`
	NotAfter       time.Time `json:"not_after"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
}

type ctSourceState struct {
	Cursor       string    `json:"cursor,omitempty"`
	CheckedAt    time.Time `json:"checked_at,omitempty"`
	NextCheck    time.Time `json:"next_check,omitempty"`
	LastComplete time.Time `json:"last_complete_at,omitempty"`
	Error        string    `json:"error,omitempty"`
}

type monitorEvidence struct {
	Hostname     string                   `json:"hostname"`
	Sources      map[string]ctSourceState `json:"ct_sources"`
	CAA          []CAAObservation         `json:"caa_observations"`
	Certificates map[string]CTObservation `json:"certificates"`
}

type factory struct {
	cfg Config
}

// NewFactory validates cfg and returns a portalite.CertObserverFactory.
func NewFactory(cfg Config) (portalite.CertObserverFactory, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &factory{cfg: cfg}, nil
}

func (f *factory) Start(hostname string) (portalite.CertObserver, error) {
	m, err := newMonitor(hostname, f.cfg)
	if err != nil {
		return nil, err
	}
	return m.start(), nil
}

// Observer is a running lease-scoped CAA/CT monitor implementing portalite.CertObserver.
type Observer struct {
	events chan []byte
	cancel context.CancelFunc
	done   chan struct{}
}

// RecordLocalLeafDER enqueues a copied local leaf certificate DER for SPKI tracking.
// Delivery is non-blocking; a dropped event can only cause a conservative warning.
func (o *Observer) RecordLocalLeafDER(der []byte) {
	if o == nil || len(der) == 0 {
		return
	}
	copied := append([]byte(nil), der...)
	select {
	case o.events <- copied:
	default:
	}
}

// Close cancels background polling and waits for the worker goroutine to exit.
func (o *Observer) Close() {
	if o == nil {
		return
	}
	o.cancel()
	<-o.done
}

type monitor struct {
	hostname     string
	dir          string
	cfg          Config
	resolver     string
	pollInterval time.Duration
	sources      []CTSource
	events       chan []byte
}

func newMonitor(hostname string, cfg Config) (*monitor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	host := normalizeASCIIHostname(hostname)
	if host == "" {
		return nil, errHostnameInvalid
	}
	resolver := strings.TrimSpace(cfg.DNSResolver)
	if resolver == "" {
		resolver = defaultDNSResolver
	}
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	sources := cfg.CTSources
	if len(sources) == 0 {
		sources = []CTSource{
			NewCertSpotterSource(httpClient, cfg.CertSpotterToken),
			NewCrtShSource(httpClient),
		}
	}
	return &monitor{
		hostname:     host,
		dir:          filepath.Join(cfg.StateDir, sha256Hex([]byte(host))),
		cfg:          cfg,
		resolver:     resolver,
		pollInterval: pollInterval,
		sources:      sources,
		events:       make(chan []byte, 16),
	}, nil
}

func (m *monitor) start() *Observer {
	ctx, cancel := context.WithCancel(context.Background())
	obs := &Observer{
		events: m.events,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(obs.done)
		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()
		m.poll(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case der := <-m.events:
				m.rememberEvent(der)
			case <-ticker.C:
				m.poll(ctx)
			}
		}
	}()
	return obs
}

func (m *monitor) emit(w Warning) {
	if m.cfg.OnWarning == nil {
		return
	}
	if w.Hostname == "" {
		w.Hostname = m.hostname
	}
	if w.ObservedAt.IsZero() {
		w.ObservedAt = time.Now().UTC()
	}
	m.cfg.OnWarning(w)
}

func (m *monitor) rememberEvent(der []byte) {
	if len(der) > maxLocalDERBytes {
		m.emit(Warning{Kind: WarningStorage, Message: "local public certificate exceeds history size limit"})
		return
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "invalid installed public certificate", Err: err})
		return
	}
	if err := cert.VerifyHostname(m.hostname); err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "installed public certificate does not cover hostname", Err: err})
		return
	}
	m.rememberLocalCertificate(cert)
}

func (m *monitor) drainEvents() {
	for range len(m.events) {
		m.rememberEvent(<-m.events)
	}
}

func (m *monitor) rememberLocalCertificate(cert *x509.Certificate) {
	if len(cert.Raw) > maxLocalDERBytes {
		return
	}
	dir := filepath.Join(m.dir, "local")
	path := filepath.Join(dir, sha256Hex(cert.Raw)+".der")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "cannot create local certificate history directory", Err: err})
		return
	}
	files, err := m.localHistoryFiles()
	if err != nil || len(files) >= maxLocalHistoryFiles {
		m.emit(Warning{Kind: WarningStorage, Message: "local public certificate history limit reached", Err: err})
		return
	}
	if err := writeFileAtomic(path, cert.Raw); err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "cannot persist local public certificate history", Err: err})
	}
}

func (m *monitor) localHistoryFiles() ([]os.DirEntry, error) {
	dir, err := os.Open(filepath.Join(m.dir, "local"))
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	files, err := dir.ReadDir(maxLocalHistoryFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(files) > maxLocalHistoryFiles {
		return nil, errLocalHistoryLimit
	}
	return files, nil
}

func (m *monitor) readBaseline() (*Baseline, error) {
	if strings.TrimSpace(m.cfg.BaselineFile) == "" {
		return nil, errBaselineUnconfigured
	}
	f, err := os.Open(m.cfg.BaselineFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBaselineBytes+1))
	if err != nil || len(data) > maxBaselineBytes {
		return nil, errBaselineRead
	}
	var baseline Baseline
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&baseline); err != nil {
		return nil, err
	}
	if dec.Decode(new(any)) != io.EOF || normalizeASCIIHostname(baseline.Hostname) != m.hostname || baseline.CAA == nil {
		return nil, errBaselineInvalid
	}
	baseline.CAA, err = canonicalCAA(baseline.CAA)
	if err != nil {
		return nil, err
	}
	for _, key := range baseline.ApprovedSPKISHA256 {
		if !validSHA256Hex(key) {
			return nil, errBaselineSPKIInvalid
		}
	}
	return &baseline, nil
}

func (m *monitor) loadEvidence() (monitorEvidence, error) {
	ev := monitorEvidence{
		Hostname:     m.hostname,
		Sources:      map[string]ctSourceState{},
		Certificates: map[string]CTObservation{},
	}
	f, err := os.Open(filepath.Join(m.dir, "evidence.json"))
	if errors.Is(err, os.ErrNotExist) {
		return ev, nil
	}
	if err != nil {
		return ev, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxEvidenceBytes+1))
	if err != nil || len(data) > maxEvidenceBytes {
		return ev, errEvidenceLimit
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return ev, err
	}
	if ev.Hostname != m.hostname || ev.Certificates == nil {
		return ev, errEvidenceInvalid
	}
	if ev.Sources == nil {
		ev.Sources = map[string]ctSourceState{}
	}
	if len(ev.Certificates) > maxEvidenceCerts {
		return ev, errEvidenceLimit
	}
	return ev, nil
}

func (m *monitor) saveEvidence(ev monitorEvidence) error {
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxEvidenceBytes || len(ev.Certificates) > maxEvidenceCerts {
		return errEvidenceLimit
	}
	return writeFileAtomic(filepath.Join(m.dir, "evidence.json"), data)
}

func (m *monitor) knownKeys(baseline *Baseline) (map[string]bool, error) {
	keys := map[string]bool{}
	if baseline != nil {
		for _, k := range baseline.ApprovedSPKISHA256 {
			keys[k] = true
		}
	}
	files, err := m.localHistoryFiles()
	if errors.Is(err, os.ErrNotExist) {
		return keys, nil
	}
	if err != nil {
		return keys, err
	}
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".der") {
			continue
		}
		f, err := os.Open(filepath.Join(m.dir, "local", entry.Name()))
		if err != nil {
			return keys, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxLocalDERBytes+1))
		_ = f.Close()
		if err != nil || len(data) > maxLocalDERBytes {
			return keys, errLocalHistoryInvalid
		}
		cert, err := x509.ParseCertificate(data)
		if err != nil || cert.VerifyHostname(m.hostname) != nil || sha256Hex(data)+".der" != entry.Name() {
			return keys, errLocalHistoryInvalid
		}
		keys[sha256Hex(cert.RawSubjectPublicKeyInfo)] = true
	}
	return keys, nil
}

func (m *monitor) poll(parent context.Context) {
	if parent.Err() != nil {
		return
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "cannot create evidence directory", Err: err})
		return
	}
	m.drainEvents()
	ev, err := m.loadEvidence()
	if err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "cannot load monitor evidence", Err: err})
		return
	}
	baseline, err := m.readBaseline()
	if err != nil && !errors.Is(err, errBaselineUnconfigured) {
		m.emit(Warning{Kind: WarningBaseline, Message: "CAA/SPKI baseline unavailable", Err: err})
	}

	// 1. Direct RFC 8659 CAA lookup against independent DNS resolver.
	caaCtx, cancelCAA := context.WithTimeout(parent, 30*time.Second)
	caaObs := m.observeCAA(caaCtx)
	cancelCAA()
	ev.CAA = append(ev.CAA, caaObs)
	if len(ev.CAA) > maxCAAObservations {
		ev.CAA = ev.CAA[len(ev.CAA)-maxCAAObservations:]
	}
	if caaObs.Error != "" {
		m.emit(Warning{Kind: WarningCAAUnavailable, Source: m.resolver, Message: "CAA observation unavailable; policy state is unknown", Err: errors.New(caaObs.Error)})
	} else if baseline != nil && !slices.Equal(caaObs.Records, baseline.CAA) {
		m.emit(Warning{Kind: WarningCAADrift, Source: m.resolver, Message: "observed CAA policy differs from accepted baseline"})
	}
	if err := m.saveEvidence(ev); err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "cannot save CAA evidence", Err: err})
		return
	}

	// 2. CT poll with automatic fallback across configured CTSources.
	ctCtx, cancelCT := context.WithTimeout(parent, 30*time.Second)
	if err := m.pollCTSources(ctCtx, &ev); err != nil {
		m.emit(Warning{Kind: WarningCTUnavailable, Message: "CT observation unavailable or incomplete", Err: err})
	}
	cancelCT()

	// 3. Compare active observed certificates against local & approved SPKI keys.
	m.drainEvents()
	keys, err := m.knownKeys(baseline)
	if err != nil {
		m.emit(Warning{Kind: WarningStorage, Message: "local public key history unavailable", Err: err})
	}
	now := time.Now().UTC()
	warned := map[string]bool{}
	for _, cert := range ev.Certificates {
		if !cert.CoversHostname || !now.Before(cert.NotAfter) || keys[cert.SPKISHA256] {
			continue
		}
		warnKey := cert.TBSSHA256 + ":" + cert.SPKISHA256
		if warnKey == ":" {
			warnKey = cert.SHA256
		}
		if warned[warnKey] {
			continue
		}
		warned[warnKey] = true
		m.emit(Warning{
			Kind:              WarningUnrecognizedSPKI,
			Source:            cert.Source,
			Message:           "unrecognized public key observed in CT for hostname",
			SPKISHA256:        cert.SPKISHA256,
			CertificateSHA256: cert.SHA256,
			Precertificate:    cert.Precertificate,
		})
	}
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".certmon-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizeASCIIHostname(raw string) string {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if host == "" || len(host) > 253 || strings.Contains(host, "*") || net.ParseIP(host) != nil {
		return ""
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return ""
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return ""
		}
		for i := range len(label) {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return ""
			}
		}
	}
	return host
}
