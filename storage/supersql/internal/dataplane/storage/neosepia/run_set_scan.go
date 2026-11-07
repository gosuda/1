package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type pinnedScanLimits struct {
	SourceBytes, BlockBytes uint64
	Merge, Reverse          mergeLimits
}

type pinnedScanCursor struct {
	mu     sync.Mutex
	bound  *boundScan
	cursor *paxScanCursor
	pin    *runPin
}

// SourceBytes bounds source metadata and live blocks. Plan metadata, predicate
// scratch, output selections and merge state have separate quotas.
func (pin *runPin) scan(ctx context.Context, timestamp model.Timestamp, direction storage.ScanDirection, spec paxScanSpec, limits pinnedScanLimits) (_ *pinnedScanCursor, err error) {
	if direction != storage.ScanForward && direction != storage.ScanReverse {
		return nil, storage.ErrInvalidScanPlan
	}
	retained, err := pin.retain(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, retained.Close())
		}
	}()
	wrapperBytes := uint64(unsafe.Sizeof(pinnedScanCursor{}))
	if wrapperBytes > limits.SourceBytes {
		return nil, columnar.ErrBatchBudget
	}
	sourceBytes := limits.SourceBytes - wrapperBytes
	ledgerBytes := uint64(unsafe.Sizeof(sourceMemory{}))
	observer := storage.ScanObserverFromContext(ctx)
	if observer != nil {
		observer.ObserveScan(storage.ScanObservation{
			ReadBytes: storage.ObservationCount{Known: true},
			DecodedBytes: storage.ObservationCount{Known: true},
			BlocksRead: storage.ObservationCount{Known: true},
			BlocksPruned: storage.ObservationCount{Known: true},
			RangesPruned: storage.ObservationCount{Known: true},
		})
	}
	metadata := uint64(unsafe.Sizeof(runVersionCursor{}))
	if direction == storage.ScanReverse {
		metadata, err = reverseRunMemory(limits.Reverse)
		if err != nil {
			return nil, err
		}
	}
	sourceOverhead := uint64(unsafe.Sizeof((*runVersionCursor)(nil))) + uint64(unsafe.Sizeof(runSequenceState{}))
	if metadata > math.MaxUint64-sourceOverhead {
		return nil, columnar.ErrBatchBudget
	}
	perSource := metadata + sourceOverhead
	var count uint64
	if len(spec.Spans) != 0 {
		if err := retained.visitScanSources(ctx, timestamp, spec, nil, func(_ *sstFile, _ []*sstFile) error {
			if count == uint64(math.MaxInt) || ledgerBytes > sourceBytes || count >= (sourceBytes-ledgerBytes)/perSource {
				return columnar.ErrBatchBudget
			}
			count++
			return nil
		}); err != nil {
			return nil, err
		}
	}
	var memory *sourceMemory
	if count != 0 {
		memory = &sourceMemory{limit: sourceBytes - ledgerBytes - count*perSource, observer: observer}
	}
	sources := make([]*runVersionCursor, 0, int(count))
	defer func() {
		if err != nil {
			for _, source := range sources {
				err = errors.Join(err, source.Close())
			}
		}
	}()
	if count != 0 {
		if err := retained.visitScanSources(ctx, timestamp, spec, observer, func(file *sstFile, sequence []*sstFile) error {
			if sequence != nil {
				sources = append(sources, newRunSequenceCursor(sequence, spec.Target, spec.Spans, direction, limits.BlockBytes, limits.Reverse, memory))
				return nil
			}
			first, end, err := file.spanCandidates(ctx, spec.Spans, timestamp)
			if err != nil {
				return err
			}
			if first == end {
				return nil
			}
			var source *runVersionCursor
			if direction == storage.ScanReverse {
				source, err = newReverseRunVersionCursor(file, first, end, limits.BlockBytes, limits.Reverse, memory)
			} else {
				source, err = newRunVersionCursor(file, first, end, limits.BlockBytes, memory)
			}
			if err != nil {
				return err
			}
			sources = append(sources, source)
			return nil
		}); err != nil {
			return nil, err
		}
	}
	visible, err := newMVCCCursor(ctx, sources, timestamp, limits.Merge, direction)
	if err != nil {
		return nil, err
	}
	cursor, err := newPAXScanCursor(ctx, visible, spec)
	if err != nil {
		return nil, err
	}
	return &pinnedScanCursor{cursor: cursor, pin: retained}, nil
}

func (cursor *pinnedScanCursor) Next(ctx context.Context, out *columnar.Batch) error {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if cursor.cursor == nil {
		return io.EOF
	}
	if err := cursor.cursor.Next(ctx, out); err != nil {
		return iterationError(err, cursor.closeLocked())
	}
	return nil
}

func (cursor *pinnedScanCursor) Close() error {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	return cursor.closeLocked()
}

func (cursor *pinnedScanCursor) closeLocked() error {
	if cursor.cursor == nil {
		return nil
	}
	inner, pin := cursor.cursor, cursor.pin
	cursor.cursor, cursor.pin = nil, nil
	err := errors.Join(inner.Close(), pin.Close())
	if cursor.bound != nil {
		// Cleanup cannot re-enter the reader lock: snapshot closure may race it.
		cursor.bound.active.Add(-1)
		cursor.bound = nil
	}
	return err
}
