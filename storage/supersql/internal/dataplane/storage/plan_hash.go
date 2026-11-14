package storage

import (
	"context"
	"crypto/sha256"
	"hash"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// HashPushdownPlan covers execution metadata, excluding Binding itself. A
// digest is not an authorization token: the reader must authenticate its issuer.
func HashPushdownPlan(ctx context.Context, plan PushdownPlan, maxBytes uint64) ([32]byte, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	if err := plan.Request.Validate(ctx); err != nil {
		return [32]byte{}, err
	}
	writer := planHashWriter{ctx: ctx, hash: sha256.New(), remaining: maxBytes}
	encoder := mutationEncoder{writer: &writer}
	encoder.bytes([]byte("supersql-pushdown-plan-v1"))
	encoder.objectLayout(ObjectLayout{Target: plan.Request.Target, Columns: plan.Request.Layout})
	if plan.Request.Occurrence != 0 {
		encoder.bytes([]byte("scan-occurrence"))
		encoder.number(uint64(plan.Request.Occurrence), 4)
	}
	encoder.number(uint64(plan.Request.IndexID), 8)
	encoder.number(uint64(len(plan.Request.KeyLayout)), 8)
	for _, column := range plan.Request.KeyLayout {
		if encoder.err != nil {
			break
		}
		encoder.number(uint64(column.ColumnID), 4)
		encoder.typeLayout(model.TypeLayout{Type: column.Type, Children: column.Children})
		encoder.number(planFlag(column.Descending), 1)
		encoder.number(planFlag(column.NullsFirst), 1)
	}
	encoder.planColumns(plan.Request.ColumnIDs)
	encoder.number(uint64(len(plan.Request.Spans)), 8)
	for _, span := range plan.Request.Spans {
		if encoder.err != nil {
			break
		}
		encoder.planBound(span.Lower)
		encoder.planBound(span.Upper)
	}
	encoder.number(uint64(len(plan.Request.EncodedSpans)), 8)
	for _, span := range plan.Request.EncodedSpans {
		if encoder.err != nil {
			break
		}
		encoder.planEncodedBound(span.Lower)
		encoder.planEncodedBound(span.Upper)
	}
	encoder.number(uint64(plan.Request.Direction), 1)
	encoder.planPredicate(plan.Request.Predicate)
	encoder.planLimit(plan.Request.SafeLimit)
	encoder.planPredicate(plan.Accepted)
	encoder.planPredicate(plan.Residual)
	encoder.planColumns(plan.Projection)
	encoder.planLimit(plan.AppliedLimit)
	encoder.number(uint64(len(plan.Ordering)), 8)
	for _, ordering := range plan.Ordering {
		if encoder.err != nil {
			break
		}
		encoder.number(uint64(ordering.KeyOrdinal), 4)
		encoder.typeLayout(model.TypeLayout{Type: ordering.Type})
		encoder.number(planFlag(ordering.Descending), 1)
		encoder.number(planFlag(ordering.NullsFirst), 1)
	}
	for _, estimate := range [...]float64{plan.Estimate.Rows, plan.Estimate.ReadBytes, plan.Estimate.DecodeBytes, plan.Estimate.VisibilityChecks, plan.Estimate.IndexProbes} {
		if encoder.err != nil {
			break
		}
		encoder.number(math.Float64bits(estimate), 8)
	}
	encoder.number(uint64(plan.SnapshotTS), 8)
	encoder.bytes(plan.FenceID[:])
	encoder.flush()
	if encoder.err != nil {
		return [32]byte{}, encoder.err
	}
	var result [32]byte
	writer.hash.Sum(result[:0])
	return result, nil
}

func (encoder *mutationEncoder) planEncodedBound(bound EncodedKeyBound) {
	encoder.bytes(bound.Key)
	encoder.number(planFlag(bound.Inclusive), 1)
	encoder.number(planFlag(bound.Prefix), 1)
	encoder.number(planFlag(bound.Unbounded), 1)
}

type planHashWriter struct {
	ctx       context.Context
	hash      hash.Hash
	remaining uint64
}

func (writer *planHashWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if uint64(len(data)) > writer.remaining {
		return 0, model.ErrValueBudget
	}
	writer.remaining -= uint64(len(data))
	return writer.hash.Write(data)
}

func planFlag(value bool) uint64 {
	if value {
		return 1
	}
	return 0
}

func (encoder *mutationEncoder) planColumns(columns []model.ColumnID) {
	encoder.number(uint64(len(columns)), 8)
	for _, column := range columns {
		if encoder.err != nil {
			break
		}
		encoder.number(uint64(column), 4)
	}
}

func (encoder *mutationEncoder) planLimit(limit ScanLimit) {
	encoder.number(planFlag(limit.Enabled), 1)
	encoder.number(limit.Count, 8)
}

func (encoder *mutationEncoder) planBound(bound KeyBound) {
	encoder.number(planFlag(bound.Unbounded), 1)
	encoder.number(planFlag(bound.Inclusive), 1)
	encoder.number(uint64(len(bound.Values)), 8)
	for _, value := range bound.Values {
		if encoder.err != nil {
			break
		}
		encoder.value(value, 0)
	}
}

func (encoder *mutationEncoder) planPredicate(predicate Predicate) {
	encoder.number(uint64(predicate.root), 4)
	encoder.number(uint64(len(predicate.nodes)), 8)
	for _, node := range predicate.nodes {
		if encoder.err != nil {
			break
		}
		encoder.number(uint64(node.Kind), 1)
		encoder.typeLayout(model.TypeLayout{Type: node.Type})
		encoder.number(uint64(node.Operator), 4)
		encoder.number(uint64(node.Collation), 4)
		encoder.number(uint64(node.ColumnID), 4)
		encoder.number(uint64(node.ConstantIndex), 4)
		encoder.number(uint64(node.Left), 4)
		encoder.number(uint64(node.Right), 4)
		encoder.number(uint64(node.FirstArgument), 4)
		encoder.number(uint64(node.ArgumentCount), 4)
		encoder.number(planFlag(node.Negated), 1)
	}
	encoder.number(uint64(len(predicate.edges)), 8)
	for _, edge := range predicate.edges {
		if encoder.err != nil {
			break
		}
		encoder.number(uint64(edge), 4)
	}
	encoder.number(uint64(len(predicate.constants)), 8)
	for _, value := range predicate.constants {
		if encoder.err != nil {
			break
		}
		encoder.value(value, 0)
	}
}
