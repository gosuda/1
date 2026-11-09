package exec

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type sampleCursor struct {
	input      storage.BatchCursor
	source     storage.SamplingCursor
	method     ir.SampleMethod
	percentage float64
	seed       uint64
	selection  []uint32
	node       physical.NodeID
	closed     bool
	closeErr   error
}

func (c *invocation) sample(id physical.NodeID, node physical.NodeSpec, children []storage.BatchCursor) (storage.BatchCursor, error) {
	if len(children) != 1 {
		return nil, ErrInvalid
	}
	graph, ok := c.plan.Graph(node.Logical.Graph)
	if !ok {
		return nil, ErrInvalid
	}
	relation, ok := graph.Rel(node.Logical.Rel)
	if !ok || relation.Kind != ir.Sample || relation.ExpressionCount() < 1 || relation.ExpressionCount() > 2 {
		return nil, ErrInvalid
	}
	rateID, _ := graph.Expression(node.Logical.Rel, 0)
	rate, err := c.scalarValue(node.Logical.Graph, rateID)
	if err != nil {
		return nil, err
	}
	var repeatable model.Value
	if relation.SampleRepeatable {
		seedID, ok := graph.Expression(node.Logical.Rel, 1)
		if !ok {
			return nil, ErrInvalid
		}
		repeatable, err = c.scalarValue(node.Logical.Graph, seedID)
		if err != nil {
			return nil, err
		}
	}
	if err := c.charge(uint64(c.portal.env.Limits.BatchRows) * 4); err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, nil
	}
	percentage, err := samplePercentage(rate)
	if err != nil {
		return nil, err
	}
	seed := c.portal.env.SampleSeed
	if relation.SampleRepeatable {
		if repeatable.IsNull || repeatable.Type != model.TypeFloat64 {
			return nil, &model.Error{Code: "2202H", Message: "TABLESAMPLE REPEATABLE seed must not be null"}
		}
		seed = mixSample(math.Float64bits(repeatable.FloatVal))
	}
	source, ok := children[0].(storage.SamplingCursor)
	if !ok {
		return nil, &RequiredExecutionError{Node: id, Algorithm: physical.Sample, Reason: "storage-native sample units required"}
	}
	return &sampleCursor{
		input: children[0], source: source, method: relation.SampleMethod,
		percentage: percentage, seed: seed, selection: make([]uint32, 0, c.portal.env.Limits.BatchRows), node: id,
	}, nil
}

func (c *invocation) scalarValue(graph physical.GraphID, root ir.ScalarID) (value model.Value, err error) {
	state, err := c.compile(graph, []ir.ScalarID{root}, nil)
	if err != nil {
		return model.Value{}, err
	}
	if c.preparing {
		return model.Value{}, nil
	}
	defer func() { err = errors.Join(err, state.Close()) }()
	input := columnar.Batch{Length: 1}
	var out columnar.Batch
	if err = state.Eval(c.portal.ctx, &input, c.portal.env.Parameters, &out, expr.Direct); err != nil {
		return model.Value{}, err
	}
	value, _, err = columnar.CopyValue(c.portal.ctx, &out.Columns[0], 0, 0)
	return value, err
}

func samplePercentage(value model.Value) (float64, error) {
	if value.IsNull || value.Type != model.TypeFloat64 || math.IsNaN(value.FloatVal) || value.FloatVal < 0 || value.FloatVal > 100 {
		return 0, &model.Error{Code: "2202H", Message: "TABLESAMPLE percentage must be between 0 and 100"}
	}
	return value.FloatVal, nil
}

func (c *sampleCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if c.closed {
		return io.EOF
	}
	if err := c.input.Next(ctx, out); err != nil {
		return err
	}
	c.selection = c.selection[:0]
	for row := range out.Rows() {
		lane := uint32(out.Lane(row))
		unit, ok := c.source.SampleUnit(lane)
		if !ok {
			return &RequiredExecutionError{Node: c.node, Algorithm: physical.Sample, Reason: "sample unit unavailable for storage lane"}
		}
		if c.method == ir.BernoulliSample {
			unit = mixSample(unit ^ uint64(lane)*0x9e3779b97f4a7c15)
		}
		if sampleAccept(c.seed, unit, c.percentage) {
			c.selection = append(c.selection, lane)
		}
	}
	out.Selection = c.selection
	return nil
}

func sampleAccept(seed, unit uint64, percentage float64) bool {
	if percentage <= 0 {
		return false
	}
	if percentage >= 100 {
		return true
	}
	value := mixSample(seed ^ unit)
	fraction := float64(value>>11) * (1.0 / (1 << 53))
	return fraction*100 < percentage
}

func mixSample(value uint64) uint64 {
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	value ^= value >> 31
	return value
}

func (c *sampleCursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	c.selection = nil
	c.closeErr = c.input.Close()
	c.input, c.source = nil, nil
	return c.closeErr
}
