package procedural

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

var (
	ErrInvalidProgram = errors.New("invalid procedural program")
	ErrLimit          = errors.New("procedural resource limit exceeded")
	ErrNoSession      = errors.New("procedural SQL requires an active session statement")
)

type Limits struct {
	MaxProgramBytes    uint64
	MaxStatements      int
	MaxDatums          int
	MaxDepth           int
	MaxSteps           int
	MaxRows            int
	MaxResultBytes     uint64
	MaxDynamicSQLBytes int
	MaxTriggerDepth    int
	MaxDeferredEvents  int
	MaxTransitionRows  int
}

func DefaultLimits() Limits {
	return Limits{
		MaxProgramBytes:    16 << 20,
		MaxStatements:      4096,
		MaxDatums:          1024,
		MaxDepth:           64,
		MaxSteps:           1_000_000,
		MaxRows:            1_000_000,
		MaxResultBytes:     64 << 20,
		MaxDynamicSQLBytes: 1 << 20,
		MaxTriggerDepth:    32,
		MaxDeferredEvents:  100_000,
		MaxTransitionRows:  1_000_000,
	}
}

func (l Limits) Validate() error {
	if l.MaxProgramBytes == 0 || l.MaxStatements <= 0 || l.MaxDatums <= 0 || l.MaxDepth <= 0 || l.MaxDepth > 64 || l.MaxSteps <= 0 || l.MaxRows <= 0 || l.MaxResultBytes == 0 || l.MaxDynamicSQLBytes <= 0 || l.MaxTriggerDepth <= 0 || l.MaxDeferredEvents <= 0 || l.MaxTransitionRows <= 0 {
		return ErrLimit
	}
	return nil
}

type Parameter struct {
	Layout model.TypeLayout
	Value  model.Value
}
type TriggerContext struct {
	Name              string
	Timing            string
	Event             string
	Level             string
	TableName         string
	SchemaName        string
	Relation          model.OID
	ConstraintTable   model.ObjectID
	ConstraintID      model.ObjectID
	Arguments         []string
	Layout            model.TypeLayout
	Old               *model.Row
	New               *model.Row
	OldTransitionName string
	NewTransitionName string
	OldTransition     []model.Row
	NewTransition     []model.Row
}

type DeferredTrigger struct {
	Program *Program
	Context TriggerContext
}

// SQLRequest carries a parsed statement and typed values. Implementations own
// all portals; only an explicit CALL/DO request may cross transaction boundaries.
type SQLRequest struct {
	Statement               *zpgparse.RawStmt
	Parameters              []Parameter
	MaxRows                 int
	MaxBytes                uint64
	AllowTransactionControl bool
}

type SQLResult struct {
	Columns      []model.TypeLayout
	ColumnNames  []string
	Rows         [][]model.Value
	RowsAffected uint64
}
type RoutineInvocation struct {
	Procedure       bool
	Atomic          bool
	SecurityDefiner bool
	Result          model.TypeLayout
	Settings        []model.RoutineSetting
}

type TransactionCommand struct {
	Rollback bool
	Chain    bool
}

// SQLExecutor is the only SQL capability visible to routine code. TransactionControl
// succeeds only for a non-atomic top-level CALL/DO invocation.
type SQLExecutor interface {
	RunRoutine(context.Context, RoutineInvocation, func(context.Context) (SQLResult, error)) (SQLResult, error)
	ExecuteSQL(context.Context, SQLRequest) (SQLResult, error)
	RunSubtransaction(context.Context, func(context.Context) error) error
	ValidateRoutineValue(context.Context, model.TypeLayout, model.Value) error
	TransactionControl(context.Context, TransactionCommand) error
	DeferTrigger(context.Context, DeferredTrigger) error
	TriggerDeferred(context.Context, model.TriggerDescriptor) (bool, error)
	InvokeNativeTrigger(context.Context, model.FunctionID, TriggerContext) (*model.Row, bool, error)
	Report(context.Context, model.Error)
}

type sqlExecutorKey struct{}

func WithSQLExecutor(ctx context.Context, executor SQLExecutor) context.Context {
	if executor == nil {
		return ctx
	}
	return context.WithValue(ctx, sqlExecutorKey{}, executor)
}

func SQLExecutorFromContext(ctx context.Context) (SQLExecutor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executor, _ := ctx.Value(sqlExecutorKey{}).(SQLExecutor)
	if executor == nil {
		return nil, ErrNoSession
	}
	return executor, nil
}

func sqlError(code, message string, cause error) error {
	return &model.Error{Code: code, Message: message, Cause: cause}
}
