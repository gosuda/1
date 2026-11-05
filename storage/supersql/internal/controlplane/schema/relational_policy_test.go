package schema

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type barrierConstraintDeferral struct {
	arrived chan struct{}
	release chan struct{}
	fail    error
}

func (d *barrierConstraintDeferral) DeferConstraint(ctx context.Context, _ txn.DeferredConstraint) (bool, error) {
	select {
	case d.arrived <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-d.release:
		return d.fail == nil, d.fail
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

type barrierConstraintEvent struct {
	validations atomic.Int32
}

func (e *barrierConstraintEvent) Identity() txn.ConstraintIdentity {
	return txn.ConstraintIdentity{Object: 1, Constraint: 2, Schema: "public", Name: "fk", Deferrable: true}
}
func (e *barrierConstraintEvent) RetainedBytes() uint64 { return 1 }
func (e *barrierConstraintEvent) Validate(context.Context, txn.ConstraintTransaction) error {
	e.validations.Add(1)
	return nil
}

func TestConstraintDeferralTransfersOnlyAfterBarrierDecision(t *testing.T) {
	for _, failure := range []error{nil, errors.New("queue admission failed")} {
		deferral := &barrierConstraintDeferral{arrived: make(chan struct{}, 2), release: make(chan struct{}), fail: failure}
		event := &barrierConstraintEvent{}
		execution := relationalExecution{deferral: deferral}
		results := make(chan error, 2)
		var release sync.Once
		t.Cleanup(func() { release.Do(func() { close(deferral.release) }) })
		for range 2 {
			go func() { results <- execution.deferOrValidate(t.Context(), modelConstraintForBarrier(), event) }()
		}
		<-deferral.arrived
		<-deferral.arrived
		release.Do(func() { close(deferral.release) })
		for range 2 {
			err := <-results
			if !errors.Is(err, failure) {
				t.Fatalf("defer result = %v, want %v", err, failure)
			}
		}
		if event.validations.Load() != 0 {
			t.Fatalf("transferred or failed event was validated %d times", event.validations.Load())
		}
	}
}

func modelConstraintForBarrier() model.ConstraintDescriptor {
	return model.ConstraintDescriptor{ID: 2, Name: "fk", Kind: model.ConstraintForeignKey, Deferrable: true}
}
