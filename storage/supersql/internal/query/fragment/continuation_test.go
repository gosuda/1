package fragment_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
)

func continuationIdentity() fragment.ScanIdentity {
	return fragment.ScanIdentity{Snapshot: 71, Plan: [32]byte{1}}
}

func continuationEncoder(t *testing.T) storage.KeyEncoder {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return encoder
}

func continuationFull() storage.KeySpan {
	return storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
}

func continuationBound(inclusive bool, values ...model.Value) storage.KeyBound {
	return storage.KeyBound{Values: values, Inclusive: inclusive}
}

func continuationRequest(types ...model.Type) storage.PushdownRequest {
	if len(types) == 0 {
		types = []model.Type{{ID: 20, Storage: model.TypeInt64}}
	}
	request := storage.PushdownRequest{
		Target: storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1},
		Spans:  []storage.KeySpan{continuationFull()},
	}
	for i, typ := range types {
		id := model.ColumnID(i + 1)
		request.Layout = append(request.Layout, storage.ColumnLayout{ID: id, Type: typ, Nullable: true})
		request.KeyLayout = append(request.KeyLayout, storage.KeyColumn{ColumnID: id, Type: typ})
		request.ColumnIDs = append(request.ColumnIDs, id)
	}
	return request
}

func continuationTable(t *testing.T, request storage.PushdownRequest, epoch uint64, partitions ...routing.Partition) *routing.Table {
	t.Helper()
	if len(partitions) == 0 {
		partitions = []routing.Partition{{ID: 1, Span: continuationFull()}}
	}
	table, err := routing.NewTable(t.Context(), routing.Spec{Target: request.Target, Epoch: epoch, KeyLayout: request.KeyLayout, Partitions: partitions}, continuationEncoder(t), routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func continuationStart(t *testing.T, request storage.PushdownRequest) fragment.Continuation {
	t.Helper()
	token, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, continuationEncoder(t), routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func continuationAdvance(t *testing.T, token fragment.Continuation, key ...model.Value) fragment.Continuation {
	t.Helper()
	next, err := token.Advance(t.Context(), key, continuationEncoder(t), routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return next
}

func continuationResume(t *testing.T, token fragment.Continuation, request storage.PushdownRequest, table *routing.Table) []routing.Morsel {
	t.Helper()
	morsels, err := token.Resume(t.Context(), continuationIdentity(), request, table, continuationEncoder(t), routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return morsels
}

func assertContinuationMorsels(t *testing.T, got []routing.Morsel, epoch uint64, ids []uint64, spans []storage.KeySpan) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("morsels = %+v, want partitions %v", got, ids)
	}
	for i, morsel := range got {
		if morsel.Epoch != epoch || morsel.PartitionID != ids[i] || !reflect.DeepEqual(morsel.Span, spans[i]) {
			t.Errorf("morsel %d = %+v; want epoch %d, partition %d, span %+v", i, morsel, epoch, ids[i], spans[i])
		}
	}
}

func TestContinuationResolvesExclusiveRestartAfterSplit(t *testing.T) {
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		t.Run(map[storage.ScanDirection]string{storage.ScanForward: "forward", storage.ScanReverse: "reverse"}[direction], func(t *testing.T) {
			request := continuationRequest()
			request.Direction = direction
			b := func(inclusive bool, n int64) storage.KeyBound { return continuationBound(inclusive, model.Int64Val(n)) }
			request.Spans = []storage.KeySpan{{Lower: b(true, 1), Upper: b(true, 4)}, {Lower: b(true, 7), Upper: b(false, 10)}}
			fresh := continuationStart(t, request)
			old := continuationTable(t, request, 1)
			assertContinuationMorsels(t, continuationResume(t, fresh, request, old), 1, []uint64{1, 1}, func() []storage.KeySpan {
				if direction == storage.ScanReverse {
					return []storage.KeySpan{request.Spans[1], request.Spans[0]}
				}
				return request.Spans
			}())
			partitions := []routing.Partition{
				{ID: 30, Span: storage.KeySpan{Lower: continuationFull().Lower, Upper: b(false, 3)}},
				{ID: 40, Span: storage.KeySpan{Lower: b(true, 3), Upper: b(false, 8)}},
				{ID: 50, Span: storage.KeySpan{Lower: b(true, 8), Upper: continuationFull().Upper}},
			}
			split := continuationTable(t, request, 2, partitions...)
			accepted := int64(2)
			if direction == storage.ScanReverse {
				accepted = 8
			}
			token := continuationAdvance(t, fresh, model.Int64Val(accepted))
			if direction == storage.ScanForward {
				assertContinuationMorsels(t, continuationResume(t, token, request, split), 2, []uint64{30, 40, 40, 50}, []storage.KeySpan{
					{Lower: b(false, 2), Upper: b(false, 3)}, {Lower: b(true, 3), Upper: b(true, 4)},
					{Lower: b(true, 7), Upper: b(false, 8)}, {Lower: b(true, 8), Upper: b(false, 10)},
				})
			} else {
				assertContinuationMorsels(t, continuationResume(t, token, request, split), 2, []uint64{40, 40, 30}, []storage.KeySpan{
					{Lower: b(true, 7), Upper: b(false, 8)}, {Lower: b(true, 3), Upper: b(true, 4)}, {Lower: b(true, 1), Upper: b(false, 3)},
				})
			}
			if _, err := token.Advance(t.Context(), []model.Value{model.Int64Val(5)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationOutsideSpan) {
				t.Fatalf("acknowledging gap: %v", err)
			}
		})
	}
}

func TestContinuationCompositeDescendingNullKeys(t *testing.T) {
	integer := model.Type{ID: 20, Storage: model.TypeInt64}
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		request := continuationRequest(integer, integer)
		request.Direction = direction
		request.KeyLayout[1].Descending = true
		request.KeyLayout[1].NullsFirst = true
		prefix := continuationBound(true, model.Int64Val(4))
		request.Spans = []storage.KeySpan{{Lower: prefix, Upper: prefix}}
		table := continuationTable(t, request, 1)
		keys := [][]model.Value{{model.Int64Val(4), model.NullVal(model.TypeInt64)}, {model.Int64Val(4), model.Int64Val(9)}, {model.Int64Val(4), model.Int64Val(2)}}
		if direction == storage.ScanReverse {
			keys[0], keys[2] = keys[2], keys[0]
		}
		token := continuationStart(t, request)
		for _, key := range keys {
			token = continuationAdvance(t, token, key...)
			want := storage.KeySpan{Lower: continuationBound(false, key...), Upper: prefix}
			if direction == storage.ScanReverse {
				want = storage.KeySpan{Lower: prefix, Upper: continuationBound(false, key...)}
			}
			assertContinuationMorsels(t, continuationResume(t, token, request, table), 1, []uint64{1}, []storage.KeySpan{want})
			if _, err := token.Advance(t.Context(), key, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationOrder) {
				t.Fatalf("duplicate composite key: %v", err)
			}
		}
		if _, err := token.Advance(t.Context(), []model.Value{model.Int64Val(4)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, storage.ErrRangeInvalid) {
			t.Fatalf("partial key: %v", err)
		}
	}
}

func TestContinuationNumericEqualityClasses(t *testing.T) {
	for _, pair := range [][2]string{{"1.00", "1e0"}, {"-0", "0.000"}, {"NaN", "nan"}, {"-Infinity", "-inf"}} {
		t.Run(pair[0], func(t *testing.T) {
			request := continuationRequest(model.Type{ID: 1700, Storage: model.TypeNumeric})
			first := model.Value{Type: model.TypeNumeric, StrVal: pair[0]}
			equal := model.Value{Type: model.TypeNumeric, StrVal: pair[1]}
			point := continuationBound(true, first)
			request.Spans = []storage.KeySpan{{Lower: point, Upper: point}}
			token := continuationAdvance(t, continuationStart(t, request), equal)
			if _, err := token.Advance(t.Context(), []model.Value{first}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationOrder) {
				t.Fatalf("equivalent numeric key advanced: %v", err)
			}
			assertContinuationMorsels(t, continuationResume(t, token, request, continuationTable(t, request, 1)), 1, nil, nil)
		})
	}
}

func TestContinuationRejectsChangedLogicalIdentity(t *testing.T) {
	changes := []struct {
		name   string
		change func(*storage.PushdownRequest, *fragment.ScanIdentity)
	}{
		{"snapshot", func(_ *storage.PushdownRequest, id *fragment.ScanIdentity) { id.Snapshot++ }},
		{"plan", func(_ *storage.PushdownRequest, id *fragment.ScanIdentity) { id.Plan[2]++ }},
		{"target", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Target.ObjectID++ }},
		{"generation", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Target.Generation++ }},
		{"descriptor", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Target.DescriptorVersion++ }},
		{"encoding", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Target.EncodingVersion++ }},
		{"index", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.IndexID++ }},
		{"layout", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Layout[0].Nullable = false }},
		{"key direction", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.KeyLayout[0].Descending = true }},
		{"key null order", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.KeyLayout[0].NullsFirst = true }},
		{"projection", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.ColumnIDs = []model.ColumnID{1, 1} }},
		{"direction", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) { r.Direction = storage.ScanReverse }},
		{"spans", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) {
			r.Spans[0].Upper = continuationBound(false, model.Int64Val(99))
		}},
		{"limit", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) {
			r.SafeLimit = storage.ScanLimit{Enabled: true, Count: 5}
		}},
		{"predicate", func(r *storage.PushdownRequest, _ *fragment.ScanIdentity) {
			p, err := storage.NewPredicate([]storage.PredicateNode{{Kind: storage.PredicateConstant, Type: model.Type{ID: 16, Storage: model.TypeBoolean}}}, nil, []model.Value{model.BoolVal(false)})
			if err != nil {
				t.Fatal(err)
			}
			r.Predicate = p
		}},
	}
	for _, test := range changes {
		t.Run(test.name, func(t *testing.T) {
			request := continuationRequest()
			token := continuationAdvance(t, continuationStart(t, request), model.Int64Val(3))
			table := continuationTable(t, request, 1)
			identity := continuationIdentity()
			test.change(&request, &identity)
			got, err := token.Resume(t.Context(), identity, request, table, continuationEncoder(t), routing.DefaultLimits())
			if !errors.Is(err, fragment.ErrContinuationMismatch) || got != nil {
				t.Fatalf("stale %s: %+v, %v", test.name, got, err)
			}
		})
	}
}

func TestContinuationOwnsRequestAndAcceptedKeys(t *testing.T) {
	request := continuationRequest(model.Type{ID: 17, Storage: model.TypeBytes})
	request.Spans[0].Lower = continuationBound(true, model.BytesVal([]byte("a")))
	original, _, err := storage.CopyPushdownRequest(t.Context(), request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	table := continuationTable(t, request, 1)
	fresh := continuationStart(t, request)
	key := model.BytesVal([]byte("b"))
	first := continuationAdvance(t, fresh, key)
	second := continuationAdvance(t, first, model.BytesVal([]byte("c")))
	request.Spans[0].Lower.Values[0].BytesVal[0] = 'z'
	request.KeyLayout[0].Descending = true
	request.Layout[0].Nullable = false
	request.ColumnIDs[0] = 99
	key.BytesVal[0] = 'z'
	for _, test := range []struct {
		token     fragment.Continuation
		lower     string
		inclusive bool
	}{{fresh, "a", true}, {first, "b", false}, {second, "c", false}} {
		got := continuationResume(t, test.token, original, table)
		assertContinuationMorsels(t, got, 1, []uint64{1}, []storage.KeySpan{{Lower: continuationBound(test.inclusive, model.BytesVal([]byte(test.lower))), Upper: continuationFull().Upper}})
		got[0].Span.Lower.Values[0].BytesVal[0] = 'x'
		again := continuationResume(t, test.token, original, table)
		if string(again[0].Span.Lower.Values[0].BytesVal) != test.lower {
			t.Fatal("morsel mutation changed token")
		}
	}
}

func TestContinuationInvalidTransitionsReturnNoToken(t *testing.T) {
	request := continuationRequest()
	request.Spans[0] = storage.KeySpan{Lower: continuationBound(false, model.Int64Val(1)), Upper: continuationBound(false, model.Int64Val(9))}
	token := continuationAdvance(t, continuationStart(t, request), model.Int64Val(5))
	cases := []struct {
		name string
		key  []model.Value
		want error
	}{
		{"duplicate", []model.Value{model.Int64Val(5)}, fragment.ErrContinuationOrder},
		{"backwards", []model.Value{model.Int64Val(4)}, fragment.ErrContinuationOrder},
		{"exclusive lower", []model.Value{model.Int64Val(1)}, fragment.ErrContinuationOutsideSpan},
		{"exclusive upper", []model.Value{model.Int64Val(9)}, fragment.ErrContinuationOutsideSpan},
		{"empty", nil, storage.ErrRangeInvalid},
		{"extra", []model.Value{model.Int64Val(6), model.Int64Val(7)}, storage.ErrRangeInvalid},
		{"wrong type", []model.Value{model.BytesVal([]byte("x"))}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			failed, err := token.Advance(t.Context(), test.key, continuationEncoder(t), routing.DefaultLimits())
			if err == nil || (test.want != nil && !errors.Is(err, test.want)) {
				t.Fatalf("advance error = %v, want %v", err, test.want)
			}
			if _, err := failed.Advance(t.Context(), []model.Value{model.Int64Val(6)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationMismatch) {
				t.Fatalf("failure exposed usable token: %v", err)
			}
		})
	}
	assertContinuationMorsels(t, continuationResume(t, token, request, continuationTable(t, request, 1)), 1, []uint64{1}, []storage.KeySpan{{Lower: continuationBound(false, model.Int64Val(5)), Upper: request.Spans[0].Upper}})
}

func TestContinuationBudgetsAndCancellation(t *testing.T) {
	request := continuationRequest()
	encoder := continuationEncoder(t)
	token := continuationStart(t, request)
	table := continuationTable(t, request, 1)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name   string
		ctx    context.Context
		limits routing.Limits
	}{
		{"canceled", canceled, routing.DefaultLimits()},
		{"nil context", nil, routing.DefaultLimits()},
		{"zero budget", t.Context(), routing.Limits{}},
		{"metadata bytes", t.Context(), func() routing.Limits { l := routing.DefaultLimits(); l.MaxBytes = 128; return l }()},
		{"node budget", t.Context(), func() routing.Limits { l := routing.DefaultLimits(); l.MaxNodes = 3; return l }()},
		{"work budget", t.Context(), func() routing.Limits { l := routing.DefaultLimits(); l.MaxWork = 3; return l }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := fragment.NewContinuation(test.ctx, continuationIdentity(), request, encoder, test.limits); err == nil {
				t.Fatal("new accepted invalid boundary")
			}
			if _, err := token.Advance(test.ctx, []model.Value{model.Int64Val(2)}, encoder, test.limits); err == nil {
				t.Fatal("advance accepted invalid boundary")
			}
			got, err := token.Resume(test.ctx, continuationIdentity(), request, table, encoder, test.limits)
			if err == nil || got != nil {
				t.Fatalf("resume returned %+v, %v", got, err)
			}
			if test.name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
	for _, identity := range []fragment.ScanIdentity{{}, {Snapshot: -1, Plan: [32]byte{1}}, {Snapshot: 1}} {
		if _, err := fragment.NewContinuation(t.Context(), identity, request, encoder, routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationMismatch) {
			t.Fatalf("invalid identity: %v", err)
		}
	}
	if _, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, nil, routing.DefaultLimits()); err == nil {
		t.Fatal("nil encoder accepted")
	}
	if _, err := token.Resume(t.Context(), continuationIdentity(), request, nil, encoder, routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationMismatch) {
		t.Fatalf("nil table: %v", err)
	}
	if _, err := (fragment.Continuation{}).Resume(t.Context(), continuationIdentity(), request, table, encoder, routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationMismatch) {
		t.Fatalf("zero token: %v", err)
	}
	bytesRequest := continuationRequest(model.Type{ID: 17, Storage: model.TypeBytes})
	bytesToken := continuationStart(t, bytesRequest)
	small := routing.DefaultLimits()
	small.MaxBytes = 4096
	if _, err := bytesToken.Advance(t.Context(), []model.Value{model.BytesVal(make([]byte, 8192))}, encoder, small); err == nil {
		t.Fatal("oversized key accepted")
	}
	cyclic := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: make([]model.TypeLayout, 1)}
	cyclic.Children[0] = cyclic
	request.Layout[0].Type, request.Layout[0].Children = cyclic.Type, cyclic.Children
	if _, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, encoder, routing.DefaultLimits()); !errors.Is(err, model.ErrInvalidTypeLayout) {
		t.Fatalf("cyclic type metadata: %v", err)
	}
}

func TestContinuationEmptyByteKeyIsNotUnbounded(t *testing.T) {
	request := continuationRequest(model.Type{ID: 17, Storage: model.TypeBytes})
	token := continuationAdvance(t, continuationStart(t, request), model.BytesVal([]byte{}))
	assertContinuationMorsels(t, continuationResume(t, token, request, continuationTable(t, request, 1)), 1, []uint64{1}, []storage.KeySpan{
		{Lower: continuationBound(false, model.BytesVal([]byte{})), Upper: continuationFull().Upper},
	})
	if _, err := token.Advance(t.Context(), []model.Value{model.BytesVal(nil)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationOrder) {
		t.Fatalf("equivalent empty key advanced: %v", err)
	}
	next := continuationAdvance(t, token, model.BytesVal([]byte{0}))
	assertContinuationMorsels(t, continuationResume(t, next, request, continuationTable(t, request, 1)), 1, []uint64{1}, []storage.KeySpan{
		{Lower: continuationBound(false, model.BytesVal([]byte{0})), Upper: continuationFull().Upper},
	})
}

func TestContinuationRejectsUnsupportedKeyTypesBeforeAnyRoute(t *testing.T) {
	request := continuationRequest(model.Type{ID: 20, Storage: model.TypeInt64})
	request.KeyLayout[0].Type.ID = 0
	if _, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, storage.ErrInvalidLayout) {
		t.Fatalf("invalid key type: %v", err)
	}
	request = continuationRequest(model.Type{ID: 25, Storage: model.TypeString, Collation: 999})
	if _, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, continuationEncoder(t), routing.DefaultLimits()); err == nil {
		t.Fatal("unavailable collation accepted for unbounded scan")
	}
}

type cancelContinuationEncoder struct {
	storage.KeyEncoder
	cancel context.CancelFunc
}

func (e cancelContinuationEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maximum uint64) ([]byte, error) {
	encoded, err := e.KeyEncoder.EncodePrefix(ctx, layout, values, maximum)
	e.cancel()
	return encoded, err
}

func TestContinuationCancellationDuringKeyValidationDoesNotAdvance(t *testing.T) {
	request := continuationRequest()
	token := continuationAdvance(t, continuationStart(t, request), model.Int64Val(2))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failed, err := token.Advance(ctx, []model.Value{model.Int64Val(3)}, cancelContinuationEncoder{KeyEncoder: continuationEncoder(t), cancel: cancel}, routing.DefaultLimits())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("advance during cancellation: %v", err)
	}
	if _, err := failed.Advance(t.Context(), []model.Value{model.Int64Val(4)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationMismatch) {
		t.Fatalf("canceled operation exposed token: %v", err)
	}
	assertContinuationMorsels(t, continuationResume(t, token, request, continuationTable(t, request, 1)), 1, []uint64{1}, []storage.KeySpan{
		{Lower: continuationBound(false, model.Int64Val(2)), Upper: continuationFull().Upper},
	})
}

func TestContinuationMetadataBudgetIncludesUnprojectedTypes(t *testing.T) {
	request := continuationRequest()
	for i := 2; i <= 100; i++ {
		request.Layout = append(request.Layout, storage.ColumnLayout{ID: model.ColumnID(i), Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	}
	limits := routing.DefaultLimits()
	limits.MaxBytes = 16 << 10
	if _, err := fragment.NewContinuation(t.Context(), continuationIdentity(), request, continuationEncoder(t), limits); !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("unprojected metadata bypassed budget: %v", err)
	}
}

func TestContinuationEmptyRequestHasNoAcknowledgements(t *testing.T) {
	request := continuationRequest()
	request.Spans = nil
	token := continuationStart(t, request)
	assertContinuationMorsels(t, continuationResume(t, token, request, continuationTable(t, request, 1)), 1, nil, nil)
	if _, err := token.Advance(t.Context(), []model.Value{model.Int64Val(1)}, continuationEncoder(t), routing.DefaultLimits()); !errors.Is(err, fragment.ErrContinuationOutsideSpan) {
		t.Fatalf("acknowledgement in empty scan: %v", err)
	}
}
