// Package ir owns immutable logical query graphs. Handles are graph-local and
// one-based; zero denotes absence. Graphs contain no invocation or physical state.
// Utility orchestration belongs to session, outside this relational boundary.
package ir

import "github.com/gosuda/supersql-alpha/internal/model"

type ScalarID uint32
type RelID uint32
type ScopeID uint32
type LayoutID uint32
type CTEID uint32
type SignatureID uint32

// Signature records the resolved overload, after polymorphic/variadic expansion.
// Registry ownership and SQL coercion selection remain with binding.
type Signature struct {
	Function model.FunctionID
	Operator model.OperatorID
	Result       LayoutID
	Nullable     bool
	Strict       bool
	SetReturning bool
	Effects      Effects
	// InputCollation is independent of the result layout; zero is indeterminate or absent.
	InputCollation model.CollationID
	// VariadicArray spreads the final array parameter instead of passing it as one value.
	VariadicArray bool
	parameters    extent
}
type SignatureSpec struct {
	Signature
	Parameters []LayoutID
}

// Span is a half-open byte range in the builder's source. Zero is synthetic.
type Span struct{ Start, End uint32 }

type Volatility uint8

const (
	Immutable Volatility = iota
	Stable
	Volatile
)

type Effects struct {
	Volatility Volatility
	MayError   bool
}

func (e Effects) Union(other Effects) Effects {
	return Effects{max(e.Volatility, other.Volatility), e.MayError || other.MayError}
}

type ScalarKind uint8

const (
	Const ScalarKind = iota + 1
	Slot
	Param
	Call
	Cast
	Case
	Coalesce
	NullTest
	Bool
	Row
	Array
	Subquery
	Field
	Once
	Collate
	Relabel
	Least
	Greatest
	ArrayStack
	ArrayElement
	ArrayCoerce
	ArrayIndex
	ArraySlice
	RowCompare
	SQLJSON
	ArrayQuantified
	FieldStore
	ArrayStore
	JSONBStore
)

// SQLJSONOperation identifies SQL/JSON syntax after binding. It is not a
// catalog function identity.
type SQLJSONOperation uint8

const (
	SQLJSONParse SQLJSONOperation = iota + 1
	SQLJSONScalar
	SQLJSONSerialize
	SQLJSONArray
	SQLJSONObject
	SQLJSONIs
	SQLJSONExists
	SQLJSONValue
	SQLJSONQuery
)

type SQLJSONFormat uint8

const (
	SQLJSONDefaultFormat SQLJSONFormat = iota + 1
	SQLJSONTextFormat
	SQLJSONBinaryFormat
)

type SQLJSONEncoding uint8

const (
	SQLJSONDefaultEncoding SQLJSONEncoding = iota + 1
	SQLJSONUTF8
	SQLJSONUTF16
	SQLJSONUTF32
)

type SQLJSONWrapper uint8

const (
	SQLJSONUnspecifiedWrapper SQLJSONWrapper = iota + 1
	SQLJSONWithoutWrapper
	SQLJSONConditionalWrapper
	SQLJSONUnconditionalWrapper
)

type SQLJSONQuotes uint8

const (
	SQLJSONUnspecifiedQuotes SQLJSONQuotes = iota + 1
	SQLJSONKeepQuotes
	SQLJSONOmitQuotes
)

type SQLJSONItemType uint8

const (
	SQLJSONAnyItem SQLJSONItemType = iota + 1
	SQLJSONObjectItem
	SQLJSONArrayItem
	SQLJSONScalarItem
)

type SQLJSONFlags uint8

const (
	SQLJSONAbsentOnNull SQLJSONFlags = 1 << iota
	SQLJSONUniqueKeys
	SQLJSONCaptureEmpty
	SQLJSONCaptureError
	SQLJSONUseTZ
)

type BoolOp uint8

const (
	And BoolOp = iota + 1
	Or
	Not
)

type RowComparisonKind uint8

const (
	RowCompareLess         RowComparisonKind = 1
	RowCompareLessEqual    RowComparisonKind = 2
	RowCompareEqual        RowComparisonKind = 3
	RowCompareGreaterEqual RowComparisonKind = 4
	RowCompareGreater      RowComparisonKind = 5
	RowCompareNotEqual     RowComparisonKind = 6
)

type SubqueryKind uint8

const (
	ScalarSubquery SubqueryKind = iota + 1
	ExistsSubquery
	AnySubquery
	AllSubquery
	RowSubquery
	ArraySubquery
	TupleSubquery
)

type CastContext uint8

const (
	ExplicitCast CastContext = iota + 1
	AssignmentCast
	ImplicitCast
)

// ArraySubscripts records provided slice bounds by dimension. Operands follow
// the source array, then all provided upper bounds, then all provided lower bounds.
type ArraySubscripts struct {
	Dimensions uint8
	Upper      uint8
	Lower      uint8
}

type Scalar struct {
	Kind          ScalarKind
	Scope         ScopeID
	Layout        LayoutID
	Nullable      bool
	Effects       Effects
	Span          Span
	Slot          model.SlotID
	Parameter     uint32
	FieldIndex    uint32
	Subscripts    ArraySubscripts
	Signature     SignatureID
	Function      model.FunctionID
	Operator      model.OperatorID
	Strict        bool
	CastContext   CastContext
	BoolOp        BoolOp
	RowComparison RowComparisonKind
	Negated       bool
	SliceAssignment bool
	SubqueryKind  SubqueryKind
	Relation      RelID
	JSONOperation SQLJSONOperation
	JSONFormat    SQLJSONFormat
	JSONEncoding  SQLJSONEncoding
	JSONOutputFormat   SQLJSONFormat
	JSONOutputEncoding SQLJSONEncoding
	JSONWrapper   SQLJSONWrapper
	JSONQuotes    SQLJSONQuotes
	JSONItemType  SQLJSONItemType
	JSONFlags     SQLJSONFlags
	JSONPassing   uint16
	operands      extent
	comparisons   extent
	constant      uint32
}

// ScalarSpec borrows its slices only during AddScalar. Case operands are searched
// WHEN/THEN pairs followed by mandatory ELSE; simple CASE is lowered by binding.
// Quantified and row-comparison subqueries have one left operand and comparison
// signature per result column. RowComparison is zero for one scalar column and
// identifies row equality, inequality, or ordered semantics otherwise.
// ArrayQuantified has scalar and array operands and one comparison signature.
// Field and FieldStore identify a zero-based composite child. FieldStore has
// composite and replacement operands. ArrayStore has source, bound operands in
// the same order as an array fetch, then replacement; SliceAssignment
// distinguishes slice bounds from element indexes. JSONBStore has source,
// typed integer-or-text subscripts, then replacement. Once has one operand,
// evaluated on first reach per row and reused by handle within one scalar program
// invocation. It never widens the caller's active mask.
// Collate relabels only its operand's top-level collation; values and nested layouts are unchanged.
// Least/Greatest evaluate every operand and skip NULLs. Their binary boolean
// signature compares a candidate against the selected value; binary-compatible
// parameter identities may differ from the common operand/result identity.
// ArrayStack combines array operands with the result's element layout into one
// additional outer dimension. The result is non-NULL; shape errors are MayError.
// ArrayCoerce has a source array and an element conversion template. The template
// is a unary Call/Cast/Relabel/Collate chain rooted at ArrayElement; other call
// arguments are constants. ArrayElement is local to that template, never a row input.
// RowCompare operands are ordered binary int4 support-function Calls, not rows.
// Planning visits all left inputs before right inputs without folding the Calls;
// execution demands one pair at a time until its comparator returns nonzero or NULL.
type ScalarSpec struct {
	Scalar
	Operands    []ScalarID
	Comparisons []SignatureID
	Value       model.Value
}

type SlotInfo struct {
	Scope    ScopeID
	Layout   LayoutID
	Nullable bool
	Object   model.ObjectID
	Column   model.ColumnID
}
type scope struct {
	parent          ScopeID
	captures        extent
	captureCapacity uint32
}
type layout struct {
	typ        model.Type
	children   extent
	fieldNames extent
}
type extent struct{ start, count uint32 }

type RelKind uint8

const (
	Scan RelKind = iota + 1
	Values
	Filter
	Project
	Join
	Apply
	Aggregate
	Window
	Sort
	Limit
	Distinct
	SetOp
	CTEProducer
	CTEConsumer
	RecursiveUnion
	LockRows
	Modify
	// GenerateSeries has 2/3 integer arguments and an optional int8 ordinal output.
	GenerateSeries
	// GenerateSubscripts evaluates array, dimension, and optional reverse once in order.
	// Outputs are non-null int4 subscripts and optional non-null int8 ordinality.
	GenerateSubscripts
	// Unnest expands one array; record data outputs may declare a runtime-checked schema.
	Unnest
	// RowsFrom zips inputs positionally, null-padding to the longest input.
	RowsFrom
	// ProjectSet evaluates direct set-returning bindings in lockstep for each
	// input row. Nested set calls are split into consecutive ProjectSet levels.
	ProjectSet
	CatalogScan
	Sample
)

type JoinKind uint8

const (
	InnerJoin JoinKind = iota + 1
	LeftJoin
	RightJoin
	FullJoin
	CrossJoin
	SemiJoin
	AntiJoin
	MarkJoin
)

type ApplyKind uint8

// ScalarApply returns NULL for no right row and raises SQLSTATE 21000 for more
// than one. Its right output is nullable and its effects include MayError.
const (
	ScalarApply ApplyKind = iota + 1
	ExistsApply
	LateralApply
)

type SetKind uint8

const (
	Union SetKind = iota + 1
	Intersect
	Except
)

type SampleMethod uint8

const (
	BernoulliSample SampleMethod = iota + 1
	SystemSample
)

type Materialization uint8

const (
	MaterializeDefault Materialization = iota
	MaterializeAlways
	MaterializeNever
)

type FrameMode uint8

const (
	RowsFrame FrameMode = iota + 1
	RangeFrame
	GroupsFrame
)

type BoundKind uint8

const (
	UnboundedPreceding BoundKind = iota + 1
	OffsetPreceding
	CurrentRow
	OffsetFollowing
	UnboundedFollowing
)

type FrameExclusion uint8

const (
	ExcludeNone FrameExclusion = iota
	ExcludeCurrent
	ExcludeGroup
	ExcludeTies
)

type FrameBound struct {
	Kind    BoundKind
	Offset  ScalarID
	InRange SignatureID
}
type Frame struct {
	Mode       FrameMode
	Start, End FrameBound
	Exclusion  FrameExclusion
}
type Direction uint8

const (
	Ascending Direction = iota + 1
	Descending
)

type NullOrder uint8

const (
	NullsFirst NullOrder = iota + 1
	NullsLast
)

type OrderKey struct {
	Expr      ScalarID
	Direction Direction
	Nulls     NullOrder
	Operator  model.OperatorID
	Collation model.CollationID
}
type Binding struct {
	Slot model.SlotID
	Expr ScalarID
}
type AggregateCall struct {
	Signature   SignatureID
	Function    model.FunctionID
	Result      model.SlotID
	Distinct    bool
	Filter      ScalarID
	Effects     Effects
	direct      extent
	args, order extent
}
type AggregateSpec struct {
	AggregateCall
	Direct []ScalarID
	Args   []ScalarID
	Order  []OrderKey
}

// GroupingCall arguments are grouping-key indices, not evaluated scalars.
type GroupingCall struct {
	Result model.SlotID
	args   extent
}
type GroupingSpec struct {
	GroupingCall
	Args []uint32
}
type WindowCall struct {
	Signature SignatureID
	Function  model.FunctionID
	Result    model.SlotID
	Filter    ScalarID
	Effects   Effects
	args      extent
}
type WindowSpec struct {
	WindowCall
	Args []ScalarID
}
type SearchMode uint8

const (
	NoSearch SearchMode = iota
	BreadthFirst
	DepthFirst
)

type Recursion struct {
	Search                                 SearchMode
	SearchSlot                             model.SlotID
	CycleMark, CyclePath                   model.SlotID
	CycleValue, CycleDefault, CycleContinue ScalarID
	searchKeys, cycleKeys                  extent
}
type RecursionSpec struct {
	Recursion
	SearchKeys, CycleKeys []model.SlotID
}
type LockStrength uint8

const (
	KeyShare LockStrength = iota + 1
	Share
	NoKeyUpdate
	UpdateLock
)

type LockWait uint8

const (
	Wait LockWait = iota
	NoWait
	SkipLocked
)

type RowLock struct {
	Object                                                   model.ObjectID
	Occurrence                                               model.ScanOccurrence
	ObjectSlot, DescriptorVersionSlot, GenerationSlot         model.SlotID
	EncodingVersionSlot, RowSlot                              model.SlotID
	Strength                                                  LockStrength
	Wait                                                      LockWait
	keys                                                      extent
}
type RowLockKey struct {
	Descending, NullsFirst bool
}
type RowLockSpec struct {
	RowLock
	Keys []RowLockKey
}
type ModifyKind uint8

const (
	Insert ModifyKind = iota + 1
	Update
	Delete
	Merge
	DoNothing
)

type ModifyTargetKind uint8

const (
	PhysicalModifyTarget ModifyTargetKind = iota + 1
	LogicalViewModifyTarget
	AutomaticViewModifyTarget
	AutomaticViewTriggerModifyTarget
)

type ViewMutationStep struct {
	View        model.ObjectID
	Predicate   ScalarID
	Check       bool
	inputs      extent
	projections, outputColumns extent
}

type ViewMutationStepSpec struct {
	ViewMutationStep
	Inputs      []model.SlotID
	Projections []ScalarID
	OutputColumns []model.ColumnID
}

type ConflictAction uint8

const (
	NoConflict ConflictAction = iota
	ConflictNothing
	ConflictUpdate
)

type MatchKind uint8

const (
	Matched MatchKind = iota + 1
	NotMatchedByTarget
	NotMatchedBySource
)

type MergeAction struct {
	Match       MatchKind
	Action      ModifyKind
	Condition   ScalarID
	assignments extent
}
type MergeActionSpec struct {
	MergeAction
	Assignments []Binding
}

type JSONTableColumnKind uint8

const (
	JSONTableOrdinality JSONTableColumnKind = iota + 1
	JSONTableExists
	JSONTableValue
	JSONTableQuery
	JSONTableNested
)

// JSONTableColumn is a flattened pre-order column tree. Parent and Output are
// one-based indices; zero denotes the table root or a non-output nested node.
// Path is a one-based index into the owning relation's Expressions.
type JSONTableColumn struct {
	Kind         JSONTableColumnKind
	Parent       uint16
	Output       uint16
	Path         uint32
	Layout       LayoutID
	JSONFormat   SQLJSONFormat
	JSONEncoding SQLJSONEncoding
	JSONWrapper  SQLJSONWrapper
	JSONQuotes   SQLJSONQuotes
	JSONFlags    SQLJSONFlags
}

// Rel metadata expresses SQL semantics, not access paths, costing, distribution,
// required ordering, or cached cardinality estimates. Sort keys are semantic.
// Outputs are explicit: outer joins use newly declared nullable output slots.
type Rel struct {
	Kind              RelKind
	Scope             ScopeID
	Span              Span
	Effects           Effects
	SecurityBarrier   bool
	Rows              uint64
	Occurrence        model.ScanOccurrence
	Object            model.ObjectID
	DescriptorVersion uint64
	Generation        uint64
	EncodingVersion   uint32
	Catalog           model.CatalogRelationID
	JoinKind          JoinKind
	ApplyKind         ApplyKind
	LateralOuter      bool
	UnnestOrdinality  bool
	// RowFunction selects a set-returning row source instead of array or multirange unnest.
	RowFunction      model.FunctionID
	RowResult        LayoutID
	RowCollation     model.CollationID
	RowStrict        bool
	RowVariadicArray bool
	// UnnestRange declares a multirange's range result; zero with no RowFunction
	// or JSONTableRoot selects array unnest.
	UnnestRange model.Type
	// JSONTableRoot is a one-based index into Expressions. JSONTableArguments is
	// the evaluated prefix (context plus PASSING metadata/values).
	JSONTableRoot      uint32
	JSONTableArguments uint16
	JSONTablePassing   uint16
	JSONTableFormat    SQLJSONFormat
	JSONTableEncoding  SQLJSONEncoding
	JSONTableFlags     SQLJSONFlags
	RowsFromOrdinality                                                                                                                                 bool
	SetKind                                                                                                                                            SetKind
	All                                                                                                                                                bool
	DistinctOn                                                                                                                                         bool
	SampleMethod                                                                                                                                       SampleMethod
	SampleRepeatable                                                                                                                                   bool
	CTE                                                                                                                                                CTEID
	RecursiveReference                                                                                                                                 bool
	Predicate                                                                                                                                          ScalarID
	Offset, Count                                                                                                                                      ScalarID
	WithTies                                                                                                                                           bool
	Frame                                                                                                                                              Frame
	Recursion                                                                                                                                          Recursion
	ModifyKind                                                                                                                                         ModifyKind
	ModifyTarget                                                                                                                                       ModifyTargetKind
	Conflict                                                                                                                                           ConflictAction
	ConflictPredicate                                                                                                                                  ScalarID
	OldRow, NewRow, SourceObject, OldTableOID, NewTableOID                                                                                              model.SlotID
	LogicalView                                                                                                                                        model.ObjectID
	ExcludedRow                                                                                                                                        model.SlotID
	conflictArbiters                                                                                                                                   extent
	MergeSourcePresent, MergeTargetPresent                                                                                                             model.SlotID
	MergeResultRow, MergeActionSlot, MergeTableOID                                                                                                     model.SlotID
	targetKeys                                                                                                                                         extent
	updatedColumns                                                                                                                                     extent
	MarkSlot                                                                                                                                           model.SlotID
	MarkKind                                                                                                                                           SubqueryKind
	inputs, expressions, outputs, bindings, assignments, conflictAssignments, order, groups, aggregates, groupings, windows, partition, locks, actions, jsonTableColumns, viewMutationSteps extent
}

// Expressions holds row-major Values cells, Aggregate grouping expressions, or
// DistinctOn keys. Bindings maps Project/Aggregate/Window outputs and RETURNING.
// Join/Apply outputs map positionally to left then right (or mark) outputs.
// Assignments and ConflictAssignments identify target columns through SlotInfo.
// UpdatedColumns records syntactic UPDATE targets for UPDATE OF trigger matching,
// including generated columns whose DEFAULT assignment emits no write expression.
// Aggregate grouping sets contain zero-based indices into Expressions.
type RelSpec struct {
	Rel
	Inputs                           []RelID
	Expressions                      []ScalarID
	Outputs                          []model.SlotID
	TargetKeys                       []model.SlotID
	Bindings                         []Binding
	Assignments, ConflictAssignments []Binding
	UpdatedColumns                   []model.ColumnID
	ConflictArbiters                 []model.ConflictArbiter
	Order                            []OrderKey
	GroupingSets                     [][]uint32
	Aggregates                       []AggregateSpec
	Groupings                        []GroupingSpec
	Windows                          []WindowSpec
	Partition                        []ScalarID
	Locks                            []RowLockSpec
	RecursionSpec                    RecursionSpec
	Actions                          []MergeActionSpec
	JSONTableColumns                 []JSONTableColumn
	ViewMutationSteps                 []ViewMutationStepSpec
}

type CTE struct {
	Scope           ScopeID
	Materialization Materialization
	Recursive       bool
	Body            RelID
	Effects         Effects
	outputs         extent
}
type CTESpec struct {
	Scope           ScopeID
	Outputs         []model.SlotID
	Materialization Materialization
	Recursive       bool
	Effects         Effects
}

type Limits struct {
	// MaxNodes is graph-wide; MaxOperands bounds each typed operand arena.
	// MaxDepth is positive and at most 512; nested SQL values/types also cap at 64.
	MaxNodes, MaxOperands, MaxSlots, MaxDepth uint32
	MaxBytes                                  uint64
	SourceBytes                               uint32
}
