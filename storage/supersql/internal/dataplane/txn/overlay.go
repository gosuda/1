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

type overlayTree = ordered.Tree[overlayKey, storage.Mutation]
type overlayCursor = ordered.Cursor[overlayKey, storage.Mutation]

type overlayState struct{ points, ranges overlayTree }

// Overlay owns a bounded normalized write set. Frozen views share immutable tree
// nodes. Ranges are disjoint ordinal-labelled segments, so lookup needs only the
// predecessor interval rather than a scan of every staged range tombstone.
type Overlay struct {
	mu                       sync.Mutex
	state                    overlayState
	limit, used, lastOrdinal uint64
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
	return &Overlay{limit: maxBytes, used: overhead}, nil
}

// StageOwned atomically stages a statement. On success callers relinquish mutable
// aliases to keys, column IDs and values; failed statements retain caller ownership.
// Retained slice capacities, including values beyond the visible length, count
// against the limit: ownership transfer keeps those backing arrays alive.
func (o *Overlay) StageOwned(ctx context.Context, mutations []storage.Mutation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return storage.ErrClosed
	}
	next, used, last := o.state, o.used, o.lastOrdinal
	catalog := o.catalogVersion
	for _, mutation := range mutations {
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
		size, err := mutationMemory(mutation, o.limit-used)
		if err != nil {
			return err
		}
		used += size
		if mutation.Op == storage.MutationDeleteRange {
			next.ranges, used, err = stageOverlayRange(ctx, next.ranges, mutation, used, o.limit)
		} else {
			var allocated uint64
			next.points, allocated, err = next.points.Set(overlayKey{target: mutation.Target, key: mutation.Key}, mutation, o.limit-used)
			used += allocated
		}
		if err != nil {
			return err
		}
		last = mutation.Ordinal
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o.state, o.used, o.lastOrdinal, o.catalogVersion = next, used, last, catalog
	return nil
}

func stageOverlayRange(ctx context.Context, tree overlayTree, mutation storage.Mutation, used, limit uint64) (overlayTree, uint64, error) {
	start := overlayKey{target: mutation.Target, key: mutation.Key}
	before := tree.Seek(start, true)
	key, previous, ok := before.Next()
	before.Close()
	if ok && key.target == mutation.Target && bytes.Compare(previous.EndKey, mutation.Key) > 0 {
		start = key
	}
	cursor := tree.Seek(start, false)
	defer cursor.Close()
	result := tree
	for {
		if err := ctx.Err(); err != nil {
			return tree, used, err
		}
		key, old, ok := cursor.Next()
		if !ok || key.target != mutation.Target || bytes.Compare(old.Key, mutation.EndKey) >= 0 {
			break
		}
		if bytes.Compare(old.EndKey, mutation.Key) <= 0 {
			continue
		}
		var allocated uint64
		var err error
		result, allocated, err = result.Delete(key, limit-used)
		used += allocated
		if err != nil {
			return tree, used, err
		}
		if bytes.Compare(old.Key, mutation.Key) < 0 {
			left := old
			left.EndKey = mutation.Key
			left.ChecksumCRC = 0
			result, allocated, err = result.Set(key, left, limit-used)
			used += allocated
			if err != nil {
				return tree, used, err
			}
		}
		if bytes.Compare(old.EndKey, mutation.EndKey) > 0 {
			right := old
			right.Key = mutation.EndKey
			right.ChecksumCRC = 0
			result, allocated, err = result.Set(overlayKey{target: mutation.Target, key: right.Key}, right, limit-used)
			used += allocated
			if err != nil {
				return tree, used, err
			}
		}
	}
	result, allocated, err := result.Set(overlayKey{target: mutation.Target, key: mutation.Key}, mutation, limit-used)
	return result, used + allocated, err
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
	return &frozenOverlay{state: o.state, limit: o.limit}, o.catalogVersion, nil
}

func (o *Overlay) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
	o.state = overlayState{}
	return nil
}

type frozenOverlay struct {
	mu     sync.RWMutex
	state  overlayState
	limit  uint64
	closed bool
}

func (v *frozenOverlay) Retain() (storage.OverlayView, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, storage.ErrClosed
	}
	return &frozenOverlay{state: v.state, limit: v.limit}, nil
}

func (v *frozenOverlay) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	v.state = overlayState{}
	return nil
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
	rangeKey, tombstone, ok := cursor.Next()
	if found && point.Ordinal >= tombstone.Ordinal {
		return point, true, nil
	}
	if ok && rangeKey.target == target && bytes.Compare(key, tombstone.EndKey) < 0 {
		return tombstone, true, nil
	}
	return point, found, nil
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
