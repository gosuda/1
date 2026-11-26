package statistics

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type Store struct {
	target      storage.ObjectRef
	encoder     storage.KeyEncoder
	limits      Limits
	maxKeyBytes uint64
	normalizer  *txn.RowNormalizer
}

// New borrows encoder. The host owns collection allocation and must register
// KeyLayout on this exact collection; statistics reserves no global ObjectID.
func New(ctx context.Context, target storage.ObjectRef, encoder storage.KeyEncoder, limits Limits, maxKeyBytes uint64) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if encoder == nil || target.EncodingVersion != EncodingVersion || maxKeyBytes == 0 {
		return nil, ErrInvalid
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if maxKeyBytes > limits.MaxBytes {
		return nil, ErrLimit
	}
	columns := []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}, {ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	normalizer, err := txn.NewRowNormalizer(ctx, target, []txn.ColumnRule{{Layout: columns[0], KeyPosition: 1}, {Layout: columns[1]}}, limits.MaxBytes)
	if err != nil {
		return nil, err
	}
	return &Store{target: target, encoder: encoder, limits: limits, maxKeyBytes: maxKeyBytes, normalizer: normalizer}, nil
}

func (s *Store) KeyLayout() []storage.KeyColumn {
	return []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
}
func (s *Store) Layouts() []storage.ObjectLayout {
	return []storage.ObjectLayout{{Target: s.target, Columns: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}, {ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}}}
}
func (s *Store) Limits() Limits {
	if s == nil {
		return Limits{}
	}
	return s.limits
}

// ResolveMutation admits the fixed publication layout for this collection.
// ANALYZE publishes through the caller transaction's ordinary mutation path,
// which resolves every target through the session's resolver chain.
func (s *Store) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || target != s.target {
		return nil, storage.ErrInvalidLayout
	}
	return s.normalizer, nil
}

type mutationResolver struct {
	store *Store
	base  txn.MutationResolver
}

// WithBaseResolver chains this collection's resolver ahead of base, matching
// the other fixed internal collections.
func (s *Store) WithBaseResolver(base txn.MutationResolver) txn.MutationResolver {
	return mutationResolver{s, base}
}
func (r mutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if target.ObjectID == r.store.target.ObjectID {
		return r.store.ResolveMutation(ctx, target)
	}
	if r.base == nil {
		return nil, storage.ErrInvalidLayout
	}
	return r.base.ResolveMutation(ctx, target)
}

func recordKey(target storage.ObjectRef) []byte {
	key := make([]byte, 36)
	binary.BigEndian.PutUint64(key, wireMagic)
	binary.BigEndian.PutUint64(key[8:], uint64(target.ObjectID))
	binary.BigEndian.PutUint64(key[16:], target.Generation)
	binary.BigEndian.PutUint64(key[24:], target.DescriptorVersion)
	binary.BigEndian.PutUint32(key[32:], target.EncodingVersion)
	return key
}

// Snapshot borrows a reader pinned at timestamp, with no mutable transaction
// overlay. The caller retains and closes it after every lookup has completed.
func (s *Store) Snapshot(reader storage.SnapshotReader, timestamp model.Timestamp) (*Snapshot, error) {
	if reader == nil || timestamp <= 0 {
		return nil, ErrInvalid
	}
	return &Snapshot{store: s, reader: reader, timestamp: timestamp}, nil
}

type Snapshot struct {
	store     *Store
	reader    storage.SnapshotReader
	timestamp model.Timestamp
}

func (s *Snapshot) Timestamp() model.Timestamp {
	if s == nil {
		return 0
	}
	return s.timestamp
}

func (s *Snapshot) Lookup(ctx context.Context, target storage.ObjectRef) (*Table, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := target.Validate(); err != nil {
		return nil, false, err
	}
	key := recordKey(target)
	encoded, err := s.store.encoder.EncodePrefix(ctx, s.store.KeyLayout(), []model.Value{model.BytesVal(key)}, s.store.maxKeyBytes)
	if err != nil {
		return nil, false, err
	}
	row, err := s.reader.Get(ctx, storage.ReadRequest{Target: s.store.target, Key: encoded, ColumnIDs: []model.ColumnID{1, 2}, Layout: s.store.Layouts()[0].Columns})
	if errors.Is(err, storage.ErrRowNotFound) {
		return nil, false, ctx.Err()
	}
	if err != nil {
		return nil, false, err
	}
	if row.Deleted || len(row.Values) != 2 {
		return nil, false, ErrInvalid
	}
	invalidTypes := row.Values[0].Type != model.TypeBytes || row.Values[1].Type != model.TypeBytes
	if invalidTypes || row.Values[0].IsNull || row.Values[1].IsNull || !bytes.Equal(row.Values[0].BytesVal, key) {
		return nil, false, ErrInvalid
	}
	t, err := Decode(ctx, row.Values[1].BytesVal, s.store.limits)
	if err != nil {
		return nil, false, err
	}
	if t.Target != target || t.Snapshot > s.timestamp || row.CommitTS > s.timestamp || row.CommitTS < t.Snapshot {
		return nil, false, ErrStale
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return t, true, nil
}

// Change transfers owned layouts and mutations to one caller transaction.
type Change struct {
	Layouts   []storage.ObjectLayout
	Mutations []storage.Mutation
}

// Publish requires the caller's PCC dependency on the encoded record key from
// before current was read through commit. current must be a fresh snapshot under
// that ownership; an older transaction snapshot cannot establish monotonicity.
// Stage the entire Change atomically; this method neither locks nor commits.
func (s *Store) Publish(ctx context.Context, current *Snapshot, next *Table) (Change, error) {
	if current == nil || current.store != s {
		return Change{}, ErrInvalid
	}
	if err := Validate(ctx, next, s.limits); err != nil {
		return Change{}, err
	}
	if next.Snapshot > current.timestamp {
		return Change{}, ErrStale
	}
	old, found, err := current.Lookup(ctx, next.Target)
	if err != nil {
		return Change{}, err
	}
	if found && (next.Epoch <= old.Epoch || next.Snapshot < old.Snapshot) {
		return Change{}, ErrStale
	}
	payload, err := Encode(ctx, next, s.limits)
	if err != nil {
		return Change{}, err
	}
	key := recordKey(next.Target)
	encoded, err := s.encoder.EncodePrefix(ctx, s.KeyLayout(), []model.Value{model.BytesVal(key)}, s.maxKeyBytes)
	if err != nil {
		return Change{}, err
	}
	op := storage.MutationInsert
	if found {
		op = storage.MutationReplace
	}
	if err := ctx.Err(); err != nil {
		return Change{}, err
	}
	return Change{Layouts: s.Layouts(), Mutations: []storage.Mutation{{Op: op, Target: s.target, Key: encoded, ColumnIDs: []model.ColumnID{1, 2}, Values: []model.Value{model.BytesVal(key), model.BytesVal(payload)}}}}, nil
}


// PublicationDependency is the exact PCC span that must be held before a fresh
// current snapshot is opened and until the resulting publication commits.
func (s *Store) PublicationDependency(target storage.ObjectRef) (txn.ReadDependency, error) {
	if s == nil {
		return txn.ReadDependency{}, ErrInvalid
	}
	if err := target.Validate(); err != nil {
		return txn.ReadDependency{}, err
	}
	key := model.BytesVal(recordKey(target))
	return txn.ReadDependency{
		Target:    s.target,
		KeyLayout: s.KeyLayout(),
		Spans: []storage.KeySpan{{
			Lower: storage.KeyBound{Values: []model.Value{key}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.BytesVal(bytes.Clone(key.BytesVal))}, Inclusive: true},
		}},
	}, nil
}
