// Package expr compiles scalar IR into immutable masked batch programs.
package expr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var (
	ErrInvalid      = errors.New("invalid expression program")
	ErrLimit        = errors.New("expression resource limit exceeded")
	ErrClosed       = errors.New("expression state closed")
	ErrSubquery     = errors.New("subquery requires physical lowering to an input slot")
	ErrSetReturning = errors.New("set-returning call requires ProjectSet")
)

// Limits are hard bounds; every field must be positive. MaxBytes bounds compiled
// storage, MaxStateBytes bounds registers/masks and owned intermediate payloads,
// and MaxOutputBytes independently bounds the published columnar builder.
type Limits struct {
	MaxInstructions  int
	MaxRegisters     int
	MaxDepth         int
	MaxRows          int
	MaxBytes         uint64
	MaxConstantBytes uint64
	MaxStateBytes    uint64
	MaxOutputBytes   uint64
}

func DefaultLimits() Limits {
	return Limits{MaxInstructions: 65536, MaxRegisters: 16384, MaxDepth: 64, MaxRows: 1024, MaxBytes: 16 << 20, MaxConstantBytes: 4 << 20, MaxStateBytes: 64 << 20, MaxOutputBytes: 16 << 20}
}

type Input struct {
	Slot   model.SlotID
	Column int
}
type Parameter struct {
	Number uint32
	Layout model.TypeLayout
}

// ScalarCall borrows arguments and must honor cancellation and both budget limits.
// Allocator suballocates the pre-admitted callback claim; it must not escape the call.
type ScalarCall func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error)

type Function struct {
	ID            model.FunctionID
	Arguments     []model.TypeLayout
	Result        model.TypeLayout
	Strict        bool
	VariadicArray bool
	// Zero represents no bound input collation or an indeterminate one.
	InputCollation model.CollationID
}

// Resolver is consulted only for identities absent from the builtin inventory.
// Its returned capability must be safe for concurrent invocation by states.
type Resolver interface {
	Resolve(Function) (ScalarCall, error)
}

// RowSourceFunction is the immutable typed identity supplied to non-builtin
// set-returning providers. Arguments and Result are owned by the compiled plan;
// Context and MaximumBytes bound any metadata the resolver retains.
type RowSourceFunction struct {
	ID             model.FunctionID
	Arguments      []model.TypeLayout
	Result         model.TypeLayout
	Strict         bool
	VariadicArray  bool
	InputCollation model.CollationID
	Context        context.Context
	MaximumBytes   uint64
}

// RowSourceResolver is an optional extension implemented by function resolvers
// that own streaming user-defined set-returning routines.
type RowSourceResolver interface {
	ResolveRows(RowSourceFunction) (builtin.RowSourcePlan, error)
}

type Options struct {
	Limits Limits
	Inputs []Input
	// Captures maps outer slots to columns of the State's one-logical-row batch.
	Captures   []Input
	Parameters []Parameter
	Subqueries []Subquery
	Builtin    builtin.ResolveOptions
	Functions  Resolver
}

type opcode uint8

const (
	opInit opcode = iota + 1
	opConst
	opInput
	opCapture
	opParam
	opCall
	opSQLJSON
	opCopy
	opNull
	opNot
	opMergeAnd
	opMergeOr
	opEnter
	opFilter
	opLeave
	opRow
	opArray
	opField
	opOnceEnter
	opOnceStore
	opOnceRead
	opCollate
	opRelabel
	opMinMax
	opArrayStack
	opArrayCoerce
	opArrayIndex
	opArraySlice
	opRowCompare
	opSubquery
	opArrayQuantified
	opSequence
	opRelation
	opFieldStore
	opArrayStore
	opJSONBStore
)

type maskKind uint8

const (
	maskAll maskKind = iota
	maskTrue
	maskNotTrue
	maskNull
	maskAnd
	maskOr
	maskNotNull
	maskZero
)

type instruction struct {
	op              opcode
	dst             int
	args            []int
	index           int
	mask            maskKind
	negated         bool
	identity        model.FunctionID
	operator        model.OperatorID
	ownsResult      bool
	call            ScalarCall
	vector          builtin.VectorKernel
	coercion        *arrayCoercion
	subscripts      ir.ArraySubscripts
	rowComparison   ir.RowComparisonKind
	subqueryKind    ir.SubqueryKind
	strict          bool
	sliceAssignment bool
}

type register struct {
	layout   model.TypeLayout
	nullable bool
	once     bool
}

type capture struct {
	Input
	layout   model.TypeLayout
	nullable bool
	used     bool
}

// Program owns all metadata and constants and is safe to share across states.
// Its zero value is invalid. No caller can mutate instructions or layouts.
type Program struct {
	code                 []instruction
	direct               []batchStep
	registers            []register
	constants            []model.Value
	outputs              []int
	outputLayouts        []model.TypeLayout
	inputs               []Input
	captures             []capture
	parameters           []Parameter
	subqueries           []compiledSubquery
	sequences            bool
	relations            bool
	limits               Limits
	bytes                uint64
	constantBytes        uint64
	templateInstructions int
	verified             bool
}

// OwnedBytes reports the conservative metadata/constant charge, including
// capacity-growth allowances. It is not a live-heap measurement.
func (p *Program) OwnedBytes() uint64 { return p.bytes }

func validLimits(l Limits) bool {
	for _, count := range [...]int{l.MaxInstructions, l.MaxRegisters, l.MaxRows} {
		if count <= 0 || count > 1<<31-1 {
			return false
		}
	}
	if l.MaxDepth <= 0 || l.MaxDepth > 64 {
		return false
	}
	for _, bytes := range [...]uint64{l.MaxBytes, l.MaxConstantBytes, l.MaxStateBytes, l.MaxOutputBytes} {
		if bytes == 0 {
			return false
		}
	}
	return true
}

func needsMask(n ir.Scalar) bool {
	switch n.Kind {
	case ir.Case, ir.Coalesce:
		return true
	case ir.Bool:
		return n.BoolOp != ir.Not
	default:
		return false
	}
}

func Compile(ctx context.Context, graph *ir.Graph, roots []ir.ScalarID, options Options) (*Program, error) {
	l := options.Limits
	if graph == nil || graph.Root() == 0 || len(roots) == 0 {
		return nil, ErrInvalid
	}
	if !validLimits(l) {
		return nil, ErrInvalid
	}
	p := &Program{limits: l}
	c := compiler{p: p, graph: graph, options: options}
	if err := c.bindInputs(ctx); err != nil {
		return nil, err
	}
	if err := c.validateSubqueries(ctx); err != nil {
		return nil, err
	}
	for _, root := range roots {
		r, err := c.scalar(ctx, root, 0)
		if err != nil {
			return nil, err
		}
		if err := p.charge(24 + 3*uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
			return nil, err
		}
		p.outputs = append(p.outputs, r)
		p.outputLayouts = append(p.outputLayouts, p.registers[r].layout)
	}
	if err := p.Verify(); err != nil {
		return nil, err
	}
	if err := p.charge(uint64(len(p.code)) * 8); err != nil {
		return nil, err
	}
	p.direct = make([]batchStep, len(p.code))
	for i, in := range p.code {
		p.direct[i] = decode(in.op)
	}
	p.verified = true
	return p, nil
}

func (p *Program) charge(n uint64) error {
	if n > p.limits.MaxBytes-p.bytes {
		return ErrLimit
	}
	p.bytes += n
	return nil
}

func (p *Program) emit(in instruction) error {
	if len(p.code) >= p.limits.MaxInstructions-p.templateInstructions {
		return ErrLimit
	}
	// Charge geometric growth and the old backing retained during reallocation.
	if err := p.charge(3*uint64(unsafe.Sizeof(in)) + 3*uint64(len(in.args))*8); err != nil {
		return err
	}
	p.code = append(p.code, in)
	return nil
}

func (p *Program) OutputCount() int { return len(p.outputs) }

// OutputType returns an owned scalar identity without cloning a nested layout.
func (p *Program) OutputType(index int) (model.Type, bool) {
	if index < 0 || index >= len(p.outputs) {
		return model.Type{}, false
	}
	return p.registers[p.outputs[index]].layout.Type, true
}

// OutputLayoutMatches checks the complete layout without copying or exposing it.
func (p *Program) OutputLayoutMatches(index int, layout model.TypeLayout) bool {
	if p == nil || index < 0 || index >= len(p.outputs) {
		return false
	}
	return model.TypeLayoutsEqual(p.registers[p.outputs[index]].layout, layout)
}

func (p *Program) OutputLayouts() []model.TypeLayout {
	result := make([]model.TypeLayout, len(p.outputs))
	for i, r := range p.outputs {
		result[i] = cloneLayout(p.registers[r].layout)
	}
	return result
}
func cloneLayout(l model.TypeLayout) model.TypeLayout {
	out := model.TypeLayout{Type: l.Type, Children: make([]model.TypeLayout, len(l.Children))}
	if len(l.FieldNames) != 0 {
		out.FieldNames = make([]string, len(l.FieldNames))
		for i, name := range l.FieldNames {
			out.FieldNames[i] = strings.Clone(name)
		}
	}
	for i, child := range l.Children {
		out.Children[i] = cloneLayout(child)
	}
	return out
}
func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }

type compiler struct {
	p       *Program
	graph   *ir.Graph
	options Options
	once    map[ir.ScalarID]int
}

func (c *compiler) layout(ctx context.Context, id ir.LayoutID, depth int) (model.TypeLayout, error) {
	if err := ctx.Err(); err != nil {
		return model.TypeLayout{}, err
	}
	if depth >= c.p.limits.MaxDepth {
		return model.TypeLayout{}, ErrLimit
	}
	typ, ok := c.graph.Type(id)
	if !ok {
		return model.TypeLayout{}, ErrInvalid
	}
	count := c.graph.LayoutChildCount(id)
	if err := c.p.charge(uint64(unsafe.Sizeof(model.TypeLayout{})) * (1 + uint64(count))); err != nil {
		return model.TypeLayout{}, err
	}
	l := model.TypeLayout{Type: typ, Children: make([]model.TypeLayout, count)}
	if names := c.graph.LayoutFieldNameCount(id); names != 0 {
		if typ.Storage != model.TypeComposite || names != count {
			return model.TypeLayout{}, ErrInvalid
		}
		if err := c.p.charge(uint64(names) * uint64(unsafe.Sizeof(""))); err != nil {
			return model.TypeLayout{}, err
		}
		l.FieldNames = make([]string, names)
		for i := range names {
			if err := ctx.Err(); err != nil {
				return model.TypeLayout{}, err
			}
			name, ok := c.graph.LayoutFieldName(id, i)
			if !ok {
				return model.TypeLayout{}, ErrInvalid
			}
			if err := c.p.charge(uint64(len(name))); err != nil {
				return model.TypeLayout{}, err
			}
			l.FieldNames[i] = strings.Clone(name)
		}
	}
	for i := range count {
		child, _ := c.graph.LayoutChild(id, i)
		var err error
		l.Children[i], err = c.layout(ctx, child, depth+1)
		if err != nil {
			return model.TypeLayout{}, err
		}
	}
	return l, l.Validate()
}

func (c *compiler) bindInputs(ctx context.Context) error {
	for _, in := range c.options.Inputs {
		if in.Column < 0 {
			return ErrInvalid
		}
		if _, ok := c.graph.Slot(in.Slot); !ok {
			return ErrInvalid
		}
		for _, previous := range c.p.inputs {
			if previous.Slot == in.Slot || previous.Column == in.Column {
				return invalid("duplicate input mapping")
			}
		}
		if err := c.p.charge(3 * uint64(unsafe.Sizeof(in))); err != nil {
			return err
		}
		c.p.inputs = append(c.p.inputs, in)
	}
	for _, in := range c.options.Captures {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot, ok := c.graph.Slot(in.Slot)
		if !ok || in.Column < 0 {
			return ErrInvalid
		}
		for _, previous := range c.p.inputs {
			if previous.Slot == in.Slot {
				return invalid("slot mapped as both input and capture")
			}
		}
		for _, previous := range c.p.captures {
			if previous.Slot == in.Slot || previous.Column == in.Column {
				return invalid("duplicate capture mapping")
			}
		}
		layout, err := c.layout(ctx, slot.Layout, 0)
		if err != nil {
			return err
		}
		if err := c.p.charge(3 * uint64(unsafe.Sizeof(capture{}))); err != nil {
			return err
		}
		c.p.captures = append(c.p.captures, capture{Input: in, layout: layout, nullable: slot.Nullable})
	}
	for _, param := range c.options.Parameters {
		if err := ctx.Err(); err != nil {
			return err
		}
		if param.Number == 0 {
			return ErrInvalid
		}
		// Parameters are declared signature positions; a routine datum may
		// carry a pseudo-type marker the call site resolves per invocation.
		if err := param.Layout.ValidateDeclaredLayout(); err != nil {
			return err
		}
		for _, previous := range c.p.parameters {
			if previous.Number == param.Number {
				return invalid("duplicate parameter")
			}
		}
		if err := c.p.charge(3 * uint64(unsafe.Sizeof(param))); err != nil {
			return err
		}
		layout, err := c.copyLayout(ctx, param.Layout, 0)
		if err != nil {
			return err
		}
		param.Layout = layout
		c.p.parameters = append(c.p.parameters, param)
	}
	return nil
}

func (c *compiler) copyLayout(ctx context.Context, l model.TypeLayout, depth int) (model.TypeLayout, error) {
	if err := ctx.Err(); err != nil {
		return model.TypeLayout{}, err
	}
	if depth >= c.p.limits.MaxDepth {
		return model.TypeLayout{}, ErrLimit
	}
	if err := c.p.charge(uint64(unsafe.Sizeof(l)) * (1 + uint64(len(l.Children)))); err != nil {
		return model.TypeLayout{}, err
	}
	out := model.TypeLayout{Type: l.Type, Children: make([]model.TypeLayout, len(l.Children))}
	if len(l.FieldNames) != 0 {
		if err := c.p.charge(uint64(len(l.FieldNames)) * uint64(unsafe.Sizeof(""))); err != nil {
			return model.TypeLayout{}, err
		}
		out.FieldNames = make([]string, len(l.FieldNames))
		for i, name := range l.FieldNames {
			if err := ctx.Err(); err != nil {
				return model.TypeLayout{}, err
			}
			if err := c.p.charge(uint64(len(name))); err != nil {
				return model.TypeLayout{}, err
			}
			out.FieldNames[i] = strings.Clone(name)
		}
	}
	for i, child := range l.Children {
		var err error
		out.Children[i], err = c.copyLayout(ctx, child, depth+1)
		if err != nil {
			return model.TypeLayout{}, err
		}
	}
	return out, nil
}

func (c *compiler) scalar(ctx context.Context, id ir.ScalarID, depth int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if depth >= c.p.limits.MaxDepth || len(c.p.registers) >= c.p.limits.MaxRegisters {
		return 0, ErrLimit
	}
	n, ok := c.graph.Scalar(id)
	if !ok {
		return 0, invalid("unknown scalar root or operand")
	}
	layout, err := c.layout(ctx, n.Layout, 0)
	if err != nil {
		return 0, err
	}
	if err := c.p.charge(3 * uint64(unsafe.Sizeof(register{}))); err != nil {
		return 0, err
	}
	dst := len(c.p.registers)
	c.p.registers = append(c.p.registers, register{layout: layout, nullable: n.Nullable})
	init := instruction{op: opInit, dst: dst}
	if n.Kind == ir.Bool && n.BoolOp != ir.Not {
		init.index = 1
		if n.BoolOp == ir.Or {
			init.index = 2
		}
	}
	if err := c.p.emit(init); err != nil {
		return 0, err
	}
	if n.Kind == ir.Subquery {
		return dst, c.subquery(ctx, id, dst, depth)
	}
	if needsMask(n) {
		return dst, c.masked(ctx, id, n, dst, depth)
	}
	if n.Kind == ir.Once {
		return dst, c.singleEvaluation(ctx, id, dst, depth)
	}
	if n.Kind == ir.RowCompare {
		return dst, c.rowCompare(ctx, id, n, dst, depth)
	}
	if n.Kind == ir.ArrayCoerce {
		source, _ := c.graph.Operand(id, 0)
		input, err := c.scalar(ctx, source, depth+1)
		if err != nil {
			return 0, err
		}
		plan, err := c.compileArrayCoerce(ctx, id, n, depth+1)
		if err != nil {
			return 0, err
		}
		return dst, c.p.emit(instruction{op: opArrayCoerce, dst: dst, args: []int{input}, coercion: plan})
	}
	if n.Kind == ir.ArrayIndex || n.Kind == ir.ArraySlice {
		return dst, c.arrayFetch(ctx, id, n, dst, depth)
	}
	in := instruction{dst: dst}
	switch n.Kind {
	case ir.Const:
		remaining := min(c.p.limits.MaxConstantBytes-c.p.constantBytes, (c.p.limits.MaxBytes-c.p.bytes)/3)
		header := uint64(unsafe.Sizeof(model.Value{}))
		if remaining < header {
			return 0, ErrLimit
		}
		payload := remaining - header
		v, err := c.graph.CloneConstant(ctx, id, payload)
		if err != nil {
			return 0, err
		}
		cost, err := valueBytes(ctx, v, payload)
		if err != nil {
			return 0, err
		}
		cost += uint64(unsafe.Sizeof(v))
		if cost > c.p.limits.MaxConstantBytes-c.p.constantBytes {
			return 0, ErrLimit
		}
		c.p.constantBytes += cost
		if err := c.p.charge(3 * cost); err != nil {
			return 0, err
		}
		in.op, in.index = opConst, len(c.p.constants)
		c.p.constants = append(c.p.constants, v)
	case ir.Slot:
		if err := c.bindSlot(n.Slot, &in); err != nil {
			return 0, err
		}
	case ir.Param:
		in.op, in.index = opParam, -1
		for i, parameter := range c.p.parameters {
			if parameter.Number == n.Parameter && model.TypeLayoutsEqual(parameter.Layout, layout) {
				in.index = i
				break
			}
		}
		if in.index < 0 {
			return 0, invalid("parameter missing or layout mismatch")
		}
	case ir.Call, ir.Cast:
		in.op = opCall
	case ir.ArrayQuantified:
		in.op, in.subqueryKind = opArrayQuantified, n.SubqueryKind
	case ir.SQLJSON:
		in.op = opSQLJSON
	case ir.Collate:
		in.op = opCollate
	case ir.NullTest:
		in.op, in.negated = opNull, n.Negated
	case ir.Bool:
		in.op = opNot
	case ir.Row:
		in.op = opRow
	case ir.Array:
		in.op = opArray
	case ir.ArrayStack:
		in.op = opArrayStack
	case ir.Field:
		in.op, in.index = opField, int(n.FieldIndex)
	case ir.FieldStore:
		in.op, in.index = opFieldStore, int(n.FieldIndex)
	case ir.ArrayStore:
		in.op, in.subscripts, in.sliceAssignment = opArrayStore, n.Subscripts, n.SliceAssignment
	case ir.JSONBStore:
		in.op = opJSONBStore
	case ir.Relabel:
		in.op = opRelabel
	case ir.Least, ir.Greatest:
		in.op = opMinMax
	default:
		return 0, invalid("unsupported scalar kind")
	}
	for i := range n.OperandCount() {
		child, _ := c.graph.Operand(id, i)
		r, err := c.scalar(ctx, child, depth+1)
		if err != nil {
			return 0, err
		}
		in.args = append(in.args, r)
	}
	if n.Kind == ir.SQLJSON {
		if err := c.resolveSQLJSON(ctx, n, &in); err != nil {
			return 0, err
		}
	} else if in.op == opCall || in.op == opMinMax || in.op == opArrayQuantified {
		if err := c.resolve(ctx, n, &in); err != nil {
			return 0, err
		}
		if in.op == opArrayQuantified {
			in.strict = n.Strict
		}
	}
	return dst, c.p.emit(in)
}

func (c *compiler) resolveSQLJSON(ctx context.Context, n ir.Scalar, in *instruction) error {
	arguments := make([]model.TypeLayout, len(in.args))
	for i, register := range in.args {
		arguments[i] = c.p.registers[register].layout
	}
	result, err := c.layout(ctx, n.Layout, 0)
	if err != nil {
		return err
	}
	spec := builtin.SQLJSONPlanSpec{
		Kind:           builtin.SQLJSONKind(n.JSONOperation),
		InputFormat:    builtin.SQLJSONInputFormat(n.JSONFormat),
		InputEncoding:  builtin.SQLJSONEncoding(n.JSONEncoding),
		OutputFormat:   builtin.SQLJSONInputFormat(n.JSONOutputFormat),
		OutputEncoding: builtin.SQLJSONEncoding(n.JSONOutputEncoding),
		Wrapper:        builtin.SQLJSONWrapper(n.JSONWrapper),
		Quotes:         builtin.SQLJSONQuotes(n.JSONQuotes),
		ItemType:       builtin.SQLJSONItemType(n.JSONItemType),
		Passing:        int(n.JSONPassing),
		AbsentOnNull:   n.JSONFlags&ir.SQLJSONAbsentOnNull != 0,
		UniqueKeys:     n.JSONFlags&ir.SQLJSONUniqueKeys != 0,
		CaptureEmpty:   n.JSONFlags&ir.SQLJSONCaptureEmpty != 0,
		CaptureError:   n.JSONFlags&ir.SQLJSONCaptureError != 0,
		UseTZ:          n.JSONFlags&ir.SQLJSONUseTZ != 0,
	}
	plan, err := builtin.ResolveSQLJSON(ctx, spec, arguments, result, c.options.Builtin, c.p.limits.MaxBytes-c.p.bytes)
	if err != nil {
		return err
	}
	if err := c.p.charge(plan.OwnedBytes()); err != nil {
		return err
	}
	in.call = plan.Call
	in.ownsResult = true
	return nil
}

func (c *compiler) resolve(ctx context.Context, n ir.Scalar, in *instruction) error {
	in.identity = n.Function
	in.operator = n.Operator
	if n.Operator != 0 {
		op, ok := builtin.LookupOperator(n.Operator)
		if !ok {
			return invalid("operator has no resolved implementation identity")
		}
		in.identity = op.Function
	}
	signature, ok := c.graph.Signature(n.Signature)
	if !ok {
		return invalid("call signature missing")
	}
	if signature.SetReturning {
		return ErrSetReturning
	}
	result, ok := c.graph.Type(signature.Result)
	if !ok {
		return invalid("call result layout missing")
	}
	if sequenceFunction(in.identity) {
		return c.resolveSequence(n, in)
	}
	if relationFunction(in.identity) {
		return c.resolveRelation(n, in)
	}
	count := int(signature.ParameterCount())
	_, builtinFunction := builtin.LookupSignature(in.identity)
	if !builtinFunction && c.options.Builtin.InformationSchemaFunctions != nil {
		_, builtinFunction = c.options.Builtin.InformationSchemaFunctions.ResolveInformationSchemaFunction(in.identity)
	}
	if builtinFunction {
		if count > builtin.MaxCallArguments {
			return invalid("builtin argument count exceeds PostgreSQL limit")
		}
		var inlineTypes [8]model.Type
		var inlineLayouts [8]model.TypeLayout
		types, argumentLayouts := inlineTypes[:], inlineLayouts[:]
		if count > len(inlineTypes) {
			bytes := uint64(count) * (uint64(unsafe.Sizeof(model.Type{})) + uint64(unsafe.Sizeof(model.TypeLayout{})))
			if err := c.p.charge(bytes); err != nil {
				return err
			}
			types = make([]model.Type, count)
			argumentLayouts = make([]model.TypeLayout, count)
		}
		types, argumentLayouts = types[:count], argumentLayouts[:count]
		var err error
		for i := range count {
			layout, _ := c.graph.SignatureParameter(n.Signature, uint32(i))
			types[i], _ = c.graph.Type(layout)
			argumentLayouts[i] = model.TypeLayout{Type: types[i]}
			if c.graph.LayoutChildCount(layout) != 0 {
				argumentLayouts[i], err = c.layout(ctx, layout, 0)
				if err != nil {
					return err
				}
			}
		}
		resultLayout := model.TypeLayout{Type: result}
		if c.graph.LayoutChildCount(signature.Result) != 0 {
			resultLayout, err = c.layout(ctx, signature.Result, 0)
			if err != nil {
				return err
			}
		}
		options := c.options.Builtin
		options.Collation = builtin.InputCollation(signature.InputCollation, types)
		var callable builtin.Callable
		if signature.VariadicArray {
			callable, err = builtin.ResolveVariadic(ctx, in.identity, argumentLayouts, resultLayout, options, c.p.limits.MaxBytes-c.p.bytes)
		} else {
			callable, err = builtin.ResolveTyped(ctx, in.identity, argumentLayouts, resultLayout, options, c.p.limits.MaxBytes-c.p.bytes)
		}
		if err != nil {
			return err
		}
		if err := c.p.charge(callable.OwnedBytes()); err != nil {
			return err
		}
		sig := callable.Signature()
		if sig.Strict != n.Strict || sig.Result != result.ID || callable.ArgumentCount() != count {
			return invalid("builtin signature mismatch")
		}
		in.call = callable.Call
		in.vector = callable.VectorKernel()
		in.ownsResult = true
		return nil
	}
	if c.options.Functions == nil {
		return &builtin.UnknownFunctionError{ID: in.identity}
	}
	resultLayout, err := c.layout(ctx, signature.Result, 0)
	if err != nil {
		return err
	}
	f := Function{ID: in.identity, Strict: n.Strict, Result: resultLayout, InputCollation: signature.InputCollation, VariadicArray: signature.VariadicArray}
	for i := range count {
		id, _ := c.graph.SignatureParameter(n.Signature, uint32(i))
		layout, err := c.layout(ctx, id, 0)
		if err != nil {
			return err
		}
		f.Arguments = append(f.Arguments, layout)
	}
	call, err := c.options.Functions.Resolve(f)
	if err != nil {
		return err
	}
	if call == nil {
		return invalid("resolver returned nil capability")
	}
	in.negated = n.Strict
	in.call = call
	return nil
}

func (c *compiler) masked(ctx context.Context, id ir.ScalarID, n ir.Scalar, dst, depth int) error {
	p := c.p
	if err := p.emit(instruction{op: opEnter, mask: maskAll}); err != nil {
		return err
	}
	count := n.OperandCount()
	for i := uint32(0); i < count; i++ {
		child, _ := c.graph.Operand(id, i)
		r, err := c.scalar(ctx, child, depth+1)
		if err != nil {
			return err
		}
		if n.Kind == ir.Case && i < count-1 {
			if err := p.emit(instruction{op: opEnter, args: []int{r}, mask: maskTrue}); err != nil {
				return err
			}
			i++
			branch, _ := c.graph.Operand(id, i)
			value, err := c.scalar(ctx, branch, depth+1)
			if err != nil {
				return err
			}
			if err := p.emit(instruction{op: opCopy, dst: dst, args: []int{value}}); err != nil {
				return err
			}
			if err := p.emit(instruction{op: opLeave}); err != nil {
				return err
			}
			if err := p.emit(instruction{op: opFilter, args: []int{r}, mask: maskNotTrue}); err != nil {
				return err
			}
			continue
		}
		op, mask := opCopy, maskNull
		if n.Kind == ir.Bool {
			op, mask = opMergeAnd, maskAnd
			if n.BoolOp == ir.Or {
				op, mask = opMergeOr, maskOr
			}
		}
		if err := p.emit(instruction{op: op, dst: dst, args: []int{r}}); err != nil {
			return err
		}
		if i < count-1 {
			if err := p.emit(instruction{op: opFilter, args: []int{dst}, mask: mask}); err != nil {
				return err
			}
		}
	}
	return p.emit(instruction{op: opLeave})
}

func (c *compiler) singleEvaluation(ctx context.Context, id ir.ScalarID, dst, depth int) error {
	cache, exists := c.once[id]
	if !exists {
		if len(c.p.registers) >= c.p.limits.MaxRegisters {
			return ErrLimit
		}
		if err := c.p.charge(64 + 3*uint64(unsafe.Sizeof(register{}))); err != nil {
			return err
		}
		cache = len(c.p.registers)
		r := c.p.registers[dst]
		r.once = true
		c.p.registers = append(c.p.registers, r)
		if c.once == nil {
			c.once = make(map[ir.ScalarID]int)
		}
		c.once[id] = cache
	}
	if err := c.p.emit(instruction{op: opOnceEnter, index: cache}); err != nil {
		return err
	}
	child, _ := c.graph.Operand(id, 0)
	value, err := c.scalar(ctx, child, depth+1)
	if err != nil {
		return err
	}
	if err := c.p.emit(instruction{op: opOnceStore, index: cache, args: []int{value}}); err != nil {
		return err
	}
	return c.p.emit(instruction{op: opOnceRead, dst: dst, index: cache})
}
