// Package distinctset implements SQL not-distinct membership with indexed spill runs.
package distinctset

import (
	"bytes"
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Limits are positive except MemoryBytes, which may be zero for direct spill.
// MemoryBytes includes resident key bytes and their compact index. WorkBytes
// includes that memory, encoder scratch, merge buffers and run metadata.
// MaxLevels bounds binary merge levels; at most MaxLevels+2 files coexist.
// Query and Encoder are borrowed through Close; Layouts is copied into key types.
type Options struct {
	Query       *runtime.Query
	Layouts     []model.TypeLayout
	Encoder     storage.KeyEncoder
	MemoryBytes uint64
	WorkBytes   uint64
	MaxKeyBytes uint64
	MaxKeys     uint64
	MaxLevels   int
}

type entry struct {
	offset uint64
	length uint64
}

// Set calls must be serialized, including Close. Insert and Contains errors
// fence the set and release its resources; failed operations cannot be retried.
type Set struct {
	query    *runtime.Query
	encoder  storage.KeyEncoder
	ctx      context.Context
	layout   []storage.KeyColumn
	claim    columnar.Allocation
	maxKey   uint64
	maxKeys  uint64
	count    uint64
	arena    []byte
	entries  []entry
	levels   []run
	left     []byte
	right    []byte
	failed   error
	closed   bool
	closeErr error
}

func resource(message string) error {
	return &model.Error{Code: "53200", Message: message, Cause: runtime.ErrBudget}
}

func New(ctx context.Context, options Options) (*Set, error) {
	missing := options.Query == nil || options.Encoder == nil
	invalidBytes := options.WorkBytes == 0 || options.MaxKeyBytes == 0
	invalidIndex := options.MaxKeys == 0 || options.MaxLevels < 1
	if missing || invalidBytes || invalidIndex {
		return nil, runtime.ErrInvalidConfig
	}
	for _, size := range []uint64{options.MemoryBytes, options.WorkBytes, options.MaxKeyBytes, options.MaxKeys, uint64(options.MaxLevels), uint64(len(options.Layouts))} {
		if size > uint64(math.MaxInt)/1024 {
			return nil, runtime.ErrInvalidConfig
		}
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(options.Query.Context())); err != nil {
		return nil, err
	}
	// Encoder numeric preparation, its owned result, and two merge heads coexist.
	// Per-column allowance covers the encoder's numeric descriptors and growth.
	work := uint64(unsafe.Sizeof(Set{})) + 4096 + options.MemoryBytes + options.MaxKeyBytes*4 + uint64(options.MaxLevels+2)*(uint64(unsafe.Sizeof(run{}))+512) + uint64(len(options.Layouts))*(uint64(unsafe.Sizeof(storage.KeyColumn{}))+256)
	// The shared numeric codec admits up to 32768 digits before key-size
	// checking, including exponent expansion of short input strings.
	for _, layout := range options.Layouts {
		if layout.Type.Storage == model.TypeNumeric {
			work += 1 << 20
			break
		}
	}
	if work > options.WorkBytes {
		return nil, resource("admit recursive distinct workspace")
	}
	var keyBytes uint64
	for _, layout := range options.Layouts {
		key := [1]storage.KeyColumn{{Type: layout.Type, Children: layout.Children}}
		used, err := storage.KeyLayoutBytes(ctx, key[:], options.WorkBytes-work)
		if err != nil {
			return nil, err
		}
		work += used
		keyBytes += used
	}
	claim, err := options.Query.Allocator(runtime.WorkMemory).Reserve(work)
	if err != nil {
		return nil, err
	}
	s := &Set{query: options.Query, encoder: options.Encoder, ctx: ctx, claim: claim, maxKey: options.MaxKeyBytes, maxKeys: options.MaxKeys}
	s.layout = make([]storage.KeyColumn, len(options.Layouts))
	for i, layout := range options.Layouts {
		s.layout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children, NullsFirst: true}
	}
	s.layout, _, err = storage.CopyKeyLayout(ctx, s.layout, keyBytes)
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if _, err := s.encoder.EncodePrefix(ctx, s.layout, nil, s.maxKey); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	entryCapacity := options.MemoryBytes / (2 * uint64(unsafe.Sizeof(entry{})))
	arenaCapacity := options.MemoryBytes - entryCapacity*uint64(unsafe.Sizeof(entry{}))
	s.entries = make([]entry, 0, int(entryCapacity))
	s.arena = make([]byte, 0, int(arenaCapacity))
	s.levels = make([]run, options.MaxLevels)
	s.left = make([]byte, int(s.maxKey))
	s.right = make([]byte, int(s.maxKey))
	return s, nil
}

func (s *Set) check(ctx context.Context) error {
	if s.failed != nil {
		return s.failed
	}
	if s.closed {
		return runtime.ErrClosed
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	return context.Cause(s.query.Context())
}

func (s *Set) key(e entry) []byte {
	return s.arena[e.offset : e.offset+e.length]
}

func (s *Set) residentPosition(key []byte) (int, bool) {
	lo, hi := 0, len(s.entries)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(s.key(s.entries[mid]), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(s.entries) && bytes.Equal(s.key(s.entries[lo]), key)
}

// Insert borrows row only during this call. True means the complete typed row
// was first admitted; NULLs, NaNs and numeric scales follow Encoder equality.
func (s *Set) Insert(ctx context.Context, row []model.Value) (inserted bool, err error) {
	defer func() { err = s.finishOperation(err) }()
	key, position, found, err := s.lookup(ctx, row)
	if err != nil || found {
		return false, err
	}
	if s.count == s.maxKeys {
		return false, resource("recursive distinct key count exceeds limit")
	}
	if err := s.check(ctx); err != nil {
		return false, err
	}
	if len(s.entries) == cap(s.entries) || len(key) > cap(s.arena)-len(s.arena) {
		if err := s.flush(ctx); err != nil {
			return false, err
		}
		position = 0
	}
	if cap(s.entries) == 0 || len(key) > cap(s.arena) {
		r, err := s.newRun(ctx)
		if err != nil {
			return false, err
		}
		if err := s.appendKey(ctx, &r, key); err != nil {
			return false, errors.Join(err, r.close())
		}
		if err := s.seal(ctx, &r); err != nil {
			return false, errors.Join(err, r.close())
		}
		if err := s.install(ctx, r); err != nil {
			return false, err
		}
	} else {
		e := entry{offset: uint64(len(s.arena)), length: uint64(len(key))}
		s.arena = append(s.arena, key...)
		s.entries = append(s.entries, entry{})
		copy(s.entries[position+1:], s.entries[position:len(s.entries)-1])
		s.entries[position] = e
	}
	s.count++
	return true, nil
}

// Contains borrows row and never admits a new key. Like Insert, a failed
// operation fences the set and joins resource cleanup.
func (s *Set) Contains(ctx context.Context, row []model.Value) (found bool, err error) {
	defer func() { err = s.finishOperation(err) }()
	_, _, found, err = s.lookup(ctx, row)
	return found, err
}

func (s *Set) finishOperation(err error) error {
	if err != nil && !s.closed {
		err = errors.Join(err, s.Close())
		s.failed = err
	}
	return err
}

func (s *Set) lookup(ctx context.Context, row []model.Value) ([]byte, int, bool, error) {
	if err := s.check(ctx); err != nil {
		return nil, 0, false, err
	}
	if len(row) != len(s.layout) {
		return nil, 0, false, columnar.ErrValueLayout
	}
	key, err := s.encoder.EncodePrefix(ctx, s.layout, row, s.maxKey)
	if err != nil {
		return nil, 0, false, err
	}
	if uint64(len(key)) > s.maxKey || uint64(cap(key)) > s.maxKey {
		return nil, 0, false, resource("recursive distinct key exceeds limit")
	}
	if err := s.check(ctx); err != nil {
		return nil, 0, false, err
	}
	position, found := s.residentPosition(key)
	if found {
		return key, position, true, nil
	}
	for i := range s.levels {
		if s.levels[i].file == nil {
			continue
		}
		found, err := s.contains(ctx, s.levels[i], key)
		if err != nil || found {
			return key, position, found, err
		}
	}
	return key, position, false, nil
}

func (s *Set) flush(ctx context.Context) error {
	if len(s.entries) == 0 {
		return nil
	}
	r, err := s.newRun(ctx)
	if err != nil {
		return err
	}
	for _, e := range s.entries {
		if err := s.appendKey(ctx, &r, s.key(e)); err != nil {
			return errors.Join(err, r.close())
		}
	}
	if err := s.seal(ctx, &r); err != nil {
		return errors.Join(err, r.close())
	}
	if err := s.install(ctx, r); err != nil {
		return err
	}
	s.entries = s.entries[:0]
	s.arena = s.arena[:0]
	return nil
}

func (s *Set) install(ctx context.Context, carry run) error {
	for i := range s.levels {
		if err := s.check(ctx); err != nil {
			return errors.Join(err, carry.close())
		}
		if s.levels[i].file == nil {
			s.levels[i] = carry
			return nil
		}
		merged, err := s.merge(ctx, s.levels[i], carry)
		if err != nil {
			return errors.Join(err, carry.close())
		}
		err = errors.Join(s.levels[i].close(), carry.close())
		s.levels[i] = run{}
		if err != nil {
			return errors.Join(err, merged.close())
		}
		carry = merged
	}
	return errors.Join(resource("recursive distinct merge levels exhausted"), carry.close())
}

func (s *Set) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	for i := range s.levels {
		s.closeErr = errors.Join(s.closeErr, s.levels[i].close())
	}
	s.layout, s.arena, s.entries, s.levels, s.left, s.right = nil, nil, nil, nil, nil, nil
	if s.claim != nil {
		s.claim.Release()
		s.claim = nil
	}
	return s.closeErr
}
