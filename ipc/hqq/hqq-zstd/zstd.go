package zstd

import (
	"errors"
	"fmt"

	"gosuda.org/hqq/bindgen/rt"
)

// Compression level constants matching github.com/DataDog/zstd.
const (
	BestSpeed          = 1
	BestCompression    = 20
	DefaultCompression = 5
)

var (
	// ErrEmptyDictionary is returned when the given dictionary is empty.
	ErrEmptyDictionary = errors.New("Dictionary is empty")
	// ErrBadDictionary is returned when cannot load the given dictionary.
	ErrBadDictionary = errors.New("Cannot load dictionary")
	// ErrEmptySlice is returned when there is nothing to compress or decompress.
	ErrEmptySlice = errors.New("Bytes slice is empty")
	// ErrNoParallelSupport is returned when multi-worker compression is not supported.
	ErrNoParallelSupport = errors.New("No parallel support")
)

// ErrorCode is an error returned by the zstd library (ZSTD_ErrorCode of zstd_errors.h).
type ErrorCode int

const (
	ErrPrefixUnknown        ErrorCode = 10 // not a zstd frame
	ErrCorruptionDetected   ErrorCode = 20
	ErrChecksumWrong        ErrorCode = 22
	ErrParameterOutOfBound  ErrorCode = 42
	ErrParameterUnsupported ErrorCode = 40
	ErrStageWrong           ErrorCode = 60
	ErrDstSizeTooSmall      ErrorCode = 70
	ErrSrcSizeWrong         ErrorCode = 72
)

// Error returns the error string given by zstd.
func (e ErrorCode) Error() string {
	switch e {
	case ErrPrefixUnknown:
		return "Unknown frame descriptor"
	case ErrCorruptionDetected:
		return "Data corruption detected"
	case ErrChecksumWrong:
		return "Restored data doesn't match checksum"
	case ErrParameterUnsupported:
		return "Unsupported parameter"
	case ErrParameterOutOfBound:
		return "Parameter is out of bound"
	case ErrStageWrong:
		return "Operation not authorized at current processing stage"
	case ErrDstSizeTooSmall:
		return "Destination buffer is too small"
	case ErrSrcSizeWrong:
		return "Src size is incorrect"
	default:
		return fmt.Sprintf("zstd error code %d", int(e))
	}
}

// ErrorCodeOf returns the ZSTD_ErrorCode of a library failure reported by a call that returns size_t,
// and false when err is not such a failure. The server sends the size_t result of the failed call,
// which is minus the code; the text of the library's own message is in the *rt.LibError.
func ErrorCodeOf(err error) (ErrorCode, bool) {
	var ec ErrorCode
	if errors.As(err, &ec) {
		return ec, true
	}
	var le *rt.LibError
	if !errors.As(err, &le) || le.Code >= 0 {
		return 0, false
	}
	return ErrorCode(-le.Code), true
}

// IsDstSizeTooSmallError returns whether the error corresponds to zstd's standard
// ErrDstSizeTooSmall error.
func IsDstSizeTooSmallError(e error) bool {
	code, ok := ErrorCodeOf(e)
	return ok && code == ErrDstSizeTooSmall
}

// maxInputSize is ZSTD_MAX_INPUT_SIZE for a 64-bit size_t.
const maxInputSize = 0xFF00FF00FF00FF00

// CompressBound returns the worst case size needed for a destination buffer,
// which can be used to preallocate a destination buffer or select a previously
// allocated buffer from a pool. See zstd.h to mirror implementation of ZSTD_COMPRESSBOUND.
func CompressBound(srcSize int) int {
	if srcSize < 0 || uint64(srcSize) >= maxInputSize {
		return 0
	}
	u := uint64(srcSize)
	n := u + u>>8
	if u < 128<<10 {
		n += (128<<10 - u) >> 11
	}
	return int(n)
}

// DefaultOptions is the session geometry that README.md documents and that the defaults of the
// server (server/main.c) match: 64 KiB frames, 64 frames per direction, and messages up to 128 MiB.
// A call with a destination of at most 65,501 bytes has a response bound of one frame (cap plus 19
// bytes of framing) and is built in place in the ring slot.
// rt.Options{} would select 16 KiB frames and 16 MiB messages.
func DefaultOptions() rt.Options {
	return rt.Options{BufferSize: 64 << 10, BufferCount: 64, MaxMessage: 128 << 20}
}
