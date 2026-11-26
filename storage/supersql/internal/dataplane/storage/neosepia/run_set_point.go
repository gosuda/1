package neosepia

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type pinnedPointLimits struct {
	SourceBytes, BlockBytes uint64
	Merge                   mergeLimits
	Point                   pointReadLimits
}

// get and Close are serialized by the snapshot owner. SourceBytes bounds source
// metadata and live blocks independently of merge state and the owned result.
// Own hits need no committed-source budget.
func (pin *runPin) get(ctx context.Context, timestamp model.Timestamp, overlay storage.OverlayView, request storage.ReadRequest, limits pinnedPointLimits) (_ model.Row, err error) {
	if pin.closed {
		return model.Row{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return model.Row{}, err
	}
	if timestamp <= 0 {
		return model.Row{}, storage.ErrSnapshotNotClosed
	}
	if err := request.Validate(ctx); err != nil {
		return model.Row{}, err
	}
	if overlay != nil {
		retained, err := overlay.Retain()
		if err != nil {
			return model.Row{}, err
		}
		defer func() { err = errors.Join(err, retained.Close()) }()
		mutation, found, err := retained.Lookup(ctx, request.Target, request.Key)
		if err != nil {
			return model.Row{}, err
		}
		if found {
			return copyOwnPoint(ctx, mutation, request, limits.Point.RowBytes)
		}
	}
	sources, err := pin.pointSources(ctx, timestamp, request, limits)
	if err != nil {
		return model.Row{}, err
	}
	visible, err := newMVCCCursor(ctx, sources, timestamp, limits.Merge, storage.ScanForward)
	if err != nil {
		return model.Row{}, err
	}
	return readPAXPoint(ctx, visible, nil, request, limits.Point)
}

func (pin *runPin) pointSources(ctx context.Context, timestamp model.Timestamp, request storage.ReadRequest, limits pinnedPointLimits) (_ []*runVersionCursor, err error) {
	ledgerBytes := uint64(unsafe.Sizeof(sourceMemory{}))
	perSource := uint64(unsafe.Sizeof(runVersionCursor{})) + uint64(unsafe.Sizeof((*runVersionCursor)(nil)))
	var count uint64
	if err := pin.visitSnapshot(ctx, request.Target, timestamp, func(file *sstFile) error {
		first, end, err := file.Candidates(ctx, request.Key, timestamp)
		if err != nil {
			return err
		}
		if first == end {
			return nil
		}
		if count == uint64(math.MaxInt) || ledgerBytes > limits.SourceBytes || count >= (limits.SourceBytes-ledgerBytes)/perSource {
			return columnar.ErrBatchBudget
		}
		count++
		return nil
	}); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	memory := &sourceMemory{limit: limits.SourceBytes - ledgerBytes - count*perSource}
	sources := make([]*runVersionCursor, 0, int(count))
	defer func() {
		if err != nil {
			for _, source := range sources {
				err = errors.Join(err, source.Close())
			}
		}
	}()
	if err := pin.visitSnapshot(ctx, request.Target, timestamp, func(file *sstFile) error {
		first, end, err := file.Candidates(ctx, request.Key, timestamp)
		if err != nil {
			return err
		}
		if first == end {
			return nil
		}
		source, err := newRunVersionCursor(file, first, end, limits.BlockBytes, memory)
		if err != nil {
			return err
		}
		source.pointKey = request.Key
		sources = append(sources, source)
		return nil
	}); err != nil {
		return nil, err
	}
	return sources, nil
}
