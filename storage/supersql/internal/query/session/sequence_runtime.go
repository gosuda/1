package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

// SequenceCatalog borrows a pinned invocation snapshot. The owner must protect
// its OID/descriptor lookups and logical sequence identities through transaction end.
type SequenceCatalog interface {
	ResolveOID(context.Context, model.OID) (catalog.PublishedDescriptor, error)
	Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error)
}

type sequenceRuntime struct {
	catalog SequenceCatalog
	session *SequenceSession
}

func NewSequenceRuntime(snapshot SequenceCatalog, session *SequenceSession) (model.SequenceRuntime, error) {
	if historicalNil(snapshot) || session == nil {
		return nil, &model.Error{Code: "22023", Message: "sequence runtime requires catalog and session"}
	}
	return &sequenceRuntime{catalog: snapshot, session: session}, nil
}

func (r *sequenceRuntime) reference(ctx context.Context, descriptor *model.Descriptor) (SequenceReference, error) {
	if err := ctx.Err(); err != nil {
		return SequenceReference{}, err
	}
	if descriptor == nil || descriptor.State() != model.StatePublic {
		return SequenceReference{}, &model.Error{Code: "42P01", Message: "sequence does not exist"}
	}
	if descriptor.Kind() != model.ObjectSequence {
		return SequenceReference{}, &model.Error{Code: "42809", Message: "relation is not a sequence"}
	}
	metadata, ok := descriptor.Sequence()
	if !ok {
		return SequenceReference{}, model.ErrInvalidDescriptor
	}
	return SequenceReference{Object: descriptor.ID(), Identity: schema.SequenceIncarnation(metadata.Identity), Incarnation: schema.SequenceIncarnation(metadata.Incarnation)}, nil
}
func (r *sequenceRuntime) resolve(ctx context.Context, oid model.OID) (SequenceReference, error) {
	descriptor, err := r.catalog.ResolveOID(ctx, oid)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return SequenceReference{}, &model.Error{Code: "42P01", Message: "sequence does not exist", Cause: err}
	}
	if err != nil {
		return SequenceReference{}, err
	}
	return r.reference(ctx, descriptor.Descriptor)
}
func (r *sequenceRuntime) NextSequence(ctx context.Context, oid model.OID) (int64, error) {
	ref, err := r.resolve(ctx, oid)
	if err != nil {
		return 0, err
	}
	return r.session.Next(ctx, ref)
}
func (r *sequenceRuntime) CurrentSequence(ctx context.Context, oid model.OID) (int64, error) {
	ref, err := r.resolve(ctx, oid)
	if err != nil {
		return 0, err
	}
	return r.session.Current(ctx, ref)
}
func (r *sequenceRuntime) SetSequence(ctx context.Context, oid model.OID, value int64, called bool) (int64, error) {
	ref, err := r.resolve(ctx, oid)
	if err != nil {
		return 0, err
	}
	return r.session.Set(ctx, ref, value, called)
}
func (r *sequenceRuntime) LastSequence(ctx context.Context) (int64, error) {
	previous, err := r.session.LastReference(ctx)
	if err != nil {
		return 0, err
	}
	descriptor, err := r.catalog.Descriptor(ctx, previous.Object)
	if errors.Is(err, catalog.ErrUnknownObject) {
		return 0, &model.Error{Code: "55000", Message: "lastval sequence is no longer defined", Cause: err}
	}
	if err != nil {
		return 0, err
	}
	current, err := r.reference(ctx, descriptor.Descriptor)
	if err != nil {
		return 0, err
	}
	if current.Identity != previous.Identity {
		return 0, &model.Error{Code: "55000", Message: "lastval sequence is no longer defined"}
	}
	return r.session.Current(ctx, current)
}

// Discard clears this session's sequence state through the real session, which
// is the capability DISCARD SEQUENCES requires. A durable operation with an
// unknown outcome propagates its pending error instead of dropping ownership.
func (r *sequenceRuntime) Discard(ctx context.Context) error {
	return r.session.Discard(ctx)
}

type readOnlySequences struct{ model.SequenceRuntime }

func (readOnlySequences) NextSequence(context.Context, model.OID) (int64, error) {
	return 0, settingError("25006", "cannot execute nextval in a read-only transaction")
}
func (readOnlySequences) SetSequence(context.Context, model.OID, int64, bool) (int64, error) {
	return 0, settingError("25006", "cannot execute setval in a read-only transaction")
}

// sequenceDiscardRuntime is the required session capability behind DISCARD
// SEQUENCES: it drops session-local currval state, cache leases, and lastval.
type sequenceDiscardRuntime interface {
	Discard(context.Context) error
}

// errSequenceDiscardUnavailable reports the session_state profile exclusion when
// the environment carries no discarding session sequence provider, instead of
// treating an absent provider as a successful discard. The session package owns
// the rejection because it happens after binding, and it carries the same
// profile message and feature id as every binder-side exclusion.
var errSequenceDiscardUnavailable = &model.Error{
	Code:      "0A000",
	Message:   bind.UnsupportedMessage(bind.SessionState),
	FeatureID: string(bind.SessionState),
}

// Discard forwards to the wrapped provider so a read-only transaction keeps the
// capability it was constructed with; the read-only wrapper never hides it.
func (r readOnlySequences) Discard(ctx context.Context) error {
	discarder, ok := r.SequenceRuntime.(sequenceDiscardRuntime)
	if !ok || historicalNil(discarder) {
		return errSequenceDiscardUnavailable
	}
	return discarder.Discard(ctx)
}
