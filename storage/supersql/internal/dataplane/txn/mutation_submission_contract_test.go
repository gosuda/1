package txn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The capture owns canonical bytes, never the transaction's sealed sequence.
type mutationBatchCapture struct {
	storage.Backend
	wire bytes.Buffer
}

func (capture *mutationBatchCapture) Write(p []byte) (int, error) {
	if len(p) > (64<<10)-capture.wire.Len() {
		return 0, storage.ErrDecodeBudget
	}
	return capture.wire.Write(p)
}

func (capture *mutationBatchCapture) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	capture.wire.Reset()
	if err := storage.EncodeMutationBatch(ctx, capture, batch); err != nil {
		return storage.ApplyResult{}, err
	}
	return capture.Backend.ApplyMutations(ctx, batch)
}

func mutationDecodeBatch(t *testing.T, wire []byte) storage.MutationBatch {
	t.Helper()
	batch, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(wire), uint64(len(wire)), storage.BatchDecodeLimits{MaxBytes: 64 << 10, MaxAllocation: 64 << 10, MaxMutationBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func mutationDurableFiles(t *testing.T, directory string) map[string][32]byte {
	t.Helper()
	files := make(map[string][32]byte)
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		files[relative] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func mutationDurableRetry(t *testing.T) {
	var capture *mutationBatchCapture
	fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
		capture = &mutationBatchCapture{Backend: backend}
		return capture
	})
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	committed := contractCommit(t, transaction)
	batch := mutationDecodeBatch(t, capture.wire.Bytes())
	if batch.TxID != transaction.ID() {
		t.Fatalf("captured TxID = %x, want %x", batch.TxID, transaction.ID())
	}

	cursor, err := batch.OpenMutations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	mutation, nextErr := cursor.Next(t.Context())
	changedMutation := storage.NewUpsertMutation(mutation.Target, mutation.Key, mutation.ColumnIDs, []model.Value{model.Int64Val(80)})
	changedMutation.Ordinal = mutation.Ordinal
	_, endErr := cursor.Next(t.Context())
	closeErr := cursor.Close()
	if nextErr != nil || endErr != io.EOF || closeErr != nil {
		t.Fatalf("captured single mutation = %v, %v, %v", nextErr, endErr, closeErr)
	}
	changed := batch
	changed.Sequence = nil
	changed.Mutations = []storage.Mutation{changedMutation}
	changed.ChecksumCRC, err = changed.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	for _, phase := range []string{"live", "reopened"} {
		t.Run(phase, func(t *testing.T) {
			before, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			files := mutationDurableFiles(t, fixture.config.Directory)
			result, err := fixture.backend.ApplyMutations(t.Context(), batch)
			if err != nil || result != committed {
				t.Fatalf("identical retry of %x = %+v, %v; want %+v", batch.TxID, result, err, committed)
			}
			result, err = fixture.backend.ApplyMutations(t.Context(), changed)
			if !errors.Is(err, storage.ErrTxnIDReused) || result != (storage.ApplyResult{}) {
				t.Fatalf("different bytes for %x = %+v, %v; want ErrTxnIDReused without a result", batch.TxID, result, err)
			}
			after, err := fixture.backend.State(t.Context())
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("retry changed backend state: before %+v, after %+v, error %v", before, after, err)
			}
			if afterFiles := mutationDurableFiles(t, fixture.config.Directory); !reflect.DeepEqual(afterFiles, files) {
				t.Fatalf("retry changed durable files: before %v, after %v", files, afterFiles)
			}
			resolution, err := fixture.manager.ResolveCommit(t.Context(), batch.TxID)
			if err != nil || resolution != (storage.CommitResolution{State: storage.CommitCommitted, Result: committed}) {
				t.Fatalf("retry resolution for %x = %+v, %v", batch.TxID, resolution, err)
			}
			for _, version := range []struct {
				timestamp model.Timestamp
				value     int64
			}{{20, 100}, {committed.CommitTS, 90}} {
				reader, err := fixture.manager.HistoricalSnapshot(t.Context(), version.timestamp)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				})
				contractValues(t, reader, fixture.request, version.value)
			}
		})
		if phase == "live" {
			reopenCommitFixture(t, &fixture)
		}
	}
}

var errMutationSubmissionInjected = errors.New("injected mutation submission failure")

type mutationSubmissionBackend struct {
	storage.Backend
	first       model.TxnID
	failed      model.TxnID
	postDurable bool
	cancel      context.CancelFunc
	entered     chan struct{}
	release     chan struct{}
}

func (backend *mutationSubmissionBackend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	if batch.TxID == backend.first {
		close(backend.entered)
		select {
		case <-backend.release:
		case <-ctx.Done():
			return storage.ApplyResult{}, ctx.Err()
		}
	}
	if batch.TxID == backend.failed && !backend.postDurable {
		return storage.ApplyResult{}, errMutationSubmissionInjected
	}
	result, err := backend.Backend.ApplyMutations(ctx, batch)
	if err != nil || batch.TxID != backend.failed {
		return result, err
	}
	backend.cancel()
	return result, errors.Join(errMutationSubmissionInjected, context.Canceled)
}

type mutationSubmissionScenario struct {
	name         string
	failureFirst bool
	postDurable  bool
}

func mutationSubmissionIsolation(t *testing.T) {
	for _, scenario := range []mutationSubmissionScenario{
		{name: "failure_then_success", failureFirst: true},
		{name: "success_then_failure"},
		{name: "success_then_indeterminate", postDurable: true},
	} {
		t.Run(scenario.name, func(t *testing.T) { mutationSubmissionCase(t, scenario) })
	}
}

func mutationSubmissionCase(t *testing.T, scenario mutationSubmissionScenario) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		var injected *mutationSubmissionBackend
		fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
			injected = &mutationSubmissionBackend{Backend: backend, postDurable: scenario.postDurable, entered: make(chan struct{}), release: make(chan struct{})}
			return injected
		})
		policy, request := contractTable(t)
		options := contractOptions(t, policy)
		success := contractBegin(t, fixture.manager, options)
		failure := contractBegin(t, fixture.manager, options)
		contractApply(t, success, storage.NewInsertMutation(request.Target, contractKey(2), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(2), model.Int64Val(22)}))
		contractApply(t, failure, storage.NewInsertMutation(request.Target, contractKey(3), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(3), model.Int64Val(33)}))
		injected.failed = failure.ID()
		first, second := success, failure
		if scenario.failureFirst {
			first, second = failure, success
		}
		injected.first = first.ID()
		ctx, cancel := context.WithCancel(t.Context())
		injected.cancel = cancel
		type outcome struct {
			result storage.ApplyResult
			err    error
		}
		outcomes := make(map[model.TxnID]chan outcome)
		var workers sync.WaitGroup
		var release sync.Once
		t.Cleanup(func() {
			cancel()
			release.Do(func() { close(injected.release) })
			workers.Wait()
		})
		start := func(transaction *Transaction) {
			done := make(chan outcome, 1)
			outcomes[transaction.ID()] = done
			workers.Add(1)
			go func() {
				defer workers.Done()
				operation := t.Context()
				if transaction == failure {
					operation = ctx
				}
				result, err := transaction.Commit(operation)
				done <- outcome{result, err}
			}()
		}
		start(first)
		select {
		case <-injected.entered:
		case early := <-outcomes[first.ID()]:
			t.Fatalf("first TxID %x completed before the backend barrier: %+v, %v", first.ID(), early.result, early.err)
		}
		start(second)
		// Both owners are committing while the first holds the real sequencer gate.
		synctest.Wait()
		if first.State() != Committing || second.State() != Committing {
			t.Fatalf("overlapping commit states = %v, %v", first.State(), second.State())
		}
		release.Do(func() { close(injected.release) })
		good := <-outcomes[success.ID()]
		bad := <-outcomes[failure.ID()]
		workers.Wait()
		if good.err != nil || success.State() != Committed || good.result.AppliedCount != 1 || good.result.LogPosition != 2 {
			t.Fatalf("successful TxID %x = %+v, %v, state %v", success.ID(), good.result, good.err, success.State())
		}
		if !errors.Is(bad.err, errMutationSubmissionInjected) || bad.result != (storage.ApplyResult{}) {
			t.Fatalf("failing TxID %x = %+v, %v", failure.ID(), bad.result, bad.err)
		}
		wantFailure := storage.CommitResolution{State: storage.CommitNotCommitted}
		if scenario.postDurable {
			if !errors.Is(bad.err, storage.ErrCommitIndeterminate) || !errors.Is(bad.err, context.Canceled) || failure.State() != Indeterminate {
				t.Fatalf("post-durable TxID %x error = %v, state %v", failure.ID(), bad.err, failure.State())
			}
			resolved, err := failure.Resolve(t.Context())
			committed := resolved.State == storage.CommitCommitted && failure.State() == Committed
			orderedResult := resolved.Result.AppliedCount == 1 && resolved.Result.LogPosition == 3 && resolved.Result.CommitTS > good.result.CommitTS
			if err != nil || !committed || !orderedResult {
				t.Fatalf("resolved TxID %x = %+v, %v, state %v", failure.ID(), resolved, err, failure.State())
			}
			wantFailure = resolved
		} else if failure.State() != Aborted || errors.Is(bad.err, storage.ErrCommitIndeterminate) {
			t.Fatalf("pre-durable TxID %x error = %v, state %v", failure.ID(), bad.err, failure.State())
		}
		stable, err := success.Resolve(t.Context())
		if err != nil || stable != (storage.CommitResolution{State: storage.CommitCommitted, Result: good.result}) || success.State() != Committed {
			t.Fatalf("failing neighbor changed successful TxID %x: %+v, %v, state %v", success.ID(), stable, err, success.State())
		}
		for _, phase := range []string{"live", "reopened"} {
			for id, want := range map[model.TxnID]storage.CommitResolution{
				success.ID(): {State: storage.CommitCommitted, Result: good.result},
				failure.ID(): wantFailure,
			} {
				got, err := fixture.manager.ResolveCommit(t.Context(), id)
				if err != nil || got != want {
					t.Fatalf("%s resolution for %x = %+v, %v; want %+v", phase, id, got, err, want)
				}
			}
			reader, err := fixture.manager.HistoricalSnapshot(t.Context(), max(good.result.CommitTS, wantFailure.Result.CommitTS))
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if err := reader.Close(); err != nil {
						t.Error(err)
					}
				}()
				request.Key = contractKey(2)
				contractValues(t, reader, request, 2, 22, 9)
				request.Key = contractKey(3)
				if scenario.postDurable {
					contractValues(t, reader, request, 3, 33, 9)
				} else {
					contractAbsent(t, reader, request)
				}
			}()
			if phase == "live" {
				reopenCommitFixture(t, &fixture)
			}
		}
	})
}
