package neosepia

import (
	"sync"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Source metadata is admitted separately. This invocation-owned ledger accounts
// encoded blocks and lazy decoded caches until their final block lease releases.
// Per-block ceilings still apply independently of this aggregate limit.
type sourceMemory struct {
	mu       sync.Mutex
	limit    uint64
	used     uint64
	observer storage.ScanObserver
}

func (m *sourceMemory) reserve(bytes uint64) error {
	if m == nil || bytes == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if bytes > m.limit-m.used {
		return columnar.ErrBatchBudget
	}
	m.used += bytes
	return nil
}

func (m *sourceMemory) release(bytes uint64) {
	if m == nil || bytes == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if bytes > m.used {
		panic("native block memory reservation underflow")
	}
	m.used -= bytes
}

func (m *sourceMemory) available() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit - m.used
}

func (m *sourceMemory) observe(observation storage.ScanObservation) {
	if m != nil && m.observer != nil {
		m.observer.ObserveScan(observation)
	}
}
