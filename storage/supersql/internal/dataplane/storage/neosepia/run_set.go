package neosepia

import (
	"context"
	"errors"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type runSetLimits struct {
	MaxBytes          uint64
	MaxFiles, MaxPins uint64
}

type runGeneration struct {
	parent         *runGeneration
	files          []*sstFile
	activeFiles    []*sstFile
	inactiveFiles  []*sstFile
	levels         [MaxLevels][]*sstFile
	position       uint64
	timestamp      model.Timestamp
	baseGeneration uint64
	references     uint64
	bytes          uint64
}

// The resident quota excludes separately budgeted preparation and scan blocks.
// A published generation transfers the current-root reference to its parent edge.
// Pins keep that immutable ancestry alive without copying the database file list.
type runSet struct {
	mu                 sync.Mutex
	head               *runGeneration
	pinHead            *runPin
	limits             runSetLimits
	bytes, files, pins uint64
	closed             bool
	reservation        *runAdmission
}

type runPin struct {
	owner          *runSet
	generation     *runGeneration
	previous, next *runPin
	timestamp      model.Timestamp
	closed         bool
}

// A reservation owns validated native files and resident capacity until atomic
// installation, abort, or set closure. Failed publication keeps it retryable.
type runAdmission struct {
	owner      *runSet
	generation *runGeneration
}

func newRunSet(limits runSetLimits) (*runSet, error) {
	fixed := uint64(unsafe.Sizeof(runSet{}))
	if limits.MaxBytes < fixed || limits.MaxFiles == 0 || limits.MaxPins == 0 {
		return nil, columnar.ErrBatchBudget
	}
	return &runSet{limits: limits, bytes: fixed}, nil
}

// publish consumes files on every return; durable writers reserve before WAL.
func (set *runSet) publish(ctx context.Context, position uint64, timestamp model.Timestamp, files []*sstFile) (err error) {
	admission, err := set.reserve(ctx, position, timestamp, files)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, admission.Close()) }()
	return admission.publish(ctx)
}

// reserve consumes files on every return. Charging the reservation under mu
// prevents new snapshot pins from consuming its installation capacity.
func (set *runSet) reserve(ctx context.Context, position uint64, timestamp model.Timestamp, files []*sstFile) (_ *runAdmission, err error) {
	clear(files[len(files):cap(files)])
	set.mu.Lock()
	defer set.mu.Unlock()
	defer func() {
		if err != nil {
			for _, file := range files {
				if file != nil {
					err = errors.Join(err, file.Close())
				}
			}
		}
	}()
	if set.closed {
		return nil, storage.ErrClosed
	}
	if set.reservation != nil {
		return nil, storage.ErrCommitIndeterminate
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if position == 0 || timestamp <= 0 || len(files) == 0 {
		return nil, storage.ErrInvalidMutation
	}
	if set.head != nil {
		if position <= set.head.position || timestamp <= set.head.timestamp {
			return nil, storage.ErrInvalidMutation
		}
		if position-set.head.position != 1 {
			return nil, storage.ErrInvalidMutation
		}
	}
	if uint64(len(files)) > set.limits.MaxFiles-set.files {
		return nil, columnar.ErrBatchBudget
	}
	remaining := set.limits.MaxBytes - set.bytes
	reservationBytes := uint64(unsafe.Sizeof(runAdmission{}))
	if reservationBytes > remaining {
		return nil, columnar.ErrBatchBudget
	}
	remaining -= reservationBytes
	bytes := uint64(unsafe.Sizeof(runGeneration{}))
	pointers := uint64(unsafe.Sizeof((*sstFile)(nil)))
	if bytes > remaining || uint64(cap(files)) > (remaining-bytes)/pointers {
		return nil, columnar.ErrBatchBudget
	}
	bytes += uint64(cap(files)) * pointers
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if file == nil {
			return nil, storage.ErrCorruptStorage
		}
		file.mu.RLock()
		closed := file.file == nil
		cost := uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(file.index)) + uint64(cap(file.descriptor.LowerKey)+cap(file.descriptor.UpperKey))
		file.mu.RUnlock()
		if closed {
			return nil, storage.ErrClosed
		}
		if cost > remaining-bytes {
			return nil, columnar.ErrBatchBudget
		}
		bytes += cost
	}
	generation := &runGeneration{parent: set.head, files: files, position: position, timestamp: timestamp, references: 1, bytes: bytes}
	generation.indexTiers()
	if err := generation.validateLevels(ctx); err != nil {
		return nil, err
	}
	admission := &runAdmission{owner: set, generation: generation}
	set.reservation = admission
	set.bytes += bytes + reservationBytes
	set.files += uint64(len(files))
	return admission, nil
}

func (admission *runAdmission) publish(ctx context.Context) error {
	set := admission.owner
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed || admission.generation == nil {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	set.head = admission.generation
	set.reservation = nil
	set.bytes -= uint64(unsafe.Sizeof(runAdmission{}))
	admission.generation = nil
	return nil
}

func (admission *runAdmission) Close() error {
	set := admission.owner
	set.mu.Lock()
	defer set.mu.Unlock()
	return admission.closeLocked()
}

func (admission *runAdmission) closeLocked() error {
	generation := admission.generation
	if generation == nil {
		return nil
	}
	admission.generation = nil
	set := admission.owner
	set.reservation = nil
	set.bytes -= uint64(unsafe.Sizeof(runAdmission{}))
	// The parent still belongs to the current root, not this uninstalled edge.
	generation.parent = nil
	return set.releaseLocked(generation)
}

func (set *runSet) publishBase(ctx context.Context, position uint64, timestamp model.Timestamp, baseGeneration uint64, files []*sstFile) error {
	if baseGeneration == 0 {
		return storage.ErrCorruptStorage
	}
	if len(files) != 0 {
		if err := set.publish(ctx, position, timestamp, files); err != nil {
			return err
		}
		set.mu.Lock()
		defer set.mu.Unlock()
		if set.head == nil || set.head.position != position || set.head.parent != nil {
			return storage.ErrCorruptStorage
		}
		set.head.baseGeneration = baseGeneration
		return nil
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return storage.ErrClosed
	}
	if set.reservation != nil {
		return storage.ErrCommitIndeterminate
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if set.head != nil || position == 0 || timestamp <= 0 {
		return storage.ErrCorruptStorage
	}
	bytes := uint64(unsafe.Sizeof(runGeneration{}))
	if bytes > set.limits.MaxBytes-set.bytes {
		return columnar.ErrBatchBudget
	}
	set.head = &runGeneration{position: position, timestamp: timestamp, baseGeneration: baseGeneration, references: 1, bytes: bytes}
	set.bytes += bytes
	return nil
}

func (set *runSet) pin(ctx context.Context) (*runPin, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil, storage.ErrClosed
	}
	return set.pinLocked(ctx, set.head, 0)
}

func (set *runSet) pinLocked(ctx context.Context, generation *runGeneration, timestamp model.Timestamp) (*runPin, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cost := uint64(unsafe.Sizeof(runPin{}))
	if set.pins == set.limits.MaxPins || cost > set.limits.MaxBytes-set.bytes {
		return nil, columnar.ErrBatchBudget
	}
	if generation != nil {
		generation.references++
	}
	set.pins++
	set.bytes += cost
	pin := &runPin{owner: set, generation: generation, timestamp: timestamp, next: set.pinHead}
	if set.pinHead != nil {
		set.pinHead.previous = pin
	}
	set.pinHead = pin
	return pin, nil
}

// Retained pins remain readable after the set closes; they own the remaining
// file handles. A derived cursor retains its own pin before its snapshot closes.
func (pin *runPin) retain(ctx context.Context) (*runPin, error) {
	set := pin.owner
	set.mu.Lock()
	defer set.mu.Unlock()
	if pin.closed {
		return nil, storage.ErrClosed
	}
	return set.pinLocked(ctx, pin.generation, pin.timestamp)
}

// visit borrows files for the duration of the call. The caller holds this pin
// through any cursor borrowing those files; visit and Close are serialized.
func (pin *runPin) visit(ctx context.Context, target storage.ObjectRef, visit func(*sstFile) error) error {
	if pin.closed {
		return storage.ErrClosed
	}
	for generation := pin.generation; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil {
				return err
			}
			if file.target.ObjectID != target.ObjectID || file.target.Generation != target.Generation {
				continue
			}
			if err := visit(file); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (pin *runPin) Close() error {
	set := pin.owner
	set.mu.Lock()
	defer set.mu.Unlock()
	if pin.closed {
		return nil
	}
	pin.closed = true
	generation := pin.generation
	pin.generation = nil
	if pin.previous != nil {
		pin.previous.next = pin.next
	} else {
		set.pinHead = pin.next
	}
	if pin.next != nil {
		pin.next.previous = pin.previous
	}
	pin.previous, pin.next = nil, nil
	set.pins--
	set.bytes -= uint64(unsafe.Sizeof(runPin{}))
	return set.releaseLocked(generation)
}

func (set *runSet) Close() error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil
	}
	set.closed = true
	generation := set.head
	set.head = nil
	var err error
	if set.reservation != nil {
		err = set.reservation.closeLocked()
	}
	return errors.Join(err, set.releaseLocked(generation))
}

func (set *runSet) releaseLocked(generation *runGeneration) error {
	var err error
	for generation != nil {
		generation.references--
		if generation.references != 0 {
			break
		}
		for _, file := range generation.files {
			err = errors.Join(err, file.Close())
		}
		set.files -= uint64(len(generation.files))
		set.bytes -= generation.bytes
		parent := generation.parent
		generation.files = nil
		generation.activeFiles = nil
		generation.inactiveFiles = nil
		generation.levels = [MaxLevels][]*sstFile{}
		generation.parent = nil
		generation = parent
	}
	return err
}

