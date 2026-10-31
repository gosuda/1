package explain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func scanPlan(t *testing.T, text string) *fragment.FragmentGraph {
	t.Helper()
	builder, err := ir.NewBuilder(ir.Limits{MaxNodes: 32, MaxOperands: 64, MaxSlots: 8, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := builder.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	types := []model.Type{{ID: 701, Modifier: -1, Storage: model.TypeFloat64}, {ID: 25, Modifier: -1, Collation: 950, Storage: model.TypeString}}
	var slots []model.SlotID
	var outputs []physical.Slot
	var columns []storage.ColumnLayout
	for i, typ := range types {
		layout, err := builder.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		slot, err := builder.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Nullable: true, Object: 7, Column: model.ColumnID(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, slot)
		outputs = append(outputs, physical.Slot{Graph: 1, ID: slot, Layout: layout, Nullable: true})
		columns = append(columns, storage.ColumnLayout{ID: model.ColumnID(i + 1), Type: typ, Nullable: true})
	}
	relation, err := builder.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: 7, Generation: 2, DescriptorVersion: 4}, Outputs: slots})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := builder.Build(t.Context(), relation)
	if err != nil {
		t.Fatal(err)
	}
	boolean := model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}
	equality := func(typ model.TypeID) model.OperatorID {
		for op := range builtin.OperatorsNamed("=") {
			if op.Left == typ && op.Right == typ {
				return op.ID
			}
		}
		t.Fatalf("missing equality for %d", typ)
		return 0
	}
	predicate, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, Type: types[0], ColumnID: 1},
		{Kind: storage.PredicateConstant, Type: types[0], ConstantIndex: 0},
		{Kind: storage.PredicateComparison, Type: boolean, Operator: equality(701), Left: 0, Right: 1},
		{Kind: storage.PredicateColumn, Type: types[1], ColumnID: 2},
		{Kind: storage.PredicateConstant, Type: types[1], ConstantIndex: 1},
		{Kind: storage.PredicateComparison, Type: boolean, Operator: equality(25), Collation: 950, Left: 3, Right: 4},
		{Kind: storage.PredicateAnd, Type: boolean, Left: 2, Right: 5},
	}, nil, []model.Value{model.Float64Val(math.NaN()), model.StringVal(text)})
	if err != nil {
		t.Fatal(err)
	}
	target := storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 4, EncodingVersion: 1}
	request := storage.PushdownRequest{Target: target, Layout: columns, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: types[0]}}, ColumnIDs: []model.ColumnID{1, 2}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Values: []model.Value{model.Float64Val(math.NaN())}, Inclusive: true}, Upper: storage.KeyBound{Unbounded: true}}}, Predicate: predicate}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{graph}, Nodes: []physical.NodeSpec{{Logical: physical.RelRef{Graph: 1, Rel: relation}, Algorithm: physical.TableScan, Output: outputs, Delivered: physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}, Break: physical.Streaming, Scan: physical.ScanSpec{Source: target, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}, {Source: 2, Target: 2}}, CapabilityEpoch: 3, Request: request, Residual: predicate, Projection: request.ColumnIDs}}}, Root: 1}, physical.Limits{MaxNodes: 32, MaxOperands: 128, MaxProperties: 128, MaxDepth: 16, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 32, MaxEdges: 64, MaxDepth: 16, MaxWork: 1000, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}

func options() explain.Options {
	return explain.Options{Limits: explain.Limits{MaxOutputBytes: 1 << 20, MaxNodeBytes: 1 << 20, MaxValueDepth: 64}}
}

func TestPlanJSONPreservesTypedScanDecisions(t *testing.T) {
	text := strings.Repeat("a", 127) + "한글\"\\\n\x00" + strings.Repeat("界", 100) + "\u2028"
	graph := scanPlan(t, text)
	var output bytes.Buffer
	opts := options()
	opts.Planning = &explain.Planning{Duration: 17 * time.Microsecond, BudgetExhausted: true}
	if err := explain.WriteJSON(t.Context(), &output, graph, opts); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Root     int
		Budget   bool  `json:"Optimizer Budget Exhausted"`
		Planning int64 `json:"Planning Nanoseconds"`
		Nodes    []struct {
			Version uint64 `json:"Descriptor Version"`
			Scan    struct {
				Pushed   struct{ Constant bool } `json:"Pushed Predicate"`
				Residual struct {
					Root  int
					Nodes []struct{ Constant struct{ Value any } }
				} `json:"Residual Predicate"`
				Spans []struct {
					Lower struct {
						Inclusive bool
						Values    []struct{ Value string }
					}
					Upper struct{ Unbounded bool }
				}
			}
		}
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Root != 1 || !document.Budget || document.Planning != 17000 {
		t.Fatalf("planner metadata=%+v", document)
	}
	if len(document.Nodes) != 1 {
		t.Fatalf("nodes=%d, want 1", len(document.Nodes))
	}
	node := document.Nodes[0]
	if node.Version != 4 || !node.Scan.Pushed.Constant || node.Scan.Residual.Root != 6 {
		t.Fatalf("scan decision=%+v", node)
	}
	if node.Scan.Residual.Nodes[1].Constant.Value != "NaN" || node.Scan.Residual.Nodes[4].Constant.Value != text {
		t.Fatal("typed constants changed in JSON")
	}
	span := node.Scan.Spans[0]
	if !span.Lower.Inclusive || !span.Upper.Unbounded || span.Lower.Values[0].Value != "NaN" {
		t.Fatalf("span=%+v", span)
	}
}

func TestPlanJSONPublishesMeasuredValuesAndOmitsUnknownMetrics(t *testing.T) {
	graph := scanPlan(t, "value")
	affected := uint64(3)
	analysis := exec.Observation{
		ExecutionNanos: 91,
		LockWaitNanos: exec.MeasuredCount{Value: 7, Known: true},
		ResourcesKnown: true,
		Resources: runtime.QueryUsage{
			Work: runtime.Usage{Peak: 11},
			Output: runtime.Usage{Peak: 12},
			Spill: runtime.Usage{Peak: 13},
			PeakFiles: 2,
			PeakReaders: 3,
		},
		Nodes: []exec.NodeObservation{{
			Node: 1, Rows: 4, Batches: 2, Loops: 1,
			Nanoseconds: exec.MeasuredCount{Value: 17, Known: true},
			Storage: exec.StorageObservation{
				ReadBytes: exec.MeasuredCount{Value: 23, Known: true},
				BlocksPruned: exec.MeasuredCount{Known: true},
			},
		}},
	}
	opts := options()
	opts.Analysis = &analysis
	opts.AffectedRows = &affected
	var output bytes.Buffer
	if err := explain.WriteJSON(t.Context(), &output, graph, opts); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Execution uint64 `json:"Execution Nanoseconds"`
		Affected uint64 `json:"Affected Rows"`
		Resources map[string]any
		Nodes []struct {
			Rows uint64 `json:"Actual Rows"`
			Loops uint64 `json:"Actual Loops"`
			Storage map[string]any
		}
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Execution != 91 || document.Affected != 3 || len(document.Nodes) != 1 || document.Nodes[0].Rows != 4 || document.Nodes[0].Loops != 1 {
		t.Fatalf("measured report=%+v", document)
	}
	if document.Nodes[0].Storage["Read Bytes"] != float64(23) || document.Nodes[0].Storage["Blocks Pruned"] != float64(0) {
		t.Fatalf("storage metrics=%v", document.Nodes[0].Storage)
	}
	if _, present := document.Nodes[0].Storage["Decoded Bytes"]; present {
		t.Fatalf("unknown decoded bytes were rendered: %v", document.Nodes[0].Storage)
	}
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }

type cancelWriter struct {
	output bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelWriter) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	w.cancel()
	return n, err
}

func TestPlanJSONHonorsOutputBoundsAndWriterFailures(t *testing.T) {
	graph := scanPlan(t, "value")
	opts := options()
	opts.Limits.MaxOutputBytes = 64
	var output bytes.Buffer
	if err := explain.WriteJSON(t.Context(), &output, graph, opts); !errors.Is(err, explain.ErrLimit) {
		t.Fatalf("output quota error=%v", err)
	}
	if output.Len() > 64 {
		t.Fatalf("wrote %d bytes beyond quota", output.Len())
	}
	if err := explain.WriteJSON(t.Context(), shortWriter{}, graph, options()); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer error=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelWriter{cancel: cancel}
	if err := explain.WriteJSON(ctx, writer, graph, options()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}
