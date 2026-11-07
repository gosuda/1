package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func hashExchangeGraph(t *testing.T, sql string, filter bool, function uint64) *fragment.FragmentGraph {
	t.Helper()
	original := sqlGraph(t, sql, nil).Plan()
	spec := physical.Spec{}
	for id := physical.GraphID(1); id <= physical.GraphID(original.GraphCount()); id++ {
		graph, _ := original.Graph(id)
		spec.Graphs = append(spec.Graphs, graph)
	}
	ids := make([]physical.NodeID, original.NodeCount()+1)
	appendHash := func(input physical.NodeID) physical.NodeID {
		child := spec.Nodes[input-1]
		if len(child.Output) == 0 {
			t.Fatal("hash fixture has no key")
		}
		slot := child.Output[0]
		graph, _ := original.Graph(slot.Graph)
		typ, _ := graph.Type(slot.Layout)
		properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.HashPartitioned, Keys: []physical.Key{{Slot: slot, Type: typ}}, Partitions: 4, Function: function, Epoch: 7}, Rewindability: physical.OnePass, Parameters: child.Delivered.Parameters}
		spec.Nodes = append(spec.Nodes, physical.NodeSpec{Logical: child.Logical, Algorithm: physical.LocalHashRepartition, Children: []physical.NodeID{input}, Output: child.Output, Required: []physical.Properties{child.Delivered}, Delivered: properties, Break: physical.ExchangeBoundary, Memory: physical.Memory{PeakBytes: 64 << 10}, Estimate: child.Estimate, Exchange: physical.ExchangeSpec{Inputs: child.Delivered.Distribution.Partitions, QueueBytes: 64 << 10}})
		return physical.NodeID(len(spec.Nodes))
	}
	appendGather := func(input physical.NodeID) physical.NodeID {
		child := spec.Nodes[input-1]
		properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass, Parameters: child.Delivered.Parameters}
		spec.Nodes = append(spec.Nodes, physical.NodeSpec{Logical: child.Logical, Algorithm: physical.LocalGather, Children: []physical.NodeID{input}, Output: child.Output, Required: []physical.Properties{child.Delivered}, Delivered: properties, Break: physical.ExchangeBoundary, Memory: physical.Memory{PeakBytes: 64 << 10}, Estimate: child.Estimate, Exchange: physical.ExchangeSpec{Inputs: child.Delivered.Distribution.Partitions, QueueBytes: 64 << 10}})
		return physical.NodeID(len(spec.Nodes))
	}
	inserted := false
	for id := physical.NodeID(1); id <= physical.NodeID(original.NodeCount()); id++ {
		node, err := original.CloneNode(t.Context(), id, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		for i, child := range node.Children {
			node.Children[i] = ids[child]
		}
		wrap := filter && node.Algorithm == physical.Filter && !inserted
		if wrap {
			hash := appendHash(node.Children[0])
			node.Children[0] = hash
			node.Required = []physical.Properties{spec.Nodes[hash-1].Delivered}
			node.Delivered = spec.Nodes[hash-1].Delivered
		}
		spec.Nodes = append(spec.Nodes, node)
		ids[id] = physical.NodeID(len(spec.Nodes))
		if wrap {
			ids[id] = appendGather(ids[id])
			inserted = true
		}
	}
	spec.Root = ids[original.Root()]
	if !filter {
		spec.Root = appendGather(appendHash(spec.Root))
		inserted = true
	}
	if !inserted {
		t.Fatal("SQL fixture had no filter to partition")
	}
	plan, err := physical.NewPlan(t.Context(), spec, physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestPhysicalHashGatherPreservesFilteredBag(t *testing.T) {
	graph := hashExchangeGraph(t, "SELECT x, x * 10 FROM (VALUES (0),(1),(2),(3),(4),(5),(6),(7),(2),(NULL)) v(x) WHERE x > 1", true, uint64(exchange.XXH3V1))
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 4 << 20}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var want [][]model.Value
	for _, value := range []int64{2, 3, 4, 5, 6, 7, 2} {
		want = append(want, []model.Value{model.Int64Val(value), model.Int64Val(value * 10)})
	}
	mergeSQLBag(t, rows(t, portal), want)
	closePortal(t, portal)
	mergeSQLReleased(t, env, shared, dir)
}

func TestPhysicalGatherPrecedesGlobalSortAndLimit(t *testing.T) {
	graph := hashExchangeGraph(t, "SELECT x FROM (VALUES (4),(2),(7),(2),(3),(0)) v(x) WHERE x > 1 ORDER BY x LIMIT 3", true, uint64(exchange.XXH3V1))
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 4 << 20}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	want := [][]model.Value{{model.Int64Val(2)}, {model.Int64Val(2)}, {model.Int64Val(3)}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("global ordering/limit: %v, want %v", got, want)
	}
	closePortal(t, portal)
	mergeSQLReleased(t, env, shared, dir)
}

func TestPhysicalHashGatherEarlyCloseAndCancellation(t *testing.T) {
	for _, cancelNext := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelNext], func(t *testing.T) {
			graph := hashExchangeGraph(t, "VALUES (1),(2),(3),(4),(5)", false, uint64(exchange.XXH3V1))
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 4 << 20}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			out := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &out); err != nil || out.Rows() != 1 {
				t.Fatalf("first batch: rows=%d error=%v", out.Rows(), err)
			}
			if cancelNext {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled exchange: %v", err)
				}
			}
			closePortal(t, portal)
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestPhysicalHashMappingRejectionOwnsInvocation(t *testing.T) {
	graph := hashExchangeGraph(t, "VALUES (1),(2)", false, 99)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 4 << 20}
	portal, err := exec.Open(t.Context(), graph, env)
	if portal != nil {
		err = errors.Join(err, portal.Close())
	}
	if !errors.Is(err, runtime.ErrInvalidConfig) {
		t.Fatalf("unknown mapping: %v", err)
	}
	mergeSQLReleased(t, env, shared, dir)
}
