package neosepia

import (
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	blockCacheShards       = 16
	defaultBlockCacheBytes = uint64(32 << 20)
)

type blockCacheKey struct {
	fileID     uint64
	blockIndex int
}

// cachedPAXBlock holds an immutable validated PAX block plus a concurrency-safe
// cache of its decoded non-LOB column vectors so concurrent readers can share
// the block without repeating pread, CRC32, visibility validation, or decoding.
type cachedPAXBlock struct {
	data        []byte
	target      storage.ObjectRef
	rows        int
	columns     int
	descriptors []byte
	vis         []byte
	sizeBytes   uint64
	cache       *paxBlockCache
	key         blockCacheKey

	colMu sync.RWMutex
	cols  []cachedPAXColumn
}

func (c *cachedPAXBlock) getColumn(id model.ColumnID) (columnar.Vector, bool) {
	c.colMu.RLock()
	for i := range c.cols {
		if c.cols[i].id == id {
			v := c.cols[i].vector
			c.colMu.RUnlock()
			return v, true
		}
	}
	c.colMu.RUnlock()
	return columnar.Vector{}, false
}

// estimateCachedVectorBytes computes the retained heap bytes of a decoded
// columnar.Vector, including struct overhead, validity bitmap, dictionary
// indices, scalar/variable-width buffers, and nested children. Slice lengths
// (rather than capacities) are measured so borrowed views into cachedPAXBlock.data
// do not overcount the enclosing block's trailing capacity.
func estimateCachedVectorBytes(v *columnar.Vector) uint64 {
	if v == nil {
		return 0
	}
	bytes := uint64(unsafe.Sizeof(columnar.Vector{}))
	bytes += uint64(len(v.Nulls))
	bytes += uint64(len(v.Indices)) * uint64(unsafe.Sizeof(uint32(0)))
	bytes += uint64(len(v.Int64)) * uint64(unsafe.Sizeof(int64(0)))
	bytes += uint64(len(v.Float64)) * uint64(unsafe.Sizeof(float64(0)))
	bytes += uint64(len(v.Data))
	bytes += uint64(len(v.Ranges)) * uint64(unsafe.Sizeof(columnar.Range{}))
	bytes += uint64(len(v.Float32)) * uint64(unsafe.Sizeof(float32(0)))
	bytes += uint64(len(v.Intervals)) * uint64(unsafe.Sizeof(model.Interval{}))
	bytes += uint64(len(v.ShapeRanges)) * uint64(unsafe.Sizeof(columnar.Range{}))
	bytes += uint64(len(v.Dimensions)) * uint64(unsafe.Sizeof(model.ArrayDimension{}))
	for i := range v.Children {
		bytes += estimateCachedVectorBytes(&v.Children[i])
	}
	return bytes
}

func cachedColumnBytes(v *columnar.Vector) uint64 {
	return estimateCachedVectorBytes(v) + (uint64(unsafe.Sizeof(cachedPAXColumn{})) - uint64(unsafe.Sizeof(columnar.Vector{})))
}

type blockCacheNode struct {
	key   blockCacheKey
	val   *cachedPAXBlock
	bytes uint64
	prev  *blockCacheNode
	next  *blockCacheNode
}

type blockCacheShard struct {
	mu      sync.Mutex
	max     uint64
	used    uint64
	entries map[blockCacheKey]*blockCacheNode
	head    *blockCacheNode
	tail    *blockCacheNode
}

// paxBlockCache is a concurrency-safe, sharded, memory-bounded LRU cache for
// validated PAX blocks keyed by (fileID, blockIndex).
type paxBlockCache struct {
	shards [blockCacheShards]blockCacheShard
	hits   atomic.Uint64
	misses atomic.Uint64
}

func newPAXBlockCache(maxBytes uint64) *paxBlockCache {
	if maxBytes == 0 {
		return nil
	}
	c := &paxBlockCache{}
	perShard := maxBytes / blockCacheShards
	if perShard == 0 {
		perShard = 1
	}
	for i := range c.shards {
		c.shards[i].max = perShard
		c.shards[i].entries = make(map[blockCacheKey]*blockCacheNode)
	}
	return c
}

func (c *paxBlockCache) shard(key blockCacheKey) *blockCacheShard {
	h := key.fileID*0x9e3779b97f4a7c15 ^ uint64(key.blockIndex)*0xbf58476d1ce4e5b9
	h ^= h >> 32
	return &c.shards[h&(blockCacheShards-1)]
}

func (c *paxBlockCache) get(key blockCacheKey) *cachedPAXBlock {
	if c == nil {
		return nil
	}
	s := c.shard(key)
	s.mu.Lock()
	node, ok := s.entries[key]
	if !ok {
		s.mu.Unlock()
		c.misses.Add(1)
		return nil
	}
	s.moveToFrontLocked(node)
	val := node.val
	s.mu.Unlock()
	c.hits.Add(1)
	return val
}

func (c *paxBlockCache) put(key blockCacheKey, val *cachedPAXBlock) *cachedPAXBlock {
	if c == nil || val == nil {
		return nil
	}
	nodeOverhead := uint64(unsafe.Sizeof(blockCacheNode{}))
	if val.sizeBytes > math.MaxUint64-nodeOverhead {
		return nil
	}
	cost := val.sizeBytes + nodeOverhead
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cost > s.max {
		return nil
	}
	if existing, ok := s.entries[key]; ok {
		s.moveToFrontLocked(existing)
		return existing.val
	}
	for s.tail != nil && s.used > s.max-cost {
		victim := s.tail
		s.unlinkLocked(victim)
		delete(s.entries, victim.key)
		s.used -= victim.bytes
	}
	val.cache = c
	val.key = key
	node := &blockCacheNode{key: key, val: val, bytes: cost}
	s.entries[key] = node
	s.pushFrontLocked(node)
	s.used += cost
	return val
}

// putColumn caches a decoded columnar.Vector on block while charging its
// retained byte size against the owning shard's LRU budget. Lock order is
// strictly shard.mu -> block.colMu, preventing lock-order inversions with
// concurrent readers. Returns true if the column was cached.
func (c *paxBlockCache) putColumn(key blockCacheKey, block *cachedPAXBlock, id model.ColumnID, vec columnar.Vector) bool {
	if c == nil || block == nil {
		return false
	}
	colBytes := cachedColumnBytes(&vec)
	s := c.shard(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.entries[key]
	if !ok || node.val != block {
		return false
	}
	block.colMu.RLock()
	for i := range block.cols {
		if block.cols[i].id == id {
			block.colMu.RUnlock()
			return false
		}
	}
	block.colMu.RUnlock()
	if colBytes > s.max || node.bytes > s.max-colBytes {
		return false
	}
	s.moveToFrontLocked(node)
	for s.tail != nil && s.tail != node && s.used > s.max-colBytes {
		victim := s.tail
		s.unlinkLocked(victim)
		delete(s.entries, victim.key)
		s.used -= victim.bytes
	}
	if s.used > s.max-colBytes {
		return false
	}
	block.colMu.Lock()
	block.cols = append(block.cols, cachedPAXColumn{id: id, vector: vec})
	block.colMu.Unlock()
	node.bytes += colBytes
	s.used += colBytes
	return true
}

func (c *paxBlockCache) Hits() uint64 {
	if c == nil {
		return 0
	}
	return c.hits.Load()
}

func (c *paxBlockCache) Misses() uint64 {
	if c == nil {
		return 0
	}
	return c.misses.Load()
}

func (c *paxBlockCache) UsedBytes() uint64 {
	if c == nil {
		return 0
	}
	var total uint64
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		total += s.used
		s.mu.Unlock()
	}
	return total
}

func (c *paxBlockCache) BudgetBytes() uint64 {
	if c == nil {
		return 0
	}
	var total uint64
	for i := range c.shards {
		total += c.shards[i].max
	}
	return total
}

func (c *paxBlockCache) Clear() {
	if c == nil {
		return
	}
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		clear(s.entries)
		s.head, s.tail = nil, nil
		s.used = 0
		s.mu.Unlock()
	}
}

func (s *blockCacheShard) pushFrontLocked(n *blockCacheNode) {
	n.prev = nil
	n.next = s.head
	if s.head != nil {
		s.head.prev = n
	} else {
		s.tail = n
	}
	s.head = n
}

func (s *blockCacheShard) unlinkLocked(n *blockCacheNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else if s.head == n {
		s.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else if s.tail == n {
		s.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (s *blockCacheShard) moveToFrontLocked(n *blockCacheNode) {
	if s.head == n {
		return
	}
	s.unlinkLocked(n)
	s.pushFrontLocked(n)
}
