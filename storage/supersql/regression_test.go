package supersql

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
)

// regressUpdate rewrites the committed per-file regression baselines under
// testdata/<corpus>/baseline. It is used exactly once to record the observed
// buckets after a fix, then the ratchet enforces equality on every run.
var regressUpdate = flag.Bool("update", false, "rewrite the regression baseline files under testdata/<corpus>/baseline")

// TestPostgres18OfficialRegression executes the vendored PostgreSQL regression
// schedules through the public Catalog API and reports each statement's
// outcome: executed, intentionally unsupported (SQLSTATE 0A000 carrying an
// embedded_pg18 exclusion id), dependent on an earlier excluded fixture in the
// same file, a psql client command, or failed. Error outcomes include the
// corpus's intentional negative tests; this gate reports the classification,
// it does not diff psql-formatted expected output.
//
// The run is opt-in because it is long: NEOSEPIA_POSTGRES_REGRESS=1 enables it.
// testdata/postgres184 is the pinned PostgreSQL 18.4 reference corpus and runs
// by default. testdata/postgres is the REL_18_STABLE auxiliary corpus; 189 of
// its 233 files are byte-identical to the reference, so it runs only when
// SUPERSQL_REGRESS_CORPORA names it (comma-separated: postgres184,postgres).
// SUPERSQL_REGRESS_FILES narrows a run to the named schedule files.
func TestPostgres18OfficialRegression(t *testing.T) {
	if os.Getenv("NEOSEPIA_POSTGRES_REGRESS") != "1" {
		t.Skip("set NEOSEPIA_POSTGRES_REGRESS=1 to run the vendored PostgreSQL regression schedule")
	}
	unsupported := unsupportedFeatureIDs(t)
	for _, corpus := range regressionCorpora(t) {
		t.Run(filepath.Base(corpus), func(t *testing.T) {
			runRegressionSchedule(t, corpus, unsupported)
		})
	}
}

// regressionCorpora returns the corpora SUPERSQL_REGRESS_CORPORA names, in the
// order given; unset selects the 18.4 reference corpus.
func regressionCorpora(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("SUPERSQL_REGRESS_CORPORA")
	if raw == "" {
		raw = "postgres184"
	}
	var corpora []string
	for _, name := range strings.Split(raw, ",") {
		switch name = strings.TrimSpace(name); name {
		case "":
		case "postgres184", "postgres":
			corpora = append(corpora, filepath.Join("testdata", name))
		default:
			t.Fatalf("SUPERSQL_REGRESS_CORPORA names %q; want postgres184 or postgres", name)
		}
	}
	return corpora
}

// TestExpectedErrorSet checks the expected-outcome extraction against the
// int4 fixture, whose negative tests the reference file marks with ERROR:.
func TestExpectedErrorSet(t *testing.T) {
	source, err := os.ReadFile("testdata/postgres/sql/int4.sql")
	if err != nil {
		t.Skip("regression corpus not present")
	}
	statements, err := splitRegressionSQL(string(source))
	if err != nil {
		t.Fatal(err)
	}
	expected := expectedErrorSet(t, "testdata/postgres", "int4", statements)
	marked := 0
	for i, stmt := range statements {
		if expected[i] {
			marked++
		}
		if strings.Contains(stmt.text, `'34.5'`) && !expected[i] {
			t.Fatalf("known expected-error statement not marked: %q", stmt.text)
		}
		if strings.Contains(stmt.text, `pg_input_is_valid`) && expected[i] {
			t.Fatalf("successful statement marked expected-error: %q", stmt.text)
		}
	}
	if marked < 8 {
		t.Fatalf("expected-error statements marked=%d, want at least 8", marked)
	}
}

// regressionOutcome buckets one scheduled statement.
type regressionOutcome int

const (
	regPass regressionOutcome = iota
	regUnsupported
	regDependent
	regClient
	regExpectedError
	regUnexpectedSuccess
	regFailed
)

type regressionCounts [7]int

func (c *regressionCounts) add(o regressionOutcome) { c[o]++ }

func (c regressionCounts) total() int {
	total := 0
	for _, n := range c {
		total += n
	}
	return total
}

// dependentCodes are undefined-object or missing-fixture SQLSTATEs: after a
// statement in a file fails or is excluded, later statements failing this way
// are presumed to depend on the fixture that statement would have produced.
var dependentCodes = map[string]bool{
	"42P01": true, // undefined_table
	"42703": true, // undefined_column
	"42704": true, // undefined_object
	"42883": true, // undefined_function
	"3F000": true, // invalid_schema_name
	"42846": true, // cannot_coerce
	"42723": true, // duplicate_function
	"42P06": true, // duplicate_schema
	"42710": true, // duplicate_object
	"25P02": true, // in_failed_sql_transaction: cascade after an abort in the same session
}

func unsupportedFeatureIDs(t *testing.T) map[string]bool {
	t.Helper()
	manifest, err := os.ReadFile("testdata/postgres/embedded_features.json")
	if err != nil {
		t.Fatalf("read exclusion manifest: %v", err)
	}
	var features struct {
		Features []struct {
			ID          string `json:"id"`
			Disposition string `json:"disposition"`
		} `json:"features"`
	}
	if err := json.Unmarshal(manifest, &features); err != nil {
		t.Fatalf("parse exclusion manifest: %v", err)
	}
	unsupported := make(map[string]bool)
	for _, feature := range features.Features {
		if feature.Disposition == "unsupported" {
			unsupported[feature.ID] = true
		}
	}
	return unsupported
}

func runRegressionSchedule(t *testing.T, corpus string, unsupported map[string]bool) {
	schedule, err := os.ReadFile(filepath.Join(corpus, "parallel_schedule"))
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	var files []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(schedule), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "test:") {
			continue
		}
		for _, name := range strings.Fields(line[len("test:"):]) {
			if !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
	}
	if len(files) == 0 {
		t.Fatalf("%s: schedule lists no tests", corpus)
	}
	files = selectRegressionFiles(t, files)

	directory := t.TempDir()
	database, err := Open(t.Context(), OpenOptions{Directory: directory, Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("%s: Close: %v", corpus, err)
		}
	}()
	var maintenanceErr error

	var totals regressionCounts
	byState := make(map[string]int)
	unsupportedLeaks := 0
	// The diagnostic is inert unless SUPERSQL_REGRESS_DUMP names a directory.
	dump := newRegressionDump(t, corpus)
	for _, name := range files {
		source, err := os.ReadFile(filepath.Join(corpus, "sql", name+".sql"))
		if err != nil {
			t.Errorf("%s: read sql file: %v", name, err)
			continue
		}
		statements, err := splitRegressionSQL(string(source))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		expected := expectedErrorSet(t, corpus, name, statements)
		if err := resolveRegressionFixtureCopies(corpus, statements); err != nil {
			t.Fatalf("%s: resolve fixture COPY: %v", name, err)
		}
		// pg_regress runs every file in a fresh psql backend: transactions,
		// prepared statements, cursors, settings and temporary objects end
		// with the file instead of leaking into the rest of the schedule.
		scope, err := database.newSession(t.Context())
		if err != nil {
			t.Fatalf("%s: open session: %v", name, err)
		}
		var counts regressionCounts
		broken := false
		var observed []string
		fileStart := time.Now()
		for i, statement := range statements {
			statementStart := time.Now()
			outcome, state, err := runRegressionStatement(t, database, scope, statement, unsupported)
			if elapsed := time.Since(statementStart); elapsed > regSlowStatement {
				t.Logf("%s/%s:%d: slow statement %s (outcome %d, sqlstate %q): %s", corpus, name, statement.line, elapsed.Round(time.Millisecond), outcome, state, truncateStatement(statement.text))
			}
			if state != "" {
				byState[state]++
			}
			switch {
			case outcome == regUnsupported, outcome == regClient:
				// Manifest exclusions and client commands stand on their own.
			case outcome == regPass && expected[i]:
				// The reference run rejected this statement but we executed
				// it: a negative test silently accepted is a correctness
				// divergence, not a pass.
				outcome = regUnexpectedSuccess
			case outcome == regFailed && expected[i]:
				outcome = regExpectedError
			}
			if outcome == regFailed && state == "0A000" {
				unsupportedLeaks++
				dump.recordLeak(name, statement, err)
			}
			outcome = mapCascadeToDependent(outcome, state, broken)
			counts.add(outcome)
			observed = append(observed, regressionBucketName(outcome))
			if outcome == regFailed || outcome == regUnsupported || outcome == regExpectedError {
				broken = true
			}
			dump.recordFailure(name, statement, outcome, state, err)
		}
		checkRegressionBaseline(t, corpus, name, statements, observed)
		if scope.txns.Active() {
			t.Logf("%s/%s: file ended inside a transaction; closing its session rolls it back", corpus, name)
		}
		if err := scope.close(); err != nil {
			t.Errorf("%s/%s: close session: %v", corpus, name, err)
		}
		for i := range totals {
			totals[i] += counts[i]
		}
		runs, _ := os.ReadDir(filepath.Join(directory, "runs"))
		t.Logf("%s/%s: elapsed=%s run_files=%d statements=%d pass=%d expected_error=%d unsupported=%d dependent=%d client=%d unexpected_success=%d failed=%d",
			corpus, name, time.Since(fileStart).Round(time.Millisecond), len(runs), counts.total(), counts[regPass], counts[regExpectedError], counts[regUnsupported],
			counts[regDependent], counts[regClient], counts[regUnexpectedSuccess], counts[regFailed])
		if err := database.MaintenanceError(); err != nil && (maintenanceErr == nil || err.Error() != maintenanceErr.Error()) {
			maintenanceErr = err
			t.Logf("%s/%s: background maintenance error: %v", corpus, name, err)
		}
	}
	t.Logf("%s totals: statements=%d pass=%d expected_error=%d unsupported=%d dependent=%d client=%d unexpected_success=%d failed=%d",
		corpus, totals.total(), totals[regPass], totals[regExpectedError], totals[regUnsupported],
		totals[regDependent], totals[regClient], totals[regUnexpectedSuccess], totals[regFailed])
	var states []string
	for state := range byState {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		t.Logf("%s SQLSTATE %s: %d", corpus, state, byState[state])
	}
	dump.write(t, totals, unsupportedLeaks)
	if totals[regPass] == 0 {
		t.Fatalf("%s: no statement executed successfully; harness is broken", corpus)
	}
	if unsupportedLeaks > 0 {
		t.Errorf("%s: %d statements rejected 0A000 without a manifest exclusion id", corpus, unsupportedLeaks)
	}
}

// regStatement is one scheduled unit: a SQL statement, a psql client command,
// or a COPY statement whose stdin data/stdout stream is caller-owned.
type regStatement struct {
	text string
	data string // COPY FROM stdin payload, without the \. terminator
	kind regressionKind
	line int
}

type regressionKind int

const (
	regSQL regressionKind = iota
	regClientCommand
	regCopyIn
	regCopyOut
)

var (
	regCopyInPattern  = regexp.MustCompile(`(?is)\bfrom\s+stdin\b`)
	regCopyOutPattern = regexp.MustCompile(`(?is)\bto\s+stdout\b`)
	regVarPattern     = regexp.MustCompile(`:'[^']*'|:"[^"]*"|:[a-zA-Z_][a-zA-Z_0-9]*|:\{\??[a-zA-Z_][a-zA-Z_0-9]*\}`)
	// regStdStringsSet tracks SET standard_conforming_strings so the
	// splitter honors backslash escapes in string literals after the
	// application turns standard-conforming strings off (strings.sql).
	regStdStringsSet   = regexp.MustCompile(`(?i)^\s*SET\s+standard_conforming_strings\s*(?:=|TO\b)\s*'?\s*(ON|TRUE|1|OFF|FALSE|0)\b`)
	regStdStringsReset = regexp.MustCompile(`(?i)^\s*RESET\s+standard_conforming_strings\b`)
)

// regSlowStatement is the per-statement duration above which the schedule logs
// the statement, so resource-heavy statements are visible without a profiler.
const regSlowStatement = 2 * time.Second

// runRegressionStatement executes one scheduled statement in the file's session
// and returns its outcome, the SQLSTATE ("" when the statement succeeded or is
// a client command) and the error the engine reported (nil on success and for
// client commands).
func runRegressionStatement(t *testing.T, database *Catalog, scope *sessionScope, statement regStatement, unsupported map[string]bool) (regressionOutcome, string, error) {
	switch statement.kind {
	case regClientCommand:
		return regClient, "", nil
	case regCopyIn, regCopyOut:
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		streams := &qcopy.Streams{Writer: io.Discard, MaxRecordBytes: 1 << 20}
		if statement.kind == regCopyIn {
			streams = &qcopy.Streams{Reader: strings.NewReader(statement.data), MaxRecordBytes: 1 << 20}
		}
		_, err := database.copyIn(ctx, scope, statement.text, streams)
		return classifyRegressionError(err, unsupported), regressionSQLState(err), err
	default:
		// Statements containing psql variable interpolation need the client
		// layer; they are not executable engine SQL.
		if regVarPattern.MatchString(statement.text) {
			return regClient, "", nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		_, err := database.execIn(ctx, scope, statement.text, 10000)
		return classifyRegressionError(err, unsupported), regressionSQLState(err), err
	}
}

func regressionSQLState(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}

func classifyRegressionError(err error, unsupported map[string]bool) regressionOutcome {
	if err == nil {
		return regPass
	}
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == "0A000" && unsupported[typed.FeatureID] {
		return regUnsupported
	}
	return regFailed
}

// mapCascadeToDependent is the schedule's session-aware dependent rule: after
// one root failure in a file (broken), a later statement failing with a
// dependent SQLSTATE counts as dependent, not as its own failure. Both
// runRegressionSchedule and TestDependent25P02Cascade call it so the proof
// pins the production mapping instead of repeating it.
func mapCascadeToDependent(outcome regressionOutcome, state string, broken bool) regressionOutcome {
	if outcome == regFailed && broken && dependentCodes[state] {
		return regDependent
	}
	return outcome
}

// regressionBucketName names a final per-statement bucket for the baseline.
func regressionBucketName(o regressionOutcome) string {
	switch o {
	case regPass:
		return "pass"
	case regExpectedError:
		return "expected_error"
	case regUnsupported:
		return "unsupported"
	case regDependent:
		return "dependent"
	case regClient:
		return "client"
	case regUnexpectedSuccess:
		return "unexpected_success"
	case regFailed:
		return "failed"
	}
	return "unknown"
}

// checkRegressionBaseline enforces the committed per-file ratchet: the
// observed bucket sequence for one schedule file must equal its baseline
// file under testdata/<corpus>/baseline/<name>.txt (<line> <bucket> per
// statement). Files without a committed baseline are skipped so baselines
// land incrementally per shard; shard 2's files are the first set. With
// -update, the observed sequence rewrites the baseline file instead of
// failing. A mismatch fails the test: a newly failing statement is a
// regression, a newly passing one must update its baseline row.
func checkRegressionBaseline(t *testing.T, corpus, name string, statements []regStatement, observed []string) {
	t.Helper()
	path := filepath.Join(corpus, "baseline", name+".txt")
	var want []string
	for i, statement := range statements {
		want = append(want, fmt.Sprintf("%d %s", statement.line, observed[i]))
	}
	if *regressUpdate {
		if err := os.MkdirAll(filepath.Join(corpus, "baseline"), 0o755); err != nil {
			t.Fatalf("%s/%s: create baseline dir: %v", corpus, name, err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(want, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("%s/%s: write baseline: %v", corpus, name, err)
		}
		t.Logf("%s/%s: baseline rewritten (%d statements)", corpus, name, len(want))
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Logf("%s/%s: no baseline, skipping ratchet (%d statements observed)", corpus, name, len(want))
			return
		}
		t.Errorf("%s/%s: read baseline: %v", corpus, name, err)
		return
	}
	var got []string
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			got = append(got, trimmed)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s/%s: baseline has %d rows, observed %d statements; run with -update to rewrite", corpus, name, len(got), len(want))
		return
	}
	mismatches := 0
	for i := range want {
		if got[i] != want[i] {
			if mismatches < 5 {
				t.Errorf("%s/%s statement %d: baseline %q, observed %q", corpus, name, i, got[i], want[i])
			}
			mismatches++
		}
	}
	if mismatches > 0 {
		t.Errorf("%s/%s: %d baseline mismatches; run with -update to rewrite after triage", corpus, name, mismatches)
	}
}

// regMaxClusterSamples caps the sample statements listed per cluster.
const regMaxClusterSamples = 3

// regMaxSampleSQL bounds the sample statement text written to the cluster
// report; the leak report carries the full statement instead.
const regMaxSampleSQL = 160

// regUnexpectedSuccessMessage clusters statements the reference .out rejected
// but that this engine executed: they carry no error, so they share one key.
const regUnexpectedSuccessMessage = "(unexpected success: reference .out expects an error)"

// regClusterKey groups failures by SQLSTATE and the first line of the engine
// error message, per the C2 triage rule.
type regClusterKey struct {
	state   string
	message string
}

// regCluster accumulates one failure group's count and sample statements.
type regCluster struct {
	count   int
	samples []string
}

// regressionDump collects per-corpus failure detail for the C2 triage rule when
// SUPERSQL_REGRESS_DUMP names a directory. A nil dump is inert, so the default
// test behavior is unchanged.
type regressionDump struct {
	corpusDir string
	corpus    string
	dir       string
	clusters  map[regClusterKey]*regCluster
	leaks     []string
}

// newRegressionDump returns the diagnostic sink for one corpus, or nil when
// SUPERSQL_REGRESS_DUMP is unset.
func newRegressionDump(t *testing.T, corpus string) *regressionDump {
	t.Helper()
	dir := os.Getenv("SUPERSQL_REGRESS_DUMP")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create regression dump directory %q: %v", dir, err)
	}
	return &regressionDump{
		corpusDir: corpus,
		corpus:    filepath.Base(corpus),
		dir:       dir,
		clusters:  make(map[regClusterKey]*regCluster),
	}
}

// regressionErrorMessage splits an engine error into its first message line and
// the embedded_pg18 exclusion id, when the error carries one.
func regressionErrorMessage(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return firstLine(typed.Message), typed.FeatureID
	}
	return firstLine(err.Error()), ""
}

// firstLine returns the first non-empty, trimmed line of text.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// truncateStatement collapses whitespace and bounds a statement for the sample
// list; the ellipsis marks truncation.
func truncateStatement(text string) string {
	flat := collapseWhitespace(text)
	runes := []rune(flat)
	if len(runes) <= regMaxSampleSQL {
		return flat
	}
	return string(runes[:regMaxSampleSQL]) + "…"
}

// location renders a statement's source position for the report.
func (dump *regressionDump) location(name string, statement regStatement) string {
	return filepath.Join(dump.corpusDir, "sql", name+".sql") + ":" + fmt.Sprint(statement.line)
}

// recordFailure adds one statement whose final outcome is a failure or an
// unexpected success to its (sqlstate, first message line) cluster.
func (dump *regressionDump) recordFailure(name string, statement regStatement, outcome regressionOutcome, state string, err error) {
	if dump == nil || (outcome != regFailed && outcome != regUnexpectedSuccess) {
		return
	}
	message, _ := regressionErrorMessage(err)
	if outcome == regUnexpectedSuccess {
		message = regUnexpectedSuccessMessage
	}
	key := regClusterKey{state: state, message: message}
	cluster := dump.clusters[key]
	if cluster == nil {
		cluster = &regCluster{}
		dump.clusters[key] = cluster
	}
	cluster.count++
	if len(cluster.samples) < regMaxClusterSamples {
		cluster.samples = append(cluster.samples,
			fmt.Sprintf("%s | %s", dump.location(name, statement), truncateStatement(statement.text)))
	}
}

// recordLeak adds one statement that failed with 0A000 without a manifest
// exclusion id: the hard-fail set the regression gate reports.
func (dump *regressionDump) recordLeak(name string, statement regStatement, err error) {
	if dump == nil {
		return
	}
	message, featureID := regressionErrorMessage(err)
	if featureID == "" {
		featureID = "(none)"
	}
	dump.leaks = append(dump.leaks, fmt.Sprintf(
		"--- leak %d\nfile: %s\nsqlstate: 0A000\nfeature_id: %s\nmessage: %s\nsql:\n%s\n",
		len(dump.leaks)+1, dump.location(name, statement), featureID, message, statement.text))
}

// write emits <dump>/<corpus>-clusters.txt (clusters sorted by count desc) and
// <dump>/<corpus>-leaks.txt (0A000 statements with no manifest exclusion id).
func (dump *regressionDump) write(t *testing.T, totals regressionCounts, unsupportedLeaks int) {
	if dump == nil {
		return
	}
	t.Helper()
	header := fmt.Sprintf(
		"# SUPERSQL_REGRESS_DUMP corpus=%s\n# statements=%d pass=%d expected_error=%d unsupported=%d dependent=%d client=%d unexpected_success=%d failed=%d\n",
		dump.corpus, totals.total(), totals[regPass], totals[regExpectedError], totals[regUnsupported],
		totals[regDependent], totals[regClient], totals[regUnexpectedSuccess], totals[regFailed])

	keys := make([]regClusterKey, 0, len(dump.clusters))
	for key := range dump.clusters {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := dump.clusters[keys[i]], dump.clusters[keys[j]]
		if left.count != right.count {
			return left.count > right.count
		}
		if keys[i].state != keys[j].state {
			return keys[i].state < keys[j].state
		}
		return keys[i].message < keys[j].message
	})

	var clusters strings.Builder
	clusters.WriteString(header)
	clusters.WriteString(fmt.Sprintf(
		"# clusters=%d failures=%d (grouped by sqlstate + first line of the engine error message, sorted by count desc)\n",
		len(keys), totals[regFailed]+totals[regUnexpectedSuccess]))
	clusters.WriteString("# columns: sqlstate | count | first message line\n")
	for _, key := range keys {
		cluster := dump.clusters[key]
		clusters.WriteString(fmt.Sprintf("%s | %d | %s\n", key.state, cluster.count, key.message))
		for _, sample := range cluster.samples {
			clusters.WriteString("    sample: " + sample + "\n")
		}
	}
	clustersPath := filepath.Join(dump.dir, dump.corpus+"-clusters.txt")
	if err := os.WriteFile(clustersPath, []byte(clusters.String()), 0o644); err != nil {
		t.Errorf("write cluster report %s: %v", clustersPath, err)
	}

	var leaks strings.Builder
	leaks.WriteString(header)
	leaks.WriteString(fmt.Sprintf(
		"# leaks=%d (regFailed statements rejected with 0A000 whose FeatureID is not in testdata/postgres/embedded_features.json)\n",
		len(dump.leaks)))
	if len(dump.leaks) != unsupportedLeaks {
		t.Errorf("dump recorded %d leaks but the schedule counted %d", len(dump.leaks), unsupportedLeaks)
	}
	for _, leak := range dump.leaks {
		leaks.WriteString(leak)
	}
	leaksPath := filepath.Join(dump.dir, dump.corpus+"-leaks.txt")
	if err := os.WriteFile(leaksPath, []byte(leaks.String()), 0o644); err != nil {
		t.Errorf("write leak report %s: %v", leaksPath, err)
	}
	t.Logf("%s: dump written to %s (%d clusters, %d leaks)", dump.corpus, dump.dir, len(keys), len(dump.leaks))
}

// expectedErrorSet locates each statement in the file's reference
// expected/<name>.out and reports which ones the reference run rejected with
// ERROR:. The .out format echoes each statement verbatim, then prints either
// its result or an ERROR: line, so an ERROR: appearing between a statement's
// echo and the next echo marks that statement's expected outcome.
// Statements that cannot be located report false: their actual outcome is
// then judged without an expected baseline, keeping real failures visible.
func expectedErrorSet(t *testing.T, corpus, name string, statements []regStatement) []bool {
	expected := make([]bool, len(statements))
	data, err := os.ReadFile(filepath.Join(corpus, "expected", name+".out"))
	if err != nil {
		return expected
	}
	out := string(data)
	resultRows := regressionAlignedResults(out)
	pos := 0
	for i, statement := range statements {
		if statement.kind != regSQL {
			continue
		}
		source := strings.TrimSpace(stripLeadingComments(statement.text))
		if source == "" {
			continue
		}
		// Match the complete echoed statement and its outer terminator. SQL
		// wrapping may differ, but literal and comment bytes stay exact,
		// including the vendored non-UTF8 encoding fixtures.
		end, found := findRegressionEcho(out, pos, regressionEchoParts(source), resultRows)
		if !found {
			continue
		}
		pos = end
		// A trailing SQL comment may precede the diagnostic; comments inside
		// the matched statement were never removed.
		expected[i] = strings.HasPrefix(stripLeadingComments(out[pos:]), "ERROR:")
	}
	return expected
}

func regressionEchoSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\v' || c == '\f'
}

// Empty parts denote whitespace outside SQL literals and comments. All other
// parts borrow the exact source bytes; a regexp cannot distinguish invalid
// UTF8 bytes in the encoding corpora.
func regressionEchoParts(source string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(source); {
		switch {
		case regressionEchoSpace(source[i]):
			if i > start {
				parts = append(parts, source[start:i])
			}
			parts = append(parts, "")
			for i < len(source) && regressionEchoSpace(source[i]) {
				i++
			}
			start = i
		case strings.HasPrefix(source[i:], "--"):
			if end := strings.IndexByte(source[i:], '\n'); end >= 0 {
				i += end + 1
			} else {
				i = len(source)
			}
		case strings.HasPrefix(source[i:], "/*"):
			i += 2
			for depth := 1; i < len(source) && depth > 0; {
				switch {
				case strings.HasPrefix(source[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(source[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
		case source[i] == '\'' || source[i] == '"':
			quote := source[i]
			i++
			for i < len(source) {
				if source[i] == quote {
					i++
					if i < len(source) && source[i] == quote {
						i++
						continue
					}
					break
				}
				if quote == '\'' && source[i] == '\\' && i+1 < len(source) {
					i += 2
				} else {
					i++
				}
			}
		case source[i] == '$':
			tagStart, tagEnd := i, i+1
			for tagEnd < len(source) && isDollarTagByte(source[tagEnd]) {
				tagEnd++
			}
			i++
			if tagEnd < len(source) && source[tagEnd] == '$' {
				tag := source[tagStart : tagEnd+1]
				if end := strings.Index(source[tagEnd+1:], tag); end >= 0 {
					i = tagEnd + 1 + end + len(tag)
				}
			}
		default:
			i++
		}
	}
	if start < len(source) {
		parts = append(parts, source[start:])
	}
	return parts
}

type regressionResultRange struct {
	start int
	end   int
}

// Recognize only complete single-line aligned results: a padded header, a
// column divider, padded rows and the matching row-count footer. Multiline
// and other psql output formats are left to ordinary echo matching.
func regressionAlignedResults(output string) []regressionResultRange {
	var result []regressionResultRange
	start, rows, previousStart := -1, 0, 0
	previous := ""
	for pos := 0; pos < len(output); {
		end := strings.IndexByte(output[pos:], '\n')
		next := len(output)
		if end < 0 {
			end = len(output)
		} else {
			end += pos
			next = end + 1
		}
		line := strings.TrimSuffix(output[pos:end], "\r")
		divider := len(line) >= 3
		for _, column := range strings.Split(line, "+") {
			if len(column) < 3 || strings.Trim(column, "-") != "" {
				divider = false
				break
			}
		}
		switch {
		case divider && strings.HasPrefix(previous, " "):
			start, rows = previousStart, 0
		case start >= 0 && strings.HasPrefix(line, "("):
			footer := fmt.Sprintf("(%d rows)", rows)
			if rows == 1 {
				footer = "(1 row)"
			}
			if line == footer {
				result = append(result, regressionResultRange{start: start, end: pos})
			}
			start = -1
		case start >= 0 && strings.HasPrefix(line, " "):
			rows++
		default:
			start = -1
		}
		previous, previousStart = line, pos
		pos = next
	}
	return result
}

func findRegressionEcho(output string, from int, parts []string, resultRows []regressionResultRange) (int, bool) {
	if len(parts) == 0 || parts[0] == "" {
		return 0, false
	}
	for search := from; search < len(output); {
		found := strings.Index(output[search:], parts[0])
		if found < 0 {
			break
		}
		start := search + found
		search = start + 1
		row := sort.Search(len(resultRows), func(i int) bool { return resultRows[i].end > start })
		if row < len(resultRows) && start >= resultRows[row].start {
			search = resultRows[row].end
			continue
		}
		// psql repeats a failing query after "LINE n:". That diagnostic is
		// not another execution, even when the next statement is identical.
		line := strings.LastIndexByte(output[:start], '\n') + 1
		prefix := strings.TrimSpace(output[line:start])
		if strings.HasPrefix(prefix, "LINE ") {
			i := len("LINE ")
			for i < len(prefix) && prefix[i] >= '0' && prefix[i] <= '9' {
				i++
			}
			if i > len("LINE ") && i < len(prefix) && prefix[i] == ':' {
				continue
			}
		}
		end, matched := start+len(parts[0]), true
		for _, part := range parts[1:] {
			if part == "" {
				before := end
				for end < len(output) && regressionEchoSpace(output[end]) {
					end++
				}
				if before == end {
					matched = false
					break
				}
			} else if strings.HasPrefix(output[end:], part) {
				end += len(part)
			} else {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		for end < len(output) && regressionEchoSpace(output[end]) {
			end++
		}
		if end < len(output) && output[end] == ';' {
			return end + 1, true
		}
	}
	return 0, false
}

// stripLeadingComments drops leading whitespace and SQL comments from a
// statement so a preceding comment does not prevent matching the echoed SQL.
func stripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		}
		if strings.HasPrefix(s, "/*") {
			depth := 1
			i := 2
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			s = s[i:]
			continue
		}
		return s
	}
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// firstSQLWord returns the statement's leading identifier, skipping whitespace
// and SQL comments.
func firstSQLWord(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		}
		if strings.HasPrefix(s, "/*") {
			depth := 1
			i := 2
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			s = s[i:]
			continue
		}
		break
	}
	end := 0
	for end < len(s) && (s[end] >= 'a' && s[end] <= 'z' || s[end] >= 'A' && s[end] <= 'Z' || s[end] == '_') {
		end++
	}
	return strings.ToLower(s[:end])
}

// splitRegressionSQL splits a regression .sql file into statements, tracking
// strings (including E” backslash escapes and backslash escapes while
// standard_conforming_strings is off), quoted identifiers, dollar-quoted
// bodies, comments, and parenthesis depth (CREATE RULE bodies hold
// semicolons). Lines whose first character is '\' are psql client commands,
// as is any statement with a mid-line backslash command (\gset, \bind,
// \sendpipeline and friends). Lines starting with '**' are psql
// query-buffer notes the server never sees (rules.sql prints and discards
// them with \p and \r). After a COPY ... FROM stdin statement, raw lines up
// to a lone \. are the caller-owned stream data.
func splitRegressionSQL(source string) ([]regStatement, error) {
	var statements []regStatement
	var buf strings.Builder
	var bufQuoted bool
	var bufClient bool
	var bufGset bool
	parenDepth := 0
	stdStrings := true
	// noteStringsSetting tracks SET/RESET standard_conforming_strings so a
	// later string literal honors backslash escapes exactly when the session
	// would: off means backslash escapes, on (or reset) means literal.
	noteStringsSetting := func(text string) {
		if regStdStringsReset.MatchString(text) {
			stdStrings = true
			return
		}
		if m := regStdStringsSet.FindStringSubmatch(text); m != nil {
			switch strings.ToUpper(m[1]) {
			case "ON", "TRUE", "1":
				stdStrings = true
			default:
				stdStrings = false
			}
		}
	}
	line := 1
	stmtLine := 0
	mark := func() {
		if stmtLine == 0 {
			stmtLine = line
		}
	}
	emitSQL := func(text string, lineNo int) {
		quoted := bufQuoted
		bufQuoted = false
		client := bufClient
		bufClient = false
		bufGset = false
		parenDepth = 0
		if trimmed := strings.TrimSpace(text); trimmed != "" && !commentOnly(trimmed) {
			noteStringsSetting(trimmed)
			stmt := regStatement{text: trimmed, line: lineNo}
			if quoted || client {
				stmt.kind = regClientCommand
			}
			statements = append(statements, stmt)
		}
	}
	i := 0
	atLineStart := true
	for i < len(source) {
		c := source[i]
		if c == '\n' {
			// An inline \gset executes and clears the query buffer. Other
			// psql commands can keep that buffer open across physical lines.
			if bufGset {
				emitSQL(buf.String(), stmtLine)
				buf.Reset()
				stmtLine = 0
			} else {
				buf.WriteByte(c)
			}
			line++
			atLineStart = true
			i++
			continue
		}
		if atLineStart && (c == '\\' || c == '*' && i+1 < len(source) && source[i+1] == '*') {
			emitSQL(buf.String(), stmtLine)
			buf.Reset()
			stmtLine = 0
			end := strings.IndexByte(source[i:], '\n')
			if end < 0 {
				end = len(source)
			} else {
				end += i
			}
			statements = append(statements, regStatement{text: strings.TrimSpace(source[i:end]), kind: regClientCommand, line: line})
			i = end
			continue
		}
		switch c {
		case ' ', '\t', '\r':
			buf.WriteByte(c)
			i++
		case '-':
			if i+1 < len(source) && source[i+1] == '-' {
				end := strings.IndexByte(source[i:], '\n')
				if end < 0 {
					end = len(source)
				} else {
					end += i
				}
				buf.WriteString(source[i:end])
				i = end
			} else {
				mark()
				atLineStart = false
				buf.WriteByte(c)
				i++
			}
		case '/':
			if i+1 < len(source) && source[i+1] == '*' {
				depth := 1
				j := i + 2
				for j < len(source) && depth > 0 {
					if strings.HasPrefix(source[j:], "/*") {
						depth++
						j += 2
					} else if strings.HasPrefix(source[j:], "*/") {
						depth--
						j += 2
					} else {
						if source[j] == '\n' {
							line++
						}
						j++
					}
				}
				buf.WriteString(source[i:j])
				i = j
				atLineStart = false
			} else {
				mark()
				atLineStart = false
				buf.WriteByte(c)
				i++
			}
		case '\'':
			mark()
			escape := i > 0 && (source[i-1] == 'e' || source[i-1] == 'E') &&
				(i < 2 || !isIdentByte(source[i-2]))
			uquoted := i > 1 && source[i-1] == '&' && (source[i-2] == 'u' || source[i-2] == 'U') &&
				(i < 3 || !isIdentByte(source[i-3]))
			if uquoted {
				escape = false
			} else if !stdStrings {
				escape = true
			}
			j := i + 1
			for j < len(source) {
				if escape && source[j] == '\\' {
					j += 2
					continue
				}
				if source[j] == '\n' {
					line++
				}
				if source[j] == '\'' {
					if j+1 < len(source) && source[j+1] == '\'' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			atLineStart = false
			buf.WriteString(source[i:j])
			i = j
		case '"':
			mark()
			j := i + 1
			for j < len(source) {
				if source[j] == '"' {
					if j+1 < len(source) && source[j+1] == '"' {
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			atLineStart = false
			buf.WriteString(source[i:j])
			i = j
		case '$':
			tagEnd := i + 1
			for tagEnd < len(source) && isDollarTagByte(source[tagEnd]) {
				tagEnd++
			}
			if tagEnd < len(source) && source[tagEnd] == '$' {
				mark()
				tag := source[i : tagEnd+1]
				end := strings.Index(source[tagEnd+1:], tag)
				if end < 0 {
					return nil, fmt.Errorf("unterminated dollar quote %q at line %d", tag, line)
				}
				end += tagEnd + 1 + len(tag)
				line += strings.Count(source[i:end], "\n")
				atLineStart = false
				buf.WriteString(source[i:end])
				i = end
			} else {
				mark()
				atLineStart = false
				buf.WriteByte(c)
				i++
			}
		case ';':
			if parenDepth > 0 && !bufClient {
				mark()
				atLineStart = false
				buf.WriteByte(';')
				i++
				break
			}
			if i > 0 && source[i-1] == '\\' {
				mark()
				atLineStart = false
				buf.WriteByte(';')
				bufQuoted = true
				i++
				break
			}
			quoted := bufQuoted
			bufQuoted = false
			client := bufClient
			bufClient = false
			bufGset = false
			parenDepth = 0
			text := strings.TrimSpace(buf.String())
			buf.Reset()
			i++
			if len(text) != 0 && !commentOnly(text) {
				noteStringsSetting(text)
				stmt := regStatement{text: text, line: stmtLine}
				if quoted || client {
					stmt.kind = regClientCommand
				}
				if stmt.kind == regSQL && firstSQLWord(text) == "copy" {
					if regCopyInPattern.MatchString(text) {
						stmt.kind = regCopyIn
					} else if regCopyOutPattern.MatchString(text) {
						stmt.kind = regCopyOut
					}
				}
				if stmt.kind == regCopyIn {
					var data strings.Builder
					for i < len(source) {
						end := strings.IndexByte(source[i:], '\n')
						var row string
						if end < 0 {
							row, i = source[i:], len(source)
						} else {
							row = source[i : i+end+1]
							i += end + 1
							line++
						}
						if strings.TrimRight(row, "\r\n") == `\.` {
							break
						}
						data.WriteString(row)
					}
					stmt.data = data.String()
				} else {
					j := i
					for j < len(source) && (source[j] == ' ' || source[j] == '\t' || source[j] == '\r') {
						j++
					}
					if j+1 < len(source) && source[j] == '-' && source[j+1] == '-' {
						if end := strings.IndexByte(source[j:], '\n'); end >= 0 {
							i = j + end
						} else {
							i = len(source)
						}
					}
				}
				statements = append(statements, stmt)
			}
			stmtLine = 0
		case '(', ')':
			// Semicolons inside rule bodies and other parenthesized
			// fragments do not end the statement (CREATE RULE ... DO
			// ALSO (stmt; stmt)). Comments and literals never reach
			// here: their cases consume them whole. Parentheses inside
			// client input (mid-line psql commands) do not count: the
			// client blob ends at a \gset newline or its SQL semicolon.
			mark()
			atLineStart = false
			if !bufClient {
				if c == '(' {
					parenDepth++
				} else if parenDepth > 0 {
					parenDepth--
				}
			}
			buf.WriteByte(c)
			i++
		case '\\':
			// A mid-line backslash command is psql behavior, not engine
			// SQL: \gset, \bind, \sendpipeline and friends execute in
			// the client. Line-leading backslashes never reach here.
			mark()
			atLineStart = false
			if i+1 < len(source) {
				next := source[i+1]
				if next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z' {
					bufClient = true
					const command = "\\gset"
					if strings.HasPrefix(source[i:], command) {
						end := i + len(command)
						if end == len(source) || source[end] == ' ' || source[end] == '\t' || source[end] == '\r' || source[end] == '\n' {
							bufGset = true
						}
					}
				}
			}
			buf.WriteByte(c)
			i++
		default:
			mark()
			atLineStart = false
			buf.WriteByte(c)
			i++
		}
	}
	emitSQL(buf.String(), stmtLine)
	return statements, nil
}

// commentOnly reports whether text is entirely whitespace and SQL comments.
func commentOnly(s string) bool {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		if s == "" {
			return true
		}
		if strings.HasPrefix(s, "--") {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return true
		}
		if strings.HasPrefix(s, "/*") {
			depth := 1
			i := 2
			for i < len(s) && depth > 0 {
				if strings.HasPrefix(s[i:], "/*") {
					depth++
					i += 2
				} else if strings.HasPrefix(s[i:], "*/") {
					depth--
					i += 2
				} else {
					i++
				}
			}
			s = s[i:]
			continue
		}
		return false
	}
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
}

func isDollarTagByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// selectRegressionFiles narrows the schedule to the comma-separated file names
// in SUPERSQL_REGRESS_FILES, keeping schedule order. Unset runs every file.
func selectRegressionFiles(t *testing.T, files []string) []string {
	t.Helper()
	raw := os.Getenv("SUPERSQL_REGRESS_FILES")
	if raw == "" {
		return files
	}
	wanted := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	var selected []string
	for _, name := range files {
		if wanted[name] {
			selected = append(selected, name)
			delete(wanted, name)
		}
	}
	for name := range wanted {
		t.Fatalf("SUPERSQL_REGRESS_FILES names %q, which the schedule does not list", name)
	}
	return selected
}
