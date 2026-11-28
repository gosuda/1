package txn

import (
	"bytes"
	"context"
	"io"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

func (v *frozenOverlay) Scan(ctx context.Context, request storage.OverlayScanRequest) (storage.MutationCursor, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.closed {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Target.Validate(); err != nil {
		return nil, err
	}
	if request.Direction != storage.ScanForward && request.Direction != storage.ScanReverse {
		return nil, storage.ErrInvalidScanPlan
	}
	if len(request.StartKey) != 0 && len(request.EndKey) != 0 && bytes.Compare(request.StartKey, request.EndKey) >= 0 {
		return nil, storage.ErrRangeInvalid
	}
	overhead := uint64(unsafe.Sizeof(frozenOverlayCursor{}))
	if overhead > v.limit || uint64(len(request.StartKey))+uint64(len(request.EndKey)) > v.limit-overhead {
		return nil, ordered.ErrBudget
	}
	request.StartKey = bytes.Clone(request.StartKey)
	request.EndKey = bytes.Clone(request.EndKey)
	reverse := request.Direction == storage.ScanReverse
	cursor := &frozenOverlayCursor{request: request}
	if reverse {
		upper := overlayKey{target: request.Target, edge: 1}
		if len(request.EndKey) != 0 {
			upper = overlayKey{target: request.Target, key: request.EndKey}
		}
		cursor.points = v.state.points.Seek(upper, true)
		cursor.ranges = v.state.ranges.Seek(upper, true)
	} else {
		lower := overlayKey{target: request.Target, key: request.StartKey}
		cursor.points = v.state.points.Seek(lower, false)
		before := v.state.ranges.Seek(lower, true)
		key, rangeMutation, ok := before.Next()
		before.Close()
		if ok && key.target == request.Target && bytes.Compare(rangeMutation.EndKey, request.StartKey) > 0 {
			lower = key
		}
		cursor.ranges = v.state.ranges.Seek(lower, false)
	}
	return cursor, nil
}

type frozenOverlayCursor struct {
	mu                                           sync.Mutex
	request                                      storage.OverlayScanRequest
	points, ranges                               overlayCursor
	point, interval                              storage.Mutation
	pointReady, rangeReady, pointDone, rangeDone bool
	closed                                       bool
}

func (c *frozenOverlayCursor) Next(ctx context.Context) (storage.Mutation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return storage.Mutation{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		c.close()
		return storage.Mutation{}, err
	}
	if !c.pointReady && !c.pointDone {
		value, ok, err := c.advance(ctx, &c.points, false)
		if err != nil {
			c.close()
			return storage.Mutation{}, err
		}
		c.point, c.pointReady, c.pointDone = value, ok, !ok
	}
	if !c.rangeReady && !c.rangeDone {
		value, ok, err := c.advance(ctx, &c.ranges, true)
		if err != nil {
			c.close()
			return storage.Mutation{}, err
		}
		c.interval, c.rangeReady, c.rangeDone = value, ok, !ok
	}
	if !c.pointReady && !c.rangeReady {
		c.close()
		return storage.Mutation{}, io.EOF
	}
	pointFirst := c.pointReady
	if c.pointReady && c.rangeReady {
		if c.request.Direction == storage.ScanReverse {
			pointFirst = bytes.Compare(c.point.Key, c.interval.EndKey) >= 0
		} else {
			order := bytes.Compare(c.point.Key, c.interval.Key)
			pointFirst = order < 0 || (order == 0 && c.point.Ordinal > c.interval.Ordinal)
		}
	}
	if pointFirst {
		value := c.point
		c.point = storage.Mutation{}
		c.pointReady = false
		return value, nil
	}
	value := c.interval
	c.interval = storage.Mutation{}
	c.rangeReady = false
	return value, nil
}

func (c *frozenOverlayCursor) advance(ctx context.Context, cursor *overlayCursor, interval bool) (storage.Mutation, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return storage.Mutation{}, false, err
		}
		key, value, ok := cursor.Next()
		if !ok || key.target != c.request.Target {
			return storage.Mutation{}, false, nil
		}
		if len(c.request.EndKey) != 0 && bytes.Compare(key.key, c.request.EndKey) >= 0 {
			if c.request.Direction == storage.ScanForward {
				return storage.Mutation{}, false, nil
			}
			continue
		}
		if len(c.request.StartKey) != 0 && bytes.Compare(key.key, c.request.StartKey) < 0 {
			if interval && bytes.Compare(value.EndKey, c.request.StartKey) > 0 {
				return value, true, nil
			}
			if c.request.Direction == storage.ScanReverse {
				return storage.Mutation{}, false, nil
			}
			continue
		}
		return value, true, nil
	}
}

func (c *frozenOverlayCursor) Close() error { c.mu.Lock(); defer c.mu.Unlock(); c.close(); return nil }
func (c *frozenOverlayCursor) close() {
	c.closed = true
	c.points.Close()
	c.ranges.Close()
	c.point, c.interval = storage.Mutation{}, storage.Mutation{}
	c.request = storage.OverlayScanRequest{}
}
