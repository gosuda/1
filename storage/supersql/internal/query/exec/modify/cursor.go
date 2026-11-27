package modify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var errTriggerSkipped = errors.New("row skipped by BEFORE trigger")

type viewMutationState struct {
	view          model.ObjectID
	inputColumns  []int
	outputColumns []model.ColumnID
	layouts       []model.TypeLayout
	projection    *expr.State
	predicate     *expr.State
	input         *columnar.Builder
	inputValues   []model.Value
	outputValues  [2][]model.Value
	check         bool
}

type immediateConstraint struct {
	event txn.DeferredConstraint
	claim columnar.Allocation
}

type cursor struct {
	options                  Options
	input                    storage.BatchCursor
	ctx                      context.Context
	cancel                   context.CancelFunc
	stopQuery                func() bool
	claim                    columnar.Allocation
	expansionClaim           columnar.Allocation
	descriptor               *model.Descriptor
	target                   storage.ObjectRef
	metadata                 uint64
	columnIDs                []model.ColumnID
	generatedColumns         []bool
	tableLayouts             []model.TypeLayout
	visibleColumns           []int
	visibleLayouts           []model.TypeLayout
	oldImage, newImage       []model.Value
	returnLayouts            []model.TypeLayout
	oldTableOID              model.OID
	newTableOID              model.OID
	keyColumns               []int
	assignmentColumns        []int
	assignment               *expr.State
	returning                *expr.State
	mergeActions             []mergeActionState
	viewMutationSteps        []viewMutationState
	viewRows                 [2]model.Row
	partitionViewRows        [2]model.Row
	partitionViewValues      [2][]model.Value
	mutatedTargets           *distinctset.Set
	conflictState            mergeActionState
	conflictInput            *columnar.Builder
	rlsInsertCheck           *expr.State
	rlsUpdateCheck           *expr.State
	rlsConflictCheck         *expr.State
	rlsCheckInput            *columnar.Builder
	assignmentInput          *columnar.Builder
	returnInput              *columnar.Builder
	output                   *columnar.Builder
	frozen                   *rowstore.Spool
	returns                  *rowstore.Spool
	reader                   *rowstore.Reader
	returnReader             *rowstore.Reader
	row                      []model.Value
	keyValues                []model.Value
	returnValues             []model.Value
	lastKey                  []byte
	lastObject               model.ObjectID
	pending                  []model.Value
	immediateConstraints     []immediateConstraint
	immediateConstraintBytes uint64
	staged                   bool
	inputErr                 error
	affected                 uint64
	closed                   bool
	closeErr                 error
}

func (c *cursor) check(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if c.ctx != nil {
		if err := context.Cause(c.ctx); err != nil {
			return err
		}
	}
	if c.options.Query != nil {
		return context.Cause(c.options.Query.Context())
	}
	return nil
}

func (c *cursor) rowPayloadBytes() uint64 {
	return max(c.options.MaxRecordBytes*8, c.options.MaxRowBytes)
}

func (c *cursor) spool(ctx context.Context, layouts []model.TypeLayout) (*rowstore.Spool, error) {
	o := c.options
	return rowstore.New(ctx, o.Query, layouts, rowstore.Limits{MemoryBytes: o.SpoolMemoryBytes, MaxBytes: o.MaxSpoolBytes, MaxRows: o.MaxRows, MaxRecordBytes: o.MaxRecordBytes, MaxLargeRecordBytes: o.MaxRowBytes, MetadataBytes: c.metadata, DecodeBytes: o.MaxRecordBytes * 8, MaxReaders: 1})
}

func copyBatchRow(ctx context.Context, batch *columnar.Batch, lane int, values []model.Value, maximum uint64) error {
	for i := range values {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], lane, maximum)
		if err != nil {
			return err
		}
		values[i], maximum = value, maximum-used
	}
	return nil
}

func (c *cursor) key(ctx context.Context, row []model.Value, columns []int) ([]byte, error) {
	for i, column := range columns {
		value := row[column]
		if value.IsNull {
			return nil, &model.Error{Code: "23502", Message: "primary key cannot be null"}
		}
		c.keyValues[i] = value
	}
	return c.options.Encoder.EncodePrefix(ctx, c.options.KeyLayout, c.keyValues, c.options.MaxRecordBytes)
}
func (c *cursor) partitionKey(ctx context.Context, child *partitionChild, row []model.Value, columns []int) ([]byte, error) {
	for i, column := range columns {
		value := row[column]
		if value.IsNull {
			return nil, &model.Error{Code: "23502", Message: "primary key cannot be null"}
		}
		c.keyValues[i] = value
	}
	return c.options.Encoder.EncodePrefix(ctx, child.keyLayout, c.keyValues, c.options.MaxRecordBytes)
}

func (c *cursor) get(ctx context.Context, key []byte) (model.Row, error) {
	return c.options.Transaction.Get(ctx, storage.ReadRequest{Target: c.target, Key: key, ColumnIDs: c.columnIDs, Layout: c.options.Layout})
}

// Freeze completes source reads and CRUD deduplication before assignments can
// move a primary key. MERGE retains every candidate and its original match class.
func (c *cursor) freeze(ctx context.Context) error {
	layouts := make([]model.TypeLayout, len(c.row))
	copy(layouts, c.options.InputLayouts)
	copy(layouts[len(c.options.InputLayouts):], c.tableLayouts)
	if c.options.Partitions != nil && c.options.Partitions.sourceImage {
		layouts[len(layouts)-1] = model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	}
	var err error
	c.frozen, err = c.spool(ctx, layouts)
	if err != nil {
		return err
	}
	inputWidth := len(c.options.InputLayouts)
	var count uint64
	var sourceImage bytes.Buffer
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		batch := columnar.Batch{MaxRows: c.options.BatchRows}
		err := c.input.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			// PostgreSQL inserts COPY rows as it reads them, so a stream failure
			// aborts the statement only after the rows already decoded have been
			// staged and their triggers run. Freeze spools ahead of staging, so
			// the failure must be deferred until the spool is applied rather than
			// discarding the rows that were read before the error.
			c.inputErr = err
			break
		}
		if err := batch.Validate(c.options.BatchRows); err != nil {
			return err
		}
		if len(batch.Columns) != inputWidth {
			return columnar.ErrValueLayout
		}
		for i, layout := range c.options.InputLayouts {
			if !batch.Columns[i].MatchesLayout(layout) {
				return columnar.ErrValueLayout
			}
		}
		for lane := range batch.Rows() {
			if count == c.options.MaxRows {
				return resource("modify input row limit")
			}
			count++
			if err := copyBatchRow(ctx, &batch, batch.Lane(lane), c.row[:inputWidth], c.rowPayloadBytes()); err != nil {
				return err
			}
			targetPresent := c.options.Operation != storage.MutationInsert
			if c.options.Merge != nil {
				match, err := c.mergeMatch(c.row[:inputWidth])
				if err != nil {
					return err
				}
				targetPresent = match != ir.NotMatchedByTarget
			}
			if logicalMutationTarget(c.options.TargetKind) {
				if !targetPresent {
					for i, layout := range c.tableLayouts {
						c.row[inputWidth+i] = model.NullVal(layout.Type.Storage)
					}
				} else {
					for i, column := range c.options.LogicalOldColumns {
						c.row[inputWidth+i] = c.row[column]
					}
				}
				if _, err := c.frozen.AppendRow(ctx, c.row); err != nil {
					return err
				}
				continue
			}
			if !targetPresent {
				for i, layout := range c.tableLayouts {
					c.row[inputWidth+i] = model.NullVal(layout.Type.Storage)
				}
				if c.options.Partitions != nil && c.options.Partitions.sourceImage {
					c.row[len(c.row)-1] = model.NullVal(model.TypeBytes)
				}
			} else {
				var source *partitionChild
				var key []byte
				var object model.ObjectID
				if c.options.Partitions != nil && c.options.SourceObjectColumn != 0 {
					index := int(c.options.SourceObjectColumn) - 1
					if index < 0 || index >= inputWidth || c.row[index].IsNull || c.row[index].Type != model.TypeInt64 || c.row[index].IntVal <= 0 {
						return storage.ErrInvalidLayout
					}
					object = model.ObjectID(c.row[index].IntVal)
					source, err = c.options.Partitions.source(object)
					if err != nil {
						return err
					}
					key, err = c.partitionKey(ctx, source, c.row, c.options.OldKeyColumns)
				} else {
					key, err = c.key(ctx, c.row, c.options.OldKeyColumns)
				}
				if err != nil {
					return err
				}
				if c.options.Merge == nil && object == c.lastObject && bytes.Equal(c.lastKey, key) {
					continue
				}
				c.lastKey, c.lastObject = key, object
				request := storage.ReadRequest{Target: c.target, Key: key, ColumnIDs: c.columnIDs, Layout: c.options.Layout}
				if source != nil {
					request.Target, request.ColumnIDs, request.Layout = source.target, source.columnIDs, source.layout
				}
				old, err := c.options.Source.Get(ctx, request)
				if err != nil {
					return err
				}
				expected := len(c.tableLayouts)
				if source != nil {
					expected = len(source.layout)
				}
				if old.Deleted || len(old.Values) != expected {
					return storage.ErrInvalidLayout
				}
				if source != nil {
					if _, err := c.options.Partitions.Project(source, old.Values, c.row[inputWidth:inputWidth+len(c.tableLayouts)]); err != nil {
						return err
					}
					if c.options.Partitions.sourceImage {
						sourceImage.Reset()
						if err := storage.EncodeValue(ctx, &sourceImage, model.Value{Type: model.TypeComposite, ArrayVal: old.Values}, c.rowPayloadBytes()); err != nil {
							return err
						}
						c.row[len(c.row)-1] = model.BytesVal(sourceImage.Bytes())
					}
				} else {
					copy(c.row[inputWidth:inputWidth+len(c.tableLayouts)], old.Values)
				}
			}
			if _, err := c.frozen.AppendRow(ctx, c.row); err != nil {
				return err
			}
		}
	}
	input := c.input
	c.input = nil
	if err := input.Close(); err != nil {
		return err
	}
	c.lastKey = nil
	c.lastObject = 0
	clear(c.row)
	if err := c.frozen.Seal(ctx); err != nil {
		return err
	}
	c.reader, err = c.frozen.Reader(ctx)
	return err
}

func (c *cursor) assignments(ctx context.Context, input []model.Value, state *expr.State, columns []int) ([]model.Value, error) {
	if state == nil && len(columns) == 0 {
		return nil, nil
	}
	c.assignmentInput.Reset()
	if err := c.assignmentInput.Append(ctx, input); err != nil {
		return nil, err
	}
	var batch, output columnar.Batch
	if err := c.assignmentInput.Publish(&batch); err != nil {
		return nil, err
	}
	if err := state.Eval(ctx, &batch, c.options.Parameters, &output, expr.Direct); err != nil {
		return nil, err
	}
	values := c.row[:len(columns)]
	if output.Rows() != 1 || len(output.Columns) != len(values) {
		return nil, storage.ErrInvalidLayout
	}
	rowLimit := c.rowPayloadBytes()
	remaining := rowLimit
	for i := range values {
		value, used, err := columnar.CopyValue(ctx, &output.Columns[i], output.Lane(0), remaining)
		if err != nil {
			return nil, err
		}
		values[i], remaining = value, remaining-used
	}
	if rowLimit-remaining > c.options.MaxRecordBytes {
		if err := state.ReleaseOutput(); err != nil {
			return nil, err
		}
		if c.assignmentInput != nil && c.assignmentInput != c.conflictInput {
			if err := c.assignmentInput.Close(); err != nil {
				return nil, err
			}
			var err error
			c.assignmentInput, err = columnar.NewBuilder(c.options.InputLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: c.options.BatchBytes, Allocator: c.options.Query.Allocator(runtime.WorkMemory)})
			if err != nil {
				return nil, err
			}
		}
	}
	return values, nil
}

func (c *cursor) mutate(ctx context.Context, frozen []model.Value, operation storage.MutationOp, assignment *expr.State, assignmentColumns []int, columnIDs []model.ColumnID) (bool, error) {
	o := &c.options
	inputWidth := len(o.InputLayouts)
	oldEnd := inputWidth + len(c.tableLayouts)
	if oldEnd > len(frozen) {
		return false, storage.ErrInvalidLayout
	}
	old := frozen[inputWidth:oldEnd]
	var source *partitionChild
	var oldKey []byte
	var assigned []model.Value
	var err error
	if operation != storage.MutationInsert && physicalMutationTarget(o.TargetKind) {
		if o.Partitions != nil && o.SourceObjectColumn != 0 {
			index := int(o.SourceObjectColumn) - 1
			if index < 0 || index >= inputWidth || frozen[index].IsNull || frozen[index].Type != model.TypeInt64 || frozen[index].IntVal <= 0 {
				return false, storage.ErrInvalidLayout
			}
			source, err = o.Partitions.source(model.ObjectID(frozen[index].IntVal))
			if err == nil {
				oldKey, err = c.partitionKey(ctx, source, old, c.keyColumns)
			}
		} else {
			oldKey, err = c.key(ctx, old, c.keyColumns)
		}
		if err != nil {
			return false, err
		}
	}
	physicalOld := old
	if source != nil && o.Partitions.sourceImage {
		if oldEnd >= len(frozen) || frozen[oldEnd].IsNull || frozen[oldEnd].Type != model.TypeBytes {
			return false, storage.ErrInvalidLayout
		}
		decoded, decodeErr := storage.DecodeValue(ctx, frozen[oldEnd].BytesVal, c.rowPayloadBytes())
		if decodeErr != nil || decoded.Type != model.TypeComposite || len(decoded.ArrayVal) != len(source.layout) {
			return false, errors.Join(decodeErr, storage.ErrInvalidLayout)
		}
		defer clear(decoded.ArrayVal)
		physicalOld = decoded.ArrayVal
	}
	if operation != storage.MutationDelete {
		assigned, err = c.assignments(ctx, frozen[:inputWidth], assignment, assignmentColumns)
		if err != nil {
			return false, err
		}
	}
	if logicalMutationTarget(o.TargetKind) {
		previous := model.Row{Values: old}
		if operation == storage.MutationDelete {
			replacement, proceed, triggerErr := o.Triggers.BeforeRow(ctx, c.descriptor, model.TriggerDelete, &previous, nil, c.rowPayloadBytes())
			if triggerErr != nil {
				return false, triggerErr
			}
			if !proceed {
				return false, nil
			}
			if replacement == nil || len(replacement.Values) != len(previous.Values) {
				return false, storage.ErrInvalidLayout
			}
			defer clear(replacement.Values)
			if err := c.finishMutation(ctx, frozen, operation, replacement.Values, nil, nil); err != nil {
				return false, err
			}
			return true, nil
		}
		current := model.Row{Values: make([]model.Value, len(old))}
		copy(current.Values, old)
		defer clear(current.Values)
		for i, column := range assignmentColumns {
			current.Values[column] = assigned[i]
		}
		replacement, proceed, triggerErr := o.Triggers.BeforeRow(ctx, c.descriptor, model.TriggerUpdate, &previous, &current, c.rowPayloadBytes())
		if triggerErr != nil {
			return false, triggerErr
		}
		if !proceed {
			return false, nil
		}
		if replacement == nil || len(replacement.Values) != len(current.Values) {
			return false, storage.ErrInvalidLayout
		}
		defer clear(replacement.Values)
		if err := c.finishMutation(ctx, frozen, operation, old, nil, replacement); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := c.applyAssigned(ctx, frozen, operation, source, physicalOld, oldKey, assigned, columnIDs); err != nil {
		if errors.Is(err, errTriggerSkipped) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *cursor) applyAssigned(ctx context.Context, frozen []model.Value, operation storage.MutationOp, source *partitionChild, old []model.Value, oldKey []byte, assigned []model.Value, columnIDs []model.ColumnID) (err error) {
	o := &c.options
	target, descriptor := c.target, c.descriptor
	if source != nil {
		target, descriptor = source.target, source.descriptor
	}
	if operation == storage.MutationDelete {
		previous := &model.Row{Key: oldKey, Values: old}
		if o.Triggers != nil {
			_, proceed, triggerErr := o.Triggers.BeforeRow(ctx, descriptor, model.TriggerDelete, previous, nil, c.rowPayloadBytes())
			if triggerErr != nil {
				return triggerErr
			}
			if !proceed {
				return errTriggerSkipped
			}
		}
		mutation := storage.Mutation{Op: storage.MutationDelete, Target: target, Key: oldKey}
		if err := o.Transaction.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
			return err
		}
		return c.finishPartitionMutation(ctx, frozen, operation, source, old, oldKey, nil)
	}
	previous := model.Row{Key: oldKey, Values: old}
	if source != nil && o.Partitions.router != nil {
		descriptor = c.descriptor
	}
	mappedColumns := columnIDs
	if source != nil {
		mappedColumns, err = o.Partitions.mapColumns(source, columnIDs)
		if err != nil {
			return err
		}
	}
	rowLimit := c.rowPayloadBytes()
	candidate, err := o.Transaction.PrepareUpdate(ctx, txn.UpdatePreparation{Target: target, Previous: &previous, ColumnIDs: mappedColumns, Values: assigned, Allocator: o.Query.Allocator(runtime.WorkMemory), MaxBytes: rowLimit, Expressions: partitionExpressions(*o)})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, candidate.Close()) }()
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return err
	}
	defer claim.Release()
	current, used, err := candidate.CopyRow(ctx, rowLimit)
	if err != nil {
		return err
	}
	if err := claim.Resize(used); err != nil {
		return err
	}
	defer func() {
		clear(c.keyValues)
		clear(current.Values)
		current = model.Row{}
	}()
	if o.Triggers != nil {
		replacement, proceed, triggerErr := o.Triggers.BeforeRow(ctx, descriptor, model.TriggerUpdate, &previous, &current, rowLimit)
		if triggerErr != nil {
			return triggerErr
		}
		if !proceed {
			return errTriggerSkipped
		}
		if replacement == nil || len(replacement.Values) != len(current.Values) {
			return storage.ErrInvalidLayout
		}
		if err := candidate.Close(); err != nil {
			return err
		}
		fullColumns, updateValues := c.columnIDs, replacement.Values
		if source != nil {
			fullColumns, updateValues = filterNonGeneratedColumns(source.columns, source.columnIDs, updateValues)
		} else {
			fullColumns, updateValues = filterNonGeneratedMask(c.generatedColumns, fullColumns, updateValues)
		}
		candidate, err = o.Transaction.PrepareUpdate(ctx, txn.UpdatePreparation{Target: target, Previous: &previous, ColumnIDs: fullColumns, Values: updateValues, Allocator: o.Query.Allocator(runtime.WorkMemory), MaxBytes: rowLimit, Expressions: partitionExpressions(*o)})
		if err != nil {
			return err
		}
		current, used, err = candidate.CopyRow(ctx, rowLimit)
		if err != nil {
			return err
		}
		if err := claim.Resize(used); err != nil {
			return err
		}
	}
	if o.Conflict != nil && o.Conflict.Action == ir.ConflictUpdate && !bytes.Equal(oldKey, current.Key) {
		if err := c.recordConflictIdentity(ctx, current.Values); err != nil {
			return err
		}
	}
	if operation == storage.MutationUpdate && c.rlsUpdateCheck != nil {
		if err := c.checkRLS(ctx, c.rlsUpdateCheck, source, current.Values, false); err != nil {
			return err
		}
	}
	if source != nil && o.Partitions.router != nil {
		destination, routeErr := o.Partitions.Route(ctx, current.Values, partitionExpressions(*o))
		if routeErr != nil {
			return routeErr
		}
		if o.PartitionTarget != 0 && !o.Partitions.contains(destination, o.PartitionTarget) {
			return &model.Error{Code: "23514", Message: "new row violates partition constraint", Table: o.Descriptor.Name().Name}
		}
		if destination != source {
			return c.movePartitionRow(ctx, frozen, source, destination, &previous, &current)
		}
	}
	if err := o.Transaction.ApplyPreparedUpdate(ctx, candidate); err != nil {
		return err
	}
	return c.finishPartitionMutation(ctx, frozen, operation, source, old, oldKey, &current)
}

func (c *cursor) checkRLS(ctx context.Context, state *expr.State, route *partitionChild, row []model.Value, usingExpr bool) error {
	if state == nil {
		return nil
	}
	source := row
	if route != nil && c.options.Partitions != nil {
		buf := make([]model.Value, len(c.tableLayouts))
		projected, err := c.options.Partitions.Project(route, row, buf)
		if err != nil {
			return err
		}
		source = projected
	}
	visible := c.visibleRow(source, c.newImage)
	defer clear(c.newImage)
	c.rlsCheckInput.Reset()
	if err := c.rlsCheckInput.Append(ctx, []model.Value{{Type: model.TypeComposite, ArrayVal: visible}}); err != nil {
		return err
	}
	var batch, out columnar.Batch
	if err := c.rlsCheckInput.Publish(&batch); err != nil {
		return err
	}
	if err := state.Eval(ctx, &batch, c.options.Parameters, &out, expr.Direct); err != nil {
		return err
	}
	if out.Rows() != 1 || len(out.Columns) != 1 {
		return storage.ErrInvalidLayout
	}
	value, _, err := columnar.CopyValue(ctx, &out.Columns[0], out.Lane(0), c.options.MaxRecordBytes)
	if err != nil {
		return err
	}
	if value.IsNull || value.Type != model.TypeBoolean || value.IntVal == 0 {
		tableName := c.descriptor.Name().Name
		msg := fmt.Sprintf("new row violates row-level security policy for table %q", tableName)
		if usingExpr {
			msg = fmt.Sprintf("new row violates row-level security policy (USING expression) for table %q", tableName)
		}
		return &model.Error{
			Code:    "42501",
			Message: msg,
			Table:   tableName,
		}
	}
	return nil
}
func (c *cursor) movePartitionRow(ctx context.Context, frozen []model.Value, source, destination *partitionChild, previous, proposed *model.Row) (err error) {
	o := &c.options
	columnIDs, err := o.Partitions.mapColumns(destination, c.columnIDs)
	if err != nil {
		return err
	}
	insertColumns, insertValues := filterNonGeneratedColumns(destination.columns, columnIDs, proposed.Values)
	rowLimit := c.rowPayloadBytes()
	candidate, err := o.Transaction.PrepareInsert(ctx, txn.InsertPreparation{Target: destination.target, ColumnIDs: insertColumns, Values: insertValues, Allocator: o.Query.Allocator(runtime.WorkMemory), MaxBytes: rowLimit, Expressions: partitionExpressions(*o)})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, candidate.Close()) }()
	current, _, err := candidate.CopyRow(ctx, rowLimit)
	if err != nil {
		return err
	}
	defer clear(current.Values)
	if err := o.Transaction.ApplyMutations(ctx, []storage.Mutation{{Op: storage.MutationDelete, Target: source.target, Key: previous.Key}}); err != nil {
		return err
	}
	if err := o.Transaction.ApplyPreparedInsert(ctx, candidate); err != nil {
		return err
	}
	if source.expansion != nil {
		if err := source.expansion.Expand(ctx, o.Transaction, previous, nil); err != nil {
			return err
		}
	}
	if destination.expansion != nil {
		if err := destination.expansion.Expand(ctx, o.Transaction, nil, &current); err != nil {
			return err
		}
	}
	expressions := partitionExpressions(*o)
	if relational, ok := source.expansion.(RelationalExpansion); ok {
		if err := relational.ExpandRelational(ctx, o.ConstraintTransaction, c, o.Triggers, expressions, o.Query.Allocator(runtime.WorkMemory), rowLimit, previous, nil); err != nil {
			return err
		}
	}
	if relational, ok := destination.expansion.(RelationalExpansion); ok {
		if err := relational.ExpandRelational(ctx, o.ConstraintTransaction, c, o.Triggers, expressions, o.Query.Allocator(runtime.WorkMemory), rowLimit, nil, &current); err != nil {
			return err
		}
	}
	expansion := o.Expansion
	o.Expansion = nil
	oldTableOID, newTableOID := c.oldTableOID, c.newTableOID
	c.oldTableOID, c.newTableOID = source.descriptor.OID(), destination.descriptor.OID()
	defer func() {
		o.Expansion = expansion
		c.oldTableOID, c.newTableOID = oldTableOID, newTableOID
	}()
	return c.finishMutationWithView(ctx, frozen, storage.MutationUpdate, previous.Values, previous.Key, &current, previous.Values, &current, source, destination)
}

func (c *cursor) DeferConstraint(ctx context.Context, event txn.DeferredConstraint) (bool, error) {
	if event == nil {
		return false, runtime.ErrInvalidConfig
	}
	identity := event.Identity()
	if identity.Object == 0 || identity.Constraint == 0 || identity.Name == "" {
		return false, runtime.ErrInvalidConfig
	}
	if identity.Deferrable && c.options.Deferral != nil {
		deferred, err := c.options.Deferral.DeferConstraint(ctx, event)
		if err != nil || deferred {
			return deferred, err
		}
	}
	bytes := max(event.RetainedBytes(), uint64(128))
	if bytes > c.options.WorkBytes || c.immediateConstraintBytes > c.options.WorkBytes-bytes {
		return false, resource("admit immediate constraint event")
	}
	claim, err := c.options.Query.Allocator(runtime.WorkMemory).Reserve(bytes)
	if err != nil {
		return false, err
	}
	c.immediateConstraints = append(c.immediateConstraints, immediateConstraint{event: event, claim: claim})
	c.immediateConstraintBytes += bytes
	return true, nil
}

func (c *cursor) validateImmediateConstraints(ctx context.Context) error {
	for i := range c.immediateConstraints {
		pending := &c.immediateConstraints[i]
		if err := pending.event.Validate(ctx, c.options.ConstraintTransaction); err != nil {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && diagnostic.Constraint == "" {
				copy := *diagnostic
				copy.Constraint = pending.event.Identity().Name
				err = &copy
			}
			return err
		}
		pending.event = nil
		pending.claim.Release()
		pending.claim = nil
	}
	clear(c.immediateConstraints)
	c.immediateConstraints = c.immediateConstraints[:0]
	c.immediateConstraintBytes = 0
	return nil
}

func (c *cursor) discardImmediateConstraints() {
	for i := range c.immediateConstraints {
		c.immediateConstraints[i].event = nil
		if c.immediateConstraints[i].claim != nil {
			c.immediateConstraints[i].claim.Release()
		}
	}
	clear(c.immediateConstraints)
	c.immediateConstraints = nil
	c.immediateConstraintBytes = 0
}

func (c *cursor) finishPartitionMutation(ctx context.Context, frozen []model.Value, operation storage.MutationOp, route *partitionChild, old []model.Value, oldKey []byte, current *model.Row) error {
	if route == nil {
		return c.finishMutation(ctx, frozen, operation, old, oldKey, current)
	}
	descriptor, target, expansion, visible := c.descriptor, c.target, c.options.Expansion, c.visibleColumns
	c.descriptor, c.target, c.options.Expansion, c.visibleColumns = route.descriptor, route.target, route.expansion, route.visibleColumns
	err := c.finishMutationWithView(ctx, frozen, operation, old, oldKey, current, old, current, route, route)
	c.descriptor, c.target, c.options.Expansion, c.visibleColumns = descriptor, target, expansion, visible
	return err
}

func (c *cursor) finishMutation(ctx context.Context, frozen []model.Value, operation storage.MutationOp, old []model.Value, oldKey []byte, current *model.Row) error {
	return c.finishMutationWithView(ctx, frozen, operation, old, oldKey, current, old, current, nil, nil)
}

func (c *cursor) finishMutationWithView(ctx context.Context, frozen []model.Value, operation storage.MutationOp, old []model.Value, oldKey []byte, current *model.Row, viewOld []model.Value, viewCurrent *model.Row, viewOldRoute, viewCurrentRoute *partitionChild) error {
	o := &c.options
	if o.Expansion != nil {
		var previous *model.Row
		if operation != storage.MutationInsert {
			previous = &model.Row{Key: oldKey, Values: old}
		}
		if err := o.Expansion.Expand(ctx, o.Transaction, previous, current); err != nil {
			return err
		}
		if relational, ok := o.Expansion.(RelationalExpansion); ok {
			expressions := model.ExpressionRuntime{Sequences: o.Sequences, Relations: o.Relations, Temporal: o.Temporal, Settings: o.Settings, TextSearch: o.TextSearch, Advisory: o.Advisory, Native: o.Native}
			if err := relational.ExpandRelational(ctx, o.ConstraintTransaction, c, o.Triggers, expressions, o.Query.Allocator(runtime.WorkMemory), c.rowPayloadBytes(), previous, current); err != nil {
				return err
			}
		}
	}
	if o.Triggers != nil {
		var previous *model.Row
		if operation != storage.MutationInsert {
			previous = &model.Row{Key: oldKey, Values: old}
		}
		if err := o.Triggers.AfterRow(ctx, c.descriptor, triggerEvent(operation), previous, current, c.rowPayloadBytes()); err != nil {
			return err
		}
	}
	if viewOldRoute != nil && operation != storage.MutationInsert {
		previous := &model.Row{Key: oldKey, Values: viewOld}
		projected, err := c.partitionViewRow(viewOldRoute, previous, 0)
		if err != nil {
			return err
		}
		viewOld = projected.Values
		defer clear(c.partitionViewValues[0])
	}
	if viewCurrentRoute != nil && viewCurrent != nil {
		projected, err := c.partitionViewRow(viewCurrentRoute, viewCurrent, 1)
		if err != nil {
			return err
		}
		viewCurrent = projected
		defer clear(c.partitionViewValues[1])
	}
	var projectedCurrent *model.Row
	enforceViewCheck := false
	for i := range c.viewMutationSteps {
		enforceViewCheck = enforceViewCheck || c.viewMutationSteps[i].check
	}
	if viewCurrent != nil && enforceViewCheck {
		var err error
		projectedCurrent, _, err = c.projectViewRow(ctx, viewCurrent, true, c.returning != nil, 1)
		if err != nil {
			return err
		}
	}
	if c.returning == nil {
		return nil
	}
	var projectedOld *model.Row
	if len(c.viewMutationSteps) != 0 {
		var err error
		if operation != storage.MutationInsert {
			projectedOld, _, err = c.projectViewRow(ctx, &model.Row{Values: viewOld}, false, true, 0)
			if err != nil {
				return err
			}
		}
		if viewCurrent != nil && projectedCurrent == nil {
			projectedCurrent, _, err = c.projectViewRow(ctx, viewCurrent, false, true, 1)
			if err != nil {
				return err
			}
		}
	}
	images := c.returnValues
	defer clear(images)
	defer clear(c.oldImage)
	defer clear(c.newImage)
	images[0], images[1] = model.NullVal(model.TypeComposite), model.NullVal(model.TypeComposite)
	if operation != storage.MutationInsert {
		values := c.visibleRow(old, c.oldImage)
		if projectedOld != nil {
			values = projectedOld.Values
		}
		images[0] = model.Value{Type: model.TypeComposite, ArrayVal: values}
	}
	if current != nil {
		values := c.visibleRow(current.Values, c.newImage)
		if projectedCurrent != nil {
			values = projectedCurrent.Values
		}
		images[1] = model.Value{Type: model.TypeComposite, ArrayVal: values}
	}
	imageCount := 2
	if o.Merge != nil {
		imageCount = 4
		images[2] = images[1]
		switch operation {
		case storage.MutationInsert:
			images[3] = model.StringVal("INSERT")
		case storage.MutationUpdate:
			images[3] = model.StringVal("UPDATE")
		case storage.MutationDelete:
			images[2] = images[0]
			images[3] = model.StringVal("DELETE")
		}
	}
	if o.ReturningTableOIDs != 0 {
		oldTableOID, newTableOID := c.descriptor.OID(), c.descriptor.OID()
		if c.oldTableOID != 0 {
			oldTableOID = c.oldTableOID
		}
		if c.newTableOID != 0 {
			newTableOID = c.newTableOID
		}
		images[imageCount], images[imageCount+1] = model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)
		if operation != storage.MutationInsert {
			images[imageCount] = model.Int64Val(int64(oldTableOID))
		}
		if operation != storage.MutationDelete {
			images[imageCount+1] = model.Int64Val(int64(newTableOID))
		}
		if o.ReturningTableOIDs == 3 {
			images[imageCount+2] = images[imageCount+1]
			if operation == storage.MutationDelete {
				images[imageCount+2] = images[imageCount]
			}
		}
	}
	for i, column := range o.ReturningSourceColumns {
		images[i+imageCount+o.ReturningTableOIDs] = frozen[column]
	}
	c.returnInput.Reset()
	if err := c.returnInput.Append(ctx, images); err != nil {
		return err
	}
	var input, output columnar.Batch
	if err := c.returnInput.Publish(&input); err != nil {
		return err
	}
	if err := c.returning.Eval(ctx, &input, o.Parameters, &output, expr.Direct); err != nil {
		return err
	}
	if output.Rows() != 1 {
		return storage.ErrInvalidLayout
	}
	_, err := c.returns.AppendBatch(ctx, &output)
	return err
}

func (c *cursor) visibleRow(values, scratch []model.Value) []model.Value {
	if len(c.visibleColumns) == len(c.tableLayouts) {
		return values
	}
	for i, column := range c.visibleColumns {
		scratch[i] = values[column]
	}
	return scratch
}

func (c *cursor) releaseInput() error {
	var err error
	if c.input != nil {
		err = c.input.Close()
		c.input = nil
	}
	if c.reader != nil {
		err = errors.Join(err, c.reader.Close())
		c.reader = nil
	}
	if c.frozen != nil {
		err = errors.Join(err, c.frozen.Close())
		c.frozen = nil
	}
	if c.mutatedTargets != nil {
		err = errors.Join(err, c.mutatedTargets.Close())
		c.mutatedTargets = nil
	}
	for i := range c.viewMutationSteps {
		step := &c.viewMutationSteps[i]
		if step.projection != nil {
			err = errors.Join(err, step.projection.Close())
		}
		if step.predicate != nil {
			err = errors.Join(err, step.predicate.Close())
		}
		if step.input != nil {
			err = errors.Join(err, step.input.Close())
		}
		clear(step.inputValues)
		for j := range step.outputValues {
			clear(step.outputValues[j])
		}
	}
	c.viewMutationSteps = nil
	for i := range c.viewRows {
		clear(c.viewRows[i].Values)
		c.viewRows[i] = model.Row{}
	}
	for i := range c.partitionViewValues {
		clear(c.partitionViewValues[i])
		c.partitionViewValues[i] = nil
		c.partitionViewRows[i] = model.Row{}
	}
	for _, action := range c.mergeActions {
		if action.predicate != nil {
			err = errors.Join(err, action.predicate.Close())
		}
		if action.assignment != nil {
			err = errors.Join(err, action.assignment.Close())
		}
	}
	c.mergeActions = nil
	if c.conflictState.predicate != nil {
		err = errors.Join(err, c.conflictState.predicate.Close())
	}
	if c.conflictState.assignment != nil {
		err = errors.Join(err, c.conflictState.assignment.Close())
	}
	c.conflictState = mergeActionState{}
	return err
}
func (c *cursor) stage(ctx context.Context) (err error) {
	defer func() { err = errors.Join(err, c.releaseInput()) }()
	events := c.statementTriggerEvents()
	active := make([]bool, len(events))
	defer func() {
		if c.options.Triggers == nil {
			return
		}
		for i := len(active) - 1; i >= 0; i-- {
			if active[i] {
				c.options.Triggers.AbortStatement(c.descriptor, events[i])
			}
		}
	}()
	if c.options.Triggers != nil {
		for i, event := range events {
			updated := []model.ColumnID(nil)
			if event == model.TriggerUpdate {
				updated = c.options.UpdatedColumns
			}
			if err := c.options.Triggers.BeginStatement(ctx, c.descriptor, event, updated); err != nil {
				return err
			}
			active[i] = true
		}
	}
	if err := c.freeze(ctx); err != nil {
		return err
	}
	if c.returning != nil {
		c.returns, err = c.spool(ctx, c.returnLayouts)
		if err != nil {
			return err
		}
	}
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		_, row, err := c.reader.NextRow(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		mutated := true
		if c.options.Merge != nil {
			mutated, err = c.merge(ctx, row)
		} else if c.options.Operation == storage.MutationInsert {
			mutated, err = c.insert(ctx, row, c.assignment, c.assignmentColumns, c.options.AssignmentColumns)
		} else {
			mutated, err = c.mutate(ctx, row, c.options.Operation, c.assignment, c.assignmentColumns, c.options.AssignmentColumns)
		}
		if err != nil {
			return err
		}
		if mutated {
			c.affected++
		}
	}
	if c.inputErr != nil {
		return c.inputErr
	}
	if err := c.validateImmediateConstraints(ctx); err != nil {
		return err
	}
	if c.options.Triggers != nil {
		for i, event := range events {
			if err := c.options.Triggers.FinishStatement(ctx, c.descriptor, event); err != nil {
				return err
			}
			active[i] = false
		}
	}
	if c.returns != nil {
		if err := c.returns.Seal(ctx); err != nil {
			return err
		}
		c.returnReader, err = c.returns.Reader(ctx)
		if err != nil {
			return err
		}
	}
	return c.check(ctx)
}

func (c *cursor) statementTriggerEvents() []model.TriggerEvent {
	var present [5]bool
	add := func(event model.TriggerEvent) {
		if event != 0 {
			present[event] = true
		}
	}
	if c.options.Merge != nil {
		for _, action := range c.options.Merge.Actions {
			add(modifyTriggerEvent(action.Action))
		}
	} else {
		add(triggerEvent(c.options.Operation))
		if c.options.Conflict != nil && c.options.Conflict.Action == ir.ConflictUpdate {
			add(model.TriggerUpdate)
		}
	}
	events := make([]model.TriggerEvent, 0, 3)
	for _, event := range []model.TriggerEvent{model.TriggerInsert, model.TriggerUpdate, model.TriggerDelete} {
		if present[event] {
			events = append(events, event)
		}
	}
	return events
}

// RowsAffected excludes duplicate source matches and failed statements.
func (c *cursor) RowsAffected() uint64 {
	if !c.staged {
		return 0
	}
	return c.affected
}

func (c *cursor) Complete(ctx context.Context) (err error) {
	if c.closed {
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, c.Close())
		}
	}()
	if err := c.check(ctx); err != nil {
		return err
	}
	if c.staged {
		return nil
	}
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	if err := c.options.Transaction.Statement(operation, c.stage); err != nil {
		return err
	}
	c.staged = true
	return nil
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil {
		return columnar.ErrInvalidBatch
	}
	out.Reset()
	if c.closed {
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil && err != io.EOF {
			out.Reset()
			err = errors.Join(err, c.Close())
		}
	}()
	if out.MaxRows < 0 {
		return columnar.ErrInvalidBatch
	}
	if err := c.Complete(ctx); err != nil {
		return err
	}
	if c.returnReader == nil {
		return io.EOF
	}
	c.output.Reset()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for c.output.Rows() < limit {
		if c.pending == nil {
			_, row, readErr := c.returnReader.NextRow(ctx)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return readErr
			}
			c.pending = row
		}
		if appendErr := c.output.Append(ctx, c.pending); appendErr != nil {
			if errors.Is(appendErr, columnar.ErrBatchBudget) && c.output.Rows() > 0 {
				break
			}
			return appendErr
		}
		c.pending = nil
	}
	if c.output.Rows() == 0 {
		return io.EOF
	}
	return c.output.Publish(out)
}

func triggerEvent(operation storage.MutationOp) model.TriggerEvent {
	switch operation {
	case storage.MutationInsert:
		return model.TriggerInsert
	case storage.MutationUpdate:
		return model.TriggerUpdate
	case storage.MutationDelete:
		return model.TriggerDelete
	default:
		return 0
	}
}

func modifyTriggerEvent(operation ir.ModifyKind) model.TriggerEvent {
	switch operation {
	case ir.Insert:
		return model.TriggerInsert
	case ir.Update:
		return model.TriggerUpdate
	case ir.Delete:
		return model.TriggerDelete
	default:
		return 0
	}
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	if c.stopQuery != nil {
		c.stopQuery()
	}
	c.discardImmediateConstraints()
	c.closeErr = c.releaseInput()
	if c.returnReader != nil {
		c.closeErr = errors.Join(c.closeErr, c.returnReader.Close())
		c.returnReader = nil
	}
	if c.returns != nil {
		c.closeErr = errors.Join(c.closeErr, c.returns.Close())
		c.returns = nil
	}
	if c.assignment != nil {
		c.closeErr = errors.Join(c.closeErr, c.assignment.Close())
	}
	if c.returning != nil {
		c.closeErr = errors.Join(c.closeErr, c.returning.Close())
	}
	if c.rlsInsertCheck != nil {
		c.closeErr = errors.Join(c.closeErr, c.rlsInsertCheck.Close())
	}
	if c.rlsUpdateCheck != nil {
		c.closeErr = errors.Join(c.closeErr, c.rlsUpdateCheck.Close())
	}
	if c.rlsConflictCheck != nil {
		c.closeErr = errors.Join(c.closeErr, c.rlsConflictCheck.Close())
	}
	for _, builder := range []*columnar.Builder{c.assignmentInput, c.conflictInput, c.rlsCheckInput, c.returnInput, c.output} {
		if builder != nil {
			c.closeErr = errors.Join(c.closeErr, builder.Close())
		}
	}
	if c.claim != nil {
		c.claim.Release()
	}
	if c.expansionClaim != nil {
		c.expansionClaim.Release()
		c.expansionClaim = nil
	}
	c.row, c.keyValues, c.pending, c.lastKey = nil, nil, nil, nil
	c.returnValues = nil
	c.columnIDs, c.generatedColumns, c.tableLayouts, c.returnLayouts = nil, nil, nil, nil
	c.visibleColumns, c.visibleLayouts, c.oldImage, c.newImage = nil, nil, nil, nil
	c.keyColumns, c.assignmentColumns = nil, nil
	c.assignmentInput, c.rlsCheckInput, c.returnInput, c.output = nil, nil, nil, nil
	c.assignment, c.returning, c.rlsInsertCheck, c.rlsUpdateCheck, c.rlsConflictCheck, c.claim, c.descriptor = nil, nil, nil, nil, nil, nil, nil
	c.options = Options{}
	return c.closeErr
}

// filterNonGeneratedMask omits generated columns from a full-row column/value
// pair before passing it back to RowNormalizer, which rejects supplied
// generated columns and recomputes them from the supplied non-generated values.
func filterNonGeneratedMask(generated []bool, columnIDs []model.ColumnID, values []model.Value) ([]model.ColumnID, []model.Value) {
	if len(generated) == 0 || len(generated) != len(columnIDs) || len(columnIDs) != len(values) {
		return columnIDs, values
	}
	filteredIDs := make([]model.ColumnID, 0, len(columnIDs))
	filteredValues := make([]model.Value, 0, len(values))
	for i, id := range columnIDs {
		if generated[i] {
			continue
		}
		filteredIDs = append(filteredIDs, id)
		filteredValues = append(filteredValues, values[i])
	}
	return filteredIDs, filteredValues
}

// filterNonGeneratedColumns omits generated columns of the target descriptor
// from a mapped column/value pair before invoking RowNormalizer.
func filterNonGeneratedColumns(columns []model.ColumnDescriptor, columnIDs []model.ColumnID, values []model.Value) ([]model.ColumnID, []model.Value) {
	if len(columnIDs) != len(values) {
		return columnIDs, values
	}
	hasGenerated := false
	for _, column := range columns {
		if column.Generated != nil {
			hasGenerated = true
			break
		}
	}
	if !hasGenerated {
		return columnIDs, values
	}
	filteredIDs := make([]model.ColumnID, 0, len(columnIDs))
	filteredValues := make([]model.Value, 0, len(values))
	for i, id := range columnIDs {
		idx := slices.IndexFunc(columns, func(col model.ColumnDescriptor) bool { return col.ID == id })
		if idx >= 0 && columns[idx].Generated != nil {
			continue
		}
		filteredIDs = append(filteredIDs, id)
		filteredValues = append(filteredValues, values[i])
	}
	return filteredIDs, filteredValues
}
