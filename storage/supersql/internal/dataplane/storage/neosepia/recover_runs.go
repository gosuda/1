package neosepia

import (
	"bytes"
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type recoveryTxnID model.TxnID

func (id recoveryTxnID) Compare(other recoveryTxnID) int {
	return bytes.Compare(id[:], other[:])
}

type transactionRecord struct {
	Identity [32]byte
	Result   storage.ApplyResult
}

type runRecoveryLimits struct {
	Log                         recoveryLimits
	Preparation                 runPreparationLimits
	Resident                    runSetLimits
	StagingBytes, IdentityBytes uint64
}

type recoveredRuns struct {
	Set            *runSet
	Position       recoveredPosition
	Transactions   ordered.Tree[recoveryTxnID, transactionRecord]
	PublishedEpoch clock.Epoch
}

// The caller owns the directory writer lock. Recovery returns a usable file set
// only after every WAL decision and its complete native file group validate.
func recoverPublishedRuns(ctx context.Context, directory *databaseDirectory, state databaseState, limits runRecoveryLimits, validate func(*storedCommit) error) (_ recoveredRuns, err error) {
	if directory == nil || directory.commits == nil || directory.runs == nil {
		return recoveredRuns{}, storage.ErrClosed
	}
	set, err := newRunSet(limits.Resident)
	if err != nil {
		return recoveredRuns{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, set.Close())
		}
	}()
	var base runBase
	if state.BaseGeneration != 0 {
		base, err = loadRunBase(ctx, directory.runs, state.BaseGeneration, limits.Preparation.MetadataBytes, limits.IdentityBytes, limits.Preparation.MaxRuns)
		if err != nil {
			return recoveredRuns{}, err
		}
		if base.Floor != state.HistoryFloor {
			return recoveredRuns{}, storage.ErrCorruptStorage
		}
		if err := validateRecordState(&storedCommit{Batch: storage.MutationBatch{Epoch: base.Epoch}, Result: storage.ApplyResult{LogPosition: base.Position, CommitTS: base.CommitTS}}, state); err != nil {
			return recoveredRuns{}, err
		}
		opened, openErr := openBaseFiles(ctx, directory.runs, base.Files, limits.Preparation.File, limits.StagingBytes)
		if openErr != nil {
			return recoveredRuns{}, openErr
		}
		if err := set.publishBase(ctx, base.Position, base.CommitTS, base.Generation, opened); err != nil {
			return recoveredRuns{}, err
		}
	}
	identities := base.Transactions
	position := recoveredPosition{LogPosition: base.Position, CommitTS: base.CommitTS}
	publishedEpoch := base.Epoch
	position, err = recoverCommits(ctx, directory.commits, position, limits.Log, func(record *storedCommit) error {
		id := recoveryTxnID(record.Batch.TxID)
		if _, exists := identities.Get(id); exists {
			return errors.Join(storage.ErrCorruptStorage, storage.ErrTxnIDReused)
		}
		if validate != nil {
			if err := validate(record); err != nil {
				return err
			}
		}
		next, _, err := identities.Set(id, transactionRecord{Identity: record.Identity, Result: record.Result}, limits.IdentityBytes-identities.NodeBytes())
		if err != nil {
			return err
		}
		publishedEpoch = record.Batch.Epoch
		identities = next
		decision := runDecision{Position: record.Result.LogPosition, TxID: record.Batch.TxID, CommitTS: record.Result.CommitTS, Identity: record.Identity, MutationCount: record.Result.AppliedCount}
		files, err := loadRunManifest(ctx, directory.runs, decision, limits.Preparation)
		if err != nil {
			return err
		}
		admission, err := set.admitDecision(ctx, directory.runs, decision, files, limits.Preparation.File, limits.StagingBytes)
		if err != nil {
			return err
		}
		if err := admission.publish(ctx); err != nil {
			return errors.Join(err, admission.Close())
		}
		return nil
	})
	if err != nil {
		return recoveredRuns{}, err
	}
	if base.Generation != 0 && position.LogPosition < base.Position {
		return recoveredRuns{}, storage.ErrCorruptStorage
	}
	return recoveredRuns{Set: set, Position: position, Transactions: identities, PublishedEpoch: publishedEpoch}, nil
}

func sameTransactionRecords(a, b ordered.Tree[recoveryTxnID, transactionRecord]) bool {
	if a.Len() != b.Len() {
		return false
	}
	left := a.First(false)
	right := b.First(false)
	for {
		leftID, leftRecord, leftOK := left.Next()
		rightID, rightRecord, rightOK := right.Next()
		if leftOK != rightOK {
			return false
		}
		if !leftOK {
			return true
		}
		if leftID != rightID || leftRecord != rightRecord {
			return false
		}
	}
}
