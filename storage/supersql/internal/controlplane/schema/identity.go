package schema

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// IdentityHighWaterMarks contains the greatest identities ever assigned, including
// retired identities. Zero means that namespace is unused. Bootstrap or upgrade
// supplies these marks; catalog collection ObjectIDs are excluded.
type IdentityHighWaterMarks struct {
	ObjectID model.ObjectID
	OID      model.OID
	JobID    JobID
}

// IdentityAllocation is provisional until commit. Before then, use it only for
// writes in that transaction; rollback invalidates it and may reuse its numbers.
// A committed allocation may reserve identities for later creation. Unused
// committed reservations leave permanent gaps; they must never be recycled.
type IdentityAllocation struct {
	ObjectID model.ObjectID
	OID      model.OID
	JobID    JobID
}

// InitializeIdentities stages revision 1 without allocating an identity. The
// caller must fence other identity sources and supply complete historical marks.
// The metadata target itself is skipped even when it lies above the supplied mark.
// Call inside the caller's atomic statement using its context and PCC transaction;
// neither initialization nor allocation commits independently.
func (s *JobStore) InitializeIdentities(ctx context.Context, transaction txn.MutationApplier, highWater IdentityHighWaterMarks) error {
	if err := s.identityOptions(ctx, transaction); err != nil {
		return err
	}
	nextObject, available := s.nextIdentityObject(highWater.ObjectID)
	if !available || highWater.OID == math.MaxUint32 || highWater.JobID == math.MaxUint64 {
		return identityRangeError()
	}
	state := identityState{
		revision: 1,
		next: IdentityAllocation{
			ObjectID: nextObject,
			OID:      highWater.OID + 1,
			JobID:    highWater.JobID + 1,
		},
	}
	_, err := s.readIdentityState(ctx, transaction)
	if err == nil {
		return &model.Error{Code: "42710", Message: "durable identity allocator already exists"}
	}
	if !indexRowAbsent(err) {
		return err
	}
	return s.writeIdentityState(ctx, transaction, storage.MutationInsert, state)
}

// AllocateIdentity advances the single PCC-locked record once, from revision N
// to N+1. Get and ApplyMutations must share the caller's statement and transaction;
// pairing a snapshot Get with a writer does not satisfy this contract. Propagate
// errors from subsequent work through the statement to roll back the allocation.
// An error returns no allocation and leaves the durable counters unchanged.
func (s *JobStore) AllocateIdentity(ctx context.Context, transaction txn.MutationApplier) (IdentityAllocation, error) {
	if err := s.identityOptions(ctx, transaction); err != nil {
		return IdentityAllocation{}, err
	}
	state, err := s.readIdentityState(ctx, transaction)
	if err != nil {
		if indexRowAbsent(err) {
			return IdentityAllocation{}, &model.Error{Code: "55000", Message: "durable identity allocator is not initialized", Cause: err}
		}
		return IdentityAllocation{}, err
	}
	if state.exhausted || state.revision == math.MaxUint64 {
		return IdentityAllocation{}, identityRangeError()
	}
	allocated := state.next
	state.revision++
	nextObject, available := s.nextIdentityObject(allocated.ObjectID)
	if !available || allocated.OID == math.MaxUint32 || allocated.JobID == math.MaxUint64 {
		// The last valid tuple is issued once. Terminal zeros denote no successor,
		// not wrapped counters, and are accepted only with the exhaustion flag.
		state.next = IdentityAllocation{}
		state.exhausted = true
	} else {
		state.next.ObjectID = nextObject
		state.next.OID++
		state.next.JobID++
	}
	if err := s.writeIdentityState(ctx, transaction, storage.MutationUpdate, state); err != nil {
		return IdentityAllocation{}, err
	}
	return allocated, nil
}

func (s *JobStore) identityOptions(ctx context.Context, participant any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.normalizer == nil || mutationPolicyNil(participant) {
		return &model.Error{Code: "22023", Message: "identity allocation requires a job store and transaction"}
	}
	if s.options.Target.ObjectID >= catalog.NamespaceObjectID {
		return storage.ErrInvalidTarget
	}
	if s.options.RecordBytes < identityRecordBytes || s.options.KeyBytes < 9 {
		return ErrJobLimit
	}
	return nil
}

func (s *JobStore) nextIdentityObject(previous model.ObjectID) (model.ObjectID, bool) {
	if previous >= catalog.NamespaceObjectID-1 {
		return 0, false
	}
	next := previous + 1
	if next == s.options.Target.ObjectID {
		if next == catalog.NamespaceObjectID-1 {
			return 0, false
		}
		next++
	}
	return next, true
}

func identityRangeError() error {
	return &model.Error{Code: "22003", Message: "durable identity counter or revision exhausted"}
}
