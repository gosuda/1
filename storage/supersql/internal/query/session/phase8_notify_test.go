package session

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/notify"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func newPhase8NotifyTransactions(t *testing.T) (*Transactions, *notify.Hub) {
	t.Helper()
	ctx := t.Context()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(history),
		CanonicalLeapHistory: history,
	}
	epoch := clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}
	provider := constraintModeClock{
		scale:   scale,
		reading: clock.Reading{Earliest: 100, Latest: 110, Epoch: epoch, Status: clock.Synced},
	}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatalf("NewFenceAuthority: %v", err)
	}
	backend, err := neosepia.Open(ctx, neosepia.Config{
		Directory:     t.TempDir(),
		Scale:         scale,
		KeyEncoder:    unusedConstraintModeEncoder{},
		FenceVerifier: authority,
	})
	if err != nil {
		t.Fatalf("neosepia.Open: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	state, err := backend.State(ctx)
	if err != nil {
		t.Fatalf("backend.State: %v", err)
	}
	manager, err := txn.NewManager(ctx, txn.ManagerConfig{
		Backend:       backend,
		Clock:         &provider,
		Authority:     authority,
		MaxOperations: 64,
		Recovered: txn.RecoveredState{
			LogPosition:  state.LogPosition,
			CommitTS:     state.CommitTS,
			ClosedTS:     state.ClosedTS,
			HistoryFloor: state.HistoryFloor,
			Epoch:        state.Epoch,
			LeapHistory:  state.Scale.LeapHistoryID,
		},
	})
	if err != nil {
		t.Fatalf("txn.NewManager: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	settings := newTestSettings(t)
	hub := notify.NewHub(0)
	t.Cleanup(hub.Close)

	txs, err := NewTransactions(ctx, TransactionConfig{
		Manager:  manager,
		Settings: settings,
		Options: txn.TransactionOptions{
			Mode:         txn.ReadWrite,
			MemoryBytes:  8 << 20,
			DisableSpill: true,
			MaxViews:     16,
			KeyEncoder:   unusedConstraintModeEncoder{},
			Resolver:     ws3PolicyResolver{},
		},
		Notify: NotifyConfig{
			Hub:       hub,
			SessionID: 1,
		},
	})
	if err != nil {
		t.Fatalf("NewTransactions: %v", err)
	}
	t.Cleanup(func() { _ = txs.Close() })
	return txs, hub
}

func requirePhase8SQLState(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with SQLSTATE %s, got nil", want)
	}
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) {
		t.Fatalf("expected *model.Error with SQLSTATE %s, got %T (%v)", want, err, err)
	}
	if sqlErr.Code != want {
		t.Fatalf("SQLSTATE = %q, want %q (err=%v)", sqlErr.Code, want, err)
	}
}

func parsePhase8Single(t *testing.T, sql string) *zpgparse.RawStmt {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil || len(parsed.Stmts) != 1 {
		t.Fatalf("parse %q: err=%v count=%d", sql, err, len(parsed.Stmts))
	}
	return parsed.Stmts[0]
}

func TestPhase8PendingNotificationsCountLimit(t *testing.T) {
	ctx := t.Context()
	txs, _ := newPhase8NotifyTransactions(t)

	if err := txs.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for i := 0; i < maxPendingNotifications; i++ {
		if err := txs.stageNotify("c", ""); err != nil {
			t.Fatalf("stageNotify %d: %v", i, err)
		}
	}
	err := txs.stageNotify("c", "")
	requirePhase8SQLState(t, err, "54000")

	if err := txs.Rollback(ctx, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if cap(txs.pendingNotifications) != 0 {
		t.Fatalf("expected oversized pendingNotifications backing array to be released, got cap=%d", cap(txs.pendingNotifications))
	}
	if txs.notify.pendingNotifyBytes != 0 || txs.notify.pendingNotifyCount != 0 {
		t.Fatalf("expected notify byte/count trackers reset to 0, got bytes=%d count=%d", txs.notify.pendingNotifyBytes, txs.notify.pendingNotifyCount)
	}
}

func TestPhase8PendingNotificationsByteLimitAndSavepointAccounting(t *testing.T) {
	ctx := t.Context()
	txs, hub := newPhase8NotifyTransactions(t)
	sub := hub.SubscribeGo("ch")
	defer sub.Close()

	if err := txs.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	payload := strings.Repeat("a", 8000)
	entrySize := pendingNotifyEntryBytes("ch", payload)
	// Stage ~5 MiB before savepoint.
	beforeCount := (5 << 20) / entrySize
	for i := 0; i < beforeCount; i++ {
		if err := txs.stageNotify("ch", payload); err != nil {
			t.Fatalf("stageNotify before savepoint %d: %v", i, err)
		}
	}
	expectedBefore := beforeCount * entrySize
	if got := txs.syncPendingNotifyBytes(); got != expectedBefore {
		t.Fatalf("pendingNotifyBytes before savepoint = %d, want %d", got, expectedBefore)
	}

	if err := txs.Savepoint(ctx, "sp1"); err != nil {
		t.Fatalf("Savepoint sp1: %v", err)
	}

	// Stage another ~2.5 MiB after savepoint (total ~7.5 MiB < 8 MiB).
	afterCount := (2500 << 10) / entrySize
	for i := 0; i < afterCount; i++ {
		if err := txs.stageNotify("ch", payload); err != nil {
			t.Fatalf("stageNotify after savepoint %d: %v", i, err)
		}
	}

	// Pushing over 8 MiB must fail with SQLSTATE 54000.
	hitLimit := false
	for i := 0; i < 200; i++ {
		if err := txs.stageNotify("ch", payload); err != nil {
			requirePhase8SQLState(t, err, "54000")
			hitLimit = true
			break
		}
	}
	if !hitLimit {
		t.Fatal("expected byte limit to trigger SQLSTATE 54000")
	}

	// Roll back to sp1: byte count must accurately restore to expectedBefore so
	// staging another ~2.5 MiB succeeds instead of being falsely rejected.
	if err := txs.RollbackTo(ctx, "sp1"); err != nil {
		t.Fatalf("RollbackTo sp1: %v", err)
	}
	if got := txs.syncPendingNotifyBytes(); got != expectedBefore {
		t.Fatalf("pendingNotifyBytes after RollbackTo sp1 = %d, want %d", got, expectedBefore)
	}
	for i := 0; i < afterCount; i++ {
		if err := txs.stageNotify("ch", payload); err != nil {
			t.Fatalf("stageNotify after rollback %d: %v", i, err)
		}
	}

	// Release savepoint retains staged bytes.
	expectedAfterRelease := (beforeCount + afterCount) * entrySize
	if err := txs.Release(ctx, "sp1"); err != nil {
		t.Fatalf("Release sp1: %v", err)
	}
	if got := txs.syncPendingNotifyBytes(); got != expectedAfterRelease {
		t.Fatalf("pendingNotifyBytes after Release sp1 = %d, want %d", got, expectedAfterRelease)
	}

	// Commit delivers deduplicated notifications and releases oversized buffer.
	if _, err := txs.Commit(ctx, false); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cap(txs.pendingNotifications) != 0 {
		t.Fatalf("expected oversized pendingNotifications released on Commit, got cap=%d", cap(txs.pendingNotifications))
	}
	if txs.notify.pendingNotifyBytes != 0 || txs.notify.pendingNotifyCount != 0 {
		t.Fatalf("expected trackers reset on Commit, got bytes=%d count=%d", txs.notify.pendingNotifyBytes, txs.notify.pendingNotifyCount)
	}
	delivered := sub.Drain()
	if len(delivered) != 1 || delivered[0].Channel != "ch" || delivered[0].Payload != payload {
		t.Fatalf("expected 1 deduplicated notification, got %d", len(delivered))
	}
}

func TestPhase8PendingListenOpsLimitAndBufferRelease(t *testing.T) {
	ctx := t.Context()
	txs, _ := newPhase8NotifyTransactions(t)

	if err := txs.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	listenStmt := parsePhase8Single(t, "LISTEN ch_limit")
	for i := 0; i < maxPendingListenOps; i++ {
		if _, handled, err := txs.runNotifyCommand(ctx, listenStmt, StatementExecution{}); !handled || err != nil {
			t.Fatalf("LISTEN %d: handled=%v err=%v", i, handled, err)
		}
	}
	_, handled, err := txs.runNotifyCommand(ctx, listenStmt, StatementExecution{})
	if !handled {
		t.Fatal("expected LISTEN to be handled")
	}
	requirePhase8SQLState(t, err, "54000")
	// Transaction must be marked failed (25P02 on next ordinary command).
	_, _, followErr := txs.runNotifyCommand(ctx, parsePhase8Single(t, "NOTIFY ch_limit"), StatementExecution{})
	requirePhase8SQLState(t, followErr, "25P02")

	if err := txs.Rollback(ctx, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if cap(txs.pendingListenOps) != 0 {
		t.Fatalf("expected oversized pendingListenOps released on Rollback, got cap=%d", cap(txs.pendingListenOps))
	}

	// Small transaction keeps reusable backing array (cap <= maxRetainedNotifySliceCap).
	if err := txs.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatalf("Begin small: %v", err)
	}
	if err := txs.stageNotify("small", "hi"); err != nil {
		t.Fatalf("stageNotify small: %v", err)
	}
	if _, err := txs.Commit(ctx, false); err != nil {
		t.Fatalf("Commit small: %v", err)
	}
	if len(txs.pendingNotifications) != 0 || cap(txs.pendingNotifications) == 0 || cap(txs.pendingNotifications) > maxRetainedNotifySliceCap {
		t.Fatalf("expected small buffer retained with len=0 cap in (0,%d], got len=%d cap=%d", maxRetainedNotifySliceCap, len(txs.pendingNotifications), cap(txs.pendingNotifications))
	}
}

func TestPhase8NotifyCommandByteLimitMarksFailed(t *testing.T) {
	ctx := t.Context()
	txs, _ := newPhase8NotifyTransactions(t)

	if err := txs.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	payload := strings.Repeat("z", 8000)
	notifyStmt := parsePhase8Single(t, "NOTIFY ch, '"+payload+"'")
	hit := false
	for i := 0; i < 2000; i++ {
		_, handled, err := txs.runNotifyCommand(ctx, notifyStmt, StatementExecution{})
		if !handled {
			t.Fatal("expected NOTIFY to be handled")
		}
		if err != nil {
			requirePhase8SQLState(t, err, "54000")
			hit = true
			break
		}
	}
	if !hit {
		t.Fatal("expected NOTIFY command to hit byte limit")
	}
	// Verify transaction was marked failed.
	_, _, afterErr := txs.runNotifyCommand(ctx, parsePhase8Single(t, "NOTIFY ch, 'ok'"), StatementExecution{})
	requirePhase8SQLState(t, afterErr, "25P02")
	if err := txs.Rollback(ctx, false); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

func TestPhase8ImplicitReadOnlyUpgradeErrorClassifier(t *testing.T) {
	if isOnlyImplicitReadOnlyUpgrade(nil) {
		t.Fatal("nil must not match upgrade sentinel")
	}
	if !isOnlyImplicitReadOnlyUpgrade(errImplicitReadOnlyUpgrade) {
		t.Fatal("direct sentinel must match")
	}
	if !isOnlyImplicitReadOnlyUpgrade(errors.Join(errImplicitReadOnlyUpgrade, nil)) {
		t.Fatal("joined sentinel must match")
	}
	if isOnlyImplicitReadOnlyUpgrade(errors.Join(errImplicitReadOnlyUpgrade, storage.ErrClosed)) {
		t.Fatal("sentinel joined with real error must not match")
	}
	if isReadOnlyAutocommitSelect(parsePhase8Single(t, "SELECT clock_timestamp()").Stmt) {
		t.Fatal("volatile clock_timestamp() should not trigger speculative read-only hint")
	}
}
