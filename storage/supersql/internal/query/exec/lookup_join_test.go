package exec_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

var errLookupProbe = errors.New("lookup probe failure")

// Only capability negotiation and cursor ownership are observed; native readers
// still perform span pruning, MVCC visibility, filtering and file reads.
type lookupProbeReader struct {
	*residualPolicyReader
	requests                            []storage.PushdownRequest
	active, peak, pulls, returned, gets int
	closed                              bool
	armed                               bool
	failure                             string
}

func (r *lookupProbeReader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	owned, _, err := storage.CopyPushdownRequest(ctx, request, 1<<20)
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	r.requests = append(r.requests, owned)
	if r.armed && r.failure == "negotiate" {
		return storage.PushdownPlan{}, errLookupProbe
	}
	return r.residualPolicyReader.PlanScan(ctx, request)
}

func (r *lookupProbeReader) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	r.gets++
	return r.SnapshotReader.Get(ctx, request)
}

func (r *lookupProbeReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	cursor, err := r.residualPolicyReader.VectorizedScan(ctx, plan)
	if err != nil {
		return nil, err
	}
	r.active++
	r.peak = max(r.peak, r.active)
	observed := &lookupProbeCursor{BatchCursor: cursor, owner: r}
	if r.armed && r.failure == "open" {
		return observed, errLookupProbe
	}
	return observed, nil
}

func (r *lookupProbeReader) Close() error {
	r.closed = true
	return r.residualPolicyReader.Close()
}

type lookupProbeCursor struct {
	storage.BatchCursor
	owner  *lookupProbeReader
	closed bool
}

func (c *lookupProbeCursor) Next(ctx context.Context, batch *columnar.Batch) error {
	c.owner.pulls++
	if c.owner.armed && c.owner.failure == "pull" {
		return errLookupProbe
	}
	err := c.BatchCursor.Next(ctx, batch)
	if err == nil {
		c.owner.returned += int(batch.Rows())
	}
	return err
}

func (c *lookupProbeCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.owner.active--
	return c.BatchCursor.Close()
}

func lookupReader(t *testing.T, s *pushdownStore, timestamp model.Timestamp, overlay storage.OverlayView, residual bool) *lookupProbeReader {
	t.Helper()
	return &lookupProbeReader{residualPolicyReader: &residualPolicyReader{SnapshotReader: s.snapshot(t, timestamp, overlay), residual: residual}}
}

func lookupGraph(t *testing.T, reader storage.SnapshotReader, request storage.PushdownRequest, kind ir.JoinKind, leftValues []model.Value, parameter bool) *fragment.FragmentGraph {
	t.Helper()
	b, err := ir.NewBuilder(irLimits())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	integer, err := b.AddLayout(t.Context(), model.TypeLayout{Type: request.KeyLayout[0].Type})
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	addSlot := func(layout ir.LayoutID, nullable bool, object model.ObjectID, column model.ColumnID) physical.Slot {
		id, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: layout, Nullable: nullable, Object: object, Column: column})
		if err != nil {
			t.Fatal(err)
		}
		return physical.Slot{Graph: 1, ID: id, Layout: layout, Nullable: nullable}
	}
	addScalar := func(spec ir.ScalarSpec) ir.ScalarID {
		spec.Scope = scope
		id, err := b.AddScalar(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	leftSlot := addSlot(integer, true, 0, 0)
	var cells []ir.ScalarID
	var expressions []physical.ScalarRef
	for _, value := range leftValues {
		spec := ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Layout: integer, Nullable: value.IsNull}, Value: value}
		if parameter {
			spec = ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Param, Layout: integer, Nullable: true, Parameter: 1}}
		}
		id := addScalar(spec)
		cells = append(cells, id)
		expressions = append(expressions, physical.ScalarRef{Graph: 1, Scalar: id})
	}
	left, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope, Rows: uint64(len(cells))}, Outputs: []model.SlotID{leftSlot.ID}, Expressions: cells})
	if err != nil {
		t.Fatal(err)
	}
	var rightSlots []physical.Slot
	var rightIDs []model.SlotID
	source := request.Target
	if request.IndexID != 0 {
		source.ObjectID = 2
	}
	columnMap := make([]physical.ScanColumn, len(request.ColumnIDs))
	for i, id := range request.ColumnIDs {
		columnMap[i] = physical.ScanColumn{Source: id, Target: id}
		var column storage.ColumnLayout
		for _, candidate := range request.Layout {
			if candidate.ID == id {
				column = candidate
			}
		}
		layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: column.Type})
		if err != nil {
			t.Fatal(err)
		}
		slot := addSlot(layout, column.Nullable, source.ObjectID, id)
		rightSlots = append(rightSlots, slot)
		rightIDs = append(rightIDs, slot.ID)
	}
	right, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope, Object: source.ObjectID, Generation: source.Generation, DescriptorVersion: source.DescriptorVersion}, Outputs: rightIDs})
	if err != nil {
		t.Fatal(err)
	}
	signature, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 410, Result: boolean, Strict: true, Nullable: true}, Parameters: []ir.LayoutID{integer, rightSlots[0].Layout}})
	if err != nil {
		t.Fatal(err)
	}
	lhs := addScalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Layout: integer, Nullable: true, Slot: leftSlot.ID}})
	rhs := addScalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Layout: rightSlots[0].Layout, Nullable: rightSlots[0].Nullable, Slot: rightSlots[0].ID}})
	predicate := addScalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Layout: boolean, Nullable: true, Operator: 410, Signature: signature, Strict: true}, Operands: []ir.ScalarID{lhs, rhs}})
	outputs := []physical.Slot{leftSlot}
	if kind != ir.SemiJoin && kind != ir.AntiJoin {
		for _, slot := range rightSlots {
			if kind == ir.LeftJoin {
				slot = addSlot(slot.Layout, true, 0, 0)
			}
			outputs = append(outputs, slot)
		}
	}
	outputIDs := make([]model.SlotID, len(outputs))
	for i, slot := range outputs {
		outputIDs[i] = slot.ID
	}
	root, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: scope, JoinKind: kind, Predicate: predicate}, Inputs: []ir.RelID{left, right}, Outputs: outputIDs})
	if err != nil {
		t.Fatal(err)
	}
	logical, err := b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.ReleasePlan(decision); err != nil {
			t.Error(err)
		}
	}()
	properties := physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
	algorithm := physical.PrimaryKeyScan
	if request.IndexID != 0 {
		algorithm = physical.SecondaryScan
	}
	key := physical.JoinKey{Left: physical.Key{Slot: leftSlot, Type: request.KeyLayout[0].Type}, Right: physical.Key{Slot: rightSlots[0], Type: request.KeyLayout[0].Type}, Equality: 410, Nulls: physical.NullsUnequal, Predicate: physical.ScalarRef{Graph: 1, Scalar: predicate}}
	nodes := []physical.NodeSpec{
		{Logical: physical.RelRef{Graph: 1, Rel: left}, Algorithm: physical.Values, Output: []physical.Slot{leftSlot}, Expressions: expressions, Delivered: properties, Break: physical.Streaming},
		{Logical: physical.RelRef{Graph: 1, Rel: right}, Algorithm: algorithm, Output: rightSlots, Delivered: properties, Break: physical.Streaming, Scan: physical.ScanSpec{Source: source, ColumnMap: columnMap, CapabilityEpoch: 1, Request: request, Accepted: decision.Accepted, Residual: decision.Residual, Projection: decision.Projection, Ordering: decision.Ordering, Estimate: decision.Estimate}},
		{Logical: physical.RelRef{Graph: 1, Rel: root}, Algorithm: physical.LookupJoin, Children: []physical.NodeID{1, 2}, Output: outputs, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: predicate}}, Required: []physical.Properties{properties, properties}, Delivered: properties, Break: physical.Streaming, Join: physical.JoinSpec{Keys: []physical.JoinKey{key}, Lookup: request.Target, LookupIndex: request.IndexID}},
	}
	plan, err := physical.NewPlan(t.Context(), physical.Spec{Graphs: []*ir.Graph{logical}, Nodes: nodes, Root: 3}, physical.Limits{MaxNodes: 100, MaxOperands: 1000, MaxProperties: 1000, MaxDepth: 100, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 100, MaxEdges: 100, MaxDepth: 100, MaxWork: 10000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func lookupAssertProbes(t *testing.T, reader *lookupProbeReader, want []storage.KeySpan, target storage.ObjectRef, index model.IndexID) {
	t.Helper()
	if len(reader.requests) != len(want) {
		t.Fatalf("probe requests=%+v, want spans=%+v", reader.requests, want)
	}
	for i, request := range reader.requests {
		if request.Target != target || request.IndexID != index || !reflect.DeepEqual(request.Spans, []storage.KeySpan{want[i]}) {
			t.Errorf("probe %d: target=%+v index=%d spans=%+v; want target=%+v index=%d span=%+v", i, request.Target, request.IndexID, request.Spans, target, index, want[i])
		}
	}
	if reader.active != 0 || reader.peak > 1 || !reader.closed || reader.gets != 0 {
		t.Errorf("probe ownership: active=%d peak=%d snapshot closed=%v point reads=%d", reader.active, reader.peak, reader.closed, reader.gets)
	}
}

func lookupPoint(values ...int64) storage.KeySpan {
	key := make([]model.Value, len(values))
	for i, value := range values {
		key[i] = model.Int64Val(value)
	}
	return storage.KeySpan{Lower: storage.KeyBound{Values: key, Inclusive: true}, Upper: storage.KeyBound{Values: key, Inclusive: true}}
}

func TestLookupJoinPrimaryDuplicateNullAndUnmatchedRows(t *testing.T) {
	i, null := model.Int64Val, model.NullVal(model.TypeInt64)
	for _, sample := range []struct {
		name string
		kind ir.JoinKind
		want [][]model.Value
	}{
		{"inner", ir.InnerJoin, [][]model.Value{{i(1), i(1), i(10)}, {i(1), i(1), i(10)}, {i(4), i(4), i(20)}}},
		{"left", ir.LeftJoin, [][]model.Value{{i(1), i(1), i(10)}, {i(1), i(1), i(10)}, {i(4), i(4), i(20)}, {i(99), null, null}, {null, null, null}}},
		{"semi", ir.SemiJoin, [][]model.Value{{i(1)}, {i(1)}, {i(4)}}},
		{"anti", ir.AntiJoin, [][]model.Value{{i(99)}, {null}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			s := newPushdownStore(t)
			request := s.request
			request.ColumnIDs = []model.ColumnID{1, 2}
			reader := lookupReader(t, s, 20, nil, false)
			graph := lookupGraph(t, reader, request, sample.kind, []model.Value{i(1), i(1), i(4), i(99), null}, false)
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Reader = reader
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			reader.requests = nil
			mergeSQLBag(t, rows(t, portal), sample.want)
			closePortal(t, portal)
			lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(1), lookupPoint(1), lookupPoint(4), lookupPoint(99)}, request.Target, 0)
			if reader.returned != 3 {
				t.Errorf("native rows=%d, want 3 matching rows rather than reread relation", reader.returned)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestLookupJoinReclaimsLiveProbePlans(t *testing.T) {
	s := newPushdownStore(t, func(limits *neosepia.Limits) { limits.Plans = 1 })
	request := s.request
	request.ColumnIDs = []model.ColumnID{1, 2}
	reader := lookupReader(t, s, 20, nil, false)
	values := make([]model.Value, 64)
	want := make([][]model.Value, len(values))
	spans := make([]storage.KeySpan, len(values))
	for i := range values {
		values[i] = model.Int64Val(1)
		want[i] = []model.Value{model.Int64Val(1), model.Int64Val(1), model.Int64Val(10)}
		spans[i] = lookupPoint(1)
	}
	graph := lookupGraph(t, reader, request, ir.InnerJoin, values, false)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	reader.requests = nil
	mergeSQLBag(t, rows(t, portal), want)
	closePortal(t, portal)
	lookupAssertProbes(t, reader, spans, request.Target, 0)
	mergeSQLReleased(t, env, shared, dir)
}

func lookupCompositeStore(t *testing.T, secondary bool) *pushdownStore {
	t.Helper()
	s := newPushdownStore(t)
	integer := s.request.Layout[0].Type
	s.request.Target.ObjectID = 2
	s.request.Layout = []storage.ColumnLayout{{ID: 1, Type: integer}, {ID: 2, Type: integer}, {ID: 3, Type: integer}}
	s.request.KeyLayout = []storage.KeyColumn{{ColumnID: 1, Type: integer}, {ColumnID: 2, Type: integer}}
	s.request.ColumnIDs = []model.ColumnID{1, 2, 3}
	if secondary {
		s.request.IndexID = 7
		s.request.Target.ObjectID = 7
	}
	var mutations []storage.Mutation
	for _, row := range [][]model.Value{{model.Int64Val(1), model.Int64Val(10), model.Int64Val(100)}, {model.Int64Val(1), model.Int64Val(20), model.Int64Val(200)}, {model.Int64Val(2), model.Int64Val(10), model.Int64Val(300)}, {model.Int64Val(3), model.Int64Val(10), model.Int64Val(400)}} {
		key, err := s.encoder.EncodePrefix(t.Context(), s.request.KeyLayout, row[:2], 128)
		if err != nil {
			t.Fatal(err)
		}
		mutations = append(mutations, storage.NewUpsertMutation(s.request.Target, key, s.request.ColumnIDs, row))
	}
	s.apply(t, 40, mutations...)
	return s
}

func TestLookupJoinCompositePrefixPreservesEveryDuplicatePair(t *testing.T) {
	i := model.Int64Val
	s := lookupCompositeStore(t, true)
	reader := lookupReader(t, s, 40, nil, false)
	graph := lookupGraph(t, reader, s.request, ir.InnerJoin, []model.Value{i(1), i(1)}, false)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	reader.requests = nil
	mergeSQLBag(t, rows(t, portal), [][]model.Value{{i(1), i(1), i(10), i(100)}, {i(1), i(1), i(20), i(200)}, {i(1), i(1), i(10), i(100)}, {i(1), i(1), i(20), i(200)}})
	closePortal(t, portal)
	lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(1), lookupPoint(1)}, s.request.Target, s.request.IndexID)
	if reader.returned != 4 {
		t.Errorf("native rows=%d, want four prefix matches without relation scans or cached results", reader.returned)
	}
	mergeSQLReleased(t, env, shared, dir)
}

func TestLookupJoinCompositePrefixIntersectsOriginalSpans(t *testing.T) {
	i := model.Int64Val
	for _, secondary := range []bool{false, true} {
		name := "primary"
		if secondary {
			name = "secondary"
		}
		t.Run(name, func(t *testing.T) {
			s := lookupCompositeStore(t, secondary)
			request := s.request
			request.Spans = []storage.KeySpan{{Lower: lookupPoint(1, 10).Lower, Upper: lookupPoint(2).Upper}}
			request.Spans[0].Lower.Inclusive = false
			reader := lookupReader(t, s, 40, nil, false)
			graph := lookupGraph(t, reader, request, ir.LeftJoin, []model.Value{i(1), i(2), i(3)}, false)
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Reader = reader
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			reader.requests = nil
			null := model.NullVal(model.TypeInt64)
			mergeSQLBag(t, rows(t, portal), [][]model.Value{{i(1), i(1), i(20), i(200)}, {i(2), i(2), i(10), i(300)}, {i(3), null, null, null}})
			closePortal(t, portal)
			lookupAssertProbes(t, reader, []storage.KeySpan{{Lower: request.Spans[0].Lower, Upper: lookupPoint(1).Upper}, lookupPoint(2)}, request.Target, request.IndexID)
			if reader.returned != 2 {
				t.Errorf("native rows=%d, want 2 after span intersection", reader.returned)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestLookupJoinEmptyIntersectionDoesNotOpenStorage(t *testing.T) {
	s := newPushdownStore(t)
	request := s.request
	request.Spans = []storage.KeySpan{lookupPoint(2)}
	reader := lookupReader(t, s, 20, nil, false)
	graph := lookupGraph(t, reader, request, ir.InnerJoin, []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64)}, false)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	reader.requests = nil
	opened := reader.opened
	mergeSQLBag(t, rows(t, portal), nil)
	closePortal(t, portal)
	lookupAssertProbes(t, reader, nil, request.Target, 0)
	if reader.opened != opened || reader.pulls != 0 {
		t.Errorf("empty intersection touched storage: opens=%d pulls=%d", reader.opened-opened, reader.pulls)
	}
	mergeSQLReleased(t, env, shared, dir)
}

func TestLookupJoinDemandClosesUndrainedPrefix(t *testing.T) {
	s := lookupCompositeStore(t, false)
	reader := lookupReader(t, s, 40, nil, false)
	graph := lookupGraph(t, reader, s.request, ir.InnerJoin, []model.Value{model.Int64Val(1), model.Int64Val(2)}, false)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	reader.requests = nil
	batch := columnar.Batch{MaxRows: 1}
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || batch.Columns[3].Int64[batch.Lane(0)] != 100 {
		t.Fatalf("first prefix match=%+v", batch)
	}
	if reader.returned != 1 || reader.pulls != 1 {
		t.Errorf("demand drained prefix: returned=%d pulls=%d", reader.returned, reader.pulls)
	}
	closePortal(t, portal)
	lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(1)}, s.request.Target, 0)
	mergeSQLReleased(t, env, shared, dir)
}

func TestLookupJoinProbeFailureDoesNotFallback(t *testing.T) {
	for _, failure := range []string{"negotiate", "open", "pull"} {
		t.Run(failure, func(t *testing.T) {
			s := newPushdownStore(t)
			reader := lookupReader(t, s, 20, nil, false)
			graph := lookupGraph(t, reader, s.request, ir.InnerJoin, []model.Value{model.Int64Val(1), model.Int64Val(2)}, false)
			env, shared, dir := mergeSQLEnvironment(t, false)
			env.Reader = reader
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			reader.requests = nil
			reader.armed, reader.failure = true, failure
			batch := columnar.Batch{MaxRows: 1}
			if err := portal.Next(t.Context(), &batch); !errors.Is(err, errLookupProbe) {
				t.Fatalf("probe failure=%v, want original cause", err)
			}
			closePortal(t, portal)
			lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(1)}, s.request.Target, 0)
			if reader.returned != 0 {
				t.Errorf("failed probe published %d storage rows", reader.returned)
			}
			mergeSQLReleased(t, env, shared, dir)
		})
	}
}

func TestLookupJoinFilteringPreservesSnapshotAndOwnWrites(t *testing.T) {
	i := model.Int64Val
	s := newPushdownStore(t)
	overlay, err := txn.NewOverlay(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := overlay.Close(); err != nil {
			t.Error(err)
		}
	})
	updates := []storage.Mutation{s.upsert(t, 2, i(10), "0"), storage.NewDeleteMutation(s.request.Target, s.key(t, 3))}
	for i := range updates {
		updates[i].Ordinal = uint64(i + 1)
	}
	if err := overlay.StageOwned(t.Context(), updates); err != nil {
		t.Fatal(err)
	}
	own, err := overlay.Freeze(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := own.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, sample := range []struct {
		name      string
		timestamp model.Timestamp
		overlay   storage.OverlayView
		want      [][]model.Value
	}{
		{"historical", 20, nil, [][]model.Value{{i(1), i(1)}, {i(3), i(3)}}},
		{"newer", 30, nil, [][]model.Value{{i(3), i(3)}}},
		{"own_write", 30, own, [][]model.Value{{i(2), i(2)}}},
	} {
		for _, residual := range []bool{false, true} {
			name := sample.name + "/accepted"
			if residual {
				name = sample.name + "/residual"
			}
			t.Run(name, func(t *testing.T) {
				reader := lookupReader(t, s, sample.timestamp, sample.overlay, residual)
				request := s.request
				request.Predicate = s.predicate(t, false)
				graph := lookupGraph(t, reader, request, ir.InnerJoin, []model.Value{i(1), i(2), i(3)}, false)
				env, shared, dir := mergeSQLEnvironment(t, false)
				env.Reader = reader
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				reader.requests = nil
				mergeSQLBag(t, rows(t, portal), sample.want)
				closePortal(t, portal)
				lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(1), lookupPoint(2), lookupPoint(3)}, request.Target, 0)
				if reader.accepted == residual {
					t.Errorf("native acceptance=%v residual policy=%v", reader.accepted, residual)
				}
				mergeSQLReleased(t, env, shared, dir)
			})
		}
	}
}

func TestLookupJoinTypedParameterSuppliesProbeKey(t *testing.T) {
	s := newPushdownStore(t)
	reader := lookupReader(t, s, 20, nil, false)
	graph := lookupGraph(t, reader, s.request, ir.InnerJoin, []model.Value{model.Int64Val(0)}, true)
	env, shared, dir := mergeSQLEnvironment(t, false)
	env.Reader = reader
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: model.TypeLayout{Type: s.request.KeyLayout[0].Type}, Value: model.Int64Val(4)}}
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	reader.requests = nil
	mergeSQLBag(t, rows(t, portal), [][]model.Value{{model.Int64Val(4), model.Int64Val(4)}})
	closePortal(t, portal)
	lookupAssertProbes(t, reader, []storage.KeySpan{lookupPoint(4)}, s.request.Target, 0)
	mergeSQLReleased(t, env, shared, dir)
}
