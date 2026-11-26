package neosepia

import (
	"context"
	"errors"
	"os"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Resolve a known submission under the database writer lock. Directory sync
// establishes durability; an I/O or cancellation failure alone never proves an
// abort. Artifact failures after that proof do not change the committed outcome.
func resolveMutationRuns(ctx context.Context, directory *databaseDirectory, decision runDecision, limits runPreparationLimits, batchLimits storage.BatchDecodeLimits) (_ storage.CommitResolution, _ []preparedRun, err error) {
	if err := ctx.Err(); err != nil {
		return storage.CommitResolution{}, nil, err
	}
	if err := decision.validate(); err != nil {
		return storage.CommitResolution{}, nil, err
	}
	if directory == nil || directory.commits == nil || directory.runs == nil {
		return storage.CommitResolution{}, nil, storage.ErrClosed
	}
	record, err := openCommit(ctx, directory.commits, decision.Position, batchLimits)
	if errors.Is(err, os.ErrNotExist) {
		if err := ctx.Err(); err != nil {
			return storage.CommitResolution{}, nil, err
		}
		if err := syncCommitDirectory(directory.commits); err != nil {
			return storage.CommitResolution{}, nil, err
		}
		files, loadErr := loadRunManifest(ctx, directory.runs, decision, limits)
		if errors.Is(loadErr, os.ErrNotExist) {
			return storage.CommitResolution{State: storage.CommitNotCommitted}, nil, nil
		}
		if loadErr != nil {
			return storage.CommitResolution{State: storage.CommitNotCommitted}, nil, loadErr
		}
		abandoned := preparedRuns{root: directory.runs, files: files, manifest: runManifestName(decision.Position)}
		return storage.CommitResolution{State: storage.CommitNotCommitted}, nil, abandoned.Close()
	}
	if err != nil {
		return storage.CommitResolution{}, nil, err
	}
	defer func() { err = errors.Join(err, record.Close()) }()
	if record.Batch.TxID != decision.TxID || record.Identity != decision.Identity || record.Result.CommitTS != decision.CommitTS || record.Result.AppliedCount != decision.MutationCount {
		return storage.CommitResolution{}, nil, storage.ErrCorruptStorage
	}
	if err := ctx.Err(); err != nil {
		return storage.CommitResolution{}, nil, err
	}
	if err := syncCommitDirectory(directory.commits); err != nil {
		return storage.CommitResolution{}, nil, err
	}
	resolution := storage.CommitResolution{State: storage.CommitCommitted, Result: record.Result}
	files, err := loadRunManifest(ctx, directory.runs, decision, limits)
	if errors.Is(err, os.ErrNotExist) {
		err = errors.Join(storage.ErrCorruptStorage, err)
	}
	return resolution, files, err
}
