package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func queryListeningChannels(t *testing.T, ctx context.Context, conn *sql.Conn) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, `SELECT * FROM pg_listening_channels()`)
	if err != nil {
		t.Fatalf("SELECT * FROM pg_listening_channels(): %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			t.Fatalf("Scan channel: %v", err)
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return out
}

func waitSQLConnNotification(ctx context.Context, conn *sql.Conn) (Notification, error) {
	var got Notification
	err := conn.Raw(func(driverConn any) error {
		w, ok := driverConn.(interface {
			WaitForNotification(context.Context) (Notification, error)
		})
		if !ok {
			return errors.New("driverConn does not implement WaitForNotification")
		}
		var err error
		got, err = w.WaitForNotification(ctx)
		return err
	})
	return got, err
}

func TestNotifyListenUnlistenAndBuiltins(t *testing.T) {
	ctx := t.Context()
	db, connector := openTestDB(t)
	_ = connector

	listener, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	sender, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn sender: %v", err)
	}
	defer func() { _ = sender.Close() }()

	// LISTEN + pg_listening_channels().
	if _, err := listener.ExecContext(ctx, `LISTEN Orders`); err != nil {
		t.Fatalf("LISTEN Orders: %v", err)
	}
	if _, err := listener.ExecContext(ctx, `LISTEN alerts`); err != nil {
		t.Fatalf("LISTEN alerts: %v", err)
	}
	if got := queryListeningChannels(t, ctx, listener); !slices.Equal(got, []string{"alerts", "orders"}) {
		t.Fatalf("pg_listening_channels() = %v, want [alerts orders]", got)
	}

	// Also verify target-list SRF form: SELECT pg_listening_channels().
	rows, err := listener.QueryContext(ctx, `SELECT pg_listening_channels()`)
	if err != nil {
		t.Fatalf("SELECT pg_listening_channels(): %v", err)
	}
	var srfChannels []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			t.Fatalf("Scan srf channel: %v", err)
		}
		srfChannels = append(srfChannels, ch)
	}
	_ = rows.Close()
	if !slices.Equal(srfChannels, []string{"alerts", "orders"}) {
		t.Fatalf("target-list pg_listening_channels() = %v, want [alerts orders]", srfChannels)
	}

	// Autocommit NOTIFY with payload.
	if _, err := sender.ExecContext(ctx, `NOTIFY orders, 'created:1'`); err != nil {
		t.Fatalf("NOTIFY: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, err := waitSQLConnNotification(waitCtx, listener)
	if err != nil {
		t.Fatalf("WaitForNotification 1: %v", err)
	}
	if n.Channel != "orders" || n.Payload != "created:1" {
		t.Fatalf("got %+v, want orders/created:1", n)
	}

	// pg_notify(text, text) and pg_notify(text, NULL).
	if _, err := sender.ExecContext(ctx, `SELECT pg_notify('alerts', 'disk-full')`); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
	n, err = waitSQLConnNotification(waitCtx, listener)
	if err != nil {
		t.Fatalf("WaitForNotification 2: %v", err)
	}
	if n.Channel != "alerts" || n.Payload != "disk-full" {
		t.Fatalf("got %+v, want alerts/disk-full", n)
	}

	if _, err := sender.ExecContext(ctx, `SELECT pg_notify('alerts', NULL)`); err != nil {
		t.Fatalf("pg_notify NULL payload: %v", err)
	}
	n, err = waitSQLConnNotification(waitCtx, listener)
	if err != nil {
		t.Fatalf("WaitForNotification 3: %v", err)
	}
	if n.Channel != "alerts" || n.Payload != "" {
		t.Fatalf("got %+v, want alerts/empty", n)
	}

	// Validation: empty/NULL channel -> 22023; payload > 8000 bytes -> 54000.
	if _, err := sender.ExecContext(ctx, `SELECT pg_notify(NULL, 'x')`); SQLState(err) != "22023" {
		t.Fatalf("pg_notify(NULL, ...) SQLSTATE = %q (err=%v), want 22023", SQLState(err), err)
	}
	if _, err := sender.ExecContext(ctx, `SELECT pg_notify('', 'x')`); SQLState(err) != "22023" {
		t.Fatalf("pg_notify('', ...) SQLSTATE = %q (err=%v), want 22023", SQLState(err), err)
	}
	bigPayload := strings.Repeat("x", 8001)
	if _, err := sender.ExecContext(ctx, `SELECT pg_notify('alerts', $1)`, bigPayload); SQLState(err) != "54000" {
		t.Fatalf("pg_notify(>8000) SQLSTATE = %q (err=%v), want 54000", SQLState(err), err)
	}
	if _, err := sender.ExecContext(ctx, fmt.Sprintf(`NOTIFY alerts, '%s'`, bigPayload)); SQLState(err) != "54000" {
		t.Fatalf("NOTIFY >8000 SQLSTATE = %q (err=%v), want 54000", SQLState(err), err)
	}

	// UNLISTEN single channel, then UNLISTEN *.
	if _, err := listener.ExecContext(ctx, `UNLISTEN orders`); err != nil {
		t.Fatalf("UNLISTEN orders: %v", err)
	}
	if got := queryListeningChannels(t, ctx, listener); !slices.Equal(got, []string{"alerts"}) {
		t.Fatalf("after UNLISTEN orders = %v, want [alerts]", got)
	}
	if _, err := listener.ExecContext(ctx, `UNLISTEN *`); err != nil {
		t.Fatalf("UNLISTEN *: %v", err)
	}
	if got := queryListeningChannels(t, ctx, listener); len(got) != 0 {
		t.Fatalf("after UNLISTEN * = %v, want empty", got)
	}
}

func TestNotifyTransactionalSemantics(t *testing.T) {
	ctx := t.Context()
	db, connector := openTestDB(t)

	sub := connector.Catalog().Subscribe("txch")
	defer sub.Close()

	sender, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn: %v", err)
	}
	defer func() { _ = sender.Close() }()

	// 1. ROLLBACK suppresses all staged notifications.
	tx, err := sender.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `NOTIFY txch, 'aborted-1'`); err != nil {
		t.Fatalf("NOTIFY in tx1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('txch', 'aborted-2')`); err != nil {
		t.Fatalf("pg_notify in tx1: %v", err)
	}
	// Must NOT be visible before commit.
	select {
	case n := <-sub.C():
		t.Fatalf("received uncommitted notification: %+v", n)
	case <-time.After(30 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback tx1: %v", err)
	}
	select {
	case n := <-sub.C():
		t.Fatalf("received rolled-back notification: %+v", n)
	case <-time.After(30 * time.Millisecond):
	}

	// 2. SAVEPOINT + ROLLBACK TO SAVEPOINT discards notifications staged after savepoint,
	//    while COMMIT delivers remaining notifications deduplicated per (channel, payload).
	tx, err = sender.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 2: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `NOTIFY txch, 'kept-1'`); err != nil {
		t.Fatalf("NOTIFY kept-1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `NOTIFY txch, 'kept-1'`); err != nil {
		t.Fatalf("NOTIFY kept-1 dup: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT sp1`); err != nil {
		t.Fatalf("SAVEPOINT sp1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `NOTIFY txch, 'discarded-1'`); err != nil {
		t.Fatalf("NOTIFY discarded-1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('txch', 'discarded-2')`); err != nil {
		t.Fatalf("pg_notify discarded-2: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp1`); err != nil {
		t.Fatalf("ROLLBACK TO SAVEPOINT sp1: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_notify('txch', 'kept-2')`); err != nil {
		t.Fatalf("pg_notify kept-2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit tx2: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n1, err := sub.Wait(waitCtx)
	if err != nil || n1.Payload != "kept-1" {
		t.Fatalf("n1 = (%+v, %v), want kept-1", n1, err)
	}
	n2, err := sub.Wait(waitCtx)
	if err != nil || n2.Payload != "kept-2" {
		t.Fatalf("n2 = (%+v, %v), want kept-2", n2, err)
	}
	select {
	case extra := <-sub.C():
		t.Fatalf("unexpected extra notification: %+v", extra)
	case <-time.After(30 * time.Millisecond):
	}

	// 3. Transactional LISTEN/UNLISTEN rolled back on ROLLBACK and applied on COMMIT.
	tx, err = sender.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 3: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `LISTEN ephemeral_ch`); err != nil {
		t.Fatalf("LISTEN in tx3: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback tx3: %v", err)
	}
	if got := queryListeningChannels(t, ctx, sender); len(got) != 0 {
		t.Fatalf("rolled-back LISTEN leaked: %v", got)
	}

	tx, err = sender.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx 4: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `LISTEN committed_ch`); err != nil {
		t.Fatalf("LISTEN in tx4: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit tx4: %v", err)
	}
	if got := queryListeningChannels(t, ctx, sender); !slices.Equal(got, []string{"committed_ch"}) {
		t.Fatalf("committed LISTEN = %v, want [committed_ch]", got)
	}
}

func TestNotifyCatalogSubscribeAndConcurrentFanout(t *testing.T) {
	ctx := t.Context()
	db, connector := openTestDB(t)
	cat := connector.Catalog()

	const numSubscribers = 4
	const numSenders = 3
	const msgsPerSender = 10
	const totalExpected = numSenders * msgsPerSender

	subs := make([]*Subscription, numSubscribers)
	for i := range numSubscribers {
		subs[i] = cat.Subscribe("fanout")
		defer subs[i].Close()
	}

	// Also verify direct Go Catalog.Notify.
	directSub := cat.Subscribe("direct")
	defer directSub.Close()
	if err := cat.Notify("direct", "go-hello"); err != nil {
		t.Fatalf("Catalog.Notify: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	dn, err := directSub.Wait(waitCtx)
	if err != nil || dn.Channel != "direct" || dn.Payload != "go-hello" {
		t.Fatalf("directSub = (%+v, %v)", dn, err)
	}

	var sendWG sync.WaitGroup
	errCh := make(chan error, numSenders)
	for s := range numSenders {
		sendWG.Add(1)
		go func(senderIdx int) {
			defer sendWG.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				errCh <- err
				return
			}
			defer func() { _ = conn.Close() }()
			for m := range msgsPerSender {
				payload := fmt.Sprintf("s%d-m%d", senderIdx, m)
				if _, err := conn.ExecContext(ctx, `SELECT pg_notify('fanout', $1)`, payload); err != nil {
					errCh <- err
					return
				}
			}
		}(s)
	}
	sendWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent sender error: %v", err)
	}

	drainCtx, drainCancel := context.WithTimeout(ctx, 5*time.Second)
	defer drainCancel()
	for idx, sub := range subs {
		seen := make(map[string]bool, totalExpected)
		for range totalExpected {
			n, err := sub.Wait(drainCtx)
			if err != nil {
				t.Fatalf("subscriber %d Wait failed after %d msgs: %v", idx, len(seen), err)
			}
			if n.Channel != "fanout" {
				t.Fatalf("subscriber %d channel = %q", idx, n.Channel)
			}
			seen[n.Payload] = true
		}
		if len(seen) != totalExpected {
			t.Fatalf("subscriber %d got %d unique payloads, want %d", idx, len(seen), totalExpected)
		}
	}
}

func TestNotifyCatalogCloseTerminatesSubscriptions(t *testing.T) {
	ctx := t.Context()
	cat, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sub := cat.Subscribe("shutdown_ch")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sub.C() {
		}
	}()

	if err := cat.Close(); err != nil {
		t.Fatalf("Catalog.Close: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Catalog.Close() did not close active Subscription channel")
	}
	sub.Close()

	// Post-close Subscribe must return an already-closed subscription without spawning a goroutine.
	post := cat.Subscribe("shutdown_ch")
	if _, ok := <-post.C(); ok {
		t.Fatal("expected post-close Subscribe channel to be closed")
	}
}
