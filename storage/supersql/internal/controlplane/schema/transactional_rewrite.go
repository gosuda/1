package schema

import (
	"bytes"
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type GenerationValueValidator func(context.Context, model.TypeLayout, model.Value) error

func RewriteGenerationInTransaction(ctx context.Context, transaction *txn.Transaction, reader storage.SnapshotReader, snapshotTS model.Timestamp, plan *GenerationPlan, codec KeyCodec, limits BackfillScanLimits, validate GenerationValueValidator) error {
	return RewriteGenerationInTransactionWithRuntime(ctx, transaction, reader, snapshotTS, plan, codec, limits, validate, model.ExpressionRuntime{})
}

func RewriteGenerationInTransactionWithRuntime(ctx context.Context, transaction *txn.Transaction, reader storage.SnapshotReader, snapshotTS model.Timestamp, plan *GenerationPlan, codec KeyCodec, limits BackfillScanLimits, validate GenerationValueValidator, runtime model.ExpressionRuntime) (err error) {
	if transaction == nil || reader == nil || plan == nil || codec == nil || snapshotTS <= 0 {
		return errIndexRunnerOptions
	}
	source := plan.Source()
	columns := plan.SourceColumns()
	keys := plan.SourceKeyLayout()
	if len(columns) == 0 || len(keys) == 0 {
		return errJobDefinition
	}
	job := Job{TableID: source.ObjectID, Kind: RewriteGenerationJob, State: JobBackfilling, SnapshotTS: snapshotTS, ProtectedTimestamp: snapshotTS}
	scan, err := NewTableKeyScan(ctx, reader, codec, codec, job, source, columns, keys, limits)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scan.Close()) }()
	columnIDs := make([]model.ColumnID, len(columns))
	for i := range columns {
		columnIDs[i] = columns[i].ID
	}
	request := storage.ReadRequest{Target: source, ColumnIDs: columnIDs, Layout: columns}
	targetColumns := make([]model.ColumnID, len(plan.targetStoredLayout))
	for i := range plan.targetStoredLayout {
		targetColumns[i] = plan.targetStoredLayout[i].ID
	}
	targetRequest := storage.ReadRequest{Target: plan.Target(), ColumnIDs: targetColumns, Layout: plan.targetStoredLayout}
	return transaction.WithMutationResolver(ctx, plan, func(ctx context.Context, applier txn.MutationApplier) error {
		for {
			batch, complete, err := scan.Next(ctx)
			if err != nil {
				return err
			}
			for _, key := range batch {
				request.Key = key
				row, err := applier.Get(ctx, request)
				if err != nil {
					if errors.Is(err, storage.ErrRowNotFound) {
						continue
					}
					return err
				}
				if row.Deleted || !bytes.Equal(row.Key, key) {
					continue
				}
				next, err := plan.transformWithRuntime(ctx, &row, runtime)
				if err != nil {
					return err
				}
				if next == nil {
					continue
				}
				if len(next.Values) != len(plan.targetLayout) {
					return model.ErrInvalidDescriptor
				}
				if validate != nil {
					for i, layout := range plan.targetLayout {
						declared := model.TypeLayout{Type: layout.Type, Children: layout.Children}
						if err := validate(ctx, declared, next.Values[i]); err != nil {
							return err
						}
					}
				}
				targetRequest.Key = next.Key
				existing, err := applier.Get(ctx, targetRequest)
				if err == nil && !existing.Deleted {
					return &model.Error{Code: "23505", Message: "type cascade rewrite produced a duplicate table key"}
				}
				if err != nil && !errors.Is(err, storage.ErrRowNotFound) {
					return err
				}
				if err := plan.expand(ctx, applier, nil, next); err != nil {
					return err
				}
			}
			if complete {
				return nil
			}
		}
	})
}
