package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type sequenceDDLBinding struct {
	definition     *bind.BoundSequenceDefinition
	ownershipError error
}

func soleSequenceOwnershipError(err error) *bind.SequenceOwnershipError {
	for err != nil {
		if ownership, ok := err.(*bind.SequenceOwnershipError); ok {
			return ownership
		}
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok {
			return nil
		}
		causes := joined.Unwrap()
		if len(causes) != 1 {
			return nil
		}
		err = causes[0]
	}
	return nil
}

func (t *Transactions) prepareAlterSequence(ctx context.Context, statement *zpgparse.AlterSeqStmt, options SequenceCommandOptions) (sequenceDDLBinding, error) {
	definition, err := t.bindSequenceDDL(ctx, nil, statement, options)
	if err == nil {
		return sequenceDDLBinding{definition: definition}, nil
	}
	ownership := soleSequenceOwnershipError(err)
	if ownership == nil {
		return sequenceDDLBinding{}, err
	}
	// Full syntax/budget checks have passed. This read-only projection omits just
	// ownership, allowing current-value validation before PostgreSQL's cache effect.
	allocation := &zpgparse.AlterSeqStmt{Sequence: statement.Sequence, ForIdentity: statement.ForIdentity, MissingOk: statement.MissingOk, Options: make([]*zpgparse.Node, 0, len(statement.Options))}
	for _, option := range statement.Options {
		if option.GetDefElem().Defname != "owned_by" {
			allocation.Options = append(allocation.Options, option)
		}
	}
	definition, err = t.bindSequenceDDL(ctx, nil, allocation, options)
	if err != nil {
		return sequenceDDLBinding{}, err
	}
	return sequenceDDLBinding{definition: definition, ownershipError: ownership}, nil
}
