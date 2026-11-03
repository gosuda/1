package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math"
	"time"
	"unsafe"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// UTCConverter is borrowed; conversion must not acquire a live clock reading.
type UTCConverter interface {
	FromUTC(time.Time) (clock.Instant, error)
}

type SystemTimeOptions struct {
	Bind        bind.Options
	Expressions expr.Options
	Converter   UTCConverter
	// MaxClauses bounds all extension sidecars. MaxBytes bounds preparation
	// metadata, AST inspection and identity encoding independently of stage limits.
	MaxClauses int
	MaxBytes   uint64
}

type SystemTimeSelection struct {
	Timestamp   model.Timestamp
	Present     bool
	Transaction bool
	Identity    [32]byte
}

// ResolveSystemTime borrows parsed and parameters only for this call. No catalog
// or storage access occurs, including while binding scalar expressions.
func ResolveSystemTime(ctx context.Context, parsed *zpgparse.ExtendedParseResult, statementIndex int, parameters []expr.ParameterValue, options SystemTimeOptions) (SystemTimeSelection, error) {
	options.Bind.Builtin = options.Expressions.Builtin
	if err := ctx.Err(); err != nil {
		return SystemTimeSelection{}, err
	}
	if err := systemTimeOptions(options); err != nil {
		return SystemTimeSelection{}, err
	}
	clauses, transaction, err := systemTimeClauses(ctx, parsed, statementIndex, options)
	if err != nil || len(clauses) == 0 {
		return SystemTimeSelection{}, err
	}
	budget := systemTimeBudget{remaining: options.MaxBytes}
	if err := budget.charge(uint64(len(clauses)) * 128); err != nil {
		return SystemTimeSelection{}, err
	}
	if err := budget.charge(64); err != nil {
		return SystemTimeSelection{}, err
	}
	relations := make(map[int32]bool, len(clauses))
	for _, clause := range clauses {
		if clause.RelationLocation >= 0 {
			if _, exists := relations[clause.RelationLocation]; exists {
				return SystemTimeSelection{}, systemTimeError("22023", "duplicate system-time relation sidecar", clause.Location)
			}
			relations[clause.RelationLocation] = false
		}
	}
	walker := systemTimeWalker{ctx: ctx, limits: options.Bind.Limits, budget: &budget, remaining: options.Bind.Limits.MaxNodes, relations: relations}
	statement := parsed.ParseResult.Stmts[statementIndex]
	if err := walker.walk(statement.Stmt.ProtoReflect(), 0, false); err != nil {
		return SystemTimeSelection{}, err
	}
	for _, found := range relations {
		if !found {
			return SystemTimeSelection{}, systemTimeError("22023", "system-time sidecar does not identify a statement relation", -1)
		}
	}
	for _, clause := range clauses {
		if err := walker.walk(clause.Expression.ProtoReflect(), 0, true); err != nil {
			return SystemTimeSelection{}, err
		}
	}
	paramTypes, metadataBytes, err := systemTimeParameterTypes(ctx, parameters, options.Bind.Limits, budget.remaining)
	if err != nil {
		return SystemTimeSelection{}, err
	}
	if err := budget.charge(metadataBytes); err != nil {
		return SystemTimeSelection{}, err
	}
	if err := budget.charge(uint64(len(parameters)) * uint64(unsafe.Sizeof(expr.Parameter{}))); err != nil {
		return SystemTimeSelection{}, err
	}
	for _, layout := range paramTypes {
		if err := systemTimeLayoutCharge(ctx, layout, &budget); err != nil {
			return SystemTimeSelection{}, err
		}
	}
	compilerParameters := make([]expr.Parameter, len(parameters))
	for i, parameter := range parameters {
		if err := budget.charge(uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return SystemTimeSelection{}, err
		}
		used, err := model.ValueBytes(ctx, parameter.Value, budget.remaining)
		if err != nil {
			return SystemTimeSelection{}, err
		}
		if err := budget.charge(used); err != nil {
			return SystemTimeSelection{}, err
		}
		compilerParameters[i] = expr.Parameter{Number: parameter.Number, Layout: paramTypes[parameter.Number-1]}
	}
	identity := sha256.New()
	identity.Write([]byte("supersql/system-time/v1"))
	systemTimeHashNumber(identity, uint64(statementIndex))
	systemTimeHashNumber(identity, uint64(len(clauses)))
	if err := systemTimeHashAST(identity, statement.Stmt, &budget); err != nil {
		return SystemTimeSelection{}, err
	}
	systemTimeHashNumber(identity, uint64(len(paramTypes)))
	for i, layout := range paramTypes {
		systemTimeHashNumber(identity, uint64(i+1))
		systemTimeHashLayout(identity, layout)
	}
	selection := SystemTimeSelection{Present: true, Transaction: transaction}
	for i, clause := range clauses {
		if err := ctx.Err(); err != nil {
			return SystemTimeSelection{}, err
		}
		systemTimeHashNumber(identity, uint64(int64(clause.RelationLocation)))
		systemTimeHashNumber(identity, uint64(int64(clause.Location)))
		if err := systemTimeHashAST(identity, clause.Expression, &budget); err != nil {
			return SystemTimeSelection{}, err
		}
		timestamp, err := evaluateSystemTime(ctx, clause, parameters, paramTypes, compilerParameters, options)
		if err != nil {
			return SystemTimeSelection{}, err
		}
		if i != 0 && timestamp != selection.Timestamp {
			return SystemTimeSelection{}, systemTimeError("0A000", "a statement cannot use different system-time timestamps", clause.Location)
		}
		selection.Timestamp = timestamp
	}
	if err := ctx.Err(); err != nil {
		return SystemTimeSelection{}, err
	}
	identity.Sum(selection.Identity[:0])
	return selection, nil
}

func systemTimeOptions(options SystemTimeOptions) error {
	limits := options.Bind.Limits
	invalidClauses := options.MaxClauses <= 0 || options.MaxBytes == 0
	missingGraphBounds := limits.MaxNodes == 0 || limits.MaxOperands == 0 || limits.MaxBytes == 0
	invalidDepth := limits.MaxDepth == 0 || limits.MaxDepth > 512
	if invalidClauses || missingGraphBounds || invalidDepth {
		return systemTimeError("22023", "system-time preparation requires positive bounded limits", -1)
	}
	if len(options.Expressions.Inputs) != 0 || len(options.Expressions.Captures) != 0 || len(options.Expressions.Parameters) != 0 {
		return systemTimeError("22023", "system-time expression inputs and parameters are supplied by preparation", -1)
	}
	return nil
}

func evaluateSystemTime(ctx context.Context, clause zpgparse.TemporalClause, parameters []expr.ParameterValue, paramTypes []model.TypeLayout, compilerParameters []expr.Parameter, options SystemTimeOptions) (timestamp model.Timestamp, err error) {
	raw := &zpgparse.RawStmt{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{
		Op:         zpgparse.SetOperation_SETOP_NONE,
		TargetList: []*zpgparse.Node{zpgparse.MakeResTargetNodeWithVal(clause.Expression, -1)},
	}}}}
	bound, err := bind.Bind(ctx, raw, nil, options.Bind, paramTypes)
	if err != nil {
		return 0, err
	}
	if bound.Effects().Volatility != ir.Immutable {
		return 0, systemTimeError("0A000", "system-time expressions must be immutable", clause.Location)
	}
	graph := bound.Graph()
	root, ok := graph.Rel(graph.Root())
	if !ok || root.Kind != ir.Project || root.BindingCount() != 1 || root.OutputCount() != 1 || root.InputCount() != 1 {
		return 0, systemTimeError("0A000", "system-time expression must produce one standalone scalar", clause.Location)
	}
	inputID, ok := graph.Input(graph.Root(), 0)
	if !ok {
		return 0, expr.ErrInvalid
	}
	input, ok := graph.Rel(inputID)
	if !ok || input.Kind != ir.Values || input.Rows != 1 || input.InputCount() != 0 || input.OutputCount() != 0 {
		return 0, systemTimeError("0A000", "system-time expression cannot depend on rows", clause.Location)
	}
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		return 0, expr.ErrInvalid
	}
	for index := uint32(0); index < graph.ScalarCount(); index++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		scalar, ok := graph.Scalar(ir.ScalarID(index + 1))
		if !ok || scalar.Kind == ir.Slot || scalar.Kind == ir.Subquery || scalar.Effects.Volatility != ir.Immutable {
			return 0, systemTimeError("0A000", "system-time expression must be immutable and row-independent", clause.Location)
		}
	}
	compileOptions := options.Expressions
	compileOptions.Parameters = compilerParameters
	program, err := expr.Compile(ctx, graph, []ir.ScalarID{binding.Expr}, compileOptions)
	if err != nil {
		return 0, err
	}
	state, err := expr.NewState(program, expr.StateOptions{Temporal: options.Bind.Temporal})
	if err != nil {
		return 0, err
	}
	defer func() {
		err = errors.Join(err, state.Close())
		if err != nil {
			timestamp = 0
		}
	}()
	var output columnar.Batch
	if err := state.Eval(ctx, &columnar.Batch{Length: 1}, parameters, &output, expr.Direct); err != nil {
		return 0, err
	}
	if output.Rows() != 1 || len(output.Columns) != 1 {
		return 0, expr.ErrInvalid
	}
	value := &output.Columns[0]
	if value.IsNull(0) {
		return 0, systemTimeError("22004", "system-time timestamp cannot be NULL", clause.Location)
	}
	switch {
	case value.Type.Storage == model.TypeInt64 && (value.Type.ID == 20 || value.Type.ID == 21 || value.Type.ID == 23):
		timestamp, err := model.NewTimestamp(value.Int64[value.ValueIndex(0)])
		if err != nil {
			return 0, &model.Error{Code: "22023", Message: "system-time timestamp must be a positive SI nanosecond count", Position: int(clause.Location) + 1, Cause: err}
		}
		return timestamp, nil
	case value.Type.Storage == model.TypeTimestamp && value.Type.ID == 1184:
		return systemTimeUTC(value.Int64[value.ValueIndex(0)], options.Converter, clause.Location)
	default:
		return 0, systemTimeError("22023", "system-time expression must have integer or timestamp with time zone type", clause.Location)
	}
}

func systemTimeUTC(microseconds int64, converter UTCConverter, location int32) (model.Timestamp, error) {
	if converter == nil {
		return 0, systemTimeError("22023", "timestamp with time zone requires a UTC-to-SI converter", location)
	}
	// PostgreSQL stores microseconds since 2000-01-01. Splitting first avoids
	// overflowing nanoseconds and preserves negative fractional seconds exactly.
	const postgresEpochSeconds int64 = 946684800
	seconds, remainder := microseconds/1_000_000, microseconds%1_000_000
	if microseconds == math.MinInt64 || microseconds == math.MaxInt64 || seconds > math.MaxInt64-postgresEpochSeconds {
		return 0, systemTimeError("22023", "system-time timestamp is outside the finite range", location)
	}
	seconds += postgresEpochSeconds
	if remainder < 0 {
		seconds--
		remainder += 1_000_000
	}
	instant, err := converter.FromUTC(time.Unix(seconds, remainder*1_000).UTC())
	if err != nil {
		if errors.Is(err, clock.ErrClockRange) {
			return 0, &model.Error{Code: "22023", Message: "system-time timestamp is outside the SI range", Position: int(location) + 1, Cause: err}
		}
		return 0, err
	}
	timestamp, err := model.NewTimestamp(int64(instant))
	if err != nil {
		return 0, &model.Error{Code: "22023", Message: "system-time timestamp must be positive", Position: int(location) + 1, Cause: err}
	}
	return timestamp, nil
}

func systemTimeParameterTypes(ctx context.Context, parameters []expr.ParameterValue, limits ir.Limits, maxBytes uint64) ([]model.TypeLayout, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if uint64(len(parameters)) > uint64(limits.MaxOperands) {
		return nil, 0, systemTimeError("54000", "system-time parameter limit exceeded", -1)
	}
	var maximum uint32
	for _, parameter := range parameters {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if parameter.Number == 0 {
			return nil, 0, fmt.Errorf("%w: invalid parameter number", expr.ErrInvalid)
		}
		if parameter.Number > limits.MaxOperands {
			return nil, 0, systemTimeError("54000", "system-time parameter limit exceeded", -1)
		}
		maximum = max(maximum, parameter.Number)
	}
	size := uint64(unsafe.Sizeof(model.TypeLayout{}))
	if uint64(maximum) > maxBytes/size {
		return nil, 0, systemTimeError("54000", "system-time parameter metadata limit exceeded", -1)
	}
	remaining := maxBytes - uint64(maximum)*size
	types := make([]model.TypeLayout, int(maximum))
	for _, parameter := range parameters {
		if types[parameter.Number-1].Type.ID != 0 {
			return nil, 0, fmt.Errorf("%w: duplicate parameter", expr.ErrInvalid)
		}
		layout, used, err := model.CopyTypeLayout(ctx, parameter.Layout, remaining)
		if err != nil {
			return nil, 0, err
		}
		remaining -= used
		types[parameter.Number-1] = layout
	}
	return types, maxBytes - remaining, nil
}

func systemTimeLayoutCharge(ctx context.Context, layout model.TypeLayout, budget *systemTimeBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := budget.charge(48); err != nil {
		return err
	}
	for _, child := range layout.Children {
		if err := systemTimeLayoutCharge(ctx, child, budget); err != nil {
			return err
		}
	}
	return nil
}

func systemTimeHashLayout(identity hash.Hash, layout model.TypeLayout) {
	systemTimeHashNumber(identity, uint64(layout.Type.ID))
	systemTimeHashNumber(identity, uint64(int64(layout.Type.Modifier)))
	systemTimeHashNumber(identity, uint64(layout.Type.Collation))
	systemTimeHashNumber(identity, uint64(layout.Type.Storage))
	systemTimeHashNumber(identity, uint64(len(layout.Children)))
	for _, child := range layout.Children {
		systemTimeHashLayout(identity, child)
	}
}

func systemTimeHashNumber(identity hash.Hash, value uint64) {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	identity.Write(encoded[:])
}

func systemTimeHashAST(identity hash.Hash, node *zpgparse.Node, budget *systemTimeBudget) error {
	// The bounded reflection pass precedes protobuf's recursive sizing/encoding.
	size := proto.Size(node)
	if err := budget.charge(uint64(size) + 8); err != nil {
		return err
	}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(node)
	if err != nil {
		return err
	}
	systemTimeHashNumber(identity, uint64(len(encoded)))
	identity.Write(encoded)
	return nil
}

func systemTimeError(code, message string, location int32) error {
	return &model.Error{Code: code, Message: message, Position: max(0, int(location)+1)}
}

type systemTimeBudget struct{ remaining uint64 }

func (b *systemTimeBudget) charge(bytes uint64) error {
	if bytes > b.remaining {
		return systemTimeError("54000", "system-time preparation byte limit exceeded", -1)
	}
	b.remaining -= bytes
	return nil
}
