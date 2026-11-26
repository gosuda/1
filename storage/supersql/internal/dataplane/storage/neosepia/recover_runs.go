package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

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
	PrunedPosition uint64
	PrunedCommitTS model.Timestamp
	FenceIndex     coveredFenceIndex
	PublishedEpoch clock.Epoch
	Memtable       memtable
}

// discoverRunManifests scans runs/ for existing <pos>.runs manifests covering
// positions strictly after basePosition so recovery distinguishes:
//   - NSRUNS01 single-position manifests
//   - NSRUNS02 coalesced multi-position manifests
//   - unflushed WAL commits (no manifest on disk, cleanly replayed into memtable)
func discoverRunManifests(ctx context.Context, root *os.Root, after uint64) (map[uint64]runManifestSpan, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	spans := make(map[uint64]runManifestSpan)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			if len(name) != 25 || !strings.HasSuffix(name, ".runs") || !decimalPrefix(name[:20]) {
				continue
			}
			pos, parseErr := strconv.ParseUint(name[:20], 10, 64)
			if parseErr != nil || pos <= after {
				continue
			}
			span, err := inspectRunManifest(ctx, root, pos)
			if err != nil {
				return nil, err
			}
			if span.firstPosition <= after {
				return nil, storage.ErrCorruptStorage
			}
			for p := span.firstPosition; p <= span.lastPosition; p++ {
				if _, dup := spans[p]; dup {
					return nil, storage.ErrCorruptStorage
				}
				spans[p] = span
			}
		}
		if errors.Is(readErr, io.EOF) {
			return spans, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

// The caller owns the directory writer lock. Recovery returns a usable file set
// only after every WAL decision and its complete native file group validate.
// Unflushed WAL decisions without a .runs manifest are replayed into Memtable.
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
	var fenceIndex coveredFenceIndex
	if state.BaseGeneration != 0 {
		base, err = loadRunBaseWithCipher(ctx, directory.runs, state.BaseGeneration, limits.Preparation.MetadataBytes, limits.IdentityBytes, limits.Preparation.MaxRuns, limits.Preparation.File.cipher)
		if err != nil {
			return recoveredRuns{}, err
		}
		if base.Floor != state.HistoryFloor {
			return recoveredRuns{}, storage.ErrCorruptStorage
		}
		if err := validateRecordState(&storedCommit{Batch: storage.MutationBatch{Epoch: base.Epoch}, Result: storage.ApplyResult{LogPosition: base.Position, CommitTS: base.CommitTS}}, state); err != nil {
			return recoveredRuns{}, err
		}
		fenceIndex, err = buildCoveredFenceIndex(ctx, base)
		if err != nil {
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
	if limits.Log.maxCommitTS > 0 && base.Generation != 0 && base.CommitTS > limits.Log.maxCommitTS {
		return recoveredRuns{}, storage.ErrInvalidMutation
	}
	var manifests map[uint64]runManifestSpan
	if limits.Log.maxCommitTS == 0 {
		manifests, err = discoverRunManifests(ctx, directory.runs, base.Position)
		if err != nil {
			return recoveredRuns{}, err
		}
	}
	identities := base.Transactions
	position := recoveredPosition{LogPosition: base.Position, CommitTS: base.CommitTS}
	publishedEpoch := base.Epoch
	var mem memtable
	var pendingSpan runManifestSpan
	var pendingDecisions []runDecision
	var pendingTSs []model.Timestamp

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
		pos := record.Result.LogPosition
		decision := runDecision{Position: pos, TxID: record.Batch.TxID, CommitTS: record.Result.CommitTS, Identity: record.Identity, MutationCount: record.Result.AppliedCount}

		span, hasManifest := manifests[pos]
		if !hasManifest {
			// Unflushed WAL commit (crashed while buffered in memtable): replay cleanly.
			if len(pendingDecisions) > 0 {
				return storage.ErrCorruptStorage
			}
			return mem.stageBatch(ctx, decision, record.Batch.Epoch, record.Batch, limits.Log.Batch.MaxAllocation)
		}
		// Manifested commits must precede any unflushed tail memtable commits.
		if !mem.empty() {
			return storage.ErrCorruptStorage
		}
		if !span.coalesced {
			if len(pendingDecisions) > 0 {
				return storage.ErrCorruptStorage
			}
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
		}
		// Coalesced NSRUNS02 manifest spanning [span.firstPosition .. span.lastPosition].
		if len(pendingDecisions) == 0 {
			if pos != span.firstPosition {
				return storage.ErrCorruptStorage
			}
			pendingSpan = span
		} else if span != pendingSpan {
			return storage.ErrCorruptStorage
		}
		pendingDecisions = append(pendingDecisions, decision)
		pendingTSs = append(pendingTSs, decision.CommitTS)
		if pos == span.lastPosition {
			coalesced, err := makeCoalescedDecision(pendingDecisions)
			if err != nil {
				return err
			}
			files, err := loadCoalescedRunManifest(ctx, directory.runs, coalesced, limits.Preparation)
			if err != nil {
				return err
			}
			admission, err := set.admitCoalescedDecision(ctx, directory.runs, coalesced, pendingTSs, files, limits.Preparation.File, limits.StagingBytes)
			if err != nil {
				return err
			}
			if err := admission.publish(ctx); err != nil {
				return errors.Join(err, admission.Close())
			}
			pendingDecisions = pendingDecisions[:0]
			pendingTSs = pendingTSs[:0]
			pendingSpan = runManifestSpan{}
		}
		return nil
	})
	if err != nil {
		return recoveredRuns{}, err
	}
	if len(pendingDecisions) > 0 {
		return recoveredRuns{}, storage.ErrCorruptStorage
	}
	if base.Generation != 0 && position.LogPosition < base.Position {
		return recoveredRuns{}, storage.ErrCorruptStorage
	}
	return recoveredRuns{
		Set:            set,
		Position:       position,
		Transactions:   identities,
		PrunedPosition: base.PrunedPosition,
		PrunedCommitTS: base.PrunedCommitTS,
		FenceIndex:     fenceIndex,
		PublishedEpoch: publishedEpoch,
		Memtable:       mem,
	}, nil
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
