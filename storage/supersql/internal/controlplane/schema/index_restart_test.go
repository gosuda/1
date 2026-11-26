package schema

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSchemaJobRestartAndDualWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	t.Run("publication_with_writes_between_recovered_chunks", func(t *testing.T) {
		f := newIndexRestartFixture(t)
		for id := int64(1); id <= 6; id++ {
			if err := f.write(t, storage.MutationInsert, id, id*10); err != nil {
				t.Fatal(err)
			}
		}
		old, err := f.manager.CurrentReadView(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := old.Close(); err != nil {
				t.Error(err)
			}
		})
		historicalTS := old.Timestamp()
		oldTable, err := f.descriptor(t, old, historicalTS, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		job := f.submit(t)
		f.reopen(t) // The pending intent itself must survive loss of its submitter.
		seen := map[JobState]bool{JobPending: true}
		chunks := 0
		for steps := 0; !job.Terminal() && steps < 32; steps++ {
			if job.State == JobBackfilling && len(job.ResumeKey) == 0 {
				// These keys are still ahead of the protected snapshot scan.
				for _, change := range []struct {
					op        storage.MutationOp
					id, value int64
				}{{storage.MutationUpdate, 5, 500}, {storage.MutationDelete, 6, 0}, {storage.MutationInsert, 7, 70}} {
					if err := f.write(t, change.op, change.id, change.value); err != nil {
						t.Fatal(err)
					}
				}
			}
			reader, err := f.manager.HistoricalSnapshot(t.Context(), historicalTS)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
			})
			// Keep the old reader open across a real runner transaction/barrier.
			previous := job
			runner := f.runner(t, revisionBoundary{IndexPolicies: f.policies, revision: job.Revision})
			job, err = runner.Run(t.Context())
			closeErr := runner.Close()
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if job.Terminal() {
				if err != nil || job.State != JobSucceeded {
					t.Fatalf("publication = %+v, %v", job, err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("revision %d boundary = %+v, %v", previous.Revision, job, err)
			}
			retained, descriptorErr := f.descriptor(t, reader, historicalTS, 1)
			if descriptorErr != nil {
				t.Fatal(descriptorErr)
			}
			if retained.Version() != oldTable.Version() {
				t.Fatalf("old reader adopted table version %d", retained.Version())
			}
			spec, err := retained.Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(spec.Table.Indexes) != 0 {
				t.Fatalf("historical table gained index: %v", spec.Table.Indexes)
			}
			if _, err := f.descriptor(t, reader, historicalTS, 3); !errors.Is(err, catalog.ErrUnknownObject) {
				t.Fatalf("historical index = %v, want absent", err)
			}
			row, err := reader.Get(t.Context(), f.request(t, storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, 6))
			if err != nil || len(row.Values) != 2 || row.Values[1].IntVal != 60 {
				t.Fatalf("old reader lost deleted row: %+v, %v", row, err)
			}
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if job.Revision != previous.Revision+1 {
				t.Fatalf("revision advanced from %d to %d", previous.Revision, job.Revision)
			}
			seen[job.State] = true
			if previous.State == JobBackfilling {
				chunks++
				if bytes.Compare(job.ResumeKey, previous.ResumeKey) <= 0 && job.State == JobBackfilling {
					t.Fatal("chunk checkpoint did not advance")
				}
				if chunks == 1 {
					// These keys have already been visited; restart must not replay stale values.
					for _, change := range []struct {
						op        storage.MutationOp
						id, value int64
					}{{storage.MutationUpdate, 1, 100}, {storage.MutationDelete, 2, 0}, {storage.MutationInsert, 8, 80}} {
						if err := f.write(t, change.op, change.id, change.value); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			f.reopen(t)
			view, err := f.manager.CurrentReadView(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := view.Close(); err != nil {
					t.Error(err)
				}
			})
			durable, err := f.jobs.Get(t.Context(), view, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if durable.State != job.State || durable.Revision != job.Revision || durable.SnapshotTS != job.SnapshotTS || !bytes.Equal(durable.ResumeKey, job.ResumeKey) {
				t.Fatalf("recovered checkpoint = %+v, want %+v", durable, job)
			}
			index, err := f.descriptor(t, view, view.Timestamp(), 3)
			if err != nil {
				t.Fatal(err)
			}
			if (index.State() == model.StatePublic) != (job.State == JobPublic || job.State == JobSucceeded) {
				t.Fatalf("index state %v at job state %v", index.State(), job.State)
			}
			if err := view.Close(); err != nil {
				t.Fatal(err)
			}
			if job.ProtectedTimestamp != 0 {
				if err := f.protections.Require(job.ID, job.SnapshotTS); err != nil {
					t.Fatalf("recovered snapshot protection: %v", err)
				}
			}
		}
		if job.State != JobSucceeded || chunks < 3 {
			t.Fatalf("incomplete multi-chunk migration: %+v, chunks=%d", job, chunks)
		}
		for _, state := range []JobState{JobPending, JobDeleteOnly, JobWriteOnly, JobBackfilling, JobValidating, JobPublic, JobSucceeded} {
			if !seen[state] {
				t.Errorf("no restart at durable state %v", state)
			}
		}
		f.assertRows(t, map[int64]int64{1: 100, 3: 30, 4: 40, 5: 500, 7: 70, 8: 80}, 2, 6)
		policy := f.policy(t)
		view, err := f.manager.CurrentReadView(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		parity, err := policy.Indexes.ValidateIndex(t.Context(), view, 3, IndexValidationLimits{BatchRows: 2, MaxRows: 100, MaxBytes: 8 << 20})
		err = errors.Join(err, view.Close())
		if err != nil || parity.BaseRows != 6 || parity.IndexRows != 6 {
			t.Fatalf("published parity = %+v, %v", parity, err)
		}
		if err := f.write(t, storage.MutationInsert, 9, 500); !errors.Is(err, errIndexUniqueViolation) {
			t.Fatalf("published unique constraint = %v", err)
		}
		f.assertRows(t, map[int64]int64{5: 500}, 9)
		f.assertUnprotected(t)
	})

	t.Run("duplicate_reverts_across_restart", func(t *testing.T) {
		f := newIndexRestartFixture(t)
		for id := int64(1); id <= 3; id++ {
			if err := f.write(t, storage.MutationInsert, id, 10); err != nil {
				t.Fatal(err)
			}
		}
		job := f.submit(t)
		reverted := false
		for steps := 0; !job.Terminal() && steps < 16; steps++ {
			runner := f.runner(t, revisionBoundary{IndexPolicies: f.policies, revision: job.Revision})
			var err error
			job, err = runner.Run(t.Context())
			if closeErr := runner.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if job.Terminal() {
				var sqlError *model.Error
				if job.State != JobFailed || !errors.As(err, &sqlError) || sqlError.Code != "23505" {
					t.Fatalf("duplicate outcome = %+v, %v", job, err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("failure boundary = %+v, %v", job, err)
			}
			if job.State == JobReverting {
				reverted = true
			}
			f.reopen(t)
		}
		if job.State != JobFailed || job.ErrorCode != "23505" || !reverted || len(job.FailureKey) == 0 {
			t.Fatalf("duplicate did not durably revert: %+v", job)
		}
		if err := f.write(t, storage.MutationUpdate, 1, 20); err != nil {
			t.Fatal(err)
		}
		if err := f.write(t, storage.MutationInsert, 4, 10); err != nil {
			t.Fatalf("failed index still constrains table: %v", err)
		}
		f.assertRows(t, map[int64]int64{1: 20, 2: 10, 3: 10, 4: 10})
		f.assertUnprotected(t)
	})

	t.Run("invocation_cancellation_retains_job_until_explicit_cancel", func(t *testing.T) {
		f := newIndexRestartFixture(t)
		if err := f.write(t, storage.MutationInsert, 1, 10); err != nil {
			t.Fatal(err)
		}
		job := f.submit(t)
		for job.State != JobBackfilling {
			runner := f.runner(t, revisionBoundary{IndexPolicies: f.policies, revision: job.Revision})
			var err error
			job, err = runner.Run(t.Context())
			if closeErr := runner.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation setup = %+v, %v", job, err)
			}
		}
		runner := f.runner(t, f.policies)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := runner.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled invocation = %v", err)
		}
		if err := runner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.protections.Require(job.ID, job.SnapshotTS); err != nil {
			t.Fatalf("runner close released durable pin: %v", err)
		}
		f.reopen(t)
		runner = f.runner(t, revisionBoundary{IndexPolicies: f.policies, revision: job.Revision})
		job, err := runner.Cancel(t.Context())
		if closeErr := runner.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if job.State != JobReverting || job.ErrorCode != "57014" || !errors.Is(err, context.Canceled) {
			t.Fatalf("durable cancellation = %+v, %v", job, err)
		}
		f.reopen(t)
		runner = f.runner(t, f.policies)
		job, err = runner.Run(t.Context())
		if closeErr := runner.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if job.State != JobCanceled || !errors.Is(err, context.Canceled) {
			t.Fatalf("recovered cancellation = %+v, %v", job, err)
		}
		if err := f.write(t, storage.MutationInsert, 2, 10); err != nil {
			t.Fatal(err)
		}
		f.assertRows(t, map[int64]int64{1: 10, 2: 10})
		f.assertUnprotected(t)
	})
}

func (f *indexRestartFixture) assertRows(t *testing.T, expected map[int64]int64, absent ...int64) {
	t.Helper()
	policy := f.policy(t)
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	for id, value := range expected {
		row, err := view.Get(t.Context(), f.request(t, policy.Table, id))
		if err != nil || len(row.Values) != 2 || row.Values[1].IntVal != value {
			t.Errorf("base row %d = %+v, %v; want value %d", id, row, err, value)
		}
	}
	for _, id := range absent {
		if row, err := view.Get(t.Context(), f.request(t, policy.Table, id)); !errors.Is(err, storage.ErrRowNotFound) {
			t.Errorf("deleted/rejected row %d = %+v, %v", id, row, err)
		}
	}
}
func (f *indexRestartFixture) assertUnprotected(t *testing.T) {
	t.Helper()
	if timestamp, protected, err := f.protections.Oldest(); err != nil || protected {
		t.Fatalf("terminal job retains protection: %d, %v, %v", timestamp, protected, err)
	}
}
