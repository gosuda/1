package schema

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// SequenceIncarnation identifies one immutable configuration. Only committed
// identities may escape their allocating transaction; committed IDs never repeat.
type SequenceIncarnation uint64

type SequenceConfig struct {
	Start, Increment, Min, Max, Cache int64
	Cycle                             bool
}

// SequenceState.Last is the reservation high-water value, not session currval.
// Called=false makes the next reservation start at Last, including after setval.
type SequenceState struct {
	Incarnation     SequenceIncarnation
	Config          SequenceConfig
	Last            int64
	Called, Retired bool
	Revision        uint64
}

// SequenceReservation is a candidate until the caller commits. It never crosses
// a cycle boundary. Sessions may discard unused values, but must not reissue them.
type SequenceReservation struct {
	Incarnation            SequenceIncarnation
	Revision               uint64
	First, Last, Increment int64
	Count                  uint64
}

func (c SequenceConfig) validate(maxCache uint64) error {
	if c.Increment == 0 || c.Min >= c.Max || c.Start < c.Min || c.Start > c.Max || c.Cache <= 0 {
		return &model.Error{Code: "22023", Message: "invalid sequence configuration"}
	}
	if uint64(c.Cache) > maxCache {
		return sequenceLimitError()
	}
	return nil
}

func sequenceLimitError() error {
	return &model.Error{Code: "54000", Message: "sequence store limit exceeded"}
}
func sequenceRangeError() error {
	return &model.Error{Code: "22003", Message: "sequence counter or revision exhausted"}
}
func sequenceUnavailableError() error {
	return &model.Error{Code: "55000", Message: "sequence incarnation is retired or unavailable"}
}

// reserveSequence uses unsigned distances to cover the full int64 domain,
// including an increment of MinInt64, without overflowing signed arithmetic.
func reserveSequence(state SequenceState) (SequenceState, SequenceReservation, error) {
	if state.Retired {
		return SequenceState{}, SequenceReservation{}, sequenceUnavailableError()
	}
	if state.Revision == math.MaxUint64 {
		return SequenceState{}, SequenceReservation{}, sequenceRangeError()
	}
	c := state.Config
	step := uint64(c.Increment)
	if c.Increment < 0 {
		step = -step
	}
	first := state.Last
	if state.Called {
		distance := uint64(c.Max) - uint64(first)
		if c.Increment < 0 {
			distance = uint64(first) - uint64(c.Min)
		}
		if distance < step {
			if !c.Cycle {
				return SequenceState{}, SequenceReservation{}, &model.Error{Code: "2200H", Message: "sequence generator limit exceeded"}
			}
			first = c.Min
			if c.Increment < 0 {
				first = c.Max
			}
		} else {
			first = int64(uint64(first) + uint64(c.Increment))
		}
	}
	distance := uint64(c.Max) - uint64(first)
	if c.Increment < 0 {
		distance = uint64(first) - uint64(c.Min)
	}
	// Clamp the number of additional values before adding one: a full-width
	// sequence with increment one has 2^64 possible values.
	additional := min(uint64(c.Cache)-1, distance/step)
	last := int64(uint64(first) + additional*uint64(c.Increment))
	state.Last, state.Called = last, true
	state.Revision++
	return state, SequenceReservation{Incarnation: state.Incarnation, Revision: state.Revision, First: first, Last: last, Increment: c.Increment, Count: additional + 1}, nil
}

// InitializeAllocator is bootstrap-only. Never delete or reconstruct this record
// from surviving incarnations: reclaimed identities remain permanently consumed.
func (s *SequenceStore) InitializeAllocator(ctx context.Context, transaction txn.MutationApplier) error {
	if err := s.check(ctx, transaction); err != nil {
		return err
	}
	_, err := s.readAllocator(ctx, transaction)
	if err == nil {
		return &model.Error{Code: "42710", Message: "sequence allocator already initialized"}
	}
	if !indexRowAbsent(err) {
		return err
	}
	return s.writeAllocator(ctx, transaction, storage.MutationInsert, sequenceAllocator{revision: 1})
}

// Initialize stages allocator advancement and a new incarnation in one atomic
// statement. The caller must roll back that statement on any error. It must commit
// before a catalog transaction can publish the ID or an autonomous user can use it.
func (s *SequenceStore) Initialize(ctx context.Context, transaction txn.MutationApplier, config SequenceConfig) (SequenceState, error) {
	if err := s.check(ctx, transaction); err != nil {
		return SequenceState{}, err
	}
	if err := config.validate(s.options.MaxCache); err != nil {
		return SequenceState{}, err
	}
	a, err := s.readAllocator(ctx, transaction)
	if err != nil {
		return SequenceState{}, err
	}
	if a.high == math.MaxUint64 || a.revision == math.MaxUint64 {
		return SequenceState{}, sequenceRangeError()
	}
	if a.count >= s.options.MaxIncarnations {
		return SequenceState{}, sequenceLimitError()
	}
	a.high++
	a.count++
	a.revision++
	state := SequenceState{Incarnation: SequenceIncarnation(a.high), Config: config, Last: config.Start, Revision: 1}
	if err := s.writeAllocator(ctx, transaction, storage.MutationUpdate, a); err != nil {
		return SequenceState{}, err
	}
	if err := s.writeState(ctx, transaction, storage.MutationInsert, state); err != nil {
		return SequenceState{}, err
	}
	return state, nil
}

func (s *SequenceStore) Reserve(ctx context.Context, transaction txn.MutationApplier, id SequenceIncarnation) (SequenceReservation, error) {
	state, err := s.Get(ctx, transaction, id)
	if err != nil {
		return SequenceReservation{}, err
	}
	next, reservation, err := reserveSequence(state)
	if err != nil {
		return SequenceReservation{}, err
	}
	if err := s.writeState(ctx, transaction, storage.MutationUpdate, next); err != nil {
		return SequenceReservation{}, err
	}
	return reservation, nil
}

// Set stages setval; it neither changes session currval nor invalidates leases.
// The session applies those effects only after a known committed outcome.
func (s *SequenceStore) Set(ctx context.Context, transaction txn.MutationApplier, id SequenceIncarnation, value int64, called bool) (SequenceState, error) {
	state, err := s.Get(ctx, transaction, id)
	if err != nil {
		return SequenceState{}, err
	}
	if state.Retired {
		return SequenceState{}, sequenceUnavailableError()
	}
	if value < state.Config.Min || value > state.Config.Max {
		return SequenceState{}, &model.Error{Code: "22003", Message: "setval value is outside sequence bounds"}
	}
	if state.Revision == math.MaxUint64 {
		return SequenceState{}, sequenceRangeError()
	}
	state.Last, state.Called = value, called
	state.Revision++
	if err := s.writeState(ctx, transaction, storage.MutationUpdate, state); err != nil {
		return SequenceState{}, err
	}
	return state, nil
}

// Retire requires proof that no catalog snapshot, pending publication, transaction
// overlay, or pinned user can still reach this incarnation. The caller owns that
// barrier through commit. Retiring a record is irreversible and prevents new use.
func (s *SequenceStore) Retire(ctx context.Context, transaction txn.MutationApplier, id SequenceIncarnation) error {
	state, err := s.Get(ctx, transaction, id)
	if err != nil {
		return err
	}
	if state.Retired {
		return nil
	}
	if state.Revision == math.MaxUint64 {
		return sequenceRangeError()
	}
	state.Retired = true
	state.Revision++
	return s.writeState(ctx, transaction, storage.MutationUpdate, state)
}

// Reclaim removes a retired record and releases one bounded storage slot. It
// never lowers the allocator high-water mark. Use the same reachability barrier
// as Retire, and roll back the entire statement on error.
func (s *SequenceStore) Reclaim(ctx context.Context, transaction txn.MutationApplier, id SequenceIncarnation) error {
	if err := s.check(ctx, transaction); err != nil {
		return err
	}
	// Match Initialize's allocator-before-state lock order.
	a, err := s.readAllocator(ctx, transaction)
	if err != nil {
		return err
	}
	state, err := s.Get(ctx, transaction, id)
	if err != nil {
		return err
	}
	if !state.Retired {
		return &model.Error{Code: "55000", Message: "sequence incarnation must be retired before reclamation"}
	}
	if a.count == 0 || uint64(id) > a.high {
		return sequenceCorruptError(nil)
	}
	if a.revision == math.MaxUint64 {
		return sequenceRangeError()
	}
	a.count--
	a.revision++
	if err := s.writeAllocator(ctx, transaction, storage.MutationUpdate, a); err != nil {
		return err
	}
	return s.writeState(ctx, transaction, storage.MutationDelete, state)
}
