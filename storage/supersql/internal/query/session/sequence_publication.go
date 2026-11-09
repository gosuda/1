package session

import (
	"context"
	"errors"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// SequenceDDLConfig borrows database services and the caller's sequence session.
// Services outlive cleanup, including resolution after Close. The sequence session
// remains open during DDL commands. CleanupTimeout bounds each cleanup attempt.
type SequenceDDLConfig struct {
	Catalog        *catalog.Store
	Identities     *schema.JobStore
	Service        *schema.SequenceService
	Session        *SequenceSession
	MaxChanges     int
	MemoryBytes    uint64
	CleanupTimeout time.Duration
}

type sequencePublication struct {
	previous, created, cleanupTarget schema.SequenceIncarnation
	initialization, discard          model.TxnID
	published, cleanup               bool
}
type sequencePublications struct {
	config  SequenceDDLConfig
	changes []sequencePublication
	marks   []int
}

var errSequencePublicationPending = &model.Error{Code: "55000", Message: "sequence DDL cleanup requires ResolveSequenceDDL"}

func newSequencePublications(config *SequenceDDLConfig, maxSavepoints int) (*sequencePublications, error) {
	if config == nil {
		return nil, nil
	}
	missing := config.Catalog == nil || config.Identities == nil || config.Service == nil || config.Session == nil
	if missing || config.MaxChanges <= 0 || config.CleanupTimeout <= 0 {
		return nil, errSessionTransactionConfig
	}
	header := uint64(unsafe.Sizeof(sequencePublications{}))
	if header > config.MemoryBytes {
		return nil, model.ErrValueBudget
	}
	remaining := config.MemoryBytes - header
	if uint64(maxSavepoints) > remaining/uint64(unsafe.Sizeof(int(0))) {
		return nil, model.ErrValueBudget
	}
	remaining -= uint64(maxSavepoints) * uint64(unsafe.Sizeof(int(0)))
	if uint64(config.MaxChanges) > remaining/uint64(unsafe.Sizeof(sequencePublication{})) {
		return nil, model.ErrValueBudget
	}
	return &sequencePublications{config: *config, changes: make([]sequencePublication, 0, config.MaxChanges), marks: make([]int, 0, maxSavepoints)}, nil
}
func (p *sequencePublications) blocked() bool {
	if p == nil {
		return false
	}
	for _, change := range p.changes {
		if change.cleanup || change.initialization != (model.TxnID{}) || change.discard != (model.TxnID{}) {
			return true
		}
	}
	return false
}
func (p *sequencePublications) reserve(previous schema.SequenceIncarnation) (int, error) {
	if len(p.changes) == cap(p.changes) {
		return 0, &model.Error{Code: "54000", Message: "sequence DDL change limit exceeded"}
	}
	index := len(p.changes)
	p.changes = append(p.changes, sequencePublication{previous: previous})
	return index, nil
}
func (p *sequencePublications) abandon(from int) {
	if p == nil {
		return
	}
	for i := from; i < len(p.changes); i++ {
		change := &p.changes[i]
		change.published = false
		change.cleanup = true
		change.cleanupTarget = change.created
	}
}
func (p *sequencePublications) finish(committed bool) {
	if p == nil {
		return
	}
	for i := range p.changes {
		change := &p.changes[i]
		if change.cleanup {
			continue
		}
		change.cleanup = true
		change.cleanupTarget = change.created
		if committed && change.published {
			change.cleanupTarget = change.previous
		}
	}
	clear(p.marks)
	p.marks = p.marks[:0]
}
func (p *sequencePublications) trim() {
	for len(p.changes) > 0 && p.changes[len(p.changes)-1] == (sequencePublication{}) {
		p.changes = p.changes[:len(p.changes)-1]
	}
}
func (p *sequencePublications) clean(ctx context.Context) error {
	if !p.blocked() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.CleanupTimeout)
	defer cancel()
	defer p.trim()
	// Any uncertain decision fences new commits, including earlier reclamations.
	for i := range p.changes {
		change := &p.changes[i]
		if !change.cleanup {
			continue
		}
		if change.initialization != (model.TxnID{}) {
			outcome, err := p.config.Service.Resolve(ctx, change.initialization)
			var pending *schema.PendingSequenceError
			if errors.As(err, &pending) {
				return err
			}
			change.initialization = model.TxnID{}
			change.created = outcome.State.Incarnation
			change.cleanupTarget = change.created
			if err != nil {
				if change.created == 0 {
					*change = sequencePublication{}
				}
				return err
			}
		}
		if change.discard != (model.TxnID{}) {
			_, err := p.config.Service.Resolve(ctx, change.discard)
			var pending *schema.PendingSequenceError
			if errors.As(err, &pending) {
				return err
			}
			change.discard = model.TxnID{}
			if err != nil {
				return err
			}
			*change = sequencePublication{}
			continue
		}
	}
	for i := range p.changes {
		change := &p.changes[i]
		if !change.cleanup {
			continue
		}
		if change.cleanupTarget != 0 {
			err := p.config.Service.Discard(ctx, change.cleanupTarget)
			if err != nil {
				var pending *schema.PendingSequenceError
				if errors.As(err, &pending) {
					change.discard = pending.Transaction
				}
				return err
			}
		}
		*change = sequencePublication{}
	}
	return nil
}

// ResolveSequenceDDL retries only retained cleanup or the original uncertain
// allocation. It neither retries SQL nor changes the parent transaction outcome.
func (t *Transactions) ResolveSequenceDDL(ctx context.Context) error {
	operation, finish, err := t.enterOperation(ctx, true)
	if err != nil {
		return err
	}
	defer finish()
	err = t.sequences.clean(operation)
	if t.closed && !t.active && t.indexSubmission == nil {
		t.closeError = err
	}
	return err
}
