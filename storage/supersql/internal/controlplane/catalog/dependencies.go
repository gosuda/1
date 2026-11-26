package catalog

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// CreateDependencies protects exact namespace, descriptor and OID absence checks
// without locking an entire catalog collection. Results own their key metadata.
func (s *Store) CreateDependencies(ctx context.Context, descriptor *model.Descriptor) ([]txn.ReadDependency, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if descriptor == nil {
		return nil, model.ErrInvalidDescriptor
	}
	namespace, err := s.NamespaceDependency(ctx, descriptor.Name())
	if err != nil {
		return nil, err
	}
	keys := [][]byte{numberKey(uint64(descriptor.ID())), numberKey(uint64(descriptor.OID()))}
	targets := []model.ObjectID{DescriptorObjectID, OIDObjectID}
	dependencies := make([]txn.ReadDependency, len(keys)+1)
	dependencies[0] = namespace
	for i, key := range keys {
		if _, err := s.encoded(ctx, key); err != nil {
			return nil, err
		}
		lower := storage.KeyBound{Values: []model.Value{model.BytesVal(key)}, Inclusive: true}
		upper := storage.KeyBound{Values: []model.Value{model.BytesVal(append([]byte(nil), key...))}, Inclusive: true}
		dependencies[i+1] = txn.ReadDependency{Target: target(targets[i]), KeyLayout: keyLayout(), Spans: []storage.KeySpan{{Lower: lower, Upper: upper}}}
	}
	return dependencies, nil
}

func (s *Store) DescriptorDependency(id model.ObjectID) txn.ReadDependency {
	key := numberKey(uint64(id))
	lower := storage.KeyBound{Values: []model.Value{model.BytesVal(key)}, Inclusive: true}
	upper := storage.KeyBound{Values: []model.Value{model.BytesVal(append([]byte(nil), key...))}, Inclusive: true}
	return txn.ReadDependency{Target: target(DescriptorObjectID), KeyLayout: keyLayout(), Spans: []storage.KeySpan{{Lower: lower, Upper: upper}}}
}

// OIDScanDependency protects every OID-index row runtime resolution may read.
// The OIDs a statement resolves (regclass/regtype casts, sequence and
// introspection builtins, routine frames) are values computed at execution
// time, so the declaration cannot enumerate keys and covers the collection.
func (s *Store) OIDScanDependency() txn.ReadDependency {
	return txn.ReadDependency{Target: target(OIDObjectID), KeyLayout: keyLayout(), Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
}
