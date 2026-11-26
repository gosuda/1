package schema

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
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
	if err := source.Validate(); err != nil {
		return err
	}
	if limits.BatchRows <= 0 || limits.MaxKeyBytes == 0 || limits.WorkBytes == 0 || limits.MaxKeyBytes > uint64(math.MaxInt)/16 {
		return ErrJobLimit
	}
	for i, key := range keys {
		if key.ColumnID == 0 || i > 0 && key.ColumnID == keys[i-1].ColumnID {
			return storage.ErrInvalidLayout
		}
		position, ok := slices.BinarySearchFunc(columns, key.ColumnID, func(column storage.ColumnLayout, id model.ColumnID) int {
			switch {
			case column.ID < id:
				return -1
			case column.ID > id:
				return 1
			default:
				return 0
			}
		})
		if !ok || !storage.KeyLayoutsEqual([]storage.KeyColumn{{Type: columns[position].Type, Children: columns[position].Children}}, []storage.KeyColumn{{Type: key.Type, Children: key.Children}}) {
			return storage.ErrInvalidLayout
		}
	}
	columnIDs := make([]model.ColumnID, len(columns))
	for i := range columns {
		columnIDs[i] = columns[i].ID
	}
	scanRequest := storage.PushdownRequest{
		Target:    source,
		Layout:    columns,
		KeyLayout: keys,
		ColumnIDs: columnIDs,
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}
	scanPlan, err := reader.PlanScan(ctx, scanRequest)
	if err != nil {
		return err
	}
	wrongSnapshot := scanPlan.SnapshotTS != snapshotTS || scanPlan.Request.Target != scanRequest.Target
	filtered := !scanPlan.Accepted.Empty() || !scanPlan.Residual.Empty() || scanPlan.AppliedLimit.Enabled
	wrongOutput := !slices.Equal(scanPlan.Projection, columnIDs) || len(scanPlan.Ordering) != len(keys)
	if wrongSnapshot || filtered || wrongOutput {
		return storage.ErrInvalidScanPlan
	}
	for i, key := range keys {
		if scanPlan.Ordering[i] != (storage.ScanOrdering{KeyOrdinal: uint32(i), Type: key.Type, Descending: key.Descending, NullsFirst: key.NullsFirst}) {
			return storage.ErrInvalidScanPlan
		}
	}
	expected, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: scanRequest, SnapshotTS: snapshotTS}, limits.WorkBytes)
	if err != nil {
		return err
	}
	actual, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: scanPlan.Request, SnapshotTS: scanPlan.SnapshotTS}, limits.WorkBytes)
	if err != nil {
		return err
	}
	if expected != actual || scanPlan.Binding == ([32]byte{}) {
		return storage.ErrInvalidScanPlan
	}
	cursor, err := reader.VectorizedScan(ctx, scanPlan)
	if err != nil {
		return err
	}
	if cursor == nil {
		return storage.ErrInvalidScanPlan
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	targetColumns := make([]model.ColumnID, len(plan.targetStoredLayout))
	for i := range plan.targetStoredLayout {
		targetColumns[i] = plan.targetStoredLayout[i].ID
	}
	targetRequest := storage.ReadRequest{Target: plan.Target(), ColumnIDs: targetColumns, Layout: plan.targetStoredLayout}
	rowBudget := max(plan.rowBytes, limits.WorkBytes)
	seenLimit := max(limits.WorkBytes, 64<<20)
	return transaction.WithMutationResolver(ctx, plan, func(ctx context.Context, applier txn.MutationApplier) error {
		batch := columnar.Batch{Columns: make([]columnar.Vector, 0, len(columns))}
		values := make([]model.Value, len(columns))
		seenKeys := make(map[string]struct{})
		var seenBytes uint64
		seenOverflow := false
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			batch.Reset()
			batch.MaxRows = limits.BatchRows
			nextErr := cursor.Next(ctx, &batch)
			if nextErr == io.EOF {
				return nil
			}
			if nextErr != nil {
				return nextErr
			}
			if err := batch.Validate(max(batch.Length, batch.MaxRows)); err != nil {
				return err
			}
			if batch.Rows() > batch.MaxRows || len(batch.Columns) != len(columns) {
				return columnar.ErrInvalidBatch
			}
			for lane := 0; lane < batch.Rows(); lane++ {
				if err := ctx.Err(); err != nil {
					return err
				}
				rowLane := batch.Lane(lane)
				remaining := rowBudget
				for i, column := range columns {
					vector := &batch.Columns[i]
					if vector.Type != column.Type {
						return columnar.ErrValueLayout
					}
					value, used, err := columnar.CopyValue(ctx, vector, rowLane, remaining)
					if err != nil {
						return err
					}
					values[i] = value
					remaining -= used
				}
				row := model.Row{Values: values}
				next, err := plan.transformWithRuntime(ctx, &row, runtime)
				clear(values)
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
				if !seenOverflow {
					if _, dup := seenKeys[string(next.Key)]; dup {
						return &model.Error{Code: "23505", Message: "type cascade rewrite produced a duplicate table key"}
					}
					entryBytes := uint64(len(next.Key)) + 32
					if entryBytes <= seenLimit-seenBytes {
						seenKeys[string(next.Key)] = struct{}{}
						seenBytes += entryBytes
					} else {
						seenOverflow = true
						seenKeys = nil
					}
				}
				if seenOverflow {
					targetRequest.Key = next.Key
					existing, err := applier.Get(ctx, targetRequest)
					if err == nil && !existing.Deleted {
						return &model.Error{Code: "23505", Message: "type cascade rewrite produced a duplicate table key"}
					}
					if err != nil && !errors.Is(err, storage.ErrRowNotFound) {
						return err
					}
				}
				if err := plan.expand(ctx, applier, nil, next); err != nil {
					return err
				}
			}
		}
	})
}
