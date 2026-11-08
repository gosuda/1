package distinctset

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	runHeaderBytes = 32
	runMagic       = uint64(0x3154455354534944) // DISTSET1
)

var errCorrupt = &model.Error{Code: "XX001", Message: "corrupt recursive distinct run"}

// Each variable-length record has a 32-byte header: ordinal:u64, offset:u64,
// keyBytes:u64, keyCRC:u32, headerCRC:u32 (little endian, IEEE CRC32).
// A dense disk index repeats those headers in key order. A checked footer holds
// magic:u64, count:u64, indexOffset:u64, reserved:u32, CRC:u32. No resident
// per-key disk index is needed; lookups read logarithmically many records.
type run struct {
	file  *runtime.Spill
	count uint64
	bytes uint64
}

func (r run) close() error {
	if r.file == nil {
		return nil
	}
	return r.file.Close()
}

func (s *Set) newRun(ctx context.Context) (run, error) {
	if err := s.check(ctx); err != nil {
		return run{}, err
	}
	file, err := s.query.NewSpill(ctx)
	return run{file: file}, err
}

func (s *Set) appendKey(ctx context.Context, r *run, key []byte) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if uint64(len(key)) > s.maxKey || r.bytes > math.MaxInt64-runHeaderBytes-uint64(len(key)) {
		return resource("recursive distinct run exceeds extent limit")
	}
	var header [runHeaderBytes]byte
	binary.LittleEndian.PutUint64(header[0:8], r.count)
	binary.LittleEndian.PutUint64(header[8:16], r.bytes)
	binary.LittleEndian.PutUint64(header[16:24], uint64(len(key)))
	binary.LittleEndian.PutUint32(header[24:28], crc32.ChecksumIEEE(key))
	binary.LittleEndian.PutUint32(header[28:32], crc32.ChecksumIEEE(header[:28]))
	if _, err := r.file.Append(ctx, header[:]); err != nil {
		return err
	}
	if _, err := r.file.Append(ctx, key); err != nil {
		return err
	}
	r.count++
	r.bytes += runHeaderBytes + uint64(len(key))
	return nil
}

func readFull(ctx context.Context, file *runtime.Spill, data []byte, offset uint64) error {
	if offset > math.MaxInt64 {
		return errCorrupt
	}
	n, err := file.ReadAt(ctx, data, int64(offset))
	truncated := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	shortRead := err == nil && n != len(data)
	if truncated || shortRead {
		return errors.Join(errCorrupt, err)
	}
	return err
}

func (s *Set) header(header []byte, r run, ordinal uint64) (offset, length uint64, err error) {
	if binary.LittleEndian.Uint32(header[28:32]) != crc32.ChecksumIEEE(header[:28]) || binary.LittleEndian.Uint64(header[:8]) != ordinal {
		return 0, 0, errCorrupt
	}
	offset = binary.LittleEndian.Uint64(header[8:16])
	length = binary.LittleEndian.Uint64(header[16:24])
	if length > s.maxKey || offset > r.bytes || r.bytes-offset < runHeaderBytes || length > r.bytes-offset-runHeaderBytes {
		return 0, 0, errCorrupt
	}
	return offset, length, nil
}

func (s *Set) seal(ctx context.Context, r *run) error {
	if r.count == 0 || r.bytes > math.MaxInt64-runHeaderBytes || r.count > (math.MaxInt64-r.bytes-runHeaderBytes)/runHeaderBytes {
		return resource("recursive distinct index exceeds extent limit")
	}
	var header [runHeaderBytes]byte
	var offset uint64
	for ordinal := uint64(0); ordinal < r.count; ordinal++ {
		if err := s.check(ctx); err != nil {
			return err
		}
		if err := readFull(ctx, r.file, header[:], offset); err != nil {
			return err
		}
		recordOffset, length, err := s.header(header[:], *r, ordinal)
		if err != nil {
			return err
		}
		if recordOffset != offset {
			return errCorrupt
		}
		if _, err := r.file.Append(ctx, header[:]); err != nil {
			return err
		}
		offset += runHeaderBytes + length
	}
	if offset != r.bytes {
		return errCorrupt
	}
	var footer [runHeaderBytes]byte
	binary.LittleEndian.PutUint64(footer[0:8], runMagic)
	binary.LittleEndian.PutUint64(footer[8:16], r.count)
	binary.LittleEndian.PutUint64(footer[16:24], r.bytes)
	binary.LittleEndian.PutUint32(footer[28:32], crc32.ChecksumIEEE(footer[:28]))
	_, err := r.file.Append(ctx, footer[:])
	return err
}

func (s *Set) validateRun(ctx context.Context, r run) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	var footer [runHeaderBytes]byte
	if err := readFull(ctx, r.file, footer[:], r.bytes+r.count*runHeaderBytes); err != nil {
		return err
	}
	if binary.LittleEndian.Uint64(footer[0:8]) != runMagic || binary.LittleEndian.Uint64(footer[8:16]) != r.count || binary.LittleEndian.Uint64(footer[16:24]) != r.bytes || binary.LittleEndian.Uint32(footer[24:28]) != 0 || binary.LittleEndian.Uint32(footer[28:32]) != crc32.ChecksumIEEE(footer[:28]) {
		return errCorrupt
	}
	return nil
}

func (s *Set) readKey(ctx context.Context, r run, ordinal uint64, buffer []byte) ([]byte, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if ordinal >= r.count {
		return nil, errCorrupt
	}
	var index, header [runHeaderBytes]byte
	if err := readFull(ctx, r.file, index[:], r.bytes+ordinal*runHeaderBytes); err != nil {
		return nil, err
	}
	offset, length, err := s.header(index[:], r, ordinal)
	if err != nil {
		return nil, err
	}
	if err := readFull(ctx, r.file, header[:], offset); err != nil {
		return nil, err
	}
	if header != index {
		return nil, errCorrupt
	}
	key := buffer[:int(length)]
	if err := readFull(ctx, r.file, key, offset+runHeaderBytes); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(key) != binary.LittleEndian.Uint32(header[24:28]) {
		return nil, errCorrupt
	}
	return key, nil
}

func (s *Set) contains(ctx context.Context, r run, key []byte) (bool, error) {
	if err := s.validateRun(ctx, r); err != nil {
		return false, err
	}
	lo, hi := uint64(0), r.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		candidate, err := s.readKey(ctx, r, mid, s.left)
		if err != nil {
			return false, err
		}
		switch bytes.Compare(candidate, key) {
		case -1:
			lo = mid + 1
		case 0:
			return true, nil
		default:
			hi = mid
		}
	}
	return false, nil
}

func (s *Set) merge(ctx context.Context, a, b run) (result run, err error) {
	if err := s.validateRun(ctx, a); err != nil {
		return run{}, err
	}
	if err := s.validateRun(ctx, b); err != nil {
		return run{}, err
	}
	result, err = s.newRun(ctx)
	if err != nil {
		return run{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, result.close())
			result = run{}
		}
	}()
	var ai, bi uint64
	var left, right []byte
	var haveLeft, haveRight bool
	for ai < a.count || bi < b.count {
		if !haveLeft && ai < a.count {
			left, err = s.readKey(ctx, a, ai, s.left)
			if err != nil {
				return result, err
			}
			haveLeft = true
		}
		if !haveRight && bi < b.count {
			right, err = s.readKey(ctx, b, bi, s.right)
			if err != nil {
				return result, err
			}
			haveRight = true
		}
		key := right
		if haveLeft && (!haveRight || bytes.Compare(left, right) < 0) {
			key = left
			ai++
			haveLeft = false
		} else {
			// Insert checks all live runs before admission, so equality indicates
			// a broken run invariant rather than another row to silently drop.
			if haveLeft && bytes.Equal(left, right) {
				return result, errCorrupt
			}
			bi++
			haveRight = false
		}
		if err := s.appendKey(ctx, &result, key); err != nil {
			return result, err
		}
	}
	if err := s.seal(ctx, &result); err != nil {
		return result, err
	}
	return result, nil
}
