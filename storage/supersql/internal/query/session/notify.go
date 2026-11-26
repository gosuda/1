package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/notify"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// NotifyConfig wires a session's Transactions controller into the database-wide
// in-process LISTEN/NOTIFY hub.
type NotifyConfig struct {
	Hub       *notify.Hub
	SessionID uint64
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
		transactions.pendingNotifications = append(transactions.pendingNotifications, notify.PendingNotification{
			Channel: ch,
			Payload: payload,
		})
		return nil
	}
	transactions.notify.Hub.Publish(transactions.notify.SessionID, []notify.PendingNotification{
		{Channel: ch, Payload: payload},
	})
	return nil
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
	clear(transactions.pendingNotifications)
	transactions.pendingNotifications = transactions.pendingNotifications[:0]
	clear(transactions.notifyMarks)
	transactions.notifyMarks = transactions.notifyMarks[:0]
	clear(transactions.pendingListenOps)
	transactions.pendingListenOps = transactions.pendingListenOps[:0]
	clear(transactions.listenMarks)
	transactions.listenMarks = transactions.listenMarks[:0]
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
			transactions.pendingListenOps = append(transactions.pendingListenOps, pendingListenOp{
				listen:  true,
				channel: ch,
			})
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
			transactions.pendingListenOps = append(transactions.pendingListenOps, pendingListenOp{
				listen:  false,
				channel: ch,
			})
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
			transactions.pendingNotifications = append(transactions.pendingNotifications, notify.PendingNotification{
				Channel: ch,
				Payload: notifyStmt.Payload,
			})
		} else {
			hub.Publish(sessionID, []notify.PendingNotification{
				{Channel: ch, Payload: notifyStmt.Payload},
			})
		}
		return ExecutionResult{Tag: "NOTIFY"}, true, rejectPreparedEnvironment(options.Execution.Environment, nil)
	}
}
