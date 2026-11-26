package supersql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplainBuffersTextAndJSON(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if _, err := database.exec(ctx, `CREATE TABLE explain_buf_items (id int PRIMARY KEY, val text)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := database.exec(ctx, `INSERT INTO explain_buf_items SELECT g, 'row-' || g::text FROM generate_series(1, 50) g`, 0); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// 1. EXPLAIN (ANALYZE, BUFFERS) in default TEXT format.
	out, err := database.exec(ctx, `EXPLAIN (ANALYZE, BUFFERS) SELECT * FROM explain_buf_items WHERE id <= 25`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (ANALYZE, BUFFERS): %v", err)
	}
	if len(out.Rows) == 0 {
		t.Fatal("EXPLAIN (ANALYZE, BUFFERS) returned no rows")
	}
	textPlan := out.Rows[0][0].StrVal
	if !strings.Contains(textPlan, "Buffers: shared hit=") || !strings.Contains(textPlan, ", temp read=") {
		t.Fatalf("TEXT plan missing Buffers line:\n%s", textPlan)
	}

	// 2. EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON).
	outJSON, err := database.exec(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT * FROM explain_buf_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON): %v", err)
	}
	var jsonDoc map[string]any
	if err := json.Unmarshal([]byte(outJSON.Rows[0][0].StrVal), &jsonDoc); err != nil {
		t.Fatalf("unmarshal EXPLAIN JSON: %v\n%s", err, outJSON.Rows[0][0].StrVal)
	}
	for _, key := range []string{"Shared Hit Blocks", "Shared Read Blocks", "Temp Read Blocks", "Temp Written Blocks"} {
		if _, ok := jsonDoc[key]; !ok {
			t.Fatalf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) missing key %q in:\n%s", key, outJSON.Rows[0][0].StrVal)
		}
	}

	// 3. EXPLAIN (BUFFERS, FORMAT JSON) without ANALYZE must succeed cleanly.
	outNoAnalyze, err := database.exec(ctx, `EXPLAIN (BUFFERS, FORMAT JSON) SELECT * FROM explain_buf_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (BUFFERS, FORMAT JSON): %v", err)
	}
	var noAnalyzeDoc map[string]any
	if err := json.Unmarshal([]byte(outNoAnalyze.Rows[0][0].StrVal), &noAnalyzeDoc); err != nil {
		t.Fatalf("unmarshal EXPLAIN (BUFFERS, FORMAT JSON): %v", err)
	}
	for _, key := range []string{"Shared Hit Blocks", "Shared Read Blocks", "Temp Read Blocks", "Temp Written Blocks"} {
		if _, ok := noAnalyzeDoc[key]; !ok {
			t.Fatalf("EXPLAIN (BUFFERS, FORMAT JSON) missing key %q", key)
		}
	}

	// 4. YAML and XML formats with BUFFERS.
	outYAML, err := database.exec(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT YAML) SELECT * FROM explain_buf_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (ANALYZE, BUFFERS, FORMAT YAML): %v", err)
	}
	if !strings.Contains(outYAML.Rows[0][0].StrVal, "Shared Hit Blocks:") || !strings.Contains(outYAML.Rows[0][0].StrVal, "Temp Written Blocks:") {
		t.Fatalf("YAML plan missing buffer fields:\n%s", outYAML.Rows[0][0].StrVal)
	}
	outXML, err := database.exec(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT XML) SELECT * FROM explain_buf_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (ANALYZE, BUFFERS, FORMAT XML): %v", err)
	}
	if !strings.Contains(outXML.Rows[0][0].StrVal, "<Shared-Hit-Blocks>") || !strings.Contains(outXML.Rows[0][0].StrVal, "<Temp-Written-Blocks>") {
		t.Fatalf("XML plan missing buffer elements:\n%s", outXML.Rows[0][0].StrVal)
	}

	// 5. Without BUFFERS, JSON omits buffer keys.
	outPlain, err := database.exec(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) SELECT * FROM explain_buf_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN (ANALYZE, FORMAT JSON): %v", err)
	}
	if strings.Contains(outPlain.Rows[0][0].StrVal, "Shared Hit Blocks") {
		t.Fatalf("EXPLAIN without BUFFERS rendered buffer keys:\n%s", outPlain.Rows[0][0].StrVal)
	}
}

func TestAutoAnalyzeTrackerUnit(t *testing.T) {
	tracker := newAutoAnalyzeTracker()
	tracker.RecordTableMutation("public", "orders", 49)
	if due := tracker.DueTables(50, 0.2); len(due) != 0 {
		t.Fatalf("due below threshold = %+v, want empty", due)
	}
	tracker.RecordTableMutation("", "orders", 1)
	due := tracker.DueTables(50, 0.2)
	if len(due) != 1 || due[0] != (tableTarget{schema: "public", table: "orders"}) {
		t.Fatalf("due at threshold = %+v, want [public.orders]", due)
	}

	// After analyzing 100 live rows, next threshold is 50 + 0.2*100 = 70.
	tracker.RecordTableAnalyzed("public", "orders", 100)
	if due := tracker.DueTables(50, 0.2); len(due) != 0 {
		t.Fatalf("due right after analyze = %+v, want empty", due)
	}
	tracker.RecordTableMutation("public", "orders", 69)
	if due := tracker.DueTables(50, 0.2); len(due) != 0 {
		t.Fatalf("due at 69/70 = %+v, want empty", due)
	}
	tracker.RecordTableMutation("public", "orders", 1)
	due = tracker.DueTables(50, 0.2)
	if len(due) != 1 || due[0].table != "orders" {
		t.Fatalf("due at 70/70 = %+v, want [public.orders]", due)
	}

	// System and temp schemas are ignored.
	tracker.RecordTableMutation("pg_catalog", "pg_class", 500)
	tracker.RecordTableMutation("pg_temp_1", "scratch", 500)
	if due := tracker.DueTables(1, 0); len(due) != 1 {
		t.Fatalf("ignored schemas leaked into due list: %+v", due)
	}

	tracker.RemoveTable("public", "orders")
	if due := tracker.DueTables(1, 0); len(due) != 0 {
		t.Fatalf("removed table still due: %+v", due)
	}
}

func TestAutoAnalyzeTriggersAndUpdatesStats(t *testing.T) {
	ctx := t.Context()
	// Background maintenance loop auto-ANALYZE test.
	database, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     retentionTestClock(),
		Maintenance: MaintenanceConfig{
			Interval:               MinMaintenanceInterval,
			AutoAnalyzeMinRows:     25,
			AutoAnalyzeScaleFactor: 0.2,
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if _, err := database.exec(ctx, `CREATE TABLE auto_analyze_items (id int PRIMARY KEY, category int)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := database.exec(ctx, `INSERT INTO auto_analyze_items SELECT g, g % 5 FROM generate_series(1, 40) g`, 0); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for database.Metrics(ctx).AutoAnalyzeRuns == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("background auto-ANALYZE did not run within 5s (metrics=%+v)", database.Metrics(ctx))
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Verify pg_stats was populated by auto-ANALYZE.
	statsOut, err := database.exec(ctx, `SELECT count(*) FROM pg_stats WHERE tablename = 'auto_analyze_items'`, 0)
	if err != nil {
		t.Fatalf("query pg_stats: %v", err)
	}
	if len(statsOut.Rows) != 1 || statsOut.Rows[0][0].IntVal == 0 {
		t.Fatalf("pg_stats for auto_analyze_items empty after auto-ANALYZE: %+v", statsOut.Rows)
	}

	// Verify EXPLAIN cardinality reflects the analyzed 40 rows.
	explainOut, err := database.exec(ctx, `EXPLAIN SELECT * FROM auto_analyze_items`, 0)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	rowRe := regexp.MustCompile(`"Rows":(\d+)`)
	match := rowRe.FindStringSubmatch(explainOut.Rows[0][0].StrVal)
	if len(match) != 2 {
		t.Fatalf("EXPLAIN missing Rows estimate:\n%s", explainOut.Rows[0][0].StrVal)
	}
	estRows, err := strconv.Atoi(match[1])
	if err != nil || estRows != 40 {
		t.Fatalf("EXPLAIN estimated rows=%q, want 40\n%s", match[1], explainOut.Rows[0][0].StrVal)
	}

	// Verify direct RunAutoAnalyze with background maintenance disabled.
	manualDB, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     newTestClock(),
		Maintenance: MaintenanceConfig{
			Disabled:               true,
			AutoAnalyzeMinRows:     10,
			AutoAnalyzeScaleFactor: 0.2,
		},
	})
	if err != nil {
		t.Fatalf("Open manualDB: %v", err)
	}
	t.Cleanup(func() {
		if err := manualDB.Close(); err != nil {
			t.Errorf("Close manualDB: %v", err)
		}
	})
	if _, err := manualDB.exec(ctx, `CREATE TABLE manual_items (id int PRIMARY KEY, v int)`, 0); err != nil {
		t.Fatalf("CREATE TABLE manual_items: %v", err)
	}
	if _, err := manualDB.exec(ctx, `INSERT INTO manual_items SELECT g, g FROM generate_series(1, 15) g`, 0); err != nil {
		t.Fatalf("INSERT manual_items: %v", err)
	}
	analyzed, err := manualDB.RunAutoAnalyze(ctx)
	if err != nil {
		t.Fatalf("RunAutoAnalyze: %v", err)
	}
	if analyzed != 1 {
		t.Fatalf("RunAutoAnalyze = %d, want 1", analyzed)
	}
	analyzedAgain, err := manualDB.RunAutoAnalyze(ctx)
	if err != nil {
		t.Fatalf("second RunAutoAnalyze: %v", err)
	}
	if analyzedAgain != 0 {
		t.Fatalf("second RunAutoAnalyze = %d, want 0", analyzedAgain)
	}
	if got := manualDB.Metrics(ctx).AutoAnalyzeRuns; got != 1 {
		t.Fatalf("manualDB AutoAnalyzeRuns = %d, want 1", got)
	}

	// Verify AutoAnalyzeDisabled prevents background runs.
	disabledDB, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     retentionTestClock(),
		Maintenance: MaintenanceConfig{
			Interval:            MinMaintenanceInterval,
			AutoAnalyzeDisabled: true,
			AutoAnalyzeMinRows:  5,
		},
	})
	if err != nil {
		t.Fatalf("Open disabledDB: %v", err)
	}
	t.Cleanup(func() {
		if err := disabledDB.Close(); err != nil {
			t.Errorf("Close disabledDB: %v", err)
		}
	})
	if _, err := disabledDB.exec(ctx, `CREATE TABLE no_auto (id int PRIMARY KEY)`, 0); err != nil {
		t.Fatalf("CREATE TABLE no_auto: %v", err)
	}
	if _, err := disabledDB.exec(ctx, `INSERT INTO no_auto SELECT g FROM generate_series(1, 20) g`, 0); err != nil {
		t.Fatalf("INSERT no_auto: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := disabledDB.Metrics(ctx).AutoAnalyzeRuns; got != 0 {
		t.Fatalf("AutoAnalyzeDisabled ran %d times, want 0", got)
	}

	// Verify invalid negative configuration options are rejected.
	if bad, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{AutoAnalyzeScaleFactor: -0.1},
	}); err == nil {
		_ = bad.Close()
		t.Fatal("Open accepted negative AutoAnalyzeScaleFactor")
	} else if SQLState(err) != "22023" {
		t.Fatalf("negative AutoAnalyzeScaleFactor SQLSTATE = %q, want 22023 (%v)", SQLState(err), err)
	}
	if bad, err := Open(ctx, OpenOptions{
		Directory:          testDirectory(t),
		Clock:              newTestClock(),
		SlowQueryThreshold: -time.Millisecond,
	}); err == nil {
		_ = bad.Close()
		t.Fatal("Open accepted negative SlowQueryThreshold")
	} else if SQLState(err) != "22023" {
		t.Fatalf("negative SlowQueryThreshold SQLSTATE = %q, want 22023 (%v)", SQLState(err), err)
	}
}

func TestObservabilityQueryObserverAndSlowQuery(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var events []QueryEvent
	var slowEvents []QueryEvent

	database, err := Open(ctx, OpenOptions{
		Directory:          testDirectory(t),
		Clock:              newTestClock(),
		Maintenance:        MaintenanceConfig{Disabled: true},
		SlowQueryThreshold: time.Nanosecond,
		Observer: QueryObserverFunc(func(_ context.Context, event QueryEvent) {
			mu.Lock()
			events = append(events, event)
			mu.Unlock()
		}),
		OnSlowQuery: func(_ context.Context, event QueryEvent) {
			mu.Lock()
			slowEvents = append(slowEvents, event)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	db := sql.OpenDB(NewConnectorWithCatalog(database))
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `CREATE TABLE obs_items (id int PRIMARY KEY, name text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO obs_items VALUES (1, 'a'), (2, 'b'), (3, 'c')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT id, name FROM obs_items ORDER BY id`)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	count := 0
	for rows.Next() {
		var id int
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		count++
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("rows.Close: %v", err)
	}
	if count != 3 {
		t.Fatalf("selected %d rows, want 3", count)
	}

	// Execute a failing query (undefined relation).
	if _, err := db.ExecContext(ctx, `SELECT * FROM obs_does_not_exist`); err == nil {
		t.Fatal("expected error for undefined table")
	}

	// Execute a syntax error through database.exec as well.
	if _, err := database.exec(ctx, `SELECT FROM (`, 0); err == nil {
		t.Fatal("expected syntax error")
	}

	// Execute a cached prepared statement twice to exercise runPreparedCore.
	stmt, err := db.PrepareContext(ctx, `INSERT INTO obs_items VALUES ($1, $2)`)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 4; i <= 5; i++ {
		if _, err := stmt.ExecContext(ctx, i, fmt.Sprintf("p%d", i)); err != nil {
			t.Fatalf("stmt.Exec %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}

	mu.Lock()
	gotEvents := append([]QueryEvent(nil), events...)
	gotSlow := append([]QueryEvent(nil), slowEvents...)
	mu.Unlock()

	if len(gotEvents) != 7 {
		t.Fatalf("observer received %d events, want 7: %+v", len(gotEvents), gotEvents)
	}
	if len(gotSlow) == 0 {
		t.Fatal("OnSlowQuery was never invoked with 1ns threshold")
	}

	var sawInsert3, sawSelect3, sawUndefinedErr, sawSyntaxErr bool
	for _, ev := range gotEvents {
		if ev.Duration <= 0 {
			t.Errorf("expected positive Duration for SQL=%q, got %v", ev.SQL, ev.Duration)
		}
		if strings.Contains(strings.ToUpper(ev.SQL), "INSERT INTO OBS_ITEMS") && ev.RowsAffected == 3 && ev.Err == nil {
			sawInsert3 = true
		}
		if strings.Contains(strings.ToUpper(ev.SQL), "SELECT ID, NAME FROM OBS_ITEMS") && ev.RowsAffected == 3 && ev.Err == nil {
			sawSelect3 = true
		}
		if strings.Contains(strings.ToLower(ev.SQL), "obs_does_not_exist") && ev.Err != nil {
			var pubErr *Error
			if !errors.As(ev.Err, &pubErr) || pubErr.Code != "42P01" {
				t.Errorf("undefined table event Err = %v, want *supersql.Error 42P01", ev.Err)
			}
			sawUndefinedErr = true
		}
		if ev.SQL == "SELECT FROM (" && ev.Err != nil {
			sawSyntaxErr = true
		}
	}
	if !sawInsert3 || !sawSelect3 || !sawUndefinedErr || !sawSyntaxErr {
		t.Fatalf("missing expected query events: insert3=%v select3=%v undefErr=%v syntaxErr=%v\nevents=%+v",
			sawInsert3, sawSelect3, sawUndefinedErr, sawSyntaxErr, gotEvents)
	}

	metrics := database.Metrics(ctx)
	if metrics.QueriesTotal != 7 {
		t.Fatalf("Metrics.QueriesTotal = %d, want 7", metrics.QueriesTotal)
	}
	if metrics.QueriesFailed != 2 {
		t.Fatalf("Metrics.QueriesFailed = %d, want 2", metrics.QueriesFailed)
	}
	if metrics.SlowQueriesTotal == 0 {
		t.Fatalf("Metrics.SlowQueriesTotal = 0, want > 0")
	}
}

func TestObservabilityMetricsConcurrentRace(t *testing.T) {
	ctx := t.Context()
	var observed atomic.Uint64
	database, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     newTestClock(),
		Maintenance: MaintenanceConfig{
			Disabled:               true,
			AutoAnalyzeMinRows:     5,
			AutoAnalyzeScaleFactor: 0.1,
		},
		SlowQueryThreshold: 50 * time.Millisecond,
		Observer: QueryObserverFunc(func(_ context.Context, _ QueryEvent) {
			observed.Add(1)
		}),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	db := sql.OpenDB(NewConnectorWithCatalog(database))
	db.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.ExecContext(ctx, `CREATE TABLE race_metrics (id int PRIMARY KEY, v int)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const workers = 4
	const perWorker = 10
	var wg sync.WaitGroup
	errCh := make(chan error, workers*2+2)

	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range perWorker {
				id := worker*1000 + i
				if _, err := db.ExecContext(ctx, `INSERT INTO race_metrics VALUES ($1, $2)`, id, i); err != nil {
					errCh <- fmt.Errorf("worker %d insert %d: %w", worker, i, err)
					return
				}
				var got int
				if err := db.QueryRowContext(ctx, `SELECT v FROM race_metrics WHERE id = $1`, id).Scan(&got); err != nil {
					errCh <- fmt.Errorf("worker %d select %d: %w", worker, i, err)
					return
				}
				_ = database.Metrics(ctx)
			}
		}(w)
	}

	// Concurrent RunAutoAnalyze + Metrics poller.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 8 {
			_, _ = database.RunAutoAnalyze(ctx)
			_ = database.Metrics(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Run one final auto-ANALYZE pass to guarantee at least one run completed.
	if _, err := database.RunAutoAnalyze(ctx); err != nil {
		t.Fatalf("final RunAutoAnalyze: %v", err)
	}

	metrics := database.Metrics(ctx)
	wantQueries := uint64(1 + workers*perWorker*2)
	if metrics.QueriesTotal != wantQueries {
		t.Fatalf("Metrics.QueriesTotal = %d, want %d", metrics.QueriesTotal, wantQueries)
	}
	if observed.Load() != wantQueries {
		t.Fatalf("observer calls = %d, want %d", observed.Load(), wantQueries)
	}
	if metrics.QueriesFailed != 0 {
		t.Fatalf("Metrics.QueriesFailed = %d, want 0", metrics.QueriesFailed)
	}
	if metrics.AutoAnalyzeRuns == 0 {
		t.Fatalf("Metrics.AutoAnalyzeRuns = 0, want >= 1")
	}
	if metrics.WALSegments == 0 && metrics.LiveRunFiles == 0 {
		t.Fatalf("expected non-zero WALSegments or LiveRunFiles after writes: %+v", metrics)
	}
}
