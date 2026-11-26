package expr

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type Mode uint8

const (
	Direct Mode = iota
	ScalarCalls
)

type ParameterValue struct {
	Number uint32
	Layout model.TypeLayout
	Value  model.Value
}

type laneRegister struct {
	vector   columnar.Vector
	values   []model.Value
	computed []byte
}
type batchStep func(*evaluation, instruction) error

// State is single-owner and not concurrent. Eval borrows input and parameters
// only during execution. Output payloads live until the next evaluation or Close.
// Retain makes an independent, budgeted copy; its owner must Close it.
type State struct {
	program    *Program
	stateBytes uint64
	registers  []laneRegister
	builder    *columnar.Builder
	options    StateOptions
	work       columnar.Allocation
	closed     bool
}

type StateOptions struct {
	// Captures is immutable and borrowed until Close; nil requires no capture dependencies.
	Captures        *columnar.Batch
	WorkAllocator   columnar.Allocator
	OutputAllocator columnar.Allocator
	// MaxStateBytes clamps the working-state budget below the program's
	// compiled limit so a nested evaluation honors its caller's grant. Zero
	// applies the program limit unchanged; larger values are clamped to it.
	MaxStateBytes uint64
	// Subqueries is borrowed, invocation-bound, and called serially until Close.
	Subqueries SubqueryRuntime
	Sequences  model.SequenceRuntime
	Relations  model.RelationRuntime
	Temporal   model.TemporalRuntime
	Settings   model.SettingsRuntime
	TextSearch model.TextSearchRuntime
	Advisory   model.AdvisoryLockRuntime
	Native     model.NativeScalarRuntime
}

func NewState(program *Program, options StateOptions) (*State, error) {
	if program == nil || !program.verified {
		return nil, ErrInvalid
	}
	if len(program.subqueries) != 0 && options.Subqueries == nil {
		return nil, invalid("scalar subquery runtime missing")
	}
	if program.sequences && options.Sequences == nil {
		return nil, invalid("sequence runtime missing")
	}
	if program.relations && options.Relations == nil {
		return nil, invalid("relation OID runtime missing")
	}
	if err := program.validateCaptures(options.Captures); err != nil {
		return nil, err
	}
	stateBytes := program.limits.MaxStateBytes
	if options.MaxStateBytes != 0 && options.MaxStateBytes < stateBytes {
		stateBytes = options.MaxStateBytes
	}
	return &State{program: program, stateBytes: stateBytes, options: options}, nil
}

func (p *Program) validateCaptures(batch *columnar.Batch) error {
	if batch == nil {
		for _, capture := range p.captures {
			if capture.used {
				return invalid("missing capture batch")
			}
		}
		return nil
	}
	if batch.Rows() != 1 {
		return invalid("capture batch must contain one logical row")
	}
	if err := batch.Validate(p.limits.MaxRows); err != nil {
		return err
	}
	for _, capture := range p.captures {
		if capture.Column >= len(batch.Columns) || !batch.Columns[capture.Column].MatchesLayout(capture.layout) {
			return invalid("capture slot layout mismatch")
		}
	}
	return nil
}

func (s *State) releaseRegisters() {
	s.registers = nil
	if s.work != nil {
		s.work.Release()
		s.work = nil
	}
}

func (s *State) release() error {
	s.releaseRegisters()
	if s.builder == nil {
		return nil
	}
	err := s.builder.Close()
	s.builder = nil
	return err
}

func (s *State) Close() error {
	if s == nil {
		return nil
	}
	err := s.release()
	s.program = nil
	s.options = StateOptions{}
	s.closed = true
	return err
}

type evaluation struct {
	ctx           context.Context
	state         *State
	input         *columnar.Batch
	parameters    []ParameterValue
	masks         [][]int
	depth         int
	used          uint64
	args          []model.Value
	vectors       []*columnar.Vector
	mode          Mode
	callAdmission bool
	callLimit     uint64
}

func (e *evaluation) charge(n uint64) error {
	if n > e.state.stateBytes-e.used {
		return ErrLimit
	}
	if e.callAdmission {
		if e.used+n > e.callLimit {
			return ErrLimit
		}
	} else if e.state.work != nil {
		if err := e.state.work.Resize(e.used + n); err != nil {
			return err
		}
	}
	e.used += n
	return nil
}
func (e *evaluation) remaining() uint64 { return e.state.stateBytes - e.used }
func (e *evaluation) lanes() []int      { return e.masks[e.depth] }

func (s *State) Eval(ctx context.Context, input *columnar.Batch, parameters []ParameterValue, out *columnar.Batch, mode Mode) (err error) {
	if s == nil || s.closed || s.program == nil {
		return ErrClosed
	}
	if input == nil || out == nil || input == out || out == s.options.Captures {
		return errors.Join(ErrInvalid, s.release())
	}
	out.Reset()
	if err := s.release(); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Reset()
			err = errors.Join(err, s.release())
		}
	}()
	defer s.releaseRegisters()
	row, err := s.evaluate(ctx, input, parameters, out.MaxRows, mode)
	if err != nil {
		return err
	}
	p := s.program
	s.builder, err = columnar.NewBuilder(p.outputLayouts, columnar.BuilderOptions{MaxRows: p.limits.MaxRows, MaxBytes: p.limits.MaxOutputBytes, Allocator: s.options.OutputAllocator})
	if err != nil {
		return err
	}
	for lane := range input.Rows() {
		for i, r := range p.outputs {
			row[i] = s.registers[r].value(lane)
			if row[i].IsNull && !p.registers[r].nullable {
				return invalid("non-nullable expression returned NULL")
			}
		}
		if err := s.builder.Append(ctx, row); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.builder.Publish(out); err != nil {
		return err
	}
	return nil
}

func (s *State) evaluate(ctx context.Context, input *columnar.Batch, parameters []ParameterValue, maxRows int, mode Mode) ([]model.Value, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := s.program
	if mode > ScalarCalls {
		return nil, ErrInvalid
	}
	if err := input.Validate(p.limits.MaxRows); err != nil {
		return nil, err
	}
	if maxRows < 0 || (maxRows > 0 && input.Rows() > maxRows) {
		return nil, ErrLimit
	}
	if len(parameters) != len(p.parameters) {
		return nil, invalid("parameter count mismatch")
	}
	for i, expected := range p.parameters {
		if parameters[i].Number != expected.Number || !model.TypeLayoutsEqual(parameters[i].Layout, expected.Layout) {
			return nil, invalid("parameter identity or layout mismatch")
		}
		if err := validateValue(ctx, parameters[i].Value, expected.Layout, 0); err != nil {
			return nil, err
		}
	}
	for _, in := range p.code {
		if in.op != opInput {
			continue
		}
		mapping := p.inputs[in.index]
		if mapping.Column >= len(input.Columns) || !input.Columns[mapping.Column].MatchesLayout(p.registers[in.dst].layout) {
			return nil, invalid("input slot layout mismatch")
		}
	}
	e := evaluation{ctx: ctx, state: s, input: input, parameters: parameters, mode: mode}
	if s.options.WorkAllocator != nil {
		var err error
		s.work, err = s.options.WorkAllocator.Reserve(0)
		if err != nil {
			return nil, err
		}
	}
	if err := e.allocate(input.Rows()); err != nil {
		return nil, err
	}
	for i, in := range p.code {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := p.direct[i](&e, in); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.args[:len(p.outputs)], nil
}

func (e *evaluation) allocate(rows int) error {
	p := e.state.program
	maxArgs := len(p.outputs)
	for _, in := range p.code {
		switch in.op {
		case opCall, opSQLJSON, opSubquery, opArrayQuantified, opFieldStore, opArrayStore, opJSONBStore:
			// Every opcode that walks its whole argument list must be sized here.
			// evalJSONBStore slices e.args to len(in.args), so a store instruction
			// whose arity exceeds len(p.outputs) reads past the allocation.
			maxArgs = max(maxArgs, len(in.args))
		case opMinMax:
			maxArgs = max(maxArgs, 2)
		case opArrayCoerce:
			maxArgs = max(maxArgs, in.coercion.maxArgs)
		}
	}
	base := uint64(len(p.registers))*uint64(unsafe.Sizeof(laneRegister{})) + uint64(maxArgs)*(uint64(unsafe.Sizeof(model.Value{}))+uint64(unsafe.Sizeof((*columnar.Vector)(nil))))
	if len(p.subqueries) != 0 {
		// One synchronous consumer frame is live while the body owns its result.
		base += 3 * (uint64(unsafe.Sizeof(compiledSubquery{})) + uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(error(nil))) + 32)
	}
	maskDepth := 1
	depth := 1
	for _, in := range p.code {
		if in.op == opEnter || in.op == opOnceEnter {
			depth++
			maskDepth = max(maskDepth, depth)
		}
		if in.op == opLeave || in.op == opOnceStore {
			depth--
		}
	}
	base += uint64(maskDepth) * (uint64(rows)*uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof([]int{})))
	if err := e.charge(base); err != nil {
		return err
	}
	e.args = make([]model.Value, maxArgs)
	e.vectors = make([]*columnar.Vector, maxArgs)
	e.masks = make([][]int, maskDepth)
	for i := range e.masks {
		e.masks[i] = make([]int, 0, rows)
	}
	for i := range rows {
		e.masks[0] = append(e.masks[0], i)
	}
	e.state.registers = make([]laneRegister, len(p.registers))
	for i, r := range p.registers {
		dst := &e.state.registers[i]
		dst.vector = columnar.Vector{Type: r.layout.Type, Length: rows}
		nulls := (uint64(rows) + 7) / 8
		width := uint64(8)
		if !fixed(r.layout.Type.Storage) {
			width = uint64(unsafe.Sizeof(model.Value{}))
		}
		if uint64(rows) > (e.remaining()-min(e.remaining(), nulls))/width {
			return ErrLimit
		}
		if err := e.charge(uint64(rows)*width + nulls); err != nil {
			return err
		}
		if r.once {
			if err := e.charge(nulls); err != nil {
				return err
			}
			dst.computed = make([]byte, int(nulls))
		}
		dst.vector.Nulls = make([]byte, int(nulls))
		switch r.layout.Type.Storage {
		case model.TypeInt64, model.TypeBoolean, model.TypeTimestamp:
			dst.vector.Int64 = make([]int64, rows)
		case model.TypeFloat64:
			dst.vector.Float64 = make([]float64, rows)
		default:
			dst.values = make([]model.Value, rows)
		}
	}
	return nil
}

func fixed(kind model.DataType) bool {
	return kind == model.TypeInt64 || kind == model.TypeBoolean || kind == model.TypeTimestamp || kind == model.TypeFloat64
}
func (r *laneRegister) null(lane int) bool { return r.vector.Nulls[lane/8]&(1<<uint(lane%8)) != 0 }
func (r *laneRegister) value(lane int) model.Value {
	if r.null(lane) {
		return model.NullVal(r.vector.Type.Storage)
	}
	switch r.vector.Type.Storage {
	case model.TypeInt64, model.TypeBoolean, model.TypeTimestamp:
		return model.Value{Type: r.vector.Type.Storage, IntVal: r.vector.Int64[lane]}
	case model.TypeFloat64:
		return model.Float64Val(r.vector.Float64[lane])
	default:
		return r.values[lane]
	}
}
func (r *laneRegister) put(lane int, value model.Value) {
	if value.IsNull {
		r.vector.Nulls[lane/8] |= 1 << uint(lane%8)
		return
	}
	r.vector.Nulls[lane/8] &^= 1 << uint(lane%8)
	switch r.vector.Type.Storage {
	case model.TypeInt64, model.TypeBoolean, model.TypeTimestamp:
		r.vector.Int64[lane] = value.IntVal
	case model.TypeFloat64:
		r.vector.Float64[lane] = value.FloatVal
	default:
		r.values[lane] = value
	}
}

func integerFits(value int64, id model.TypeID) bool {
	switch id {
	case 21:
		return value >= math.MinInt16 && value <= math.MaxInt16
	case 23:
		return value >= math.MinInt32 && value <= math.MaxInt32
	default:
		if builtin.IsOIDType(id) {
			return value >= 0 && value <= math.MaxUint32
		}
		return true
	}
}

func validateValue(ctx context.Context, v model.Value, l model.TypeLayout, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 || v.Type != l.Type.Storage {
		return invalid("value storage mismatch")
	}
	if v.IsNull {
		return nil
	}
	switch v.Type {
	case model.TypeBoolean:
		if v.IntVal != 0 && v.IntVal != 1 {
			return invalid("invalid boolean")
		}
	case model.TypeInt64:
		if !integerFits(v.IntVal, l.Type.ID) {
			return &model.Error{Code: "22003", Message: "integer out of range"}
		}
	case model.TypeFloat64:
		if l.Type.ID == 700 && !math.IsNaN(v.FloatVal) && float64(float32(v.FloatVal)) != v.FloatVal {
			return invalid("float4 value not rounded to binary32")
		}
	case model.TypeBytes:
		if l.Type.ID == 1266 {
			if _, _, err := builtin.DecodeTimetz(v.BytesVal); err != nil {
				return err
			}
		}
		switch l.Type.ID {
		case 600, 601, 602, 603, 604, 628, 718:
			return builtin.ValidateGeometryValue(ctx, l.Type.ID, v.BytesVal)
		}
		if _, known := builtin.LookupRangeFamily(l.Type.ID); known {
			return builtin.ValidateRangeValue(ctx, l.Type.ID, v.BytesVal)
		}
	case model.TypeArray:
		if len(l.Children) != 1 {
			return ErrInvalid
		}
		if err := model.ValidateArrayShape(v.Dimensions, uint64(len(v.ArrayVal))); err != nil {
			return err
		}
		for _, value := range v.ArrayVal {
			if err := validateValue(ctx, value, l.Children[0], depth+1); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(v.ArrayVal) != len(l.Children) {
			return ErrInvalid
		}
		for i, value := range v.ArrayVal {
			if err := validateValue(ctx, value, l.Children[i], depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// Retained owns an independent immutable snapshot, valid across producer Close.
// Close releases the owner's references without mutating borrowed published nodes.
type Retained struct{ builder *columnar.Builder }

type RetainOptions struct {
	MaxRows       int
	MaxBytes      uint64
	WorkAllocator columnar.Allocator
	// Nil permits the retained snapshot to outlive the producing query. A
	// nonnil allocator must itself outlive Retained.Close.
	OutputAllocator columnar.Allocator
}

func Retain(ctx context.Context, input *columnar.Batch, layouts []model.TypeLayout, options RetainOptions) (_ *Retained, err error) {
	maxRows, maxBytes := options.MaxRows, options.MaxBytes
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || len(input.Columns) != len(layouts) {
		return nil, ErrInvalid
	}
	if err := input.Validate(maxRows); err != nil {
		return nil, err
	}
	for i := range layouts {
		if !input.Columns[i].MatchesLayout(layouts[i]) {
			return nil, ErrInvalid
		}
	}
	rowBytes := uint64(len(layouts)) * uint64(unsafe.Sizeof(model.Value{}))
	if rowBytes >= maxBytes {
		return nil, ErrLimit
	}
	// A copied row remains live while Append grows the owned vector buffers.
	payloadBudget := (maxBytes - rowBytes) / 2
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: maxRows, MaxBytes: payloadBudget, Allocator: options.OutputAllocator})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, builder.Close())
		}
	}()
	var work columnar.Allocation
	if options.WorkAllocator != nil {
		work, err = options.WorkAllocator.Reserve(rowBytes)
		if err != nil {
			return nil, err
		}
	}
	row := make([]model.Value, len(layouts))
	defer func() {
		row = nil
		if work != nil {
			work.Release()
		}
	}()
	for i := range input.Rows() {
		clear(row)
		if work != nil {
			if err := work.Resize(rowBytes); err != nil {
				return nil, err
			}
		}
		remaining := payloadBudget
		for col := range layouts {
			bytes, err := columnar.ValueBytes(ctx, &input.Columns[col], input.Lane(i), remaining)
			if err != nil {
				return nil, err
			}
			remaining -= bytes
			if work != nil {
				if err := work.Resize(rowBytes + payloadBudget - remaining); err != nil {
					return nil, err
				}
			}
			value, _, err := columnar.CopyValue(ctx, &input.Columns[col], input.Lane(i), bytes)
			if err != nil {
				return nil, err
			}
			row[col] = value
		}
		if err := builder.Append(ctx, row); err != nil {
			return nil, err
		}
	}
	return &Retained{builder: builder}, nil
}
func (r *Retained) Publish(out *columnar.Batch) error {
	if r == nil || r.builder == nil {
		return ErrClosed
	}
	return r.builder.Publish(out)
}
func (r *Retained) Close() error {
	if r == nil || r.builder == nil {
		return nil
	}
	err := r.builder.Close()
	r.builder = nil
	return err
}
