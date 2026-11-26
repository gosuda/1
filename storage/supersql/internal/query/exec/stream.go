package exec

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func batchRows(out *columnar.Batch, env *Environment) int {
	if out.MaxRows > 0 {
		return min(out.MaxRows, env.Limits.BatchRows)
	}
	return env.Limits.BatchRows
}
func builder(env *Environment, layouts []model.TypeLayout) (*columnar.Builder, error) {
	return columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: env.Limits.BatchRows, MaxBytes: env.Limits.BatchBytes, Allocator: env.Query.Allocator(runtime.OutputMemory)})
}

// appendLane copies only an active physical lane. The temporary datum tree is
// admitted separately because Append owns a second copy until this call returns.
func appendLane(ctx context.Context, env *Environment, destination *columnar.Builder, batch *columnar.Batch, row int) error {
	lane := batch.Lane(row)
	var bytes uint64
	for i := range batch.Columns {
		size, err := columnar.ValueBytes(ctx, &batch.Columns[i], lane, env.Limits.BatchBytes-bytes)
		if err != nil {
			return err
		}
		bytes += size
	}
	claim, err := env.Query.Allocator(runtime.WorkMemory).Reserve(bytes)
	if err != nil {
		return err
	}
	defer claim.Release()
	values := env.row[:len(batch.Columns)]
	defer clear(values)
	for i := range values {
		value, _, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, env.Limits.BatchBytes)
		if err != nil {
			return err
		}
		values[i] = value
	}
	return destination.Append(ctx, values)
}

func publishView(out *columnar.Batch, input *columnar.Batch) {
	out.Columns = append(out.Columns[:0], input.Columns...)
	out.Length = input.Length
	out.Selection = input.Selection
}

type valuesCursor struct {
	env       *Environment
	layouts   []model.TypeLayout
	states    []*expr.State
	rows      uint64
	next      uint64
	output    *columnar.Builder
	evaluated columnar.Batch
	closed    bool
	closeErr  error
}

func (v *valuesCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if v.closed || v.next == v.rows {
		return io.EOF
	}
	if v.output != nil {
		if err := v.output.Close(); err != nil {
			return err
		}
		v.output = nil
	}
	var err error
	v.output, err = builder(v.env, v.layouts)
	if err != nil {
		return err
	}
	limit := batchRows(out, v.env)
	for v.next < v.rows && v.output.Rows() < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(v.layouts) == 0 {
			if err = v.output.Append(ctx, nil); err != nil {
				return err
			}
		} else {
			input := columnar.Batch{Length: 1}
			state := v.states[v.next]
			if err = state.Eval(ctx, &input, v.env.Parameters, &v.evaluated, expr.Direct); err != nil {
				return err
			}
			if err = appendLane(ctx, v.env, v.output, &v.evaluated, 0); err != nil {
				return err
			}
			if err = state.Close(); err != nil {
				return err
			}
			v.states[v.next] = nil
		}
		v.next++
	}
	return v.output.Publish(out)
}
func (v *valuesCursor) Close() error {
	if v.closed {
		return v.closeErr
	}
	v.closed = true
	v.evaluated.Reset()
	for _, state := range v.states {
		if state != nil {
			v.closeErr = errors.Join(v.closeErr, state.Close())
		}
	}
	v.states = nil
	if v.output != nil {
		v.closeErr = errors.Join(v.closeErr, v.output.Close())
		v.output = nil
	}
	return v.closeErr
}

type scanCursor struct {
	reader      storage.SnapshotReader
	spec        physical.ScanSpec
	decision    [32]byte
	maxBytes    uint64
	cursor      storage.BatchCursor
	releasePlan, ignoreOrdering bool
	issuedPlan    *storage.PushdownPlan
	closed      bool
	closeErr    error
}

func (s *scanCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if s.closed {
		return io.EOF
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.cursor == nil {
		plan, err := s.reader.PlanScan(ctx, s.spec.Request)
		if s.releasePlan && plan.Binding != ([32]byte{}) {
			s.issuedPlan = &plan
		}
		if err != nil {
			return err
		}
		decisionPlan := plan
		if s.ignoreOrdering {
			decisionPlan.Ordering = nil
		}
		decision, err := scanDecision(ctx, decisionPlan, s.maxBytes)
		if err != nil {
			return err
		}
		if decision != s.decision {
			return ErrCapability
		}
		s.cursor, err = s.reader.VectorizedScan(ctx, plan)
		if err != nil {
			return err
		}
		if s.cursor == nil {
			return ErrCapability
		}
	}
	if err := s.cursor.Next(ctx, out); err != nil {
		return err
	}
	if len(out.Columns) != len(s.spec.Projection) {
		return ErrCapability
	}
	return nil
}
func (s *scanCursor) SampleUnit(lane uint32) (uint64, bool) {
	source, ok := s.cursor.(storage.SamplingCursor)
	if !ok {
		return 0, false
	}
	return source.SampleUnit(lane)
}

func (s *scanCursor) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.cursor != nil {
		s.closeErr = s.cursor.Close()
		s.cursor = nil
	}
	if s.issuedPlan != nil {
		s.closeErr = errors.Join(s.closeErr, s.reader.ReleasePlan(*s.issuedPlan))
		s.issuedPlan = nil
	}
	return s.closeErr
}

type expressionCursor struct {
	input      storage.BatchCursor
	state      *expr.State
	env        *Environment
	layouts    []model.TypeLayout
	filter     bool
	projection []int
	source     columnar.Batch
	evaluated  columnar.Batch
	compact    columnar.Batch
	gather     *columnar.Builder
	selection  []uint32
	closed     bool
	closeErr   error
}

func (e *expressionCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if e.closed {
		return io.EOF
	}
	if e.gather != nil {
		if err := e.gather.Close(); err != nil {
			return err
		}
		e.gather = nil
		e.compact.Reset()
	}
	e.source.MaxRows = batchRows(out, e.env)
	if err := e.input.Next(ctx, &e.source); err != nil {
		return err
	}
	input := &e.source
	if e.projection != nil {
		for _, col := range e.projection {
			if col >= len(input.Columns) {
				return ErrInvalid
			}
			out.Columns = append(out.Columns, input.Columns[col])
		}
		out.Length = input.Length
		out.Selection = input.Selection
		return nil
	}
	if input.Length > e.env.Limits.Expressions.MaxRows {
		var err error
		e.gather, err = builder(e.env, e.layouts)
		if err != nil {
			return err
		}
		for row := range input.Rows() {
			if err = appendLane(ctx, e.env, e.gather, input, row); err != nil {
				return err
			}
		}
		e.compact.MaxRows = e.source.MaxRows
		if err = e.gather.Publish(&e.compact); err != nil {
			return err
		}
		input = &e.compact
	}
	if e.state == nil {
		out.Length = input.Rows()
		return nil
	}
	e.evaluated.MaxRows = input.Rows()
	if err := e.state.Eval(ctx, input, e.env.Parameters, &e.evaluated, expr.Direct); err != nil {
		return err
	}
	if !e.filter {
		publishView(out, &e.evaluated)
		return nil
	}
	// Filter results are dense in logical selection order. Preserve input payloads
	// and map surviving result rows back to their physical lanes.
	e.selection = e.selection[:0]
	predicate := &e.evaluated.Columns[0]
	for row := range input.Rows() {
		if !predicate.IsNull(row) && predicate.Int64[predicate.ValueIndex(row)] != 0 {
			e.selection = append(e.selection, uint32(input.Lane(row)))
		}
	}
	publishView(out, input)
	out.Selection = e.selection
	return nil
}
func (e *expressionCursor) SampleUnit(lane uint32) (uint64, bool) {
	source, ok := e.input.(storage.SamplingCursor)
	if !ok || e.gather != nil {
		return 0, false
	}
	if e.state != nil && !e.filter && e.projection == nil {
		if uint64(lane) >= uint64(e.source.Rows()) {
			return 0, false
		}
		lane = uint32(e.source.Lane(int(lane)))
	}
	return source.SampleUnit(lane)
}

func (e *expressionCursor) Close() error {
	if e.closed {
		return e.closeErr
	}
	e.closed = true
	e.source.Reset()
	e.evaluated.Reset()
	e.compact.Reset()
	e.selection = nil
	e.closeErr = e.input.Close()
	if e.state != nil {
		e.closeErr = errors.Join(e.closeErr, e.state.Close())
		e.state = nil
	}
	if e.gather != nil {
		e.closeErr = errors.Join(e.closeErr, e.gather.Close())
		e.gather = nil
	}
	return e.closeErr
}

type unionCursor struct {
	inputs   []storage.BatchCursor
	next     int
	closed   bool
	closeErr error
}

func (u *unionCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if u.closed {
		return io.EOF
	}
	for u.next < len(u.inputs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := u.inputs[u.next].Next(ctx, out)
		if err != io.EOF {
			return err
		}
		if err = u.inputs[u.next].Close(); err != nil {
			return err
		}
		u.next++
	}
	return io.EOF
}
func (u *unionCursor) Close() error {
	if u.closed {
		return u.closeErr
	}
	u.closed = true
	for _, input := range u.inputs {
		u.closeErr = errors.Join(u.closeErr, input.Close())
	}
	u.inputs = nil
	return u.closeErr
}
