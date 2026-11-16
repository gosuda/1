package exec_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

// optionalResidualReader models a storage engine selecting different conjuncts.
// The native batch keeps full PAX vectors even when its active morsel is empty.
type optionalResidualReader struct {
	storage.SnapshotReader
	mode         string
	predicate    storage.Predicate
	accepted     storage.Predicate
	residual     storage.Predicate
	change       func(*storage.PushdownPlan)
	planError    error
	opened       int
	consumed     int
	closed       bool
	cursorClosed bool
}

func (r *optionalResidualReader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	if err := ctx.Err(); err != nil {
		return storage.PushdownPlan{}, err
	}
	if r.planError != nil {
		return storage.PushdownPlan{}, r.planError
	}
	plan := storage.PushdownPlan{Request: request, Projection: []model.ColumnID{2, 1}}
	switch r.mode {
	case "accepted":
		plan.Accepted = r.predicate
	case "mixed":
		plan.Accepted, plan.Residual = r.accepted, r.residual
	default:
		plan.Residual = r.predicate
	}
	if r.change != nil {
		r.change(&plan)
	}
	return plan, nil
}

func (r *optionalResidualReader) VectorizedScan(ctx context.Context, _ storage.PushdownPlan) (storage.BatchCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.opened++
	return &optionalResidualCursor{reader: r}, nil
}

func (r *optionalResidualReader) Close() error { r.closed = true; return nil }

type optionalResidualCursor struct {
	reader    *optionalResidualReader
	morsel    int
	selection [2]uint32
}

func (c *optionalResidualCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.morsel == 4 {
		return io.EOF
	}
	c.reader.consumed++
	active := c.selection[:0]
	if c.morsel > 0 {
		for row := (c.morsel - 1) * 2; row < c.morsel*2; row++ {
			if c.reader.mode == "accepted" && row != 1 && row != 2 && row != 4 {
				continue
			}
			if c.reader.mode == "mixed" && row == 0 {
				continue
			}
			active = append(active, uint32(row))
		}
	}
	c.morsel++
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	out.Columns = append(out.Columns,
		columnar.Vector{Type: typ, Length: 6, Int64: []int64{2, 0, 2, 3, 0, 4}, Nulls: []byte{1<<1 | 1<<4}},
		columnar.Vector{Type: typ, Length: 6, Int64: []int64{1, 2, 3, 4, 5, 6}})
	out.Length, out.Selection = 6, active
	return nil
}

func (c *optionalResidualCursor) Close() error { c.reader.cursorClosed = true; return nil }

func residualFixture(t *testing.T, mode string) (*optionalResidualReader, storage.PushdownRequest) {
	t.Helper()
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	boolean := model.Type{ID: 16, Storage: model.TypeBoolean}
	predicate, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, Type: typ, ColumnID: 2},
		{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: 0},
		{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: 1},
		{Kind: storage.PredicateIn, Type: boolean, Operator: 410, Left: 0, FirstArgument: 0, ArgumentCount: 2},
		{Kind: storage.PredicateNullTest, Type: boolean, Left: 0},
		{Kind: storage.PredicateOr, Type: boolean, Left: 3, Right: 4},
		{Kind: storage.PredicateColumn, Type: typ, ColumnID: 1},
		{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: 0},
		{Kind: storage.PredicateComparison, Type: boolean, Operator: 415, Left: 6, Right: 7},
		{Kind: storage.PredicateAnd, Type: boolean, Left: 8, Right: 5},
	}, []uint32{1, 2}, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := predicate.Subtree(8)
	if err != nil {
		t.Fatal(err)
	}
	residual, err := predicate.Subtree(5)
	if err != nil {
		t.Fatal(err)
	}
	reader := &optionalResidualReader{mode: mode, predicate: predicate, accepted: accepted, residual: residual}
	request := storage.PushdownRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Layout: []storage.ColumnLayout{{ID: 1, Type: typ}, {ID: 2, Type: typ, Nullable: true}}, ColumnIDs: []model.ColumnID{1},
		Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}, Predicate: predicate}
	return reader, request
}

func TestStorageResidualMatchesAcceptedPushdown(t *testing.T) {
	for _, mode := range []string{"accepted", "residual", "mixed"} {
		for _, surface := range []struct {
			name         string
			physicalRows int
		}{{"selected-compaction", 2}, {"borrowed-selection", 8}} {
			t.Run(mode+"/"+surface.name, func(t *testing.T) {
				reader, request := residualFixture(t, mode)
				graph := scanGraph(t, reader, request, false, false)
				env, shared := environment(t)
				env.Reader = reader
				env.Limits.Expressions.MaxRows = surface.physicalRows
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				got := rows(t, portal)
				want := [][]model.Value{{model.Int64Val(2)}, {model.Int64Val(3)}, {model.Int64Val(5)}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s rows = %v, want %v", mode, got, want)
				}
				if !reader.closed || !reader.cursorClosed || shared.Usage().Live != 0 {
					t.Fatalf("EOF retained scan ownership: reader=%v cursor=%v memory=%+v", reader.closed, reader.cursorClosed, shared.Usage())
				}
			})
		}
	}
}

func TestStorageResidualRejectsChangedDecisionBeforeConsumption(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*storage.PushdownPlan)
	}{
		{"request", func(p *storage.PushdownPlan) { p.Request.Direction = storage.ScanReverse }},
		{"accepted", func(p *storage.PushdownPlan) { p.Accepted = p.Residual }},
		{"residual", func(p *storage.PushdownPlan) { p.Residual = storage.Predicate{} }},
		{"projection", func(p *storage.PushdownPlan) { p.Projection = []model.ColumnID{1, 2} }},
		{"ordering", func(p *storage.PushdownPlan) { p.Ordering = []storage.ScanOrdering{{KeyOrdinal: 0}} }},
		{"limit", func(p *storage.PushdownPlan) { p.AppliedLimit = storage.ScanLimit{Enabled: true, Count: 1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, request := residualFixture(t, "residual")
			graph := scanGraph(t, reader, request, false, false)
			reader.change = test.change
			env, shared := environment(t)
			env.Reader = reader
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var out columnar.Batch
			if err := portal.Next(t.Context(), &out); !errors.Is(err, exec.ErrCapability) {
				t.Fatalf("changed decision = %v", err)
			}
			if reader.opened != 0 || reader.consumed != 0 || !reader.closed || shared.Usage().Live != 0 {
				t.Fatalf("capability failure consumed or retained source: %+v, memory=%+v", reader, shared.Usage())
			}
		})
	}
}

func TestStorageResidualRejectsUnknownOperatorBeforeConsumption(t *testing.T) {
	reader, request := residualFixture(t, "residual")
	typ := request.Layout[0].Type
	predicate, err := storage.NewPredicate([]storage.PredicateNode{
		{Kind: storage.PredicateColumn, Type: typ, ColumnID: 1},
		{Kind: storage.PredicateConstant, Type: typ},
		{Kind: storage.PredicateComparison, Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Operator: model.OperatorID(4000000000), Left: 0, Right: 1},
	}, nil, []model.Value{model.Int64Val(2)})
	if err != nil {
		t.Fatal(err)
	}
	reader.predicate, request.Predicate = predicate, predicate
	graph := scanGraph(t, reader, request, false, false)
	env, shared := environment(t)
	env.Reader = reader
	if _, err := exec.Open(t.Context(), graph, env); !errors.Is(err, exec.ErrCapability) {
		t.Fatalf("unknown residual operator = %v", err)
	}
	if reader.opened != 0 || reader.consumed != 0 || !reader.closed || shared.Usage().Live != 0 {
		t.Fatalf("unsupported residual consumed or retained source: %+v, memory=%+v", reader, shared.Usage())
	}
}

func TestStorageResidualCancellationClosesNativeCursor(t *testing.T) {
	reader, request := residualFixture(t, "residual")
	graph := scanGraph(t, reader, request, false, false)
	env, shared := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var out columnar.Batch
	if err := portal.Next(t.Context(), &out); err != nil || out.Rows() != 0 {
		t.Fatalf("empty native morsel = %d rows, %v", out.Rows(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := portal.Next(ctx, &out); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled residual = %v", err)
	}
	if !reader.closed || !reader.cursorClosed || shared.Usage().Live != 0 {
		t.Fatalf("cancellation retained source: %+v, memory=%+v", reader, shared.Usage())
	}
}

func TestStorageResidualPropagatesPlanningErrorWithoutRetry(t *testing.T) {
	reader, request := residualFixture(t, "residual")
	graph := scanGraph(t, reader, request, false, false)
	failure := errors.New("snapshot capabilities unavailable")
	reader.planError = failure
	env, shared := environment(t)
	env.Reader = reader
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var out columnar.Batch
	for range 2 {
		if err := portal.Next(t.Context(), &out); !errors.Is(err, failure) {
			t.Fatalf("planning error = %v", err)
		}
	}
	if reader.opened != 0 || !reader.closed || shared.Usage().Live != 0 {
		t.Fatalf("planning error retained source: %+v, memory=%+v", reader, shared.Usage())
	}
}

func TestStorageResidualNotInThreeValuedSemantics(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []uint32
		want      []int64
	}{
		{"non-null-member", []uint32{1}, []int64{4, 6}},
		{"null-member", []uint32{1, 2}, nil},
		{"empty-list", nil, []int64{1, 2, 3, 4, 5, 6}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, request := residualFixture(t, "residual")
			typ := request.Layout[0].Type
			predicate, err := storage.NewPredicate([]storage.PredicateNode{
				{Kind: storage.PredicateColumn, Type: typ, ColumnID: 2},
				{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: 0},
				{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: 1},
				{Kind: storage.PredicateIn, Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Operator: 410, Left: 0, ArgumentCount: uint32(len(test.arguments)), Negated: true},
			}, test.arguments, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)})
			if err != nil {
				t.Fatal(err)
			}
			reader.predicate, request.Predicate = predicate, predicate
			graph := scanGraph(t, reader, request, false, false)
			env, _ := environment(t)
			env.Reader = reader
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var got []int64
			for _, row := range rows(t, portal) {
				got = append(got, row[0].IntVal)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("NOT IN IDs = %v, want %v", got, test.want)
			}
		})
	}
}
