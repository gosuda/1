package model

import (
	"context"
	"time"
)

// PreparedStatementSummary is borrowed only for the duration of a visitor call.
// ParameterTypes and ResultTypes must not be retained or mutated by the visitor.
type PreparedStatementSummary struct {
	Name           string
	Statement      string
	PreparedAt     time.Time
	ParameterTypes []TypeID
	ResultTypes    []TypeID
	FromSQL        bool
	GenericPlans   uint64
	CustomPlans    uint64
}

// CursorSummary is borrowed only for the duration of a visitor call.
type CursorSummary struct {
	Name       string
	Statement  string
	CreatedAt  time.Time
	Holdable   bool
	Binary     bool
	Scrollable bool
}

// SessionIntrospection visits session-owned state synchronously. Implementations
// propagate cancellation and visitor errors and do not permit summaries to escape.
type SessionIntrospection interface {
	VisitPreparedStatements(context.Context, func(PreparedStatementSummary) error) error
	VisitCursors(context.Context, func(CursorSummary) error) error
}
