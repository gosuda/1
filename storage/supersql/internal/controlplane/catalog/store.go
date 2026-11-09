package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The top three uint64 ObjectIDs are reserved for catalog collections. User and
// legacy object IDs occupy [1, NamespaceObjectID); no IDs are remapped.
// model.CatalogCollectionObjectID is the shared floor the transaction manager
// uses to recognize a commit that rewrites catalog rows.
const (
	NamespaceObjectID  model.ObjectID = model.CatalogCollectionObjectID
	DescriptorObjectID model.ObjectID = model.CatalogCollectionObjectID + 1
	OIDObjectID        model.ObjectID = model.CatalogCollectionObjectID + 2
)

var (
	ErrDuplicateObject   = &model.Error{Code: "42710", Message: "catalog object already exists"}
	ErrUnknownObject     = &model.Error{Code: "42704", Message: "catalog object does not exist"}
	ErrDescriptorChanged = &model.Error{Code: "40001", Message: "catalog descriptor version changed"}
)

type Store struct {
	encoder storage.KeyEncoder
	limits  Limits
	// reads counts catalog rows read through this store, including every row a
	// snapshot namespace scan visits. It lets a caller prove that a repeated
	// statement re-reads no catalog metadata.
	reads atomic.Uint64
	// typeScans counts namespace scans over a schema's ObjectType children. The
	// declared-type runtime performs exactly this scan, so a caller can prove a
	// repeated statement does not rebuild it.
	typeScans atomic.Uint64
}

// TypeNamespaceScans reports how many ObjectType namespace scans this store has
// served since it was created.
func (s *Store) TypeNamespaceScans() uint64 {
	if s == nil {
		return 0
	}
	return s.typeScans.Load()
}

// ReadCount reports how many catalog rows this store has read since it was
// created.
func (s *Store) ReadCount() uint64 {
	if s == nil {
		return 0
	}
	return s.reads.Load()
}

func New(encoder storage.KeyEncoder, limits Limits) (*Store, error) {
	if encoder == nil {
		return nil, storage.ErrInvalidLayout
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	return &Store{encoder: encoder, limits: limits}, nil
}
func target(id model.ObjectID) storage.ObjectRef {
	return storage.ObjectRef{ObjectID: id, Generation: 1, DescriptorVersion: 1, EncodingVersion: EncodingVersion}
}
func columns() []storage.ColumnLayout {
	return []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}, {ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
}
func keyLayout() []storage.KeyColumn {
	return []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
}

// KeyLayout applies to all three catalog collections and returns owned metadata.
func (s *Store) KeyLayout() []storage.KeyColumn { return keyLayout() }

// Layouts returns owned declarations, ordered by target, for a common atomic batch.
func (s *Store) Layouts() []storage.ObjectLayout {
	return []storage.ObjectLayout{{Target: target(NamespaceObjectID), Columns: columns()}, {Target: target(DescriptorObjectID), Columns: columns()}, {Target: target(OIDObjectID), Columns: columns()}}
}
func numberKey(id uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, id)
	return key
}
func (s *Store) nameKey(name model.NamespaceName) ([]byte, error) {
	if err := model.ValidateNamespaceName(name); err != nil {
		return nil, err
	}
	if uint64(len(name.Name))+uint64(len(name.Signature))+17 > s.limits.MaxKeyBytes {
		return nil, model.ErrDescriptorLimit
	}
	key := make([]byte, 17+len(name.Name)+len(name.Signature))
	binary.BigEndian.PutUint64(key, uint64(name.Parent))
	key[8] = byte(name.Kind)
	binary.BigEndian.PutUint32(key[9:], uint32(len(name.Name)))
	copy(key[13:], name.Name)
	offset := 13 + len(name.Name)
	binary.BigEndian.PutUint32(key[offset:], uint32(len(name.Signature)))
	copy(key[offset+4:], name.Signature)
	return key, nil
}
func decodeName(key []byte) (model.NamespaceName, error) {
	if len(key) < 17 {
		return model.NamespaceName{}, ErrCorruptCatalog
	}
	n := uint64(binary.BigEndian.Uint32(key[9:]))
	if n > uint64(len(key)-17) {
		return model.NamespaceName{}, ErrCorruptCatalog
	}
	offset := 13 + int(n)
	m := uint64(binary.BigEndian.Uint32(key[offset:]))
	if m != uint64(len(key)-offset-4) {
		return model.NamespaceName{}, ErrCorruptCatalog
	}
	name := model.NamespaceName{Parent: model.ObjectID(binary.BigEndian.Uint64(key)), Kind: model.ObjectKind(key[8]), Name: string(key[13:offset]), Signature: string(key[offset+4:])}
	if err := model.ValidateNamespaceName(name); err != nil {
		return model.NamespaceName{}, ErrCorruptCatalog
	}
	return name, nil
}
func reference(id model.ObjectID, version uint64) []byte {
	data := make([]byte, 17)
	data[0] = byte(EncodingVersion)
	binary.BigEndian.PutUint64(data[1:], uint64(id))
	binary.BigEndian.PutUint64(data[9:], version)
	return data
}
func decodeReference(data []byte) (model.ObjectID, uint64, error) {
	if len(data) != 17 || data[0] != byte(EncodingVersion) {
		return 0, 0, ErrCorruptCatalog
	}
	id := model.ObjectID(binary.BigEndian.Uint64(data[1:]))
	version := binary.BigEndian.Uint64(data[9:])
	if id == 0 || id >= NamespaceObjectID || version == 0 {
		return 0, 0, ErrCorruptCatalog
	}
	return id, version, nil
}
func (s *Store) encoded(ctx context.Context, key []byte) ([]byte, error) {
	return s.encoder.EncodePrefix(ctx, keyLayout(), []model.Value{model.BytesVal(key)}, s.limits.MaxKeyBytes)
}
func (s *Store) mutation(ctx context.Context, op storage.MutationOp, id model.ObjectID, key, payload []byte) (storage.Mutation, error) {
	encoded, err := s.encoded(ctx, key)
	if err != nil {
		return storage.Mutation{}, err
	}
	m := storage.Mutation{Op: op, Target: target(id), Key: encoded}
	if op != storage.MutationDelete {
		m.ColumnIDs = []model.ColumnID{1, 2}
		m.Values = []model.Value{model.BytesVal(key), model.BytesVal(payload)}
	}
	return m, nil
}

// Change is an owned mutation group. Stage every mutation and Layouts in one
// caller transaction/statement. It has no independent commit or publication.
type Change struct {
	Layouts   []storage.ObjectLayout
	Mutations []storage.Mutation
}

func (s *Store) change(ctx context.Context, old, next *model.Descriptor) (Change, error) {
	result := Change{Layouts: s.Layouts()}
	appendMutation := func(op storage.MutationOp, id model.ObjectID, key, payload []byte) error {
		m, err := s.mutation(ctx, op, id, key, payload)
		if err == nil {
			result.Mutations = append(result.Mutations, m)
		}
		return err
	}
	if old != nil && (next == nil || old.Name() != next.Name()) {
		key, err := s.nameKey(old.Name())
		if err != nil {
			return Change{}, err
		}
		if err := appendMutation(storage.MutationDelete, NamespaceObjectID, key, nil); err != nil {
			return Change{}, err
		}
	}
	if next == nil {
		if err := appendMutation(storage.MutationDelete, DescriptorObjectID, numberKey(uint64(old.ID())), nil); err != nil {
			return Change{}, err
		}
		if err := appendMutation(storage.MutationDelete, OIDObjectID, numberKey(uint64(old.OID())), nil); err != nil {
			return Change{}, err
		}
		return result, nil
	}
	if next.ID() >= NamespaceObjectID {
		return Change{}, model.ErrInvalidDescriptor
	}
	data, err := EncodeDescriptor(ctx, next, s.limits)
	if err != nil {
		return Change{}, err
	}
	key, err := s.nameKey(next.Name())
	if err != nil {
		return Change{}, err
	}
	namespaceOp, descriptorOp := storage.MutationInsert, storage.MutationInsert
	if old != nil {
		descriptorOp = storage.MutationUpdate
		if old.Name() == next.Name() {
			namespaceOp = storage.MutationUpdate
		}
	}
	if err := appendMutation(namespaceOp, NamespaceObjectID, key, reference(next.ID(), next.Version())); err != nil {
		return Change{}, err
	}
	if err := appendMutation(descriptorOp, DescriptorObjectID, numberKey(uint64(next.ID())), data); err != nil {
		return Change{}, err
	}
	if err := appendMutation(descriptorOp, OIDObjectID, numberKey(uint64(next.OID())), reference(next.ID(), next.Version())); err != nil {
		return Change{}, err
	}
	return result, nil
}

// Create checks snapshot-visible conflicts. The caller must hold namespace/ID/OID
// dependency locks through commit; Insert also defends existence during staging.
func (s *Store) Create(ctx context.Context, snapshot *CatalogSnapshot, next *model.Descriptor) (Change, error) {
	if next == nil || next.Version() != 1 {
		return Change{}, model.ErrInvalidDescriptor
	}
	if err := s.checkSnapshot(snapshot); err != nil {
		return Change{}, err
	}
	if err := s.vacantName(ctx, snapshot, next.Name()); err != nil {
		return Change{}, err
	}
	if err := snapshot.requireAbsent(ctx, DescriptorObjectID, numberKey(uint64(next.ID()))); err != nil {
		return Change{}, err
	}
	if err := snapshot.requireAbsent(ctx, OIDObjectID, numberKey(uint64(next.OID()))); err != nil {
		return Change{}, err
	}
	return s.change(ctx, nil, next)
}

// Update also implements rename. Version must advance by exactly one; SQL OID
// and logical ObjectID cannot change. Physical generation is independently versioned.
func (s *Store) Update(ctx context.Context, snapshot *CatalogSnapshot, expected uint64, next *model.Descriptor) (Change, error) {
	if next == nil || expected == ^uint64(0) || next.Version() != expected+1 {
		return Change{}, model.ErrInvalidDescriptor
	}
	if err := s.checkSnapshot(snapshot); err != nil {
		return Change{}, err
	}
	old, err := snapshot.Descriptor(ctx, next.ID())
	if err != nil {
		return Change{}, err
	}
	if old.Descriptor.Version() != expected {
		return Change{}, ErrDescriptorChanged
	}
	if err := old.Descriptor.ValidateSuccessor(ctx, next); err != nil {
		return Change{}, err
	}
	if old.Descriptor.Name() != next.Name() {
		if err := s.vacantName(ctx, snapshot, next.Name()); err != nil {
			return Change{}, err
		}
	}
	return s.change(ctx, old.Descriptor, next)
}
func (s *Store) Drop(ctx context.Context, snapshot *CatalogSnapshot, id model.ObjectID, expected uint64) (Change, error) {
	if err := s.checkSnapshot(snapshot); err != nil {
		return Change{}, err
	}
	old, err := snapshot.Descriptor(ctx, id)
	if err != nil {
		return Change{}, err
	}
	if old.Descriptor.Version() != expected {
		return Change{}, ErrDescriptorChanged
	}
	return s.change(ctx, old.Descriptor, nil)
}
func (s *Store) checkSnapshot(snapshot *CatalogSnapshot) error {
	if snapshot == nil || snapshot.store != s {
		return storage.ErrInvalidTarget
	}
	return nil
}
func (s *Store) read(ctx context.Context, reader storage.SnapshotReader, id model.ObjectID, key []byte) (model.Row, error) {
	s.reads.Add(1)
	encoded, err := s.encoded(ctx, key)
	if err != nil {
		return model.Row{}, err
	}
	row, err := reader.Get(ctx, storage.ReadRequest{Target: target(id), Key: encoded, ColumnIDs: []model.ColumnID{1, 2}, Layout: columns()})
	if err != nil {
		return model.Row{}, err
	}
	if row.Deleted || len(row.Values) != 2 {
		return model.Row{}, fmt.Errorf("%w: invalid catalog row", ErrCorruptCatalog)
	}
	bytesColumns := row.Values[0].Type == model.TypeBytes && row.Values[1].Type == model.TypeBytes
	nonNullColumns := !row.Values[0].IsNull && !row.Values[1].IsNull
	if !bytesColumns || !nonNullColumns || !bytes.Equal(row.Values[0].BytesVal, key) {
		return model.Row{}, fmt.Errorf("%w: invalid catalog row", ErrCorruptCatalog)
	}
	return row, nil
}
