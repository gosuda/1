package runtime

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
)

type PoolConfig struct {
	// Zero selects GOMAXPROCS, twice parallelism, and 1024 groups respectively.
	Parallelism   int
	QueueCapacity int
	MaxGroups     int
}

// Pool is owned by one database. Close cancels groups, drains the bounded queue,
// and joins workers and caller-goroutine tasks. Tasks must honor their context.
type Pool struct {
	mu          sync.Mutex
	queue       chan *job
	slots       chan struct{}
	workers     sync.WaitGroup
	groups      map[*Group]struct{}
	maxGroups   int
	maxActive   int
	parallelism int
	closed      bool
	closeOnce   sync.Once
	closeErr    error
}

func NewPool(config PoolConfig) (*Pool, error) {
	if config.Parallelism < 0 || config.QueueCapacity < 0 || config.MaxGroups < 0 {
		return nil, ErrInvalidConfig
	}
	if config.Parallelism == 0 {
		config.Parallelism = goruntime.GOMAXPROCS(0)
	}
	maxInt := int(^uint(0) >> 1)
	if config.Parallelism > maxInt/2 {
		return nil, ErrInvalidConfig
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = 2 * config.Parallelism
	}
	if config.QueueCapacity > maxInt-config.Parallelism {
		return nil, ErrInvalidConfig
	}
	if config.MaxGroups == 0 {
		config.MaxGroups = 1024
	}
	p := &Pool{queue: make(chan *job, config.QueueCapacity), slots: make(chan struct{}, config.QueueCapacity), groups: make(map[*Group]struct{}), maxGroups: config.MaxGroups, maxActive: config.Parallelism + config.QueueCapacity, parallelism: config.Parallelism}
	for range config.QueueCapacity {
		p.slots <- struct{}{}
	}
	for range config.Parallelism {
		p.workers.Add(1)
		go func() {
			defer p.workers.Done()
			for task := range p.queue {
				p.slots <- struct{}{}
				task.execute()
			}
		}()
	}
	return p, nil
}

// Task ownership transfers even on rejected submission. Cleanup runs exactly
// once after Run (or rejection), so it can release captured reservations/pins.
// A task must not wait for work queued to the same saturated pool; use Run for
// nested short work. Run and Cleanup must not call their own Group.Close.
type Task struct {
	Run     func(context.Context) error
	Cleanup func() error
}

func (t Task) cleanup(err error) error {
	if t.Cleanup != nil {
		return errors.Join(err, t.Cleanup())
	}
	return err
}

type Group struct {
	pool       *Pool
	ctx        context.Context
	cancel     context.CancelCauseFunc
	mu         sync.Mutex
	sealed     bool
	active     int
	producers  int
	pending    sync.WaitGroup
	first      error
	finishOnce sync.Once
}

func (p *Pool) NewGroup(ctx context.Context) (*Group, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if len(p.groups) == p.maxGroups {
		return nil, resourceError("53200", "admit query task group", ErrBudget)
	}
	groupCtx, cancel := context.WithCancelCause(ctx)
	g := &Group{pool: p, ctx: groupCtx, cancel: cancel}
	p.groups[g] = struct{}{}
	return g, nil
}

func (g *Group) Context() context.Context { return g.ctx }

// Future.Wait cancellation only abandons the wait. Cancel requests task
// cancellation; Wait must still join it before its resources can be reused.
type Future struct {
	done   chan struct{}
	cancel context.CancelCauseFunc
	err    error
}

func (f *Future) Wait(ctx context.Context) error {
	select {
	case <-f.done:
		return f.err
	default:
	}
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (f *Future) Cancel(cause error) { f.cancel(cause) }

type job struct {
	group  *Group
	ctx    context.Context
	task   Task
	future *Future
}

func (g *Group) begin(task Task) (*job, error) {
	if task.Run == nil {
		return nil, fmt.Errorf("%w: task function required", ErrInvalidConfig)
	}
	if g.sealed {
		return nil, ErrClosed
	}
	if g.ctx.Err() != nil {
		return nil, context.Cause(g.ctx)
	}
	if g.active == g.pool.maxActive {
		return nil, resourceError("53200", "admit query task", ErrBudget)
	}
	ctx, cancel := context.WithCancelCause(g.ctx)
	f := &Future{done: make(chan struct{}), cancel: cancel}
	g.active++
	g.pending.Add(1)
	return &job{group: g, ctx: ctx, task: task, future: f}, nil
}

// Submit applies bounded queue backpressure, not memory-quota waiting. ctx
// governs admission only; admitted work uses Group.Context and Future.Cancel.
func (g *Group) Submit(ctx context.Context, task Task) (*Future, error) {
	return g.submit(ctx, task, true)
}

func (g *Group) TrySubmit(ctx context.Context, task Task) (*Future, error) {
	return g.submit(ctx, task, false)
}

func (g *Group) submit(ctx context.Context, task Task, wait bool) (*Future, error) {
	// Register producers before waiting so Close joins rejected cleanup too.
	g.mu.Lock()
	if g.sealed || g.ctx.Err() != nil {
		g.mu.Unlock()
		return nil, task.cleanup(errors.Join(ErrClosed, context.Cause(g.ctx)))
	}
	if g.producers == g.pool.maxActive {
		g.mu.Unlock()
		return nil, task.cleanup(resourceError("53200", "admit query producer", ErrBudget))
	}
	g.producers++
	g.pending.Add(1)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.producers--
		g.mu.Unlock()
		g.pending.Done()
	}()
	if ctx.Err() != nil {
		return nil, task.cleanup(context.Cause(ctx))
	}
	if wait {
		select {
		case <-g.pool.slots:
		case <-ctx.Done():
			return nil, task.cleanup(context.Cause(ctx))
		case <-g.ctx.Done():
			return nil, task.cleanup(context.Cause(g.ctx))
		}
	} else {
		select {
		case <-g.pool.slots:
		default:
			return nil, task.cleanup(ErrQueueFull)
		}
	}
	p := g.pool
	p.mu.Lock()
	g.mu.Lock()
	var next *job
	var err error
	if p.closed {
		err = ErrClosed
	} else if ctx.Err() != nil {
		err = context.Cause(ctx)
	} else {
		next, err = g.begin(task)
	}
	if err == nil {
		p.queue <- next
	}
	g.mu.Unlock()
	p.mu.Unlock()
	if err != nil {
		p.slots <- struct{}{}
		return nil, task.cleanup(err)
	}
	return next.future, nil
}

// Run executes on the caller goroutine and still participates in group joining.
// Scheduling thresholds and volatile/serial semantics belong to the executor.
func (g *Group) Run(ctx context.Context, task Task) error {
	p := g.pool
	p.mu.Lock()
	g.mu.Lock()
	var next *job
	var err error
	if p.closed {
		err = ErrClosed
	} else if ctx.Err() != nil {
		err = context.Cause(ctx)
	} else {
		next, err = g.begin(task)
	}
	g.mu.Unlock()
	p.mu.Unlock()
	if err != nil {
		return task.cleanup(err)
	}
	stop := context.AfterFunc(ctx, func() { next.future.Cancel(context.Cause(ctx)) })
	defer stop()
	next.execute()
	return next.future.err
}

func (j *job) execute() {
	err := context.Cause(j.ctx)
	if err == nil {
		err = j.task.Run(j.ctx)
	}
	err = j.task.cleanup(err)
	j.task = Task{}
	j.future.cancel(nil)
	j.future.err = err
	g := j.group
	g.mu.Lock()
	g.active--
	if err != nil && g.first == nil {
		// Keep one causal chain, not an ever-growing history of failed tasks.
		g.first = err
		g.cancel(err)
	}
	g.mu.Unlock()
	close(j.future.done)
	g.pending.Done()
}

// Wait seals admission and joins all admitted tasks/producers. It returns the
// first task error (including that task's cleanup); each future keeps its own.
func (g *Group) Wait() error {
	g.finishOnce.Do(func() {
		g.mu.Lock()
		g.sealed = true
		g.mu.Unlock()
		g.pending.Wait()
		g.pool.mu.Lock()
		delete(g.pool.groups, g)
		g.pool.mu.Unlock()
	})
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.first
}

func (g *Group) Close() error {
	g.cancel(ErrClosed)
	return g.Wait()
}

func (p *Pool) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		groups := make([]*Group, 0, len(p.groups))
		for group := range p.groups {
			group.cancel(ErrClosed)
			groups = append(groups, group)
		}
		close(p.queue)
		p.mu.Unlock()
		for _, group := range groups {
			// All task errors remain available from their futures/groups.
			if err := group.Wait(); p.closeErr == nil {
				p.closeErr = err
			}
		}
		p.workers.Wait()
	})
	return p.closeErr
}
