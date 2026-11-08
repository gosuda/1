package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Relation names share one SQL namespace even though their persisted keys retain
// object kind. Keeping that encoding also preserves existing historical lookups.
func namespaceVariants(name model.NamespaceName) ([5]model.NamespaceName, int) {
	var names [5]model.NamespaceName
	if name.Kind < model.ObjectTable || name.Kind > model.ObjectMaterializedView {
		names[0] = name
		return names, 1
	}
	count := 0
	for kind := model.ObjectTable; kind <= model.ObjectMaterializedView; kind++ {
		name.Kind = kind
		names[count] = name
		count++
	}
	return names, count
}

// NamespaceDependency protects a name against every conflicting relation kind.
// Rename callers hold this destination dependency in addition to the descriptor
// dependency; mutations retain the old name's exclusive lock through commit.
func (s *Store) NamespaceDependency(ctx context.Context, name model.NamespaceName) (txn.ReadDependency, error) {
	if err := model.ValidateNamespaceName(name); err != nil {
		return txn.ReadDependency{}, err
	}
	names, count := namespaceVariants(name)
	rawBytes := uint64(17) + uint64(len(name.Name)) + uint64(len(name.Signature))
	fixed := uint64(unsafe.Sizeof(storage.KeySpan{})) + 2*uint64(unsafe.Sizeof(model.Value{}))
	if rawBytes > s.limits.MaxKeyBytes || rawBytes > (s.limits.Descriptor.MaxBytes/uint64(count))/8 || fixed > s.limits.Descriptor.MaxBytes/uint64(count)-rawBytes*8 {
		return txn.ReadDependency{}, model.ErrDescriptorLimit
	}
	dependency := txn.ReadDependency{Target: target(NamespaceObjectID), KeyLayout: keyLayout(), Spans: make([]storage.KeySpan, count)}
	for i := range count {
		if err := ctx.Err(); err != nil {
			return txn.ReadDependency{}, err
		}
		key, err := s.nameKey(names[i])
		if err != nil {
			return txn.ReadDependency{}, err
		}
		if _, err := s.encoded(ctx, key); err != nil {
			return txn.ReadDependency{}, err
		}
		lower := storage.KeyBound{Values: []model.Value{model.BytesVal(key)}, Inclusive: true}
		upper := storage.KeyBound{Values: []model.Value{model.BytesVal(append([]byte(nil), key...))}, Inclusive: true}
		dependency.Spans[i] = storage.KeySpan{Lower: lower, Upper: upper}
	}
	return dependency, nil
}

// NamespaceScanDependency protects all namespace rows visited by ScanNamespace.
// DescriptorDependency declarations are still required for traversed descriptors.
// The returned key metadata is owned by the caller.
func (s *Store) NamespaceScanDependency(ctx context.Context, parent model.ObjectID, kind model.ObjectKind) (txn.ReadDependency, error) {
	span, err := s.namespaceScanSpan(ctx, parent, kind)
	if err != nil {
		return txn.ReadDependency{}, err
	}
	return txn.ReadDependency{Target: target(NamespaceObjectID), KeyLayout: keyLayout(), Spans: []storage.KeySpan{span}}, nil
}

func (s *Store) namespaceScanSpan(ctx context.Context, parent model.ObjectID, kind model.ObjectKind) (storage.KeySpan, error) {
	if err := ctx.Err(); err != nil {
		return storage.KeySpan{}, err
	}
	// The accepted range must cover every real ObjectKind. Aggregates are
	// ObjectRoutine (pg_proc namespace) and triggers are nested metadata, so
	// neither is a standalone namespace kind.
	if kind < model.ObjectSchema || kind > model.ObjectOperator {
		return storage.KeySpan{}, model.ErrInvalidDescriptor
	}
	const metadataBytes = uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(storage.KeySpan{})) + 2*uint64(unsafe.Sizeof(model.Value{})) + 18
	if s.limits.MaxKeyBytes < 9 || s.limits.Descriptor.MaxBytes < metadataBytes {
		return storage.KeySpan{}, model.ErrDescriptorLimit
	}
	keys := [18]byte{8: byte(kind), 17: byte(kind + 1)}
	binary.BigEndian.PutUint64(keys[:8], uint64(parent))
	copy(keys[9:17], keys[:8])
	bounds := [2]model.Value{model.BytesVal(keys[:9:9]), model.BytesVal(keys[9:18:18])}
	return storage.KeySpan{Lower: storage.KeyBound{Values: bounds[:1:1], Inclusive: true}, Upper: storage.KeyBound{Values: bounds[1:2:2]}}, nil
}

func (s *Store) vacantName(ctx context.Context, snapshot *CatalogSnapshot, name model.NamespaceName) error {
	names, count := namespaceVariants(name)
	for i := range count {
		key, err := s.nameKey(names[i])
		if err != nil {
			return err
		}
		if err := snapshot.requireAbsent(ctx, NamespaceObjectID, key); err != nil {
			return err
		}
	}
	return nil
}

// Absence probes must not follow a conflicting row's descriptor reference: that
// object's ID is not part of a create/rename transaction's declared read spans.
func (s *CatalogSnapshot) requireAbsent(ctx context.Context, object model.ObjectID, key []byte) error {
	_, err := s.row(ctx, object, key)
	if err == nil {
		return ErrDuplicateObject
	}
	if err == ErrUnknownObject {
		return nil
	}
	return err
}

func catalogRowAbsent(err error) bool {
	if err == storage.ErrRowNotFound {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !catalogRowAbsent(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return catalogRowAbsent(cause)
	}
	return false
}
