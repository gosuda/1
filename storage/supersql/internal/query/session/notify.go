package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/notify"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	// maxPendingNotifications bounds queued NOTIFY/pg_notify entries per transaction.
	maxPendingNotifications = 16384
	// maxPendingNotifyBytes bounds total channel+payload bytes staged per transaction (8 MiB).
	maxPendingNotifyBytes = 8 << 20
	// maxPendingListenOps bounds queued LISTEN/UNLISTEN operations per transaction.
	maxPendingListenOps = 16384
	// maxRetainedNotifySliceCap drops oversized backing arrays in finishNotify so a
	// single large transaction does not pin memory for the lifetime of the session.
	maxRetainedNotifySliceCap = 256
)

// NotifyConfig wires a session's Transactions controller into the database-wide
// in-process LISTEN/NOTIFY hub.
type NotifyConfig struct {
	Hub       *notify.Hub
	SessionID uint64

	pendingNotifyBytes int
	pendingNotifyCount int
}

type pendingListenOp struct {
	listen  bool
	channel string
}

type sessionNotifyRuntime struct {
	transactions *Transactions
}

var _ builtin.NotifyRuntime = sessionNotifyRuntime{}

func (rt sessionNotifyRuntime) Notify(channel, payload string) error {
	if rt.transactions == nil {
		return &model.Error{Code: "55000", Message: "notification runtime is unavailable"}
	}
	return rt.transactions.stageNotify(channel, payload)
}

func (rt sessionNotifyRuntime) ListeningChannels() []string {
	if rt.transactions == nil || rt.transactions.notify.Hub == nil {
		return nil
	}
	return rt.transactions.notify.Hub.ListeningChannels(rt.transactions.notify.SessionID)
}

func (transactions *Transactions) notifyRuntime() builtin.NotifyRuntime {
	if transactions == nil || transactions.notify.Hub == nil {
		return nil
	}
	return sessionNotifyRuntime{transactions: transactions}
}

func pendingNotifyEntryBytes(channel, payload string) int {
	return len(channel) + len(payload)
}

// syncPendingNotifyBytes reconciles the cached byte total with the slice length.
// Rollbacks (savepoint, statement, or routine subtransaction) only truncate
// pendingNotifications, strictly shrinking len below pendingNotifyCount, so a
// mismatch reliably triggers an exact recomputation on the next append or read.
func (transactions *Transactions) syncPendingNotifyBytes() int {
	if len(transactions.pendingNotifications) != transactions.notify.pendingNotifyCount {
		total := 0
		for _, item := range transactions.pendingNotifications {
			total += pendingNotifyEntryBytes(item.Channel, item.Payload)
		}
		transactions.notify.pendingNotifyBytes = total
		transactions.notify.pendingNotifyCount = len(transactions.pendingNotifications)
	}
	return transactions.notify.pendingNotifyBytes
}

func (transactions *Transactions) appendPendingNotification(channel, payload string) error {
	usedBytes := transactions.syncPendingNotifyBytes()
	if len(transactions.pendingNotifications) >= maxPendingNotifications {
		return &model.Error{Code: "54000", Message: "NOTIFY queue is full"}
	}
	entryBytes := pendingNotifyEntryBytes(channel, payload)
	if usedBytes+entryBytes > maxPendingNotifyBytes {
		return &model.Error{Code: "54000", Message: "NOTIFY queue is full"}
	}
	transactions.pendingNotifications = append(transactions.pendingNotifications, notify.PendingNotification{
		Channel: channel,
		Payload: payload,
	})
	transactions.notify.pendingNotifyBytes = usedBytes + entryBytes
	transactions.notify.pendingNotifyCount = len(transactions.pendingNotifications)
	return nil
}

func (transactions *Transactions) appendPendingListenOp(listen bool, channel string) error {
	if len(transactions.pendingListenOps) >= maxPendingListenOps {
		return &model.Error{Code: "54000", Message: "pending LISTEN/UNLISTEN operation limit exceeded"}
	}
	transactions.pendingListenOps = append(transactions.pendingListenOps, pendingListenOp{
		listen:  listen,
		channel: channel,
	})
	return nil
}

func (transactions *Transactions) stageNotify(channel, payload string) error {
	ch, err := builtin.ValidateNotifyChannel(channel)
	if err != nil {
		return err
	}
	if err := builtin.ValidateNotifyPayload(payload); err != nil {
		return err
	}
	if transactions.notify.Hub == nil {
		return &model.Error{Code: "55000", Message: "notification hub is unavailable"}
	}
	if transactions.active {
		return transactions.appendPendingNotification(ch, payload)
	}
	transactions.notify.Hub.Publish(transactions.notify.SessionID, []notify.PendingNotification{
		{Channel: ch, Payload: payload},
	})
	return nil
}

func resetOrReleaseSlice[T any](s []T) []T {
	if cap(s) > maxRetainedNotifySliceCap {
		return nil
	}
	clear(s)
	return s[:0]
}

func (transactions *Transactions) finishNotify(committed bool) {
	hub := transactions.notify.Hub
	sessionID := transactions.notify.SessionID
	if committed && hub != nil {
		for _, op := range transactions.pendingListenOps {
			if op.listen {
				hub.Listen(sessionID, op.channel)
			} else {
				hub.Unlisten(sessionID, op.channel)
			}
		}
		if len(transactions.pendingNotifications) > 0 {
			hub.Publish(sessionID, transactions.pendingNotifications)
		}
	}
	transactions.pendingNotifications = resetOrReleaseSlice(transactions.pendingNotifications)
	transactions.notifyMarks = resetOrReleaseSlice(transactions.notifyMarks)
	transactions.pendingListenOps = resetOrReleaseSlice(transactions.pendingListenOps)
	transactions.listenMarks = resetOrReleaseSlice(transactions.listenMarks)
	transactions.notify.pendingNotifyBytes = 0
	transactions.notify.pendingNotifyCount = 0
}

func (transactions *Transactions) resetNotify() {
	if transactions.notify.Hub == nil {
		return
	}
	transactions.notify.Hub.Unlisten(transactions.notify.SessionID, "*")
	transactions.notify.Hub.EnsureSession(transactions.notify.SessionID).Drain()
}

func (transactions *Transactions) runNotifyCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil {
		return ExecutionResult{}, false, nil
	}
	root := statement.Stmt
	listenStmt := root.GetListenStmt()
	notifyStmt := root.GetNotifyStmt()
	unlistenStmt := root.GetUnlistenStmt()
	if listenStmt == nil && notifyStmt == nil && unlistenStmt == nil {
		return ExecutionResult{}, false, nil
	}
	reject := func(err error) (ExecutionResult, bool, error) {
		return ExecutionResult{}, true, errors.Join(err, rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	_, finish, err := transactions.enter(ctx)
	if err != nil {
		return reject(err)
	}
	defer finish()

	if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
		err := &model.Error{Code: "42P02", Message: "utility command does not accept parameters"}
		if transactions.active {
			transactions.markFailed(err)
		}
		return reject(err)
	}

	if transactions.active {
		if err := transactions.ordinary(); err != nil {
			return reject(err)
		}
	}
	hub := transactions.notify.Hub
	if hub == nil {
		err := &model.Error{Code: "55000", Message: "notification hub is unavailable"}
		if transactions.active {
			transactions.markFailed(err)
		}
		return reject(err)
	}
	sessionID := transactions.notify.SessionID

	switch {
	case listenStmt != nil:
		ch, err := builtin.ValidateNotifyChannel(listenStmt.Conditionname)
		if err != nil {
			if transactions.active {
				transactions.markFailed(err)
			}
			return reject(err)
		}
		if transactions.active {
			if err := transactions.appendPendingListenOp(true, ch); err != nil {
				transactions.markFailed(err)
				return reject(err)
			}
		} else {
			hub.Listen(sessionID, ch)
		}
		return ExecutionResult{Tag: "LISTEN"}, true, rejectPreparedEnvironment(options.Execution.Environment, nil)

	case unlistenStmt != nil:
		ch := unlistenStmt.Conditionname
		if ch != "" && ch != "*" {
			validated, err := builtin.ValidateNotifyChannel(ch)
			if err != nil {
				if transactions.active {
					transactions.markFailed(err)
				}
				return reject(err)
			}
			ch = validated
		}
		if transactions.active {
			if err := transactions.appendPendingListenOp(false, ch); err != nil {
				transactions.markFailed(err)
				return reject(err)
			}
		} else {
			hub.Unlisten(sessionID, ch)
		}
		return ExecutionResult{Tag: "UNLISTEN"}, true, rejectPreparedEnvironment(options.Execution.Environment, nil)

	default: // notifyStmt != nil
		ch, err := builtin.ValidateNotifyChannel(notifyStmt.Conditionname)
		if err != nil {
			if transactions.active {
				transactions.markFailed(err)
			}
			return reject(err)
		}
		if err := builtin.ValidateNotifyPayload(notifyStmt.Payload); err != nil {
			if transactions.active {
				transactions.markFailed(err)
			}
			return reject(err)
		}
		if transactions.active {
			if err := transactions.appendPendingNotification(ch, notifyStmt.Payload); err != nil {
				transactions.markFailed(err)
				return reject(err)
			}
		} else {
			hub.Publish(sessionID, []notify.PendingNotification{
				{Channel: ch, Payload: notifyStmt.Payload},
			})
		}
		return ExecutionResult{Tag: "NOTIFY"}, true, rejectPreparedEnvironment(options.Execution.Environment, nil)
	}
}
