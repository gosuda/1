package neosepia

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func makeGroupCommitReq(t *testing.T, batch storage.MutationBatch) *groupCommitReq {
	t.Helper()
	val, err := validateMutationBatch(t.Context(), batch)
	if err != nil {
		t.Fatalf("validateMutationBatch: %v", err)
	}
	return &groupCommitReq{
		ctx:       t.Context(),
		batch:     batch,
		validated: val,
		done:      make(chan struct{}),
	}
}

func TestGroupCommitDuplicateTxIDPropagatesWALFailure(t *testing.T) {
	t.Run("CleanPhase1WALFailure", func(t *testing.T) {
		fixture := openBackendFixture(t)
		target := encTestTarget()
		batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})

		origWALBytes := fixture.backend.limits.WALBytes
		fixture.backend.mu.Lock()
		fixture.backend.limits.WALBytes = 1
		fixture.backend.mu.Unlock()

		req1 := makeGroupCommitReq(t, batch1)
		req2 := makeGroupCommitReq(t, batch1)
		req3 := makeGroupCommitReq(t, batch1)
		fixture.backend.processCommitGroup([]*groupCommitReq{req1, req2, req3})

		for i, req := range []*groupCommitReq{req1, req2, req3} {
			if !errors.Is(req.err, columnar.ErrBatchBudget) {
				t.Fatalf("req[%d].err = %v, want ErrBatchBudget", i, req.err)
			}
			if errors.Is(req.err, storage.ErrCommitIndeterminate) {
				t.Fatalf("req[%d].err = %v, should not be ErrCommitIndeterminate on clean Phase 1 failure", i, req.err)
			}
			if req.result != (storage.ApplyResult{}) {
				t.Fatalf("req[%d].result = %+v, want zero ApplyResult", i, req.result)
			}
		}

		st, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if st.LogPosition != 0 || st.CommitTS != 0 {
			t.Fatalf("state after clean WAL failure = %+v, want pos=0 ts=0", st)
		}

		// Restoring WALBytes allows the same TxID to commit cleanly at pos 1.
		fixture.backend.mu.Lock()
		fixture.backend.limits.WALBytes = origWALBytes
		fixture.backend.mu.Unlock()

		res, err := fixture.backend.ApplyMutations(t.Context(), batch1)
		if err != nil {
			t.Fatalf("retry ApplyMutations after clean WAL failure: %v", err)
		}
		if res.LogPosition != 1 || res.CommitTS != 10 {
			t.Fatalf("retry result = %+v, want pos=1 ts=10", res)
		}
	})

	t.Run("PartialLinkIndeterminateWALFailure", func(t *testing.T) {
		fixture := openBackendFixture(t)
		target := encTestTarget()
		batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})
		batch2 := makeIntRowsBatch(t, fixture, 2, 20, target, []int64{2}, []int64{202})

		// Block hardlink for position 2 so pos 1 links (linked == 1) and pos 2 fails.
		blocker := filepath.Join(fixture.config.Directory, "commits", commitName(2))
		if err := os.WriteFile(blocker, nil, 0600); err != nil {
			t.Fatalf("create blocker: %v", err)
		}

		req1a := makeGroupCommitReq(t, batch1)
		req1b := makeGroupCommitReq(t, batch1)
		req2a := makeGroupCommitReq(t, batch2)
		req2b := makeGroupCommitReq(t, batch2)
		fixture.backend.processCommitGroup([]*groupCommitReq{req1a, req1b, req2a, req2b})

		if err := os.Remove(blocker); err != nil {
			t.Fatalf("remove blocker: %v", err)
		}

		for i, req := range []*groupCommitReq{req1a, req1b, req2a, req2b} {
			if !errors.Is(req.err, storage.ErrCommitIndeterminate) {
				t.Fatalf("req[%d].err = %v, want ErrCommitIndeterminate", i, req.err)
			}
			if req.result != (storage.ApplyResult{}) {
				t.Fatalf("req[%d].result = %+v, want zero ApplyResult", i, req.result)
			}
		}
	})
}

func TestGroupCommitDuplicateTxIDPropagatesIndeterminateOnStageFailure(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()
	strLayout := []storage.ColumnLayout{
		{ID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: predCollC}},
	}

	makeStrBatch := func(txNum uint64, ts model.Timestamp, k int64, payload string) storage.MutationBatch {
		t.Helper()
		b := storage.MutationBatch{
			TxID:            model.TxnID{byte(txNum)},
			CommitTS:        ts,
			Epoch:           fixture.epoch,
			EncodingVersion: storage.MutationEncodingVersion,
			Layouts:         []storage.ObjectLayout{{Target: target, Columns: strLayout}},
			Mutations: []storage.Mutation{
				storage.NewUpsertMutation(target, backendKey(k), []model.ColumnID{1}, []model.Value{model.StringVal(payload)}),
			},
		}
		b.Mutations[0].Ordinal = 1
		var err error
		b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Tx1 fits within RowBytes=32; Tx2 (128 bytes) passes WAL write but fails
	// post-WAL memtable.stageBatch with ErrValueBudget; Tx3 trails Tx2.
	batch1 := makeStrBatch(1, 10, 1, "ok")
	batch2 := makeStrBatch(2, 20, 2, string(bytes.Repeat([]byte("x"), 128)))
	batch3 := makeStrBatch(3, 30, 3, "tail")

	origRowBytes := fixture.backend.limits.RowBytes
	fixture.backend.mu.Lock()
	fixture.backend.limits.RowBytes = 32
	fixture.backend.mu.Unlock()

	req1a := makeGroupCommitReq(t, batch1)
	req1b := makeGroupCommitReq(t, batch1)
	req2a := makeGroupCommitReq(t, batch2)
	req2b := makeGroupCommitReq(t, batch2)
	req3a := makeGroupCommitReq(t, batch3)
	req3b := makeGroupCommitReq(t, batch3)

	fixture.backend.processCommitGroup([]*groupCommitReq{req1a, req1b, req2a, req2b, req3a, req3b})

	if req1a.err != nil || req1a.result.LogPosition != 1 {
		t.Fatalf("req1a = (res=%+v, err=%v), want pos=1 nil err", req1a.result, req1a.err)
	}
	if req1b.err != nil || req1b.result != req1a.result {
		t.Fatalf("req1b = (res=%+v, err=%v), want identical result %+v and nil err", req1b.result, req1b.err, req1a.result)
	}

	for i, req := range []*groupCommitReq{req2a, req2b, req3a, req3b} {
		if !errors.Is(req.err, storage.ErrCommitIndeterminate) || !errors.Is(req.err, model.ErrValueBudget) {
			t.Fatalf("failed req[%d].err = %v, want ErrCommitIndeterminate + ErrValueBudget", i, req.err)
		}
		if req.result != (storage.ApplyResult{}) {
			t.Fatalf("failed req[%d].result = %+v, want zero ApplyResult", i, req.result)
		}
	}

	// Restoring RowBytes allows ResolveCommit to recover durable Tx2 and Tx3.
	fixture.backend.mu.Lock()
	fixture.backend.limits.RowBytes = origRowBytes
	fixture.backend.mu.Unlock()

	res2, err := fixture.backend.ResolveCommit(t.Context(), batch2.TxID)
	if err != nil {
		t.Fatalf("ResolveCommit(Tx2): %v", err)
	}
	if res2.State != storage.CommitCommitted || res2.Result.LogPosition != 2 {
		t.Fatalf("ResolveCommit(Tx2) = %+v, want CommitCommitted at pos 2", res2)
	}
	res3, err := fixture.backend.ResolveCommit(t.Context(), batch3.TxID)
	if err != nil {
		t.Fatalf("ResolveCommit(Tx3): %v", err)
	}
	if res3.State != storage.CommitCommitted || res3.Result.LogPosition != 3 {
		t.Fatalf("ResolveCommit(Tx3) = %+v, want CommitCommitted at pos 3", res3)
	}
}

func TestGroupCommitDuplicateTxIDSucceedsTogether(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})
	batch2 := makeIntRowsBatch(t, fixture, 2, 20, target, []int64{2}, []int64{202})

	req1a := makeGroupCommitReq(t, batch1)
	req1b := makeGroupCommitReq(t, batch1)
	req1c := makeGroupCommitReq(t, batch1)
	req2a := makeGroupCommitReq(t, batch2)
	req2b := makeGroupCommitReq(t, batch2)

	fixture.backend.processCommitGroup([]*groupCommitReq{req1a, req1b, req1c, req2a, req2b})

	for i, req := range []*groupCommitReq{req1a, req1b, req1c} {
		if req.err != nil {
			t.Fatalf("req1[%d].err = %v, want nil", i, req.err)
		}
		if req.result.LogPosition != 1 || req.result.CommitTS != 10 {
			t.Fatalf("req1[%d].result = %+v, want pos=1 ts=10", i, req.result)
		}
		if req.result != req1a.result {
			t.Fatalf("req1[%d].result = %+v, want %+v", i, req.result, req1a.result)
		}
	}
	for i, req := range []*groupCommitReq{req2a, req2b} {
		if req.err != nil {
			t.Fatalf("req2[%d].err = %v, want nil", i, req.err)
		}
		if req.result.LogPosition != 2 || req.result.CommitTS != 20 {
			t.Fatalf("req2[%d].result = %+v, want pos=2 ts=20", i, req.result)
		}
		if req.result != req2a.result {
			t.Fatalf("req2[%d].result = %+v, want %+v", i, req.result, req2a.result)
		}
	}

	st, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.LogPosition != 2 || st.CommitTS != 20 {
		t.Fatalf("state = %+v, want pos=2 ts=20", st)
	}
	if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(3))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no WAL at pos 3, stat err = %v", err)
	}

	reader := fixture.snapshot(t, 20)
	defer func() {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if got := readIntKey(t, reader, target, 1); got != 101 {
		t.Fatalf("key 1 = %d, want 101", got)
	}
	if got := readIntKey(t, reader, target, 2); got != 202 {
		t.Fatalf("key 2 = %d, want 202", got)
	}
}

func TestGroupCommitDuplicateTxIDMismatchRejected(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})
	batch1Different := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{999})

	req1 := makeGroupCommitReq(t, batch1)
	reqMismatch := makeGroupCommitReq(t, batch1Different)
	reqDup := makeGroupCommitReq(t, batch1)

	fixture.backend.processCommitGroup([]*groupCommitReq{req1, reqMismatch, reqDup})

	if req1.err != nil || req1.result.LogPosition != 1 {
		t.Fatalf("req1 = (res=%+v, err=%v), want pos=1 nil err", req1.result, req1.err)
	}
	if reqDup.err != nil || reqDup.result != req1.result {
		t.Fatalf("reqDup = (res=%+v, err=%v), want identical result %+v and nil err", reqDup.result, reqDup.err, req1.result)
	}
	if !errors.Is(reqMismatch.err, storage.ErrTxnIDReused) {
		t.Fatalf("reqMismatch.err = %v, want ErrTxnIDReused", reqMismatch.err)
	}
	if reqMismatch.result != (storage.ApplyResult{}) {
		t.Fatalf("reqMismatch.result = %+v, want zero ApplyResult", reqMismatch.result)
	}

	reader := fixture.snapshot(t, 10)
	defer func() {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if got := readIntKey(t, reader, target, 1); got != 101 {
		t.Fatalf("key 1 = %d, want 101 (primary batch)", got)
	}
}

func TestGroupCommitAlreadyDurableRetrySurvivesSiblingWALFailure(t *testing.T) {
	fixture := openBackendFixture(t)
	target := encTestTarget()

	batch1 := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{101})
	res1, err := fixture.backend.ApplyMutations(t.Context(), batch1)
	if err != nil {
		t.Fatalf("ApplyMutations(batch1): %v", err)
	}
	if res1.LogPosition != 1 {
		t.Fatalf("batch1 LogPosition = %d, want 1", res1.LogPosition)
	}

	batch1Mismatch := makeIntRowsBatch(t, fixture, 1, 10, target, []int64{1}, []int64{777})
	batch2 := makeIntRowsBatch(t, fixture, 2, 20, target, []int64{2}, []int64{202})

	// Force Phase 1 WAL failure for the new sibling batch2.
	fixture.backend.mu.Lock()
	fixture.backend.limits.WALBytes = 1
	fixture.backend.mu.Unlock()

	reqRetryDurable := makeGroupCommitReq(t, batch1)
	reqRetryMismatch := makeGroupCommitReq(t, batch1Mismatch)
	reqSiblingA := makeGroupCommitReq(t, batch2)
	reqSiblingB := makeGroupCommitReq(t, batch2)

	fixture.backend.processCommitGroup([]*groupCommitReq{reqRetryDurable, reqRetryMismatch, reqSiblingA, reqSiblingB})

	if reqRetryDurable.err != nil || reqRetryDurable.result != res1 {
		t.Fatalf("reqRetryDurable = (res=%+v, err=%v), want durable %+v nil err", reqRetryDurable.result, reqRetryDurable.err, res1)
	}
	if !errors.Is(reqRetryMismatch.err, storage.ErrTxnIDReused) {
		t.Fatalf("reqRetryMismatch.err = %v, want ErrTxnIDReused", reqRetryMismatch.err)
	}
	if reqRetryMismatch.result != (storage.ApplyResult{}) {
		t.Fatalf("reqRetryMismatch.result = %+v, want zero ApplyResult", reqRetryMismatch.result)
	}
	for i, req := range []*groupCommitReq{reqSiblingA, reqSiblingB} {
		if !errors.Is(req.err, columnar.ErrBatchBudget) {
			t.Fatalf("sibling[%d].err = %v, want ErrBatchBudget", i, req.err)
		}
		if req.result != (storage.ApplyResult{}) {
			t.Fatalf("sibling[%d].result = %+v, want zero ApplyResult", i, req.result)
		}
	}

	st, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.LogPosition != 1 || st.CommitTS != 10 {
		t.Fatalf("state after sibling WAL failure = %+v, want pos=1 ts=10", st)
	}
}
