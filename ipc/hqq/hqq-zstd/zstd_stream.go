package zstd

import (
	"context"
	"errors"
	"io"
)

const (
	streamInChunkSize  = 64 << 10
	streamOutChunkSize = 64 << 10
)

// Writer is an io.WriteCloser that zstd-compresses its input.
type Writer struct {
	CompressionLevel int

	w      io.Writer
	dict   []byte
	cl     *Client
	cctx   CCtx
	outBuf []byte
	closed bool
	err    error
}

// NewWriter creates a new Writer with default compression options. Writes to
// the writer will be written in compressed form to w.
func NewWriter(w io.Writer) *Writer {
	return NewWriterLevelDict(w, DefaultCompression, nil)
}

// NewWriterLevel is like NewWriter but specifies the compression level instead
// of assuming default compression.
func NewWriterLevel(w io.Writer, level int) *Writer {
	return NewWriterLevelDict(w, level, nil)
}

// NewWriterLevelDict is like NewWriterLevel but specifies a dictionary to
// compress with. If the dictionary is empty or nil it is ignored. The
// dictionary should not be modified until the writer is closed.
func NewWriterLevelDict(w io.Writer, level int, dict []byte) *Writer {
	return &Writer{
		CompressionLevel: level,
		w:                w,
		dict:             dict,
	}
}

func (w *Writer) init() error {
	if w.err != nil {
		return w.err
	}
	if w.cl != nil {
		return nil
	}
	cl, err := sharedPool.Get(context.Background())
	if err != nil {
		w.err = err
		return err
	}
	cctx, err := cl.CreateCCtx()
	if err != nil {
		releasePooledClient(cl, err)
		w.err = err
		return err
	}
	if _, err := cl.CCtxSetParameter(cctx, ZSTD_c_compressionLevel, int32(w.CompressionLevel)); err != nil {
		_ = cl.FreeCCtx(cctx)
		releasePooledClient(cl, err)
		w.err = err
		return err
	}
	if len(w.dict) > 0 {
		if _, err := cl.CCtxLoadDictionary(cctx, w.dict); err != nil {
			_ = cl.FreeCCtx(cctx)
			releasePooledClient(cl, err)
			w.err = err
			return err
		}
	}
	w.cl = cl
	w.cctx = cctx
	w.outBuf = make([]byte, CompressBound(streamInChunkSize)+64)
	return nil
}

// SetNbWorkers sets the number of workers to run compression in parallel.
// Because zstd_server executes single-threaded per session, n > 1 returns
// ErrNoParallelSupport.
func (w *Writer) SetNbWorkers(n int) error {
	if n > 1 {
		return ErrNoParallelSupport
	}
	return nil
}

// Write writes a compressed form of p to the underlying io.Writer.
func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("zstd: write to closed Writer")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.init(); err != nil {
		return 0, err
	}
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > streamInChunkSize {
			chunk = chunk[:streamInChunkSize]
		}
		for len(chunk) > 0 {
			_, out, consumed, err := w.cl.CompressStream2Flat(w.cctx, w.outBuf, chunk, ZSTD_e_continue)
			if err != nil {
				w.err = err
				return total, err
			}
			if len(out) > 0 {
				if _, werr := w.w.Write(out); werr != nil {
					w.err = werr
					return total, werr
				}
			}
			c := int(consumed)
			chunk = chunk[c:]
			p = p[c:]
			total += c
		}
	}
	return total, nil
}

// Flush writes any unwritten data to the underlying io.Writer.
func (w *Writer) Flush() error {
	if w.closed {
		return errors.New("zstd: flush of closed Writer")
	}
	if err := w.init(); err != nil {
		return err
	}
	for {
		rem, out, _, err := w.cl.CompressStream2Flat(w.cctx, w.outBuf, nil, ZSTD_e_flush)
		if err != nil {
			w.err = err
			return err
		}
		if len(out) > 0 {
			if _, werr := w.w.Write(out); werr != nil {
				w.err = werr
				return werr
			}
		}
		if rem == 0 {
			return nil
		}
	}
}

// Close closes the Writer, flushing any unwritten data to the underlying
// io.Writer and freeing objects, but does not close the underlying io.Writer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	if w.cl == nil && w.err == nil {
		if err := w.init(); err != nil {
			w.closed = true
			return err
		}
	}
	w.closed = true
	var firstErr error
	if w.cl != nil && w.err == nil {
		for {
			rem, out, _, err := w.cl.CompressStream2Flat(w.cctx, w.outBuf, nil, ZSTD_e_end)
			if err != nil {
				firstErr = err
				break
			}
			if len(out) > 0 {
				if _, werr := w.w.Write(out); werr != nil {
					firstErr = werr
					break
				}
			}
			if rem == 0 {
				break
			}
		}
	} else if w.err != nil {
		firstErr = w.err
	}
	if w.cl != nil {
		freeErr := w.cl.FreeCCtx(w.cctx)
		if firstErr == nil && freeErr != nil {
			firstErr = freeErr
		}
		releasePooledClient(w.cl, firstErr)
		w.cl = nil
		w.cctx = CCtx{}
	}
	return firstErr
}

type reader struct {
	r        io.Reader
	dict     []byte
	cl       *Client
	dctx     DCtx
	inBuf    []byte
	inOff    int
	inEnd    int
	outBuf   []byte
	outSlice []byte
	lastHint uint64
	hasFrame bool
	eof      bool
	closed   bool
	err      error
}

// NewReader creates a new io.ReadCloser. Reads from the returned ReadCloser
// read and decompress data from r. It is the caller's responsibility to call
// Close on the ReadCloser when done.
func NewReader(r io.Reader) io.ReadCloser {
	return NewReaderDict(r, nil)
}

// NewReaderDict is like NewReader but uses a preset dictionary. NewReaderDict
// ignores the dictionary if it is nil.
func NewReaderDict(r io.Reader, dict []byte) io.ReadCloser {
	return &reader{
		r:    r,
		dict: dict,
	}
}

func (r *reader) init() error {
	if r.err != nil {
		return r.err
	}
	if r.cl != nil {
		return nil
	}
	cl, err := sharedPool.Get(context.Background())
	if err != nil {
		r.err = err
		return err
	}
	dctx, err := cl.CreateDCtx()
	if err != nil {
		releasePooledClient(cl, err)
		r.err = err
		return err
	}
	if len(r.dict) > 0 {
		if _, err := cl.DCtxLoadDictionary(dctx, r.dict); err != nil {
			_ = cl.FreeDCtx(dctx)
			releasePooledClient(cl, err)
			r.err = err
			return err
		}
	}
	r.cl = cl
	r.dctx = dctx
	r.inBuf = make([]byte, streamInChunkSize)
	r.outBuf = make([]byte, streamOutChunkSize)
	return nil
}

func (r *reader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, errors.New("zstd: read from closed Reader")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.outSlice) > 0 {
		n := copy(p, r.outSlice)
		r.outSlice = r.outSlice[n:]
		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if err := r.init(); err != nil {
		return 0, err
	}

	for {
		if r.inOff < r.inEnd || r.lastHint > 0 {
			in := r.inBuf[r.inOff:r.inEnd]
			hint, out, consumed, err := r.cl.DecompressStreamFlat(r.dctx, r.outBuf, in)
			if err != nil {
				r.err = err
				return 0, err
			}
			r.inOff += int(consumed)
			r.lastHint = hint
			r.hasFrame = true
			if len(out) > 0 {
				n := copy(p, out)
				r.outSlice = out[n:]
				return n, nil
			}
			if r.inOff < r.inEnd {
				continue
			}
		}

		if r.eof {
			if r.lastHint != 0 {
				r.err = io.ErrUnexpectedEOF
				return 0, r.err
			}
			r.err = io.EOF
			return 0, io.EOF
		}

		r.inOff = 0
		r.inEnd = 0
		n, err := r.r.Read(r.inBuf)
		if n > 0 {
			r.inEnd = n
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.eof = true
				if r.inEnd == 0 {
					if r.lastHint != 0 {
						r.err = io.ErrUnexpectedEOF
						return 0, r.err
					}
					r.err = io.EOF
					return 0, io.EOF
				}
			} else {
				r.err = err
				return 0, err
			}
		}
	}
}

func (r *reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.cl == nil {
		return nil
	}
	err := r.cl.FreeDCtx(r.dctx)
	releasePooledClient(r.cl, err)
	r.cl = nil
	r.dctx = DCtx{}
	return err
}
