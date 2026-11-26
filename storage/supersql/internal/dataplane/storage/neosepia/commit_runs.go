package neosepia

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type committedRuns struct {
	Decision  runDecision
	Result    storage.ApplyResult
	Admission *runAdmission
}

type validatedMutationBatch struct {
	batch    storage.MutationBatch
	identity [32]byte
}

func validateMutationBatch(ctx context.Context, batch storage.MutationBatch) (validatedMutationBatch, error) {
	identity, err := batch.Identity(ctx)
	if err != nil {
		return validatedMutationBatch{}, err
	}
	return validatedMutationBatch{batch: batch, identity: identity}, nil
}

// The caller holds the database writer lock and completes its clock fence before
// submission. Admission precedes WAL; durable or indeterminate results transfer
// the reservation to the caller until publication, resolution, or set closure.
func commitMutationRuns(ctx context.Context, directory *databaseDirectory, set *runSet, position uint64, validated validatedMutationBatch, limits runPreparationLimits, maxWALBytes, maxPublicationBytes uint64, fault commitFault) (committedRuns, error) {
	if directory == nil || directory.runs == nil || directory.commits == nil || set == nil {
		return committedRuns{}, storage.ErrClosed
	}
	batch, identity := validated.batch, validated.identity
	prepared, err := prepareMutationRuns(ctx, directory.runs, position, batch, identity, limits)
	if err != nil {
		return committedRuns{}, err
	}
	decision := runDecision{Position: position, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}
	admission, err := set.admitDecision(ctx, directory.runs, decision, prepared.files, limits.File, maxPublicationBytes)
	if err != nil {
		return committedRuns{}, errors.Join(err, prepared.Close())
	}
	result, err := writeCommit(ctx, directory.commits, position, batch, identity, maxWALBytes, fault)
	if err != nil && !errors.Is(err, storage.ErrCommitIndeterminate) {
		return committedRuns{}, errors.Join(err, admission.Close(), prepared.Close())
	}
	prepared.detach()
	return committedRuns{Decision: decision, Result: result, Admission: admission}, err
}
