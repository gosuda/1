# hqq-zstd (`gosuda.org/zstd`)

`gosuda.org/zstd` is a **zero-`cgo` (`CGO_ENABLED=0`) Zstandard (`libzstd`) library for Go** powered by HQQ shared-memory IPC, designed as a drop-in replacement for [`github.com/DataDog/zstd`](https://pkg.go.dev/github.com/DataDog/zstd) plus low-level pipelined async RPC (`*zstd.Client`). Your Go binary compiles without linking C objects, while executing the official C `libzstd` (`1.5.7`) compressor and decompressor in an isolated shared-memory worker process (`zstd_server`).

SLSA-attested prebuilt `zstd_server` binaries are embedded per target platform (`//go:build` + `//go:embed`), extracted to the user cache on first use, verified against pinned SHA-256 digests via `gosuda.org/hqq/secureexec`, and bound to the Go parent process lifetime via `--parent-stdin`. No external binary download or C toolchain is required.

```bash
go get gosuda.org/zstd
```

## Supported Platforms

| OS | Architectures | Embedded & SLSA-Attested Release Binary |
| :--- | :--- | :--- |
| **Linux** | `amd64`, `arm64` | `zstd_server_linux_amd64`, `zstd_server_linux_arm64` (embedded) |
| **macOS** | `arm64` (Apple Silicon), `amd64` | `zstd_server_darwin_arm64`, `zstd_server_darwin_amd64` (embedded) |
| **Windows** | `amd64`, `arm64` | `zstd_server_windows_amd64.exe`, `zstd_server_windows_arm64.exe` (embedded) |

---

## Quickstart

### 1. Simple `Compress` / `Decompress` (`github.com/DataDog/zstd` Drop-In)

```go
package main

import (
	"fmt"
	"log"

	"gosuda.org/zstd"
)

func main() {
	src := []byte("zero-cgo Zstandard over HQQ shared memory")

	compressed, err := zstd.Compress(nil, src)
	if err != nil {
		log.Fatal(err)
	}

	restored, err := zstd.Decompress(nil, compressed)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d B -> %d B -> %q\n", len(src), len(compressed), restored)
}
```

### 2. Streaming `io.Writer` / `io.ReadCloser` (`NewWriter`, `NewReader`)

```go
var buf bytes.Buffer

w := zstd.NewWriterLevel(&buf, zstd.BestSpeed)
if _, err := w.Write(payload); err != nil {
	return err
}
if err := w.Close(); err != nil {
	return err
}

r := zstd.NewReader(&buf)
defer r.Close()
restored, err := io.ReadAll(r)
```

### 3. Reusable Contexts & Dictionary Bulk Processing (`NewCtx`, `NewBulkProcessor`)

```go
// Reusable per-goroutine context:
ctx := zstd.NewCtx()
compressed, err := ctx.CompressLevel(nil, src, zstd.DefaultCompression)
restored, err := ctx.Decompress(nil, compressed)

// Pre-digested dictionary processor (safe for concurrent goroutines):
bp, err := zstd.NewBulkProcessor(dictionary, zstd.DefaultCompression)
if err != nil {
	return err
}
compressed, err = bp.Compress(nil, src)
restored, err = bp.Decompress(nil, compressed)
```

### 4. Pipelined Batch Calls (`zstd.Open`, `*Future`)

Open a dedicated shared-memory session via `zstd.Open(ctx)` to dispatch multiple blocks concurrently over the shared-memory ring with `CompressAsync` and `DecompressAsync`:

```go
client, err := zstd.Open(ctx)
if err != nil {
	return err
}
defer client.Close()

futures := make([]zstd.CompressFuture, len(blocks))
for i, block := range blocks {
	dst := make([]byte, zstd.CompressBound(len(block)))
	fut, err := client.CompressAsync(dst, block, 1)
	if err != nil {
		return err
	}
	futures[i] = fut
}

for i, fut := range futures {
	frame, err := fut.Wait()
	if err != nil {
		return err
	}
	_ = frame // compressed output for blocks[i]
}
```

---

## Public API Summary

### `github.com/DataDog/zstd` Drop-In API (`package zstd`)

```go
const (
	BestSpeed          = 1
	BestCompression    = 20
	DefaultCompression = 5
)

var (
	ErrEmptyDictionary   = errors.New("Dictionary is empty")
	ErrBadDictionary     = errors.New("Cannot load dictionary")
	ErrEmptySlice        = errors.New("Bytes slice is empty")
	ErrNoParallelSupport = errors.New("No parallel support")
)

// One-shot compression & decompression
func Compress(dst, src []byte) ([]byte, error)
func CompressLevel(dst, src []byte, level int) ([]byte, error)
func CompressBound(srcSize int) int
func Decompress(dst, src []byte) ([]byte, error)
func DecompressInto(dst, src []byte) (int, error)
func IsDstSizeTooSmallError(e error) bool

// Streaming io.Writer & io.ReadCloser
type Writer struct {
	CompressionLevel int
	// contains filtered or unexported fields
}
func NewWriter(w io.Writer) *Writer
func NewWriterLevel(w io.Writer, level int) *Writer
func NewWriterLevelDict(w io.Writer, level int, dict []byte) *Writer
func (w *Writer) Write(p []byte) (int, error)
func (w *Writer) Flush() error
func (w *Writer) Close() error
func (w *Writer) SetNbWorkers(n int) error

func NewReader(r io.Reader) io.ReadCloser
func NewReaderDict(r io.Reader, dict []byte) io.ReadCloser

// Reusable Context
type Ctx interface {
	Compress(dst, src []byte) ([]byte, error)
	CompressLevel(dst, src []byte, level int) ([]byte, error)
	Decompress(dst, src []byte) ([]byte, error)
	DecompressInto(dst, src []byte) (int, error)
}
func NewCtx() Ctx

// Pre-digested Dictionary BulkProcessor
type BulkProcessor struct {
	// contains filtered or unexported fields
}
func NewBulkProcessor(dictionary []byte, compressionLevel int) (*BulkProcessor, error)
func (p *BulkProcessor) Compress(dst, src []byte) ([]byte, error)
func (p *BulkProcessor) Decompress(dst, src []byte) ([]byte, error)
```

### Low-Level Session, Pool & Pipelined Client (`*zstd.Client`)

```go
const (
	Version           = "v0.3.10507"
	ZstdVersionNumber = 10507
)

func Open(ctx context.Context) (*Client, error)
func OpenWith(ctx context.Context, opts rt.Options) (*Client, error)
func StartServer(ctx context.Context) (*launch.Server, error)
func ServerBinary() (path string, hash secureexec.Hash, err error)
func DefaultOptions() rt.Options
func ExpectedHash(goos, goarch string) (secureexec.Hash, bool)
func Dial(ctx context.Context, name string, opts rt.Options) (*Client, error)
func NewPool(name string, opts rt.Options, max int) *rt.Pool[*Client]
func (c *Client) Close() error

// One-shot & async pipelining
func (c *Client) Compress(dst []byte, src []byte, compressionLevel int32) ([]byte, error)
func (c *Client) CompressAsync(dst []byte, src []byte, compressionLevel int32) (CompressFuture, error)
func (c *Client) Decompress(dst []byte, src []byte) ([]byte, error)
func (c *Client) DecompressAsync(dst []byte, src []byte) (DecompressFuture, error)
func (c *Client) GetFrameContentSize(src []byte) (uint64, error)

// Reusable contexts & dictionaries
func (c *Client) CreateCCtx() (CCtx, error)
func (c *Client) FreeCCtx(cctx CCtx) error
func (c *Client) CompressCCtx(cctx CCtx, dst []byte, src []byte, compressionLevel int32) ([]byte, error)
func (c *Client) CCtxSetParameter(cctx CCtx, param int32, value int32) (uint64, error)
func (c *Client) CCtxSetPledgedSrcSize(cctx CCtx, pledgedSrcSize uint64) (uint64, error)
func (c *Client) CCtxReset(cctx CCtx, reset uint32) (uint64, error)
func (c *Client) CCtxLoadDictionary(cctx CCtx, dict []byte) (uint64, error)
func (c *Client) CreateCDict(dictBuffer []byte, compressionLevel int32) (CDict, error)
func (c *Client) FreeCDict(cdict CDict) error
func (c *Client) CompressUsingCDict(cctx CCtx, dst []byte, src []byte, cdict CDict) ([]byte, error)

func (c *Client) CreateDCtx() (DCtx, error)
func (c *Client) FreeDCtx(dctx DCtx) error
func (c *Client) DecompressDCtx(dctx DCtx, dst []byte, src []byte) ([]byte, error)
func (c *Client) DCtxReset(dctx DCtx, reset uint32) (uint64, error)
func (c *Client) DCtxLoadDictionary(dctx DCtx, dict []byte) (uint64, error)
func (c *Client) CreateDDict(dictBuffer []byte) (DDict, error)
func (c *Client) FreeDDict(ddict DDict) error
func (c *Client) DecompressUsingDDict(dctx DCtx, dst []byte, src []byte, ddict DDict) ([]byte, error)

// Streaming
func (c *Client) CompressStream2Flat(cctx CCtx, dst []byte, src []byte, endOp uint32) (rem uint64, out []byte, consumed uint64, err error)
func (c *Client) CompressStream2FlatAsync(cctx CCtx, dst []byte, src []byte, endOp uint32) (CompressStream2FlatFuture, error)
func (c *Client) DecompressStreamFlat(dctx DCtx, dst []byte, src []byte) (rem uint64, out []byte, consumed uint64, err error)
func (c *Client) DecompressStreamFlatAsync(dctx DCtx, dst []byte, src []byte) (DecompressStreamFlatFuture, error)
```

### Error Handling (`package zstd`)

```go
type ErrorCode int

const (
	ErrPrefixUnknown        ErrorCode = 10
	ErrCorruptionDetected   ErrorCode = 20
	ErrChecksumWrong        ErrorCode = 22
	ErrParameterUnsupported ErrorCode = 40
	ErrParameterOutOfBound  ErrorCode = 42
	ErrStageWrong           ErrorCode = 60
	ErrDstSizeTooSmall      ErrorCode = 70
	ErrSrcSizeWrong         ErrorCode = 72
)

func (e ErrorCode) Error() string
func ErrorCodeOf(err error) (ErrorCode, bool)
```

---

## Documentation and License

- [**`docs/ARCHITECTURE.md`**](docs/ARCHITECTURE.md): Shared-memory ring geometry, credit flow control, exposed IDL surface, security policy, and `cgo` comparison benchmarks.
- **License**: Wrapper code is licensed under the [MIT License](LICENSE); Zstandard (`1.5.7`) is used under the [BSD-3-Clause License](third_party/zstd/LICENSE).
