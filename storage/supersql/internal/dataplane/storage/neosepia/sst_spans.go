package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The returned interval covers every span and earlier crossing tombstones.
// Holes inside the interval remain residual span checks in the native cursor.
func (s *sstFile) spanCandidates(ctx context.Context, spans []encodedSpan, snapshotTS model.Timestamp) (int, int, error) {
	first, end, _, _, err := s.measuredSpanCandidates(ctx, spans, snapshotTS)
	return first, end, err
}

func (s *sstFile) observedSpanCandidates(ctx context.Context, spans []encodedSpan, snapshotTS model.Timestamp, observer storage.ScanObserver) (int, int, error) {
	first, end, blocks, ranges, err := s.measuredSpanCandidates(ctx, spans, snapshotTS)
	if err == nil && observer != nil {
		observer.ObserveScan(storage.ScanObservation{
			BlocksPruned: storage.ObservationCount{Value: blocks, Known: true},
			RangesPruned: storage.ObservationCount{Value: ranges, Known: true},
		})
	}
	return first, end, err
}

func (s *sstFile) measuredSpanCandidates(ctx context.Context, spans []encodedSpan, snapshotTS model.Timestamp) (int, int, uint64, uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return 0, 0, 0, 0, storage.ErrClosed
	}
	first, end := s.count, 0
	matched := uint64(0)
	for _, span := range spans {
		if err := ctx.Err(); err != nil {
			return 0, 0, 0, 0, err
		}
		if compareCuts(span.lower, span.upper) >= 0 {
			return 0, 0, 0, 0, storage.ErrInvalidScanPlan
		}
		low, high := 0, s.count
		for low < high {
			mid := low + (high-low)/2
			coverage := keyCut{prefix: s.key(s.entry(mid).prefixCoverage)}
			if compareCuts(coverage, span.lower) < 0 {
				low = mid + 1
			} else {
				high = mid
			}
		}
		begin := low
		high = s.count
		for low < high {
			mid := low + (high-low)/2
			minimum := keyCut{prefix: s.key(s.entry(mid).minimum)}
			if compareCuts(minimum, span.upper) < 0 {
				low = mid + 1
			} else {
				high = mid
			}
		}
		if begin < low {
			matched++
			first = min(first, begin)
			end = max(end, low)
		}
	}
	first, end = s.fragmentBlocks(first, end)
	if snapshotTS > 0 {
		for first < end && s.entry(first).minCommitTS > snapshotTS {
			first++
		}
		for first < end && s.entry(end-1).minCommitTS > snapshotTS {
			end--
		}
	}
	if first >= end {
		first, end = 0, 0
	}
	return first, end, uint64(s.count-(end-first)), uint64(len(spans))-matched, ctx.Err()
}
