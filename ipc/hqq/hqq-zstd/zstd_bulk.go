package zstd

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"sync"
)

type bulkState struct {
	mu    sync.Mutex
	cl    *Client
	cctx  CCtx
	dctx  DCtx
	cdict CDict
	ddict DDict
}

func (s *bulkState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		_ = s.cl.Close()
		s.cl = nil
	}
}

// BulkProcessor implements the pre-digested dictionary API matching
// github.com/DataDog/zstd. When compressing or decompressing multiple payloads
// with the same dictionary, NewBulkProcessor digests the dictionary once and
// reuses the digested state across calls. BulkProcessor is safe for concurrent
// use by multiple goroutines.
type BulkProcessor struct {
	dict  []byte
	level int
	state *bulkState
}

// NewBulkProcessor creates a new BulkProcessor with a pre-trained dictionary
// and compression level.
func NewBulkProcessor(dictionary []byte, compressionLevel int) (*BulkProcessor, error) {
	if len(dictionary) == 0 {
		return nil, ErrEmptyDictionary
	}
	cl, err := Open(context.Background())
	if err != nil {
		return nil, err
	}
	cctx, err := cl.CreateCCtx()
	if err != nil {
		_ = cl.Close()
		return nil, err
	}
	dctx, err := cl.CreateDCtx()
	if err != nil {
		_ = cl.Close()
		return nil, err
	}
	cdict, err := cl.CreateCDict(dictionary, int32(compressionLevel))
	if err != nil {
		_ = cl.Close()
		return nil, ErrBadDictionary
	}
	ddict, err := cl.CreateDDict(dictionary)
	if err != nil {
		_ = cl.Close()
		return nil, ErrBadDictionary
	}
	st := &bulkState{
		cl:    cl,
		cctx:  cctx,
		dctx:  dctx,
		cdict: cdict,
		ddict: ddict,
	}
	p := &BulkProcessor{
		dict:  bytes.Clone(dictionary),
		level: compressionLevel,
		state: st,
	}
	runtime.AddCleanup(p, func(s *bulkState) {
		s.close()
	}, st)
	return p, nil
}

// Compress compresses src into dst with the dictionary given when creating the
// BulkProcessor. If you have a buffer to use, you can pass it to prevent
// allocation. If it is too small, or if nil is passed, a new buffer will be
// allocated and returned.
func (p *BulkProcessor) Compress(dst, src []byte) ([]byte, error) {
	buf, bound, err := prepareCompressDst(dst, src)
	if err != nil {
		return nil, err
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	out, err := p.state.cl.CompressUsingCDict(p.state.cctx, buf, src, p.state.cdict)
	if err != nil && IsDstSizeTooSmallError(err) && len(buf) < bound {
		buf = make([]byte, bound)
		out, err = p.state.cl.CompressUsingCDict(p.state.cctx, buf, src, p.state.cdict)
	}
	return out, err
}

// Decompress decompresses src into dst with the dictionary given when creating
// the BulkProcessor. If you have a buffer to use, you can pass it to prevent
// allocation. If it is too small, or if nil is passed, a new buffer will be
// allocated and returned.
func (p *BulkProcessor) Decompress(dst, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, ErrEmptySlice
	}
	const maxOneShotDst = 64 << 20

	p.state.mu.Lock()
	doOne := func(buf []byte) ([]byte, error) {
		return p.state.cl.DecompressUsingDDict(p.state.dctx, buf, src, p.state.ddict)
	}

	if len(dst) > 0 {
		maxCap := len(dst)
		if maxCap > maxOneShotDst {
			maxCap = maxOneShotDst
		}
		out, err := doOne(dst[:maxCap])
		if err == nil {
			p.state.mu.Unlock()
			return out, nil
		}
		if !IsDstSizeTooSmallError(err) {
			p.state.mu.Unlock()
			return nil, err
		}
	}

	hdr := src
	if len(hdr) > int(ZSTD_FRAMEHEADER_MAX) {
		hdr = hdr[:ZSTD_FRAMEHEADER_MAX]
	}
	if sz, err := p.state.cl.GetFrameContentSize(hdr); err == nil &&
		sz != ZSTD_CONTENTSIZE_UNKNOWN &&
		sz != ZSTD_CONTENTSIZE_ERROR &&
		sz <= maxOneShotDst {
		buf := make([]byte, int(sz))
		out, err := doOne(buf)
		if err == nil {
			p.state.mu.Unlock()
			return out, nil
		}
		if !IsDstSizeTooSmallError(err) {
			p.state.mu.Unlock()
			return nil, err
		}
	}

	nextSize := len(src) * 4
	if nextSize < 64 {
		nextSize = 64
	}
	for i := 0; i < 3; i++ {
		if nextSize > maxOneShotDst {
			break
		}
		buf := make([]byte, nextSize)
		out, err := doOne(buf)
		if err == nil {
			p.state.mu.Unlock()
			return out, nil
		}
		if !IsDstSizeTooSmallError(err) {
			p.state.mu.Unlock()
			return nil, err
		}
		nextSize *= 2
	}
	p.state.mu.Unlock()

	r := NewReaderDict(bytes.NewReader(src), p.dict)
	out, rerr := io.ReadAll(r)
	cerr := r.Close()
	if rerr != nil {
		return nil, rerr
	}
	return out, cerr
}
