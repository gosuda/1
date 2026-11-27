package columnar

// Allocation owns an admitted byte claim. Resize is atomic on failure; reducing
// a live claim must succeed. Release is idempotent and ends its ownership.
// Owners drop references before shrinking or releasing their claims.
type Allocation interface {
	Resize(bytes uint64) error
	// GrowUpTo atomically admits up to maximum additional bytes. Zero growth is
	// valid; failure leaves the claim unchanged. The caller releases unused bytes.
	GrowUpTo(maximum uint64) (uint64, error)
	Release()
}

// Allocator admits actual live capacity, including old and new growth buffers.
// A zero-byte claim still owns an admission slot until Release.
type Allocator interface {
	Reserve(bytes uint64) (Allocation, error)
}

type BuilderOptions struct {
	MaxRows  int
	MaxBytes uint64
	// Nil uses only MaxBytes; otherwise the builder owns the claim until Close.
	Allocator Allocator
}
