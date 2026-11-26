package model

import "sync/atomic"

// PublicationOwner is a resource-free transaction identity and publication decision.
// Prepared metadata may retain it after all transaction resources have closed.
type PublicationOwner struct{ state atomic.Uint32 }

type PublicationState uint32

const (
	PublicationActive PublicationState = iota
	PublicationPending
	PublicationCommitted
	PublicationAborted
)

func (owner *PublicationOwner) Decide(state PublicationState) { owner.state.Store(uint32(state)) }

// ProvisionalBinding identifies one overlay mutation, not a reusable object/version.
type ProvisionalBinding struct {
	owner   *PublicationOwner
	ordinal uint64
	invalid atomic.Bool
}

func NewProvisionalBinding(owner *PublicationOwner, ordinal uint64) *ProvisionalBinding {
	return &ProvisionalBinding{owner: owner, ordinal: ordinal}
}

func (binding *ProvisionalBinding) Ordinal() uint64 { return binding.ordinal }
func (binding *ProvisionalBinding) Invalidate()     { binding.invalid.Store(true) }
func (binding *ProvisionalBinding) Usable(owner *PublicationOwner) bool {
	if binding == nil {
		return true
	}
	if binding.invalid.Load() || binding.owner == nil {
		return false
	}
	switch PublicationState(binding.owner.state.Load()) {
	case PublicationCommitted:
		return true
	case PublicationActive:
		return owner == binding.owner
	default:
		return false
	}
}
