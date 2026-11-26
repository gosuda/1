package session

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Budgets independently admit binding, compiled execution, borrowed inputs and
// owned results. Builtin supplies default invocation settings; generated and
// CHECK expressions use frozen settings. Functions is consulted only while
// compiling. Returned calls must be concurrency-safe, bounded, honor cancellation,
// and retain no catalog snapshot or mutable session state.
type ColumnExpressionOptions struct {
	Binding     ir.Limits
	Evaluation  expr.Limits
	InputBytes  uint64
	ResultBytes uint64
	Builtin     builtin.ResolveOptions
	Functions   expr.Resolver
	// Temporal is borrowed only while binding; compiled programs do not retain it.
	Temporal model.TemporalRuntime
}

func (options ColumnExpressionOptions) ForSnapshot(ctx context.Context, snapshot *catalog.CatalogSnapshot) (schema.ColumnExpressions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewColumnExpressionCompiler(snapshot, options)
}

// ColumnExpressionCompiler borrows the snapshot until compilation finishes.
// Returned programs retain neither this compiler nor the snapshot; every
// evaluation owns a fresh VM state and borrows runtime capabilities only for that call.
type ColumnExpressionCompiler struct {
	snapshot bind.CatalogSnapshot
	options  ColumnExpressionOptions
}

func NewColumnExpressionCompiler(snapshot bind.CatalogSnapshot, options ColumnExpressionOptions) (*ColumnExpressionCompiler, error) {
	if snapshot == nil || options.InputBytes == 0 || options.ResultBytes == 0 {
		return nil, &model.Error{Code: "22023", Message: "column expression compiler requires a snapshot and positive budgets"}
	}
	if _, err := ir.NewBuilder(options.Binding); err != nil {
		return nil, err
	}
	limits := options.Evaluation
	for _, count := range [...]int{limits.MaxInstructions, limits.MaxRegisters, limits.MaxRows} {
		if count <= 0 || count > 1<<31-1 {
			return nil, expr.ErrInvalid
		}
	}
	invalidDepth := limits.MaxDepth <= 0 || limits.MaxDepth > 64
	missingProgramBudget := limits.MaxBytes == 0 || limits.MaxConstantBytes == 0
	missingStateBudget := limits.MaxStateBytes == 0 || limits.MaxOutputBytes == 0
	if invalidDepth || missingProgramBudget || missingStateBudget {
		return nil, expr.ErrInvalid
	}
	options.Evaluation.MaxRows = 1
	return &ColumnExpressionCompiler{snapshot: snapshot, options: options}, nil
}

func (c *ColumnExpressionCompiler) CompileColumnDefault(ctx context.Context, definition *model.ExpressionDefinition, result model.TypeLayout) (txn.ValueExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	binding, err := bind.BindColumnDefault(ctx, definition, result, c.snapshot, bind.Options{Limits: c.options.Binding, Builtin: c.options.Builtin, Collation: c.options.Builtin.Collation, DefaultCollation: c.options.Builtin.DefaultCollation, Temporal: c.options.Temporal})
	if err != nil {
		return nil, err
	}
	return c.compile(ctx, binding, nil, columnDefault)
}

func (c *ColumnExpressionCompiler) CompileStoredGenerated(ctx context.Context, definition *model.ExpressionDefinition, column model.ColumnID, columns []storage.ColumnLayout, result model.TypeLayout) (txn.ValueExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	binding, err := bind.BindStoredGenerated(ctx, definition, column, columns, result, c.snapshot, bind.Options{Limits: c.options.Binding, Builtin: c.options.Builtin, Collation: c.options.Builtin.Collation, DefaultCollation: c.options.Builtin.DefaultCollation, Temporal: c.options.Temporal})
	if err != nil {
		return nil, err
	}
	return c.compile(ctx, binding, columns, columnStoredGenerated)
}

func (c *ColumnExpressionCompiler) CompileCheck(ctx context.Context, definition *model.ExpressionDefinition, constraint model.ObjectID, columns []storage.ColumnLayout) (txn.ValueExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	binding, err := bind.BindCheck(ctx, definition, constraint, columns, c.snapshot, bind.Options{Limits: c.options.Binding, Builtin: c.options.Builtin, Collation: c.options.Builtin.Collation, DefaultCollation: c.options.Builtin.DefaultCollation, Temporal: c.options.Temporal})
	if err != nil {
		return nil, err
	}
	return c.compile(ctx, binding, columns, columnCheck)
}

type columnExpressionPurpose uint8

const (
	columnDefault columnExpressionPurpose = iota
	columnStoredGenerated
	columnCheck
)

func (c *ColumnExpressionCompiler) compile(ctx context.Context, binding *bind.BoundColumnExpression, columns []storage.ColumnLayout, purpose columnExpressionPurpose) (*columnValueExpression, error) {
	if binding == nil || binding.Graph == nil {
		return nil, expr.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, ok := binding.Graph.Scalar(binding.Root); !ok || (purpose == columnDefault && len(binding.Parameters) != 0) {
		return nil, expr.ErrInvalid
	}
	for id := ir.ScalarID(1); uint32(id) <= binding.Graph.ScalarCount(); id++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, _ := binding.Graph.Scalar(id)
		mutableGeneration := purpose == columnStoredGenerated && node.Effects.Volatility != ir.Immutable
		if node.Kind == ir.Subquery || node.Kind == ir.Slot || mutableGeneration {
			return nil, &model.Error{Code: "42P17", Message: "column expression contains unsupported effects or row references"}
		}
	}
	count := uint64(len(binding.Parameters))
	cost := uint64(unsafe.Sizeof(expr.Parameter{})) + uint64(unsafe.Sizeof(int(0)))
	if count > c.options.Evaluation.MaxBytes/cost || count > c.options.InputBytes/uint64(unsafe.Sizeof(expr.ParameterValue{})) {
		return nil, expr.ErrLimit
	}
	remaining := c.options.Evaluation.MaxBytes - count*cost
	if uint64(unsafe.Sizeof(columnValueExpression{})) > remaining {
		return nil, expr.ErrLimit
	}
	remaining -= uint64(unsafe.Sizeof(columnValueExpression{}))
	parameters := make([]expr.Parameter, len(binding.Parameters))
	positions := make([]int, len(binding.Parameters))
	for i, parameter := range binding.Parameters {
		position := -1
		for j, column := range columns {
			if column.ID == parameter.Column {
				position = j
				break
			}
		}
		if position < 0 {
			return nil, &model.Error{Code: "42703", Message: "row expression column no longer exists"}
		}
		layout, used, err := model.CopyTypeLayout(ctx, parameter.Layout, remaining)
		if err != nil {
			return nil, err
		}
		remaining -= used
		parameters[i] = expr.Parameter{Number: uint32(i + 1), Layout: layout}
		positions[i] = position
	}
	if remaining == 0 {
		return nil, expr.ErrLimit
	}
	limits := c.options.Evaluation
	limits.MaxBytes = remaining
	program, err := expr.Compile(ctx, binding.Graph, []ir.ScalarID{binding.Root}, expr.Options{Limits: limits, Parameters: parameters, Builtin: binding.Builtin, Functions: c.options.Functions})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &columnValueExpression{program: program, parameters: parameters, positions: positions, width: len(columns), purpose: purpose, inputBytes: c.options.InputBytes, resultBytes: c.options.ResultBytes, ownedBytes: c.options.Evaluation.MaxBytes - remaining + program.OwnedBytes()}, nil
}

type columnValueExpression struct {
	program     *expr.Program
	parameters  []expr.Parameter
	positions   []int
	width       int
	purpose     columnExpressionPurpose
	inputBytes  uint64
	resultBytes uint64
	ownedBytes  uint64
}

func (p *columnValueExpression) OwnedBytes() uint64 { return p.ownedBytes }

func (p *columnValueExpression) Evaluate(ctx context.Context, row []model.Value, runtime model.ExpressionRuntime) (model.Value, error) {
	return p.evaluate(ctx, row, expr.StateOptions{Sequences: runtime.Sequences, Relations: runtime.Relations, Temporal: runtime.Temporal, Settings: runtime.Settings, TextSearch: runtime.TextSearch, Advisory: runtime.Advisory, Native: runtime.Native})
}

// evaluateWithin admits the evaluation against an invoking call's granted
// budget: working-state charges reserve from budget.Allocator and the granted
// MemoryBytes clamp the program's compiled state limit.
func (p *columnValueExpression) evaluateWithin(ctx context.Context, row []model.Value, budget builtin.CallBudget) (model.Value, error) {
	return p.evaluate(ctx, row, expr.StateOptions{MaxStateBytes: budget.MemoryBytes, WorkAllocator: budget.Allocator, Sequences: budget.Sequences, Relations: budget.Relations, Temporal: budget.Temporal, Settings: budget.Settings, TextSearch: budget.TextSearch, Advisory: budget.Advisory, Native: budget.Native})
}

func (p *columnValueExpression) evaluate(ctx context.Context, row []model.Value, options expr.StateOptions) (result model.Value, err error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	unexpectedInput := p.purpose == columnDefault && row != nil
	wrongWidth := p.purpose != columnDefault && len(row) != p.width
	if unexpectedInput || wrongWidth {
		return model.Value{}, &model.Error{Code: "42804", Message: "column expression row differs from its binding"}
	}
	remaining := p.inputBytes - uint64(len(p.parameters))*uint64(unsafe.Sizeof(expr.ParameterValue{}))
	parameters := make([]expr.ParameterValue, len(p.parameters))
	for i, parameter := range p.parameters {
		value := row[p.positions[i]]
		used, err := model.ValueBytes(ctx, value, remaining)
		if err != nil {
			return model.Value{}, err
		}
		remaining -= used
		parameters[i] = expr.ParameterValue{Number: parameter.Number, Layout: parameter.Layout, Value: value}
	}
	state, err := expr.NewState(p.program, options)
	if err != nil {
		return model.Value{}, err
	}
	defer func() {
		err = errors.Join(err, state.Close(), ctx.Err())
		if err != nil {
			result = model.Value{}
		}
	}()
	input, output := columnar.Batch{Length: 1, MaxRows: 1}, columnar.Batch{MaxRows: 1}
	if err := state.Eval(ctx, &input, parameters, &output, expr.Direct); err != nil {
		return model.Value{}, err
	}
	if output.Rows() != 1 || len(output.Columns) != 1 {
		return model.Value{}, expr.ErrInvalid
	}
	result, _, err = columnar.CopyValue(ctx, &output.Columns[0], output.Lane(0), p.resultBytes)
	return result, err
}
