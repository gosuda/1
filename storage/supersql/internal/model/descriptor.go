package model

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
	"unsafe"
)

type OID uint32
type ObjectKind uint8

const (
	ObjectSchema ObjectKind = iota + 1
	ObjectTable
	ObjectIndex
	ObjectSequence
	ObjectView
	ObjectMaterializedView
	ObjectRoutine
	ObjectType
	ObjectTextSearchDictionary
	ObjectTextSearchConfiguration
	ObjectTextSearchTemplate
	ObjectCompositeRelation
	ObjectCast
	ObjectOperator
)

// SignatureBearing reports whether the catalog namespace key includes an
// overload signature. Aggregates share PostgreSQL's pg_proc name namespace with
// routines, so ObjectRoutine covers both; operators key on left/right types.
func (k ObjectKind) SignatureBearing() bool {
	return k == ObjectRoutine || k == ObjectOperator
}

type SchemaState uint8

const (
	StateAbsent SchemaState = iota
	StateDeleteOnly
	StateWriteOnly
	StatePublic
	StateDropWriteOnly
	StateDropDeleteOnly
)

// Namespace names are already normalized by the SQL binder. Signature separates
// overloaded routines; it is empty for other object kinds.
type NamespaceName struct {
	Parent    ObjectID
	Kind      ObjectKind
	Name      string
	Signature string
}
type Dependency struct {
	Object  ObjectID
	Version uint64
	Column  ColumnID
}

// ExpressionDefinition is semantic input, not an executable program. A binder
// must validate its encoding version and dependencies before execution.
type ExpressionDefinition struct {
	SQL             string
	Dependencies    []Dependency
	SemanticVersion uint32
	Encoding        []byte
}
type PhysicalIdentity struct {
	Generation      uint64
	EncodingVersion uint32
}
type ColumnInheritance struct {
	Table  ObjectID
	Column ColumnID
}
type ColumnDescriptor struct {
	ID                  ColumnID
	Name                string
	Description         string
	StatisticsTarget    int32
	StatisticsTargetSet bool
	State               SchemaState
	Layout              TypeLayout
	Nullable            bool
	InheritedFrom       []ColumnInheritance
	Local               bool
	// SystemIdentity is an engine-only physical row key, never a SQL column.
	SystemIdentity   bool
	ConstantDefault  *Value
	Default          *ExpressionDefinition
	DefaultOID       OID
	Generated        *ExpressionDefinition
	GeneratedStored  bool
	IdentitySequence ObjectID
	IdentityAlways   bool
}
type KeyElement struct {
	Column     ColumnID
	Expression *ExpressionDefinition
	// ExpressionName is the persisted pg_attribute name of an index expression.
	// It is empty for column and partition keys.
	ExpressionName      string
	Type                Type
	Children            []TypeLayout `json:",omitempty"`
	OperatorClass       OID
	Collation           CollationID
	Descending          bool
	StatisticsTarget    int32
	StatisticsTargetSet bool
	NullsFirst          bool
}
type ConstraintKind uint8

const (
	ConstraintCheck ConstraintKind = iota + 1
	ConstraintUnique
	ConstraintPrimaryKey
	ConstraintForeignKey
	ConstraintExclude
	ConstraintNotNull
	ConstraintTrigger
)

type ConstraintDescriptor struct {
	ID                             ObjectID
	OID                            OID
	Name                           string
	Description                    string
	Kind                           ConstraintKind
	Columns                        []ColumnID
	ReferencedObject               ObjectID
	ReferencedColumns              []ColumnID
	ReferencedConstraint           ObjectID
	ReferencedIndex                ObjectID
	Expression                     *ExpressionDefinition
	Index                          ObjectID
	ExcludeOperators               []OperatorID
	ParentForeignEqualityOperators []OperatorID
	ParentEqualityOperators        []OperatorID
	ForeignEqualityOperators       []OperatorID
	Deferrable                     bool
	InitiallyDeferred              bool
	Validated                      bool
	Enforced                       bool
	Mutation                       bool
	NoInherit                      bool
	ParentTable                    ObjectID
	ParentConstraint               ObjectID
	InheritanceCount               uint32
	Local                          bool
	OnDelete                       string
	OnDeleteColumns                []ColumnID
	OnUpdate                       string
	OnUpdateColumns                []ColumnID
	Match                          string
}
type TriggerEnableMode uint8

const (
	TriggerDisabled TriggerEnableMode = iota + 1
	TriggerEnabledOrigin
	TriggerEnabledReplica
	TriggerEnabledAlways
)

func (mode TriggerEnableMode) Fires(replica bool) bool {
	if replica {
		return mode == TriggerEnabledReplica || mode == TriggerEnabledAlways
	}
	return mode == TriggerEnabledOrigin || mode == TriggerEnabledAlways
}

type TriggerDescriptor struct {
	ID                 ObjectID
	OID                OID
	Name               string
	Description        string
	Routine            ObjectID
	Function           FunctionID
	SchemaName         string
	Timing             string
	Events             []string
	Columns            []ColumnID
	When               *ExpressionDefinition
	Arguments          []string
	ForEachRow         bool
	EnableMode         TriggerEnableMode
	Internal           bool
	Constraint         bool
	Deferrable         bool
	InitiallyDeferred  bool
	ConstraintTable    ObjectID
	ConstraintID       ObjectID
	ConstraintRelation ObjectID
	ConstraintIndex    ObjectID
	OldTransitionTable string
	NewTransitionTable string
}
type ExtendedStatisticDescriptor struct {
	OID         OID
	Name        string
	Description string
	Owner       OID
	Namespace   ObjectID
	Target      int32
	TargetSet   bool
	Keys        []ColumnID
	Kinds       []byte
	Expressions []*ExpressionDefinition
}
type TypedColumnMapping struct {
	Field  ColumnID
	Column ColumnID
}
type PartitionDatumKind uint8

const (
	PartitionDatumValue PartitionDatumKind = iota + 1
	PartitionDatumMin
	PartitionDatumMax
)

// PartitionDatum keeps the resolved SQL type and collation beside its physical
// value. Min/max datums have an empty Value and remain ordered sentinels.
type PartitionDatum struct {
	Kind   PartitionDatumKind
	Layout TypeLayout
	Value  Value
}

// PartitionBound is the immutable child-to-parent routing contract. Strategy is
// the parent's strategy. ListValues are individual one-key LIST values; Lower
// and Upper are lexicographic RANGE endpoints. RANGE lower is inclusive and
// upper is exclusive. HASH uses 0 <= Remainder < Modulus.
type PartitionBound struct {
	Strategy   byte
	Default    bool
	Definition string
	Modulus    uint32
	Remainder  uint32
	ListValues []PartitionDatum
	Lower      []PartitionDatum
	Upper      []PartitionDatum
}

type PartitionAttachmentState uint8

const (
	PartitionAttached PartitionAttachmentState = iota + 1
	PartitionDetachPending
)

type TableMetadata struct {
	Owner OID
	// ColumnIDHighWatermark includes removed and reserved IDs. It never decreases.
	ColumnIDHighWatermark ColumnID
	Columns               []ColumnDescriptor
	PrimaryKey            []KeyElement
	Indexes               []ObjectID
	Constraints           []ConstraintDescriptor
	Triggers              []TriggerDescriptor
	Parents               []ObjectID
	Partition             *ExpressionDefinition
	PartitionStrategy     byte
	PartitionKeys         []KeyElement
	PartitionDefault      ObjectID
	PartitionBound        *PartitionBound
	PartitionState        PartitionAttachmentState
	InheritanceChildren   uint32
	TypedOf               ObjectID
	RowType               ObjectID
	TypedColumns          []TypedColumnMapping
	ExtendedStatistics    []ExtendedStatisticDescriptor
	Persistence           string
	OnCommit              string
}
type SchemaMetadata struct{ Owner OID }
type IndexMetadata struct {
	Owner               OID
	Table               ObjectID
	Keys                []KeyElement
	Include             []ColumnID
	Predicate           *ExpressionDefinition
	Unique              bool
	NullsNotDistinct    bool
	DeferredUnique      bool
	PendingConstraint   *ConstraintDescriptor
	PendingDependencies []Dependency
	ParentIndex         ObjectID
	// Inverse lifecycle flags preserve the zero value as an ordinary usable
	// index for descriptors written before lifecycle publication existed.
	Invalid  bool
	NotReady bool
	NotLive  bool
}

func (m IndexMetadata) Valid() bool { return !m.Invalid }
func (m IndexMetadata) Ready() bool { return !m.NotReady }
func (m IndexMetadata) Live() bool  { return !m.NotLive }
func (m IndexMetadata) WriteMaintained() bool {
	return m.Ready() && m.Live()
}

type SequenceMetadata struct {
	Owner OID
	// Identity survives RESTART; Incarnation selects the current allocation state.
	Identity, Incarnation             uint64
	Type                              Type
	Start, Increment, Min, Max, Cache int64
	Cycle                             bool
	OwnerTable                        ObjectID
	OwnerColumn                       ColumnID
	Persistence                       string
}

// ViewMutationColumn maps one view output to its immediate base-relation
// column. Computed outputs are intentionally absent from the mapping.
type ViewMutationColumn struct {
	View ColumnID
	Base ColumnID
}

// ViewMutationMetadata is the declaration-time semantic proof that a view has
// the PostgreSQL automatically-updatable query shape.
type ViewMutationMetadata struct {
	BaseRelation ObjectID
	BaseOID      OID
	Columns      []ViewMutationColumn
}

type ViewMetadata struct {
	Owner           OID
	Columns         []ColumnDescriptor
	Query           ExpressionDefinition
	Definition      string
	RowType         ObjectID
	Triggers        []TriggerDescriptor
	Recursive       bool
	CheckOption     string
	Populated       bool
	Persistence     string
	SecurityBarrier bool
	SecurityInvoker bool
	Automatic       *ViewMutationMetadata
}
type CompositeRelationMetadata struct {
	Owner                OID
	RowType              ObjectID
	FieldIDHighWatermark ColumnID
	Columns              []ColumnDescriptor
}
type RoutineParameter struct {
	Name    string
	Layout  TypeLayout
	Mode    string
	Default *ExpressionDefinition
}
type RoutineSetting struct {
	Name  string
	Value string
	Reset bool
}
type RoutineMetadata struct {
	Parameters      []RoutineParameter
	Settings        []RoutineSetting
	Result          TypeLayout
	Owner           OID
	LanguageOID     OID
	Body            ExpressionDefinition
	Language        string
	Volatility      string
	Parallel        string
	Cost            float64
	Rows            float64
	Strict          bool
	ReturnsSet      bool
	Procedure       bool
	Atomic          bool
	SecurityDefiner bool
	Leakproof       bool
	Support         ObjectID
	SupportFunction FunctionID
	Source          string
	Binary          string
	ExternalCode    string
}
type TypeKind uint8

const (
	TypeKindDomain TypeKind = iota + 1
	TypeKindEnum
	TypeKindComposite
	TypeKindRange
	TypeKindMultirange
	TypeKindArray
	TypeKindShell
)

type RangeTypeMetadata struct {
	Subtype            TypeLayout
	OperatorClass      OID
	Collation          CollationID
	Canonical          ObjectID
	Difference         ObjectID
	CanonicalFunction  FunctionID
	DifferenceFunction FunctionID
	Range              ObjectID
	Multirange         ObjectID
}

type TypeMetadata struct {
	Kind                 TypeKind
	Owner                OID
	Layout               TypeLayout
	Array                ObjectID
	Relation             ObjectID
	Base                 ObjectID
	BaseLayout           TypeLayout
	Default              *ExpressionDefinition
	NotNull              bool
	Elements             []string
	ElementOIDs          []OID
	UnsafeElementOIDs    []OID
	FieldIDHighWatermark ColumnID
	Fields               []ColumnDescriptor
	Constraints          []ConstraintDescriptor
	Range                *RangeTypeMetadata
}
type TextSearchTemplateMetadata struct {
	Owner  OID
	Init   OID
	Lexize OID
}

type TextSearchDictionaryMetadata struct {
	Owner             OID
	Template          OID
	Kind              TextSearchDictionaryKind
	Language          string
	StopWords         []string
	StopWordsLanguage string
	Accept            bool
	InitOptions       string
	// DictionaryFile and AffixFile are pinned tsearch_data basenames for an
	// Ispell dictionary; empty means the option was omitted.
	DictionaryFile string
	AffixFile      string
	// SynonymsFile is the pinned tsearch_data basename for a Synonym dictionary.
	SynonymsFile string
	// CaseSensitive selects exact-case matching for a Synonym dictionary.
	CaseSensitive bool
	// ThesaurusFile is the pinned tsearch_data basename for a Thesaurus
	// dictionary and SubDictionary names its normalizing sub-dictionary.
	ThesaurusFile string
	SubDictionary OID
}
type TextSearchMappingReference struct {
	TokenType    uint16
	Dictionaries []OID
}
type TextSearchConfigurationMetadata struct {
	Owner    OID
	Parser   OID
	Mappings []TextSearchMappingReference
}
type DescriptorSpec struct {
	ID                      ObjectID
	OID                     OID
	Name                    NamespaceName
	Description             string
	Version                 uint64
	State                   SchemaState
	Physical                PhysicalIdentity
	Dependencies            []Dependency
	Schema                  *SchemaMetadata
	Table                   *TableMetadata
	Index                   *IndexMetadata
	Sequence                *SequenceMetadata
	View                    *ViewMetadata
	Routine                 *RoutineMetadata
	Type                    *TypeMetadata
	CompositeRelation       *CompositeRelationMetadata
	TextSearchTemplate      *TextSearchTemplateMetadata
	TextSearchDictionary    *TextSearchDictionaryMetadata
	TextSearchConfiguration *TextSearchConfigurationMetadata
	Cast                    *CastMetadata
	Operator                *OperatorMetadata
	Aggregate               *AggregateMetadata
}

// Limits bound each owned descriptor, including nested type/default payloads.
// MaxBytes includes metadata headers; zero never means unlimited.
type DescriptorLimits struct {
	MaxBytes uint64
	MaxItems int
	MaxDepth int
}

func (l DescriptorLimits) Validate() error {
	if l.MaxBytes == 0 || l.MaxItems <= 0 || l.MaxDepth <= 0 || l.MaxDepth > 64 {
		return ErrDescriptorLimit
	}
	return nil
}

var (
	ErrInvalidDescriptor = errors.New("invalid logical descriptor")
	ErrDescriptorLimit   = errors.New("descriptor exceeds metadata limits")
)

type Descriptor struct {
	spec        DescriptorSpec
	limits      DescriptorLimits
	provisional *ProvisionalBinding
}

// WithProvisionalBinding preserves the immutable spec without retaining its reader.
func (d *Descriptor) WithProvisionalBinding(binding *ProvisionalBinding) *Descriptor {
	copy := *d
	copy.provisional = binding
	return &copy
}

func (d *Descriptor) ProvisionalBinding() *ProvisionalBinding { return d.provisional }

func NewDescriptor(ctx context.Context, spec DescriptorSpec, limits DescriptorLimits) (*Descriptor, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	copier := descriptorCopier{ctx: ctx, limits: limits}
	owned, err := copier.copy(spec)
	if err != nil {
		return nil, err
	}
	if err := validateDescriptor(owned); err != nil {
		return nil, err
	}
	return &Descriptor{spec: owned, limits: limits}, nil
}
func (d *Descriptor) ID() ObjectID               { return d.spec.ID }
func (d *Descriptor) OID() OID                   { return d.spec.OID }
func (d *Descriptor) Version() uint64            { return d.spec.Version }
func (d *Descriptor) Kind() ObjectKind           { return d.spec.Name.Kind }
func (d *Descriptor) Name() NamespaceName        { return d.spec.Name }
func (d *Descriptor) State() SchemaState         { return d.spec.State }
func (d *Descriptor) Physical() PhysicalIdentity { return d.spec.Physical }
func (d *Descriptor) Description() string        { return d.spec.Description }

// Sequence returns scalar metadata by value without copying the descriptor.
func (d *Descriptor) Sequence() (SequenceMetadata, bool) {
	if d == nil || d.spec.Sequence == nil {
		return SequenceMetadata{}, false
	}
	return *d.spec.Sequence, true
}

// Index returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Index() (IndexMetadata, bool) {
	if d == nil || d.spec.Index == nil {
		return IndexMetadata{}, false
	}
	return *d.spec.Index, true
}

// Table returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Table() (TableMetadata, bool) {
	if d == nil || d.spec.Table == nil {
		return TableMetadata{}, false
	}
	return *d.spec.Table, true
}

// View returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) View() (ViewMetadata, bool) {
	if d == nil || d.spec.View == nil {
		return ViewMetadata{}, false
	}
	return *d.spec.View, true
}

// CompositeRelation returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) CompositeRelation() (CompositeRelationMetadata, bool) {
	if d == nil || d.spec.CompositeRelation == nil {
		return CompositeRelationMetadata{}, false
	}
	return *d.spec.CompositeRelation, true
}

// Routine returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Routine() (RoutineMetadata, bool) {
	if d == nil || d.spec.Routine == nil {
		return RoutineMetadata{}, false
	}
	return *d.spec.Routine, true
}

// DeclaredType returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) DeclaredType() (TypeMetadata, bool) {
	if d == nil || d.spec.Type == nil {
		return TypeMetadata{}, false
	}
	return *d.spec.Type, true
}

// TextSearchTemplate returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) TextSearchTemplate() (TextSearchTemplateMetadata, bool) {
	if d == nil || d.spec.TextSearchTemplate == nil {
		return TextSearchTemplateMetadata{}, false
	}
	return *d.spec.TextSearchTemplate, true
}

// TextSearchDictionary returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) TextSearchDictionary() (TextSearchDictionaryMetadata, bool) {
	if d == nil || d.spec.TextSearchDictionary == nil {
		return TextSearchDictionaryMetadata{}, false
	}
	return *d.spec.TextSearchDictionary, true
}

// TextSearchConfiguration returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) TextSearchConfiguration() (TextSearchConfigurationMetadata, bool) {
	if d == nil || d.spec.TextSearchConfiguration == nil {
		return TextSearchConfigurationMetadata{}, false
	}
	return *d.spec.TextSearchConfiguration, true
}

// Cast returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Cast() (CastMetadata, bool) {
	if d == nil || d.spec.Cast == nil {
		return CastMetadata{}, false
	}
	return *d.spec.Cast, true
}

// Operator returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Operator() (OperatorMetadata, bool) {
	if d == nil || d.spec.Operator == nil {
		return OperatorMetadata{}, false
	}
	return *d.spec.Operator, true
}

// Aggregate returns immutable metadata borrowed from the descriptor.
func (d *Descriptor) Aggregate() (AggregateMetadata, bool) {
	if d == nil || d.spec.Aggregate == nil {
		return AggregateMetadata{}, false
	}
	return *d.spec.Aggregate, true
}

// Spec returns a bounded, deeply owned editable copy.
func (d *Descriptor) Spec(ctx context.Context) (DescriptorSpec, error) {
	if d == nil {
		return DescriptorSpec{}, ErrInvalidDescriptor
	}
	return d.SpecWithin(ctx, d.limits)
}

// SpecWithin clones directly under the receiving owner's limits, without first
// allocating a copy under the descriptor creator's potentially larger budget.
func (d *Descriptor) SpecWithin(ctx context.Context, limits DescriptorLimits) (DescriptorSpec, error) {
	spec, _, err := d.SpecWithinBytes(ctx, limits)
	return spec, err
}

// SpecWithinBytes also reports the bytes admitted for the owned copy.
func (d *Descriptor) SpecWithinBytes(ctx context.Context, limits DescriptorLimits) (DescriptorSpec, uint64, error) {
	if d == nil || d.spec.ID == 0 {
		return DescriptorSpec{}, 0, ErrInvalidDescriptor
	}
	if err := limits.Validate(); err != nil {
		return DescriptorSpec{}, 0, err
	}
	copier := descriptorCopier{ctx: ctx, limits: limits}
	spec, err := copier.copy(d.spec)
	if err != nil {
		return spec, copier.used, err
	}
	return spec, copier.used, err
}

type descriptorCopier struct {
	ctx    context.Context
	limits DescriptorLimits
	used   uint64
	err    error
}

func (c *descriptorCopier) charge(n uint64) bool {
	if c.err != nil {
		return false
	}
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return false
	}
	if n > c.limits.MaxBytes-c.used {
		c.err = ErrDescriptorLimit
		return false
	}
	c.used += n
	return true
}
func descriptorSlice[T any](c *descriptorCopier, in []T) []T {
	if len(in) > c.limits.MaxItems || !c.charge(uint64(len(in))*uint64(unsafe.Sizeof(*new(T)))) {
		if c.err == nil {
			c.err = ErrDescriptorLimit
		}
		return nil
	}
	return slices.Clone(in)
}
func (c *descriptorCopier) text(s string) string {
	if !c.charge(uint64(len(s))) {
		return ""
	}
	if !utf8.ValidString(s) {
		c.err = ErrInvalidDescriptor
		return ""
	}
	return strings.Clone(s)
}
func (c *descriptorCopier) blob(b []byte) []byte {
	if !c.charge(uint64(len(b))) {
		return nil
	}
	return bytes.Clone(b)
}
func (c *descriptorCopier) expression(in *ExpressionDefinition) *ExpressionDefinition {
	if in == nil {
		return nil
	}
	if !c.charge(uint64(unsafe.Sizeof(*in))) {
		return nil
	}
	out := *in
	out.SQL = c.text(in.SQL)
	out.Encoding = c.blob(in.Encoding)
	out.Dependencies = c.dependencies(in.Dependencies)
	if in.SemanticVersion == 0 || (in.SQL == "" && len(in.Encoding) == 0) {
		c.err = ErrInvalidDescriptor
	}
	if err := validateDependencies(out.Dependencies); err != nil {
		c.err = err
	}
	return &out
}
func (c *descriptorCopier) dependencies(in []Dependency) []Dependency {
	out := descriptorSlice(c, in)
	slices.SortFunc(out, func(a, b Dependency) int {
		if n := cmp.Compare(a.Object, b.Object); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Version, b.Version); n != 0 {
			return n
		}
		return cmp.Compare(a.Column, b.Column)
	})
	return out
}
func (c *descriptorCopier) layout(in TypeLayout, depth int) TypeLayout {
	return c.copyLayout(in, depth, false)
}

func (c *descriptorCopier) copyLayout(in TypeLayout, depth int, physical bool) TypeLayout {
	if depth > c.limits.MaxDepth {
		c.err = ErrDescriptorLimit
		return TypeLayout{}
	}
	if physical {
		in.FieldNames = nil
	}
	out := in
	out.Children = descriptorSlice(c, in.Children)
	out.FieldNames = nil
	if len(in.FieldNames) > c.limits.MaxItems {
		c.err = ErrDescriptorLimit
		return TypeLayout{}
	}
	if !c.charge(uint64(len(in.FieldNames)) * uint64(unsafe.Sizeof(""))) {
		return TypeLayout{}
	}
	if len(in.FieldNames) != 0 {
		out.FieldNames = make([]string, len(in.FieldNames))
	}
	for i, name := range in.FieldNames {
		out.FieldNames[i] = c.text(name)
	}
	for i := range out.Children {
		out.Children[i] = c.copyLayout(in.Children[i], depth+1, physical)
	}
	return out
}
func (c *descriptorCopier) columns(in []ColumnDescriptor) []ColumnDescriptor {
	out := descriptorSlice(c, in)
	for i := range out {
		x := &out[i]
		x.Name = c.text(x.Name)
		x.Description = c.text(x.Description)
		x.InheritedFrom = descriptorSlice(c, x.InheritedFrom)
		x.Layout = c.layout(x.Layout, 1)
		x.Default = c.expression(x.Default)
		x.Generated = c.expression(x.Generated)
		if x.ConstantDefault != nil && c.err == nil {
			nodes := (c.limits.MaxBytes - c.used) / uint64(unsafe.Sizeof(Value{}))
			if err := c.valueShape(*x.ConstantDefault, 1, &nodes); err != nil {
				c.err = err
				continue
			}
			if !c.charge(uint64(unsafe.Sizeof(Value{}))) {
				continue
			}
			value, n, err := CopyValue(c.ctx, *x.ConstantDefault, c.limits.MaxBytes-c.used)
			if err != nil {
				c.err = err
				continue
			}
			c.used += n
			x.ConstantDefault = &value
		}
	}
	return out
}
func (c *descriptorCopier) valueShape(value Value, depth int, nodes *uint64) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if *nodes == 0 || depth > c.limits.MaxDepth || len(value.ArrayVal) > c.limits.MaxItems || len(value.Dimensions) > c.limits.MaxItems {
		return ErrDescriptorLimit
	}
	*nodes--
	for _, child := range value.ArrayVal {
		if err := c.valueShape(child, depth+1, nodes); err != nil {
			return err
		}
	}
	return nil
}
func (c *descriptorCopier) keys(in []KeyElement) []KeyElement {
	out := descriptorSlice(c, in)
	for i := range out {
		out[i].ExpressionName = c.text(out[i].ExpressionName)
		out[i].Expression = c.expression(out[i].Expression)
		out[i].Children = c.copyLayout(TypeLayout{Type: out[i].Type, Children: out[i].Children}, 1, true).Children
	}
	return out
}
func (c *descriptorCopier) constraints(in []ConstraintDescriptor) []ConstraintDescriptor {
	out := descriptorSlice(c, in)
	for i := range out {
		x := &out[i]
		x.Name = c.text(x.Name)
		x.Columns = descriptorSlice(c, x.Columns)
		x.Description = c.text(x.Description)
		x.ReferencedColumns = descriptorSlice(c, x.ReferencedColumns)
		x.ExcludeOperators = descriptorSlice(c, x.ExcludeOperators)
		x.ParentForeignEqualityOperators = descriptorSlice(c, x.ParentForeignEqualityOperators)
		x.ParentEqualityOperators = descriptorSlice(c, x.ParentEqualityOperators)
		x.ForeignEqualityOperators = descriptorSlice(c, x.ForeignEqualityOperators)
		x.OnDeleteColumns = descriptorSlice(c, x.OnDeleteColumns)
		x.OnUpdateColumns = descriptorSlice(c, x.OnUpdateColumns)
		x.Expression = c.expression(x.Expression)
		x.OnDelete = c.text(x.OnDelete)
		x.OnUpdate = c.text(x.OnUpdate)
		x.Match = c.text(x.Match)
	}
	return out
}
func (c *descriptorCopier) triggers(in []TriggerDescriptor) []TriggerDescriptor {
	out := descriptorSlice(c, in)
	for i := range out {
		t := &out[i]
		t.Name = c.text(t.Name)
		t.SchemaName = c.text(t.SchemaName)
		t.Timing = c.text(t.Timing)
		t.Description = c.text(t.Description)
		t.OldTransitionTable = c.text(t.OldTransitionTable)
		t.NewTransitionTable = c.text(t.NewTransitionTable)
		t.Columns = descriptorSlice(c, t.Columns)
		t.Events = descriptorSlice(c, t.Events)
		for j := range t.Events {
			t.Events[j] = c.text(t.Events[j])
		}
		t.Arguments = descriptorSlice(c, t.Arguments)
		for j := range t.Arguments {
			t.Arguments[j] = c.text(t.Arguments[j])
		}
		t.When = c.expression(t.When)
	}
	return out
}
func (c *descriptorCopier) partitionDatum(in PartitionDatum) PartitionDatum {
	out := in
	out.Layout = c.layout(in.Layout, 1)
	if in.Kind != PartitionDatumValue || c.err != nil {
		out.Value = Value{}
		return out
	}
	nodes := (c.limits.MaxBytes - c.used) / uint64(unsafe.Sizeof(Value{}))
	if err := c.valueShape(in.Value, 1, &nodes); err != nil {
		c.err = err
		return PartitionDatum{}
	}
	value, used, err := CopyValue(c.ctx, in.Value, c.limits.MaxBytes-c.used)
	if err != nil {
		c.err = err
		return PartitionDatum{}
	}
	c.used += used
	out.Value = value
	return out
}

func (c *descriptorCopier) partitionBound(in *PartitionBound) *PartitionBound {
	if in == nil || !c.charge(uint64(unsafe.Sizeof(*in))) {
		return nil
	}
	out := *in
	out.Definition = c.text(in.Definition)
	copyDatums := func(values []PartitionDatum) []PartitionDatum {
		result := descriptorSlice(c, values)
		for i := range result {
			result[i] = c.partitionDatum(result[i])
		}
		return result
	}
	out.ListValues = copyDatums(in.ListValues)
	out.Lower = copyDatums(in.Lower)
	out.Upper = copyDatums(in.Upper)
	return &out
}

func (c *descriptorCopier) copy(in DescriptorSpec) (DescriptorSpec, error) {
	if !c.charge(uint64(unsafe.Sizeof(in))) {
		return DescriptorSpec{}, c.err
	}
	out := in
	out.Name.Name = c.text(in.Name.Name)
	out.Name.Signature = c.text(in.Name.Signature)
	out.Dependencies = c.dependencies(in.Dependencies)
	out.Description = c.text(in.Description)
	if in.Schema != nil && c.charge(uint64(unsafe.Sizeof(*in.Schema))) {
		x := *in.Schema
		out.Schema = &x
	}
	if in.Table != nil && c.charge(uint64(unsafe.Sizeof(*in.Table))) {
		x := *in.Table
		x.Columns = c.columns(x.Columns)
		x.TypedColumns = descriptorSlice(c, x.TypedColumns)
		x.PrimaryKey = c.keys(x.PrimaryKey)
		x.Indexes = descriptorSlice(c, x.Indexes)
		x.Parents = descriptorSlice(c, x.Parents)
		x.Constraints = c.constraints(x.Constraints)
		x.Partition = c.expression(x.Partition)
		x.PartitionKeys = c.keys(x.PartitionKeys)
		x.PartitionBound = c.partitionBound(x.PartitionBound)
		x.ExtendedStatistics = descriptorSlice(c, x.ExtendedStatistics)
		for i := range x.ExtendedStatistics {
			statistic := &x.ExtendedStatistics[i]
			statistic.Name = c.text(statistic.Name)
			statistic.Keys = descriptorSlice(c, statistic.Keys)
			statistic.Kinds = descriptorSlice(c, statistic.Kinds)
			expressions := descriptorSlice(c, statistic.Expressions)
			statistic.Description = c.text(statistic.Description)
			for j := range expressions {
				expressions[j] = c.expression(expressions[j])
			}
			statistic.Expressions = expressions
		}
		x.Persistence = c.text(x.Persistence)
		x.OnCommit = c.text(x.OnCommit)
		x.Triggers = c.triggers(x.Triggers)
		out.Table = &x
	}
	if in.Index != nil && c.charge(uint64(unsafe.Sizeof(*in.Index))) {
		x := *in.Index
		x.Keys = c.keys(x.Keys)
		x.Include = descriptorSlice(c, x.Include)
		x.Predicate = c.expression(x.Predicate)
		x.PendingDependencies = c.dependencies(x.PendingDependencies)
		if x.PendingConstraint != nil && c.charge(uint64(unsafe.Sizeof(*x.PendingConstraint))) {
			pending := *x.PendingConstraint
			pending.Name = c.text(pending.Name)
			pending.Description = c.text(pending.Description)
			pending.Columns = descriptorSlice(c, pending.Columns)
			pending.ReferencedColumns = descriptorSlice(c, pending.ReferencedColumns)
			pending.ExcludeOperators = descriptorSlice(c, pending.ExcludeOperators)
			pending.ParentForeignEqualityOperators = descriptorSlice(c, pending.ParentForeignEqualityOperators)
			pending.ParentEqualityOperators = descriptorSlice(c, pending.ParentEqualityOperators)
			pending.ForeignEqualityOperators = descriptorSlice(c, pending.ForeignEqualityOperators)
			pending.OnDeleteColumns = descriptorSlice(c, pending.OnDeleteColumns)
			pending.OnUpdateColumns = descriptorSlice(c, pending.OnUpdateColumns)
			pending.Expression = c.expression(pending.Expression)
			pending.OnDelete = c.text(pending.OnDelete)
			pending.OnUpdate = c.text(pending.OnUpdate)
			pending.Match = c.text(pending.Match)
			x.PendingConstraint = &pending
		}
		out.Index = &x
	}
	if in.Sequence != nil && c.charge(uint64(unsafe.Sizeof(*in.Sequence))) {
		x := *in.Sequence
		x.Persistence = c.text(x.Persistence)
		out.Sequence = &x
	}
	if in.View != nil && c.charge(uint64(unsafe.Sizeof(*in.View))) {
		x := *in.View
		x.Columns = c.columns(x.Columns)
		x.Definition = c.text(x.Definition)
		x.Persistence = c.text(x.Persistence)
		x.CheckOption = c.text(x.CheckOption)
		if x.Automatic != nil && c.charge(uint64(unsafe.Sizeof(*x.Automatic))) {
			automatic := *x.Automatic
			automatic.Columns = descriptorSlice(c, automatic.Columns)
			x.Automatic = &automatic
		}
		e := c.expression(&x.Query)
		if e != nil {
			x.Query = *e
		}
		x.Triggers = c.triggers(x.Triggers)
		out.View = &x
	}
	if in.CompositeRelation != nil && c.charge(uint64(unsafe.Sizeof(*in.CompositeRelation))) {
		x := *in.CompositeRelation
		x.Columns = c.columns(x.Columns)
		out.CompositeRelation = &x
	}
	if in.Routine != nil && c.charge(uint64(unsafe.Sizeof(*in.Routine))) {
		x := *in.Routine
		x.Parameters = descriptorSlice(c, x.Parameters)
		for i := range x.Parameters {
			p := &x.Parameters[i]
			p.Name = c.text(p.Name)
			p.Mode = c.text(p.Mode)
			p.Layout = c.layout(p.Layout, 1)
			p.Default = c.expression(p.Default)
		}
		x.Settings = descriptorSlice(c, x.Settings)
		for i := range x.Settings {
			x.Settings[i].Name = c.text(x.Settings[i].Name)
			x.Settings[i].Value = c.text(x.Settings[i].Value)
		}
		x.Result = c.layout(x.Result, 1)
		e := c.expression(&x.Body)
		if e != nil {
			x.Body = *e
		}
		x.Language = c.text(x.Language)
		x.Volatility = c.text(x.Volatility)
		x.Parallel = c.text(x.Parallel)
		x.Source = c.text(x.Source)
		x.Binary = c.text(x.Binary)
		x.ExternalCode = c.text(x.ExternalCode)
		out.Routine = &x
	}
	if in.Type != nil && c.charge(uint64(unsafe.Sizeof(*in.Type))) {
		x := *in.Type
		x.Layout = c.layout(x.Layout, 1)
		if x.BaseLayout.Type.ID != 0 {
			x.BaseLayout = c.layout(x.BaseLayout, 1)
		}
		x.Default = c.expression(x.Default)
		x.Elements = descriptorSlice(c, x.Elements)
		x.ElementOIDs = descriptorSlice(c, x.ElementOIDs)
		x.UnsafeElementOIDs = descriptorSlice(c, x.UnsafeElementOIDs)
		for i := range x.Elements {
			x.Elements[i] = c.text(x.Elements[i])
		}
		x.Fields = c.columns(x.Fields)
		x.Constraints = c.constraints(x.Constraints)
		if x.Range != nil && c.charge(uint64(unsafe.Sizeof(*x.Range))) {
			r := *x.Range
			r.Subtype = c.layout(r.Subtype, 1)
			x.Range = &r
		}
		out.Type = &x
	}
	if in.TextSearchTemplate != nil && c.charge(uint64(unsafe.Sizeof(*in.TextSearchTemplate))) {
		x := *in.TextSearchTemplate
		out.TextSearchTemplate = &x
	}
	if in.TextSearchDictionary != nil && c.charge(uint64(unsafe.Sizeof(*in.TextSearchDictionary))) {
		x := *in.TextSearchDictionary
		x.Language = c.text(x.Language)
		x.InitOptions = c.text(x.InitOptions)
		x.StopWordsLanguage = c.text(x.StopWordsLanguage)
		x.DictionaryFile = c.text(x.DictionaryFile)
		x.AffixFile = c.text(x.AffixFile)
		x.SynonymsFile = c.text(x.SynonymsFile)
		x.ThesaurusFile = c.text(x.ThesaurusFile)
		x.StopWords = descriptorSlice(c, x.StopWords)
		for i := range x.StopWords {
			x.StopWords[i] = c.text(x.StopWords[i])
		}
		out.TextSearchDictionary = &x
	}
	if in.TextSearchConfiguration != nil && c.charge(uint64(unsafe.Sizeof(*in.TextSearchConfiguration))) {
		x := *in.TextSearchConfiguration
		x.Mappings = descriptorSlice(c, x.Mappings)
		for i := range x.Mappings {
			x.Mappings[i].Dictionaries = descriptorSlice(c, x.Mappings[i].Dictionaries)
		}
		out.TextSearchConfiguration = &x
	}
	if in.Cast != nil && c.charge(uint64(unsafe.Sizeof(*in.Cast))) {
		x := *in.Cast
		x.Source = c.layout(x.Source, 1)
		x.Target = c.layout(x.Target, 1)
		out.Cast = &x
	}
	if in.Operator != nil && c.charge(uint64(unsafe.Sizeof(*in.Operator))) {
		x := *in.Operator
		x.Name = c.text(x.Name)
		x.Left = c.layout(x.Left, 1)
		x.Right = c.layout(x.Right, 1)
		x.Result = c.layout(x.Result, 1)
		out.Operator = &x
	}
	if in.Aggregate != nil && c.charge(uint64(unsafe.Sizeof(*in.Aggregate))) {
		x := *in.Aggregate
		x.Arguments = descriptorSlice(c, x.Arguments)
		for i := range x.Arguments {
			x.Arguments[i] = c.layout(x.Arguments[i], 1)
		}
		x.ArgumentNames = descriptorSlice(c, x.ArgumentNames)
		for i := range x.ArgumentNames {
			x.ArgumentNames[i] = c.text(x.ArgumentNames[i])
		}
		x.TransitionLayout = c.layout(x.TransitionLayout, 1)
		x.MovingTransitionLayout = c.layout(x.MovingTransitionLayout, 1)
		x.Result = c.layout(x.Result, 1)
		x.InitialValue = c.text(x.InitialValue)
		x.MovingInitialValue = c.text(x.MovingInitialValue)
		x.Parallel = c.text(x.Parallel)
		x.Volatility = c.text(x.Volatility)
		out.Aggregate = &x
	}
	if c.err != nil {
		return DescriptorSpec{}, c.err
	}
	return out, nil
}
