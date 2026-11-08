package storage

import "context"

// ScanObservation contains measured deltas from one physical scan source.
// ReadBytes counts bytes returned by physical reads; DecodedBytes counts encoded
// column payload consumed by decoders. RangesPruned counts source/query-span
// pairs rejected before block reads. Known distinguishes a measured zero from a
// metric the source cannot observe.
type ScanObservation struct {
	ReadBytes     ObservationCount
	DecodedBytes  ObservationCount
	BlocksRead    ObservationCount
	BlocksPruned  ObservationCount
	RangesPruned  ObservationCount
}

// ScanObserver is borrowed from the VectorizedScan context. Implementations must
// be concurrency-safe because one invocation can scan independent morsels.
type ScanObserver interface {
	ObserveScan(ScanObservation)
}

type scanObserverKey struct{}

// WithScanObserver attaches a borrowed observer to one scan-construction path.
func WithScanObserver(ctx context.Context, observer ScanObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, scanObserverKey{}, observer)
}

// ScanObserverFromContext returns the observer borrowed by a storage scan.
func ScanObserverFromContext(ctx context.Context) ScanObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(scanObserverKey{}).(ScanObserver)
	return observer
}
