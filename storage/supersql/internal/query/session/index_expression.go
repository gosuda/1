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

// Each budget is positive and independent: IR/semantic decoding, compiled
// program/state/output, borrowed input admission, and independently owned result.
type IndexExpressionOptions struct {
	Binding     ir.Limits
	Evaluation  expr.Limits
	InputBytes  uint64
	ResultBytes uint64
}

// ForSnapshot supplies schema.IndexPolicyOptions.Expressions without pinning a
// binding-time catalog in the database-lifetime policy provider.
func (options IndexExpressionOptions) ForSnapshot(ctx context.Context, snapshot *catalog.CatalogSnapshot) (schema.IndexExpressions, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return NewIndexExpressionCompiler(snapshot, options)
}

// The snapshot is borrowed until the compiler is discarded, not by its programs.
// Construct a compiler per immutable catalog snapshot; Evaluate performs no reads.
type IndexExpressionCompiler struct {
	snapshot bind.CatalogSnapshot
	options  IndexExpressionOptions
}

func NewIndexExpressionCompiler(snapshot bind.CatalogSnapshot, options IndexExpressionOptions) (*IndexExpressionCompiler, error) {
	if snapshot == nil || options.InputBytes == 0 || options.ResultBytes == 0 {
		return nil, &model.Error{Code: "22023", Message: "index expression compiler requires a snapshot and positive budgets"}
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
	missingExecutionBudget := limits.MaxStateBytes == 0 || limits.MaxOutputBytes == 0
	if invalidDepth || missingProgramBudget || missingExecutionBudget {
		return nil, expr.ErrInvalid
	}
	options.Evaluation.MaxRows = 1
	return &IndexExpressionCompiler{snapshot: snapshot, options: options}, nil
}

func (c *IndexExpressionCompiler) CheckIndexExpression(ctx context.Context, binding *bind.BoundIndexExpression) error {
	_, _, err := c.compile(ctx, binding)
	return err
}

func (c *IndexExpressionCompiler) BindIndexExpression(ctx context.Context, definition *model.ExpressionDefinition, columns []storage.ColumnLayout, result model.TypeLayout) (*bind.BoundIndexExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	binding, err := bind.BindIndexExpression(ctx, definition, columns, result, c.snapshot, c.options.Binding)
	if err != nil {
		return nil, err
	}
	if err := c.CheckIndexExpression(ctx, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// CompileIndexExpression implements schema.IndexExpressions. The returned value
// owns its program and layouts, and creates and closes a fresh State per call.
func (c *IndexExpressionCompiler) CompileIndexExpression(ctx context.Context, definition *model.ExpressionDefinition, columns []storage.ColumnLayout, result model.TypeLayout) (txn.ValueExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	binding, err := bind.BindIndexExpression(ctx, definition, columns, result, c.snapshot, c.options.Binding)
	if err != nil {
		return nil, err
	}
	return c.compileValueExpression(ctx, binding, columns)
}

// CompilePartitionExpressions returns evaluators in partition-key expression order.
func (c *IndexExpressionCompiler) CompilePartitionExpressions(ctx context.Context, descriptor *model.Descriptor) ([]txn.ValueExpression, error) {
	if c == nil || descriptor == nil {
		return nil, expr.ErrInvalid
	}
	table, ok := descriptor.Table()
	if !ok || table.PartitionStrategy == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	columns := make([]storage.ColumnLayout, len(table.Columns))
	for i, column := range table.Columns {
		columns[i] = storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
	}
	expressions := make([]txn.ValueExpression, 0, len(table.PartitionKeys))
	for _, key := range table.PartitionKeys {
		if key.Column != 0 {
			continue
		}
		if key.Expression == nil {
			return nil, model.ErrInvalidDescriptor
		}
		evaluator, err := c.CompileIndexExpression(ctx, key.Expression, columns, model.TypeLayout{Type: key.Type, Children: key.Children})
		if err != nil {
			return nil, err
		}
		expressions = append(expressions, evaluator)
	}
	return expressions, nil
}

// CompileStatisticsExpression returns the frozen result layout together with an
// evaluator that performs no catalog reads.
func (c *IndexExpressionCompiler) CompileStatisticsExpression(ctx context.Context, definition *model.ExpressionDefinition, columns []storage.ColumnLayout) (txn.ValueExpression, model.TypeLayout, error) {
	if c == nil {
		return nil, model.TypeLayout{}, expr.ErrInvalid
	}
	binding, result, err := bind.BindStatisticsExpression(ctx, definition, columns, c.snapshot, c.options.Binding)
	if err != nil {
		return nil, model.TypeLayout{}, err
	}
	expression, err := c.compileValueExpression(ctx, binding, columns)
	return expression, result, err
}

func (c *IndexExpressionCompiler) compileValueExpression(ctx context.Context, binding *bind.BoundIndexExpression, columns []storage.ColumnLayout) (txn.ValueExpression, error) {
	program, parameters, err := c.compile(ctx, binding)
	if err != nil {
		return nil, err
	}
	positions := make([]int, len(parameters))
	for i, parameter := range binding.Parameters {
		positions[i] = -1
		for j, column := range columns {
			if column.ID == parameter.Column {
				positions[i] = j
				break
			}
		}
		if positions[i] < 0 {
			return nil, &model.Error{Code: "42703", Message: "expression column no longer exists"}
		}
	}
	return &indexValueExpression{program: program, parameters: parameters, positions: positions, width: len(columns), inputBytes: c.options.InputBytes, resultBytes: c.options.ResultBytes}, nil
}

func (c *IndexExpressionCompiler) compile(ctx context.Context, binding *bind.BoundIndexExpression) (*expr.Program, []expr.Parameter, error) {
	if c == nil || binding == nil || binding.Graph == nil {
		return nil, nil, expr.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	root, ok := binding.Graph.Scalar(binding.Root)
	if !ok || root.Effects.Volatility != ir.Immutable {
		return nil, nil, &model.Error{Code: "42P17", Message: "index expressions must be IMMUTABLE"}
	}
	for id := ir.ScalarID(1); uint32(id) <= binding.Graph.ScalarCount(); id++ {
		node, _ := binding.Graph.Scalar(id)
		if node.Kind == ir.Subquery || node.Kind == ir.Slot || node.Effects.Volatility != ir.Immutable {
			return nil, nil, &model.Error{Code: "42P17", Message: "index expression contains effects or unbound row references"}
		}
		if node.Function != 0 {
			if _, ok := builtin.LookupSignature(node.Function); !ok {
				return nil, nil, &bind.MissingBindingError{Surface: "immutable SQL routine index expression execution"}
			}
		}
	}
	count := uint64(len(binding.Parameters))
	cost := uint64(unsafe.Sizeof(expr.Parameter{})) + uint64(unsafe.Sizeof(int(0)))
	if count > c.options.Evaluation.MaxBytes/cost || count > c.options.InputBytes/uint64(unsafe.Sizeof(expr.ParameterValue{})) {
		return nil, nil, expr.ErrLimit
	}
	remaining := c.options.Evaluation.MaxBytes - count*cost
	parameters := make([]expr.Parameter, len(binding.Parameters))
	for i, parameter := range binding.Parameters {
		layout, bytes, err := model.CopyTypeLayout(ctx, parameter.Layout, remaining)
		if err != nil {
			return nil, nil, err
		}
		remaining -= bytes
		parameters[i] = expr.Parameter{Number: uint32(i + 1), Layout: layout}
	}
	limits := c.options.Evaluation
	if remaining == 0 {
		return nil, nil, expr.ErrLimit
	}
	limits.MaxBytes = remaining
	// Binding supplies the persisted context and fixed immutable output policy.
	program, err := expr.Compile(ctx, binding.Graph, []ir.ScalarID{binding.Root}, expr.Options{Limits: limits, Parameters: parameters, Builtin: binding.Builtin})
	if err != nil {
		return nil, nil, err
	}
	return program, parameters, nil
}

type indexValueExpression struct {
	program     *expr.Program
	parameters  []expr.Parameter
	positions   []int
	width       int
	inputBytes  uint64
	resultBytes uint64
}

func (p *indexValueExpression) Evaluate(ctx context.Context, row []model.Value, runtime model.ExpressionRuntime) (result model.Value, err error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if len(row) != p.width {
		return model.Value{}, &model.Error{Code: "42804", Message: "index expression row width differs from its layout"}
	}
	remaining := p.inputBytes - uint64(len(p.parameters))*uint64(unsafe.Sizeof(expr.ParameterValue{}))
	parameters := make([]expr.ParameterValue, len(p.parameters))
	for i, parameter := range p.parameters {
		value := row[p.positions[i]]
		bytes, err := model.ValueBytes(ctx, value, remaining)
		if err != nil {
			return model.Value{}, err
		}
		remaining -= bytes
		parameters[i] = expr.ParameterValue{Number: parameter.Number, Layout: parameter.Layout, Value: value}
	}
	state, err := expr.NewState(p.program, expr.StateOptions{Sequences: runtime.Sequences, Relations: runtime.Relations, Temporal: runtime.Temporal, Settings: runtime.Settings, TextSearch: runtime.TextSearch, Advisory: runtime.Advisory, Native: runtime.Native})
	if err != nil {
		return model.Value{}, err
	}
	defer func() { err = errors.Join(err, state.Close()) }()
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
