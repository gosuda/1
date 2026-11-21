package schema

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The native catalog manifest shares the composition-owned metadata collection
// with job, retention-policy, and identity records. Prefix 6 lies outside the
// job scan [2,3) and outside every existing guard/definition/retirement/identity
// prefix, so adopting it reserves no new global ObjectID.
func catalogManifestKey() []byte { return jobLogicalKey(6, 0) }

var (
	errCatalogManifestOptions = &model.Error{Code: "22023", Message: "native catalog manifest requires a job store and transaction"}
	errCatalogManifestExists  = &model.Error{Code: "42710", Message: "native catalog manifest already exists"}
	errCatalogManifestCorrupt = &model.Error{Code: "XX001", Message: "invalid durable native catalog manifest", Cause: storage.ErrCorruptStorage}
)

// CatalogManifest returns the durable native-catalog manifest payload bounded by
// maximum. Its encoding belongs to the catalog producer; this store never
// interprets it, so the producer supplies the bound rather than the per-job
// RecordBytes, which describes a single job record and is far smaller. The result
// borrows the reader's row storage: consume it before any further read on that
// reader. An absent record returns storage.ErrRowNotFound so a caller can
// distinguish an unbootstrapped database from a corrupt record.
func (s *JobStore) CatalogManifest(ctx context.Context, reader JobReader, maximum uint64) ([]byte, error) {
	if s == nil || s.normalizer == nil || mutationPolicyNil(reader) {
		return nil, errCatalogManifestOptions
	}
	data, err := s.readLimit(ctx, reader, catalogManifestKey(), maximum)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errCatalogManifestCorrupt
	}
	return data, nil
}

// CatalogManifestDependency protects the manifest record for one strict-PCC
// read. Declare it before opening the read view that supplies the manifest.
func (s *JobStore) CatalogManifestDependency() txn.ReadDependency {
	bound := storage.KeyBound{Values: []model.Value{model.BytesVal(catalogManifestKey())}, Inclusive: true}
	return txn.ReadDependency{Target: s.options.Target, KeyLayout: jobKeyLayout(), Spans: []storage.KeySpan{{Lower: bound, Upper: bound}}}
}

// CreateCatalogManifest inserts the initial manifest in the caller's statement
// bounded by maximum. It never replaces an existing record, and payload must stay
// unmodified until the statement returns. The caller owns publication and rollback.
func (s *JobStore) CreateCatalogManifest(ctx context.Context, transaction txn.MutationApplier, payload []byte, maximum uint64) error {
	if s == nil || s.normalizer == nil || mutationPolicyNil(transaction) {
		return errCatalogManifestOptions
	}
	if len(payload) == 0 || uint64(len(payload)) > maximum {
		return ErrJobLimit
	}
	switch _, err := s.CatalogManifest(ctx, transaction, maximum); {
	case err == nil:
		return errCatalogManifestExists
	case !errors.Is(err, storage.ErrRowNotFound):
		return err
	}
	mutation, err := s.mutation(ctx, storage.MutationInsert, catalogManifestKey(), payload)
	if err != nil {
		return err
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}

