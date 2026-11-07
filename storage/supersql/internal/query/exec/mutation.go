package exec

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/modify"
)

// MutationTarget owns the mutation capabilities for one logical relation and
// its recursively reachable inheritance children.
type MutationTarget struct {
	Descriptor           *model.Descriptor
	Expansion            modify.Expansion
	PartitionChildren    []MutationTarget
	PartitionExpressions []txn.ValueExpression
	// PartitionRouting owns the top-level declarative partition tree used when
	// the SQL target names an attached partition directly.
	PartitionRouting *MutationTarget
}

type MutationTargets interface {
	ResolveMutationTarget(context.Context, storage.ObjectRef) (MutationTarget, error)
}

func (target MutationTarget) ResolveMutationTarget(ctx context.Context, ref storage.ObjectRef) (MutationTarget, error) {
	if err := ctx.Err(); err != nil {
		return MutationTarget{}, err
	}
	logical := ref.Generation == 0 && ref.EncodingVersion == 0
	if !logical {
		if err := ref.Validate(); err != nil {
			return MutationTarget{}, err
		}
	} else if ref.ObjectID == 0 || ref.DescriptorVersion == 0 {
		return MutationTarget{}, storage.ErrInvalidTarget
	}
	if target.Descriptor == nil {
		return MutationTarget{}, ErrInvalid
	}
	if logical && target.Descriptor.Kind() != model.ObjectView {
		return MutationTarget{}, storage.ErrInvalidTarget
	}
	physical := target.Descriptor.Physical()
	want := storage.ObjectRef{ObjectID: target.Descriptor.ID(), DescriptorVersion: target.Descriptor.Version(), Generation: physical.Generation, EncodingVersion: physical.EncodingVersion}
	if ref != want {
		return MutationTarget{}, storage.ErrInvalidTarget
	}
	return target, nil
}

// mutationScope borrows the driver's callback token, never the constructor's
// context. Calls remain serialized by the portal, including scalar invocations.
type mutationScope struct {
	transaction modify.Transaction
	active      context.Context
}

func (scope *mutationScope) call(ctx context.Context) (context.Context, func(), error) {
	if scope.active == nil {
		return nil, nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	call, cancel := context.WithCancel(scope.active)
	stop := context.AfterFunc(ctx, cancel)
	return call, func() { stop(); cancel() }, nil
}

func (scope *mutationScope) Statement(ctx context.Context, run func(context.Context) error) error {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return run(call)
}

func (scope *mutationScope) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return model.Row{}, err
	}
	defer finish()
	return scope.transaction.Get(call, request)
}
func (scope *mutationScope) ReadView(ctx context.Context, dependencies []txn.ReadDependency) (txn.ReadView, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	transaction, ok := scope.transaction.(txn.ConstraintTransaction)
	if !ok {
		return nil, ErrInvalid
	}
	return transaction.ReadView(call, dependencies)
}

func (scope *mutationScope) ApplyMutations(ctx context.Context, mutations []storage.Mutation) error {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return scope.transaction.ApplyMutations(call, mutations)
}

func (scope *mutationScope) PrepareInsert(ctx context.Context, preparation txn.InsertPreparation) (*txn.InsertCandidate, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	return scope.transaction.PrepareInsert(call, preparation)
}

func (scope *mutationScope) ApplyPreparedInsert(ctx context.Context, candidate *txn.InsertCandidate) error {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return scope.transaction.ApplyPreparedInsert(call, candidate)
}

func (scope *mutationScope) PrepareUpdate(ctx context.Context, preparation txn.UpdatePreparation) (*txn.UpdateCandidate, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	return scope.transaction.PrepareUpdate(call, preparation)
}

func (scope *mutationScope) ApplyPreparedUpdate(ctx context.Context, candidate *txn.UpdateCandidate) error {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return scope.transaction.ApplyPreparedUpdate(call, candidate)
}

func (scope *mutationScope) LockRow(ctx context.Context, target storage.ObjectRef, key []byte, strength txn.RowLockStrength, wait txn.RowLockWait) (bool, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	return scope.transaction.LockRow(call, target, key, strength, wait)
}
func (scope *mutationScope) LockRows(ctx context.Context, requests []txn.RowLockRequest, wait txn.RowLockWait) (bool, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return false, err
	}
	defer finish()
	return scope.transaction.LockRows(call, requests, wait)
}
func (scope *mutationScope) ConstraintDefaultValues(ctx context.Context, target storage.ObjectRef, columns []model.ColumnID, expressions model.ExpressionRuntime, maxBytes uint64) ([]model.Value, error) {
	call, finish, err := scope.call(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	transaction, ok := scope.transaction.(txn.ConstraintTransaction)
	if !ok {
		return nil, ErrInvalid
	}
	return transaction.ConstraintDefaultValues(call, target, columns, expressions, maxBytes)
}

type mutationRequest struct {
	ctx        context.Context
	out        *columnar.Batch
	writesOnly bool
}

type mutationDriver struct {
	requests chan mutationRequest
	results  chan error
	done     chan struct{}
	err      error
	panicked any
}

func (p *Portal) mutationNext(ctx context.Context, out *columnar.Batch) error {
	return p.mutationRequest(mutationRequest{ctx: ctx, out: out})
}

func (p *Portal) mutationRequest(request mutationRequest) error {
	if p.driver == nil {
		p.driver = &mutationDriver{requests: make(chan mutationRequest), results: make(chan error), done: make(chan struct{})}
		go p.runMutations(p.driver)
	}
	select {
	case p.driver.requests <- request:
	case <-p.driver.done:
		return p.driver.result()
	}
	select {
	case err := <-p.driver.results:
		return err
	case <-p.driver.done:
		return p.driver.result()
	}
}

func (p *Portal) runMutations(driver *mutationDriver) {
	defer func() {
		if value := recover(); value != nil {
			p.affected = 0
			driver.panicked = value
			driver.err = errors.Join(ErrInvalid, p.releaseResources())
		}
		close(driver.done)
	}()
	terminal := io.EOF
	driver.err = p.mutations.transaction.Statement(p.ctx, func(active context.Context) error {
		p.mutations.active = active
		defer func() { p.mutations.active = nil }()
		started := false
		for {
			request := <-driver.requests
			if request.out == nil && !request.writesOnly {
				return p.releaseResources()
			}
			call, finish, err := p.mutations.call(request.ctx)
			if err == nil && !started {
				started = true
				for _, producer := range p.mandatory {
					if err = producer.complete(call); err != nil {
						break
					}
				}
			}
			if err == nil {
				if request.writesOnly {
					if p.mutation != nil {
						err = p.mutation.Complete(call)
					}
				} else {
					err = p.root.Next(call, request.out)
				}
			}
			if finish != nil {
				finish()
			}
			if p.mutation != nil {
				p.affected = p.mutation.RowsAffected()
			}
			if err != nil {
				terminal = err
				cleanup := p.releaseResources()
				if err == io.EOF {
					return cleanup
				}
				return errors.Join(err, cleanup)
			}
			driver.results <- nil
		}
	})
	if driver.err != nil {
		p.affected = 0
		driver.err = errors.Join(driver.err, p.releaseResources())
	} else {
		driver.err = terminal
	}
}

func (p *Portal) closeMutationDriver() error {
	select {
	case p.driver.requests <- mutationRequest{}:
	case <-p.driver.done:
	}
	<-p.driver.done
	if p.driver.err == io.EOF {
		return nil
	}
	return p.driver.result()
}

func (driver *mutationDriver) result() error {
	if driver.panicked != nil {
		panic(driver.panicked)
	}
	return driver.err
}

// FinishWrites completes statement effects without demanding SELECT output.
// The grouped statement remains open until Close or normal result exhaustion.
func (p *Portal) FinishWrites(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.terminal != nil {
		if p.terminal == io.EOF {
			return nil
		}
		return p.terminal
	}
	if len(p.mandatory) == 0 && p.mutation == nil {
		return nil
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	defer cancel()
	if err := p.ctx.Err(); err != nil {
		return p.finish(err)
	}
	if err := call.Err(); err != nil {
		return p.finish(err)
	}
	if err := p.mutationRequest(mutationRequest{ctx: call, writesOnly: true}); err != nil {
		err = p.finish(err)
		if err == io.EOF {
			return nil
		}
		return err
	}
	return nil
}
