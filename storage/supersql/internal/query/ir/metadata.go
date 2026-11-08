package ir

// Metadata removes graph-local payload ranges for rebuilding with a new Builder.
// Constant payloads require CloneConstant; all handles must be explicitly remapped.
func (n Scalar) Metadata() Scalar { n.operands = extent{}; n.comparisons = extent{}; n.constant = 0; return n }

// Rel.Metadata excludes Recursion, which is supplied separately in RecursionSpec.
func (n Rel) Metadata() Rel {
	n.inputs = extent{}
	n.expressions = extent{}
	n.outputs = extent{}
	n.targetKeys = extent{}
	n.updatedColumns = extent{}
	n.bindings = extent{}
	n.assignments = extent{}
	n.conflictAssignments = extent{}
	n.conflictArbiters = extent{}
	n.order = extent{}
	n.groups = extent{}
	n.aggregates = extent{}
	n.groupings = extent{}
	n.windows = extent{}
	n.partition = extent{}
	n.locks = extent{}
	n.actions = extent{}
	n.jsonTableColumns = extent{}
	n.viewMutationSteps = extent{}
	n.Recursion = Recursion{}
	return n
}
func (n Signature) Metadata() Signature         { n.parameters = extent{}; return n }
func (n AggregateCall) Metadata() AggregateCall {
	n.direct, n.args, n.order = extent{}, extent{}, extent{}
	return n
}
func (n GroupingCall) Metadata() GroupingCall   { n.args = extent{}; return n }
func (n WindowCall) Metadata() WindowCall       { n.args = extent{}; return n }
func (n RowLock) Metadata() RowLock             { n.keys = extent{}; return n }
func (n MergeAction) Metadata() MergeAction     { n.assignments = extent{}; return n }
func (n ViewMutationStep) Metadata() ViewMutationStep {
	n.inputs, n.projections, n.outputColumns = extent{}, extent{}, extent{}
	return n
}
func (n Recursion) Metadata() Recursion         { n.searchKeys = extent{}; n.cycleKeys = extent{}; return n }
