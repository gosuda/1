package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
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
	ReadOnly                    bool
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
	prunedPosition := base.PrunedPosition
	prunedCommitTS := base.PrunedCommitTS
	position := recoveredPosition{LogPosition: base.Position, CommitTS: base.CommitTS}
	publishedEpoch := base.Epoch
	var mem memtable
	var sawUnflushed bool
	var cleanedStaleRuns bool
	var pendingSpan runManifestSpan
	var pendingDecisions []runDecision
	var pendingTSs []model.Timestamp

	flushThreshold := recoveryMemtableMaxBytes(limits)
	nodeBudget := recoveryMemtableNodeBudget(limits)
	prepLimits := limits.Preparation
	if prepLimits.File.cipher == nil {
		prepLimits.File.cipher = limits.Log.cipher
	}

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
		var err error
		identities, prunedPosition, prunedCommitTS, err = insertRecoveredTransaction(
			ctx, identities, prunedPosition, prunedCommitTS,
			id, transactionRecord{Identity: record.Identity, Result: record.Result},
			limits.IdentityBytes,
		)
		if err != nil {
			return err
		}
		publishedEpoch = record.Batch.Epoch
		pos := record.Result.LogPosition
		decision := runDecision{Position: pos, TxID: record.Batch.TxID, CommitTS: record.Result.CommitTS, Identity: record.Identity, MutationCount: record.Result.AppliedCount}

		span, hasManifest := manifests[pos]
		if !hasManifest {
			// Unflushed WAL commit (crashed while buffered in memtable or PITR replay): replay cleanly.
			if len(pendingDecisions) > 0 {
				return storage.ErrCorruptStorage
			}
			sawUnflushed = true
			if err := mem.stageBatchWithBudget(ctx, decision, record.Batch.Epoch, record.Batch, limits.Log.Batch.MaxAllocation, nodeBudget); err != nil {
				return err
			}
			if !limits.ReadOnly && prepLimits.MaxRuns > 0 && limits.StagingBytes > 0 && mem.shouldFlushThreshold(flushThreshold) {
				if limits.Log.maxCommitTS > 0 && !cleanedStaleRuns {
					if err := cleanupObsoleteRuns(ctx, directory.runs, set); err != nil {
						return err
					}
					cleanedStaleRuns = true
				} else if limits.Log.maxCommitTS == 0 {
					if err := removeOrphanedRunSSTs(directory.runs, mem.lastPos); err != nil {
						return err
					}
				}
				if err := mem.flushToRunsWithLimits(ctx, directory.runs, set, prepLimits, limits.StagingBytes, nodeBudget, 0); err != nil {
					return err
				}
			}
			return nil
		}
		// Manifested commits must precede any unflushed tail memtable commits.
		if sawUnflushed || !mem.empty() {
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
		PrunedPosition: prunedPosition,
		PrunedCommitTS: prunedCommitTS,
		FenceIndex:     fenceIndex,
		PublishedEpoch: publishedEpoch,
		Memtable:       mem,
	}, nil
}

func recoveryMemtableMaxBytes(limits runRecoveryLimits) uint64 {
	threshold := defaultMemtableMaxBytes
	if limits.Preparation.Chunk.BuildBytes > 0 {
		threshold = min(threshold, limits.Preparation.Chunk.BuildBytes/4)
	}
	if limits.Resident.MaxBytes > 0 {
		threshold = min(threshold, limits.Resident.MaxBytes/4)
	}
	if limits.StagingBytes > 0 {
		threshold = min(threshold, limits.StagingBytes/4)
	}
	return threshold
}

func recoveryMemtableNodeBudget(limits runRecoveryLimits) uint64 {
	return max(defaultMemtableNodeBudget, limits.Resident.MaxBytes)
}

func removeOrphanedRunSSTs(runsRoot *os.Root, position uint64) error {
	for idx := 0; ; idx++ {
		err := runsRoot.Remove(preparedRunName(position, idx))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func insertRecoveredTransaction(
	ctx context.Context,
	identities ordered.Tree[recoveryTxnID, transactionRecord],
	prunedPosition uint64,
	prunedCommitTS model.Timestamp,
	id recoveryTxnID,
	record transactionRecord,
	identityBytes uint64,
) (ordered.Tree[recoveryTxnID, transactionRecord], uint64, model.Timestamp, error) {
	var remaining uint64
	if identityBytes > identities.NodeBytes() {
		remaining = identityBytes - identities.NodeBytes()
	}
	next, _, err := identities.Set(id, record, remaining)
	if err == nil {
		return next, prunedPosition, prunedCommitTS, nil
	}
	if !errors.Is(err, ordered.ErrBudget) || identities.Len() == 0 {
		return identities, prunedPosition, prunedCommitTS, err
	}
	// Evict oldest committed transaction identities (lowest LogPosition / CommitTS)
	// in contiguous prefix order, matching pruneBaseTransactions semantics so large
	// WAL/PITR replays keep a bounded recent dedup window within IdentityBytes.
	entries := make([]baseTransaction, 0, int(identities.Len()))
	cursor := identities.First(false)
	for {
		if err := ctx.Err(); err != nil {
			return identities, prunedPosition, prunedCommitTS, err
		}
		entryID, entryRec, ok := cursor.Next()
		if !ok {
			break
		}
		entries = append(entries, baseTransaction{id: entryID, record: entryRec})
	}
	slices.SortFunc(entries, func(a, b baseTransaction) int {
		if a.record.Result.LogPosition < b.record.Result.LogPosition {
			return -1
		}
		if a.record.Result.LogPosition > b.record.Result.LogPosition {
			return 1
		}
		return a.id.Compare(b.id)
	})
	keep := min(len(entries)/2, int(defaultMaxRetainedTransactions))
	for {
		var rebuilt ordered.Tree[recoveryTxnID, transactionRecord]
		var buildErr error
		keepFrom := len(entries) - keep
		for _, entry := range entries[keepFrom:] {
			if err := ctx.Err(); err != nil {
				return identities, prunedPosition, prunedCommitTS, err
			}
			var rem uint64
			if identityBytes > rebuilt.NodeBytes() {
				rem = identityBytes - rebuilt.NodeBytes()
			}
			rebuilt, _, buildErr = rebuilt.Set(entry.id, entry.record, rem)
			if buildErr != nil {
				break
			}
		}
		if buildErr == nil {
			var rem uint64
			if identityBytes > rebuilt.NodeBytes() {
				rem = identityBytes - rebuilt.NodeBytes()
			}
			rebuilt, _, buildErr = rebuilt.Set(id, record, rem)
		}
		if buildErr == nil {
			evicted := entries[keepFrom-1]
			return rebuilt, evicted.record.Result.LogPosition, evicted.record.Result.CommitTS, nil
		}
		if !errors.Is(buildErr, ordered.ErrBudget) || keep == 0 {
			return identities, prunedPosition, prunedCommitTS, buildErr
		}
		keep /= 2
	}
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
