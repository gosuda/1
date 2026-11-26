package schema

import (
	"context"
	"encoding/binary"
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const sequenceRecordBytes = 80
const sequenceAllocatorBytes = 32

type SequenceStoreOptions struct {
	Target                               storage.ObjectRef
	Encoder                              storage.KeyEncoder
	MetadataBytes, RecordBytes, KeyBytes uint64
	MaxIncarnations, MaxCache            uint64
}

// SequenceStore borrows Encoder and owns no transactions, caches, or locks.
// Target must be an internal collection distinct from every logical sequence
// ObjectID: parent schema locks span all generations of their logical object.
// Every mutating call borrows one caller-owned atomic PCC statement. No candidate
// result is durable until its caller commits; indeterminate commits retain both
// that transaction and its candidate, rather than allocating again.
type SequenceStore struct {
	options    SequenceStoreOptions
	normalizer *txn.RowNormalizer
}

func NewSequenceStore(ctx context.Context, options SequenceStoreOptions) (*SequenceStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := options.Target.Validate(); err != nil {
		return nil, err
	}
	invalidMetadata := options.MetadataBytes == 0 || options.MetadataBytes > uint64(math.MaxInt)
	invalidRecord := options.RecordBytes < sequenceRecordBytes || options.RecordBytes > uint64(math.MaxInt)/8
	invalidKey := options.KeyBytes < 9 || options.KeyBytes > options.RecordBytes
	invalidCapacity := options.MaxIncarnations == 0 || options.MaxCache == 0 || options.MaxCache > math.MaxInt64
	if mutationPolicyNil(options.Encoder) || invalidMetadata || invalidRecord || invalidKey || invalidCapacity {
		return nil, sequenceLimitError()
	}
	if _, err := options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(jobLogicalKey(1, math.MaxUint64))}, options.KeyBytes); err != nil {
		return nil, err
	}
	columns := jobColumns()
	n, err := txn.NewRowNormalizer(ctx, options.Target, []txn.ColumnRule{{Layout: columns[0], KeyPosition: 1}, {Layout: columns[1]}}, options.MetadataBytes)
	if err != nil {
		return nil, err
	}
	return &SequenceStore{options: options, normalizer: n}, nil
}

func (s *SequenceStore) Layouts() []storage.ObjectLayout {
	return []storage.ObjectLayout{s.normalizer.Layout()}
}
func (s *SequenceStore) KeyLayout() []storage.KeyColumn { return s.normalizer.KeyLayout() }
func (s *SequenceStore) Target() storage.ObjectRef      { return s.options.Target }
func (s *SequenceStore) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target != s.options.Target {
		return nil, storage.ErrInvalidLayout
	}
	return s.normalizer, nil
}

type sequenceMutationResolver struct {
	store *SequenceStore
	base  txn.MutationResolver
}

func (s *SequenceStore) WithBaseResolver(base txn.MutationResolver) txn.MutationResolver {
	return sequenceMutationResolver{s, base}
}
func (r sequenceMutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if target.ObjectID == r.store.options.Target.ObjectID {
		return r.store.ResolveMutation(ctx, target)
	}
	if r.base == nil {
		return nil, storage.ErrInvalidLayout
	}
	return r.base.ResolveMutation(ctx, target)
}

func (s *SequenceStore) check(ctx context.Context, participant any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.normalizer == nil || mutationPolicyNil(participant) {
		return &model.Error{Code: "22023", Message: "sequence store requires a transaction or reader"}
	}
	return nil
}

func (s *SequenceStore) readRecord(ctx context.Context, reader JobReader, kind byte, id uint64) ([]byte, error) {
	if err := s.check(ctx, reader); err != nil {
		return nil, err
	}
	key, err := s.options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(jobLogicalKey(kind, id))}, s.options.KeyBytes)
	if err != nil {
		return nil, err
	}
	if uint64(cap(key)) > s.options.KeyBytes {
		return nil, sequenceLimitError()
	}
	row, err := reader.Get(ctx, storage.ReadRequest{Target: s.options.Target, Key: key, ColumnIDs: []model.ColumnID{2}, Layout: jobColumns()})
	if err != nil {
		return nil, err
	}
	if row.Deleted {
		return nil, storage.ErrRowNotFound
	}
	if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].Type != model.TypeBytes {
		return nil, sequenceCorruptError(nil)
	}
	if uint64(len(row.Values[0].BytesVal)) > s.options.RecordBytes {
		return nil, sequenceCorruptError(nil)
	}
	return row.Values[0].BytesVal, nil
}

func (s *SequenceStore) writeRecord(ctx context.Context, transaction txn.MutationApplier, op storage.MutationOp, kind byte, id uint64, payload []byte) error {
	if err := s.check(ctx, transaction); err != nil {
		return err
	}
	logical := jobLogicalKey(kind, id)
	key, err := s.options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(logical)}, s.options.KeyBytes)
	if err != nil {
		return err
	}
	if uint64(cap(key)) > s.options.KeyBytes || uint64(len(payload)) > s.options.RecordBytes {
		return sequenceLimitError()
	}
	mutation := storage.Mutation{Op: op, Target: s.options.Target, Key: key}
	if op != storage.MutationDelete {
		mutation.ColumnIDs = []model.ColumnID{1, 2}
		mutation.Values = []model.Value{model.BytesVal(logical), model.BytesVal(payload)}
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}

func (s *SequenceStore) Get(ctx context.Context, reader JobReader, id SequenceIncarnation) (SequenceState, error) {
	if id == 0 {
		return SequenceState{}, sequenceUnavailableError()
	}
	data, err := s.readRecord(ctx, reader, 1, uint64(id))
	if err != nil {
		return SequenceState{}, err
	}
	return decodeSequence(data, id, s.options.MaxCache)
}

func decodeSequence(data []byte, id SequenceIncarnation, maxCache uint64) (SequenceState, error) {
	if len(data) != sequenceRecordBytes || string(data[:8]) != "SQST0001" || data[72] > 1 || data[73] > 1 || data[74] > 1 {
		return SequenceState{}, sequenceCorruptError(nil)
	}
	for _, b := range data[75:] {
		if b != 0 {
			return SequenceState{}, sequenceCorruptError(nil)
		}
	}
	state := SequenceState{
		Incarnation: SequenceIncarnation(binary.LittleEndian.Uint64(data[8:16])),
		Revision:    binary.LittleEndian.Uint64(data[16:24]),
		Config: SequenceConfig{
			Start:     int64(binary.LittleEndian.Uint64(data[24:32])),
			Increment: int64(binary.LittleEndian.Uint64(data[32:40])),
			Min:       int64(binary.LittleEndian.Uint64(data[40:48])),
			Max:       int64(binary.LittleEndian.Uint64(data[48:56])),
			Cache:     int64(binary.LittleEndian.Uint64(data[56:64])),
			Cycle:     data[72] == 1,
		},
		Last:   int64(binary.LittleEndian.Uint64(data[64:72])),
		Called: data[73] == 1, Retired: data[74] == 1,
	}
	invalidIdentity := id == 0 || state.Incarnation != id || state.Revision == 0
	invalidValue := state.Last < state.Config.Min || state.Last > state.Config.Max
	if invalidIdentity || invalidValue || state.Config.validate(maxCache) != nil {
		return SequenceState{}, sequenceCorruptError(nil)
	}
	changedInitial := state.Called || state.Retired || state.Last != state.Config.Start
	if state.Revision == 1 && changedInitial {
		return SequenceState{}, sequenceCorruptError(nil)
	}
	return state, nil
}

func encodeSequence(state SequenceState) []byte {
	data := make([]byte, sequenceRecordBytes)
	copy(data, "SQST0001")
	binary.LittleEndian.PutUint64(data[8:16], uint64(state.Incarnation))
	binary.LittleEndian.PutUint64(data[16:24], state.Revision)
	binary.LittleEndian.PutUint64(data[24:32], uint64(state.Config.Start))
	binary.LittleEndian.PutUint64(data[32:40], uint64(state.Config.Increment))
	binary.LittleEndian.PutUint64(data[40:48], uint64(state.Config.Min))
	binary.LittleEndian.PutUint64(data[48:56], uint64(state.Config.Max))
	binary.LittleEndian.PutUint64(data[56:64], uint64(state.Config.Cache))
	binary.LittleEndian.PutUint64(data[64:72], uint64(state.Last))
	if state.Config.Cycle {
		data[72] = 1
	}
	if state.Called {
		data[73] = 1
	}
	if state.Retired {
		data[74] = 1
	}
	return data
}
func (s *SequenceStore) writeState(ctx context.Context, transaction txn.MutationApplier, op storage.MutationOp, state SequenceState) error {
	var data []byte
	if op != storage.MutationDelete {
		data = encodeSequence(state)
	}
	return s.writeRecord(ctx, transaction, op, 1, uint64(state.Incarnation), data)
}

type sequenceAllocator struct{ high, count, revision uint64 }

func (s *SequenceStore) readAllocator(ctx context.Context, reader JobReader) (sequenceAllocator, error) {
	data, err := s.readRecord(ctx, reader, 0, 0)
	if err != nil {
		return sequenceAllocator{}, err
	}
	if len(data) != sequenceAllocatorBytes || string(data[:8]) != "SQAL0001" {
		return sequenceAllocator{}, sequenceCorruptError(nil)
	}
	a := sequenceAllocator{high: binary.LittleEndian.Uint64(data[8:16]), count: binary.LittleEndian.Uint64(data[16:24]), revision: binary.LittleEndian.Uint64(data[24:32])}
	changedInitial := a.revision == 1 && (a.high != 0 || a.count != 0)
	if a.revision == 0 || a.count > a.high || changedInitial {
		return sequenceAllocator{}, sequenceCorruptError(nil)
	}
	return a, nil
}
func (s *SequenceStore) writeAllocator(ctx context.Context, transaction txn.MutationApplier, op storage.MutationOp, a sequenceAllocator) error {
	data := make([]byte, sequenceAllocatorBytes)
	copy(data, "SQAL0001")
	binary.LittleEndian.PutUint64(data[8:16], a.high)
	binary.LittleEndian.PutUint64(data[16:24], a.count)
	binary.LittleEndian.PutUint64(data[24:32], a.revision)
	return s.writeRecord(ctx, transaction, op, 0, 0, data)
}
func sequenceCorruptError(cause error) error {
	return &model.Error{Code: "XX001", Message: "invalid durable sequence record", Cause: errors.Join(storage.ErrCorruptStorage, cause)}
}
