package txn

import (
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// Registration is charged monotonically: abandoned bindings can outlive rollback.
// The registry is released at transaction end; retained bindings own only atomics.
type publicationRegistry struct {
	mu          sync.Mutex
	owner       *model.PublicationOwner
	bindings    []*model.ProvisionalBinding
	used, limit uint64
}

func (transaction *Transaction) PublicationOwner() *model.PublicationOwner {
	return transaction.publications.owner
}

func (view *transactionView) PublicationOwner() *model.PublicationOwner {
	return view.publicationOwner
}

func (view *transactionView) ProvisionalBinding(ordinal uint64) (*model.ProvisionalBinding, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return nil, errTransactionInactive
	}
	registry := &view.transaction.publications
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, binding := range registry.bindings {
		if binding.Ordinal() == ordinal {
			return binding, nil
		}
	}
	charge := uint64(unsafe.Sizeof(model.ProvisionalBinding{})) + 2*uint64(unsafe.Sizeof((*model.ProvisionalBinding)(nil)))
	if ordinal == 0 || charge > registry.limit-registry.used {
		return nil, errTransactionBudget
	}
	binding := model.NewProvisionalBinding(registry.owner, ordinal)
	registry.bindings = append(registry.bindings, binding)
	registry.used += charge
	return binding, nil
}

func (registry *publicationRegistry) rollback(ordinal uint64) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	retained := registry.bindings[:0]
	for _, binding := range registry.bindings {
		if binding.Ordinal() > ordinal {
			binding.Invalidate()
		} else {
			retained = append(retained, binding)
		}
	}
	clear(registry.bindings[len(retained):])
	registry.bindings = retained
}

func (registry *publicationRegistry) finish() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.bindings = nil
}
