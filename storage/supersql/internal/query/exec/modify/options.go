// Package modify stages a complete SQL write statement before publishing RETURNING.
package modify

import (
	"context"
	"errors"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type Transaction interface {
	Statement(context.Context, func(context.Context) error) error
	txn.MutationApplier
	PrepareInsert(context.Context, txn.InsertPreparation) (*txn.InsertCandidate, error)
	ApplyPreparedInsert(context.Context, *txn.InsertCandidate) error
	PrepareUpdate(context.Context, txn.UpdatePreparation) (*txn.UpdateCandidate, error)
	ApplyPreparedUpdate(context.Context, *txn.UpdateCandidate) error
	LockRow(context.Context, storage.ObjectRef, []byte, txn.RowLockStrength, txn.RowLockWait) (bool, error)
	LockRows(context.Context, []txn.RowLockRequest, txn.RowLockWait) (bool, error)
}

// RowReader supplies the statement's fixed source and OLD-row snapshot.
type RowReader interface {
	Get(context.Context, storage.ReadRequest) (model.Row, error)
}

// Expansion stages descriptor-dependent writes inside the existing statement.
// Images are complete borrowed rows; nil denotes an absent image. Workspace is
// reserved before validation/staging, independently of transaction-owned memory.
type Expansion interface {
	ValidateDescriptor(context.Context, *model.Descriptor) error
	WorkspaceBytes() uint64
	Expand(context.Context, txn.MutationApplier, *model.Row, *model.Row) error
}

// RelationalExpansion extends physical index maintenance with relational
// constraints and referential actions in the same statement transaction.
type RelationalExpansion interface {
	ExpandRelational(context.Context, txn.ConstraintTransaction, txn.ConstraintDeferral, model.TriggerRuntime, model.ExpressionRuntime, columnar.Allocator, uint64, *model.Row, *model.Row) error
}

type ConflictLookup interface {
	FindConflict(context.Context, txn.LockingMutationApplier, *model.Row, []model.ConflictArbiter, uint64) (*model.ConflictOwner, error)
}

type ConflictPlan struct {
	Action            ir.ConflictAction
	Arbiters          []model.ConflictArbiter
	Predicate         *expr.Program
	Assignment        *expr.Program
	AssignmentColumns []model.ColumnID
}

type MergeAction struct {
	Match             ir.MatchKind
	Action            ir.ModifyKind
	Predicate         *expr.Program
	Assignment        *expr.Program
	AssignmentColumns []model.ColumnID
}

type MergePlan struct {
	SourcePresent int
	TargetPresent int
	Actions       []MergeAction
}

type ViewMutationStep struct {
	View          model.ObjectID
	InputColumns  []model.ColumnID
	OutputColumns []model.ColumnID
	Projection    *expr.Program
	Predicate     *expr.Program
	Check         bool
}

type Cursor interface {
	storage.BatchCursor
	// Complete stages all writes without consuming RETURNING rows.
	Complete(context.Context) error
	// RowsAffected is zero before successful staging and after failure.
	RowsAffected() uint64
}

// Programs, Query, Encoder, Transaction and Source are borrowed. All mutable metadata is
// copied under WorkBytes. Assignments consume InputLayouts; RETURNING consumes
// OLD and NEW composites, then MERGE's result composite and action text if present,
// followed by OLD/NEW and optional MERGE-result table OIDs, then
// ReturningSourceColumns. An absent image or OID is a typed SQL NULL.
// SpoolMemoryBytes may be zero; other byte/row limits must be positive.
// MaxSpoolBytes bounds each of the input and RETURNING spools. Sort limits are
// required only for UPDATE/DELETE. Next and Close are caller-serialized.
type Options struct {
	Operation   storage.MutationOp
	TargetKind  ir.ModifyTargetKind
	Merge       *MergePlan
	Conflict    *ConflictPlan
	Conflicts   ConflictLookup
	Transaction Transaction
	Source      RowReader
	Descriptor  *model.Descriptor
	Expansion   Expansion
	Partitions  *PartitionPlan
	// PartitionTarget is zero for a partition root and identifies a directly
	// named attached partition for constraint and arbiter mapping.
	PartitionTarget       model.ObjectID
	ConstraintTransaction txn.ConstraintTransaction
	Deferral              txn.ConstraintDeferral
	Layout                []storage.ColumnLayout
	KeyLayout             []storage.KeyColumn
	InputLayouts          []model.TypeLayout
	OldKeyColumns         []int
	// SourceObjectColumn is the one-based input column carrying the physical
	// relation identity for an inherited UPDATE or DELETE.
	SourceObjectColumn    uint32
	Assignment            *expr.Program
	LogicalOldColumns     []int
	AssignmentColumns     []model.ColumnID
	UpdatedColumns        []model.ColumnID
	ViewMutationSteps     []ViewMutationStep
	Returning             *expr.Program
	ReturningInputLayouts []model.TypeLayout
	// ReturningSourceColumns are strictly increasing indices into InputLayouts.
	ReturningSourceColumns []int
	// ReturningTableOIDs is zero for logical views, two for table writes, and
	// three for MERGE. OID layouts follow the RETURNING row-image layouts.
	ReturningTableOIDs int
	Parameters         []expr.ParameterValue
	Subqueries         expr.SubqueryRuntime
	Sequences          model.SequenceRuntime
	Relations          model.RelationRuntime
	Temporal           model.TemporalRuntime
	Settings           model.SettingsRuntime
	TextSearch         model.TextSearchRuntime
	Advisory           model.AdvisoryLockRuntime
	Native             model.NativeScalarRuntime
	Triggers           model.TriggerRuntime
	Query              *runtime.Query
	Encoder            storage.KeyEncoder
	BatchRows          int
	BatchBytes         uint64
	MaxRecordBytes     uint64
	SpoolMemoryBytes   uint64
	MaxSpoolBytes      uint64
	MaxRows            uint64
	WorkBytes          uint64
	SortRunBytes       uint64
	SortMaxRuns        int
	SortMergeFanIn     int
}

// ErrUnsupported reports a write target whose descriptor needs a constraint,
// index, trigger, or partition expansion the embedded writer does not
// implement. It carries the write_descriptor_expansion profile exclusion so the
// regression harness classifies the rejection instead of leaking it.
var ErrUnsupported = &model.Error{
	Code:      "0A000",
	Message:   bind.UnsupportedMessage(bind.WriteDescriptorExpansion),
	FeatureID: string(bind.WriteDescriptorExpansion),
}

func resource(message string) error {
	return &model.Error{Code: "53200", Message: message, Cause: runtime.ErrBudget}
}

func logicalMutationTarget(target ir.ModifyTargetKind) bool {
	return target == ir.LogicalViewModifyTarget || target == ir.AutomaticViewTriggerModifyTarget
}

func physicalMutationTarget(target ir.ModifyTargetKind) bool {
	return target == ir.PhysicalModifyTarget || target == ir.AutomaticViewModifyTarget
}

func automaticMutationTarget(target ir.ModifyTargetKind) bool {
	return target == ir.AutomaticViewModifyTarget || target == ir.AutomaticViewTriggerModifyTarget
}

// New takes ownership of input on every outcome, without pulling it. It never
// commits: successful staging remains owned by the caller's transaction.
func New(ctx context.Context, input storage.BatchCursor, options Options) (Cursor, error) {
	c := &cursor{input: input, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	missingInput := c.input == nil || o.Transaction == nil || o.Descriptor == nil || o.Query == nil || o.TargetKind == 0 || physicalMutationTarget(o.TargetKind) && o.Encoder == nil
	invalidBatch := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxRecordBytes < 128
	invalidSpool := o.WorkBytes == 0 || o.MaxSpoolBytes == 0 || o.MaxRows == 0
	if missingInput || invalidBatch || invalidSpool {
		return runtime.ErrInvalidConfig
	}
	for _, n := range []uint64{o.BatchBytes, o.MaxRecordBytes, o.SpoolMemoryBytes, o.MaxSpoolBytes, o.WorkBytes, o.SortRunBytes, o.MaxRows} {
		if n > uint64(math.MaxInt)/128 {
			return runtime.ErrInvalidConfig
		}
	}
	if o.Conflict != nil && (o.Operation != storage.MutationInsert || o.Merge != nil) {
		return runtime.ErrInvalidConfig
	}
	if o.Merge != nil {
		if o.Operation != 0 || o.Assignment != nil || len(o.AssignmentColumns) != 0 {
			return runtime.ErrInvalidConfig
		}
	} else if o.Operation != storage.MutationInsert && o.Operation != storage.MutationUpdate && o.Operation != storage.MutationDelete {
		return ErrUnsupported
	}
	if o.Operation != storage.MutationInsert && o.Source == nil {
		return runtime.ErrInvalidConfig
	}
	if o.Operation == storage.MutationDelete {
		if o.Assignment != nil || len(o.AssignmentColumns) != 0 {
			return runtime.ErrInvalidConfig
		}
	} else if o.Merge == nil {
		missingProgram := o.Assignment == nil && len(o.AssignmentColumns) != 0
		if missingProgram {
			return runtime.ErrInvalidConfig
		}
	}
	logicalNoopMerge := o.Merge != nil && !slices.ContainsFunc(o.Merge.Actions, func(action MergeAction) bool {
		return action.Action != ir.DoNothing
	})
	if logicalMutationTarget(o.TargetKind) && (o.Expansion != nil || o.Triggers == nil && !logicalNoopMerge || len(o.KeyLayout) != 0) {
		return runtime.ErrInvalidConfig
	}
	if automaticMutationTarget(o.TargetKind) != (len(o.ViewMutationSteps) != 0) {
		return runtime.ErrInvalidConfig
	}
	if _, relational := o.Expansion.(RelationalExpansion); relational && o.ConstraintTransaction == nil {
		return runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	expansionBytes := uint64(0)
	if o.Expansion != nil {
		expansionBytes = o.Expansion.WorkspaceBytes()
	}
	if o.Partitions != nil {
		partitionBytes := o.Partitions.WorkspaceBytes()
		if expansionBytes > ^uint64(0)-partitionBytes {
			return resource("admit mutation expansion workspace")
		}
		expansionBytes += partitionBytes
	}
	if expansionBytes != 0 {
		// WorkspaceBytes reports a provisioning ceiling derived from the schema's
		// index entry limits, not live memory. The expansion is not pre-allocated
		// here: its per-row allocations debit this same query account as they are
		// made. Reserving the ceiling would charge the account twice for work that
		// may never occur, so a session work_mem clamp below the ceiling would
		// reject an otherwise admissible statement. Admit a slot only.
		claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(0)
		if err != nil {
			return err
		}
		c.expansionClaim = claim
	}
	c.metadata = o.WorkBytes / 64
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(len(o.Layout)+len(o.InputLayouts)+len(o.AssignmentColumns)+len(o.UpdatedColumns)+len(o.KeyLayout)+len(o.Parameters)+len(o.ReturningSourceColumns)+len(o.ViewMutationSteps)*7+2)*1024
	if o.Merge != nil {
		base += uint64(len(o.Merge.Actions)) * 1024
		for _, action := range o.Merge.Actions {
			base += uint64(len(action.AssignmentColumns)) * 1024
		}
	}
	if o.Conflict != nil {
		base += uint64(len(o.Conflict.Arbiters)+len(o.Conflict.AssignmentColumns)+2) * 1024
	}
	local := c.metadata + base + o.MaxRecordBytes*32
	spoolPeak := 2*(c.metadata+o.MaxRecordBytes*9+8192+o.SpoolMemoryBytes) + 2*(o.MaxRecordBytes*9+8192)
	if base > c.metadata/2 || local > o.WorkBytes/4 || spoolPeak > o.WorkBytes/4 {
		return resource("admit modify workspace")
	}
	claim, err := o.Query.Allocator(runtime.WorkMemory).Reserve(local)
	if err != nil {
		return err
	}
	c.claim = claim
	spec, err := o.Descriptor.SpecWithin(ctx, model.DescriptorLimits{MaxBytes: c.metadata / 2, MaxItems: int(c.metadata / 64), MaxDepth: 64})
	if err != nil {
		return err
	}
	_, relational := o.Expansion.(RelationalExpansion)
	if relational && o.Deferral == nil && spec.Table != nil {
		for _, constraint := range spec.Table.Constraints {
			if constraint.Enforced && constraint.InitiallyDeferred {
				return runtime.ErrInvalidConfig
			}
		}
	}
	if err := preflight(spec, o.TargetKind, o.Operation, o.Expansion != nil, relational, o.Triggers != nil, o.Partitions != nil); err != nil {
		return err
	}
	if o.PartitionTarget != 0 && (o.Partitions == nil || spec.Table == nil || spec.Table.PartitionBound == nil ||
		spec.Table.PartitionState != model.PartitionAttached || o.PartitionTarget != spec.ID) {
		return runtime.ErrInvalidConfig
	}
	if o.PartitionTarget != 0 {
		route, err := o.Partitions.source(o.PartitionTarget)
		if err != nil || route.descriptor.ID() != o.Descriptor.ID() || route.descriptor.Version() != o.Descriptor.Version() {
			return runtime.ErrInvalidConfig
		}
	}
	if o.Expansion != nil {
		if err := o.Expansion.ValidateDescriptor(ctx, o.Descriptor); err != nil {
			return err
		}
	}
	var descriptorColumns []model.ColumnDescriptor
	var descriptorKeys []model.KeyElement
	if logicalMutationTarget(o.TargetKind) {
		descriptorColumns = spec.View.Columns
	} else {
		descriptorColumns, descriptorKeys = spec.Table.Columns, spec.Table.PrimaryKey
		c.target = storage.ObjectRef{ObjectID: spec.ID, Generation: spec.Physical.Generation, DescriptorVersion: spec.Version, EncodingVersion: spec.Physical.EncodingVersion}
		if err := c.target.Validate(); err != nil {
			return err
		}
	}
	if len(o.Layout) != len(descriptorColumns) || len(o.KeyLayout) != len(descriptorKeys) || physicalMutationTarget(o.TargetKind) && len(o.KeyLayout) == 0 {
		return storage.ErrInvalidLayout
	}
	remaining := c.metadata/2 - base
	// remaining bounds the layouts and values this cursor copies under WorkBytes.
	// Compiled programs are borrowed from the plan: the portal already charged
	// every program's OwnedBytes against MetadataBytes and reserved it on the
	// same query account, so re-charging them here would double-count plan
	// metadata against an operator workspace that never held it.
	ownedLayouts := make([]storage.ColumnLayout, len(o.Layout))
	c.columnIDs = make([]model.ColumnID, len(o.Layout))
	c.tableLayouts = make([]model.TypeLayout, len(o.Layout))
	for i, column := range o.Layout {
		descriptorColumn := descriptorColumns[i]
		layout := model.TypeLayout{Type: column.Type, Children: column.Children}
		outOfOrder := i > 0 && o.Layout[i-1].ID >= column.ID
		changedColumn := column.ID != descriptorColumn.ID || column.Nullable != descriptorColumn.Nullable || !model.TypeLayoutsEqual(layout, descriptorColumn.Layout)
		if changedColumn || outOfOrder {
			return storage.ErrInvalidLayout
		}
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		column.Children = owned.Children
		if column.HasMissingValue {
			value, used, err := model.CopyValue(ctx, column.MissingValue, remaining)
			if err != nil {
				return err
			}
			column.MissingValue, remaining = value, remaining-used
		} else {
			column.MissingValue = model.Value{}
		}
		ownedLayouts[i], c.columnIDs[i], c.tableLayouts[i] = column, column.ID, owned
		if !descriptorColumn.SystemIdentity {
			c.visibleColumns = append(c.visibleColumns, i)
			c.visibleLayouts = append(c.visibleLayouts, owned)
		}
	}
	if len(c.visibleColumns) != len(c.tableLayouts) {
		c.oldImage = make([]model.Value, len(c.visibleColumns))
		c.newImage = make([]model.Value, len(c.visibleColumns))
	}
	o.Layout = ownedLayouts
	var keyBytes uint64
	o.KeyLayout, keyBytes, err = storage.CopyKeyLayout(ctx, o.KeyLayout, remaining)
	if err != nil {
		return err
	}
	remaining -= keyBytes
	c.keyColumns = make([]int, len(o.KeyLayout))
	for i, key := range o.KeyLayout {
		pk := descriptorKeys[i]
		column := slices.Index(c.columnIDs, key.ColumnID)
		if column < 0 {
			return storage.ErrInvalidLayout
		}
		changedIdentity := key.ColumnID != pk.Column || key.Type != pk.Type || !model.TypeLayoutsEqual(model.TypeLayout{Type: key.Type, Children: key.Children}, c.tableLayouts[column])
		changedOrder := key.Descending != pk.Descending || key.NullsFirst != pk.NullsFirst
		if changedIdentity || changedOrder {
			return storage.ErrInvalidLayout
		}
		c.keyColumns[i] = column
	}
	copyLayouts := func(layouts []model.TypeLayout) ([]model.TypeLayout, error) {
		owned := make([]model.TypeLayout, len(layouts))
		for i, layout := range layouts {
			copy, used, err := model.CopyTypeLayout(ctx, layout, remaining)
			if err != nil {
				return nil, err
			}
			owned[i], remaining = copy, remaining-used
		}
		return owned, nil
	}
	o.InputLayouts, err = copyLayouts(o.InputLayouts)
	if err != nil {
		return err
	}
	if o.SourceObjectColumn > uint32(len(o.InputLayouts)) || o.Operation == storage.MutationInsert && o.SourceObjectColumn != 0 || o.Partitions != nil && o.Operation != storage.MutationInsert && o.SourceObjectColumn == 0 {
		return runtime.ErrInvalidConfig
	}
	if o.SourceObjectColumn != 0 {
		layout := o.InputLayouts[o.SourceObjectColumn-1]
		if layout.Type.Storage != model.TypeInt64 || layout.Type.ID == 0 {
			return storage.ErrInvalidLayout
		}
	}
	o.OldKeyColumns = slices.Clone(o.OldKeyColumns)
	o.LogicalOldColumns = slices.Clone(o.LogicalOldColumns)
	if o.Operation == storage.MutationInsert {
		if len(o.OldKeyColumns) != 0 || len(o.LogicalOldColumns) != 0 {
			return runtime.ErrInvalidConfig
		}
	} else if logicalMutationTarget(o.TargetKind) {
		if len(o.OldKeyColumns) != 0 || len(o.LogicalOldColumns) != len(descriptorColumns) {
			return storage.ErrInvalidLayout
		}
		for i, column := range o.LogicalOldColumns {
			if column < 0 || column >= len(o.InputLayouts) || !model.TypeLayoutsEqual(o.InputLayouts[column], c.tableLayouts[i]) {
				return storage.ErrInvalidLayout
			}
		}
	} else {
		if len(o.LogicalOldColumns) != 0 || len(o.OldKeyColumns) != len(o.KeyLayout) {
			return storage.ErrInvalidLayout
		}
		for i, column := range o.OldKeyColumns {
			if column < 0 || column >= len(o.InputLayouts) || !model.TypeLayoutsEqual(o.InputLayouts[column], c.tableLayouts[c.keyColumns[i]]) {
				return storage.ErrInvalidLayout
			}
		}
	}
	o.AssignmentColumns = slices.Clone(o.AssignmentColumns)
	o.UpdatedColumns = slices.Clone(o.UpdatedColumns)
	canUpdate := o.Operation == storage.MutationUpdate || o.Conflict != nil && o.Conflict.Action == ir.ConflictUpdate
	if o.Merge != nil {
		canUpdate = slices.ContainsFunc(o.Merge.Actions, func(action MergeAction) bool { return action.Action == ir.Update })
	}
	if !canUpdate && len(o.UpdatedColumns) != 0 || canUpdate && o.Triggers != nil && len(o.UpdatedColumns) == 0 {
		return runtime.ErrInvalidConfig
	}
	for i, column := range o.UpdatedColumns {
		if !slices.Contains(c.columnIDs, column) || slices.Contains(o.UpdatedColumns[:i], column) {
			return storage.ErrInvalidLayout
		}
	}
	c.assignmentColumns = make([]int, len(o.AssignmentColumns))
	if o.Assignment != nil {
		layouts := o.Assignment.OutputLayouts()
		if len(layouts) != len(o.AssignmentColumns) {
			return storage.ErrInvalidLayout
		}
		for i, column := range o.AssignmentColumns {
			index := slices.Index(c.columnIDs, column)
			if index < 0 || slices.Contains(o.AssignmentColumns[:i], column) || !model.TypeLayoutsEqual(layouts[i], c.tableLayouts[index]) {
				return storage.ErrInvalidLayout
			}
			c.assignmentColumns[i] = index
		}
		c.assignment, err = expr.NewState(o.Assignment, c.stateOptions())
		if err != nil {
			return err
		}
	}
	if err := c.configureViewMutations(ctx); err != nil {
		return err
	}
	if o.Merge != nil {
		if err := c.configureMerge(ctx); err != nil {
			return err
		}
	}
	if o.Conflict != nil {
		if err := c.configureConflict(ctx, spec.Table); err != nil {
			return err
		}
	}
	if o.Returning == nil {
		if len(o.ReturningInputLayouts) != 0 || len(o.ReturningSourceColumns) != 0 || o.ReturningTableOIDs != 0 {
			return runtime.ErrInvalidConfig
		}
	} else {
		imageCount := 2
		compositeCount := 2
		if o.Merge != nil {
			imageCount, compositeCount = 4, 3
		}
		expectedTableOIDs := 0
		if physicalMutationTarget(o.TargetKind) {
			expectedTableOIDs = 2
		}
		if o.Merge != nil {
			expectedTableOIDs = 3
		}
		if o.ReturningTableOIDs != expectedTableOIDs || len(o.ReturningSourceColumns) > len(o.InputLayouts) || len(o.ReturningInputLayouts) != imageCount+o.ReturningTableOIDs+len(o.ReturningSourceColumns) {
			return storage.ErrInvalidLayout
		}
		imageLayouts := c.visibleLayouts
		if len(c.viewMutationSteps) != 0 {
			imageLayouts = c.viewMutationSteps[len(c.viewMutationSteps)-1].layouts
		}
		for _, layout := range o.ReturningInputLayouts[:compositeCount] {
			if layout.Type.Storage != model.TypeComposite || len(layout.Children) != len(imageLayouts) {
				return storage.ErrInvalidLayout
			}
			for i, child := range layout.Children {
				if !model.TypeLayoutsEqual(child, imageLayouts[i]) {
					return storage.ErrInvalidLayout
				}
			}
		}
		if o.Merge != nil && o.ReturningInputLayouts[3].Type.Storage != model.TypeString {
			return storage.ErrInvalidLayout
		}
		for _, layout := range o.ReturningInputLayouts[imageCount : imageCount+o.ReturningTableOIDs] {
			if layout.Type.ID != 26 || layout.Type.Storage != model.TypeInt64 {
				return storage.ErrInvalidLayout
			}
		}
		for i, column := range o.ReturningSourceColumns {
			outsideInput := column < 0 || column >= len(o.InputLayouts)
			outOfOrder := i > 0 && column <= o.ReturningSourceColumns[i-1]
			if outsideInput || outOfOrder {
				return storage.ErrInvalidLayout
			}
			if !model.TypeLayoutsEqual(o.ReturningInputLayouts[i+imageCount+o.ReturningTableOIDs], o.InputLayouts[column]) {
				return storage.ErrInvalidLayout
			}
		}
		o.ReturningSourceColumns = slices.Clone(o.ReturningSourceColumns)
		c.returnValues = make([]model.Value, len(o.ReturningInputLayouts))
		o.ReturningInputLayouts, err = copyLayouts(o.ReturningInputLayouts)
		if err != nil {
			return err
		}
		c.returnLayouts = o.Returning.OutputLayouts()
		c.returning, err = expr.NewState(o.Returning, c.stateOptions())
		if err != nil {
			return err
		}
		c.returnInput, err = columnar.NewBuilder(o.ReturningInputLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.WorkMemory)})
		if err != nil {
			return err
		}
		c.output, err = columnar.NewBuilder(c.returnLayouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
		if err != nil {
			return err
		}
	}
	parameters := make([]expr.ParameterValue, len(o.Parameters))
	for i, parameter := range o.Parameters {
		layout, used, err := model.CopyTypeLayout(ctx, parameter.Layout, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		value, used, err := model.CopyValue(ctx, parameter.Value, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		parameters[i] = expr.ParameterValue{Number: parameter.Number, Layout: layout, Value: value}
	}
	o.Parameters = parameters
	c.descriptor = o.Descriptor
	o.Descriptor = nil
	sourceImageColumns := 0
	if o.Partitions != nil && o.Partitions.sourceImage {
		sourceImageColumns = 1
	}
	c.row = make([]model.Value, len(o.InputLayouts)+len(o.Layout)+sourceImageColumns)
	c.keyValues = make([]model.Value, len(o.KeyLayout))
	c.assignmentInput, err = columnar.NewBuilder(o.InputLayouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.WorkMemory)})
	if err != nil {
		return err
	}
	if physicalMutationTarget(o.TargetKind) {
		if _, err := o.Encoder.EncodePrefix(ctx, o.KeyLayout, nil, o.MaxRecordBytes); err != nil {
			return err
		}
	}
	c.input = &limitedInput{input: c.input, maximum: o.MaxRows}
	if physicalMutationTarget(o.TargetKind) && o.Merge == nil && o.Operation != storage.MutationInsert {
		keys := make([]externalsort.Key, len(o.OldKeyColumns))
		for i, column := range o.OldKeyColumns {
			keys[i] = externalsort.Key{Column: column, Descending: o.KeyLayout[i].Descending, NullsFirst: o.KeyLayout[i].NullsFirst}
		}
		input := c.input
		c.input = nil
		c.input, err = externalsort.New(ctx, input, externalsort.Options{Layouts: o.InputLayouts, Keys: keys, Encoder: o.Encoder, Query: o.Query, BatchRows: o.BatchRows, BatchBytes: o.BatchBytes, MaxRecordBytes: o.MaxRecordBytes, RunBytes: o.SortRunBytes, MaxRuns: o.SortMaxRuns, MergeFanIn: o.SortMergeFanIn, WorkBytes: o.WorkBytes / 2})
	}
	return err
}

func preflight(spec model.DescriptorSpec, target ir.ModifyTargetKind, operation storage.MutationOp, expandIndexes, relational, triggers, partitioned bool) error {
	if logicalMutationTarget(target) {
		if spec.Name.Kind != model.ObjectView || spec.View == nil || expandIndexes || operation != 0 && (!triggers || len(spec.View.Triggers) == 0) {
			return ErrUnsupported
		}
		if operation != 0 {
			event := map[storage.MutationOp]string{storage.MutationInsert: "insert", storage.MutationUpdate: "update", storage.MutationDelete: "delete"}[operation]
			if event == "" || !slices.ContainsFunc(spec.View.Triggers, func(trigger model.TriggerDescriptor) bool {
				return trigger.EnableMode.Fires(false) && trigger.Timing == "instead" && trigger.ForEachRow && slices.Contains(trigger.Events, event)
			}) {
				return ErrUnsupported
			}
		}
		for _, column := range spec.View.Columns {
			if column.State != model.StatePublic {
				return ErrUnsupported
			}
		}
		return nil
	}
	if !physicalMutationTarget(target) || spec.Name.Kind != model.ObjectTable && spec.Name.Kind != model.ObjectMaterializedView || spec.Table == nil {
		return ErrUnsupported
	}
	table := spec.Table
	missingIndexes := len(table.Indexes) != 0 && !expandIndexes && !partitioned
	missingTriggers := len(table.Triggers) != 0 && !triggers
	missingPartitionRouting := table.PartitionBound != nil && table.PartitionState == model.PartitionAttached && !partitioned
	if missingIndexes || missingTriggers || table.Partition != nil || missingPartitionRouting || table.PartitionStrategy != 0 && !partitioned {
		return ErrUnsupported
	}
	for _, column := range table.Columns {
		if column.State != model.StatePublic {
			return ErrUnsupported
		}
	}
	for _, key := range table.PrimaryKey {
		changedCollation := key.Collation != 0 && key.Collation != key.Type.Collation
		if key.Expression != nil || key.OperatorClass != 0 || changedCollation {
			return ErrUnsupported
		}
	}
	if partitioned {
		return nil
	}
	for _, constraint := range table.Constraints {
		supported := false
		switch constraint.Kind {
		case model.ConstraintPrimaryKey:
			supported = (constraint.Index == 0 || expandIndexes) && (!constraint.Deferrable || relational)
		case model.ConstraintNotNull, model.ConstraintCheck:
			supported = !constraint.Deferrable || relational
		case model.ConstraintUnique:
			supported = expandIndexes && (!constraint.Deferrable || relational)
		case model.ConstraintForeignKey, model.ConstraintExclude:
			supported = relational
		case model.ConstraintTrigger:
			supported = triggers
		}
		if !supported {
			return ErrUnsupported
		}
		if err := preflightConstraint(table, constraint); err != nil {
			return err
		}
	}
	return nil
}

func preflightConstraint(table *model.TableMetadata, constraint model.ConstraintDescriptor) error {
	if constraint.Kind == model.ConstraintCheck {
		return nil
	}
	if constraint.Kind == model.ConstraintUnique {
		if constraint.Index == 0 || !slices.Contains(table.Indexes, constraint.Index) {
			return ErrUnsupported
		}
		return nil
	}
	if constraint.Kind == model.ConstraintPrimaryKey {
		if constraint.Index != 0 && slices.Contains(table.Indexes, constraint.Index) {
			for _, id := range constraint.Columns {
				if !slices.ContainsFunc(table.Columns, func(column model.ColumnDescriptor) bool {
					return column.ID == id && !column.Nullable
				}) {
					return ErrUnsupported
				}
			}
			return nil
		}
		if len(constraint.Columns) != len(table.PrimaryKey) {
			return ErrUnsupported
		}
		for i, column := range constraint.Columns {
			if column != table.PrimaryKey[i].Column {
				return ErrUnsupported
			}
		}
		return nil
	}
	if constraint.Kind == model.ConstraintNotNull {
		for _, id := range constraint.Columns {
			if !slices.ContainsFunc(table.Columns, func(column model.ColumnDescriptor) bool {
				return column.ID == id && !column.Nullable
			}) {
				return ErrUnsupported
			}
		}
	}
	return nil
}

func (c *cursor) stateOptions() expr.StateOptions {
	return expr.StateOptions{WorkAllocator: c.options.Query.Allocator(runtime.WorkMemory), OutputAllocator: c.options.Query.Allocator(runtime.WorkMemory), Subqueries: c.options.Subqueries, Sequences: c.options.Sequences, Relations: c.options.Relations, Temporal: c.options.Temporal, Settings: c.options.Settings, TextSearch: c.options.TextSearch, Advisory: c.options.Advisory, Native: c.options.Native}
}
