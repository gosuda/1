package neosepia

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

const defaultMaxOpenFDs = 256

// fdPool maintains a concurrency-safe, LRU-bounded pool of read-only OS file
// descriptors for immutable SST files. In-flight ReadAt/Stat callers hold a
// pin on their fdEntry so evicted descriptors are closed only after the last
// concurrent caller releases them.
type fdPool struct {
	mu      sync.Mutex
	maxOpen int
	entries map[uint64]*fdEntry
	head    *fdEntry // MRU
	tail    *fdEntry // LRU
	openCnt int
	closed  bool

	opens  atomic.Uint64
	hits   atomic.Uint64
	evicts atomic.Uint64
}

type fdEntry struct {
	fileID   uint64
	file     *os.File
	pinCount int
	evicted  bool
	prev     *fdEntry
	next     *fdEntry
}

func newFDPool(maxOpen int) *fdPool {
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenFDs
	}
	return &fdPool{
		maxOpen: maxOpen,
		entries: make(map[uint64]*fdEntry, min(maxOpen, 64)),
	}
}

func (p *fdPool) acquire(fileID uint64, path string) (*fdEntry, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, storage.ErrClosed
	}
	if e, ok := p.entries[fileID]; ok {
		p.moveToFrontLocked(e)
		e.pinCount++
		p.hits.Add(1)
		p.mu.Unlock()
		return e, nil
	}
	p.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = f.Close()
		return nil, storage.ErrClosed
	}
	if e, ok := p.entries[fileID]; ok {
		p.moveToFrontLocked(e)
		e.pinCount++
		p.hits.Add(1)
		p.mu.Unlock()
		_ = f.Close()
		return e, nil
	}
	e := &fdEntry{fileID: fileID, file: f, pinCount: 1}
	p.entries[fileID] = e
	p.pushFrontLocked(e)
	p.openCnt++
	p.opens.Add(1)
	toClose := p.evictIdleLocked()
	p.mu.Unlock()

	for _, stale := range toClose {
		_ = stale.Close()
	}
	return e, nil
}

func (p *fdPool) release(e *fdEntry) {
	if e == nil {
		return
	}
	var toClose []*os.File
	p.mu.Lock()
	e.pinCount--
	if e.pinCount == 0 && e.evicted && e.file != nil {
		toClose = append(toClose, e.file)
		e.file = nil
	}
	if !p.closed && p.openCnt > p.maxOpen {
		toClose = append(toClose, p.evictIdleLocked()...)
	}
	p.mu.Unlock()

	for _, f := range toClose {
		_ = f.Close()
	}
}

// adopt takes ownership of an already-open read-only file handle and places it
// into the pool (or closes it immediately if the pool is closed or duplicate).
func (p *fdPool) adopt(fileID uint64, f *os.File) {
	if f == nil {
		return
	}
	p.mu.Lock()
	if p.closed || p.maxOpen <= 0 {
		p.mu.Unlock()
		_ = f.Close()
		return
	}
	if _, exists := p.entries[fileID]; exists {
		p.mu.Unlock()
		_ = f.Close()
		return
	}
	e := &fdEntry{fileID: fileID, file: f}
	p.entries[fileID] = e
	p.pushFrontLocked(e)
	p.openCnt++
	toClose := p.evictIdleLocked()
	p.mu.Unlock()

	for _, stale := range toClose {
		_ = stale.Close()
	}
}

// evict removes fileID from the pool and closes its FD once unpinned.
func (p *fdPool) evict(fileID uint64) error {
	var toClose *os.File
	p.mu.Lock()
	if e, ok := p.entries[fileID]; ok {
		delete(p.entries, fileID)
		p.unlinkLocked(e)
		p.openCnt--
		e.evicted = true
		if e.pinCount == 0 {
			toClose = e.file
			e.file = nil
		}
	}
	p.mu.Unlock()
	if toClose != nil {
		return toClose.Close()
	}
	return nil
}

func (p *fdPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var toClose []*os.File
	for id, e := range p.entries {
		delete(p.entries, id)
		p.unlinkLocked(e)
		e.evicted = true
		if e.pinCount == 0 && e.file != nil {
			toClose = append(toClose, e.file)
			e.file = nil
		}
	}
	p.openCnt = 0
	p.head, p.tail = nil, nil
	p.mu.Unlock()

	var err error
	for _, f := range toClose {
		err = errors.Join(err, f.Close())
	}
	return err
}

func (p *fdPool) OpenCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.openCnt
}

func (p *fdPool) setMaxOpen(maxOpen int) {
	if maxOpen <= 0 {
		return
	}
	p.mu.Lock()
	p.maxOpen = maxOpen
	toClose := p.evictIdleLocked()
	p.mu.Unlock()
	for _, f := range toClose {
		_ = f.Close()
	}
}

func (p *fdPool) evictIdleLocked() []*os.File {
	var toClose []*os.File
	for cur := p.tail; cur != nil && p.openCnt > p.maxOpen; {
		prev := cur.prev
		if cur.pinCount == 0 {
			delete(p.entries, cur.fileID)
			p.unlinkLocked(cur)
			p.openCnt--
			cur.evicted = true
			p.evicts.Add(1)
			if cur.file != nil {
				toClose = append(toClose, cur.file)
				cur.file = nil
			}
		}
		cur = prev
	}
	return toClose
}

func (p *fdPool) pushFrontLocked(e *fdEntry) {
	e.prev = nil
	e.next = p.head
	if p.head != nil {
		p.head.prev = e
	} else {
		p.tail = e
	}
	p.head = e
}

func (p *fdPool) unlinkLocked(e *fdEntry) {
	if e.prev != nil {
		e.prev.next = e.next
	} else if p.head == e {
		p.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else if p.tail == e {
		p.tail = e.prev
	}
	e.prev, e.next = nil, nil
}

func (p *fdPool) moveToFrontLocked(e *fdEntry) {
	if p.head == e {
		return
	}
	p.unlinkLocked(e)
	p.pushFrontLocked(e)
}
