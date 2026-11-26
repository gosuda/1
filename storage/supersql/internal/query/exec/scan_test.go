package exec_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type observedReader struct {
	storage.SnapshotReader
	opened       int
	cursorClosed bool
	readerClosed bool
}

func (r *observedReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	cursor, err := r.SnapshotReader.VectorizedScan(ctx, plan)
	if err != nil {
		return nil, err
	}
	r.opened++
	return &observedCursor{BatchCursor: cursor, owner: r}, nil
}
func (r *observedReader) Close() error { r.readerClosed = true; return r.SnapshotReader.Close() }

type observedCursor struct {
	storage.BatchCursor
	owner *observedReader
}

func (c *observedCursor) Close() error { c.owner.cursorClosed = true; return c.BatchCursor.Close() }

func realScan(t *testing.T) (*observedReader, storage.PushdownRequest) {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}
	epoch := clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	limits := neosepia.DefaultLimits()
	limits.BlockRows = 4
	backend, err := neosepia.Open(t.Context(), neosepia.Config{Directory: t.TempDir(), Scale: scale, KeyEncoder: encoder, FenceVerifier: authority, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	layout := []storage.ColumnLayout{{ID: 1, Type: typ}}
	keys := []storage.KeyColumn{{ColumnID: 1, Type: typ}}
	batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, Epoch: epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: layout}}}
	for i := int64(1); i <= 4; i++ {
		values := []model.Value{model.Int64Val(i)}
		key, err := encoder.EncodePrefix(t.Context(), keys, values, 100)
		if err != nil {
			t.Fatal(err)
		}
		mutation := storage.NewUpsertMutation(target, key, []model.ColumnID{1}, values)
		mutation.Ordinal = uint64(i)
		batch.Mutations = append(batch.Mutations, mutation)
	}
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	state, err := backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), 20, state.LogPosition, epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 20, Fence: fence})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	predicate, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, ColumnID: 1, Type: typ},
		{Kind: storage.PredicateConstant, ConstantIndex: 0, Type: typ},
		{Kind: storage.PredicateComparison, Operator: 413, Left: 0, Right: 1, Type: model.Type{ID: 16, Storage: model.TypeBoolean}},
	}, nil, []model.Value{model.Int64Val(2)})
	if err != nil {
		t.Fatal(err)
	}
	request := storage.PushdownRequest{Target: target, Layout: layout, KeyLayout: keys, ColumnIDs: []model.ColumnID{1}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}, Predicate: predicate}
	return &observedReader{SnapshotReader: reader}, request
}

func scanGraph(t *testing.T, reader storage.SnapshotReader, request storage.PushdownRequest, project, limit bool) *fragment.FragmentGraph {
	t.Helper()
	b, err := ir.NewBuilder(irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: request.Layout[0].Type})
	if err != nil {
		t.Fatal(err)
	}
	slot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Object: request.Target.ObjectID, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: request.Target.ObjectID, Generation: 1, DescriptorVersion: 1}, Outputs: []model.SlotID{slot}})
	if err != nil {
		t.Fatal(err)
	}
	root := scan
	output := slot
	var projection ir.ScalarID
	if project {
		output, err = b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout})
		if err != nil {
			t.Fatal(err)
		}
		projection, err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: layout, Slot: slot}})
		if err != nil {
			t.Fatal(err)
		}
		projection, err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: scope, Layout: layout}, Operands: []ir.ScalarID{projection}})
		if err != nil {
			t.Fatal(err)
		}
		root, err = b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scope}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{output}, Bindings: []ir.Binding{{Slot: output, Expr: projection}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	var count ir.ScalarID
	if limit {
		count, err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: layout}, Value: model.Int64Val(1)})
		if err != nil {
			t.Fatal(err)
		}
		root, err = b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: scope, Count: count, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{root}, Outputs: []model.SlotID{output}})
		if err != nil {
			t.Fatal(err)
		}
	}
	graph, err := b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	negotiated, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
	nodes := []physical.NodeSpec{{Logical: physical.RelRef{Graph: 1, Rel: scan}, Algorithm: physical.TableScan, Output: []physical.Slot{{Graph: 1, ID: slot, Layout: layout}}, Delivered: properties, Break: physical.Streaming, Scan: physical.ScanSpec{Source: request.Target, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, CapabilityEpoch: 1, Request: request, Accepted: negotiated.Accepted, Residual: negotiated.Residual, Projection: negotiated.Projection, Ordering: negotiated.Ordering, Estimate: negotiated.Estimate}}}
	if project {
		projectRel := root
		if limit {
			projectRel, _ = graph.Input(root, 0)
		}
		nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: projectRel}, Algorithm: physical.Project, Children: []physical.NodeID{1}, Output: []physical.Slot{{Graph: 1, ID: output, Layout: layout}}, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: projection}}, Required: []physical.Properties{properties}, Delivered: properties, Break: physical.Streaming})
	}
	if limit {
		nodes = append(nodes, physical.NodeSpec{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.Limit, Children: []physical.NodeID{physical.NodeID(len(nodes))}, Output: []physical.Slot{{Graph: 1, ID: output, Layout: layout}}, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: count}}, Required: []physical.Properties{properties}, Delivered: properties, Break: physical.Streaming, Limit: physical.LimitSpec{Scope: physical.GlobalLimit, Count: 1}})
	}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{graph}, Nodes: nodes, Root: physical.NodeID(len(nodes))}, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}

func TestRealScanReturnsSelectedRowsAcrossEmptyBatches(t *testing.T) {
	reader, request := realScan(t)
	graph := scanGraph(t, reader, request, false, false)
	env, _ := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(3)}, {model.Int64Val(4)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected scan rows = %v", got)
	}
}
func TestRealScanCompactsOnlySelectedRowsForExpressionCap(t *testing.T) {
	reader, request := realScan(t)
	graph := scanGraph(t, reader, request, true, false)
	env, _ := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(3)}, {model.Int64Val(4)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected projection = %v", got)
	}
}
func TestLimitClosesRealScanBeforeReturningFinalPrefix(t *testing.T) {
	reader, request := realScan(t)
	graph := scanGraph(t, reader, request, false, true)
	env, _ := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	for batch.Rows() == 0 {
		if err = portal.Next(t.Context(), &batch); err != nil {
			t.Fatal(err)
		}
	}
	if !reader.cursorClosed || reader.readerClosed {
		t.Fatalf("limit cursor=%v snapshot=%v", reader.cursorClosed, reader.readerClosed)
	}
	value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(0), 100)
	if err != nil || value.IntVal != 3 {
		t.Fatalf("final retained prefix = %v, %v", value, err)
	}
	if err = portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("limit EOF = %v", err)
	}
	closePortal(t, portal)
	if !reader.readerClosed {
		t.Fatal("portal did not close its snapshot")
	}
}

func TestOpenFailureOwnsRealSnapshot(t *testing.T) {
	reader, request := realScan(t)
	graph := scanGraph(t, reader, request, false, false)
	env, _ := environment(t)
	env.Reader = reader
	env.Limits.BatchRows = 0
	_, err := exec.Open(t.Context(), graph, env)
	if !errors.Is(err, exec.ErrInvalid) || !reader.readerClosed || reader.opened != 0 {
		t.Fatalf("failed Open: error=%v reader closed=%v scans=%d", err, reader.readerClosed, reader.opened)
	}
}
