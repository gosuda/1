package txn

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// UpdatePreparation borrows a complete frozen OLD row and partial assignments
// only during preparation. Allocator and MaxBytes admit the sealed NEW image.
type UpdatePreparation struct {
	Target      storage.ObjectRef
	Previous    *model.Row
	ColumnIDs   []model.ColumnID
	Values      []model.Value
	Allocator   columnar.Allocator
	MaxBytes    uint64
	Expressions model.ExpressionRuntime
}

// UpdateCandidate seals both the original identity and completed NEW image.
// Copies share single-use and Close state; the caller must Close after use.
type UpdateCandidate struct {
	update struct{}
	state  *rowCandidateState
}

func (candidate *UpdateCandidate) Close() error {
	if candidate == nil || candidate.state == nil {
		return nil
	}
	return candidate.state.close()
}

// CopyRow returns independent payloads, valid after Close. The caller owns and
// accounts for the returned bytes until discarding the row.
func (candidate *UpdateCandidate) CopyRow(ctx context.Context, maxBytes uint64) (model.Row, uint64, error) {
	if candidate == nil || candidate.state == nil {
		return model.Row{}, 0, storage.ErrInvalidMutation
	}
	return candidate.state.copyRow(ctx, maxBytes)
}

func (transaction *Transaction) PrepareUpdate(ctx context.Context, preparation UpdatePreparation) (*UpdateCandidate, error) {
	if preparation.Previous == nil {
		return nil, storage.ErrInvalidMutation
	}
	state, err := transaction.prepareRow(ctx, InsertPreparation{
		Target: preparation.Target, ColumnIDs: preparation.ColumnIDs, Values: preparation.Values,
		Allocator: preparation.Allocator, MaxBytes: preparation.MaxBytes,
		Expressions: preparation.Expressions,
	}, preparation.Previous)
	if err != nil {
		return nil, err
	}
	return &UpdateCandidate{state: state}, nil
}

func (transaction *Transaction) ApplyPreparedUpdate(ctx context.Context, candidate *UpdateCandidate) error {
	if candidate == nil || candidate.state == nil || candidate.state.operation != storage.MutationUpdate {
		return storage.ErrInvalidMutation
	}
	return transaction.applyPreparedRow(ctx, candidate.state)
}
