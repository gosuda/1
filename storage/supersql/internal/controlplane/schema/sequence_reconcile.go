package schema

import (
	"cmp"
	"context"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sequenceReferenceState struct {
	incarnation   SequenceIncarnation
	configuration SequenceConfig
	seen          bool
}

// ReconcileSequences runs before admitting sessions, against one recovered catalog
// and storage snapshot. With no pending publishers or allocation users, records
// absent from the catalog are abandoned incarnations. Discard preserves MVCC
// history and never recycles their identities. A failed uncertain discard remains
// owned by service.Resolve; retry reconciliation only after resolving it.
func ReconcileSequences(ctx context.Context, service *SequenceService, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, memoryBytes uint64) error {
	if service == nil || snapshot == nil || reader == nil {
		return model.ErrInvalidDescriptor
	}
	width := uint64(unsafe.Sizeof(sequenceReferenceState{}))
	maximum := min(service.options.Store.options.MaxIncarnations, memoryBytes/(4*width))
	if maximum == 0 {
		return sequenceLimitError()
	}
	var references []sequenceReferenceState
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		return snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectSequence, func(published catalog.PublishedDescriptor) error {
			metadata, ok := published.Descriptor.Sequence()
			if !ok {
				return catalog.ErrCorruptCatalog
			}
			if uint64(len(references)) == maximum {
				return sequenceLimitError()
			}
			references = append(references, sequenceReferenceState{incarnation: SequenceIncarnation(metadata.Incarnation), configuration: SequenceConfig{Start: metadata.Start, Increment: metadata.Increment, Min: metadata.Min, Max: metadata.Max, Cache: metadata.Cache, Cycle: metadata.Cycle}})
			return ctx.Err()
		})
	})
	if err != nil {
		return err
	}
	slices.SortFunc(references, func(a, b sequenceReferenceState) int { return cmp.Compare(a.incarnation, b.incarnation) })
	for i := 1; i < len(references); i++ {
		if references[i-1].incarnation == references[i].incarnation {
			return catalog.ErrCorruptCatalog
		}
	}
	err = service.options.Store.Scan(ctx, reader, func(state SequenceState) error {
		index, found := slices.BinarySearchFunc(references, state.Incarnation, func(reference sequenceReferenceState, id SequenceIncarnation) int {
			return cmp.Compare(reference.incarnation, id)
		})
		if !found {
			return service.Discard(ctx, state.Incarnation)
		}
		if state.Retired || state.Config != references[index].configuration {
			return catalog.ErrCorruptCatalog
		}
		references[index].seen = true
		return ctx.Err()
	})
	if err != nil {
		return err
	}
	for _, reference := range references {
		if !reference.seen {
			return catalog.ErrCorruptCatalog
		}
	}
	return ctx.Err()
}
