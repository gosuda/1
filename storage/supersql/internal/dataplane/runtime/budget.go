package runtime

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrClosed        = errors.New("query resource closed")
	ErrBudget        = errors.New("query resource budget exceeded")
	ErrInvalidConfig = errors.New("invalid query resource configuration")
	ErrSpillDisabled = errors.New("query spill disabled")
	ErrFenced        = errors.New("spill extent restoration failed")
	ErrQueueFull     = errors.New("worker queue full")
	ErrReadersActive = errors.New("spill readers are active")
)

func resourceError(code, operation string, cause error) error {
	return &model.Error{Code: code, Message: operation, Cause: cause}
}

func diskError(operation string, err error) error {
	if err == nil {
		return nil
	}
	code := "58030"
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		code = "53100"
	}
	return resourceError(code, operation, err)
}

// Usage counts admitted bytes, including growth charged before its I/O begins.
type Usage struct {
	Limit        int64
	Live         int64
	Peak         int64
	Reservations int
}

// Budget never waits for bytes. A shared root enforces a cross-query cap;
// query work and output accounts both debit that root.
type Budget struct {
	mu              *sync.Mutex
	parent          *Budget
	limit           int64
	live            int64
	peak            int64
	maxReservations int
	count           int
	head            *Reservation
	closed          bool
}

func NewBudget(bytes int64, maxReservations int) (*Budget, error) {
	if bytes <= 0 || maxReservations <= 0 {
		return nil, fmt.Errorf("%w: positive bytes and reservation count required", ErrInvalidConfig)
	}
	return &Budget{mu: new(sync.Mutex), limit: bytes, maxReservations: maxReservations}, nil
}

func (b *Budget) child(bytes int64, maxReservations int) *Budget {
	return &Budget{mu: b.mu, parent: b, limit: bytes, maxReservations: maxReservations}
}

func (b *Budget) Usage() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Usage{Limit: b.limit, Live: b.live, Peak: b.peak, Reservations: b.count}
}

func (b *Budget) admit(bytes int64, newReservation bool) error {
	for account := b; account != nil; account = account.parent {
		if account.closed {
			return ErrClosed
		}
		if bytes > account.limit-account.live || (newReservation && account.count == account.maxReservations) {
			return resourceError("53200", "reserve query bytes", ErrBudget)
		}
	}
	for account := b; account != nil; account = account.parent {
		account.live += bytes
		account.peak = max(account.peak, account.live)
		if newReservation {
			account.count++
		}
	}
	return nil
}

func (b *Budget) TryReserve(bytes int64) (*Reservation, error) {
	if bytes <= 0 {
		return nil, fmt.Errorf("%w: reservation must be positive", ErrInvalidConfig)
	}
	return b.reserve(bytes)
}

func (b *Budget) reserve(bytes int64) (*Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.admit(bytes, true); err != nil {
		return nil, err
	}
	r := &Reservation{budget: b, bytes: bytes, next: b.head}
	if b.head != nil {
		b.head.previous = r
	}
	b.head = r
	return r, nil
}

// Reservation owns one accounting claim. Resize and Release are concurrent-safe;
// Release is idempotent. It does not free memory allocated by its caller.
type Reservation struct {
	budget   *Budget
	bytes    int64
	previous *Reservation
	next     *Reservation
	released bool
}

func (r *Reservation) Bytes() int64 {
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	return r.bytes
}

func (r *Reservation) Resize(bytes int64) error {
	if bytes < 0 {
		return fmt.Errorf("%w: negative reservation", ErrInvalidConfig)
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	if r.released {
		return ErrClosed
	}
	if bytes > r.bytes {
		if err := r.budget.admit(bytes-r.bytes, false); err != nil {
			return err
		}
	} else {
		for account := r.budget; account != nil; account = account.parent {
			account.live -= r.bytes - bytes
		}
	}
	r.bytes = bytes
	return nil
}

// GrowUpTo reserves available headroom across every ancestor without waiting.
// It returns the admitted increase, not the resulting total reservation size.
func (r *Reservation) GrowUpTo(maximum int64) (int64, error) {
	if maximum < 0 {
		return 0, fmt.Errorf("%w: negative reservation growth", ErrInvalidConfig)
	}
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	if r.released {
		return 0, ErrClosed
	}
	growth := maximum
	for account := r.budget; account != nil; account = account.parent {
		if account.closed {
			return 0, ErrClosed
		}
		growth = min(growth, account.limit-account.live)
	}
	if err := r.budget.admit(growth, false); err != nil {
		return 0, err
	}
	r.bytes += growth
	return growth, nil
}

func (r *Reservation) Release() {
	r.budget.mu.Lock()
	defer r.budget.mu.Unlock()
	r.releaseLocked()
}

func (r *Reservation) releaseLocked() {
	if r.released {
		return
	}
	for account := r.budget; account != nil; account = account.parent {
		account.live -= r.bytes
		account.count--
	}
	if r.previous == nil {
		r.budget.head = r.next
	} else {
		r.previous.next = r.next
	}
	if r.next != nil {
		r.next.previous = r.previous
	}
	r.bytes, r.previous, r.next, r.released = 0, nil, nil, true
}

func (b *Budget) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for b.head != nil {
		b.head.releaseLocked()
	}
}
