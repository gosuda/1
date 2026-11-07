package opt

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	logicalpartition "github.com/gosuda/supersql-alpha/internal/query/partition"
)

// AccessPlanner transfers owned metadata within the supplied limits. Negotiate
// must call the actual reader's planning boundary, then discard its invocation
// binding, snapshot and fence. It must not open a scan cursor.
type AccessPlanner interface {
	Access(context.Context, AccessRequest) (Access, error)
	Negotiate(context.Context, storage.PushdownRequest, uint64) (physical.ScanSpec, error)
}
type AccessRequest struct {
	Object                        model.ObjectID
	Generation, DescriptorVersion uint64
	Graph                         *ir.Graph
	Predicate                     ir.ScalarID
	// Columns is borrowed for the duration of Access.
	Columns  []model.ColumnID
	MaxPaths int
	MaxBytes uint64
}
type Access struct {
	Target storage.ObjectRef
	Layout []storage.ColumnLayout
	Paths  []AccessPath
}
// PartitionAccess proves which direct child of one declarative partition router
// owns rows from this physical scan.
type PartitionAccess struct {
	Router *logicalpartition.Router
	Child  model.ObjectID
	Keys   []storage.KeyColumn
}

type AccessExpressionParameter struct {
	Column model.ColumnID
	Layout model.TypeLayout
}

type AccessExpression struct {
	Graph      *ir.Graph
	Root       ir.ScalarID
	Parameters []AccessExpressionParameter
	Builtin    builtin.ResolveOptions
}

type LookupAccess struct {
	Enabled     bool
	OwnerColumn model.ColumnID
	Target      storage.ObjectRef
	Layout      []storage.ColumnLayout
	ColumnMap   []physical.ScanColumn
	Keys        []storage.KeyColumn
}

func (l LookupAccess) PhysicalColumn(source model.ColumnID) model.ColumnID {
	for _, column := range l.ColumnMap {
		if column.Source == source {
			return column.Target
		}
	}
	return 0
}

type AccessPath struct {
	Target         storage.ObjectRef
	Layout         []storage.ColumnLayout
	ColumnMap      []physical.ScanColumn
	Algorithm      physical.Algorithm
	Index          model.IndexID
	Keys           []storage.KeyColumn
	KeyExpressions []AccessExpression
	Predicate      AccessExpression
	// Operators prove the SQL ordering identity for each index-key prefix.
	Operators  []model.OperatorID
	Direction  storage.ScanDirection
	Partitions []PartitionAccess
	Lookup     LookupAccess
}

type pathStatistic struct {
	target      storage.ObjectRef
	table       *statistics.Table
	selectivity float64
}

type scanAccess struct {
	paths               []*expression
	blocks, versionWork float64
	statistics          *statistics.Table
	statisticsTarget    storage.ObjectRef
	statisticsTracked   bool
	pathStatistics      []pathStatistic
}

func (o *optimizer) predicateSpans(id ir.RelID, object model.ObjectID, outputs []physical.Slot, columns []storage.KeyColumn, expressions []AccessExpression, columnMap []physical.ScanColumn, unpruned []storage.KeySpan) []storage.KeySpan {
	predicate := o.predicates[id]
	if predicate == 0 || len(columns) == 0 {
		return unpruned
	}
	keys := allocate[span.Key](o, uint64(len(columns)))
	if o.err != nil {
		return nil
	}
	for i, column := range columns {
		if i < len(expressions) && expressions[i].Graph != nil {
			expression := o.findAccessExpression(object, predicate, expressions[i], 0)
			keys[i] = span.Key{Column: column, Expression: expression}
			duplicate := slices.ContainsFunc(keys[:i], func(key span.Key) bool { return key.Expression == expression })
			if expression == 0 || duplicate {
				keys = keys[:i]
				break
			}
			continue
		}
		slot := o.outputColumnSlot(outputs, (physical.ScanSpec{ColumnMap: columnMap}).LogicalColumn(column.ColumnID)).ID
		for j, output := range outputs {
			if output.ID == slot && j < len(o.predicateSlots[id]) && o.predicateSlots[id][j] != 0 {
				slot = o.predicateSlots[id][j]
				break
			}
		}
		keys[i] = span.Key{Column: column, Slot: slot}
		if keys[i].Slot == 0 {
			keys = keys[:i]
			break
		}
	}
	remaining := min(o.options.MaxBytes-o.bytes, o.options.IR.MaxBytes)
	if len(keys) == 0 {
		return unpruned
	}
	if !o.charge(remaining, 1) {
		return nil
	}
	work := min((o.options.MaxWork-o.work)/4, uint64(o.options.IR.MaxNodes)*uint64(o.options.Plan.MaxOperands))
	if !o.step(work) {
		return nil
	}
	limits := span.Limits{MaxSpans: min(1024, int(o.options.Plan.MaxOperands)), MaxNodes: o.options.IR.MaxNodes, MaxWork: work, MaxBytes: remaining}
	var bounds span.Result
	var err error
	if o.options.Parameters == nil {
		bounds, err = span.ExtractStatic(o.ctx, o.graph, predicate, keys, limits)
	} else {
		bounds, err = span.Extract(o.ctx, o.graph, predicate, keys, o.options.Parameters, limits)
	}
	if err != nil {
		o.err = err
		return nil
	}
	o.bytes -= remaining - bounds.UsedBytes
	o.work -= work - bounds.UsedWork
	return bounds.Spans
}
func (o *optimizer) scanSpans(id ir.RelID, outputs []physical.Slot, path AccessPath, unpruned []storage.KeySpan) []storage.KeySpan {
	relation, _ := o.graph.Rel(id)
	spans := o.predicateSpans(id, relation.Object, outputs, path.Keys, path.KeyExpressions, path.ColumnMap, unpruned)
	for _, constraint := range path.Partitions {
		if constraint.Router == nil || constraint.Child == 0 || len(constraint.Keys) == 0 {
			continue
		}
		partitionSpans := o.predicateSpans(id, relation.Object, outputs, constraint.Keys, nil, path.ColumnMap, unpruned)
		targets, err := constraint.Router.Prune(o.ctx, partitionSpans)
		if err != nil {
			o.err = err
			return nil
		}
		if !slices.ContainsFunc(targets, func(target logicalpartition.Target) bool {
			return target.Ref.ObjectID == constraint.Child
		}) {
			return nil
		}
	}
	return spans
}

func (o *optimizer) scans(id ir.RelID, node physical.NodeSpec) []*expression {
	if paths := o.access[id].paths; paths != nil {
		return paths
	}
	if o.options.Access == nil {
		o.unsupported(id, "scan requires an AccessPlanner")
		return nil
	}
	r, _ := o.graph.Rel(id)
	columns := allocate[model.ColumnID](o, uint64(len(node.Output)))
	if o.err != nil {
		return nil
	}
	for i, output := range node.Output {
		info, _ := o.graph.Slot(output.ID)
		columns[i] = info.Column
	}
	available, err := o.options.Access.Access(o.ctx, AccessRequest{Object: r.Object, Generation: r.Generation, DescriptorVersion: r.DescriptorVersion, Graph: o.graph, Predicate: o.predicates[id], Columns: columns, MaxPaths: o.options.MaxAccessPaths, MaxBytes: o.options.MaxBytes - o.bytes})
	if err != nil {
		o.err = err
		return nil
	}
	if !o.step(1) {
		return nil
	}
	if len(available.Paths) == 0 {
		o.unsupported(id, "reader advertises no scan access path")
		return nil
	}
	if len(available.Paths) > o.options.MaxAccessPaths || len(available.Layout) > int(o.options.MaxOutputColumns) {
		o.err = ErrLimit
		return nil
	}
	if available.Target.ObjectID != r.Object || available.Target.Generation != r.Generation || available.Target.DescriptorVersion != r.DescriptorVersion {
		o.err = fmt.Errorf("%w: stale access target", ErrInvalid)
		return nil
	}
	if err := available.Target.Validate(); err != nil {
		o.err = err
		return nil
	}
	if !o.charge(uint64(cap(available.Paths)), uint64(unsafe.Sizeof(AccessPath{}))) || !o.charge(uint64(cap(available.Layout)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))) {
		return nil
	}
	for _, column := range available.Layout {
		if !o.accountLayout(model.TypeLayout{Type: column.Type, Children: column.Children}, 0) {
			return nil
		}
	}
	for pathIndex, path := range available.Paths {
		if len(path.Layout) > int(o.options.Plan.MaxOperands) || len(path.ColumnMap) > int(o.options.MaxOutputColumns) || len(path.Keys) > int(o.options.Plan.MaxOperands) || len(path.KeyExpressions) > int(o.options.Plan.MaxOperands) || len(path.Partitions) > int(o.options.IR.MaxDepth) || len(path.Lookup.Layout) > int(o.options.Plan.MaxOperands) || len(path.Lookup.ColumnMap) > int(o.options.MaxOutputColumns) || len(path.Lookup.Keys) > int(o.options.Plan.MaxOperands) {
			o.err = ErrLimit
			return nil
		}
		if !o.charge(uint64(cap(path.Layout)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))) || !o.charge(uint64(cap(path.ColumnMap)), uint64(unsafe.Sizeof(physical.ScanColumn{}))) {
			return nil
		}
		for _, column := range path.Layout {
			if !o.accountLayout(model.TypeLayout{Type: column.Type, Children: column.Children}, 0) {
				return nil
			}
		}
		if err := validateAccessPath(available, path, columns); err != nil {
			o.err = err
			return nil
		}
		keyBytes, err := storage.KeyLayoutBytes(o.ctx, path.Keys, o.options.MaxBytes-o.bytes)
		if err != nil {
			o.err = err
			return nil
		}
		if !o.charge(1, keyBytes) || !o.charge(uint64(cap(path.Keys)-len(path.Keys)), uint64(unsafe.Sizeof(storage.KeyColumn{}))) || !o.charge(uint64(cap(path.KeyExpressions)), uint64(unsafe.Sizeof(AccessExpression{}))) || !o.charge(uint64(cap(path.Operators)), uint64(unsafe.Sizeof(model.OperatorID(0)))) {
			return nil
		}
		for _, expression := range path.KeyExpressions {
			if !o.accountAccessExpression(expression) {
				return nil
			}
		}
		if !o.accountAccessExpression(path.Predicate) {
			return nil
		}
		if path.Lookup.Enabled {
			if !o.charge(uint64(cap(path.Lookup.Layout)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))) || !o.charge(uint64(cap(path.Lookup.ColumnMap)), uint64(unsafe.Sizeof(physical.ScanColumn{}))) {
				return nil
			}
			for _, column := range path.Lookup.Layout {
				if !o.accountLayout(model.TypeLayout{Type: column.Type, Children: column.Children}, 0) {
					return nil
				}
			}
			lookupKeyBytes, err := storage.KeyLayoutBytes(o.ctx, path.Lookup.Keys, o.options.MaxBytes-o.bytes)
			if err != nil {
				o.err = err
				return nil
			}
			if !o.charge(1, lookupKeyBytes) || !o.charge(uint64(cap(path.Lookup.Keys)-len(path.Lookup.Keys)), uint64(unsafe.Sizeof(storage.KeyColumn{}))) {
				return nil
			}
		}
		if !o.charge(uint64(cap(path.Partitions)), uint64(unsafe.Sizeof(PartitionAccess{}))) {
			return nil
		}
		for _, constraint := range path.Partitions {
			if constraint.Router == nil || constraint.Child == 0 || len(constraint.Keys) == 0 || len(constraint.Keys) > int(o.options.Plan.MaxOperands) {
				o.err = ErrInvalid
				return nil
			}
			partitionKeyBytes, err := storage.KeyLayoutBytes(o.ctx, constraint.Keys, o.options.MaxBytes-o.bytes)
			if err != nil {
				o.err = err
				return nil
			}
			if !o.charge(1, partitionKeyBytes) || pathIndex == 0 && !o.charge(1, constraint.Router.OwnedBytes()) {
				return nil
			}
			for _, key := range constraint.Keys {
				if !slices.ContainsFunc(path.Layout, func(column storage.ColumnLayout) bool {
					return column.ID == key.ColumnID
				}) {
					o.err = ErrInvalid
					return nil
				}
			}
		}
		if path.Algorithm < physical.TableScan || path.Algorithm > physical.SecondaryScan || (path.Algorithm == physical.SecondaryScan) != (path.Index != 0) || len(path.Operators) > len(path.Keys) {
			o.err = ErrInvalid
			return nil
		}
	}
	// Stable ordering gives identical winners even when catalog inventory order differs.
	sort.Slice(available.Paths, func(i, j int) bool { return compareAccess(available.Paths[i], available.Paths[j]) < 0 })
	paths := allocate[*expression](o, uint64(len(available.Paths)))
	if o.err != nil {
		return nil
	}
	base := o.tableEstimate(id, available.Target)
	if o.err != nil {
		return nil
	}
	if o.options.Statistics != nil {
		o.access[id].pathStatistics = allocate[pathStatistic](o, uint64(len(available.Paths)))
		o.access[id].pathStatistics = o.access[id].pathStatistics[:0]
		if o.err != nil {
			return nil
		}
	}
	for i, path := range available.Paths {
		if !o.step(1) {
			return nil
		}
		if slices.Contains(o.lockedOccurrences, r.Occurrence) && (path.Algorithm == physical.SecondaryScan || path.Lookup.Enabled) {
			continue
		}
		if path.Predicate.Graph != nil && !o.predicateImplies(r.Object, o.predicates[id], path.Predicate) {
			if o.err != nil {
				return nil
			}
			continue
		}
		if i > 0 && compareAccess(available.Paths[i-1], path) == 0 {
			previous := available.Paths[i-1]
			if previous.Target != path.Target || !equalAccessColumns(previous.Layout, path.Layout) || !slices.Equal(previous.ColumnMap, path.ColumnMap) || !storage.KeyLayoutsEqual(previous.Keys, path.Keys) || !equalAccessExpressions(previous.KeyExpressions, path.KeyExpressions) || !equalAccessExpression(previous.Predicate, path.Predicate) || !slices.Equal(previous.Operators, path.Operators) || !equalPartitionAccess(previous.Partitions, path.Partitions) || !equalLookupAccess(previous.Lookup, path.Lookup) {
				o.err = ErrInvalid
				return nil
			}
			continue
		}
		if path.Target != available.Target {
			o.loadPathStatistics(id, r.Object, o.predicates[id], path)
			if o.err != nil {
				return nil
			}
		}
		request := storage.PushdownRequest{Target: path.Target, Occurrence: r.Occurrence, Layout: path.Layout, KeyLayout: path.Keys, IndexID: path.Index, Direction: path.Direction}
		request.Predicate = o.scanPredicate(id, node.Output, path.ColumnMap)
		if o.err != nil {
			return nil
		}
		request.ColumnIDs = allocate[model.ColumnID](o, uint64(len(node.Output)+1))
		if o.err != nil {
			return nil
		}
		request.ColumnIDs = request.ColumnIDs[:0]
		for _, slot := range node.Output {
			info, _ := o.graph.Slot(slot.ID)
			column := (physical.ScanSpec{ColumnMap: path.ColumnMap}).PhysicalColumn(info.Column)
			if column != 0 && !slices.Contains(request.ColumnIDs, column) {
				request.ColumnIDs = append(request.ColumnIDs, column)
			}
		}
		if path.Lookup.Enabled && !slices.Contains(request.ColumnIDs, path.Lookup.OwnerColumn) {
			request.ColumnIDs = append(request.ColumnIDs, path.Lookup.OwnerColumn)
		}
		request.Spans = allocate[storage.KeySpan](o, 1)
		if o.err != nil {
			return nil
		}
		request.Spans[0] = storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
		request.Spans = o.scanSpans(id, node.Output, path, request.Spans)
		if o.err != nil {
			return nil
		}
		decision, ok := o.negotiateScan(request)
		if !ok {
			return nil
		}
		decision.Source, decision.ColumnMap = available.Target, path.ColumnMap
		o.applyPathStatistic(id, path.Target, &decision)
		if o.err != nil {
			return nil
		}
		if path.Lookup.Enabled {
			baseRequest := storage.PushdownRequest{Target: path.Lookup.Target, Occurrence: r.Occurrence, Layout: path.Lookup.Layout, KeyLayout: path.Lookup.Keys, Direction: storage.ScanForward}
			baseRequest.ColumnIDs = allocate[model.ColumnID](o, uint64(len(node.Output)))
			if o.err != nil {
				return nil
			}
			baseRequest.ColumnIDs = baseRequest.ColumnIDs[:0]
			for _, slot := range node.Output {
				info, _ := o.graph.Slot(slot.ID)
				if (physical.ScanSpec{ColumnMap: path.ColumnMap}).PhysicalColumn(info.Column) != 0 {
					continue
				}
				column := (LookupAccess{ColumnMap: path.Lookup.ColumnMap}).PhysicalColumn(info.Column)
				if column == 0 || slices.Contains(baseRequest.ColumnIDs, column) {
					continue
				}
				baseRequest.ColumnIDs = append(baseRequest.ColumnIDs, column)
			}
			if len(baseRequest.ColumnIDs) == 0 {
				o.err = ErrInvalid
				return nil
			}
			baseRequest.Spans = allocate[storage.KeySpan](o, 1)
			if o.err != nil {
				return nil
			}
			baseRequest.Spans[0] = storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
			baseDecision, ok := o.negotiateScan(baseRequest)
			if !ok {
				return nil
			}
			decision.Lookup = physical.LookupScanSpec{
				Enabled:        true,
				OwnerColumn:    path.Lookup.OwnerColumn,
				BaseColumnMap:  path.Lookup.ColumnMap,
				BaseRequest:    baseDecision.Request,
				BaseAccepted:   baseDecision.Accepted,
				BaseResidual:   baseDecision.Residual,
				BaseProjection: baseDecision.Projection,
			}
			decision.Estimate.IndexProbes += decision.Estimate.Rows
		}
		candidate := node
		candidate.Algorithm, candidate.Scan = path.Algorithm, decision
		candidate.Estimate = base
		if len(request.Spans) == 0 {
			candidate.Estimate.Rows, candidate.Estimate.LowerRows, candidate.Estimate.UpperRows = 0, 0, 0
		}
		candidate.Delivered.Ordering = nil
		for j, order := range decision.Ordering {
			if j >= len(path.Operators) || path.Operators[j] == 0 || int(order.KeyOrdinal) >= len(path.Keys) {
				break
			}
			column := decision.LogicalColumn(path.Keys[order.KeyOrdinal].ColumnID)
			var slot model.SlotID
			for _, output := range node.Output {
				info, _ := o.graph.Slot(output.ID)
				if info.Column == column {
					slot = output.ID
					break
				}
			}
			if slot == 0 {
				break
			}
			key := physical.OrderKey{Key: o.key(slot), Operator: path.Operators[j], Direction: ir.Ascending, Nulls: ir.NullsLast}
			if order.Descending {
				key.Direction = ir.Descending
			}
			if order.NullsFirst {
				key.Nulls = ir.NullsFirst
			}
			if !o.charge(2, uint64(unsafe.Sizeof(key))) {
				return nil
			}
			candidate.Delivered.Ordering = append(candidate.Delivered.Ordering, key)
		}
		paths[i] = o.newExpression(candidate, nil, nil)
		if o.err != nil {
			return nil
		}
	}
	paths = slices.DeleteFunc(paths, func(e *expression) bool { return e == nil })

	o.access[id].paths = paths
	return paths
}
func (o *optimizer) negotiateScan(request storage.PushdownRequest) (physical.ScanSpec, bool) {
	decision, err := o.options.Access.Negotiate(o.ctx, request, o.options.Plan.MaxBytes)
	if err != nil {
		o.err = err
		return physical.ScanSpec{}, false
	}
	if !o.chargeScanSpec(decision) {
		return physical.ScanSpec{}, false
	}
	if !o.step(1) {
		return physical.ScanSpec{}, false
	}
	before, err := storage.HashPushdownPlan(o.ctx, storage.PushdownPlan{Request: request}, o.options.Plan.MaxBytes)
	if err != nil {
		o.err = err
		return physical.ScanSpec{}, false
	}
	after, err := storage.HashPushdownPlan(o.ctx, storage.PushdownPlan{Request: decision.Request}, o.options.Plan.MaxBytes)
	if err != nil {
		o.err = err
		return physical.ScanSpec{}, false
	}
	if before != after || decision.AppliedLimit.Enabled || decision.LimitOwner != 0 {
		o.err = fmt.Errorf("%w: reader changed required scan semantics", ErrInvalid)
		return physical.ScanSpec{}, false
	}
	return decision, true
}

// chargeScanSpec charges the bytes the plan retains from one negotiated scan.
// Plan.MaxBytes bounds a single plan object's size, so passing it as an item
// count multiplies a size ceiling into the working-byte budget; the retained
// metadata is what actually survives into the plan.
func (o *optimizer) chargeScanSpec(spec physical.ScanSpec) bool {
	if !o.charge(1, uint64(unsafe.Sizeof(spec))) {
		return false
	}
	for _, slice := range [...]struct{ count, size uint64 }{
		{uint64(cap(spec.ColumnMap)), uint64(unsafe.Sizeof(physical.ScanColumn{}))},
		{uint64(cap(spec.Request.Layout)), uint64(unsafe.Sizeof(storage.ColumnLayout{}))},
		{uint64(cap(spec.Request.ColumnIDs)), uint64(unsafe.Sizeof(model.ColumnID(0)))},
		{uint64(cap(spec.Request.Spans)), uint64(unsafe.Sizeof(storage.KeySpan{}))},
		{uint64(cap(spec.Request.EncodedSpans)), uint64(unsafe.Sizeof(storage.EncodedKeySpan{}))},
		{uint64(cap(spec.Projection)), uint64(unsafe.Sizeof(model.ColumnID(0)))},
		{uint64(cap(spec.Ordering)), uint64(unsafe.Sizeof(storage.ScanOrdering{}))},
		{uint64(cap(spec.Lookup.BaseColumnMap)), uint64(unsafe.Sizeof(physical.ScanColumn{}))},
		{uint64(cap(spec.Lookup.BaseProjection)), uint64(unsafe.Sizeof(model.ColumnID(0)))},
	} {
		if !o.charge(slice.count, slice.size) {
			return false
		}
	}
	return true
}

func validateAccessPath(access Access, path AccessPath, columns []model.ColumnID) error {
	if err := path.Target.Validate(); err != nil {
		return err
	}
	secondary := path.Algorithm == physical.SecondaryScan
	unversioned := path.Target.EncodingVersion == 0 || path.Target.DescriptorVersion == 0
	wrongTarget := path.Target != access.Target || path.Index != 0
	if secondary {
		wrongTarget = path.Target.ObjectID == access.Target.ObjectID || path.Index != model.IndexID(path.Target.ObjectID)
	}
	if unversioned || wrongTarget {
		return fmt.Errorf("%w: access path identity", ErrInvalid)
	}
	if len(path.ColumnMap) == 0 && !path.Lookup.Enabled && len(columns) != 0 {
		return fmt.Errorf("%w: access column mapping missing", ErrInvalid)
	}
	for _, layout := range [][]storage.ColumnLayout{access.Layout, path.Layout} {
		for i, column := range layout {
			if column.ID == 0 {
				return fmt.Errorf("%w: zero access column", ErrInvalid)
			}
			for _, previous := range layout[:i] {
				if previous.ID == column.ID {
					return fmt.Errorf("%w: duplicate access column", ErrInvalid)
				}
			}
		}
	}
	for i, mapping := range path.ColumnMap {
		missingColumn := mapping.Source == 0 || mapping.Target == 0
		remappedPrimary := !secondary && mapping.Source != mapping.Target
		if missingColumn || remappedPrimary {
			return fmt.Errorf("%w: access column mapping", ErrInvalid)
		}
		for _, previous := range path.ColumnMap[:i] {
			if previous.Source == mapping.Source || previous.Target == mapping.Target {
				return fmt.Errorf("%w: duplicate access column mapping", ErrInvalid)
			}
		}
		source := slices.IndexFunc(access.Layout, func(column storage.ColumnLayout) bool { return column.ID == mapping.Source })
		target := slices.IndexFunc(path.Layout, func(column storage.ColumnLayout) bool { return column.ID == mapping.Target })
		if source < 0 || target < 0 {
			return fmt.Errorf("%w: access mapping column absent", ErrInvalid)
		}
		a, b := access.Layout[source], path.Layout[target]
		if a.Nullable != b.Nullable || !model.TypeLayoutsEqual(model.TypeLayout{Type: a.Type, Children: a.Children}, model.TypeLayout{Type: b.Type, Children: b.Children}) {
			return fmt.Errorf("%w: access mapping layout mismatch", ErrInvalid)
		}
	}
	if len(path.KeyExpressions) > len(path.Keys) {
		return fmt.Errorf("%w: access expression key alignment", ErrInvalid)
	}
	for i, expression := range path.KeyExpressions {
		key := path.Keys[i]
		if err := validateAccessExpression(access.Layout, expression, model.TypeLayout{Type: key.Type, Children: key.Children}); err != nil {
			return err
		}
	}
	if err := validateAccessExpression(access.Layout, path.Predicate, model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}); err != nil {
		return err
	}
	if err := validateLookupAccess(access, path); err != nil {
		return err
	}
	scan := physical.ScanSpec{ColumnMap: path.ColumnMap}
	for _, column := range columns {
		if scan.PhysicalColumn(column) == 0 && path.Lookup.PhysicalColumn(column) == 0 {
			return fmt.Errorf("%w: access projection incomplete", ErrInvalid)
		}
	}
	return nil
}

func validateAccessExpression(layout []storage.ColumnLayout, expression AccessExpression, result model.TypeLayout) error {
	if expression.Graph == nil {
		if expression.Root != 0 || len(expression.Parameters) != 0 {
			return fmt.Errorf("%w: incomplete access expression", ErrInvalid)
		}
		return nil
	}
	root, ok := expression.Graph.Scalar(expression.Root)
	if !ok || !accessModelLayoutEqual(expression.Graph, root.Layout, result) || root.Effects.Volatility != ir.Immutable {
		return fmt.Errorf("%w: invalid access expression result", ErrInvalid)
	}
	for i, parameter := range expression.Parameters {
		if parameter.Column == 0 {
			return fmt.Errorf("%w: zero access expression parameter", ErrInvalid)
		}
		for _, previous := range expression.Parameters[:i] {
			if previous.Column == parameter.Column {
				return fmt.Errorf("%w: duplicate access expression parameter", ErrInvalid)
			}
		}
		column := slices.IndexFunc(layout, func(column storage.ColumnLayout) bool { return column.ID == parameter.Column })
		if column < 0 || !model.TypeLayoutsEqual(parameter.Layout, model.TypeLayout{Type: layout[column].Type, Children: layout[column].Children}) {
			return fmt.Errorf("%w: access expression parameter layout", ErrInvalid)
		}
	}
	for id := ir.ScalarID(1); uint32(id) <= expression.Graph.ScalarCount(); id++ {
		node, _ := expression.Graph.Scalar(id)
		if node.Kind == ir.Subquery || node.Kind == ir.Slot || node.Effects.Volatility != ir.Immutable {
			return fmt.Errorf("%w: effectful access expression", ErrInvalid)
		}
		if node.Kind == ir.Param && (node.Parameter == 0 || int(node.Parameter) > len(expression.Parameters) || !accessModelLayoutEqual(expression.Graph, node.Layout, expression.Parameters[node.Parameter-1].Layout)) {
			return fmt.Errorf("%w: access expression parameter binding", ErrInvalid)
		}
	}
	return nil
}

func validateLookupAccess(access Access, path AccessPath) error {
	lookup := path.Lookup
	if !lookup.Enabled {
		if lookup.OwnerColumn != 0 || lookup.Target != (storage.ObjectRef{}) || len(lookup.Layout) != 0 || len(lookup.ColumnMap) != 0 || len(lookup.Keys) != 0 {
			return fmt.Errorf("%w: disabled lookup access metadata", ErrInvalid)
		}
		return nil
	}
	if path.Algorithm != physical.SecondaryScan || lookup.OwnerColumn == 0 || lookup.Target != access.Target || !equalAccessColumns(lookup.Layout, access.Layout) || len(lookup.Keys) == 0 || len(lookup.ColumnMap) == 0 {
		return fmt.Errorf("%w: lookup access identity", ErrInvalid)
	}
	owner := slices.IndexFunc(path.Layout, func(column storage.ColumnLayout) bool { return column.ID == lookup.OwnerColumn })
	if owner < 0 || path.Layout[owner].Type.Storage != model.TypeBytes {
		return fmt.Errorf("%w: lookup owner key layout", ErrInvalid)
	}
	for i, mapping := range lookup.ColumnMap {
		if mapping.Source == 0 || mapping.Source != mapping.Target {
			return fmt.Errorf("%w: lookup access mapping", ErrInvalid)
		}
		for _, previous := range lookup.ColumnMap[:i] {
			if previous.Source == mapping.Source {
				return fmt.Errorf("%w: duplicate lookup access mapping", ErrInvalid)
			}
		}
		if !slices.ContainsFunc(access.Layout, func(column storage.ColumnLayout) bool { return column.ID == mapping.Source }) {
			return fmt.Errorf("%w: lookup access column absent", ErrInvalid)
		}
	}
	for i, key := range lookup.Keys {
		column := slices.IndexFunc(lookup.Layout, func(column storage.ColumnLayout) bool { return column.ID == key.ColumnID })
		if column < 0 || !model.TypeLayoutsEqual(model.TypeLayout{Type: lookup.Layout[column].Type, Children: lookup.Layout[column].Children}, model.TypeLayout{Type: key.Type, Children: key.Children}) {
			return fmt.Errorf("%w: lookup access key layout", ErrInvalid)
		}
		for _, previous := range lookup.Keys[:i] {
			if previous.ColumnID == key.ColumnID {
				return fmt.Errorf("%w: duplicate lookup access key", ErrInvalid)
			}
		}
	}
	return nil
}

func accessModelLayoutEqual(graph *ir.Graph, id ir.LayoutID, layout model.TypeLayout) bool {
	value, ok := graph.Type(id)
	if !ok || value != layout.Type || graph.LayoutChildCount(id) != uint32(len(layout.Children)) {
		return false
	}
	for i := range layout.Children {
		child, _ := graph.LayoutChild(id, uint32(i))
		if !accessModelLayoutEqual(graph, child, layout.Children[i]) {
			return false
		}
	}
	return true
}

func (o *optimizer) accountAccessExpression(expression AccessExpression) bool {
	if expression.Graph == nil {
		return true
	}
	if !o.charge(1, uint64(unsafe.Sizeof(*expression.Graph))+expression.Graph.OwnedBytes()) || !o.charge(uint64(cap(expression.Parameters)), uint64(unsafe.Sizeof(AccessExpressionParameter{}))) {
		return false
	}
	for _, parameter := range expression.Parameters {
		if !o.accountLayout(parameter.Layout, 0) {
			return false
		}
	}
	return true
}

func equalPartitionAccess(left, right []PartitionAccess) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Router != right[i].Router || left[i].Child != right[i].Child || !storage.KeyLayoutsEqual(left[i].Keys, right[i].Keys) {
			return false
		}
	}
	return true
}

func equalAccessColumns(a, b []storage.ColumnLayout) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Nullable != b[i].Nullable || !model.TypeLayoutsEqual(model.TypeLayout{Type: a[i].Type, Children: a[i].Children}, model.TypeLayout{Type: b[i].Type, Children: b[i].Children}) {
			return false
		}
	}
	return true
}

func compareAccess(a, b AccessPath) int {
	if a.Algorithm != b.Algorithm {
		if a.Algorithm < b.Algorithm {
			return -1
		}
		return 1
	}
	if a.Index != b.Index {
		if a.Index < b.Index {
			return -1
		}
		return 1
	}
	if a.Direction != b.Direction {
		if a.Direction < b.Direction {
			return -1
		}
		return 1
	}
	// One index has one key layout at an object descriptor version.
	return 0
}
func (o *optimizer) accountLayout(layout model.TypeLayout, depth uint32) bool {
	if depth >= min(64, o.options.IR.MaxDepth) {
		o.err = ErrLimit
		return false
	}
	if !o.charge(uint64(cap(layout.Children)), uint64(unsafe.Sizeof(model.TypeLayout{}))) {
		return false
	}
	if !o.charge(uint64(cap(layout.FieldNames)), uint64(unsafe.Sizeof(""))) {
		return false
	}
	for _, name := range layout.FieldNames {
		if !o.step(1) || !o.charge(uint64(len(name)), 1) {
			return false
		}
	}
	for _, child := range layout.Children {
		if !o.accountLayout(child, depth+1) {
			return false
		}
	}
	return true
}
func (o *optimizer) lookupStatistic(target storage.ObjectRef) (*statistics.Table, bool) {
	table, found, err := o.options.Statistics.Lookup(o.ctx, target)
	if err != nil {
		o.err = err
		return nil, false
	}
	if !found {
		if table != nil {
			o.err = ErrInvalid
		}
		return nil, false
	}
	if table == nil || table.Target != target {
		o.err = ErrInvalid
		return nil, false
	}
	limits := statistics.Limits{MaxBytes: o.options.MaxBytes - o.bytes, MaxColumns: int(o.options.MaxOutputColumns), MaxValues: int(o.options.IR.MaxOperands), MaxDepth: int(min(64, o.options.IR.MaxDepth))}
	if err := statistics.Validate(o.ctx, table, limits); err != nil {
		o.err = err
		return nil, false
	}
	bytes, err := statistics.OwnedBytes(o.ctx, table, limits.MaxBytes)
	if err != nil {
		o.err = err
		return nil, false
	}
	if !o.charge(bytes, 1) {
		return nil, false
	}
	return table, true
}

func (o *optimizer) validatePathStatistic(path AccessPath, table *statistics.Table) bool {
	for _, column := range table.Columns {
		if !o.step(1) {
			return false
		}
		ordinal := int(column.ID) - 1
		if ordinal < 0 || ordinal >= len(path.KeyExpressions) || ordinal >= len(path.Keys) || path.KeyExpressions[ordinal].Graph == nil {
			o.err = ErrInvalid
			return false
		}
		key := path.Keys[ordinal]
		if !model.TypeLayoutsEqual(column.Layout, model.TypeLayout{Type: key.Type, Children: key.Children}) {
			o.err = ErrInvalid
			return false
		}
	}
	for ordinal, expression := range path.KeyExpressions {
		if expression.Graph == nil {
			continue
		}
		if !o.step(1) || !slices.ContainsFunc(table.Columns, func(column statistics.Column) bool {
			return column.ID == model.ColumnID(ordinal+1)
		}) {
			if o.err == nil {
				o.err = ErrInvalid
			}
			return false
		}
	}
	return true
}

func (o *optimizer) loadPathStatistics(id ir.RelID, object model.ObjectID, predicate ir.ScalarID, path AccessPath) {
	if o.options.Statistics == nil || o.err != nil {
		return
	}
	table, found := o.lookupStatistic(path.Target)
	if o.err != nil {
		return
	}
	entry := pathStatistic{target: path.Target, table: table, selectivity: 1}
	if found {
		if !o.validatePathStatistic(path, table) {
			return
		}
		entry.selectivity = o.accessPathSelectivity(object, predicate, path, table)
		if o.err != nil {
			return
		}
	}
	o.access[id].pathStatistics = append(o.access[id].pathStatistics, entry)
}

func (o *optimizer) pathStatistic(id ir.RelID, target storage.ObjectRef) (pathStatistic, bool) {
	for _, statistic := range o.access[id].pathStatistics {
		if !o.step(1) {
			return pathStatistic{}, false
		}
		if statistic.target == target {
			return statistic, true
		}
	}
	return pathStatistic{}, false
}
func (o *optimizer) applyPathStatistic(id ir.RelID, target storage.ObjectRef, decision *physical.ScanSpec) {
	statistic, tracked := o.pathStatistic(id, target)
	if !tracked || statistic.table == nil || o.err != nil {
		return
	}
	table := statistic.table
	if table.LiveRows.Known {
		rows := float64(table.LiveRows.Value) * statistic.selectivity
		decision.Estimate.Rows = rows
		if decision.Estimate.DecodeBytes == 0 && table.AverageRowBytes > 0 {
			decision.Estimate.DecodeBytes = rows * table.AverageRowBytes
		}
		if decision.Estimate.VisibilityChecks == 0 && table.VersionChainWorkKnown {
			decision.Estimate.VisibilityChecks = rows * table.VersionChainWork
		}
	}
}


func (o *optimizer) tableEstimate(id ir.RelID, target storage.ObjectRef) physical.Estimate {
	prior := physical.Estimate{Rows: 1000, UpperRows: 1000000, RowBytes: 64, Confidence: 0.1}
	o.access[id].blocks, o.access[id].versionWork = 10, 1
	if o.options.Statistics == nil {
		return prior
	}
	o.access[id].statisticsTarget = target
	o.access[id].statisticsTracked = true
	table, found := o.lookupStatistic(target)
	if o.err != nil || !found {
		return prior
	}
	o.access[id].statistics = table
	prior.StatisticsEpoch, prior.Confidence = table.Epoch, table.Confidence
	if table.AverageRowBytes > 0 {
		prior.RowBytes = table.AverageRowBytes
	}
	if table.LiveRows.Known {
		prior.Rows = float64(table.LiveRows.Value)
		prior.UpperRows = prior.Rows
		prior.LowerRows = prior.Rows
	} else {
		prior.Confidence = min(prior.Confidence, 0.1)
	}
	if table.Blocks.Known {
		o.access[id].blocks = float64(table.Blocks.Value)
	}
	if table.VersionChainWorkKnown {
		o.access[id].versionWork = table.VersionChainWork
	}
	return prior
}
