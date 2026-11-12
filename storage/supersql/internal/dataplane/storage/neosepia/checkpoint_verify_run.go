package neosepia

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"math"
	"os"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type checkpointTarget storage.ObjectRef

func (target checkpointTarget) Compare(other checkpointTarget) int {
	if order := cmp.Compare(target.ObjectID, other.ObjectID); order != 0 {
		return order
	}
	if order := cmp.Compare(target.Generation, other.Generation); order != 0 {
		return order
	}
	if order := cmp.Compare(target.DescriptorVersion, other.DescriptorVersion); order != 0 {
		return order
	}
	return cmp.Compare(target.EncodingVersion, other.EncodingVersion)
}

func (check *checkpointVerification) verifyRun(ctx context.Context, root *os.Root, descriptor preparedRun, layout *storage.ObjectLayout, visit func(*paxBlock, int) error) (err error) {
	if err := check.consume("runs/"+descriptor.Name, &descriptor); err != nil {
		return err
	}
	file, err := root.Open(descriptor.Name)
	if err != nil {
		return err
	}
	run, err := openSST(ctx, file, check.limits.preparation().File)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	defer func() { err = errors.Join(err, run.Close()) }()
	if !validFragmentBounds(descriptor, check.limits.KeyBytes) {
		return storage.ErrCorruptStorage
	}
	run.descriptor = descriptor
	if run.target != descriptor.Target || run.rows != descriptor.Rows {
		return storage.ErrCorruptStorage
	}
	if uint64(run.rangeCount) > check.limits.ReadMergeBytes {
		return columnar.ErrBatchBudget
	}
	ranges := make([]bool, run.rangeCount)
	var previousTS model.Timestamp
	var previousOrdinal uint64
	for index := range run.count {
		block, err := run.ReadBlock(ctx, index, check.limits.ReadBlockBytes, nil)
		if err != nil {
			return err
		}
		lastTS, lastOrdinal, verifyErr := check.verifyBlock(ctx, run, block, index, previousTS, previousOrdinal, ranges, layout, visit)
		if err := errors.Join(verifyErr, block.Close()); err != nil {
			return err
		}
		previousTS, previousOrdinal = lastTS, lastOrdinal
	}
	for _, seen := range ranges {
		if !seen {
			return storage.ErrCorruptStorage
		}
	}
	return nil
}

func (check *checkpointVerification) verifyBlock(ctx context.Context, run *sstFile, block *paxBlock, index int, previousTS model.Timestamp, previousOrdinal uint64, ranges []bool, layout *storage.ObjectLayout, visit func(*paxBlock, int) error) (model.Timestamp, uint64, error) {
	if err := check.verifyBlockLayout(ctx, block, layout); err != nil {
		return 0, 0, err
	}
	entry := run.entry(index)
	minimum, _, firstTS, firstOrdinal, _, err := block.Version(0)
	if err != nil {
		return 0, 0, err
	}
	maximum, _, lastTS, lastOrdinal, _, err := block.Version(block.Rows() - 1)
	if err != nil {
		return 0, 0, err
	}
	if !bytes.Equal(minimum, run.key(entry.minimum)) || !bytes.Equal(maximum, run.key(entry.maximum)) {
		return 0, 0, storage.ErrCorruptStorage
	}
	if index > 0 && bytes.Equal(minimum, run.key(run.entry(index-1).maximum)) && storage.CompareVersion(firstTS, firstOrdinal, previousTS, previousOrdinal) >= 0 {
		return 0, 0, storage.ErrCorruptStorage
	}
	coverage := maximum
	minCommitTS, maxCommitTS := firstTS, firstTS
	for row := range block.Rows() {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		key, end, ts, ordinal, kind, err := block.Version(row)
		if err != nil {
			return 0, 0, err
		}
		minCommitTS, maxCommitTS = min(minCommitTS, ts), max(maxCommitTS, ts)
		if uint64(len(key)) > check.limits.KeyBytes || uint64(len(end)) > check.limits.KeyBytes {
			return 0, 0, columnar.ErrBatchBudget
		}
		if ts > check.manifest.SnapshotTS {
			return 0, 0, storage.ErrCorruptStorage
		}
		if kind == storage.MutationDeleteRange {
			if bytes.Compare(end, coverage) > 0 {
				coverage = end
			}
			if err := verifyCheckpointRange(run, key, end, ts, ordinal, ranges); err != nil {
				return 0, 0, err
			}
		}
		if run.containsKey(key) {
			if err := visit(block, row); err != nil {
				return 0, 0, err
			}
		}
	}
	if !bytes.Equal(coverage, run.key(entry.coverage)) || (run.entryBytes == sstEntrySize && (minCommitTS != entry.minCommitTS || maxCommitTS != entry.maxCommitTS)) {
		return 0, 0, storage.ErrCorruptStorage
	}
	return lastTS, lastOrdinal, nil
}

func verifyCheckpointRange(run *sstFile, key, end []byte, ts model.Timestamp, ordinal uint64, seen []bool) error {
	low := sort.Search(run.rangeCount, func(index int) bool {
		entry := run.rangeEntry(index)
		if order := bytes.Compare(run.key(entry.end), end); order != 0 {
			return order < 0
		}
		if order := storage.CompareVersion(entry.timestamp, entry.ordinal, ts, ordinal); order != 0 {
			return order < 0
		}
		return bytes.Compare(run.key(entry.start), key) >= 0
	})
	if low == run.rangeCount || seen[low] {
		return storage.ErrCorruptStorage
	}
	entry := run.rangeEntry(low)
	if entry.timestamp != ts || entry.ordinal != ordinal || !bytes.Equal(run.key(entry.start), key) || !bytes.Equal(run.key(entry.end), end) {
		return storage.ErrCorruptStorage
	}
	seen[low] = true
	return nil
}

func (check *checkpointVerification) verifyBlockLayout(ctx context.Context, block *paxBlock, layout *storage.ObjectLayout) error {
	if layout != nil && (layout.Target != block.target || len(layout.Columns) != block.columns) {
		return storage.ErrCorruptStorage
	}
	digest := sha256.New()
	for index := range block.columns {
		id := model.ColumnID(binary.LittleEndian.Uint32(block.descriptors[index*paxColumnSize:]))
		vector, err := block.Column(ctx, id)
		if err != nil {
			return err
		}
		if err := vector.Validate(); err != nil {
			return errors.Join(storage.ErrCorruptStorage, err)
		}
		if layout != nil {
			column := layout.Columns[index]
			if column.ID != id || !scanTypeMatches(vector, model.TypeLayout{Type: column.Type, Children: column.Children}) {
				return storage.ErrCorruptStorage
			}
		}
		var columnID [4]byte
		binary.LittleEndian.PutUint32(columnID[:], uint32(id))
		digest.Write(columnID[:])
		if err := checkpointTypeDigest(ctx, digest, &vector); err != nil {
			return err
		}
		descriptor, err := parsePAXDescriptor(block.descriptors[index*paxColumnSize:], false)
		if err != nil {
			return err
		}
		if descriptor.flags&paxZoneInteger != 0 {
			minimum, maximum := int64(math.MaxInt64), int64(math.MinInt64)
			for row := range vector.Length {
				if err := ctx.Err(); err != nil {
					return err
				}
				if !vector.IsNull(row) {
					value := vector.Int64[vector.ValueIndex(row)]
					minimum, maximum = min(minimum, value), max(maximum, value)
				}
			}
			if minimum != descriptor.min || maximum != descriptor.max {
				return storage.ErrCorruptStorage
			}
		}
	}
	var signature [32]byte
	digest.Sum(signature[:0])
	target := checkpointTarget(block.target)
	if previous, found := check.layouts.Get(target); found {
		if previous != signature {
			return storage.ErrCorruptStorage
		}
		return nil
	}
	remaining := check.limits.MetadataBytes - check.metadata
	if check.layouts.NodeBytes() > remaining {
		return columnar.ErrBatchBudget
	}
	next, _, err := check.layouts.Set(target, signature, remaining-check.layouts.NodeBytes())
	if err != nil {
		return err
	}
	check.layouts = next
	return nil
}

func checkpointTypeDigest(ctx context.Context, digest hash.Hash, vector *columnar.Vector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var data [21]byte
	binary.LittleEndian.PutUint32(data[0:4], uint32(vector.Type.ID))
	binary.LittleEndian.PutUint32(data[4:8], uint32(vector.Type.Modifier))
	binary.LittleEndian.PutUint32(data[8:12], uint32(vector.Type.Collation))
	data[12] = byte(vector.Type.Storage)
	binary.LittleEndian.PutUint64(data[13:21], uint64(len(vector.Children)))
	digest.Write(data[:])
	for i := range vector.Children {
		if err := checkpointTypeDigest(ctx, digest, &vector.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

type checkpointRowHasher struct {
	values  []model.Value
	columns []model.ColumnID
	digest  hash.Hash
	maximum uint64
	fixed   uint64
}

func newCheckpointRowHasher(layouts []storage.ObjectLayout, maximum uint64) (*checkpointRowHasher, error) {
	var count int
	for _, layout := range layouts {
		count = max(count, len(layout.Columns))
	}
	unit := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.ColumnID(0)))
	if uint64(count) > maximum/unit {
		return nil, columnar.ErrBatchBudget
	}
	return &checkpointRowHasher{
		values: make([]model.Value, count), columns: make([]model.ColumnID, count),
		digest: sha256.New(), maximum: maximum, fixed: uint64(count) * unit,
	}, nil
}

func (hasher *checkpointRowHasher) rowDigest(ctx context.Context, block *paxBlock, row int) ([32]byte, error) {
	key, end, _, ordinal, kind, err := block.Version(row)
	if err != nil {
		return [32]byte{}, err
	}
	mutation := storage.Mutation{Target: block.target, Key: key, EndKey: end, Ordinal: ordinal, Op: kind}
	used := hasher.fixed
	defer clear(hasher.values)
	if kind == storage.MutationUpsert {
		mutation.ColumnIDs = hasher.columns[:block.columns]
		mutation.Values = hasher.values[:block.columns]
	}
	for index := range block.columns {
		id := model.ColumnID(binary.LittleEndian.Uint32(block.descriptors[index*paxColumnSize:]))
		vector, err := block.Column(ctx, id)
		if err != nil {
			return [32]byte{}, err
		}
		if kind != storage.MutationUpsert {
			if !vector.IsNull(row) {
				return [32]byte{}, storage.ErrCorruptStorage
			}
			continue
		}
		value, bytes, err := columnar.CopyValue(ctx, &vector, row, hasher.maximum-used)
		if err != nil {
			return [32]byte{}, err
		}
		used += bytes
		mutation.ColumnIDs[index], mutation.Values[index] = id, value
	}
	hasher.digest.Reset()
	if err := storage.EncodeMutation(hasher.digest, mutation); err != nil {
		return [32]byte{}, err
	}
	var result [32]byte
	hasher.digest.Sum(result[:0])
	return result, ctx.Err()
}
