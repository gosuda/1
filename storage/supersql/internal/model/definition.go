package model

// CastMetadata is the persisted identity of one SQL-defined cast created by
// CREATE CAST. Casts are not schema-qualified in PostgreSQL; the descriptor's
// namespace parent is the pg_catalog schema and its name is the stable decimal
// pair "<sourceTypeID>:<targetTypeID>". Function identity is recorded as the
// executable FunctionID; the corresponding descriptor, when user-defined, is
// also present in DescriptorSpec.Dependencies so DROP closure reaches it.
type CastMetadata struct {
	Owner  OID
	Source TypeLayout
	Target TypeLayout
	// Function is the conversion routine for Method 'f'; zero for 'b' and 'i'.
	Function FunctionID
	// Context is PostgreSQL castcontext: 'i' implicit, 'a' assignment, 'e' explicit.
	Context byte
	// Method is PostgreSQL castmethod: 'f' function, 'b' binary-coercible, 'i' inout.
	Method byte
}

// OperatorMetadata is the persisted identity of one SQL-defined operator.
// Left or Right is absent (Type.ID == 0) for prefix and postfix operators.
// Commutator and Negator are resolved operator identities: a builtin operator
// ID, or the logical OID of a user-defined operator, which also appears in
// DescriptorSpec.Dependencies.
type OperatorMetadata struct {
	Owner OID
	// Name duplicates DescriptorSpec.Name.Name for catalog introspection.
	Name        string
	Left, Right TypeLayout
	Result      TypeLayout
	Function    FunctionID
	Commutator  OperatorID
	Negator     OperatorID
	// Restrict and Join are independent planner support function identities;
	// either may be zero.
	Restrict FunctionID
	Join     FunctionID
	// CanHash and CanMerge record the declaration's HASHES/MERGES flags. They
	// state what the operator's definition claims, not what this engine has
	// verified it can execute, so no planner support function is required.
	CanHash  bool
	CanMerge bool
}

// AggregateKind separates PostgreSQL's ordinary, ordered-set, and
// hypothetical-set aggregate forms. The numeric values are persisted, so they
// are append-only and must match the catalog encoding.
type AggregateKind uint8

const (
	AggregateOrdinary AggregateKind = iota + 1
	AggregateOrderedSet
	AggregateHypotheticalSet
)

// AggregateFinalModify mirrors PostgreSQL aggfinalmodify/aggmfinalmodify.
type AggregateFinalModify uint8

const (
	AggregateFinalReadOnly AggregateFinalModify = iota + 1
	AggregateFinalShareable
	AggregateFinalReadWrite
)

// AggregateMetadata is the persisted identity of one SQL-defined aggregate.
// The descriptor shares PostgreSQL's pg_proc name/signature namespace with
// routines even though its executable semantics differ, so it carries its own
// argument list rather than a RoutineMetadata. Every referenced support routine
// is a FunctionID plus, for user-defined routines, a DescriptorSpec.Dependencies
// edge.
type AggregateMetadata struct {
	Owner           OID
	Kind            AggregateKind
	Arguments       []TypeLayout
	// ArgumentNames parallels Arguments by ordinal and is empty when no
	// argument is named (pg_proc.proargnames). A named argument is the empty
	// string only when a later argument is named, which PostgreSQL permits.
	ArgumentNames []string
	// Variadic is the element type of the single VARIADIC argument, or zero when
	// the aggregate has none. The corresponding Arguments entry keeps the
	// declared array type, and pg_proc.provariadic reports this element type.
	Variadic        TypeID
	DirectArguments uint16

	// Result is the aggregate's SQL result type (pg_proc.prorettype): the final
	// function's result when one exists, otherwise the transition state type.
	Result TypeLayout

	Transition FunctionID
	Final      FunctionID
	Combine    FunctionID
	Serial     FunctionID
	Deserial   FunctionID

	MovingTransition FunctionID
	MovingInverse    FunctionID
	MovingFinal      FunctionID

	FinalExtra        bool
	MovingFinalExtra  bool
	FinalModify       AggregateFinalModify
	MovingFinalModify AggregateFinalModify

	SortOperator OperatorID

	TransitionLayout       TypeLayout
	MovingTransitionLayout TypeLayout
	TransitionSpace        int32
	MovingTransitionSpace  int32

	InitialValue          string
	HasInitialValue       bool
	MovingInitialValue    string
	HasMovingInitialValue bool

	Parallel   string
	Volatility string
}
