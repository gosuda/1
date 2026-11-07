// Package exec executes the explicitly supported subset of local fragment plans.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	exexchange "github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	exrecursive "github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exset "github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	exwindow "github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

var ErrInvalid = errors.New("invalid execution invocation")
var ErrClosed = errors.New("execution portal closed")
var ErrCapability = errors.New("invocation scan capability differs from physical plan")

// RequiredExecutionError identifies an implementation gap, not a SQL exclusion.
type RequiredExecutionError struct {
	Node      physical.NodeID
	Algorithm physical.Algorithm
	Reason    string
}

func (e *RequiredExecutionError) Error() string {
	return fmt.Sprintf("required execution missing: node %d algorithm %d: %s", e.Node, e.Algorithm, e.Reason)
}

type Limits struct {
	Expressions    expr.Limits
	BatchRows      int
	BatchBytes     uint64
	ParameterBytes uint64
	MetadataBytes  uint64
}

// Environment transfers Query and Reader to Open, including on failure. The
// graph, function resolver and key encoder remain borrowed and immutable. Parameter
// values and layouts are copied; the caller may reuse them after Open returns.
// Query must be dedicated to this invocation. Sort supplies tuning only: Open
// replaces its Query, Layouts, Keys, Encoder, BatchRows and BatchBytes fields.
// Exchange supplies MaxKeyBytes and WorkBytes only; its schema, keys, mapping,
// queue budget and runtime collaborators come from the bound physical node.
type Environment struct {
	// Query supplies this invocation's memory and parallelism, and is the
	// invocation's own by default: the portal closes it when releasing, even on
	// failure. BorrowQuery retains the caller's query instead, leaving the
	// lifetime with whoever created it, which is required when the caller
	// reuses one query across statements.
	Query           *runtime.Query
	BorrowQuery     bool
	Instrumentation *Instrumentation
	// Reader supplies this invocation's snapshot. It is owned by default: the
	// portal closes it when releasing. BorrowReader retains the caller's reader
	// instead, leaving its lifetime with whoever opened it, which is required
	// when a nested statement runs against a view the enclosing frame owns.
	Reader       storage.SnapshotReader
	BorrowReader bool
	SampleSeed   uint64
	Functions       expr.Resolver
	Sequences       model.SequenceRuntime
	Relations       model.RelationRuntime
	Temporal        model.TemporalRuntime
	Settings        model.SettingsRuntime
	TextSearch      model.TextSearchRuntime
	Advisory        model.AdvisoryLockRuntime
	Native          model.NativeScalarRuntime
	Triggers        model.TriggerRuntime
	SessionIntrospection model.SessionIntrospection
	Catalog         catalogrel.Scanner
	Builtin         builtin.ResolveOptions
	CopySource      builtin.RowSourcePlan
	Keys            storage.KeyEncoder
	Parameters      []expr.ParameterValue
	Limits          Limits
	row             []model.Value
	Spool           rowstore.Limits
	Sort            exsort.Options
	Join            exjoin.Options
	Aggregate       exaggregate.Options
	Set             exset.Options
	Window          exwindow.Options
	Modify          exmodify.Options
	MutationTargets MutationTargets
	Apply           exapply.Options
	Recursive       exrecursive.Options
	Exchange        exexchange.HashOptions
}

// Portal serializes Next and Close. Published batches are borrowed until either
// method is called again; a caller must finish reading a batch before Close.
// Close cancels an in-flight Next before waiting for its mutable state.
// A begun write portal holds its transaction's statement scope until EOF or
// Close; callers must not independently use that transaction in the meantime.
type Portal struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	stopQuery        func() bool
	root             storage.BatchCursor
	env              Environment
	owned            []io.Closer
	producers        []*sharedProducer
	claims           []columnar.Allocation
	terminal         error
	closed           bool
	closeErr         error
	mutation         exmodify.Cursor
	mutations        mutationScope
	mandatory        []*sharedProducer
	driver           *mutationDriver
	rowLocks         bool
	affected         uint64
	borrowed         bool
	queryScalars     []queryScalarEntry
	queryScalarBytes uint64
}

func Open(ctx context.Context, graph *fragment.FragmentGraph, env Environment) (_ *Portal, err error) {
	p := &Portal{env: env}
	p.mutations.transaction = env.Modify.Transaction
	p.ctx, p.cancel = context.WithCancel(ctx)
	if env.Query != nil {
		p.stopQuery = context.AfterFunc(env.Query.Context(), p.cancel)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()
	if env.Query == nil || graph == nil || graph.Plan() == nil || !validLimits(env.Limits) {
		return nil, ErrInvalid
	}
	if err = p.ctx.Err(); err != nil {
		return nil, err
	}
	if env.Instrumentation != nil {
		if err = env.Instrumentation.bind(graph.Plan()); err != nil {
			return nil, err
		}
	}
	if err = preflight(graph, env); err != nil {
		return nil, fmt.Errorf("execution preflight: %w", err)
	}
	if err = p.copyParameters(); err != nil {
		return nil, fmt.Errorf("execution parameters: %w", err)
	}
	compiler := invocation{portal: p, graph: graph, plan: graph.Plan()}
	if err = compiler.prepare(); err != nil {
		return nil, fmt.Errorf("execution preparation: %w", err)
	}
	p.root, err = compiler.build()
	if err != nil {
		return nil, fmt.Errorf("execution construction: %w", err)
	}
	return p, nil
}

func validLimits(l Limits) bool {
	e := l.Expressions
	if e.MaxDepth > 64 || e.MaxRows > 1<<31-1 || e.MaxInstructions > 1<<31-1 || e.MaxRegisters > 1<<31-1 {
		return false
	}
	return l.BatchRows > 0 && l.BatchRows <= e.MaxRows && l.BatchBytes > 0 && l.ParameterBytes > 0 && l.MetadataBytes > 0 && e.MaxInstructions > 0 && e.MaxRegisters > 0 && e.MaxDepth > 0 && e.MaxRows > 0 && e.MaxBytes > 0 && e.MaxConstantBytes > 0 && e.MaxStateBytes > 0 && e.MaxOutputBytes > 0
}

func (p *Portal) Next(ctx context.Context, out *columnar.Batch) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if out == nil {
		return ErrInvalid
	}
	out.Reset()
	if p.closed {
		return ErrClosed
	}
	if p.terminal != nil {
		return p.terminal
	}
	if out.MaxRows < 0 {
		return ErrInvalid
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
	request := out.MaxRows
	if out.MaxRows == 0 || out.MaxRows > p.env.Limits.BatchRows {
		out.MaxRows = p.env.Limits.BatchRows
	}
	var err error
	if p.mutations.transaction != nil && (p.rowLocks || len(p.mandatory) != 0 || p.mutation != nil) {
		err = p.mutationNext(call, out)
	} else {
		err = p.root.Next(call, out)
	}
	if p.mutation != nil {
		p.affected = p.mutation.RowsAffected()
	}
	out.MaxRows = request
	if err != nil {
		out.Reset()
		return p.finish(err)
	}
	return nil
}

// RowsAffected reports successfully staged mutations, not a durable commit.
func (p *Portal) RowsAffected() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.affected
}

func (p *Portal) finish(err error) error {
	p.cancel()
	if p.driver != nil {
		if grouped := p.closeMutationDriver(); grouped != nil {
			err = errors.Join(err, grouped)
		}
		p.driver = nil
	}
	cleanup := p.releaseResources()
	p.closeErr = errors.Join(p.closeErr, cleanup)
	if err == io.EOF && cleanup == nil {
		p.terminal = io.EOF
	} else {
		p.terminal = errors.Join(err, cleanup)
		p.affected = 0
	}
	return p.terminal
}

func (p *Portal) closeCursors() error {
	var err error
	for i := len(p.owned) - 1; i >= 0; i-- {
		err = errors.Join(err, p.owned[i].Close())
	}
	p.owned = nil
	p.root = nil
	p.mutation = nil
	return err
}

func (p *Portal) Close() error {
	if !p.mu.TryLock() {
		p.cancel()
		p.mu.Lock()
	}
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	if p.driver != nil {
		p.closeErr = errors.Join(p.closeErr, p.closeMutationDriver())
	}
	p.cancel()
	p.closeErr = errors.Join(p.closeErr, p.releaseResources())
	return p.closeErr
}

func (p *Portal) releaseResources() error {
	if p.stopQuery != nil {
		p.stopQuery()
		p.stopQuery = nil
	}
	err := p.closeCursors()
	// Producer inputs can retain borrowed portals that still pin scalar caches.
	for i := len(p.producers) - 1; i >= 0; i-- {
		err = errors.Join(err, p.producers[i].closeInput())
	}
	err = errors.Join(err, p.closeQueryScalars())
	for i := len(p.producers) - 1; i >= 0; i-- {
		err = errors.Join(err, p.producers[i].Close())
	}
	p.producers = nil
	p.mandatory = nil
	if !p.borrowed && !p.env.BorrowReader && p.env.Reader != nil {
		err = errors.Join(err, p.env.Reader.Close())
		p.env.Reader = nil
	}
	p.env.Parameters = nil
	p.env.row = nil
	for _, claim := range p.claims {
		claim.Release()
	}
	p.claims = nil
	if !p.borrowed && p.env.Instrumentation != nil {
		if p.env.Query != nil {
			p.env.Instrumentation.observeUsage(p.env.Query.Usage())
		}
		defer p.env.Instrumentation.finish()
	}
	if !p.borrowed && !p.env.BorrowQuery && p.env.Query != nil {
		err = errors.Join(err, p.env.Query.Close())
		p.env.Query = nil
	}
	p.env = Environment{}
	return err
}
