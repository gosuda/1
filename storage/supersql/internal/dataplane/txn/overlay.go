package txn

import (
	"bytes"
	"cmp"
	"context"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type overlayKey struct {
	target storage.ObjectRef
	key    []byte
	edge   int8
}

func (a overlayKey) Compare(b overlayKey) int {
	if order := cmp.Compare(a.target.ObjectID, b.target.ObjectID); order != 0 {
		return order
	}
	if order := cmp.Compare(a.target.Generation, b.target.Generation); order != 0 {
		return order
	}
	if order := cmp.Compare(a.target.DescriptorVersion, b.target.DescriptorVersion); order != 0 {
		return order
	}
	if order := cmp.Compare(a.target.EncodingVersion, b.target.EncodingVersion); order != 0 {
		return order
	}
	if a.edge != 0 || b.edge != 0 {
		return cmp.Compare(a.edge, b.edge)
	}
	return bytes.Compare(a.key, b.key)
}

type overlayEntry struct {
	inline      *storage.Mutation
	op          storage.MutationOp
	ordinal     uint64
	key         []byte
	ref         mutationLogRef
	memBytes    uint64
	inlineBytes uint64
}

type overlayRangeEntry struct {
	mutation storage.Mutation
	memBytes uint64
}

type overlayPointTree = ordered.Tree[overlayKey, overlayEntry]
type overlayPointCursor = ordered.Cursor[overlayKey, overlayEntry]
type overlayRangeTree = ordered.Tree[overlayKey, overlayRangeEntry]
type overlayRangeCursor = ordered.Cursor[overlayKey, overlayRangeEntry]

type overlayState struct {
	points overlayPointTree
	ranges overlayRangeTree
}

// Overlay owns a bounded normalized write set. Frozen views share immutable tree
// nodes. Ranges are disjoint ordinal-labelled segments, so lookup needs only the
// predecessor interval rather than a scan of every staged range tombstone.
type Overlay struct {
	mu                             sync.Mutex
	state                          overlayState
	overhead                       uint64
	limit, inlineLimit, indexLimit uint64
	payloadBytes, inlineBytes      uint64
	lastOrdinal                    uint64
	log                            *mutationLog
	// catalogVersion counts the staged changes that rewrite catalog rows. It is
	// the content identity of the overlay's catalog contribution: callers that
	// derive catalog metadata from a frozen overlay can reuse the result while
	// this value is unchanged, and every staged catalog mutation advances it.
	catalogVersion uint64
	closed         bool
}

func NewOverlay(maxBytes uint64) (*Overlay, error) {
	overhead := uint64(unsafe.Sizeof(Overlay{}))
	if maxBytes < overhead {
		return nil, ordered.ErrBudget
	}
	return &Overlay{
		overhead:    overhead,
		limit:       maxBytes,
		inlineLimit: maxBytes,
		indexLimit:  maxBytes,
	}, nil
}

// newTransactionOverlay creates an overlay backed by the transaction's
// spillable mutation log. Inline payloads stay bounded by memoryLimit/2 while
// spilled entries retain only keys and log offsets so a transaction can stage
// up to spillLimit without duplicating full row payloads in RAM.
func newTransactionOverlay(memoryLimit, spillLimit uint64, log *mutationLog) (*Overlay, error) {
	overhead := uint64(unsafe.Sizeof(Overlay{}))
	if memoryLimit < overhead {
		return nil, ordered.ErrBudget
	}
	if log == nil || log.limits.DisableSpill {
		return &Overlay{
			overhead:    overhead,
			limit:       memoryLimit,
			inlineLimit: memoryLimit,
			indexLimit:  memoryLimit,
		}, nil
	}
	return &Overlay{
		overhead:    overhead,
		limit:       memoryLimit,
		inlineLimit: memoryLimit / 2,
		indexLimit:  max(memoryLimit, spillLimit),
		log:         log,
	}, nil
}

// StageOwned atomically stages a statement. On success callers relinquish mutable
// aliases to keys, column IDs and values; failed statements retain caller ownership.
// Retained slice capacities, including values beyond the visible length, count
// against the limit: ownership transfer keeps those backing arrays alive.
func (o *Overlay) StageOwned(ctx context.Context, mutations []storage.Mutation) error {
	return o.stageOwned(ctx, mutations, nil)
}

func (o *Overlay) stageOwnedWithRef(ctx context.Context, mutation storage.Mutation, ref mutationLogRef) error {
	mutations := [1]storage.Mutation{mutation}
	refs := [1]mutationLogRef{ref}
	return o.stageOwned(ctx, mutations[:], refs[:])
}

func (o *Overlay) stageOwned(ctx context.Context, mutations []storage.Mutation, refs []mutationLogRef) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return storage.ErrClosed
	}
	next := o.state
	payloadBytes, inlineBytes, last := o.payloadBytes, o.inlineBytes, o.lastOrdinal
	catalog := o.catalogVersion
	for i, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mutation.Ordinal <= last {
			return storage.ErrUnnormalizedBatch
		}
		if mutation.Op != storage.MutationUpsert && mutation.Op != storage.MutationDelete && mutation.Op != storage.MutationDeleteRange {
			return storage.ErrUnnormalizedBatch
		}
		if mutation.Target.ObjectID >= model.CatalogCollectionObjectID {
			catalog++
		}
		if _, err := storage.EncodedMutationSize(mutation); err != nil {
			return err
		}
		if mutation.Op == storage.MutationDeleteRange {
			var err error
			next.ranges, payloadBytes, err = stageOverlayRange(ctx, next.ranges, mutation, payloadBytes, o.overhead+next.points.NodeBytes(), o.indexLimit)
			if err != nil {
				return err
			}
		} else {
			var err error
			var ref mutationLogRef
			hasRef := refs != nil && o.log != nil
			if hasRef {
				ref = refs[i]
			}
			next.points, payloadBytes, inlineBytes, err = o.stagePoint(next.points, next.ranges.NodeBytes(), mutation, ref, hasRef, payloadBytes, inlineBytes)
			if err != nil {
				return err
			}
		}
		last = mutation.Ordinal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.state = next
	o.payloadBytes, o.inlineBytes, o.lastOrdinal, o.catalogVersion = payloadBytes, inlineBytes, last, catalog
	return nil
}

func (o *Overlay) stagePoint(points overlayPointTree, rangeNodes uint64, mutation storage.Mutation, ref mutationLogRef, hasRef bool, payloadBytes, inlineBytes uint64) (overlayPointTree, uint64, uint64, error) {
	key := overlayKey{target: mutation.Target, key: mutation.Key}
	var reclaimedPayload, reclaimedInline uint64
	if old, existed := points.Get(key); existed {
		reclaimedPayload = old.memBytes
		reclaimedInline = old.inlineBytes
	}
	basePayload := payloadBytes - reclaimedPayload
	baseInline := inlineBytes - reclaimedInline

	var entry overlayEntry
	if !hasRef {
		if o.overhead+basePayload >= o.limit {
			return points, payloadBytes, inlineBytes, ordered.ErrBudget
		}
		size, err := mutationMemory(mutation, o.limit-(o.overhead+basePayload))
		if err != nil {
			return points, payloadBytes, inlineBytes, err
		}
		owned := mutation
		entry = overlayEntry{
			inline:      &owned,
			op:          mutation.Op,
			ordinal:     mutation.Ordinal,
			key:         mutation.Key,
			memBytes:    size,
			inlineBytes: size,
		}
	} else {
		liveNodes := points.NodeBytes() + rangeNodes
		storedInline := false
		if baseInline < o.inlineLimit && o.overhead+liveNodes+basePayload < o.limit {
			maxInline := min(o.inlineLimit-baseInline, o.limit-(o.overhead+liveNodes+basePayload))
			if size, err := mutationMemory(mutation, maxInline); err == nil {
				owned := mutation
				entry = overlayEntry{
					inline:      &owned,
					op:          mutation.Op,
					ordinal:     mutation.Ordinal,
					key:         mutation.Key,
					ref:         ref,
					memBytes:    size,
					inlineBytes: size,
				}
				storedInline = true
			}
		}
		if !storedInline {
			keyCopy := bytes.Clone(mutation.Key)
			keyMem := uint64(cap(keyCopy))
			entry = overlayEntry{
				op:          mutation.Op,
				ordinal:     mutation.Ordinal,
				key:         keyCopy,
				ref:         ref,
				memBytes:    keyMem,
				inlineBytes: 0,
			}
			key.key = keyCopy
		}
	}

	budgetLimit := o.limit
	if hasRef {
		budgetLimit = o.indexLimit
	}
	if entry.memBytes > budgetLimit-(o.overhead+basePayload) {
		return points, payloadBytes, inlineBytes, ordered.ErrBudget
	}
	newPayload := basePayload + entry.memBytes
	treeBudget := budgetLimit - (o.overhead + newPayload)
	updated, _, err := points.Set(key, entry, treeBudget)
	if err != nil {
		return points, payloadBytes, inlineBytes, err
	}
	if o.overhead+updated.NodeBytes()+rangeNodes+newPayload > budgetLimit {
		return points, payloadBytes, inlineBytes, ordered.ErrBudget
	}
	return updated, newPayload, baseInline + entry.inlineBytes, nil
}

func stageOverlayRange(ctx context.Context, tree overlayRangeTree, mutation storage.Mutation, payloadBytes, baseOverhead, limit uint64) (overlayRangeTree, uint64, error) {
	if baseOverhead+payloadBytes > limit {
		return tree, payloadBytes, ordered.ErrBudget
	}
	size, err := mutationMemory(mutation, limit-(baseOverhead+payloadBytes))
	if err != nil {
		return tree, payloadBytes, err
	}
	start := overlayKey{target: mutation.Target, key: mutation.Key}
	before := tree.Seek(start, true)
	key, previous, ok := before.Next()
	before.Close()
	if ok && key.target == mutation.Target && bytes.Compare(previous.mutation.EndKey, mutation.Key) > 0 {
		start = key
	}
	cursor := tree.Seek(start, false)
	defer cursor.Close()
	result := tree
	for {
		if err := ctx.Err(); err != nil {
			return tree, payloadBytes, err
		}
		key, oldEntry, ok := cursor.Next()
		if !ok || key.target != mutation.Target || bytes.Compare(oldEntry.mutation.Key, mutation.EndKey) >= 0 {
			break
		}
		old := oldEntry.mutation
		if bytes.Compare(old.EndKey, mutation.Key) <= 0 {
			continue
		}
		if baseOverhead+payloadBytes > limit {
			return tree, payloadBytes, ordered.ErrBudget
		}
		result, _, err = result.Delete(key, limit-(baseOverhead+payloadBytes))
		if err != nil {
			return tree, payloadBytes, err
		}
		payloadBytes -= oldEntry.memBytes
		if bytes.Compare(old.Key, mutation.Key) < 0 {
			left := old
			left.EndKey = mutation.Key
			left.ChecksumCRC = 0
			leftMem, err := mutationMemory(left, limit-(baseOverhead+payloadBytes))
			if err != nil {
				return tree, payloadBytes, err
			}
			payloadBytes += leftMem
			result, _, err = result.Set(key, overlayRangeEntry{mutation: left, memBytes: leftMem}, limit-(baseOverhead+payloadBytes))
			if err != nil {
				return tree, payloadBytes, err
			}
		}
		if bytes.Compare(old.EndKey, mutation.EndKey) > 0 {
			right := old
			right.Key = mutation.EndKey
			right.ChecksumCRC = 0
			rightMem, err := mutationMemory(right, limit-(baseOverhead+payloadBytes))
			if err != nil {
				return tree, payloadBytes, err
			}
			payloadBytes += rightMem
			result, _, err = result.Set(overlayKey{target: mutation.Target, key: right.Key}, overlayRangeEntry{mutation: right, memBytes: rightMem}, limit-(baseOverhead+payloadBytes))
			if err != nil {
				return tree, payloadBytes, err
			}
		}
	}
	if size > limit-(baseOverhead+payloadBytes) {
		return tree, payloadBytes, ordered.ErrBudget
	}
	payloadBytes += size
	result, _, err = result.Set(overlayKey{target: mutation.Target, key: mutation.Key}, overlayRangeEntry{mutation: mutation, memBytes: size}, limit-(baseOverhead+payloadBytes))
	if err != nil {
		return tree, payloadBytes, err
	}
	if baseOverhead+result.NodeBytes()+payloadBytes > limit {
		return tree, payloadBytes, ordered.ErrBudget
	}
	return result, payloadBytes, nil
}

func (o *Overlay) Freeze(ctx context.Context) (storage.OverlayView, error) {
	view, _, err := o.FreezeVersion(ctx)
	return view, err
}

// CatalogVersion reports whether this overlay has ever staged a change to the
// catalog collections. Every mutation a transaction stages passes through
// StageOwned, so a false result proves the transaction never rewrote catalog
// rows and its commit cannot change any snapshot-derived catalog result. The
// value is monotonic for the overlay's life: a savepoint restore that discards
// catalog changes still reports true, which over-invalidates a cache rather
// than risking a stale reuse.
func (o *Overlay) CatalogVersion() uint64 {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.catalogVersion
}

// FreezeVersion returns a frozen view together with the content version of its
// catalog contribution. Both are captured under the overlay's lock, so a caller
// that caches catalog metadata derived from the view can name exactly the
// catalog rows the view exposes.
func (o *Overlay) FreezeVersion(ctx context.Context) (storage.OverlayView, uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, 0, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	view := &frozenOverlay{state: o.state, limit: o.limit, log: o.log}
	if view.log != nil {
		view.log.retainPin()
		view.pinned = true
	}
	return view, o.catalogVersion, nil
}

func (o *Overlay) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.state = overlayState{}
	o.payloadBytes, o.inlineBytes = 0, 0
	o.log = nil
	return nil
}

type frozenOverlay struct {
	mu     sync.RWMutex
	state  overlayState
	limit  uint64
	log    *mutationLog
	pinned bool
	closed bool
}

func (v *frozenOverlay) Retain() (storage.OverlayView, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, storage.ErrClosed
	}
	retained := &frozenOverlay{state: v.state, limit: v.limit, log: v.log}
	if retained.log != nil {
		retained.log.retainPin()
		retained.pinned = true
	}
	return retained, nil
}

func (v *frozenOverlay) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	v.state = overlayState{}
	if v.pinned && v.log != nil {
		v.log.releasePin()
		v.pinned = false
	}
	v.log = nil
	return nil
}

func materializeOverlayEntry(ctx context.Context, log *mutationLog, entry overlayEntry) (storage.Mutation, error) {
	if entry.inline != nil {
		return *entry.inline, nil
	}
	if log == nil {
		return storage.Mutation{}, storage.ErrCorruptStorage
	}
	return log.readFrame(ctx, entry.ref)
}

func (v *frozenOverlay) Lookup(ctx context.Context, target storage.ObjectRef, key []byte) (storage.Mutation, bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return storage.Mutation{}, false, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.Mutation{}, false, err
	}
	if err := target.Validate(); err != nil {
		return storage.Mutation{}, false, err
	}
	lookup := overlayKey{target: target, key: key}
	point, found := v.state.points.Get(lookup)
	cursor := v.state.ranges.Seek(lookup, true)
	defer cursor.Close()
	rangeKey, rangeEntry, ok := cursor.Next()
	if found && point.ordinal >= rangeEntry.mutation.Ordinal {
		mutation, err := materializeOverlayEntry(ctx, v.log, point)
		if err != nil {
			return storage.Mutation{}, false, err
		}
		return mutation, true, nil
	}
	if ok && rangeKey.target == target && bytes.Compare(key, rangeEntry.mutation.EndKey) < 0 {
		return rangeEntry.mutation, true, nil
	}
	if found {
		mutation, err := materializeOverlayEntry(ctx, v.log, point)
		if err != nil {
			return storage.Mutation{}, false, err
		}
		return mutation, true, nil
	}
	return storage.Mutation{}, false, nil
}

func mutationMemory(m storage.Mutation, limit uint64) (uint64, error) {
	counter := overlayMemory{limit: limit}
	if err := counter.add(uint64(cap(m.Key))+uint64(cap(m.EndKey)), 1); err != nil {
		return 0, err
	}
	if err := counter.add(uint64(cap(m.ColumnIDs)), 4); err != nil {
		return 0, err
	}
	if err := counter.values(m.Values, 0); err != nil {
		return 0, err
	}
	return counter.used, nil
}

type overlayMemory struct{ limit, used uint64 }

func (m *overlayMemory) add(count, size uint64) error {
	if size != 0 && count > (m.limit-m.used)/size {
		return ordered.ErrBudget
	}
	m.used += count * size
	return nil
}
func (m *overlayMemory) values(values []model.Value, depth int) error {
	if cap(values) == 0 {
		return nil
	}
	if depth > 64 {
		return storage.ErrInvalidEncoding
	}
	if err := m.add(uint64(cap(values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return err
	}
	for _, value := range values[:cap(values)] {
		if err := m.add(uint64(len(value.StrVal))+uint64(cap(value.BytesVal)), 1); err != nil {
			return err
		}
		if err := m.add(uint64(cap(value.VectorVal)), 4); err != nil {
			return err
		}
		if err := m.add(uint64(cap(value.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{}))); err != nil {
			return err
		}
		if err := m.values(value.ArrayVal, depth+1); err != nil {
			return err
		}
	}
	return nil
}
