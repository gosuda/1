package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sqlMoment struct {
	value model.SQLTimestamp
	err   error
}

// Frames contain no transaction, reader, session, or request context.
type temporalFrame struct {
	clock       clock.Clock
	transaction sqlMoment
	statement   sqlMoment
}

func (f temporalFrame) TransactionTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	if err := ctx.Err(); err != nil {
		return 0, sessionTransactionError(err)
	}
	return f.transaction.value, f.transaction.err
}

func (f temporalFrame) StatementTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	if err := ctx.Err(); err != nil {
		return 0, sessionTransactionError(err)
	}
	return f.statement.value, f.statement.err
}

func (f temporalFrame) ClockTimestamp(ctx context.Context) (model.SQLTimestamp, error) {
	moment := readSQLMoment(ctx, f.clock)
	return moment.value, moment.err
}

func temporalClockError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sessionTransactionError(err)
	}
	return &model.Error{Code: "55000", Message: "SQL temporal clock unavailable", Cause: err}
}

func readSQLMoment(ctx context.Context, source clock.Clock) sqlMoment {
	if err := ctx.Err(); err != nil {
		return sqlMoment{err: temporalClockError(err)}
	}
	if historicalNil(source) {
		return sqlMoment{err: temporalClockError(clock.ErrClockUnavailable)}
	}
	reading, err := source.Read(ctx)
	if err != nil {
		return sqlMoment{err: temporalClockError(err)}
	}
	validStatus := reading.Status == clock.Synced || reading.Status == clock.Holdover
	if reading.Earliest < 0 || reading.Latest < reading.Earliest || !validStatus {
		return sqlMoment{err: temporalClockError(clock.ErrClockRange)}
	}
	midpoint := reading.Earliest + (reading.Latest-reading.Earliest)/2
	projection, err := source.ToUTC(midpoint)
	if err != nil {
		return sqlMoment{err: temporalClockError(err)}
	}
	if err := ctx.Err(); err != nil {
		return sqlMoment{err: temporalClockError(err)}
	}
	// Clock.ToUTC projects a leap second onto the preceding UTC second.
	utc := projection.Time.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return sqlMoment{err: temporalClockError(clock.ErrClockRange)}
	}
	const postgresEpochUnixSeconds = 946684800
	micros := (utc.Unix()-postgresEpochUnixSeconds)*1000000 + int64(utc.Nanosecond()/1000)
	return sqlMoment{value: model.SQLTimestamp(micros)}
}

// TemporalRuntime may be called from a Statement callback. The returned immutable
// frame borrows only the configured clock, which must outlive its consumers.
func (transactions *Transactions) TemporalRuntime(ctx context.Context) (model.TemporalRuntime, error) {
	token, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation)
	if !ok || token.owner != transactions || !token.active.Load() {
		return nil, errPreparedInvocation
	}
	if err := ctx.Err(); err != nil {
		return nil, sessionTransactionError(err)
	}
	return transactions.temporal, nil
}
