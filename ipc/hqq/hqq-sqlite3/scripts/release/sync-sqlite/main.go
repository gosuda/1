package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultDownloadURL = "https://www.sqlite.org/download.html"

type upstreamRelease struct {
	Version       string
	VersionNumber int
	Archive       string
	URL           string
	Size          int64
	SHA3_256      string
}

type localPin struct {
	Version       string
	VersionNumber int
	Archive       string
	URL           string
	SHA256        string
	SHA3_256      string
	WrapperPrefix string
}

func main() {
	var (
		rootDir      = flag.String("root", ".", "repository root directory")
		downloadURL  = flag.String("url", defaultDownloadURL, "SQLite download page URL")
		htmlFile     = flag.String("html", "", "read SQLite download HTML from local file instead of HTTP")
		apply        = flag.Bool("apply", false, "download, verify, and update pin files when an update is available")
		force        = flag.Bool("force", false, "apply update even when upstream version matches current pin")
		githubOutput = flag.Bool("github-output", false, "write outputs to $GITHUB_OUTPUT (or stdout if unset)")
	)
	flag.Parse()

	if err := run(*rootDir, *downloadURL, *htmlFile, *apply, *force, *githubOutput); err != nil {
		fmt.Fprintf(os.Stderr, "sync-sqlite: %v\n", err)
		os.Exit(1)
	}
}

func run(root, downloadURL, htmlFile string, apply, force, githubOutput bool) error {
	var htmlBytes []byte
	var err error
	if htmlFile != "" {
		htmlBytes, err = os.ReadFile(htmlFile)
	} else {
		htmlBytes, err = fetchURL(downloadURL, 0)
	}
	if err != nil {
		return fmt.Errorf("fetch download page: %w", err)
	}

	upstream, err := parseDownloadHTML(htmlBytes)
	if err != nil {
		return err
	}

	pin, err := readLocalPin(root)
	if err != nil {
		return err
	}

	updated := isUpdateAvailable(pin, upstream)
	tag := fmt.Sprintf("%s%d", pin.WrapperPrefix, upstream.VersionNumber)

	var sha256Hex string
	if apply && (updated || force) {
		zipData, err := fetchURL(upstream.URL, upstream.Size)
		if err != nil {
			return fmt.Errorf("download %s: %w", upstream.URL, err)
		}
		sha256Hex, err = verifyAmalgamationZip(zipData, upstream)
		if err != nil {
			return err
		}
		if err := applyPinUpdates(root, upstream, sha256Hex, tag); err != nil {
			return err
		}
	} else {
		sha256Hex = pin.SHA256
	}

	if githubOutput {
		if err := writeOutputs(updated, upstream, sha256Hex, tag); err != nil {
			return err
		}
	} else {
		fmt.Printf("current=%s (%d) upstream=%s (%d) updated=%t tag=%s\n",
			pin.Version, pin.VersionNumber, upstream.Version, upstream.VersionNumber, updated, tag)
	}
	return nil
}

func fetchURL(rawURL string, expectedSize int64) ([]byte, error) {
	if !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("refusing non-https URL: %s", rawURL)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "hqq-sqlite3-sync/1.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if expectedSize > 0 && int64(len(data)) != expectedSize {
			lastErr = fmt.Errorf("downloaded %d bytes, want %d bytes", len(data), expectedSize)
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

var (
	reAmalgamationRel = regexp.MustCompile(`^(?:[0-9]{4}/)?sqlite-amalgamation-([0-9]+)\.zip$`)
	reSHA3Hex         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reVersionNumber   = regexp.MustCompile(`(?m)^#define\s+SQLITE_VERSION_NUMBER\s+([0-9]+)\b`)
)

func parseDownloadHTML(html []byte) (upstreamRelease, error) {
	lines := strings.Split(string(html), "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "PRODUCT,") {
			continue
		}
		r := csv.NewReader(strings.NewReader(line))
		rec, err := r.Read()
		if err != nil || len(rec) < 5 {
			continue
		}
		ver := strings.TrimSpace(rec[1])
		relURL := strings.TrimSpace(rec[2])
		sizeStr := strings.TrimSpace(rec[3])
		sha3Hex := strings.ToLower(strings.TrimSpace(rec[4]))

		m := reAmalgamationRel.FindStringSubmatch(relURL)
		if m == nil {
			continue
		}
		verNum, err := parseSQLiteVersion(ver)
		if err != nil {
			return upstreamRelease{}, fmt.Errorf("invalid SQLite version %q in CSV: %w", ver, err)
		}
		urlVerNum, err := strconv.Atoi(m[1])
		if err != nil || urlVerNum != verNum {
			return upstreamRelease{}, fmt.Errorf("version number mismatch: version %q (%d) vs archive %q", ver, verNum, relURL)
		}
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || size <= 0 {
			return upstreamRelease{}, fmt.Errorf("invalid archive size %q", sizeStr)
		}
		if !reSHA3Hex.MatchString(sha3Hex) {
			return upstreamRelease{}, fmt.Errorf("invalid SHA3-256 digest %q", sha3Hex)
		}
		return upstreamRelease{
			Version:       ver,
			VersionNumber: verNum,
			Archive:       path.Base(relURL),
			URL:           "https://www.sqlite.org/" + relURL,
			Size:          size,
			SHA3_256:      sha3Hex,
		}, nil
	}
	return upstreamRelease{}, errors.New("no sqlite-amalgamation PRODUCT CSV entry found in download HTML")
}

func parseSQLiteVersion(v string) (int, error) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 && len(parts) != 4 {
		return 0, fmt.Errorf("expected 3 or 4 numeric components, got %q", v)
	}
	nums := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (i > 0 && n >= 100) {
			return 0, fmt.Errorf("invalid component %q in %q", p, v)
		}
		nums[i] = n
	}
	return nums[0]*1_000_000 + nums[1]*10_000 + nums[2]*100 + nums[3], nil
}

func readLocalPin(root string) (localPin, error) {
	pinDir := filepath.Join(root, "third_party", "sqlite")
	verBytes, err := os.ReadFile(filepath.Join(pinDir, "VERSION"))
	if err != nil {
		return localPin{}, err
	}
	var pin localPin
	for _, rawLine := range strings.Split(string(verBytes), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return localPin{}, fmt.Errorf("invalid line in VERSION: %q", line)
		}
		switch strings.TrimSpace(k) {
		case "version":
			pin.Version = strings.TrimSpace(v)
		case "archive":
			pin.Archive = strings.TrimSpace(v)
		case "url":
			pin.URL = strings.TrimSpace(v)
		}
	}
	if pin.Version == "" || pin.Archive == "" || pin.URL == "" {
		return localPin{}, errors.New("incomplete third_party/sqlite/VERSION")
	}
	verNum, err := parseSQLiteVersion(pin.Version)
	if err != nil {
		return localPin{}, fmt.Errorf("invalid pinned version %q: %w", pin.Version, err)
	}
	pin.VersionNumber = verNum

	pin.SHA256, err = readPinnedDigest(filepath.Join(pinDir, "SHA256"), pin.Archive)
	if err != nil {
		return localPin{}, err
	}
	pin.SHA3_256, err = readPinnedDigest(filepath.Join(pinDir, "SHA3-256"), pin.Archive)
	if err != nil {
		return localPin{}, err
	}

	hashesBytes, err := os.ReadFile(filepath.Join(root, "hashes.go"))
	if err != nil {
		return localPin{}, err
	}
	pin.WrapperPrefix, err = extractWrapperPrefix(string(hashesBytes))
	if err != nil {
		return localPin{}, err
	}
	return pin, nil
}

func readPinnedDigest(filePath, archive string) (string, error) {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == archive {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no entry for %s in %s", archive, filePath)
}

var reWrapperVersion = regexp.MustCompile(`Version\s*=\s*"(v[0-9]+\.[0-9]+\.)[0-9]+"`)

func extractWrapperPrefix(hashesSrc string) (string, error) {
	m := reWrapperVersion.FindStringSubmatch(hashesSrc)
	if m == nil {
		return "", errors.New("could not find Version constant in hashes.go")
	}
	return m[1], nil
}

func isUpdateAvailable(pin localPin, up upstreamRelease) bool {
	if up.VersionNumber > pin.VersionNumber {
		return true
	}
	if up.VersionNumber < pin.VersionNumber {
		return false
	}
	return up.Archive != pin.Archive || up.URL != pin.URL || up.SHA3_256 != pin.SHA3_256
}

func verifyAmalgamationZip(data []byte, up upstreamRelease) (string, error) {
	if int64(len(data)) != up.Size {
		return "", fmt.Errorf("archive size %d != expected %d", len(data), up.Size)
	}
	gotSHA3 := sha3.Sum256(data)
	gotSHA3Hex := hex.EncodeToString(gotSHA3[:])
	if gotSHA3Hex != up.SHA3_256 {
		return "", fmt.Errorf("SHA3-256 mismatch: got %s, want %s", gotSHA3Hex, up.SHA3_256)
	}
	gotSHA256 := sha256.Sum256(data)
	sha256Hex := hex.EncodeToString(gotSHA256[:])

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("open zip: %w", err)
	}
	base := strings.TrimSuffix(up.Archive, ".zip")
	var hasC, hasH bool
	var headerData []byte
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "/") || strings.Contains(f.Name, "..") {
			return "", fmt.Errorf("unsafe zip entry name: %s", f.Name)
		}
		if f.Name == base+"/sqlite3.c" {
			hasC = true
		}
		if f.Name == base+"/sqlite3.h" {
			hasH = true
			rc, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("open sqlite3.h: %w", err)
			}
			headerData, err = io.ReadAll(io.LimitReader(rc, 4<<20))
			_ = rc.Close()
			if err != nil {
				return "", fmt.Errorf("read sqlite3.h: %w", err)
			}
		}
	}
	if !hasC || !hasH {
		return "", fmt.Errorf("missing sqlite3.c or sqlite3.h under %s/ in %s", base, up.Archive)
	}
	m := reVersionNumber.FindSubmatch(headerData)
	if m == nil {
		return "", errors.New("SQLITE_VERSION_NUMBER not found in sqlite3.h")
	}
	headerNum, err := strconv.Atoi(string(m[1]))
	if err != nil || headerNum != up.VersionNumber {
		return "", fmt.Errorf("sqlite3.h SQLITE_VERSION_NUMBER=%q, want %d", string(m[1]), up.VersionNumber)
	}
	return sha256Hex, nil
}

func applyPinUpdates(root string, up upstreamRelease, sha256Hex, tag string) error {
	pinDir := filepath.Join(root, "third_party", "sqlite")
	versionFile := fmt.Sprintf(`# SQLite amalgamation pinned for hqq-sqlite-server release builds.
# Read by scripts/release/fetch-sqlite.sh and fetch-sqlite.ps1.
# Public domain (https://www.sqlite.org/copyright.html). Not vendored in git.
# Checksums: SHA256 and SHA3-256 files in this directory; both are listed on
# https://www.sqlite.org/download.html (SHA3-256 column).
version=%s
archive=%s
url=%s

`, up.Version, up.Archive, up.URL)

	if err := os.WriteFile(filepath.Join(pinDir, "VERSION"), []byte(versionFile), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pinDir, "SHA256"), []byte(fmt.Sprintf("%s  %s\n", sha256Hex, up.Archive)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pinDir, "SHA3-256"), []byte(fmt.Sprintf("%s  %s\n", up.SHA3_256, up.Archive)), 0o644); err != nil {
		return err
	}

	if err := updateHashesGo(filepath.Join(root, "hashes.go"), up, tag); err != nil {
		return err
	}
	if err := updateReadme(filepath.Join(root, "README.md"), up, tag); err != nil {
		return err
	}
	if err := updateSQLiteReadme(filepath.Join(root, "sqlite3", "README.md"), up); err != nil {
		return err
	}
	return nil
}

var (
	reHashComment = regexp.MustCompile(`(?m)^// where [0-9]+ corresponds to SQLITE_VERSION_NUMBER \(SQLite [^)]+\)\.$`)
	reHashVersion = regexp.MustCompile(`(?m)^(\tVersion\s*=\s*)"[^"]+"$`)
	reHashVerNum  = regexp.MustCompile(`(?m)^(\tSQLiteVersionNumber\s*=\s*)[0-9]+$`)
)

func updateHashesGo(filePath string, up upstreamRelease, tag string) error {
	b, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}
	s := string(b)
	s = reHashComment.ReplaceAllString(s, fmt.Sprintf("// where %d corresponds to SQLITE_VERSION_NUMBER (SQLite %s).", up.VersionNumber, up.Version))
	s = reHashVersion.ReplaceAllString(s, fmt.Sprintf(`${1}"%s"`, tag))
	s = reHashVerNum.ReplaceAllString(s, fmt.Sprintf(`${1}%d`, up.VersionNumber))
	return os.WriteFile(filePath, []byte(s), 0o644)
}

func updateReadme(filePath string, up upstreamRelease, tag string) error {
	b, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s := string(b)
	s = reHashVersion.ReplaceAllString(s, fmt.Sprintf(`${1}"%s"`, tag))
	s = reHashVerNum.ReplaceAllString(s, fmt.Sprintf(`${1}%d`, up.VersionNumber))
	return os.WriteFile(filePath, []byte(s), 0o644)
}

var reSQLiteReadmePin = regexp.MustCompile("Fetch the pinned SQLite amalgamation \\(`[0-9.]+`\\)")

func updateSQLiteReadme(filePath string, up upstreamRelease) error {
	b, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s := reSQLiteReadmePin.ReplaceAllString(string(b), fmt.Sprintf("Fetch the pinned SQLite amalgamation (`%s`)", up.Version))
	return os.WriteFile(filePath, []byte(s), 0o644)
}

func writeOutputs(updated bool, up upstreamRelease, sha256Hex, tag string) error {
	lines := fmt.Sprintf("updated=%t\nversion=%s\nversion_number=%d\ntag=%s\narchive=%s\nurl=%s\nsha256=%s\nsha3_256=%s\n",
		updated, up.Version, up.VersionNumber, tag, up.Archive, up.URL, sha256Hex, up.SHA3_256)

	outPath := os.Getenv("GITHUB_OUTPUT")
	if outPath == "" {
		_, err := os.Stdout.WriteString(lines)
		return err
	}
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(lines)
	return err
}
