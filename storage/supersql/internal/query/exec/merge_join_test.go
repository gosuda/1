package exec_test

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func mergeSQLGraph(t *testing.T, sql string) *fragment.FragmentGraph {
	t.Helper()
	original := sqlGraph(t, sql, nil).Plan()
	spec := physical.Spec{Root: original.Root()}
	for id := physical.GraphID(1); id <= physical.GraphID(original.GraphCount()); id++ {
		graph, _ := original.Graph(id)
		spec.Graphs = append(spec.Graphs, graph)
	}
	merges := 0
	for id := physical.NodeID(1); id <= physical.NodeID(original.NodeCount()); id++ {
		node, err := original.CloneNode(t.Context(), id, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		graph, _ := original.Graph(node.Logical.Graph)
		logical, _ := graph.Rel(node.Logical.Rel)
		if logical.Kind == ir.Join && len(node.Children) == 2 && logical.JoinKind != ir.MarkJoin && logical.JoinKind != ir.CrossJoin {
			left, right := spec.Nodes[node.Children[0]-1], spec.Nodes[node.Children[1]-1]
			keys := mergeSQLKeys(t, graph, node.Logical.Graph, logical.Predicate, left, right)
			if len(keys) == 0 {
				t.Fatalf("join %d has no merge-compatible equality conjunct", id)
			}
			if len(left.Delivered.Ordering) < len(keys) || len(right.Delivered.Ordering) < len(keys) {
				t.Fatalf("join %d SQL inputs do not deliver complete key ordering: left=%v right=%v keys=%v", id, left.Delivered.Ordering, right.Delivered.Ordering, keys)
			}
			for i, key := range keys {
				l, r := left.Delivered.Ordering[i], right.Delivered.Ordering[i]
				if l.Key != key.Left || r.Key != key.Right || l.Direction != r.Direction || l.Nulls != r.Nulls || l.Operator != r.Operator {
					t.Fatalf("join %d key %d lacks matching SQL input order: left=%+v right=%+v key=%+v", id, i, l, r, key)
				}
			}
			node.Algorithm, node.Break = physical.MergeJoin, physical.Streaming
			node.Join.Keys = keys
			node.Required = []physical.Properties{left.Delivered, right.Delivered}
			for side := range node.Required {
				node.Required[side].Ordering = slices.Clone(node.Required[side].Ordering[:len(keys)])
				node.Required[side].Rewindability = physical.OnePass
			}
			merges++
		}
		spec.Nodes = append(spec.Nodes, node)
	}
	if merges != 1 {
		t.Fatalf("forced %d merge joins, want one", merges)
	}
	plan, err := physical.NewPlan(t.Context(), spec, physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	merges = 0
	for id := physical.NodeID(1); id <= physical.NodeID(graph.Plan().NodeCount()); id++ {
		node, _ := graph.Plan().Node(id)
		if node.Metadata().Algorithm == physical.MergeJoin {
			merges++
		}
	}
	if merges != 1 {
		t.Fatalf("compiled fragment contains %d merge joins, want one", merges)
	}
	return graph
}

func mergeSQLKeys(t *testing.T, graph *ir.Graph, graphID physical.GraphID, predicate ir.ScalarID, left, right physical.NodeSpec) []physical.JoinKey {
	t.Helper()
	var keys []physical.JoinKey
	var visit func(ir.ScalarID, int)
	visit = func(id ir.ScalarID, depth int) {
		if depth >= 100 {
			t.Fatal("join predicate exceeds fixture depth")
		}
		scalar, _ := graph.Scalar(id)
		if scalar.Kind == ir.Bool && scalar.BoolOp == ir.And {
			for i := range scalar.OperandCount() {
				child, _ := graph.Operand(id, i)
				visit(child, depth+1)
			}
			return
		}
		if scalar.Kind != ir.Call || scalar.OperandCount() != 2 || !scalar.Strict {
			return
		}
		operator, ok := builtin.LookupOperator(scalar.Operator)
		if !ok || !operator.MergeJoinable {
			return
		}
		lhs, _ := graph.Operand(id, 0)
		rhs, _ := graph.Operand(id, 1)
		l, _ := graph.Scalar(lhs)
		r, _ := graph.Scalar(rhs)
		if l.Kind != ir.Slot || r.Kind != ir.Slot {
			return
		}
		li := slices.IndexFunc(left.Output, func(slot physical.Slot) bool { return slot.Graph == graphID && slot.ID == l.Slot })
		ri := slices.IndexFunc(right.Output, func(slot physical.Slot) bool { return slot.Graph == graphID && slot.ID == r.Slot })
		if li < 0 || ri < 0 {
			return
		}
		lt, _ := graph.Type(left.Output[li].Layout)
		rt, _ := graph.Type(right.Output[ri].Layout)
		if lt != rt {
			return
		}
		keys = append(keys, physical.JoinKey{
			Left:     physical.Key{Slot: left.Output[li], Type: lt, Collation: lt.Collation},
			Right:    physical.Key{Slot: right.Output[ri], Type: rt, Collation: rt.Collation},
			Equality: scalar.Operator, Nulls: physical.NullsUnequal,
			Predicate: physical.ScalarRef{Graph: graphID, Scalar: id},
		})
	}
	visit(predicate, 0)
	return keys
}

func mergeSQLEnvironment(t *testing.T, spill bool) (exec.Environment, *runtime.Budget, string) {
	t.Helper()
	dir := t.TempDir()
	env, shared := environmentWithQuery(t, runtime.QueryConfig{WorkBytes: 96 << 20, OutputBytes: 32 << 20, SpillBytes: 32 << 20, SpillMode: runtime.SpillEnabled, Directory: dir, MaxFiles: 16, MaxReaders: 8, MaxReservations: 10000})
	var err error
	env.Keys, err = builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	env.Join = exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 4}
	if spill {
		env.Join.MemoryBytes = 0
	}
	env.Sort = exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2}
	env.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 1024, MaxRecordBytes: 4096, MetadataBytes: 64 << 10, DecodeBytes: 64 << 10, MaxReaders: 8}
	env.Apply = apply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10}
	return env, shared, dir
}

func mergeSQLReleased(t *testing.T, env exec.Environment, shared *runtime.Budget, dir string) {
	t.Helper()
	usage := env.Query.Usage()
	for name, account := range map[string]runtime.Usage{"work": usage.Work, "output": usage.Output, "spill": usage.Spill, "shared": shared.Usage()} {
		if account.Live != 0 || account.Reservations != 0 {
			t.Errorf("%s survived join termination: %+v", name, account)
		}
	}
	if usage.Files != 0 || usage.Readers != 0 {
		t.Errorf("join retained files or readers: %+v", usage)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Errorf("spill cleanup: files=%v error=%v", files, err)
	}
}

func mergeSQLBag(t *testing.T, got, want [][]model.Value) {
	t.Helper()
	remaining := slices.Clone(want)
	for _, row := range got {
		i := slices.IndexFunc(remaining, func(expected []model.Value) bool { return reflect.DeepEqual(row, expected) })
		if i < 0 {
			t.Fatalf("unexpected join row %v; got bag %v, want %v", row, got, want)
		}
		remaining = slices.Delete(remaining, i, i+1)
	}
	if len(remaining) != 0 {
		t.Fatalf("missing join rows %v; got bag %v, want %v", remaining, got, want)
	}
}

func TestSQLMergeJoinDuplicateResidualBags(t *testing.T) {
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	matches := [][]model.Value{{i(10), i(20)}, {i(10), i(21)}, {i(10), i(22)}, {i(11), i(20)}, {i(11), i(21)}}
	leftOnly := [][]model.Value{{i(12), null}, {i(13), null}, {i(14), null}}
	rightOnly := [][]model.Value{{null, i(23)}, {null, i(24)}, {null, i(25)}}
	for _, scenario := range []struct {
		kind string
		want [][]model.Value
	}{
		{"INNER", matches},
		{"LEFT", slices.Concat(matches, leftOnly)},
		{"RIGHT", slices.Concat(matches, rightOnly)},
		{"FULL", slices.Concat(matches, leftOnly, rightOnly)},
	} {
		for _, spill := range []bool{false, true} {
			mode := "resident"
			if spill {
				mode = "spill"
			}
			t.Run(scenario.kind+"/"+mode, func(t *testing.T) {
				graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM
					(SELECT * FROM (VALUES(1,10),(1,11),(2,12),(NULL,13),(4,14)) l(k,v) ORDER BY k) a `+scenario.kind+` JOIN
					(SELECT * FROM (VALUES(1,20),(1,21),(1,22),(3,23),(NULL,24),(4,25)) r(k,v) ORDER BY k) b
					ON a.k=b.k AND a.v+b.v<33`)
				env, shared, dir := mergeSQLEnvironment(t, spill)
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				mergeSQLBag(t, rows(t, portal), scenario.want)
				usage := env.Query.Usage()
				if spilled := usage.Spill.Peak > 0 && usage.PeakFiles > 0; spilled != spill {
					t.Errorf("spill=%v, want %v: %+v", spilled, spill, usage)
				}
				mergeSQLReleased(t, env, shared, dir)
			})
		}
	}
}

func TestSQLMergeJoinTypedKeys(t *testing.T) {
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	for _, scenario := range []struct {
		name, left, right, order, on string
		want                         [][]model.Value
	}{
		{"descending nulls first", `(NULL,1),(1,2),(2,3)`, `(2,20),(NULL,30),(1,10)`, `k DESC NULLS FIRST`, `a.k=b.k`, [][]model.Value{{i(1), null}, {i(2), i(10)}, {i(3), i(20)}, {null, i(30)}}},
		{"descending nulls last", `(NULL,1),(1,2),(2,3)`, `(2,20),(NULL,30),(1,10)`, `k DESC NULLS LAST`, `a.k=b.k`, [][]model.Value{{i(1), null}, {i(2), i(10)}, {i(3), i(20)}, {null, i(30)}}},
		{"text", `('z'::text,1),('aa',2),('a',3),('aa',4)`, `('a'::text,10),('aa',20),('b',30)`, `k`, `a.k=b.k`, [][]model.Value{{i(1), null}, {i(2), i(20)}, {i(3), i(10)}, {i(4), i(20)}, {null, i(30)}}},
		{"numeric scale and exactness", `('0.1'::numeric,1),('0.1000',2),('0.1000000000000000000000000000000001',3),('NaN',4)`, `('0.10'::numeric,10),('NaN',20)`, `k`, `a.k=b.k`, [][]model.Value{{i(1), i(10)}, {i(2), i(10)}, {i(3), null}, {i(4), i(20)}}},
		{"float nan and signed zero", `('NaN'::float8,1),('-0',2),('Infinity',3),('NaN',4)`, `('0'::float8,10),('NaN',20),('-Infinity',30)`, `k DESC NULLS LAST`, `a.k=b.k`, [][]model.Value{{i(1), i(20)}, {i(2), i(10)}, {i(3), null}, {i(4), i(20)}, {null, i(30)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM (SELECT * FROM (VALUES`+scenario.left+`) l(k,v) ORDER BY `+scenario.order+`) a FULL JOIN (SELECT * FROM (VALUES`+scenario.right+`) r(k,v) ORDER BY `+scenario.order+`) b ON `+scenario.on)
			env, shared, dir := mergeSQLEnvironment(t, true)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			mergeSQLBag(t, rows(t, portal), scenario.want)
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestSQLMergeJoinCompositeKeys(t *testing.T) {
	graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM
		(SELECT * FROM (VALUES(1,'a'::text,10),(1,'b',11),(2,'a',12),(2,NULL,13)) l(k,s,v) ORDER BY k DESC,s NULLS FIRST) a FULL JOIN
		(SELECT * FROM (VALUES(1,'a'::text,20),(1,'b',21),(2,'b',22),(2,NULL,23)) r(k,s,v) ORDER BY k DESC,s NULLS FIRST) b
		ON a.k=b.k AND a.s=b.s`)
	env, shared, dir := mergeSQLEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	mergeSQLBag(t, rows(t, portal), [][]model.Value{{i(10), i(20)}, {i(11), i(21)}, {i(12), null}, {i(13), null}, {null, i(22)}, {null, i(23)}})
	mergeSQLReleased(t, env, shared, dir)
}

func TestSQLMergeJoinDemandDoesNotDrainEqualityGroup(t *testing.T) {
	const prefix = `SELECT a.v,b.v FROM (SELECT * FROM (VALUES(1,10),(1,10)) l(k,v) ORDER BY k) a JOIN (SELECT * FROM (VALUES`
	sql := prefix + strings.TrimSuffix(strings.Repeat("(1,20),", 32), ",") + `) r(k,v) ORDER BY k) b ON a.k=b.k`
	for _, termination := range []string{"limit", "close", "cancel", "extent"} {
		t.Run(termination, func(t *testing.T) {
			query := sql
			if termination == "limit" {
				query += " LIMIT 1"
			}
			graph := mergeSQLGraph(t, query)
			env, shared, dir := mergeSQLEnvironment(t, true)
			env.Join.PartitionBytes = 256
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatalf("first demanded match drained equality group: %v", err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 2 || batch.Columns[0].Int64[batch.Lane(0)] != 10 || batch.Columns[1].Int64[batch.Lane(0)] != 20 {
				t.Fatalf("first match=%+v", batch)
			}
			if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles == 0 {
				t.Fatalf("active equality group did not spill: %+v", usage)
			}
			switch termination {
			case "limit":
				if err := portal.Next(t.Context(), &batch); err != io.EOF {
					t.Fatalf("root LIMIT advanced right equality group: %v", err)
				}
			case "close":
				closePortal(t, portal)
			case "cancel":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel active equality group: %v", err)
				}
			case "extent":
				for range 64 {
					err = portal.Next(t.Context(), &batch)
					if err != nil {
						break
					}
				}
				if !errors.Is(err, runtime.ErrBudget) {
					t.Fatalf("equality group extent bound: %v", err)
				}
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestSQLMergeJoinResidualErrorReleasesSpilledGroup(t *testing.T) {
	graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM
		(SELECT * FROM (VALUES(1,10),(1,11)) l(k,v) ORDER BY k,v) a JOIN
		(SELECT * FROM (VALUES(1,20),(1,21)) r(k,v) ORDER BY k,v) b
		ON a.k=b.k AND 10/(21-b.v)>0`)
	env, shared, dir := mergeSQLEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	batch := columnar.Batch{MaxRows: 1}
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || batch.Columns[1].Int64[batch.Lane(0)] != 20 {
		t.Fatalf("first residual match=%+v", batch)
	}
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" || batch.Rows() != 0 {
		t.Fatalf("residual error=%v rows=%d", err, batch.Rows())
	}
	mergeSQLReleased(t, env, shared, dir)
}

func TestSQLMergeJoinCorrelatedInvocation(t *testing.T) {
	graph := mergeSQLGraph(t, `SELECT o.x,j.v FROM (VALUES(1),(2),(1)) o(x) CROSS JOIN LATERAL (
		SELECT a.v FROM (SELECT * FROM (VALUES(1,10),(1,20)) l(k,v) ORDER BY k) a JOIN
		(SELECT * FROM (VALUES(1),(1)) r(k) ORDER BY k) b ON a.k=b.k AND a.v=o.x*10
	) j`)
	env, shared, dir := mergeSQLEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	mergeSQLBag(t, rows(t, portal), [][]model.Value{{i(1), i(10)}, {i(1), i(10)}, {i(2), i(20)}, {i(2), i(20)}, {i(1), i(10)}, {i(1), i(10)}})
	mergeSQLReleased(t, env, shared, dir)
}

func TestSQLMergeJoinSpillCreateFailure(t *testing.T) {
	graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM
		(SELECT * FROM (VALUES(1,10),(1,11)) l(k,v) ORDER BY k) a JOIN
		(SELECT * FROM (VALUES(1,20),(1,21)) r(k,v) ORDER BY k) b ON a.k=b.k`)
	env, shared, dir := mergeSQLEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	if restoreErr := os.Mkdir(dir, 0700); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if !errors.Is(err, os.ErrNotExist) || batch.Rows() != 0 {
		t.Fatalf("spill create error=%v rows=%d", err, batch.Rows())
	}
	mergeSQLReleased(t, env, shared, dir)
}

func TestSQLMergeJoinEmptyInputs(t *testing.T) {
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	for _, scenario := range []struct {
		name, leftFilter, rightFilter string
		want                          [][]model.Value
	}{
		{"left", "WHERE FALSE", "", [][]model.Value{{null, i(20)}, {null, i(21)}}},
		{"right", "", "WHERE FALSE", [][]model.Value{{i(10), null}, {i(11), null}}},
		{"both", "WHERE FALSE", "WHERE FALSE", nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := mergeSQLGraph(t, `SELECT a.v,b.v FROM
				(SELECT * FROM (VALUES(1,10),(1,11)) l(k,v) `+scenario.leftFilter+` ORDER BY k) a FULL JOIN
				(SELECT * FROM (VALUES(1,20),(1,21)) r(k,v) `+scenario.rightFilter+` ORDER BY k) b ON a.k=b.k`)
			env, shared, dir := mergeSQLEnvironment(t, true)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			mergeSQLBag(t, rows(t, portal), scenario.want)
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}
