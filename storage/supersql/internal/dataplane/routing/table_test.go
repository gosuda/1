package routing_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func encoder(t *testing.T) storage.KeyEncoder {
	t.Helper()
	result, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func full() storage.KeySpan {
	return storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
}

func bound(inclusive bool, values ...int64) storage.KeyBound {
	result := storage.KeyBound{Inclusive: inclusive, Values: make([]model.Value, len(values))}
	for i, value := range values {
		result.Values[i] = model.Int64Val(value)
	}
	return result
}

func spec() routing.Spec {
	return routing.Spec{
		Target: storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1},
		Epoch:  9,
		KeyLayout: []storage.KeyColumn{
			{ColumnID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
			{ColumnID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		},
		Partitions: []routing.Partition{
			{ID: 10, Span: storage.KeySpan{Lower: full().Lower, Upper: bound(false, 1)}},
			{ID: 20, Span: storage.KeySpan{Lower: bound(true, 1), Upper: bound(false, 1, 0)}},
			{ID: 30, Span: storage.KeySpan{Lower: bound(true, 1, 0), Upper: full().Upper}},
		},
	}
}

func table(t *testing.T, specification routing.Spec) *routing.Table {
	t.Helper()
	result, err := routing.NewTable(t.Context(), specification, encoder(t), routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertMorsels(t *testing.T, got []routing.Morsel, specification routing.Spec, ids []uint64, spans []storage.KeySpan) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("morsels = %d, want %d: %+v", len(got), len(ids), got)
	}
	for i, morsel := range got {
		if morsel.Target != specification.Target || morsel.Epoch != specification.Epoch || morsel.PartitionID != ids[i] {
			t.Errorf("morsel %d identity = (%+v, %d, %d), want (%+v, %d, %d)", i, morsel.Target, morsel.Epoch, morsel.PartitionID, specification.Target, specification.Epoch, ids[i])
		}
		if !reflect.DeepEqual(morsel.Span, spans[i]) {
			t.Errorf("morsel %d span = %+v, want %+v", i, morsel.Span, spans[i])
		}
	}
}

func TestTenantRoutingCoalescesOnlyOverlappingCoverage(t *testing.T) {
	specification := spec()
	router := table(t, specification)
	first := storage.KeySpan{Lower: bound(true, 1, -2), Upper: bound(true, 1, 1)}
	overlap := storage.KeySpan{Lower: bound(true, 1, -1), Upper: bound(true, 1, 2)}
	disjoint := storage.KeySpan{Lower: bound(true, 2), Upper: bound(true, 2)}
	requested := []storage.KeySpan{disjoint, overlap, first, first}
	got, err := router.Route(t.Context(), requested, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	assertMorsels(t, got, specification, []uint64{20, 30, 30}, []storage.KeySpan{
		{Lower: bound(true, 1, -2), Upper: bound(false, 1, 0)},
		{Lower: bound(true, 1, 0), Upper: bound(true, 1, 2)},
		disjoint,
	})
	for tenant := int64(0); tenant <= 3; tenant++ {
		for id := int64(-3); id <= 3; id++ {
			row := []model.Value{model.Int64Val(tenant), model.Int64Val(id)}
			assertCoverage(t, specification, requested, got, row)
		}
	}
}

func TestFullAndEmptyRequests(t *testing.T) {
	specification := spec()
	router := table(t, specification)
	got, err := router.Route(t.Context(), nil, routing.DefaultLimits())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty request = %+v, %v", got, err)
	}
	got, err = router.Route(t.Context(), []storage.KeySpan{full(), full()}, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	assertMorsels(t, got, specification, []uint64{10, 20, 30}, []storage.KeySpan{specification.Partitions[0].Span, specification.Partitions[1].Span, specification.Partitions[2].Span})
}

// The finite oracle compares logical values, not encoded bytes or routing cuts.
func prefixOrder(row, prefix []model.Value, layout []storage.KeyColumn) int {
	for i, value := range prefix {
		actual := row[i]
		if actual.IsNull || value.IsNull {
			if actual.IsNull == value.IsNull {
				continue
			}
			if actual.IsNull == layout[i].NullsFirst {
				return -1
			}
			return 1
		}
		order := 0
		if actual.IntVal < value.IntVal {
			order = -1
		}
		if actual.IntVal > value.IntVal {
			order = 1
		}
		if layout[i].Descending {
			order = -order
		}
		if order != 0 {
			return order
		}
	}
	return 0
}

func contains(row []model.Value, span storage.KeySpan, layout []storage.KeyColumn) bool {
	if !span.Lower.Unbounded {
		order := prefixOrder(row, span.Lower.Values, layout)
		if order < 0 || (order == 0 && !span.Lower.Inclusive) {
			return false
		}
	}
	if !span.Upper.Unbounded {
		order := prefixOrder(row, span.Upper.Values, layout)
		if order > 0 || (order == 0 && !span.Upper.Inclusive) {
			return false
		}
	}
	return true
}

func assertCoverage(t *testing.T, specification routing.Spec, requested []storage.KeySpan, got []routing.Morsel, row []model.Value) {
	t.Helper()
	want := 0
	for _, span := range requested {
		if contains(row, span, specification.KeyLayout) {
			want = 1
		}
	}
	seen := 0
	for _, morsel := range got {
		if !contains(row, morsel.Span, specification.KeyLayout) {
			continue
		}
		seen++
		matched := false
		for _, partition := range specification.Partitions {
			if partition.ID == morsel.PartitionID && contains(row, partition.Span, specification.KeyLayout) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("row %+v routed to wrong partition %d", row, morsel.PartitionID)
		}
	}
	if seen != want {
		t.Errorf("row %+v covered %d times, want %d", row, seen, want)
	}
}

func TestPrefixCutsWithDescendingNullsAndExtremes(t *testing.T) {
	values := []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(math.MinInt64), model.Int64Val(-1), model.Int64Val(0), model.Int64Val(1), model.Int64Val(math.MaxInt64)}
	for _, descending := range []bool{false, true} {
		for _, nullsFirst := range []bool{false, true} {
			for _, splitAfter := range []bool{false, true} {
				checkPrefixCuts(t, values, descending, nullsFirst, splitAfter)
			}
		}
	}
}

func checkPrefixCuts(t *testing.T, values []model.Value, descending, nullsFirst, splitAfter bool) {
	t.Helper()
	specification := spec()
	for i := range specification.KeyLayout {
		specification.KeyLayout[i].Descending = descending
		specification.KeyLayout[i].NullsFirst = nullsFirst
	}
	specification.Partitions = []routing.Partition{
		{ID: 1, Span: storage.KeySpan{Lower: full().Lower, Upper: bound(splitAfter, 0)}},
		{ID: 2, Span: storage.KeySpan{Lower: bound(!splitAfter, 0), Upper: full().Upper}},
	}
	router := table(t, specification)
	for _, requested := range prefixRequests(values) {
		got, err := router.Route(t.Context(), requested, routing.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		for _, tenant := range values {
			for _, id := range values {
				assertCoverage(t, specification, requested, got, []model.Value{tenant, id})
			}
		}
	}
}

func prefixRequests(values []model.Value) [][]storage.KeySpan {
	requests := [][]storage.KeySpan{{full()}, {{Lower: bound(true, 0), Upper: bound(true, 0)}}}
	for _, inclusive := range []bool{false, true} {
		requests = append(requests,
			[]storage.KeySpan{{Lower: full().Lower, Upper: bound(inclusive, 0)}},
			[]storage.KeySpan{{Lower: bound(inclusive, 0), Upper: full().Upper}},
			[]storage.KeySpan{{Lower: full().Lower, Upper: bound(inclusive, 0)}, {Lower: bound(inclusive, 0), Upper: full().Upper}},
		)
	}
	for _, value := range values {
		endpoint := storage.KeyBound{Values: []model.Value{model.Int64Val(0), value}, Inclusive: true}
		requests = append(requests, []storage.KeySpan{{Lower: endpoint, Upper: endpoint}})
		prefix := storage.KeyBound{Values: []model.Value{value}, Inclusive: true}
		requests = append(requests, []storage.KeySpan{{Lower: prefix, Upper: prefix}})
		for _, inclusive := range []bool{false, true} {
			prefix.Inclusive = inclusive
			requests = append(requests,
				[]storage.KeySpan{{Lower: full().Lower, Upper: prefix}},
				[]storage.KeySpan{{Lower: prefix, Upper: full().Upper}},
			)
		}
	}
	return requests
}

func TestRejectsInvalidCoverageAndIdentity(t *testing.T) {
	cases := []struct {
		name   string
		change func(*routing.Spec)
		want   error
	}{
		{"epoch", func(s *routing.Spec) { s.Epoch = 0 }, routing.ErrInvalidSpec},
		{"generation", func(s *routing.Spec) { s.Target.Generation = 0 }, storage.ErrInvalidTarget},
		{"descriptor", func(s *routing.Spec) { s.Target.DescriptorVersion = 0 }, storage.ErrInvalidTarget},
		{"partition zero", func(s *routing.Spec) { s.Partitions[0].ID = 0 }, routing.ErrInvalidSpec},
		{"duplicate identity", func(s *routing.Spec) { s.Partitions[1].ID = s.Partitions[0].ID }, routing.ErrInvalidSpec},
		{"gap", func(s *routing.Spec) { s.Partitions[1].Span.Lower = bound(true, 1, -1) }, routing.ErrCoverage},
		{"overlap", func(s *routing.Spec) { s.Partitions[0].Span.Upper = bound(true, 1) }, routing.ErrCoverage},
		{"missing lower", func(s *routing.Spec) { s.Partitions[0].Span.Lower = bound(true, -1) }, routing.ErrCoverage},
		{"missing upper", func(s *routing.Spec) { s.Partitions[2].Span.Upper = bound(true, 3) }, routing.ErrCoverage},
		{"reversed partition", func(s *routing.Spec) { s.Partitions[1].Span.Upper = bound(false, 0) }, storage.ErrRangeInvalid},
		{"empty partition", func(s *routing.Spec) { s.Partitions[1].Span.Upper = bound(false, 1) }, storage.ErrRangeInvalid},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			specification := spec()
			test.change(&specification)
			got, err := routing.NewTable(t.Context(), specification, encoder(t), routing.DefaultLimits())
			if got != nil || !errors.Is(err, test.want) {
				t.Fatalf("construction = %v, %v; want nil, %v", got, err, test.want)
			}
		})
	}
}

func TestOwnsConstructionAndResultPayloads(t *testing.T) {
	specification := spec()
	specification.KeyLayout = []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	endpoint := func(value string, inclusive bool) storage.KeyBound {
		return storage.KeyBound{Values: []model.Value{model.BytesVal([]byte(value))}, Inclusive: inclusive}
	}
	specification.Partitions = []routing.Partition{
		{ID: 1, Span: storage.KeySpan{Lower: full().Lower, Upper: endpoint("m", false)}},
		{ID: 2, Span: storage.KeySpan{Lower: endpoint("m", true), Upper: full().Upper}},
	}
	router := table(t, specification)
	specification.Partitions[1].Span.Lower.Values[0].BytesVal[0] = 'z'
	specification.KeyLayout[0].Descending = true
	specification.Target.Generation++
	requested := []storage.KeySpan{{Lower: endpoint("b", true), Upper: endpoint("x", true)}}
	got, err := router.Route(t.Context(), requested, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	requested[0].Lower.Values[0].BytesVal[0] = 'a'
	if len(got) != 2 || string(got[0].Span.Lower.Values[0].BytesVal) != "b" || string(got[1].Span.Lower.Values[0].BytesVal) != "m" || got[0].Target.Generation != 2 {
		t.Fatalf("caller mutation changed routing: %+v", got)
	}
	got[0].Span.Upper.Values[0].BytesVal[0] = 'q'
	if string(got[1].Span.Lower.Values[0].BytesVal) != "m" {
		t.Fatal("adjacent morsels share boundary payloads")
	}
	again, err := router.Route(t.Context(), []storage.KeySpan{full()}, routing.DefaultLimits())
	if err != nil || len(again) != 2 || string(again[0].Span.Upper.Values[0].BytesVal) != "m" {
		t.Fatalf("output mutation changed table: %+v, %v", again, err)
	}
}

var errEncoder = errors.New("encoder comparison failed")

type controlledEncoder struct {
	storage.KeyEncoder
	fail   bool
	cancel context.CancelFunc
}

func (e *controlledEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, limit uint64) ([]byte, error) {
	if e.fail {
		return nil, errEncoder
	}
	result, err := e.KeyEncoder.EncodePrefix(ctx, layout, values, limit)
	if e.cancel != nil {
		e.cancel()
	}
	return result, err
}

func TestCancellationAndEncoderErrorsReturnNoPartialOutput(t *testing.T) {
	codec := &controlledEncoder{KeyEncoder: encoder(t)}
	router, err := routing.NewTable(t.Context(), spec(), codec, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	requested := []storage.KeySpan{full(), {Lower: bound(true, 1), Upper: bound(true, 1)}}
	codec.fail = true
	got, err := router.Route(t.Context(), requested, routing.DefaultLimits())
	if got != nil || !errors.Is(err, errEncoder) {
		t.Fatalf("encoder failure = %+v, %v", got, err)
	}
	codec.fail = false
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	codec.cancel = cancel
	got, err = router.Route(ctx, requested, routing.DefaultLimits())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-call cancellation = %+v, %v", got, err)
	}
	got, err = router.Route(ctx, nil, routing.DefaultLimits())
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled empty route = %+v, %v", got, err)
	}
	constructed, err := routing.NewTable(ctx, spec(), encoder(t), routing.DefaultLimits())
	if constructed != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled construction = %+v, %v", constructed, err)
	}
}

func TestResourceLimitsRejectWholeOperation(t *testing.T) {
	router := table(t, spec())
	requested := []storage.KeySpan{{Lower: bound(true, 1, -2), Upper: bound(true, 1, 2)}, full()}
	cases := []struct {
		name   string
		change func(*routing.Limits)
	}{
		{"nodes", func(l *routing.Limits) { l.MaxNodes = 1 }},
		{"spans", func(l *routing.Limits) { l.MaxSpans = 1 }},
		{"partitions", func(l *routing.Limits) { l.MaxPartitions = 1 }},
		{"work", func(l *routing.Limits) { l.MaxWork = 1 }},
		{"bytes", func(l *routing.Limits) { l.MaxBytes = 1 }},
		{"morsels", func(l *routing.Limits) { l.MaxMorsels = 1 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			limits := routing.DefaultLimits()
			test.change(&limits)
			got, err := router.Route(t.Context(), requested, limits)
			if got != nil || !errors.Is(err, routing.ErrLimit) {
				t.Fatalf("limited route = %+v, %v", got, err)
			}
			if test.name != "morsels" {
				constructed, err := routing.NewTable(t.Context(), spec(), encoder(t), limits)
				if constructed != nil || !errors.Is(err, routing.ErrLimit) {
					t.Fatalf("limited construction = %+v, %v", constructed, err)
				}
			}
		})
	}
}

func TestMalformedRequestedBoundsNeverSilentlyOmitPartitions(t *testing.T) {
	router := table(t, spec())
	cases := []storage.KeySpan{
		{Lower: bound(false, 1), Upper: bound(true, 1)},
		{Lower: bound(true, 2), Upper: bound(true, 1)},
		{Lower: storage.KeyBound{}, Upper: full().Upper},
		{Lower: storage.KeyBound{Unbounded: true, Inclusive: true}, Upper: full().Upper},
		{Lower: bound(true, 1, 2, 3), Upper: full().Upper},
		{Lower: storage.KeyBound{Values: []model.Value{model.StringVal("wrong type")}}, Upper: full().Upper},
	}
	for _, span := range cases {
		got, err := router.Route(t.Context(), []storage.KeySpan{full(), span}, routing.DefaultLimits())
		if got != nil || !errors.Is(err, storage.ErrRangeInvalid) {
			t.Fatalf("invalid span %+v returned %+v, %v", span, got, err)
		}
	}
	cycle := model.Int64Val(1)
	cycle.ArrayVal = make([]model.Value, 1)
	cycle.ArrayVal[0] = cycle
	got, err := router.Route(t.Context(), []storage.KeySpan{{Lower: storage.KeyBound{Values: []model.Value{cycle}}, Upper: full().Upper}}, routing.DefaultLimits())
	if got != nil || !errors.Is(err, routing.ErrLimit) {
		t.Fatalf("cyclic value = %+v, %v", got, err)
	}
}
