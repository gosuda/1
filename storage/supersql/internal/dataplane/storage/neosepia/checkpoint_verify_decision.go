package neosepia

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type checkpointMutationIdentity struct {
	ordinal uint64
	digest  [32]byte
	seen    bool
}

func (check *checkpointVerification) verifyDecision(ctx context.Context, commits, runs *os.Root, state databaseState, previous recoveredPosition) (_ recoveredPosition, err error) {
	position := previous.LogPosition + 1
	if _, hasSingle := check.index("runs/" + runManifestName(position)); !hasSingle {
		return check.verifyCoalescedDecisions(ctx, commits, runs, state, previous)
	}
	span, spanErr := inspectRunManifest(ctx, runs, position)
	if spanErr != nil {
		return recoveredPosition{}, spanErr
	}
	if span.coalesced {
		return check.verifyCoalescedDecisions(ctx, commits, runs, state, previous)
	}
	if err := check.consume("commits/"+commitName(position), nil); err != nil {
		return recoveredPosition{}, err
	}
	if err := check.consume("runs/"+runManifestName(position), nil); err != nil {
		return recoveredPosition{}, err
	}
	record, err := openCommitWithCipher(ctx, commits, position, check.limits.recovery().Batch, check.cipher)
	if err != nil {
		return recoveredPosition{}, err
	}
	defer func() { err = errors.Join(err, record.Close()) }()
	if record.Result.CommitTS <= previous.CommitTS {
		return recoveredPosition{}, storage.ErrCorruptStorage
	}
	if err := validateRecordState(record, state); err != nil {
		return recoveredPosition{}, err
	}
	id := recoveryTxnID(record.Batch.TxID)
	if _, exists := check.transactions.Get(id); exists {
		return recoveredPosition{}, errors.Join(storage.ErrCorruptStorage, storage.ErrTxnIDReused)
	}
	identities, err := checkpointMutationIdentities(ctx, record.Batch, check.limits.IdentityBytes-check.transactions.NodeBytes())
	if err != nil {
		return recoveredPosition{}, err
	}
	decision := runDecision{Position: position, TxID: record.Batch.TxID, CommitTS: record.Result.CommitTS, Identity: record.Identity, MutationCount: record.Result.AppliedCount}
	limits := check.limits.preparation()
	limits.MetadataBytes = min(limits.MetadataBytes, check.limits.MetadataBytes-check.metadata-check.layouts.NodeBytes())
	files, err := loadRunManifest(ctx, runs, decision, limits)
	if err != nil {
		return recoveredPosition{}, err
	}
	fileBytes, err := check.reserveRunMetadata(files)
	if err != nil {
		return recoveredPosition{}, err
	}
	defer func() { check.metadata -= fileBytes }()
	hasher, err := newCheckpointRowHasher(record.Batch.Layouts, check.limits.RowBytes)
	if err != nil {
		return recoveredPosition{}, err
	}
	for _, descriptor := range files {
		layout, found := record.Batch.Layout(descriptor.Target)
		if !found {
			return recoveredPosition{}, storage.ErrCorruptStorage
		}
		if err := check.verifyRun(ctx, runs, descriptor, &layout, func(block *paxBlock, row int) error {
			_, _, ts, ordinal, _, err := block.Version(row)
			if err != nil {
				return err
			}
			index, found := slices.BinarySearchFunc(identities, ordinal, func(identity checkpointMutationIdentity, ordinal uint64) int {
				return cmp.Compare(identity.ordinal, ordinal)
			})
			if ts != record.Result.CommitTS || !found || identities[index].seen {
				return storage.ErrCorruptStorage
			}
			digest, err := hasher.rowDigest(ctx, block, row)
			if err != nil {
				return err
			}
			if digest != identities[index].digest {
				return storage.ErrCorruptStorage
			}
			identities[index].seen = true
			return nil
		}); err != nil {
			return recoveredPosition{}, err
		}
	}
	for _, identity := range identities {
		if !identity.seen {
			return recoveredPosition{}, storage.ErrCorruptStorage
		}
	}
	identityBytes := uint64(len(identities)) * uint64(unsafe.Sizeof(checkpointMutationIdentity{}))
	next, _, err := check.transactions.Set(id, transactionRecord{Identity: record.Identity, Result: record.Result}, check.limits.IdentityBytes-check.transactions.NodeBytes()-identityBytes)
	if err != nil {
		return recoveredPosition{}, err
	}
	check.transactions = next
	return recoveredPosition{LogPosition: position, CommitTS: record.Result.CommitTS}, nil
}

type checkpointCoalescedIdentity struct {
	commitTS model.Timestamp
	ordinal  uint64
	digest   [32]byte
	seen     bool
}

func (check *checkpointVerification) verifyCoalescedDecisions(ctx context.Context, commits, runs *os.Root, state databaseState, previous recoveredPosition) (recoveredPosition, error) {
	firstPos := previous.LogPosition + 1
	var lastPos uint64
	for pos := firstPos; pos <= check.manifest.Fence.LogPosition; pos++ {
		if _, found := check.index("runs/" + runManifestName(pos)); found {
			lastPos = pos
			break
		}
	}
	if lastPos == 0 {
		return recoveredPosition{}, storage.ErrCorruptStorage
	}
	span, err := inspectRunManifest(ctx, runs, lastPos)
	if err != nil {
		return recoveredPosition{}, err
	}
	if !span.coalesced || span.firstPosition != firstPos || span.lastPosition != lastPos {
		return recoveredPosition{}, storage.ErrCorruptStorage
	}
	if err := check.consume("runs/"+runManifestName(lastPos), nil); err != nil {
		return recoveredPosition{}, err
	}
	decisions := make([]runDecision, 0, int(lastPos-firstPos+1))
	var allIdentities []checkpointCoalescedIdentity
	var allLayouts []storage.ObjectLayout
	curr := previous
	for pos := firstPos; pos <= lastPos; pos++ {
		if err := ctx.Err(); err != nil {
			return recoveredPosition{}, err
		}
		if err := check.consume("commits/"+commitName(pos), nil); err != nil {
			return recoveredPosition{}, err
		}
		record, err := openCommitWithCipher(ctx, commits, pos, check.limits.recovery().Batch, check.cipher)
		if err != nil {
			return recoveredPosition{}, err
		}
		if record.Result.CommitTS <= curr.CommitTS {
			return recoveredPosition{}, errors.Join(storage.ErrCorruptStorage, record.Close())
		}
		if err := validateRecordState(record, state); err != nil {
			return recoveredPosition{}, errors.Join(err, record.Close())
		}
		id := recoveryTxnID(record.Batch.TxID)
		if _, exists := check.transactions.Get(id); exists {
			return recoveredPosition{}, errors.Join(storage.ErrCorruptStorage, storage.ErrTxnIDReused, record.Close())
		}
		ids, err := checkpointMutationIdentities(ctx, record.Batch, check.limits.IdentityBytes-check.transactions.NodeBytes())
		if err != nil {
			return recoveredPosition{}, errors.Join(err, record.Close())
		}
		for _, item := range ids {
			allIdentities = append(allIdentities, checkpointCoalescedIdentity{commitTS: record.Result.CommitTS, ordinal: item.ordinal, digest: item.digest})
		}
		allLayouts = append(allLayouts, record.Batch.Layouts...)
		decisions = append(decisions, runDecision{
			Position:      pos,
			TxID:          record.Batch.TxID,
			CommitTS:      record.Result.CommitTS,
			Identity:      record.Identity,
			MutationCount: record.Result.AppliedCount,
		})
		next, _, setErr := check.transactions.Set(id, transactionRecord{Identity: record.Identity, Result: record.Result}, check.limits.IdentityBytes-check.transactions.NodeBytes())
		closeErr := record.Close()
		if setErr != nil {
			return recoveredPosition{}, errors.Join(setErr, closeErr)
		}
		if closeErr != nil {
			return recoveredPosition{}, closeErr
		}
		check.transactions = next
		curr = recoveredPosition{LogPosition: pos, CommitTS: record.Result.CommitTS}
	}
	coalesced, err := makeCoalescedDecision(decisions)
	if err != nil {
		return recoveredPosition{}, err
	}
	limits := check.limits.preparation()
	limits.MetadataBytes = min(limits.MetadataBytes, check.limits.MetadataBytes-check.metadata-check.layouts.NodeBytes())
	files, err := loadCoalescedRunManifest(ctx, runs, coalesced, limits)
	if err != nil {
		return recoveredPosition{}, err
	}
	fileBytes, err := check.reserveRunMetadata(files)
	if err != nil {
		return recoveredPosition{}, err
	}
	defer func() { check.metadata -= fileBytes }()
	slices.SortFunc(allIdentities, func(a, b checkpointCoalescedIdentity) int {
		if c := cmp.Compare(a.commitTS, b.commitTS); c != 0 {
			return c
		}
		return cmp.Compare(a.ordinal, b.ordinal)
	})
	hasher, err := newCheckpointRowHasher(allLayouts, check.limits.RowBytes)
	if err != nil {
		return recoveredPosition{}, err
	}
	for _, descriptor := range files {
		if err := check.verifyRun(ctx, runs, descriptor, nil, func(block *paxBlock, row int) error {
			_, _, ts, ordinal, _, err := block.Version(row)
			if err != nil {
				return err
			}
			index, found := slices.BinarySearchFunc(allIdentities, struct {
				ts  model.Timestamp
				ord uint64
			}{ts, ordinal}, func(identity checkpointCoalescedIdentity, key struct {
				ts  model.Timestamp
				ord uint64
			}) int {
				if c := cmp.Compare(identity.commitTS, key.ts); c != 0 {
					return c
				}
				return cmp.Compare(identity.ordinal, key.ord)
			})
			if !found || allIdentities[index].seen {
				return storage.ErrCorruptStorage
			}
			digest, err := hasher.rowDigest(ctx, block, row)
			if err != nil {
				return err
			}
			if digest != allIdentities[index].digest {
				return storage.ErrCorruptStorage
			}
			allIdentities[index].seen = true
			return nil
		}); err != nil {
			return recoveredPosition{}, err
		}
	}
	for _, identity := range allIdentities {
		if !identity.seen {
			return recoveredPosition{}, storage.ErrCorruptStorage
		}
	}
	return curr, nil
}

func checkpointMutationIdentities(ctx context.Context, batch storage.MutationBatch, maximum uint64) (_ []checkpointMutationIdentity, err error) {
	count := batch.Count()
	if count > uint64(math.MaxInt) || count > maximum/uint64(unsafe.Sizeof(checkpointMutationIdentity{})) {
		return nil, columnar.ErrBatchBudget
	}
	identities := make([]checkpointMutationIdentity, int(count))
	cursor, err := batch.OpenMutations(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	digest := sha256.New()
	for i := range identities {
		mutation, err := cursor.Next(ctx)
		if err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		digest.Reset()
		if err := storage.EncodeMutation(digest, mutation); err != nil {
			return nil, err
		}
		identities[i].ordinal = mutation.Ordinal
		digest.Sum(identities[i].digest[:0])
	}
	if _, err := cursor.Next(ctx); err != io.EOF {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	return identities, nil
}

type checkpointBaseTransaction struct {
	timestamp model.Timestamp
	applied   uint64
	retained  uint64
}

func (check *checkpointVerification) verifyBase(ctx context.Context, runs *os.Root, state databaseState) (recoveredPosition, error) {
	if state.BaseGeneration == 0 {
		return recoveredPosition{}, nil
	}
	if err := check.consume("runs/"+baseManifestName(state.BaseGeneration), nil); err != nil {
		return recoveredPosition{}, err
	}
	metadata := min(check.limits.PreparationBytes, check.limits.MetadataBytes-check.metadata)
	base, err := loadRunBase(ctx, runs, state.BaseGeneration, metadata, check.limits.IdentityBytes, min(check.limits.MaxFiles, check.limits.Runs))
	if err != nil {
		return recoveredPosition{}, err
	}
	if base.Floor != state.HistoryFloor || base.Position > state.Closed.LogPosition || base.CommitTS > state.Closed.ClosedTS {
		return recoveredPosition{}, storage.ErrCorruptStorage
	}
	if err := validateRecordState(&storedCommit{Batch: storage.MutationBatch{Epoch: base.Epoch}, Result: storage.ApplyResult{LogPosition: base.Position, CommitTS: base.CommitTS}}, state); err != nil {
		return recoveredPosition{}, err
	}
	fileBytes, err := check.reserveRunMetadata(base.Files)
	if err != nil {
		return recoveredPosition{}, err
	}
	defer func() { check.metadata -= fileBytes }()
	check.transactions = base.Transactions
	check.prunedPosition = base.PrunedPosition
	retainedCount := base.Transactions.Len()
	unit := uint64(unsafe.Sizeof(checkpointBaseTransaction{}))
	if retainedCount > uint64(math.MaxInt) || retainedCount > (check.limits.IdentityBytes-check.transactions.NodeBytes())/unit {
		return recoveredPosition{}, columnar.ErrBatchBudget
	}
	transactions := make([]checkpointBaseTransaction, int(retainedCount))
	cursor := base.Transactions.First(false)
	defer cursor.Close()
	for {
		if err := ctx.Err(); err != nil {
			return recoveredPosition{}, err
		}
		_, record, ok := cursor.Next()
		if !ok {
			break
		}
		if record.Result.LogPosition <= base.PrunedPosition || record.Result.LogPosition > base.Position {
			return recoveredPosition{}, storage.ErrCorruptStorage
		}
		transactions[record.Result.LogPosition-base.PrunedPosition-1] = checkpointBaseTransaction{timestamp: record.Result.CommitTS, applied: record.Result.AppliedCount}
	}
	for _, descriptor := range base.Files {
		if err := check.verifyRun(ctx, runs, descriptor, nil, func(block *paxBlock, row int) error {
			_, _, ts, ordinal, kind, err := block.Version(row)
			if err != nil {
				return err
			}
			if ts <= 0 || ts > base.CommitTS || ordinal == 0 {
				return storage.ErrCorruptStorage
			}
			if ts > base.PrunedCommitTS {
				index, found := slices.BinarySearchFunc(transactions, ts, func(transaction checkpointBaseTransaction, timestamp model.Timestamp) int {
					return cmp.Compare(transaction.timestamp, timestamp)
				})
				if !found || transactions[index].retained == transactions[index].applied {
					return storage.ErrCorruptStorage
				}
				transactions[index].retained++
			}
			if kind == storage.MutationUpsert {
				return nil
			}
			return verifyCheckpointTombstone(ctx, block, row)
		}); err != nil {
			return recoveredPosition{}, err
		}
	}
	for _, transaction := range transactions {
		if transaction.timestamp > base.Floor && transaction.applied != transaction.retained {
			return recoveredPosition{}, storage.ErrCorruptStorage
		}
	}
	return recoveredPosition{LogPosition: base.Position, CommitTS: base.CommitTS}, nil
}

func verifyCheckpointTombstone(ctx context.Context, block *paxBlock, row int) error {
	for column := range block.columns {
		id := model.ColumnID(binary.LittleEndian.Uint32(block.descriptors[column*paxColumnSize:]))
		vector, err := block.Column(ctx, id)
		if err != nil {
			return err
		}
		if !vector.IsNull(row) {
			return storage.ErrCorruptStorage
		}
	}
	return nil
}
