package expr

import (
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (e *evaluation) invokeScalar(call ScalarCall, args []model.Value, result register, owned bool) (value model.Value, err error) {
	base := e.used
	budget := e.remaining()
	if e.state.work != nil {
		budget, err = e.state.work.GrowUpTo(budget)
		if err != nil {
			return model.Value{}, err
		}
	}
	e.callAdmission = true
	e.callLimit = base + budget
	completed := false
	local := callbackAllocator{limit: budget}
	defer func() {
		local.closed = true
		if !completed {
			value = model.Value{}
			e.used = base
		}
		e.callAdmission = false
		e.callLimit = 0
		if e.state.work != nil {
			if releaseErr := e.state.work.Resize(e.used); releaseErr != nil {
				value = model.Value{}
				err = errors.Join(err, releaseErr)
			}
		}
	}()
	if !owned {
		budget /= 2
	}
	value, err = call(e.ctx, args, builtin.CallBudget{OutputBytes: int64(min(budget, uint64(1<<63-1))), MemoryBytes: local.limit, Allocator: &local, Temporal: e.state.options.Temporal, Sequences: e.state.options.Sequences, Relations: e.state.options.Relations, Settings: e.state.options.Settings, TextSearch: e.state.options.TextSearch, Advisory: e.state.options.Advisory, Native: e.state.options.Native})
	if local.used != 0 {
		return model.Value{}, errors.Join(err, invalid("scalar callback retained workspace reservations"))
	}
	if err != nil {
		return model.Value{}, err
	}
	if err = e.ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if err = validateValue(e.ctx, value, result.layout, 0); err != nil {
		return model.Value{}, err
	}
	if value.IsNull && !result.nullable {
		return model.Value{}, invalid("non-nullable function returned NULL")
	}
	remaining := local.limit - local.used
	if !owned {
		remaining /= 2
	}
	value, err = e.retainCallResult(value, owned, min(budget, remaining))
	if err != nil {
		return model.Value{}, err
	}
	completed = true
	return value, nil
}

// callbackAllocator divides an already admitted claim without reserving it again.
type callbackAllocator struct {
	limit  uint64
	used   uint64
	closed bool
}

func (a *callbackAllocator) AvailableBytes() uint64 {
	if a.closed {
		return 0
	}
	return a.limit - a.used
}

func (a *callbackAllocator) Reserve(bytes uint64) (columnar.Allocation, error) {
	header := uint64(unsafe.Sizeof(callbackAllocation{}))
	if a.closed || header > a.limit-a.used || bytes > a.limit-a.used-header {
		return nil, ErrLimit
	}
	a.used += bytes + header
	return &callbackAllocation{owner: a, bytes: bytes}, nil
}

type callbackAllocation struct {
	owner *callbackAllocator
	bytes uint64
}

func (a *callbackAllocation) Resize(bytes uint64) error {
	if a.owner == nil || a.owner.closed {
		return ErrLimit
	}
	if bytes > a.bytes && bytes-a.bytes > a.owner.limit-a.owner.used {
		return ErrLimit
	}
	a.owner.used = a.owner.used - a.bytes + bytes
	a.bytes = bytes
	return nil
}

func (a *callbackAllocation) GrowUpTo(maximum uint64) (uint64, error) {
	if a.owner == nil || a.owner.closed {
		return 0, ErrLimit
	}
	growth := min(maximum, a.owner.limit-a.owner.used)
	a.owner.used += growth
	a.bytes += growth
	return growth, nil
}

func (a *callbackAllocation) Release() {
	if a.owner != nil {
		a.owner.used -= a.bytes + uint64(unsafe.Sizeof(callbackAllocation{}))
		a.owner = nil
		a.bytes = 0
	}
}
