package physical

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ReadSpan is the complete declared key-space read by a physical scan, including
// ranges whose rows can be removed by residual predicates. It owns its metadata.
type ReadSpan struct {
	Source    storage.ObjectRef
	Target    storage.ObjectRef
	Occurrence model.ScanOccurrence
	KeyLayout []storage.KeyColumn
	Spans     []storage.KeySpan
}

func (node Node) HasReadSpan() bool {
	if node.plan == nil || node.id == 0 || uint64(node.id) > uint64(len(node.plan.nodes)) {
		return false
	}
	return node.plan.nodes[node.id-1].Scan.Request.Target.ObjectID != 0
}

func (node Node) HasLookupReadSpan() bool {
	if node.plan == nil || node.id == 0 || uint64(node.id) > uint64(len(node.plan.nodes)) {
		return false
	}
	return node.plan.nodes[node.id-1].Scan.Lookup.Enabled
}

func (node Node) ReadTargets() (source, target storage.ObjectRef) {
	if !node.HasReadSpan() {
		return storage.ObjectRef{}, storage.ObjectRef{}
	}
	scan := node.plan.nodes[node.id-1].Scan
	return scan.Source, scan.Request.Target
}

func (node Node) CloneReadSpan(ctx context.Context, maximumBytes uint64) (ReadSpan, uint64, error) {
	if err := ctx.Err(); err != nil {
		return ReadSpan{}, 0, err
	}
	if !node.HasReadSpan() {
		return ReadSpan{}, 0, invalid("node has no scan read span")
	}
	scan := node.plan.nodes[node.id-1].Scan.Request
	source := storage.PushdownRequest{Target: scan.Target, Occurrence: scan.Occurrence, KeyLayout: scan.KeyLayout, Spans: scan.Spans}
	owned, bytes, err := storage.CopyPushdownRequest(ctx, source, maximumBytes)
	if err != nil {
		return ReadSpan{}, 0, err
	}
	return ReadSpan{Source: node.plan.nodes[node.id-1].Scan.Source, Target: owned.Target, Occurrence: owned.Occurrence, KeyLayout: owned.KeyLayout, Spans: owned.Spans}, bytes, nil
}

func (node Node) CloneLookupReadSpan(ctx context.Context, maximumBytes uint64) (ReadSpan, uint64, error) {
	if err := ctx.Err(); err != nil {
		return ReadSpan{}, 0, err
	}
	if !node.HasLookupReadSpan() {
		return ReadSpan{}, 0, invalid("node has no lookup read span")
	}
	request := node.plan.nodes[node.id-1].Scan.Lookup.BaseRequest
	source := storage.PushdownRequest{Target: request.Target, Occurrence: request.Occurrence, KeyLayout: request.KeyLayout, Spans: request.Spans}
	owned, bytes, err := storage.CopyPushdownRequest(ctx, source, maximumBytes)
	if err != nil {
		return ReadSpan{}, 0, err
	}
	return ReadSpan{Source: owned.Target, Target: owned.Target, Occurrence: owned.Occurrence, KeyLayout: owned.KeyLayout, Spans: owned.Spans}, bytes, nil
}
