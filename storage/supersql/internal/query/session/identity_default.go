package session

import (
	"context"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

type identityValueExpression struct {
	oid         model.OID
	cast        builtin.Callable
	narrow      bool
	resultBytes uint64
}

func (p *identityValueExpression) OwnedBytes() uint64 {
	return uint64(unsafe.Sizeof(identityValueExpression{})) + p.cast.OwnedBytes()
}

func (c *ColumnExpressionCompiler) CompileIdentityDefault(ctx context.Context, spec schema.IdentityDefaultSpec) (txn.ValueExpression, error) {
	if c == nil {
		return nil, expr.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.Sequence == 0 || spec.Table == 0 || spec.Column == 0 {
		return nil, expr.ErrInvalid
	}
	if err := spec.Result.Validate(); err != nil {
		return nil, err
	}
	integer := spec.Result.Type.ID == 20 || spec.Result.Type.ID == 21 || spec.Result.Type.ID == 23
	if !integer || spec.Result.Type.Storage != model.TypeInt64 {
		return nil, &model.Error{Code: "22023", Message: "identity column requires an integer type"}
	}
	header := uint64(unsafe.Sizeof(identityValueExpression{}))
	if header > c.options.Evaluation.MaxBytes {
		return nil, expr.ErrLimit
	}
	published, err := c.snapshot.Descriptor(ctx, spec.Sequence)
	if err != nil {
		return nil, err
	}
	descriptor := published.Descriptor
	sequence, ok := descriptor.Sequence()
	if !ok || descriptor.State() != model.StatePublic {
		return nil, &model.Error{Code: "55000", Message: "identity sequence is unavailable"}
	}
	if sequence.OwnerTable != spec.Table || sequence.OwnerColumn != spec.Column {
		return nil, &model.Error{Code: "55000", Message: "identity sequence ownership differs from column"}
	}
	program := &identityValueExpression{oid: descriptor.OID(), narrow: spec.Result.Type.ID != 20, resultBytes: c.options.ResultBytes}
	if program.narrow {
		cast, ok := builtin.LookupCast(20, spec.Result.Type.ID)
		if !ok || cast.Function == 0 {
			return nil, expr.ErrInvalid
		}
		program.cast, err = builtin.Resolve(cast.Function, c.options.Builtin)
		if err != nil {
			return nil, err
		}
	}
	if program.cast.OwnedBytes() > c.options.Evaluation.MaxBytes-header {
		return nil, expr.ErrLimit
	}
	return program, nil
}
func (p *identityValueExpression) Evaluate(ctx context.Context, row []model.Value, runtime model.ExpressionRuntime) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if row != nil {
		return model.Value{}, expr.ErrInvalid
	}
	if runtime.Sequences == nil {
		return model.Value{}, &model.Error{Code: "55000", Message: "identity default requires sequence runtime"}
	}
	next, err := runtime.Sequences.NextSequence(ctx, p.oid)
	if err != nil {
		return model.Value{}, err
	}
	value := model.Int64Val(next)
	if p.narrow {
		args := [1]model.Value{value}
		value, err = p.cast.Call(ctx, args[:], builtin.CallBudget{OutputBytes: int64(min(p.resultBytes, uint64(1<<63-1))), MemoryBytes: p.resultBytes})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return model.Value{}, err
	}
	return value, nil
}
