package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// collationAwareTypeResolver wraps a builtin.TypeResolver and also implements
// builtin.CollationResolver for user-created MVCC catalog collations scanned
// from the same snapshot.
type collationAwareTypeResolver struct {
	builtin.TypeResolver
	collations  map[model.CollationID]builtin.ICUCollationSpec
	fingerprint [sha256.Size]byte
}

func (r *collationAwareTypeResolver) ResolveCollation(id model.CollationID) (builtin.ICUCollationSpec, bool) {
	if r == nil || len(r.collations) == 0 {
		return builtin.ICUCollationSpec{}, false
	}
	spec, ok := r.collations[id]
	return spec, ok
}

func (r *collationAwareTypeResolver) CollationFingerprint() [sha256.Size]byte {
	if r == nil {
		return [sha256.Size]byte{}
	}
	return r.fingerprint
}

// ResolveCollation forwards catalog collation lookups to the statement-pinned
// runtime so the session's shared KeyEncoder always sees the active snapshot's
// custom collations.
func (r *sessionTypeResolver) ResolveCollation(id model.CollationID) (builtin.ICUCollationSpec, bool) {
	if r == nil || r.owner == nil || r.owner.statementTypes == nil {
		return builtin.ICUCollationSpec{}, false
	}
	cr, ok := r.owner.statementTypes.(builtin.CollationResolver)
	if !ok {
		return builtin.ICUCollationSpec{}, false
	}
	return cr.ResolveCollation(id)
}

func (r *sessionTypeResolver) CollationFingerprint() [sha256.Size]byte {
	if r == nil || r.owner == nil || r.owner.statementTypes == nil {
		return [sha256.Size]byte{}
	}
	cr, ok := r.owner.statementTypes.(builtin.CollationResolver)
	if !ok {
		return [sha256.Size]byte{}
	}
	return cr.CollationFingerprint()
}

func wrapWithCatalogCollations(ctx context.Context, snapshot *catalog.CatalogSnapshot, limits model.DescriptorLimits, base builtin.TypeResolver) (builtin.TypeResolver, error) {
	if snapshot == nil || base == nil {
		return base, nil
	}
	specs, err := scanCatalogCollations(ctx, snapshot, limits)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return base, nil
	}
	byID := make(map[model.CollationID]builtin.ICUCollationSpec, len(specs))
	ids := make([]model.CollationID, 0, len(specs))
	for _, spec := range specs {
		byID[spec.ID] = spec
		ids = append(ids, spec.ID)
	}
	slices.Sort(ids)
	h := sha256.New()
	var buf [8]byte
	for _, id := range ids {
		spec := byID[id]
		binary.LittleEndian.PutUint64(buf[:], uint64(spec.ID))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(spec.Locale))
		if spec.Deterministic {
			_, _ = h.Write([]byte{1})
		} else {
			_, _ = h.Write([]byte{0})
		}
	}
	var fp [sha256.Size]byte
	copy(fp[:], h.Sum(nil))
	return &collationAwareTypeResolver{
		TypeResolver: base,
		collations:   byID,
		fingerprint:  fp,
	}, nil
}

func scanCatalogCollations(ctx context.Context, snapshot *catalog.CatalogSnapshot, limits model.DescriptorLimits) ([]builtin.ICUCollationSpec, error) {
	var specs []builtin.ICUCollationSpec
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectCollation, func(published catalog.PublishedDescriptor) error {
			if len(specs) >= limits.MaxItems {
				return &model.Error{Code: "54000", Message: "collation descriptor limit exceeded"}
			}
			meta, ok := published.Descriptor.Collation()
			if !ok {
				return nil
			}
			specs = append(specs, builtin.ICUCollationSpec{
				ID:            model.CollationID(published.Descriptor.ID()),
				Name:          published.Descriptor.Name().Name,
				Locale:        meta.Locale,
				Deterministic: meta.Deterministic,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return specs, nil
}
