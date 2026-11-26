package catalog

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type ReaderOwnership uint8

const (
	BorrowReader ReaderOwnership = iota + 1
	OwnReader
)

// PublishedDescriptor has only the publication boundary visible at this exact
// snapshot. A later version's timestamp (ValidTo) is unknowable here.
// PublishedAt is zero only for an unpublished transaction-overlay descriptor.
type PublishedDescriptor struct {
	Descriptor  *model.Descriptor
	PublishedAt model.Timestamp
}

// NamespaceReference is the name-index identity visible at this snapshot. It
// intentionally omits descriptor payload so callers can acquire descriptor
// locks before reading the descriptor row.
type NamespaceReference struct {
	Name    model.NamespaceName
	ID      model.ObjectID
	Version uint64
}

// CatalogSnapshot borrows an already-open reader at exactly Timestamp. The
// creator guarantees that binding, including the read fence/pin. Calls and Close
// are serialized by the caller. Results own all metadata and survive Close.
// No global or whole-catalog cache is retained.
type CatalogSnapshot struct {
	store            *Store
	reader           storage.SnapshotReader
	timestamp        model.Timestamp
	identity         txn.CatalogReadIdentity
	overlayVersion   uint64
	ownership        ReaderOwnership
	closed           bool
	allowUnpublished bool
	overlayByID      map[model.ObjectID]*model.Descriptor
	overlayByName    map[model.NamespaceName]*model.Descriptor
	overlayByOID     map[model.OID]*model.Descriptor
}

func (s *Store) Snapshot(reader storage.SnapshotReader, timestamp model.Timestamp, ownership ReaderOwnership) (*CatalogSnapshot, error) {
	validOwnership := ownership == BorrowReader || ownership == OwnReader
	if reader == nil || timestamp <= 0 || !validOwnership {
		return nil, storage.ErrSnapshotNotClosed
	}
	if exact, ok := reader.(interface{ Timestamp() model.Timestamp }); ok && exact.Timestamp() != timestamp {
		return nil, storage.ErrSnapshotNotClosed
	}
	snapshot := &CatalogSnapshot{store: s, reader: reader, timestamp: timestamp, ownership: ownership}
	if source, ok := reader.(interface {
		CatalogIdentity() txn.CatalogReadIdentity
	}); ok {
		snapshot.identity = source.CatalogIdentity()
	}
	return snapshot, nil
}

// CatalogIdentity names the exact catalog content this snapshot exposes. A zero
// identity means the snapshot's reader cannot name it, so callers must not key
// derived metadata by it. The snapshot's own overlay generation is folded in, so
// a statement that adds descriptors to this snapshot invalidates anything keyed
// by the identity it reported before the addition.
func (s *CatalogSnapshot) CatalogIdentity() txn.CatalogReadIdentity {
	if s == nil {
		return txn.CatalogReadIdentity{}
	}
	identity := s.identity
	identity.SnapshotOverlay = s.overlayVersion
	return identity
}

// TransactionReadView is a frozen overlay and its exact committed base snapshot.
// The caller declares namespace/descriptor/OID read dependencies before opening it.
type TransactionReadView interface {
	storage.SnapshotReader
	Timestamp() model.Timestamp
}

func (s *Store) TransactionSnapshot(reader TransactionReadView, ownership ReaderOwnership) (*CatalogSnapshot, error) {
	if reader == nil {
		return nil, storage.ErrSnapshotNotClosed
	}
	snapshot, err := s.Snapshot(reader, reader.Timestamp(), ownership)
	if err != nil {
		return nil, err
	}
	snapshot.allowUnpublished = true
	return snapshot, nil
}

// AddSessionOverlay adds validated session-owned descriptors to this borrowed
// snapshot without publishing them to the database catalog.
func (s *CatalogSnapshot) AddSessionOverlay(descriptors []*model.Descriptor) error {
	if s == nil || s.closed {
		return storage.ErrClosed
	}
	if len(descriptors) == 0 {
		return nil
	}
	if s.overlayByID == nil {
		s.overlayByID = make(map[model.ObjectID]*model.Descriptor, len(descriptors))
		s.overlayByName = make(map[model.NamespaceName]*model.Descriptor, len(descriptors))
		s.overlayByOID = make(map[model.OID]*model.Descriptor, len(descriptors))
	}
	s.overlayVersion++
	for _, descriptor := range descriptors {
		if descriptor == nil {
			return model.ErrInvalidDescriptor
		}
		if previous := s.overlayByID[descriptor.ID()]; previous != nil && previous.Name() != descriptor.Name() {
			delete(s.overlayByName, previous.Name())
			delete(s.overlayByOID, previous.OID())
		}
		if previous := s.overlayByName[descriptor.Name()]; previous != nil && previous.ID() != descriptor.ID() {
			return model.ErrInvalidDescriptor
		}
		if previous := s.overlayByOID[descriptor.OID()]; previous != nil && previous.ID() != descriptor.ID() {
			return model.ErrInvalidDescriptor
		}
		s.overlayByID[descriptor.ID()] = descriptor
		s.overlayByName[descriptor.Name()] = descriptor
		s.overlayByOID[descriptor.OID()] = descriptor
	}
	return nil
}
func (s *CatalogSnapshot) Timestamp() model.Timestamp { return s.timestamp }
func (s *CatalogSnapshot) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	reader := s.reader
	s.reader = nil
	if s.ownership == OwnReader {
		return reader.Close()
	}
	return nil
}
func (s *CatalogSnapshot) row(ctx context.Context, id model.ObjectID, key []byte) (model.Row, error) {
	if s.closed {
		return model.Row{}, storage.ErrClosed
	}
	row, err := s.store.read(ctx, s.reader, id, key)
	if catalogRowAbsent(err) {
		return model.Row{}, ErrUnknownObject
	}
	if err != nil {
		return model.Row{}, err
	}
	unpublishedCommittedRow := row.CommitTS == 0 && !s.allowUnpublished
	if row.CommitTS < 0 || unpublishedCommittedRow || row.CommitTS > s.timestamp {
		return model.Row{}, ErrCorruptCatalog
	}
	return row, nil
}
func (s *CatalogSnapshot) Descriptor(ctx context.Context, id model.ObjectID) (PublishedDescriptor, error) {
	if descriptor := s.overlayByID[id]; descriptor != nil {
		return PublishedDescriptor{Descriptor: descriptor}, nil
	}
	if id == 0 || id >= NamespaceObjectID {
		return PublishedDescriptor{}, ErrUnknownObject
	}
	row, err := s.row(ctx, DescriptorObjectID, numberKey(uint64(id)))
	if err != nil {
		return PublishedDescriptor{}, err
	}
	d, err := DecodeDescriptor(ctx, row.Values[1].BytesVal, s.store.limits)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	if d.ID() != id {
		return PublishedDescriptor{}, ErrCorruptCatalog
	}
	if row.CommitTS == 0 {
		source, ok := s.reader.(interface {
			ProvisionalBinding(uint64) (*model.ProvisionalBinding, error)
		})
		if !ok {
			return PublishedDescriptor{}, ErrCorruptCatalog
		}
		binding, err := source.ProvisionalBinding(row.Ordinal)
		if err != nil {
			return PublishedDescriptor{}, err
		}
		d = d.WithProvisionalBinding(binding)
	}
	return PublishedDescriptor{Descriptor: d, PublishedAt: row.CommitTS}, nil
}
func (s *CatalogSnapshot) resolveRow(ctx context.Context, row model.Row) (PublishedDescriptor, error) {
	id, version, err := decodeReference(row.Values[1].BytesVal)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	d, err := s.Descriptor(ctx, id)
	if errors.Is(err, ErrUnknownObject) {
		return PublishedDescriptor{}, ErrCorruptCatalog
	}
	if err != nil {
		return PublishedDescriptor{}, err
	}
	if d.Descriptor.Version() != version || d.PublishedAt != row.CommitTS {
		return PublishedDescriptor{}, ErrCorruptCatalog
	}
	return d, nil
}
func (s *CatalogSnapshot) Resolve(ctx context.Context, name model.NamespaceName) (PublishedDescriptor, error) {
	if descriptor := s.overlayByName[name]; descriptor != nil {
		return PublishedDescriptor{Descriptor: descriptor}, nil
	}
	// A parent that exists only in the session overlay owns no stored namespace
	// rows: a session-temporary schema and its members are never published, and
	// no statement declares a read of them. Resolving a name under such a parent
	// therefore cannot find a row, so the lookup is answered without reading the
	// namespace collection.
	if name.Parent != 0 && s.overlayByID[name.Parent] != nil {
		return PublishedDescriptor{}, ErrUnknownObject
	}
	key, err := s.store.nameKey(name)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	row, err := s.row(ctx, NamespaceObjectID, key)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	d, err := s.resolveRow(ctx, row)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	if d.Descriptor.Name() != name {
		return PublishedDescriptor{}, ErrCorruptCatalog
	}
	return d, nil
}
func (s *CatalogSnapshot) ResolveOID(ctx context.Context, oid model.OID) (PublishedDescriptor, error) {
	if descriptor := s.overlayByOID[oid]; descriptor != nil {
		return PublishedDescriptor{Descriptor: descriptor}, nil
	}
	if oid == 0 {
		return PublishedDescriptor{}, ErrUnknownObject
	}
	row, err := s.row(ctx, OIDObjectID, numberKey(uint64(oid)))
	if err != nil {
		return PublishedDescriptor{}, err
	}
	d, err := s.resolveRow(ctx, row)
	if err != nil {
		return PublishedDescriptor{}, err
	}
	if d.Descriptor.OID() != oid {
		return PublishedDescriptor{}, ErrCorruptCatalog
	}
	return d, nil
}

// ScanNamespace streams one namespace/kind through bounded storage batches.
// visit may retain its owned descriptor but must not close this snapshot.
func (s *CatalogSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(PublishedDescriptor) error) error {
	if visit == nil {
		return model.ErrInvalidDescriptor
	}
	return s.ScanNamespaceReferences(ctx, parent, kind, func(reference NamespaceReference) error {
		descriptor, err := s.Resolve(ctx, reference.Name)
		if err != nil {
			return err
		}
		if descriptor.Descriptor.ID() != reference.ID || descriptor.Descriptor.Version() != reference.Version {
			return ErrCorruptCatalog
		}
		return visit(descriptor)
	})
}

// ScanNamespaceReferences streams name-index identities without reading
// descriptor rows. It is used for nonblocking descriptor-lock admission.
func (s *CatalogSnapshot) ScanNamespaceReferences(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(NamespaceReference) error) (err error) {
	if s.closed {
		return storage.ErrClosed
	}
	if visit == nil {
		return model.ErrInvalidDescriptor
	}
	// A parent that exists only in the session overlay owns no stored namespace
	// rows: its members are exactly the overlay descriptors named under it, so
	// the storage scan below is provably empty. Session-temporary schemas and
	// their objects are never published, and a statement's declared read set
	// enumerates committed schemas only, so issuing that scan would be rejected
	// as a read outside the declaration.
	if s.overlayByID[parent] != nil {
		return s.scanSessionNamespaceReferences(ctx, parent, kind, visit)
	}
	span, err := s.store.namespaceScanSpan(ctx, parent, kind)
	if err != nil {
		return err
	}
	if kind == model.ObjectType {
		s.store.typeScans.Add(1)
	}
	lower := span.Lower.Values[0].BytesVal
	request := storage.PushdownRequest{Target: target(NamespaceObjectID), Layout: columns(), KeyLayout: keyLayout(), ColumnIDs: []model.ColumnID{1, 2}, Spans: []storage.KeySpan{span}}
	plan, err := s.reader.PlanScan(ctx, request)
	if plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, s.reader.ReleasePlan(plan)) }()
	}
	if err != nil {
		return err
	}
	if plan.SnapshotTS != s.timestamp {
		return storage.ErrInvalidScanPlan
	}
	cursor, err := s.reader.VectorizedScan(ctx, plan)
	if err != nil {
		if cursor != nil {
			err = errors.Join(err, cursor.Close())
		}
		return err
	}
	if cursor == nil {
		return storage.ErrInvalidScanPlan
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	batch := columnar.Batch{MaxRows: s.store.limits.ScanBatchRows}
	for {
		err := cursor.Next(ctx, &batch)
		if err == io.EOF {
			return s.scanSessionNamespaceReferences(ctx, parent, kind, visit)
		}
		if err != nil {
			return err
		}
		if batch.Rows() > s.store.limits.ScanBatchRows {
			return columnar.ErrInvalidBatch
		}
		if err := batch.Validate(max(batch.Length, s.store.limits.ScanBatchRows)); err != nil {
			return err
		}
		if len(batch.Columns) != 2 {
			return ErrCorruptCatalog
		}
		for row := range batch.Rows() {
			key, err := batchBytes(&batch.Columns[0], batch.Lane(row))
			if err != nil {
				return err
			}
			payload, err := batchBytes(&batch.Columns[1], batch.Lane(row))
			if err != nil {
				return err
			}
			if len(key) < 9 || uint64(len(key)) > s.store.limits.MaxKeyBytes || !bytes.Equal(key[:9], lower) {
				return ErrCorruptCatalog
			}
			name, err := decodeName(key)
			if err != nil {
				return err
			}
			if s.overlayByName[name] != nil {
				continue
			}
			id, version, err := decodeReference(payload)
			if err != nil {
				return err
			}
			s.store.reads.Add(1)
			if err := visit(NamespaceReference{Name: name, ID: id, Version: version}); err != nil {
				return err
			}
			if s.closed {
				return storage.ErrClosed
			}
		}
	}
}

func (s *CatalogSnapshot) scanSessionNamespaceReferences(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(NamespaceReference) error) error {
	descriptors := make([]*model.Descriptor, 0, len(s.overlayByID))
	for _, descriptor := range s.overlayByID {
		if descriptor.Name().Parent == parent && descriptor.Kind() == kind {
			descriptors = append(descriptors, descriptor)
		}
	}
	slices.SortFunc(descriptors, func(left, right *model.Descriptor) int {
		if order := cmp.Compare(left.Name().Name, right.Name().Name); order != 0 {
			return order
		}
		return cmp.Compare(left.ID(), right.ID())
	})
	for _, descriptor := range descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(NamespaceReference{Name: descriptor.Name(), ID: descriptor.ID(), Version: descriptor.Version()}); err != nil {
			return err
		}
		if s.closed {
			return storage.ErrClosed
		}
	}
	return nil
}

func batchBytes(v *columnar.Vector, lane int) ([]byte, error) {
	if v.Type.Storage != model.TypeBytes || v.IsNull(lane) {
		return nil, ErrCorruptCatalog
	}
	r := v.Ranges[v.ValueIndex(lane)]
	return v.Data[int(r.Offset) : int(r.Offset)+int(r.Length)], nil
}
