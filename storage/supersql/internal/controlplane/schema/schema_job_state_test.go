package schema

import (
	"bytes"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestDefinedJobKindsFollowDurableStatePath(t *testing.T) {
	for _, test := range []struct {
		name string
		kind JobKind
	}{{"drop_index", DropIndexJob}, {"add_column", AddColumnJob}, {"rewrite_generation", RewriteGenerationJob}} {
		t.Run(test.name, func(t *testing.T) {
			kind := test.kind
			job := Job{ID: 1, TableID: 2, MutationID: 3, Kind: kind, State: JobPending, SourceVersion: 4, TargetVersion: 7, Revision: 1}
			path := []JobState{JobDeleteOnly, JobWriteOnly, JobBackfilling, JobValidating, JobPublic, JobSucceeded}
			for i, state := range path {
				next := job
				next.Revision++
				if i == 0 {
					next.Attempt++
				}
				next.State = state
				if state == JobBackfilling {
					next.SnapshotTS = 11
					next.ProtectedTimestamp = 11
				}
				if state == JobValidating {
					next.ResumeKey = []byte("checkpoint")
				}
				if state == JobSucceeded {
					next.ProtectedTimestamp = 0
				}
				if err := validateJobSuccessor(job, next); err != nil {
					t.Fatalf("%d -> %d: %v", job.State, state, err)
				}
				encoded, err := EncodeJob(t.Context(), next, 4096)
				if err != nil {
					t.Fatal(err)
				}
				recovered, err := DecodeJob(t.Context(), encoded, 4096)
				if err != nil {
					t.Fatal(err)
				}
				if !sameBackfillJob(recovered, next) {
					t.Fatalf("recovered checkpoint = %+v, want %+v", recovered, next)
				}
				job = recovered
			}
		})
	}
}

func TestDefinedJobRestartFencesPriorAttempt(t *testing.T) {
	job := Job{ID: 1, TableID: 2, MutationID: 3, Kind: RewriteGenerationJob, State: JobBackfilling, SourceVersion: 4, TargetVersion: 7, Revision: 9, SnapshotTS: 8, ProtectedTimestamp: 8, Attempt: 2, ResumeKey: []byte("a")}
	next := job
	next.Revision++
	next.Attempt++
	next.ResumeKey = []byte("b")
	if err := validateJobSuccessor(job, next); err != nil {
		t.Fatal(err)
	}
	stale := next
	stale.Revision = job.Revision
	stale.Attempt = job.Attempt
	if sameBackfillJob(stale, next) || bytes.Equal(stale.ResumeKey, job.ResumeKey) {
		t.Fatal("restart fence accepted a stale owner or checkpoint")
	}
	if err := validJob(next, 4096); err != nil {
		t.Fatal(err)
	}
	if next.ProtectedTimestamp != model.Timestamp(8) {
		t.Fatal("restart lost protected snapshot")
	}
}
