package builtin

import (
	"context"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type JSONTableColumnKind uint8

const (
	JSONTableOrdinality JSONTableColumnKind = iota + 1
	JSONTableExists
	JSONTableValue
	JSONTableQuery
	JSONTableNested
)

type JSONTableColumnSpec struct {
	Kind           JSONTableColumnKind
	Parent         int
	Output         int
	Path           []byte
	OutputFormat   SQLJSONInputFormat
	OutputEncoding SQLJSONEncoding
	Wrapper        SQLJSONWrapper
	Quotes         SQLJSONQuotes
	CaptureEmpty   bool
	CaptureError   bool
}

type JSONTableSpec struct {
	Root         []byte
	InputFormat  SQLJSONInputFormat
	InputEncoding SQLJSONEncoding
	Passing      int
	EmptyOnError bool
	UseTZ       bool
	Columns      []JSONTableColumnSpec
}

type jsonTableColumnPlan struct {
	spec JSONTableColumnSpec
	path *JSONPathPlan
	value *SQLJSONPlan
}

type JSONTablePlan struct {
	spec       JSONTableSpec
	arguments  []model.TypeLayout
	result     model.TypeLayout
	root       *JSONPathPlan
	columns    []jsonTableColumnPlan
	children   [][]int
	subtreeEnd []int
	options    ResolveOptions
	ownedBytes uint64
}

func (plan *JSONTablePlan) OwnedBytes() uint64 {
	if plan == nil {
		return 0
	}
	return plan.ownedBytes
}

func ResolveJSONTable(ctx context.Context, spec JSONTableSpec, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (*JSONTablePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.InputFormat < SQLJSONDefaultInput || spec.InputFormat > SQLJSONBinaryInput || spec.InputEncoding < SQLJSONDefaultEncoding || spec.InputEncoding > SQLJSONUTF32 || spec.Passing < 0 || len(arguments) != 1+4*spec.Passing || len(spec.Columns) == 0 || result.Type.Storage != model.TypeComposite {
		return nil, sqlError("XX000", "invalid JSON_TABLE plan metadata")
	}
	used := uint64(unsafe.Sizeof(JSONTablePlan{})) + uint64(len(arguments))*uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(len(spec.Columns))*uint64(unsafe.Sizeof(jsonTableColumnPlan{}))
	used += uint64(len(spec.Columns)+1)*uint64(unsafe.Sizeof([]int{})) + uint64(len(spec.Columns))*3*uint64(unsafe.Sizeof(int(0)))
	if used > maximum {
		return nil, sqlError("54000", "JSON_TABLE metadata budget exceeded")
	}
	resultBytes, err := model.TypeLayoutBytes(ctx, result, maximum-used)
	if err != nil {
		return nil, codecLayoutError(err)
	}
	used += resultBytes
	plan := &JSONTablePlan{spec: JSONTableSpec{InputFormat: spec.InputFormat, InputEncoding: spec.InputEncoding, Passing: spec.Passing, EmptyOnError: spec.EmptyOnError, UseTZ: spec.UseTZ}, result: cloneSQLJSONLayout(result), options: options}
	plan.arguments = make([]model.TypeLayout, len(arguments))
	for i, layout := range arguments {
		bytes, err := model.TypeLayoutBytes(ctx, layout, maximum-used)
		if err != nil {
			return nil, codecLayoutError(err)
		}
		used += bytes
		plan.arguments[i] = cloneSQLJSONLayout(layout)
	}
	plan.root, err = DecodeJSONPathBinary(ctx, spec.Root, maximum-used)
	if err != nil {
		return nil, err
	}
	used += plan.root.OwnedBytes()
	plan.columns = make([]jsonTableColumnPlan, len(spec.Columns))
	plan.children = make([][]int, len(spec.Columns)+1)
	plan.subtreeEnd = make([]int, len(spec.Columns))
	pathArguments := make([]model.TypeLayout, 2+4*spec.Passing)
	pathArguments[0] = arguments[0]
	pathArguments[1] = model.TypeLayout{Type: model.Type{ID: 4072, Modifier: -1, Storage: model.TypeBytes}}
	copy(pathArguments[2:], arguments[1:])
	for i, column := range spec.Columns {
		if column.Kind < JSONTableOrdinality || column.Kind > JSONTableNested || column.Parent < 0 || column.Parent > i || column.Parent > 0 && spec.Columns[column.Parent-1].Kind != JSONTableNested {
			return nil, sqlError("XX000", "invalid JSON_TABLE column tree")
		}
		pathBytes := column.Path
		column.Path = nil
		plan.columns[i].spec = column
		plan.children[column.Parent] = append(plan.children[column.Parent], i)
		if column.Kind != JSONTableOrdinality {
			plan.columns[i].path, err = DecodeJSONPathBinary(ctx, pathBytes, maximum-used)
			if err != nil {
				return nil, err
			}
			used += plan.columns[i].path.OwnedBytes()
		}
		if column.Kind == JSONTableNested || column.Kind == JSONTableOrdinality {
			continue
		}
		if column.Output < 1 || column.Output > len(result.Children) {
			return nil, sqlError("XX000", "invalid JSON_TABLE output mapping")
		}
		kind := SQLJSONValueKind
		if column.Kind == JSONTableExists {
			kind = SQLJSONExistsKind
		} else if column.Kind == JSONTableQuery {
			kind = SQLJSONQueryKind
		}
		valueSpec := SQLJSONPlanSpec{
			Kind: kind, InputFormat: SQLJSONDefaultInput, InputEncoding: SQLJSONDefaultEncoding,
			OutputFormat: column.OutputFormat, OutputEncoding: column.OutputEncoding,
			Wrapper: column.Wrapper, Quotes: column.Quotes, ItemType: SQLJSONAnyItem,
			Passing: spec.Passing, CaptureEmpty: column.CaptureEmpty, CaptureError: column.CaptureError,
		}
		plan.columns[i].value, err = ResolveSQLJSON(ctx, valueSpec, pathArguments, result.Children[column.Output-1], options, maximum-used)
		if err != nil {
			return nil, err
		}
		used += plan.columns[i].value.OwnedBytes()
	}
	for i, column := range spec.Columns {
		end := i + 1
		if column.Kind == JSONTableNested {
			for end < len(spec.Columns) {
				descendant := false
				for parent := spec.Columns[end].Parent; parent != 0; parent = spec.Columns[parent-1].Parent {
					if parent == i+1 {
						descendant = true
						break
					}
				}
				if !descendant {
					break
				}
				end++
			}
		}
		plan.subtreeEnd[i] = end
	}
	plan.ownedBytes = used
	if used > maximum {
		return nil, sqlError("54000", "JSON_TABLE metadata budget exceeded")
	}
	return plan, nil
}

func (plan *JSONTablePlan) Open(ctx context.Context, values []model.Value, options RowSourceOptions) (RowSourceIterator, error) {
	if plan == nil || len(values) != len(plan.arguments) || options.MaxRowBytes == 0 || options.MaxMemoryBytes == 0 || options.MaxRowBytes > math.MaxInt64 {
		return nil, sqlError("XX000", "invalid JSON_TABLE invocation")
	}
	for i, value := range values {
		if value.Type != plan.arguments[i].Type.Storage {
			return nil, sqlError("42804", "JSON_TABLE argument storage differs from its plan")
		}
	}
	if values[0].IsNull {
		return &jsonTableIterator{done: true}, nil
	}
	budget := CallBudget{OutputBytes: int64(options.MaxRowBytes), MemoryBytes: options.MaxMemoryBytes, Temporal: options.Temporal}
	if allocator, ok := options.Allocator.(InvocationAllocator); ok {
		budget.Allocator = allocator
	}
	document, err := decodeSQLJSONDocument(values[0], plan.arguments[0], plan.spec.InputFormat, plan.spec.InputEncoding)
	if err != nil {
		if plan.spec.EmptyOnError && suppressibleSQLJSONError(err) {
			return &jsonTableIterator{done: true}, nil
		}
		return nil, err
	}
	if uint64(len(document)) > options.MaxInputBytes {
		return nil, sqlError("54000", "JSON_TABLE input exceeds budget")
	}
	passing := make([]JSONPathVariable, plan.spec.Passing)
	for i := range passing {
		base := 1 + 4*i
		if values[base].IsNull || values[base].Type != model.TypeString || values[base+1].IsNull || values[base+2].IsNull {
			return nil, sqlError("XX000", "invalid JSON_TABLE PASSING metadata")
		}
		format, encoding := SQLJSONInputFormat(values[base+1].IntVal), SQLJSONEncoding(values[base+2].IntVal)
		value, layout := values[base+3], plan.arguments[base+3]
		if format != SQLJSONDefaultInput {
			text, decodeErr := decodeSQLJSONDocument(value, layout, format, encoding)
			if decodeErr != nil {
				if plan.spec.EmptyOnError && suppressibleSQLJSONError(decodeErr) {
					return &jsonTableIterator{done: true}, nil
				}
				return nil, decodeErr
			}
			value = model.Value{Type: model.TypeJSONB, StrVal: text, IsNull: value.IsNull}
			layout = model.TypeLayout{Type: model.Type{ID: 114, Modifier: -1, Storage: model.TypeJSONB}}
		}
		passing[i] = JSONPathVariable{Name: values[base].StrVal, Layout: layout, Value: value}
	}
	execution, resources, err := prepareJSONPathExecution(ctx, document, plan.root, "{}", passing, plan.spec.UseTZ, budget, plan.options)
	if err != nil {
		if plan.spec.EmptyOnError && suppressibleSQLJSONError(err) {
			return &jsonTableIterator{done: true}, nil
		}
		return nil, err
	}
	life, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	iterator := &jsonTableIterator{
		plan: plan, execution: execution, resources: resources, budget: budget,
		life: life, cancel: cancel, requests: make(chan context.Context), responses: make(chan jsonTableResponse),
		row: make([]model.Value, len(plan.result.Children)),
	}
	iterator.wait.Add(1)
	go iterator.run()
	return iterator, nil
}

type jsonTableResponse struct {
	row  []model.Value
	done bool
	err  error
}

type jsonTableIterator struct {
	plan      *JSONTablePlan
	execution *jsonPathExecution
	resources *jsonPathExecutionResources
	budget    CallBudget
	life      context.Context
	cancel    context.CancelCauseFunc
	requests  chan context.Context
	responses chan jsonTableResponse
	row       []model.Value
	wait      sync.WaitGroup
	done      bool
	closed    bool
}

func (iterator *jsonTableIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator == nil || iterator.closed {
		return nil, false, sqlError("XX000", "JSON_TABLE iterator is closed")
	}
	if iterator.done {
		return nil, false, nil
	}
	select {
	case iterator.requests <- ctx:
	case <-ctx.Done():
		iterator.cancel(ctx.Err())
		return nil, false, ctx.Err()
	case <-iterator.life.Done():
		return nil, false, context.Cause(iterator.life)
	}
	select {
	case response := <-iterator.responses:
		if response.done {
			iterator.done = true
			return nil, false, response.err
		}
		return response.row, true, nil
	case <-ctx.Done():
		iterator.cancel(ctx.Err())
		return nil, false, ctx.Err()
	case <-iterator.life.Done():
		return nil, false, context.Cause(iterator.life)
	}
}
func (iterator *jsonTableIterator) Close() error {
	if iterator == nil || iterator.closed {
		return nil
	}
	iterator.closed = true
	if iterator.cancel != nil {
		iterator.cancel(nil)
		iterator.wait.Wait()
	}
	if iterator.resources != nil {
		iterator.resources.release()
		iterator.resources = nil
	}
	clear(iterator.row)
	return nil
}

func (iterator *jsonTableIterator) run() {
	defer iterator.wait.Done()
	request, ok := iterator.request()
	if !ok {
		return
	}
	iterator.execution.ctx = request
	ordinal := int64(0)
	err := iterator.execution.evaluate(iterator.plan.root.expression, jsonPathItem{}, -1, func(item jsonPathItem) error {
		ordinal++
		return iterator.level(0, item, ordinal)
	})
	if iterator.plan.spec.EmptyOnError && suppressibleSQLJSONError(err) {
		err = nil
	}
	select {
	case iterator.responses <- jsonTableResponse{done: true, err: err}:
	case <-iterator.life.Done():
	}
}

func (iterator *jsonTableIterator) request() (context.Context, bool) {
	select {
	case ctx := <-iterator.requests:
		return ctx, true
	case <-iterator.life.Done():
		return nil, false
	}
}

func (iterator *jsonTableIterator) emit() error {
	select {
	case iterator.responses <- jsonTableResponse{row: iterator.row}:
	case <-iterator.life.Done():
		return context.Cause(iterator.life)
	}
	next, ok := iterator.request()
	if !ok {
		return context.Cause(iterator.life)
	}
	iterator.execution.ctx = next
	return next.Err()
}

func (iterator *jsonTableIterator) level(parent int, item jsonPathItem, ordinal int64) error {
	nested := iterator.plan.children[parent]
	hasNested := false
	for _, index := range nested {
		column := &iterator.plan.columns[index]
		switch column.spec.Kind {
		case JSONTableOrdinality:
			iterator.row[column.spec.Output-1] = model.Int64Val(ordinal)
		case JSONTableNested:
			hasNested = true
		default:
			value, err := column.value.pathFrom(iterator.execution, column.path, item, iterator.budget)
			if err != nil {
				return err
			}
			iterator.row[column.spec.Output-1] = value
		}
	}
	if !hasNested {
		return iterator.emit()
	}
	for _, index := range nested {
		column := &iterator.plan.columns[index]
		if column.spec.Kind != JSONTableNested {
			continue
		}
		for _, sibling := range nested {
			if iterator.plan.columns[sibling].spec.Kind == JSONTableNested {
				iterator.clearNested(sibling)
			}
		}
		count := int64(0)
		err := iterator.evaluate(column.path, item, func(child jsonPathItem) error {
			count++
			return iterator.level(index+1, child, count)
		})
		if err != nil {
			if iterator.plan.spec.EmptyOnError && suppressibleSQLJSONError(err) {
				count = 0
			} else {
				return err
			}
		}
		if count == 0 {
			if err := iterator.emit(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (iterator *jsonTableIterator) evaluate(path *JSONPathPlan, root jsonPathItem, emit func(jsonPathItem) error) error {
	previousRoot, previousPlan := iterator.execution.root, iterator.execution.plan
	iterator.execution.root, iterator.execution.plan = root, path
	defer func() {
		iterator.execution.root, iterator.execution.plan = previousRoot, previousPlan
	}()
	return iterator.execution.evaluate(path.expression, jsonPathItem{}, -1, emit)
}

func (iterator *jsonTableIterator) clearNested(index int) {
	for i := index + 1; i < iterator.plan.subtreeEnd[index]; i++ {
		output := iterator.plan.columns[i].spec.Output
		if output != 0 {
			iterator.row[output-1] = model.NullVal(iterator.plan.result.Children[output-1].Type.Storage)
		}
	}
}

var _ RowSourcePlan = (*JSONTablePlan)(nil)
var _ RowSourceIterator = (*jsonTableIterator)(nil)
