package fragment_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/resume"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
)

type continuationStore struct {
	reader  storage.SnapshotReader
	encoder storage.KeyEncoder
	request storage.PushdownRequest
}

func continuationFixture(t *testing.T) *continuationStore {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	epoch := clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}
	limits := neosepia.DefaultLimits()
	limits.Plans, limits.BlockRows = 1, 16
	limits.BlockBytes = 256 << 10
	limits.DecodedBlockBytes = 1 << 20
	limits.ReadBlockBytes = limits.BlockBytes + limits.DecodedBlockBytes
	backend, err := neosepia.Open(t.Context(), neosepia.Config{Directory: t.TempDir(), Scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}, KeyEncoder: encoder, FenceVerifier: authority, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	layout := []storage.ColumnLayout{{ID: 1, Type: integer}, {ID: 2, Type: integer}, {ID: 3, Type: integer}}
	keys := []storage.KeyColumn{{ColumnID: 1, Type: integer}, {ColumnID: 2, Type: integer}}
	batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, Epoch: epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: layout}}}
	for id := int64(1); id <= 100; id++ {
		row := []model.Value{model.Int64Val(1), model.Int64Val(id), model.Int64Val(id * 10)}
		key, err := encoder.EncodePrefix(t.Context(), keys, row[:2], 128)
		if err != nil {
			t.Fatal(err)
		}
		mutation := storage.NewUpsertMutation(target, key, []model.ColumnID{1, 2, 3}, row)
		mutation.Ordinal = uint64(id)
		batch.Mutations = append(batch.Mutations, mutation)
	}
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	newer := batch
	newer.TxID, newer.CommitTS = model.TxnID{2}, 30
	row := []model.Value{model.Int64Val(1), model.Int64Val(50), model.Int64Val(9000)}
	key, err := encoder.EncodePrefix(t.Context(), keys, row[:2], 128)
	if err != nil {
		t.Fatal(err)
	}
	mutation := storage.NewUpsertMutation(target, key, []model.ColumnID{1, 2, 3}, row)
	mutation.Ordinal = 1
	newer.Mutations = []storage.Mutation{mutation}
	newer.ChecksumCRC, err = newer.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	state, err := backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), state.ClosedTS, state.LogPosition, epoch)
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
	request := storage.PushdownRequest{Target: target, Layout: layout, KeyLayout: keys, ColumnIDs: []model.ColumnID{3}, Spans: []storage.KeySpan{{Lower: continuationStoreBound(1, true), Upper: continuationStoreBound(100, true)}}}
	return &continuationStore{reader: reader, encoder: encoder, request: request}
}
func continuationStoreBound(id int64, inclusive bool) storage.KeyBound {
	return storage.KeyBound{Values: []model.Value{model.Int64Val(1), model.Int64Val(id)}, Inclusive: inclusive}
}
func (f *continuationStore) routes(t *testing.T, split bool) *routing.Table {
	t.Helper()
	partitions := []routing.Partition{{ID: 1, Span: storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
	epoch := uint64(1)
	if split {
		epoch = 2
		partitions = []routing.Partition{
			{ID: 99, Span: storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: continuationStoreBound(60, false)}},
			{ID: 41, Span: storage.KeySpan{Lower: continuationStoreBound(60, true), Upper: continuationStoreBound(75, false)}},
			{ID: 7, Span: storage.KeySpan{Lower: continuationStoreBound(75, true), Upper: storage.KeyBound{Unbounded: true}}},
		}
	}
	table, err := routing.NewTable(t.Context(), routing.Spec{Target: f.request.Target, Epoch: epoch, KeyLayout: f.request.KeyLayout, Partitions: partitions}, f.encoder, routing.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return table
}
func continuationQuery(t *testing.T) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(64<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, budget, nil, runtime.QueryConfig{WorkBytes: 32 << 20, OutputBytes: 4 << 20, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	return query
}
func (f *continuationStore) options(t *testing.T, request storage.PushdownRequest, split bool, start *fragment.Continuation) resume.Options {
	t.Helper()
	return resume.Options{Query: continuationQuery(t), Reader: f.reader, Routing: f.routes(t, split), Encoder: f.encoder, Identity: fragment.ScanIdentity{Snapshot: 20, Plan: [32]byte{7}}, Request: request, Start: start, BatchRows: 17, MetadataBytes: 1 << 20, KeyBytes: 4096, WorkBytes: 16 << 20}
}
func continuationValue(t *testing.T, batch *columnar.Batch, row int) int64 {
	t.Helper()
	if len(batch.Columns) != 1 {
		t.Fatalf("hidden key columns escaped: %d columns", len(batch.Columns))
	}
	value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(row), 4096)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull || value.Type != model.TypeInt64 {
		t.Fatalf("unexpected public value: %+v", value)
	}
	return value.IntVal
}
func continuationReleased(t *testing.T, scan *resume.Scan, query *runtime.Query) {
	t.Helper()
	if err := scan.Close(); err != nil {
		t.Fatal(err)
	}
	usage := query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("scan retained resources: %+v", usage)
	}
	if err := query.Close(); err != nil {
		t.Fatal(err)
	}
}
func continuationEvenPredicate(t *testing.T, typ model.Type) storage.Predicate {
	t.Helper()
	nodes := []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 2, Type: typ}}
	constants := make([]model.Value, 50)
	edges := make([]uint32, 50)
	for i := range constants {
		constants[i] = model.Int64Val(int64((i + 1) * 2))
		edges[i] = uint32(len(nodes))
		nodes = append(nodes, storage.PredicateNode{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: uint32(i)})
	}
	nodes = append(nodes, storage.PredicateNode{Kind: storage.PredicateIn, Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Operator: 410, Left: 0, FirstArgument: 0, ArgumentCount: 50})
	predicate, err := storage.NewPredicate(nodes, edges, constants)
	if err != nil {
		t.Fatal(err)
	}
	return predicate
}

func continuationInput(t *testing.T, request storage.PushdownRequest, reverse, filtered bool) (storage.PushdownRequest, []int64) {
	t.Helper()
	if reverse {
		request.Direction = storage.ScanReverse
	}
	if filtered {
		request.Predicate = continuationEvenPredicate(t, request.Layout[1].Type)
	}
	var want []int64
	for id := int64(1); id <= 100; id++ {
		if !filtered || id%2 == 0 {
			want = append(want, id*10)
		}
	}
	if reverse {
		slices.Reverse(want)
	}
	return request, want
}

func TestLocalFragmentContinuation(t *testing.T) {
	for _, sample := range []struct {
		name              string
		reverse, filtered bool
	}{
		{"forward", false, false}, {"reverse", true, false}, {"filtered_forward", false, true}, {"filtered_reverse", true, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			f := continuationFixture(t)
			request, want := continuationInput(t, f.request, sample.reverse, sample.filtered)
			firstOptions := f.options(t, request, false, nil)
			first, err := resume.New(t.Context(), firstOptions)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := first.Close(); err != nil {
					t.Error(err)
				}
			})
			accepted := len(want) * 2 / 5
			var got []int64
			for len(got) < accepted {
				out := columnar.Batch{MaxRows: 17}
				call, cancel := context.WithCancel(t.Context())
				err := first.Next(call, &out)
				cancel()
				if err != nil {
					t.Fatal(err)
				}
				if out.Rows() == 0 {
					continue
				}
				if out.Rows() > out.MaxRows {
					t.Fatal("scan exceeded first-batch demand")
				}
				count := min(out.Rows(), accepted-len(got))
				before := continuationValue(t, &out, 0)
				for row := range count {
					got = append(got, continuationValue(t, &out, row))
				}
				if err := first.Accept(t.Context(), count); err != nil {
					t.Fatal(err)
				}
				if continuationValue(t, &out, 0) != before {
					t.Fatal("acknowledgement invalidated published batch")
				}
				if count < out.Rows() {
					if err := first.Next(t.Context(), &columnar.Batch{MaxRows: 17}); !errors.Is(err, resume.ErrPendingBatch) {
						t.Fatalf("discarded unaccepted rows: %v", err)
					}
				}
			}
			checkpoint := first.Checkpoint()
			continuationReleased(t, first, firstOptions.Query)
			nextOptions := f.options(t, request, true, &checkpoint)
			next, err := resume.New(t.Context(), nextOptions)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := next.Close(); err != nil {
					t.Error(err)
				}
			})
			for {
				out := columnar.Batch{MaxRows: 11}
				err := next.Next(t.Context(), &out)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.Rows() > out.MaxRows {
					t.Fatal("resumed scan exceeded batch demand")
				}
				for row := range out.Rows() {
					got = append(got, continuationValue(t, &out, row))
				}
				if err := next.Accept(t.Context(), out.Rows()); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("accepted then split/resumed rows: %v; want %v", got, want)
			}
			if usage := nextOptions.Query.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 {
				t.Fatalf("EOF retained scan buffers: %+v", usage)
			}
			continuationReleased(t, next, nextOptions.Query)
			key, err := f.encoder.EncodePrefix(t.Context(), request.KeyLayout, []model.Value{model.Int64Val(1), model.Int64Val(50)}, 128)
			if err != nil {
				t.Fatal(err)
			}
			row, err := f.reader.Get(t.Context(), storage.ReadRequest{Target: request.Target, Key: key, ColumnIDs: []model.ColumnID{3}, Layout: request.Layout})
			if err != nil {
				t.Fatalf("scan closed borrowed historical reader: %v", err)
			}
			if len(row.Values) != 1 || row.Values[0].IntVal != 500 {
				t.Fatalf("snapshot changed after resume: %+v", row)
			}
		})
	}
}

func TestLocalContinuationRejectsChangedIdentity(t *testing.T) {
	f := continuationFixture(t)
	options := f.options(t, f.request, false, nil)
	scan, err := resume.New(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scan.Close(); err != nil {
			t.Error(err)
		}
	})
	var batch columnar.Batch
	if err := scan.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if err := scan.Accept(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	token := scan.Checkpoint()
	continuationReleased(t, scan, options.Query)
	for _, sample := range []struct {
		name     string
		identity fragment.ScanIdentity
	}{
		{"snapshot", fragment.ScanIdentity{Snapshot: 21, Plan: [32]byte{7}}},
		{"plan", fragment.ScanIdentity{Snapshot: 20, Plan: [32]byte{8}}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			stale := f.options(t, f.request, true, &token)
			stale.Identity = sample.identity
			rejected, err := resume.New(t.Context(), stale)
			if rejected != nil {
				err = errors.Join(err, rejected.Close())
			}
			if !errors.Is(err, fragment.ErrContinuationMismatch) {
				t.Fatalf("changed identity: %v", err)
			}
			usage := stale.Query.Usage()
			if usage.Work.Live != 0 || usage.Output.Live != 0 {
				t.Fatalf("rejected identity retained buffers: %+v", usage)
			}
			if err := stale.Query.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
