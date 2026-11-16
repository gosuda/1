package zstd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"

	"gosuda.org/zstd/internal/embedded"
	"gosuda.org/hqq/bindgen/rt"
	"gosuda.org/hqq/launch"
	"gosuda.org/hqq/secureexec"
)

// ServerBinary materializes the embedded prebuilt zstd_server binary for the
// current OS and architecture, verifies its SHA-256 digest against
// ExpectedHash(runtime.GOOS, runtime.GOARCH), and returns its filesystem path
// and verified digest.
//
// If no embedded binary is available for the current platform, ServerBinary
// falls back to the HQQ_ZSTD_SERVER environment variable when set.
func ServerBinary() (string, secureexec.Hash, error) {
	if expected, ok := ExpectedHash(runtime.GOOS, runtime.GOARCH); ok {
		path, err := embedded.Materialize(expected)
		if err == nil {
			return path, expected, nil
		}
	}
	if p := os.Getenv("HQQ_ZSTD_SERVER"); p != "" {
		h, _, err := secureexec.HashFile(p)
		if err != nil {
			return "", secureexec.Hash{}, fmt.Errorf("zstd: hash HQQ_ZSTD_SERVER (%s): %w", p, err)
		}
		return p, h, nil
	}
	return "", secureexec.Hash{}, fmt.Errorf("zstd: no embedded zstd_server binary for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// StartServer verifies the embedded zstd_server binary with secureexec and
// starts an isolated worker process bound to the parent process lifetime via
// --parent-stdin.
func StartServer(ctx context.Context) (*launch.Server, error) {
	binPath, hash, err := ServerBinary()
	if err != nil {
		return nil, err
	}
	listener, err := launch.NewListenerName("hqqzstd")
	if err != nil {
		return nil, err
	}
	return launch.Start(ctx, launch.Config{
		Binary:   binPath,
		SHA256:   hash,
		Listener: listener,
		Args:     []string{listener},
	})
}

var (
	sharedMu   sync.Mutex
	sharedSrv  *launch.Server
	sharedPool = rt.NewPool(16, func(ctx context.Context) (*Client, error) {
		return Open(ctx)
	}, func(c *Client) error {
		return c.Close()
	})
)

func ensureSharedServer(ctx context.Context) (string, error) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedSrv != nil {
		select {
		case <-sharedSrv.Done():
			sharedSrv = nil
		default:
			return sharedSrv.Listener(), nil
		}
	}
	srv, err := StartServer(ctx)
	if err != nil {
		return "", err
	}
	sharedSrv = srv
	return srv.Listener(), nil
}

// Open connects a new *Client session to the automatically managed embedded
// zstd_server worker process using DefaultOptions().
func Open(ctx context.Context) (*Client, error) {
	return OpenWith(ctx, DefaultOptions())
}

// OpenWith connects a new *Client session to the automatically managed embedded
// zstd_server worker process using opts.
func OpenWith(ctx context.Context, opts rt.Options) (*Client, error) {
	listener, err := ensureSharedServer(ctx)
	if err != nil {
		return nil, err
	}
	cl, err := Dial(ctx, listener, opts)
	if err != nil {
		sharedMu.Lock()
		if sharedSrv != nil && sharedSrv.Listener() == listener {
			select {
			case <-sharedSrv.Done():
				sharedSrv = nil
			default:
			}
		}
		sharedMu.Unlock()
		return nil, err
	}
	return cl, nil
}

func releasePooledClient(cl *Client, err error) {
	if err == nil {
		sharedPool.Put(cl)
		return
	}
	var le *rt.LibError
	if errors.As(err, &le) {
		sharedPool.Put(cl)
	} else {
		sharedPool.Discard(cl)
	}
}

// prepareCompressDst selects or allocates a destination slice of at most
// CompressBound(len(src)) bytes.
func prepareCompressDst(dst, src []byte) ([]byte, int, error) {
	bound := CompressBound(len(src))
	if bound <= 0 {
		return nil, 0, ErrSrcSizeWrong
	}
	if cap(dst) > 0 {
		n := cap(dst)
		if n > bound {
			n = bound
		}
		return dst[:n], bound, nil
	}
	return make([]byte, bound), bound, nil
}

// Compress compresses src into dst at DefaultCompression. If you have a buffer
// to use, you can pass it to prevent allocation. If it is too small, or if nil
// is passed, a new buffer will be allocated and returned.
func Compress(dst, src []byte) ([]byte, error) {
	return CompressLevel(dst, src, DefaultCompression)
}

// CompressLevel is the same as Compress but you can pass a compression level.
func CompressLevel(dst, src []byte, level int) ([]byte, error) {
	buf, bound, err := prepareCompressDst(dst, src)
	if err != nil {
		return nil, err
	}
	cl, err := sharedPool.Get(context.Background())
	if err != nil {
		return nil, err
	}
	out, err := cl.Compress(buf, src, int32(level))
	if err != nil && IsDstSizeTooSmallError(err) && len(buf) < bound {
		buf = make([]byte, bound)
		out, err = cl.Compress(buf, src, int32(level))
	}
	releasePooledClient(cl, err)
	return out, err
}

// DecompressInto decompresses src into dst. Unlike Decompress, DecompressInto
// requires that dst be sufficiently large to hold the decompressed payload.
// It returns the number of bytes copied and an error if any is encountered.
// If dst is too small, DecompressInto errors.
func DecompressInto(dst, src []byte) (int, error) {
	if len(src) == 0 {
		return 0, ErrEmptySlice
	}
	cl, err := sharedPool.Get(context.Background())
	if err != nil {
		return 0, err
	}
	out, err := cl.Decompress(dst, src)
	releasePooledClient(cl, err)
	if err != nil {
		return 0, err
	}
	return len(out), nil
}

// Decompress decompresses src into dst. If you have a buffer to use, you can
// pass it to prevent allocation. If it is too small, or if nil is passed, a
// new buffer will be allocated and returned.
func Decompress(dst, src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, ErrEmptySlice
	}
	cl, err := sharedPool.Get(context.Background())
	if err != nil {
		return nil, err
	}
	out, canPool, err := decompressWith(cl, nil, dst, src)
	if canPool {
		sharedPool.Put(cl)
	} else {
		sharedPool.Discard(cl)
	}
	return out, err
}

// decompressWith executes DataDog/zstd's decompression strategy using either
// one-shot cl.Decompress (when dctx == nil) or cl.DecompressDCtx (when dctx != nil):
//  1. Try caller-supplied dst if non-empty.
//  2. Inspect GetFrameContentSize for exact single-frame allocation when known.
//  3. Retry up to 3 times starting at 4*len(src) and doubling on ErrDstSizeTooSmall.
//  4. Fall back to the streaming reader when compression ratio exceeds the retry bound.
func decompressWith(cl *Client, dctx *DCtx, dst, src []byte) ([]byte, bool, error) {
	const maxOneShotDst = 64 << 20

	doOne := func(buf []byte) ([]byte, error) {
		if dctx != nil {
			return cl.DecompressDCtx(*dctx, buf, src)
		}
		return cl.Decompress(buf, src)
	}
	isReusableErr := func(err error) bool {
		if err == nil {
			return true
		}
		var le *rt.LibError
		return errors.As(err, &le)
	}

	if len(dst) > 0 {
		maxCap := len(dst)
		if maxCap > maxOneShotDst {
			maxCap = maxOneShotDst
		}
		out, err := doOne(dst[:maxCap])
		if err == nil {
			return out, true, nil
		}
		if !IsDstSizeTooSmallError(err) {
			return nil, isReusableErr(err), err
		}
	}

	hdr := src
	if len(hdr) > int(ZSTD_FRAMEHEADER_MAX) {
		hdr = hdr[:ZSTD_FRAMEHEADER_MAX]
	}
	if sz, err := cl.GetFrameContentSize(hdr); err == nil &&
		sz != ZSTD_CONTENTSIZE_UNKNOWN &&
		sz != ZSTD_CONTENTSIZE_ERROR &&
		sz <= maxOneShotDst {
		buf := make([]byte, int(sz))
		out, err := doOne(buf)
		if err == nil {
			return out, true, nil
		}
		if !IsDstSizeTooSmallError(err) {
			return nil, isReusableErr(err), err
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
			return out, true, nil
		}
		if !IsDstSizeTooSmallError(err) {
			return nil, isReusableErr(err), err
		}
		nextSize *= 2
	}

	r := NewReader(bytes.NewReader(src))
	out, rerr := io.ReadAll(r)
	cerr := r.Close()
	if rerr != nil {
		return nil, true, rerr
	}
	return out, true, cerr
}
