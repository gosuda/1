package exec_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func partitionExchangeGraph(t *testing.T, sql string, partitionCap bool) *fragment.FragmentGraph {
	t.Helper()
	original := sqlGraph(t, sql, nil).Plan()
	spec := physical.Spec{}
	for id := physical.GraphID(1); id <= physical.GraphID(original.GraphCount()); id++ {
		graph, _ := original.Graph(id)
		spec.Graphs = append(spec.Graphs, graph)
	}
	originals := make([]physical.NodeSpec, original.NodeCount()+1)
	var sortID, globalID physical.NodeID
	for id := physical.NodeID(1); id <= physical.NodeID(original.NodeCount()); id++ {
		node, err := original.CloneNode(t.Context(), id, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		originals[id] = node
		if node.Algorithm == physical.Sort && sortID == 0 {
			sortID = id
		}
		limit := node.Algorithm == physical.Limit || node.Algorithm == physical.TopK
		if partitionCap && limit && node.Limit.Scope == physical.GlobalLimit {
			if globalID != 0 || node.Limit.RuntimeBounds || node.Limit.WithTies || len(node.Children) != 1 {
				t.Fatal("partition cap fixture requires one static global limit")
			}
			globalID = id
		}
	}
	if partitionCap {
		if globalID == 0 {
			t.Fatal("partition cap fixture has no global limit")
		}
		sortID = originals[globalID].Children[0]
	}
	if sortID == 0 || originals[sortID].Algorithm != physical.Sort || len(originals[sortID].Delivered.Ordering) == 0 {
		t.Fatal("fixture requires an actual SQL Sort immediately below its global limit")
	}
	ids := make([]physical.NodeID, len(originals))
	var capID physical.NodeID
	for id := physical.NodeID(1); id <= physical.NodeID(original.NodeCount()); id++ {
		node := originals[id]
		for i, child := range node.Children {
			node.Children[i] = ids[child]
		}
		if id == sortID {
			childID := node.Children[0]
			child := spec.Nodes[childID-1]
			key := node.Delivered.Ordering[0].Key
			partitioned := physical.Properties{
				Distribution:  physical.Distribution{Kind: physical.HashPartitioned, Keys: []physical.Key{key}, Partitions: 4, Function: uint64(exchange.XXH3V1), Epoch: 7},
				Rewindability: physical.OnePass, Parameters: child.Delivered.Parameters,
			}
			spec.Nodes = append(spec.Nodes, physical.NodeSpec{Logical: child.Logical, Algorithm: physical.LocalHashRepartition, Children: []physical.NodeID{childID}, Output: child.Output, Required: []physical.Properties{child.Delivered}, Delivered: partitioned, Break: physical.ExchangeBoundary, Memory: physical.Memory{PeakBytes: 64 << 10}, Estimate: child.Estimate, Exchange: physical.ExchangeSpec{Inputs: child.Delivered.Distribution.Partitions, QueueBytes: 64 << 10}})
			node.Children[0] = physical.NodeID(len(spec.Nodes))
			node.Required = []physical.Properties{partitioned}
			node.Delivered.Distribution = partitioned.Distribution
			node.Delivered.Rewindability = physical.OnePass
		}
		if id == globalID {
			node.Algorithm = physical.TopK
			node.Break = physical.MaterializeInput
			node.Memory.PeakBytes = max(node.Memory.PeakBytes, 64<<10)
			node.Delivered.Ordering = spec.Nodes[node.Children[0]-1].Delivered.Ordering
			node.Required = []physical.Properties{spec.Nodes[node.Children[0]-1].Delivered}
		}
		spec.Nodes = append(spec.Nodes, node)
		ids[id] = physical.NodeID(len(spec.Nodes))
		if id != sortID {
			continue
		}
		if partitionCap {
			global := originals[globalID].Limit
			// The cap is an enforcer of the real Sort relation, not a replacement SQL operator.
			cap := physical.NodeSpec{Logical: node.Logical, Algorithm: physical.TopK, Children: []physical.NodeID{ids[id]}, Output: node.Output, Required: []physical.Properties{node.Delivered}, Delivered: node.Delivered, Break: physical.MaterializeInput, Memory: node.Memory, Estimate: node.Estimate, Limit: physical.LimitSpec{Scope: physical.PartitionLimit, Count: global.Offset + global.Count}}
			spec.Nodes = append(spec.Nodes, cap)
			capID = physical.NodeID(len(spec.Nodes))
			ids[id] = capID
		}
		input := spec.Nodes[ids[id]-1]
		merged := input.Delivered
		merged.Distribution = physical.Distribution{Kind: physical.SingleNode, Partitions: 1}
		spec.Nodes = append(spec.Nodes, physical.NodeSpec{Logical: input.Logical, Algorithm: physical.LocalOrderedMerge, Children: []physical.NodeID{ids[id]}, Output: input.Output, Required: []physical.Properties{input.Delivered}, Delivered: merged, Break: physical.ExchangeBoundary, Memory: physical.Memory{PeakBytes: 64 << 10}, Estimate: input.Estimate, Exchange: physical.ExchangeSpec{Inputs: input.Delivered.Distribution.Partitions, QueueBytes: 64 << 10}})
		ids[id] = physical.NodeID(len(spec.Nodes))
	}
	if partitionCap {
		spec.Nodes[capID-1].Limit.Global = ids[globalID]
	}
	spec.Root = ids[original.Root()]
	plan, err := physical.NewPlan(t.Context(), spec, physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var hash, sort, merge, caps int
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, _ := plan.Node(id)
		kind, _, _, _ := node.Delivered().Distribution()
		switch node.Metadata().Algorithm {
		case physical.LocalHashRepartition:
			hash++
		case physical.Sort:
			if kind == physical.HashPartitioned {
				sort++
			}
		case physical.LocalOrderedMerge:
			merge++
		case physical.TopK:
			if node.Metadata().Limit.Scope == physical.PartitionLimit {
				caps++
			}
		}
	}
	if hash != 1 || sort != 1 || merge != 1 || (caps == 1) != partitionCap {
		t.Fatalf("fixture bypasses partitioned ordering: hash=%d sort=%d merge=%d caps=%d", hash, sort, merge, caps)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestPhysicalPartitionSortGlobalNullOrdering(t *testing.T) {
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	nulls := [][]model.Value{{null, i(900)}, {null, i(910)}}
	ascending := [][]model.Value{{i(1), i(100)}, {i(2), i(200)}, {i(2), i(200)}, {i(2), i(210)}, {i(3), i(300)}}
	descending := [][]model.Value{{i(3), i(300)}, {i(2), i(200)}, {i(2), i(200)}, {i(2), i(210)}, {i(1), i(100)}}
	for _, scenario := range []struct {
		order string
		want  [][]model.Value
	}{
		{"ASC NULLS FIRST", append(append([][]model.Value{}, nulls...), ascending...)},
		{"ASC NULLS LAST", append(append([][]model.Value{}, ascending...), nulls...)},
		{"DESC NULLS FIRST", append(append([][]model.Value{}, nulls...), descending...)},
		{"DESC NULLS LAST", append(append([][]model.Value{}, descending...), nulls...)},
	} {
		t.Run(scenario.order, func(t *testing.T) {
			graph := partitionExchangeGraph(t, `SELECT x, v * 10 FROM (VALUES (2,20),(NULL,90),(1,10),(2,21),(NULL,91),(0,-1),(3,30),(2,20)) t(x,v) WHERE v > 0 ORDER BY x `+scenario.order+`, v`, false)
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("ordered rows=%v, want %v", got, scenario.want)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestPhysicalPartitionTopKPreservesGlobalOffset(t *testing.T) {
	for _, cap := range []bool{false, true} {
		t.Run(fmt.Sprintf("partition cap=%v", cap), func(t *testing.T) {
			graph := partitionExchangeGraph(t, `SELECT x, x * 10 FROM (VALUES `+partitionExchangeValues(96)+`) t(x) WHERE x > 0 ORDER BY x DESC LIMIT 4 OFFSET 3`, cap)
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var want [][]model.Value
			for _, value := range []int64{92, 91, 90, 89} {
				want = append(want, []model.Value{model.Int64Val(value), model.Int64Val(value * 10)})
			}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("global offset rows=%v, want %v", got, want)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func partitionExchangeValues(count int) string {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("(%d)", count-i-1)
	}
	return strings.Join(values, ",")
}

func TestPhysicalPartitionSortSpillSurvivesCallerReset(t *testing.T) {
	const count = 96
	graph := partitionExchangeGraph(t, `SELECT x, x * 10 FROM (VALUES `+partitionExchangeValues(count)+`) t(x) ORDER BY x`, false)
	env, shared, dir := mergeSQLEnvironment(t, true)
	env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
	env.Sort.RunBytes = 1024
	env.Limits.BatchRows, env.Limits.Expressions.MaxRows = 17, 17
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	batch := columnar.Batch{MaxRows: 1}
	for value := int64(0); value < count; value++ {
		if err := portal.Next(t.Context(), &batch); err != nil {
			t.Fatalf("row %d: %v", value, err)
		}
		if batch.Rows() != 1 || len(batch.Columns) != 2 {
			t.Fatalf("row %d: rows=%d columns=%d", value, batch.Rows(), len(batch.Columns))
		}
		lane := batch.Lane(0)
		if got, payload := batch.Columns[0].Int64[lane], batch.Columns[1].Int64[lane]; got != value || payload != value*10 {
			t.Fatalf("row %d: (%d,%d), want (%d,%d)", value, got, payload, value, value*10)
		}
		batch.Reset()
	}
	if err := portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("sorted stream termination: %v", err)
	}
	if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles == 0 {
		t.Fatalf("bounded sort did not spill: %+v", usage)
	}
	mergeSQLReleased(t, env, shared, dir)
}

func TestPhysicalPartitionSortTerminationReleasesSpill(t *testing.T) {
	for _, termination := range []string{"close", "cancel"} {
		t.Run(termination, func(t *testing.T) {
			graph := partitionExchangeGraph(t, `SELECT x FROM (VALUES `+partitionExchangeValues(96)+`) t(x) ORDER BY x`, false)
			env, shared, dir := mergeSQLEnvironment(t, true)
			env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
			env.Sort.RunBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); err != nil || batch.Rows() != 1 || batch.Columns[0].Int64[batch.Lane(0)] != 0 {
				t.Fatalf("first ordered row: batch=%+v error=%v", batch, err)
			}
			if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles == 0 {
				t.Fatalf("bounded sort did not spill before termination: %+v", usage)
			}
			batch.Reset()
			if termination == "cancel" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("caller cancellation: %v", err)
				}
			} else {
				closePortal(t, portal)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestPhysicalPartitionSortLateQuotaFailureReleasesSpill(t *testing.T) {
	for _, boundary := range []string{"source", "sort record"} {
		t.Run(boundary, func(t *testing.T) {
			payload := 512
			if boundary == "source" {
				payload = 6000
			}
			values := make([]string, 96)
			for i := range values {
				values[i] = fmt.Sprintf("(%d,'ok'::text)", i)
			}
			values[len(values)-1] = fmt.Sprintf("(95,'%s'::text)", strings.Repeat("x", payload))
			graph := partitionExchangeGraph(t, `SELECT x, v FROM (VALUES `+strings.Join(values, ",")+`) t(x,v) ORDER BY x`, false)
			env, shared, dir := mergeSQLEnvironment(t, true)
			env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
			env.Sort.RunBytes = 1024
			if boundary == "source" {
				env.Sort.MaxRecordBytes = 8192
				env.Limits.Expressions.MaxOutputBytes = 4096
			} else {
				env.Sort.MaxRecordBytes = 256
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatalf("quota failed before reading source: %v", err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			err = portal.Next(t.Context(), &batch)
			if !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("late %s quota error=%v", boundary, err)
			}
			if batch.Rows() != 0 {
				t.Fatalf("failed blocking sort published %d rows", batch.Rows())
			}
			if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles == 0 {
				t.Fatalf("%s quota failed before sort spilled: %+v", boundary, usage)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestPhysicalZeroPartitionCapDoesNotEvaluateSource(t *testing.T) {
	graph := partitionExchangeGraph(t, `SELECT x FROM (VALUES (1/$1),(2)) t(x) ORDER BY x LIMIT 0`, true)
	env, shared, dir := mergeSQLEnvironment(t, true)
	env.Exchange = exchange.HashOptions{MaxKeyBytes: 4096, WorkBytes: 16 << 20}
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}, Value: model.Int64Val(0)}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	if err := portal.Next(t.Context(), &columnar.Batch{MaxRows: 1}); err != io.EOF {
		t.Fatalf("zero cap evaluated source: %v", err)
	}
	if env.Query.Usage().Spill.Peak != 0 {
		t.Fatal("zero cap sorted the input")
	}
	mergeSQLReleased(t, env, shared, dir)
}
