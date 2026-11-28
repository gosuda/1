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
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type checkpointMutationIdentity struct {
	ordinal uint64
	digest  [32]byte
	seen    bool
}

func (check *checkpointVerification) verifyDecision(ctx context.Context, commits, runs *os.Root, state databaseState, previous recoveredPosition) (_ recoveredPosition, err error) {
	position := previous.LogPosition + 1
	if err := check.consume("commits/"+commitName(position), nil); err != nil {
		return recoveredPosition{}, err
	}
	if err := check.consume("runs/"+runManifestName(position), nil); err != nil {
		return recoveredPosition{}, err
	}
	record, err := openCommit(ctx, commits, position, check.limits.recovery().Batch)
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

type checkpointVersion struct {
	timestamp model.Timestamp
	ordinal   uint64
}

func (version checkpointVersion) Compare(other checkpointVersion) int {
	if order := cmp.Compare(version.timestamp, other.timestamp); order != 0 {
		return order
	}
	return cmp.Compare(version.ordinal, other.ordinal)
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
	unit := uint64(unsafe.Sizeof(checkpointBaseTransaction{}))
	if base.Position > uint64(math.MaxInt) || base.Position > (check.limits.IdentityBytes-check.transactions.NodeBytes())/unit {
		return recoveredPosition{}, columnar.ErrBatchBudget
	}
	transactions := make([]checkpointBaseTransaction, int(base.Position))
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
		transactions[record.Result.LogPosition-1] = checkpointBaseTransaction{timestamp: record.Result.CommitTS, applied: record.Result.AppliedCount}
	}
	remaining := check.limits.IdentityBytes - check.transactions.NodeBytes() - base.Position*unit
	var versions ordered.Tree[checkpointVersion, struct{}]
	for _, descriptor := range base.Files {
		if err := check.verifyRun(ctx, runs, descriptor, nil, func(block *paxBlock, row int) error {
			_, _, ts, ordinal, kind, err := block.Version(row)
			if err != nil {
				return err
			}
			index, found := slices.BinarySearchFunc(transactions, ts, func(transaction checkpointBaseTransaction, timestamp model.Timestamp) int {
				return cmp.Compare(transaction.timestamp, timestamp)
			})
			if !found || transactions[index].retained == transactions[index].applied {
				return storage.ErrCorruptStorage
			}
			version := checkpointVersion{timestamp: ts, ordinal: ordinal}
			if _, duplicate := versions.Get(version); duplicate {
				return storage.ErrCorruptStorage
			}
			next, _, err := versions.Set(version, struct{}{}, remaining-versions.NodeBytes())
			if err != nil {
				return err
			}
			versions = next
			transactions[index].retained++
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
