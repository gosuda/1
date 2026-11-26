package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestSQLSortWithTiesRetainsCompletePeerGroup(t *testing.T) {
	graph := sqlGraph(t, "SELECT x FROM (VALUES (3), (1), (2), (2), (2)) AS v(x) ORDER BY x FETCH FIRST 2 ROWS WITH TIES", nil)
	env, _ := environment(t)
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env.Sort = exsort.Options{RunBytes: 1 << 20, MaxRecordBytes: 64 << 10, MaxRuns: 4, MergeFanIn: 2, WorkBytes: 32 << 20}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(2)}, {model.Int64Val(2)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WITH TIES rows = %v, want %v", got, want)
	}
}

func TestMaterializeReadsSealedRowsAcrossBatches(t *testing.T) {
	original := sqlGraph(t, "SELECT x FROM (VALUES (7), (9), (11)) AS v(x)", nil).Plan()
	spec := physical.Spec{Root: physical.NodeID(original.NodeCount() + 1)}
	for id := physical.GraphID(1); id <= physical.GraphID(original.GraphCount()); id++ {
		graph, _ := original.Graph(id)
		spec.Graphs = append(spec.Graphs, graph)
	}
	for id := physical.NodeID(1); id <= physical.NodeID(original.NodeCount()); id++ {
		node, err := original.CloneNode(t.Context(), id, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		spec.Nodes = append(spec.Nodes, node)
	}
	root := spec.Nodes[original.Root()-1]
	delivered := root.Delivered
	delivered.Rewindability = physical.Rewindable
	spec.Nodes = append(spec.Nodes, physical.NodeSpec{Logical: root.Logical, Algorithm: physical.Materialize, Children: []physical.NodeID{original.Root()}, Output: root.Output, Required: []physical.Properties{root.Delivered}, Delivered: delivered, Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 1 << 20, Spillable: true}})
	plan, err := physical.NewPlan(t.Context(), spec, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	env, shared := environment(t)
	env.Spool = rowstore.Limits{MemoryBytes: 1 << 20, MaxBytes: 4 << 20, MaxRows: 1000, MaxRecordBytes: 64 << 10, MetadataBytes: 64 << 10, DecodeBytes: 64 << 10, MaxReaders: 2}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(7)}, {model.Int64Val(9)}, {model.Int64Val(11)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("materialized rows = %v", got)
	}
	closePortal(t, portal)
	if shared.Usage().Live != 0 {
		t.Fatal("materialization retained resources")
	}
}

type blockedReader struct {
	storage.SnapshotReader
	entered chan struct{}
	closed  chan struct{}
}

func (r *blockedReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	cursor, err := r.SnapshotReader.VectorizedScan(ctx, plan)
	if err != nil {
		return nil, err
	}
	return &blockedCursor{BatchCursor: cursor, reader: r}, nil
}

type blockedCursor struct {
	storage.BatchCursor
	reader *blockedReader
	once   bool
}

func (c *blockedCursor) Next(ctx context.Context, out *columnar.Batch) error {
	close(c.reader.entered)
	<-ctx.Done()
	return ctx.Err()
}
func (c *blockedCursor) Close() error {
	err := c.BatchCursor.Close()
	if !c.once {
		c.once = true
		close(c.reader.closed)
	}
	return err
}
func TestCloseCancelsAndJoinsRunningScan(t *testing.T) {
	real, request := realScan(t)
	reader := &blockedReader{SnapshotReader: real, entered: make(chan struct{}), closed: make(chan struct{})}
	graph := scanGraph(t, reader, request, false, false)
	env, shared := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { var batch columnar.Batch; result <- portal.Next(t.Context(), &batch) }()
	<-reader.entered
	closeErr := portal.Close()
	if closeErr != nil && !errors.Is(closeErr, context.Canceled) {
		t.Fatal(closeErr)
	}
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight Next returned %v", err)
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("Close returned before scan cleanup")
	}
	if shared.Usage().Live != 0 {
		t.Fatal("racing Close retained query resources")
	}
}
