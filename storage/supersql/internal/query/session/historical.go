package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// HistoricalSnapshots transfers an owned reader fixed at the requested timestamp.
// Resolving retained history must not require a live clock read or wait.
type HistoricalSnapshots interface {
	HistoricalSnapshot(context.Context, model.Timestamp) (storage.SnapshotReader, error)
}

type HistoricalOptions struct {
	Snapshots  HistoricalSnapshots
	Catalog    *catalog.Store
	SystemTime SystemTimeOptions
	// Planning borrows both snapshots for this call. Its access and statistics
	// collaborators must use these snapshots, not a current catalog or reader.
	Planning    func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader) (PrepareOptions, error)
	TypeRuntime *DeclaredTypeRuntimeOptions
}

// HistoricalQuery owns one unopened snapshot and its preparation-time parameters.
// Open and Close serialize ownership transfer; metadata remains inspectable afterward.
type HistoricalQuery struct {
	mu         sync.Mutex
	prepared   *Prepared
	selection  SystemTimeSelection
	reader     *historicalReader
	parameters []expr.ParameterValue
	closed     bool
	closeErr   error
}

type historicalReader struct {
	storage.SnapshotReader
	timestamp model.Timestamp
}

func (reader *historicalReader) Timestamp() model.Timestamp {
	if exact, ok := reader.SnapshotReader.(interface{ Timestamp() model.Timestamp }); ok {
		return exact.Timestamp()
	}
	return reader.timestamp
}

func PrepareHistorical(ctx context.Context, parsed *zpgparse.ExtendedParseResult, statementIndex int, parameters []expr.ParameterValue, options HistoricalOptions) (_ *HistoricalQuery, err error) {
	selection, err := ResolveSystemTime(ctx, parsed, statementIndex, parameters, options.SystemTime)
	if err != nil {
		return nil, err
	}
	if !selection.Present {
		return nil, settingError("0A000", "historical preparation requires relation-level AS OF SYSTEM TIME")
	}
	if selection.Transaction {
		return nil, settingError("0A000", "transaction-level AS OF SYSTEM TIME requires transaction coordination")
	}
	statement := parsed.ParseResult.Stmts[statementIndex]
	if err := historicalStatement(ctx, statement, options.SystemTime.Bind.Limits); err != nil {
		return nil, err
	}
	if historicalNil(options.Snapshots) || options.Catalog == nil || options.Planning == nil {
		return nil, settingError("22023", "historical preparation requires snapshots, catalog, and planning factory")
	}
	parameterTypes, parameterBytes, err := systemTimeParameterTypes(ctx, parameters, options.SystemTime.Bind.Limits, options.SystemTime.MaxBytes)
	if err != nil {
		return nil, err
	}
	ownedParameters, err := historicalParameters(ctx, parameters, parameterTypes, parameterBytes, options.SystemTime.MaxBytes)
	if err != nil {
		return nil, err
	}
	opened, err := options.Snapshots.HistoricalSnapshot(ctx, selection.Timestamp)
	if !historicalNil(opened) {
		defer func() {
			if opened != nil {
				err = errors.Join(err, opened.Close())
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	if historicalNil(opened) {
		return nil, settingError("22023", "historical snapshot provider returned no reader")
	}
	reader := &historicalReader{SnapshotReader: opened, timestamp: selection.Timestamp}
	snapshot, err := options.Catalog.Snapshot(reader, selection.Timestamp, catalog.BorrowReader)
	if err != nil {
		return nil, err
	}
	defer func() {
		if snapshot != nil {
			err = errors.Join(err, snapshot.Close())
		}
	}()
	planning, err := options.Planning(ctx, snapshot, reader)
	if err != nil {
		return nil, err
	}
	planning.Normalize.Builtin.InformationSchemaFunctions = planning.Bind.CatalogRelations
	planning.Normalize.Builtin.InformationSchemaTypes = planning.Bind.CatalogRelations
	if options.TypeRuntime != nil {
		runtimeOptions := *options.TypeRuntime
		runtimeOptions.Expressions.Builtin = planning.Normalize.Builtin
		types, resolveErr := NewDeclaredTypeRuntime(ctx, snapshot, runtimeOptions)
		if resolveErr != nil {
			return nil, resolveErr
		}
		planning.Normalize.Builtin.Types = types
	}
	if encoder, ok := planning.Optimize.StatisticsKeyEncoder.(*builtin.KeyEncoder); ok {
		planning.Optimize.StatisticsKeyEncoder = encoder.WithResolveOptions(planning.Normalize.Builtin)
	}
	nilAccess := planning.Optimize.Access != nil && historicalNil(planning.Optimize.Access)
	nilStatistics := planning.Optimize.Statistics != nil && historicalNil(planning.Optimize.Statistics)
	if nilAccess || nilStatistics {
		return nil, settingError("22023", "historical planning factory returned a nil collaborator")
	}
	if err := historicalSearchPath(ctx, planning.Bind); err != nil {
		return nil, err
	}
	validateBound := planning.ValidateBound
	planning.ValidateBound = func(ctx context.Context, bound *bind.BoundStatement) error {
		if err := historicalBound(ctx, bound, selection.Timestamp); err != nil {
			return err
		}
		if validateBound != nil {
			return validateBound(ctx, bound)
		}
		return nil
	}
	prepared, err := Prepare(ctx, statement, snapshot, planning, parameterTypes)
	if err != nil {
		return nil, err
	}
	if prepared == nil || prepared.Timestamp() != selection.Timestamp || reader.Timestamp() != selection.Timestamp {
		return nil, storage.ErrSnapshotNotClosed
	}
	if err := snapshot.Close(); err != nil {
		return nil, err
	}
	snapshot = nil
	prepared.requiredTimestamp = selection.Timestamp
	query := &HistoricalQuery{prepared: prepared, selection: selection, reader: reader, parameters: ownedParameters}
	opened = nil
	return query, nil
}

func (query *HistoricalQuery) Prepared() *Prepared {
	if query == nil {
		return nil
	}
	return query.prepared
}

func (query *HistoricalQuery) Selection() SystemTimeSelection {
	if query == nil {
		return SystemTimeSelection{}
	}
	return query.selection
}

// Open consumes this query's reader and parameters on every outcome. Reader and
// Parameters in environment must be empty: preparation fixed both. Query transfers
// to exec.Open even on rejection; an incorrectly supplied Reader is also closed.
func (query *HistoricalQuery) Open(ctx context.Context, environment exec.Environment) (*exec.Portal, error) {
	var reader *historicalReader
	var parameters []expr.ParameterValue
	var prepared *Prepared
	var rejection error
	if query == nil {
		rejection = exec.ErrInvalid
	} else {
		query.mu.Lock()
		if query.closed {
			rejection = errors.Join(exec.ErrClosed, query.closeErr)
		} else {
			query.closed = true
			reader, parameters, prepared = query.reader, query.parameters, query.prepared
			query.reader, query.parameters = nil, nil
			if reader == nil || prepared == nil {
				rejection = exec.ErrInvalid
			}
		}
		query.mu.Unlock()
	}
	if environment.Reader != nil || len(environment.Parameters) != 0 {
		rejection = errors.Join(rejection, settingError("22023", "historical execution cannot replace its reader or parameters"))
	}
	if !historicalNil(environment.Reader) && !historicalOwnReader(reader, environment.Reader) {
		rejection = errors.Join(rejection, environment.Reader.Close())
	}
	environment.Reader = nil
	if reader != nil {
		environment.Reader = reader
	}
	environment.Parameters = parameters
	if rejection != nil {
		_, err := exec.Open(ctx, nil, environment)
		return nil, errors.Join(rejection, err)
	}
	environment.Builtin.Types = prepared.builtins.Types
	environment.Builtin.InformationSchemaFunctions = prepared.builtins.InformationSchemaFunctions
	environment.Builtin.InformationSchemaTypes = prepared.builtins.InformationSchemaTypes
	if encoder, ok := environment.Keys.(*builtin.KeyEncoder); ok {
		environment.Keys = encoder.WithResolveOptions(environment.Builtin)
	}
	return prepared.Open(ctx, environment)
}

func (query *HistoricalQuery) Close() error {
	if query == nil {
		return nil
	}
	query.mu.Lock()
	defer query.mu.Unlock()
	if query.closed {
		return query.closeErr
	}
	query.closed = true
	reader := query.reader
	query.reader, query.parameters = nil, nil
	if reader != nil {
		query.closeErr = reader.Close()
	}
	return query.closeErr
}

func historicalOwnReader(reader *historicalReader, candidate storage.SnapshotReader) bool {
	if reader == nil {
		return false
	}
	if candidate == reader {
		return true
	}
	original := reflect.ValueOf(reader.SnapshotReader)
	return original.Comparable() && reader.SnapshotReader == candidate
}

func historicalNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func historicalParameters(ctx context.Context, parameters []expr.ParameterValue, layouts []model.TypeLayout, used, maximum uint64) ([]expr.ParameterValue, error) {
	if used > maximum || uint64(len(parameters)) > (maximum-used)/uint64(unsafe.Sizeof(expr.ParameterValue{})) {
		return nil, expr.ErrLimit
	}
	used += uint64(len(parameters)) * uint64(unsafe.Sizeof(expr.ParameterValue{}))
	owned := make([]expr.ParameterValue, len(parameters))
	for i, parameter := range parameters {
		value, bytes, err := model.CopyValue(ctx, parameter.Value, maximum-used)
		if err != nil {
			return nil, err
		}
		used += bytes
		layout := layouts[parameter.Number-1]
		if value.Type != layout.Type.Storage {
			return nil, expr.ErrInvalid
		}
		owned[i] = expr.ParameterValue{Number: parameter.Number, Layout: layout, Value: value}
	}
	return owned, nil
}

func historicalBound(ctx context.Context, bound *bind.BoundStatement, timestamp model.Timestamp) error {
	if bound == nil || bound.Graph() == nil || bound.Timestamp() != timestamp {
		return storage.ErrSnapshotNotClosed
	}
	graph := bound.Graph()
	for i := uint32(0); i < graph.RelCount(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		relation, _ := graph.Rel(ir.RelID(i + 1))
		if relation.Kind == ir.Modify {
			return settingError("25006", "historical queries cannot write data")
		}
		if relation.Effects.Volatility > ir.Stable {
			return settingError("0A000", "historical query contains unproved mutable effects")
		}
	}
	for i := uint32(0); i < graph.ScalarCount(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		scalar, _ := graph.Scalar(ir.ScalarID(i + 1))
		if scalar.Effects.Volatility > ir.Stable {
			return settingError("0A000", "historical query contains unproved mutable effects")
		}
	}
	for i := uint32(0); i < graph.SignatureCount(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		signature, _ := graph.Signature(ir.SignatureID(i + 1))
		if signature.Effects.Volatility > ir.Stable {
			return settingError("0A000", "historical query contains unproved mutable effects")
		}
	}
	for i := uint32(0); i < graph.CTECount(); i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		cte, _ := graph.CTE(ir.CTEID(i + 1))
		if cte.Effects.Volatility > ir.Stable {
			return settingError("0A000", "historical query contains unproved mutable effects")
		}
	}
	return nil
}

func historicalSearchPath(ctx context.Context, options bind.Options) error {
	if uint64(len(options.SearchPath)) > uint64(options.Limits.MaxOperands) {
		return settingError("54000", "historical search path limit exceeded")
	}
	var bytes uint64
	for _, namespace := range options.SearchPath {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uint64(len(namespace)) > options.Limits.MaxBytes-bytes {
			return settingError("54000", "historical search path byte limit exceeded")
		}
		bytes += uint64(len(namespace))
		if historicalTempNamespace(namespace) {
			return settingError("0A000", "historical queries cannot use a temporary catalog")
		}
	}
	return nil
}

func historicalTempNamespace(namespace string) bool {
	return namespace == "pg_temp" || strings.HasPrefix(namespace, "pg_temp_") || strings.HasPrefix(namespace, "pg_toast_temp_")
}

type historicalAST struct {
	limits    ir.Limits
	remaining uint32
	bytes     uint64
}

func historicalStatement(ctx context.Context, statement *zpgparse.RawStmt, limits ir.Limits) error {
	if statement == nil || statement.Stmt == nil {
		return settingError("42601", "expected one historical query statement")
	}
	if limits.MaxNodes == 0 || limits.MaxOperands == 0 || limits.MaxDepth == 0 || limits.MaxDepth > 512 || limits.MaxBytes == 0 {
		return settingError("22023", "invalid historical statement limits")
	}
	inspection := historicalAST{limits: limits, remaining: limits.MaxNodes, bytes: limits.MaxBytes}
	if err := inspection.message(ctx, statement.Stmt.ProtoReflect(), 0); err != nil {
		return err
	}
	if statement.Stmt.GetSelectStmt() == nil {
		return settingError("0A000", "historical preparation supports only query statements")
	}
	return nil
}

func (inspection *historicalAST) message(ctx context.Context, message protoreflect.Message, depth uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !message.IsValid() {
		return settingError("42601", "invalid historical statement AST")
	}
	if depth > inspection.limits.MaxDepth || inspection.remaining == 0 {
		return settingError("54000", "historical statement AST limit exceeded")
	}
	inspection.remaining--
	if len(message.GetUnknown()) != 0 {
		return settingError("0A000", "historical statement contains unknown AST fields")
	}
	switch node := message.Interface().(type) {
	case *zpgparse.InsertStmt, *zpgparse.UpdateStmt, *zpgparse.DeleteStmt, *zpgparse.MergeStmt, *zpgparse.IntoClause, *zpgparse.LockingClause:
		return settingError("25006", "historical queries cannot write data or lock rows")
	case *zpgparse.SelectStmt:
		if node.IntoClause != nil || len(node.LockingClause) != 0 {
			return settingError("25006", "historical queries cannot write data or lock rows")
		}
	case *zpgparse.RangeVar:
		if node.Relpersistence == "t" || historicalTempNamespace(node.Schemaname) {
			return transactionSQLError("0A000", "historical queries cannot use a temporary catalog", node.Location)
		}
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if !message.Has(field) {
			continue
		}
		value := message.Get(field)
		if field.IsMap() {
			return settingError("0A000", "historical statement contains an unsupported AST map")
		}
		if !field.IsList() {
			if err := inspection.value(ctx, field.Kind(), value, depth); err != nil {
				return err
			}
			continue
		}
		list := value.List()
		if uint64(list.Len()) > uint64(inspection.limits.MaxOperands) {
			return settingError("54000", "historical statement operand limit exceeded")
		}
		for j := range list.Len() {
			if err := inspection.value(ctx, field.Kind(), list.Get(j), depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (inspection *historicalAST) value(ctx context.Context, kind protoreflect.Kind, value protoreflect.Value, depth uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var bytes uint64
	switch kind {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return inspection.message(ctx, value.Message(), depth+1)
	case protoreflect.StringKind:
		bytes = uint64(len(value.String()))
	case protoreflect.BytesKind:
		bytes = uint64(len(value.Bytes()))
	}
	if bytes > inspection.bytes {
		return settingError("54000", "historical statement payload limit exceeded")
	}
	inspection.bytes -= bytes
	return nil
}
