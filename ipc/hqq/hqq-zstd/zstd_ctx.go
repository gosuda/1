package zstd

import (
	"context"
	"runtime"
	"sync"
)

// Ctx defines an interface for reusable Zstandard compression and decompression
// contexts matching github.com/DataDog/zstd.
type Ctx interface {
	// Compress src into dst. If you have a buffer to use, you can pass it to
	// prevent allocation. If it is too small, or if nil is passed, a new buffer
	// will be allocated and returned.
	Compress(dst, src []byte) ([]byte, error)

	// CompressLevel is the same as Compress but you can pass a compression level.
	CompressLevel(dst, src []byte, level int) ([]byte, error)

	// Decompress src into dst. If you have a buffer to use, you can pass it to
	// prevent allocation. If it is too small, or if nil is passed, a new buffer
	// will be allocated and returned.
	Decompress(dst, src []byte) ([]byte, error)

	// DecompressInto decompresses src into dst. Unlike Decompress, DecompressInto
	// requires that dst be sufficiently large to hold the decompressed payload.
	// It returns the number of bytes copied and an error if any is encountered.
	// If dst is too small, DecompressInto errors.
	DecompressInto(dst, src []byte) (int, error)
}

type ctxState struct {
	mu   sync.Mutex
	cl   *Client
	cctx CCtx
	dctx DCtx
}

func (s *ctxState) ensureClient() error {
	if s.cl != nil {
		return nil
	}
	cl, err := Open(context.Background())
	if err != nil {
		return err
	}
	s.cl = cl
	return nil
}

func (s *ctxState) ensureCCtx() error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	if s.cctx != (CCtx{}) {
		return nil
	}
	h, err := s.cl.CreateCCtx()
	if err != nil {
		return err
	}
	s.cctx = h
	return nil
}

func (s *ctxState) ensureDCtx() error {
	if err := s.ensureClient(); err != nil {
		return err
	}
	if s.dctx != (DCtx{}) {
		return nil
	}
	h, err := s.cl.CreateDCtx()
	if err != nil {
		return err
	}
	s.dctx = h
	return nil
}

func (s *ctxState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cl != nil {
		_ = s.cl.Close()
		s.cl = nil
		s.cctx = CCtx{}
		s.dctx = DCtx{}
	}
}

type zstdCtx struct {
	state *ctxState
}

// NewCtx creates a new reusable Zstandard context.
//
// When compressing or decompressing many times, reusing a context avoids
// repeated context allocations across operations. Use one context per goroutine
// for parallel execution.
func NewCtx() Ctx {
	st := &ctxState{}
	c := &zstdCtx{state: st}
	runtime.AddCleanup(c, func(s *ctxState) {
		s.close()
	}, st)
	return c
}

func (c *zstdCtx) Compress(dst, src []byte) ([]byte, error) {
	return c.CompressLevel(dst, src, DefaultCompression)
}

func (c *zstdCtx) CompressLevel(dst, src []byte, level int) ([]byte, error) {
	buf, bound, err := prepareCompressDst(dst, src)
	if err != nil {
		return nil, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if err := c.state.ensureCCtx(); err != nil {
		return nil, err
	}
	out, err := c.state.cl.CompressCCtx(c.state.cctx, buf, src, int32(level))
	if err != nil && IsDstSizeTooSmallError(err) && len(buf) < bound {
		buf = make([]byte, bound)
		out, err = c.state.cl.CompressCCtx(c.state.cctx, buf, src, int32(level))
	}
	return out, err
}

func (c *zstdCtx) Decompress(dst, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, ErrEmptySlice
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if err := c.state.ensureDCtx(); err != nil {
		return nil, err
	}
	out, _, err := decompressWith(c.state.cl, &c.state.dctx, dst, src)
	return out, err
}

func (c *zstdCtx) DecompressInto(dst, src []byte) (int, error) {
	if len(src) == 0 {
		return 0, ErrEmptySlice
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if err := c.state.ensureDCtx(); err != nil {
		return 0, err
	}
	out, err := c.state.cl.DecompressDCtx(c.state.dctx, dst, src)
	if err != nil {
		return 0, err
	}
	return len(out), nil
}
