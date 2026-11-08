package schema

import (
	"context"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// NewDefinedJob validates and fixes the complete immutable transformation before
// an intent can be persisted. The final publication is the third table version:
// delete-only and write-only each establish a fenced policy version first.
func NewDefinedJob(ctx context.Context, id JobID, mutation model.ObjectID, definition JobDefinition, maximum uint64) (Job, JobDefinition, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, JobDefinition{}, err
	}
	if id == 0 || mutation == 0 || definition.SourceTable == nil || definition.TargetTable == nil {
		return Job{}, JobDefinition{}, ErrJobRecord
	}
	source := definition.SourceTable
	if source.Version() > math.MaxUint64-3 || definition.TargetTable.Version() != source.Version()+3 {
		return Job{}, JobDefinition{}, ErrJobRecord
	}
	var kind JobKind
	switch definition.Operation {
	case SchemaAddColumn:
		kind = AddColumnJob
	case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey, SchemaReindex:
		kind = RewriteGenerationJob
	case SchemaDropIndex:
		kind = DropIndexJob
	default:
		return Job{}, JobDefinition{}, ErrJobRecord
	}
	job := Job{
		ID: id, TableID: source.ID(), MutationID: mutation, Kind: kind,
		State: JobPending, SourceVersion: source.Version(), TargetVersion: source.Version() + 3, Revision: 1,
	}
	definition.JobID = id
	definition.SourceIndexes = slices.Clone(definition.SourceIndexes)
	definition.TargetIndexes = slices.Clone(definition.TargetIndexes)
	definition.Transforms = slices.Clone(definition.Transforms)
	for i := range definition.Transforms {
		expression := definition.Transforms[i].Expression
		if expression == nil {
			continue
		}
		owned := *expression
		owned.SQL = string(append([]byte(nil), expression.SQL...))
		owned.Encoding = slices.Clone(expression.Encoding)
		owned.Dependencies = slices.Clone(expression.Dependencies)
		definition.Transforms[i].Expression = &owned
	}
	if err := validJob(job, maximum); err != nil {
		return Job{}, JobDefinition{}, err
	}
	if err := validateJobDefinition(ctx, job, definition, maximum); err != nil {
		return Job{}, JobDefinition{}, err
	}
	return job, definition, nil
}

func descriptorTarget(descriptor *model.Descriptor) (storage.ObjectRef, error) {
	if descriptor == nil {
		return storage.ObjectRef{}, model.ErrInvalidDescriptor
	}
	physical := descriptor.Physical()
	target := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
	return target, target.Validate()
}
