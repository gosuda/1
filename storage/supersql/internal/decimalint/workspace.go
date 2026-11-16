// Package decimalint provides caller-owned signed base-10 integer coefficients.
package decimalint

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
)

const limbBase uint64 = 1000000000
const limbDigits = 9

var (
	ErrInvalid        = errors.New("invalid decimal integer operation")
	ErrClosed         = errors.New("decimal integer ownership released")
	ErrLimit          = errors.New("decimal integer workspace limit exceeded")
	ErrDivisionByZero = errors.New("decimal integer division by zero")
	ErrNegativeSqrt   = errors.New("decimal integer square root of negative value")
)

// Workspace is single-owner. It borrows Allocator and owns every result until
// Int.Close or Workspace.Close. No storage is pooled or retained after Close.
type Workspace struct {
	claim       columnar.Allocation
	limit, used uint64
	head        *Int
	serial      uint64
	closed      bool
}

// Int is immutable after publication. Arithmetic borrows inputs and returns an
// independent owned result. Access after Close is a programming error.
type Int struct {
	limbs          []uint32
	negative       bool
	owner          *Workspace
	previous, next *Int
	bytes          uint64
	serial         uint64
	released       bool
}

// OwnedBytes reports this value's header and retained limb capacity.
func (value *Int) OwnedBytes() uint64 {
	if value == nil {
		return 0
	}
	value.requireLive()
	return value.bytes
}

func NewWorkspace(ctx context.Context, allocator columnar.Allocator, limit uint64) (*Workspace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	header := uint64(unsafe.Sizeof(Workspace{}))
	if limit < header || limit > uint64(math.MaxInt) {
		return nil, ErrLimit
	}
	var claim columnar.Allocation
	if allocator != nil {
		var err error
		claim, err = allocator.Reserve(header)
		if err != nil {
			return nil, err
		}
	}
	return &Workspace{claim: claim, limit: limit, used: header}, nil
}

func (workspace *Workspace) check(ctx context.Context, values ...*Int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if workspace == nil || workspace.closed {
		return ErrClosed
	}
	for _, value := range values {
		if value == nil {
			return ErrInvalid
		}
		if value.released || value.owner == nil || value.owner.closed {
			return ErrClosed
		}
	}
	return nil
}

func (workspace *Workspace) newInt(ctx context.Context, capacity int) (*Int, error) {
	if err := workspace.check(ctx); err != nil {
		return nil, err
	}
	if workspace.serial == math.MaxUint64 {
		return nil, ErrLimit
	}
	header := uint64(unsafe.Sizeof(Int{}))
	if capacity < 0 || capacity > math.MaxInt/limbDigits-1 || header > workspace.limit-workspace.used {
		return nil, ErrLimit
	}
	remaining := workspace.limit - workspace.used - header
	if uint64(capacity) > remaining/4 {
		return nil, ErrLimit
	}
	bytes := header + uint64(capacity)*4
	if workspace.claim != nil {
		if err := workspace.claim.Resize(workspace.used + bytes); err != nil {
			return nil, err
		}
	}
	workspace.serial++
	value := &Int{owner: workspace, limbs: make([]uint32, capacity), bytes: bytes, next: workspace.head, serial: workspace.serial}
	if workspace.head != nil {
		workspace.head.previous = value
	}
	workspace.head = value
	workspace.used += bytes
	return value, nil
}

func (workspace *Workspace) UsedBytes() uint64 {
	if workspace == nil {
		return 0
	}
	return workspace.used
}

func (value *Int) normalize() {
	for len(value.limbs) > 0 && value.limbs[len(value.limbs)-1] == 0 {
		value.limbs = value.limbs[:len(value.limbs)-1]
	}
	if len(value.limbs) == 0 {
		value.negative = false
	}
}
func (value *Int) requireLive() {
	if value == nil || value.released || value.owner == nil || value.owner.closed {
		panic(ErrClosed)
	}
}
func (value *Int) Sign() int {
	value.requireLive()
	if len(value.limbs) == 0 {
		return 0
	}
	if value.negative {
		return -1
	}
	return 1
}
func (value *Int) Digits() int {
	value.requireLive()
	if len(value.limbs) == 0 {
		return 1
	}
	top := value.limbs[len(value.limbs)-1]
	digits := (len(value.limbs)-1)*limbDigits + 1
	for top >= 10 {
		top /= 10
		digits++
	}
	return digits
}

func (value *Int) Close() error {
	if value == nil || value.released {
		return nil
	}
	workspace := value.owner
	if value.previous == nil {
		workspace.head = value.next
	} else {
		value.previous.next = value.next
	}
	if value.next != nil {
		value.next.previous = value.previous
	}
	bytes := value.bytes
	value.limbs = nil
	value.owner = nil
	value.previous = nil
	value.next = nil
	value.bytes = 0
	value.negative = false
	value.released = true
	workspace.used -= bytes
	if workspace.claim != nil {
		return workspace.claim.Resize(workspace.used)
	}
	return nil
}
func (workspace *Workspace) Close() error {
	if workspace == nil || workspace.closed {
		return nil
	}
	for value := workspace.head; value != nil; {
		next := value.next
		value.limbs = nil
		value.owner = nil
		value.previous = nil
		value.next = nil
		value.bytes = 0
		value.negative = false
		value.released = true
		value = next
	}
	workspace.head = nil
	workspace.used = 0
	workspace.closed = true
	if workspace.claim != nil {
		workspace.claim.Release()
		workspace.claim = nil
	}
	return nil
}

func (workspace *Workspace) Clone(ctx context.Context, source *Int) (*Int, error) {
	if err := workspace.check(ctx, source); err != nil {
		return nil, err
	}
	value, err := workspace.newInt(ctx, len(source.limbs))
	if err != nil {
		return nil, err
	}
	for i := range source.limbs {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, errors.Join(err, value.Close())
			}
		}
		value.limbs[i] = source.limbs[i]
	}
	value.negative = source.negative
	return value, nil
}
func (workspace *Workspace) FromInt64(ctx context.Context, input int64) (*Int, error) {
	magnitude := uint64(input)
	if input < 0 {
		magnitude = 0 - magnitude
	}
	count := 0
	for remaining := magnitude; remaining != 0; remaining /= limbBase {
		count++
	}
	value, err := workspace.newInt(ctx, count)
	if err != nil {
		return nil, err
	}
	for i := range value.limbs {
		value.limbs[i] = uint32(magnitude % limbBase)
		magnitude /= limbBase
	}
	value.negative = input < 0
	return value, nil
}

func compareMagnitude(ctx context.Context, left, right *Int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(left.limbs) < len(right.limbs) {
		return -1, nil
	}
	if len(left.limbs) > len(right.limbs) {
		return 1, nil
	}
	for i := len(left.limbs) - 1; i >= 0; i-- {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if left.limbs[i] < right.limbs[i] {
			return -1, nil
		}
		if left.limbs[i] > right.limbs[i] {
			return 1, nil
		}
	}
	return 0, nil
}
func (value *Int) Cmp(ctx context.Context, other *Int) (int, error) {
	if value == nil {
		return 0, ErrInvalid
	}
	if err := value.owner.check(ctx, value, other); err != nil {
		return 0, err
	}
	leftSign, rightSign := value.Sign(), other.Sign()
	if leftSign < rightSign {
		return -1, nil
	}
	if leftSign > rightSign {
		return 1, nil
	}
	order, err := compareMagnitude(ctx, value, other)
	if value.negative {
		order = -order
	}
	return order, err
}
