package schema

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Descriptor snapshots and retained plan metadata are admitted separately against
// MetadataBytes. EntryBytes bounds each complete input image, output and scratch
// region; borrowed compiler/encoder resources have their own admission policy.
// Zero limits admit nothing. MaxColumns bounds base and physical index columns.
type IndexLimits struct {
	MetadataBytes uint64
	EntryBytes    uint64
	MaxColumns    int
}

// IndexExpressions must bind immutable index semantics, including dependency
// versions, volatility and the exact result type. Inputs are borrowed for the
// call; the returned program owns its bound metadata and obeys ValueExpression's
// input ownership and evaluation resource contract.
type IndexExpressions interface {
	CompileIndexExpression(context.Context, *model.ExpressionDefinition, []storage.ColumnLayout, model.TypeLayout) (txn.ValueExpression, error)
}

type IndexEntry struct {
	Key       []byte
	ColumnIDs []model.ColumnID
	Values    []model.Value
}

var (
	ErrInvalidIndexPlan = errors.New("invalid secondary index plan")
	ErrIndexLimit       = errors.New("secondary index exceeds resource limits")
)

type MissingIndexExpressionsError struct{}

func (*MissingIndexExpressionsError) Error() string {
	return "secondary index requires an immutable expression compiler"
}

// IndexKeyImplementationError retains the encoder's typed cause without
// importing SQL evaluation packages into the schema control plane.
type IndexKeyImplementationError struct {
	Type          model.Type
	OperatorClass model.OID
	Cause         error
}

func (e *IndexKeyImplementationError) Error() string {
	return fmt.Sprintf("secondary index key implementation unavailable for type %d", e.Type.ID)
}

func (e *IndexKeyImplementationError) Unwrap() error { return e.Cause }

type indexPlanKey struct {
	column     int
	expression txn.ValueExpression
}

// IndexPlan owns immutable metadata and borrows its encoder and bound programs.
// Index columns are keys, a bytea disambiguator, a bytea owner, then INCLUDEs.
// The disambiguator remains declared even when uniqueness makes its value empty.
type IndexPlan struct {
	target              storage.ObjectRef
	tableID             model.ObjectID
	tableVersion        uint64
	state               model.SchemaState
	parentIndex         model.ObjectID
	valid               bool
	writeMaintained     bool
	unique              bool
	nullsNotDistinct    bool
	deferred            bool
	disambiguated       bool
	exclusion           bool
	pendingKind         model.ConstraintKind
	pendingName         string
	pendingDependencies []model.Dependency
	baseColumns         []storage.ColumnLayout
	baseKeyLayout       []storage.KeyColumn
	baseKeyColumns      []int
	keys                []indexPlanKey
	includes            []int
	predicate           txn.ValueExpression
	layout              storage.ObjectLayout
	keyLayout           []storage.KeyColumn
	encoder             storage.KeyEncoder
	limits              IndexLimits
	metadata            uint64
}

func NewIndexPlan(ctx context.Context, table, index *model.Descriptor, encoder storage.KeyEncoder, expressions IndexExpressions, limits IndexLimits) (*IndexPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateIndexLimits(limits); err != nil {
		return nil, err
	}
	if table == nil || index == nil {
		return nil, invalidIndexPlan()
	}
	wrongKinds := !tableShapedKind(table.Kind()) || index.Kind() != model.ObjectIndex
	if wrongKinds || table.State() != model.StatePublic || index.ID() == table.ID() {
		return nil, invalidIndexPlan()
	}
	if encoder == nil {
		return nil, &IndexKeyImplementationError{}
	}
	descriptorLimits := model.DescriptorLimits{MaxBytes: limits.MetadataBytes, MaxItems: int(limits.MetadataBytes), MaxDepth: 64}
	tableSpec, err := table.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return nil, indexPlanLimitError(err)
	}
	indexSpec, err := index.SpecWithin(ctx, descriptorLimits)
	if err != nil {
		return nil, indexPlanLimitError(err)
	}
	if tableSpec.Table == nil || indexSpec.Index == nil || indexSpec.Index.Table != table.ID() || index.State() > model.StateDropDeleteOnly {
		return nil, invalidIndexPlan()
	}
	metadata := indexSpec.Index
	missingKeys := len(tableSpec.Table.Columns) == 0 || len(tableSpec.Table.PrimaryKey) == 0 || len(metadata.Keys) == 0
	if missingKeys || (metadata.NullsNotDistinct && !metadata.Unique) {
		return nil, invalidIndexPlan()
	}
	if len(tableSpec.Table.Columns) > limits.MaxColumns || len(metadata.Keys) > limits.MaxColumns-2 || len(metadata.Include) > limits.MaxColumns-2-len(metadata.Keys) {
		return nil, indexPlanLimitError(model.ErrValueBudget)
	}
	physical := index.Physical()
	p := &IndexPlan{
		target:  storage.ObjectRef{ObjectID: index.ID(), Generation: physical.Generation, DescriptorVersion: index.Version(), EncodingVersion: physical.EncodingVersion},
		tableID: table.ID(), tableVersion: table.Version(), state: index.State(), parentIndex: metadata.ParentIndex,
		valid: metadata.Valid(), writeMaintained: metadata.WriteMaintained(),
		unique: metadata.Unique, nullsNotDistinct: metadata.NullsNotDistinct, disambiguated: metadata.DeferredUnique,
		encoder: encoder, limits: limits,
	}
	if err := p.target.Validate(); err != nil {
		return nil, err
	}
	budget := indexPlanBudget{limit: limits.MetadataBytes}
	if err := budget.add(1, uint64(unsafe.Sizeof(IndexPlan{}))); err != nil {
		return nil, err
	}
	if metadata.PendingConstraint != nil {
		if err := budget.add(1, uint64(len(metadata.PendingConstraint.Name))); err != nil {
			return nil, err
		}
		if err := budget.add(uint64(len(metadata.PendingDependencies)), uint64(unsafe.Sizeof(model.Dependency{}))); err != nil {
			return nil, err
		}
		p.pendingKind = metadata.PendingConstraint.Kind
		p.pendingName = strings.Clone(metadata.PendingConstraint.Name)
		p.pendingDependencies = slices.Clone(metadata.PendingDependencies)
	}
	if err := p.bindBase(ctx, tableSpec.Table, &budget); err != nil {
		return nil, err
	}
	if err := p.bindIndex(ctx, metadata, expressions, &budget); err != nil {
		return nil, err
	}
	if err := p.layout.Validate(ctx); err != nil {
		return nil, errors.Join(ErrInvalidIndexPlan, err)
	}
	if err := p.validateKeyCodecs(ctx); err != nil {
		return nil, err
	}
	p.metadata = budget.used
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func validateIndexLimits(limits IndexLimits) error {
	invalidMetadata := limits.MetadataBytes == 0 || limits.MetadataBytes > math.MaxInt
	invalidEntry := limits.EntryBytes == 0 || limits.EntryBytes > math.MaxInt
	invalidColumns := limits.MaxColumns <= 0 || uint64(limits.MaxColumns) > math.MaxUint32
	if invalidMetadata || invalidEntry || invalidColumns {
		return indexPlanLimitError(model.ErrValueBudget)
	}
	return nil
}

func (p *IndexPlan) bindBase(ctx context.Context, table *model.TableMetadata, budget *indexPlanBudget) error {
	if err := budget.add(uint64(len(table.Columns)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))); err != nil {
		return err
	}
	p.baseColumns = make([]storage.ColumnLayout, 0, len(table.Columns))
	for _, column := range table.Columns {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch column.State {
		case model.StatePublic, model.StateWriteOnly:
		case model.StateDeleteOnly, model.StateDropWriteOnly, model.StateDropDeleteOnly:
			continue
		default:
			return invalidIndexPlan()
		}
		if err := budget.layout(ctx, column.Layout); err != nil {
			return err
		}
		layout := storage.ColumnLayout{ID: column.ID, Type: column.Layout.Type, Children: column.Layout.Children, Nullable: column.Nullable}
		if column.ConstantDefault != nil {
			value, used, err := model.CopyValue(ctx, *column.ConstantDefault, budget.limit-budget.used)
			if err != nil {
				return indexPlanLimitError(err)
			}
			budget.used += used
			layout.HasMissingValue, layout.MissingValue = true, value
		}
		p.baseColumns = append(p.baseColumns, layout)
	}
	slices.SortFunc(p.baseColumns, func(a, b storage.ColumnLayout) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for i, column := range p.baseColumns {
		if column.ID == 0 || (i != 0 && p.baseColumns[i-1].ID == column.ID) {
			return invalidIndexPlan()
		}
	}
	if err := budget.add(uint64(len(table.PrimaryKey)), uint64(unsafe.Sizeof(storage.KeyColumn{}))+uint64(unsafe.Sizeof(int(0)))); err != nil {
		return err
	}
	p.baseKeyLayout = make([]storage.KeyColumn, len(table.PrimaryKey))
	p.baseKeyColumns = make([]int, len(table.PrimaryKey))
	for i, key := range table.PrimaryKey {
		if err := ctx.Err(); err != nil {
			return err
		}
		if key.Expression != nil || key.Column == 0 {
			return invalidIndexPlan()
		}
		column, ok := p.baseColumn(key.Column)
		if !ok || p.baseColumns[column].Nullable || p.baseColumns[column].Type != key.Type {
			return invalidIndexPlan()
		}
		layout := model.TypeLayout{Type: key.Type, Children: p.baseColumns[column].Children}
		if err := p.validateIndexPlanKey(ctx, key, layout); err != nil {
			return err
		}
		owned, used, err := model.CopyPhysicalTypeLayout(ctx, layout, budget.limit-budget.used)
		if err != nil {
			return indexPlanLimitError(err)
		}
		budget.used += used
		p.baseKeyLayout[i] = storage.KeyColumn{ColumnID: key.Column, Type: key.Type, Children: owned.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
		p.baseKeyColumns[i] = column
	}
	return nil
}

func (p *IndexPlan) bindIndex(ctx context.Context, metadata *model.IndexMetadata, expressions IndexExpressions, budget *indexPlanBudget) error {
	count := len(metadata.Keys) + 2 + len(metadata.Include)
	for _, allocation := range [...]struct{ count, size uint64 }{
		{uint64(count), uint64(unsafe.Sizeof(storage.ColumnLayout{}))},
		{uint64(len(metadata.Keys) + 1), uint64(unsafe.Sizeof(storage.KeyColumn{}))},
		{uint64(len(metadata.Keys)), uint64(unsafe.Sizeof(indexPlanKey{}))},
		{uint64(len(metadata.Include)), uint64(unsafe.Sizeof(int(0)))},
	} {
		if err := budget.add(allocation.count, allocation.size); err != nil {
			return err
		}
	}
	p.layout = storage.ObjectLayout{Target: p.target, Columns: make([]storage.ColumnLayout, count)}
	p.keyLayout = make([]storage.KeyColumn, len(metadata.Keys)+1)
	p.keys = make([]indexPlanKey, len(metadata.Keys))
	p.includes = make([]int, len(metadata.Include))
	for i, key := range metadata.Keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := model.ColumnID(i + 1)
		column := storage.ColumnLayout{ID: id, Type: key.Type, Children: key.Children, Nullable: true}
		bound := indexPlanKey{column: -1}
		if key.Expression == nil {
			slot, ok := p.baseColumn(key.Column)
			if !ok {
				return invalidIndexPlan()
			}
			baseType := p.baseColumns[slot].Type
			baseType.Collation = key.Type.Collation
			if baseType != key.Type {
				return invalidIndexPlan()
			}
			bound.column = slot
			column.Nullable = p.baseColumns[slot].Nullable
			column.Children = p.baseColumns[slot].Children
		}
		layout := model.TypeLayout{Type: key.Type, Children: column.Children}
		if err := p.validateIndexPlanKey(ctx, key, layout); err != nil {
			return err
		}
		if key.Expression != nil {
			if err := budget.layout(ctx, layout); err != nil {
				return err
			}
			program, err := p.compile(ctx, expressions, key.Expression, layout)
			if err != nil {
				return err
			}
			bound.expression = program
		}
		owned, used, err := model.CopyPhysicalTypeLayout(ctx, layout, budget.limit-budget.used)
		if err != nil {
			return indexPlanLimitError(err)
		}
		budget.used += used
		p.keys[i] = bound
		p.layout.Columns[i] = column
		p.keyLayout[i] = storage.KeyColumn{ColumnID: id, Type: key.Type, Children: owned.Children, Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	bytea := model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}
	disambiguator := model.ColumnID(len(metadata.Keys) + 1)
	p.layout.Columns[len(metadata.Keys)] = storage.ColumnLayout{ID: disambiguator, Type: bytea}
	p.layout.Columns[len(metadata.Keys)+1] = storage.ColumnLayout{ID: disambiguator + 1, Type: bytea}
	p.keyLayout[len(metadata.Keys)] = storage.KeyColumn{ColumnID: disambiguator, Type: bytea}
	for i, id := range metadata.Include {
		if err := ctx.Err(); err != nil {
			return err
		}
		slot, ok := p.baseColumn(id)
		if !ok {
			return invalidIndexPlan()
		}
		column := p.baseColumns[slot]
		if err := budget.layout(ctx, model.TypeLayout{Type: column.Type, Children: column.Children}); err != nil {
			return err
		}
		column.ID = model.ColumnID(len(metadata.Keys) + 3 + i)
		p.layout.Columns[int(column.ID)-1] = column
		p.includes[i] = slot
	}
	if metadata.Predicate != nil {
		program, err := p.compile(ctx, expressions, metadata.Predicate, model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
		if err != nil {
			return err
		}
		p.predicate = program
	}
	return nil
}

func (p *IndexPlan) validateIndexPlanKey(ctx context.Context, key model.KeyElement, layout model.TypeLayout) error {
	if (key.Column == 0) == (key.Expression == nil) || key.Collation != key.Type.Collation {
		return invalidIndexPlan()
	}
	if key.Type.Storage == model.TypeVectorF32 {
		return &IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass}
	}
	if err := layout.Validate(); err != nil {
		return errors.Join(ErrInvalidIndexPlan, err)
	}
	declared := storage.KeyColumn{Type: key.Type, Children: key.Children}
	resolved := storage.KeyColumn{Type: layout.Type, Children: layout.Children}
	if !storage.KeyLayoutsEqual([]storage.KeyColumn{declared}, []storage.KeyColumn{resolved}) {
		return invalidIndexPlan()
	}
	if key.Type.Storage != model.TypeString && key.Type.Collation != 0 {
		return invalidIndexPlan()
	}
	if key.OperatorClass != 0 {
		encoder, ok := p.encoder.(storage.IndexKeyEncoder)
		if !ok {
			return &IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass}
		}
		if err := encoder.ValidateOperatorClass(ctx, layout, key.OperatorClass); err != nil {
			return &IndexKeyImplementationError{Type: key.Type, OperatorClass: key.OperatorClass, Cause: err}
		}
	}
	return nil
}

func (p *IndexPlan) compile(ctx context.Context, compiler IndexExpressions, definition *model.ExpressionDefinition, result model.TypeLayout) (txn.ValueExpression, error) {
	if compiler == nil {
		return nil, &MissingIndexExpressionsError{}
	}
	for _, dependency := range definition.Dependencies {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if dependency.Object != p.tableID {
			continue
		}
		if dependency.Version == 0 || dependency.Version > p.tableVersion {
			return nil, invalidIndexPlan()
		}
		if dependency.Column != 0 {
			if _, ok := p.baseColumn(dependency.Column); !ok {
				return nil, invalidIndexPlan()
			}
		}
		// A policy-only descriptor version can change the index set without
		// changing this persistent column-ID/type binding. The compiler checks
		// semantic compatibility against the supplied current row layout.
	}
	program, err := compiler.CompileIndexExpression(ctx, definition, p.baseColumns, result)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if program == nil {
		return nil, &MissingIndexExpressionsError{}
	}
	return program, nil
}

func (p *IndexPlan) validateKeyCodecs(ctx context.Context) error {
	for _, layout := range [][]storage.KeyColumn{p.baseKeyLayout, p.keyLayout} {
		for _, key := range layout {
			value := model.NullVal(key.Type.Storage)
			encoded, err := p.encoder.EncodePrefix(ctx, []storage.KeyColumn{key}, []model.Value{value}, p.limits.EntryBytes)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return &IndexKeyImplementationError{Type: key.Type, Cause: err}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if uint64(cap(encoded)) > p.limits.EntryBytes {
				return indexPlanLimitError(model.ErrValueBudget)
			}
			if len(encoded) == 0 {
				return &IndexKeyImplementationError{Type: key.Type, Cause: storage.ErrInvalidEncoding}
			}
		}
	}
	return ctx.Err()
}

func (p *IndexPlan) baseColumn(id model.ColumnID) (int, bool) {
	position := sort.Search(len(p.baseColumns), func(i int) bool { return p.baseColumns[i].ID >= id })
	return position, position < len(p.baseColumns) && p.baseColumns[position].ID == id
}

func (p *IndexPlan) Target() storage.ObjectRef       { return p.target }
func (p *IndexPlan) Table() (model.ObjectID, uint64) { return p.tableID, p.tableVersion }
func (p *IndexPlan) State() model.SchemaState        { return p.state }
func (p *IndexPlan) Valid() bool                     { return p.valid }
func (p *IndexPlan) WriteMaintained() bool           { return p.writeMaintained }
func (p *IndexPlan) OwnerColumn() model.ColumnID     { return model.ColumnID(len(p.keys) + 2) }
func (p *IndexPlan) KeyLayout() []storage.KeyColumn {
	layout, _, err := storage.CopyKeyLayout(context.Background(), p.keyLayout, p.metadata)
	if err != nil {
		panic(err)
	}
	return layout
}
func (p *IndexPlan) metadataBytes() uint64 { return p.metadata }
func (p *IndexPlan) isUnique() bool        { return p.unique }
func (p *IndexPlan) entryBytes() uint64    { return p.limits.EntryBytes }

func (p *IndexPlan) Layout() storage.ObjectLayout {
	return storage.ObjectLayout{Target: p.target, Columns: cloneIndexPlanColumns(p.layout.Columns)}
}

func cloneIndexPlanColumns(source []storage.ColumnLayout) []storage.ColumnLayout {
	columns := slices.Clone(source)
	for i := range columns {
		columns[i].Children = cloneIndexPlanLayouts(columns[i].Children)
	}
	return columns
}

func cloneIndexPlanLayouts(source []model.TypeLayout) []model.TypeLayout {
	children := slices.Clone(source)
	for i := range children {
		children[i].Children = cloneIndexPlanLayouts(children[i].Children)
		if len(children[i].FieldNames) != 0 {
			children[i].FieldNames = make([]string, len(source[i].FieldNames))
			for j, name := range source[i].FieldNames {
				children[i].FieldNames[j] = strings.Clone(name)
			}
		} else {
			children[i].FieldNames = nil
		}
	}
	return children
}

type indexPlanBudget struct{ limit, used uint64 }

func (b *indexPlanBudget) add(count, size uint64) error {
	if size != 0 && count > (b.limit-b.used)/size {
		return indexPlanLimitError(model.ErrValueBudget)
	}
	b.used += count * size
	return nil
}

func (b *indexPlanBudget) layout(ctx context.Context, layout model.TypeLayout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.add(uint64(len(layout.Children)), uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return err
	}
	if err := b.add(uint64(len(layout.FieldNames)), uint64(unsafe.Sizeof(""))); err != nil {
		return err
	}
	for _, name := range layout.FieldNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.add(uint64(len(name)), 1); err != nil {
			return err
		}
	}
	for _, child := range layout.Children {
		if err := b.layout(ctx, child); err != nil {
			return err
		}
	}
	return nil
}

func invalidIndexPlan() error {
	return errors.Join(ErrInvalidIndexPlan, model.ErrInvalidDescriptor, storage.ErrInvalidLayout)
}

func indexPlanLimitError(err error) error {
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrDescriptorLimit) {
		return errors.Join(ErrIndexLimit, err)
	}
	return err
}
