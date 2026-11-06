package txn

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ConstraintIdentity is the stable catalog identity used by SET CONSTRAINTS.
// Schema and Name are retained so diagnostics never require a live catalog read.
type ConstraintIdentity struct {
	Object            model.ObjectID
	Constraint        model.ObjectID
	Schema            string
	Name              string
	Deferrable        bool
	InitiallyDeferred bool
}

// DeferredConstraint owns every value needed to recheck one constraint against
// the transaction's final overlay. Validate must not retain the transaction.
type DeferredConstraint interface {
	Identity() ConstraintIdentity
	RetainedBytes() uint64
	Validate(context.Context, ConstraintTransaction) error
}

// ConstraintDeferral decides the current SET CONSTRAINTS mode. A true result
// means ownership of event transferred to the transaction-end queue. A false
// result requires the caller to validate event before returning from the statement.
type ConstraintDeferral interface {
	DeferConstraint(context.Context, DeferredConstraint) (bool, error)
}

// ConstraintTransaction is the complete mutation and read surface used by
// relational constraint validation and referential actions.
type ConstraintTransaction interface {
	LockingMutationApplier
	ReadView(context.Context, []ReadDependency) (ReadView, error)
	PrepareInsert(context.Context, InsertPreparation) (*InsertCandidate, error)
	ApplyPreparedInsert(context.Context, *InsertCandidate) error
	PrepareUpdate(context.Context, UpdatePreparation) (*UpdateCandidate, error)
	ApplyPreparedUpdate(context.Context, *UpdateCandidate) error
	ConstraintDefaultValues(context.Context, storage.ObjectRef, []model.ColumnID, model.ExpressionRuntime, uint64) ([]model.Value, error)
}

func (transaction *Transaction) ConstraintDefaultValues(ctx context.Context, target storage.ObjectRef, columns []model.ColumnID, runtime model.ExpressionRuntime, maxBytes uint64) ([]model.Value, error) {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return nil, err
	}
	policy, err := transaction.target(operation, target)
	if err != nil {
		return nil, err
	}
	return policy.normalizer.constraintDefaults(operation, columns, runtime, maxBytes)
}
