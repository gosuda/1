package physical

import (
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateScan(n NodeSpec, logical ir.Rel) error {
	s := n.Scan
	r := s.Request
	if r.Occurrence != logical.Occurrence || s.Lookup.Enabled && s.Lookup.BaseRequest.Occurrence != logical.Occurrence {
		return invalid("scan occurrence differs from logical RTE")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if err := s.Source.Validate(); err != nil {
		return fmt.Errorf("%w: scan source: %w", ErrInvalid, err)
	}
	staleSource := s.Source.ObjectID != logical.Object || s.Source.Generation != logical.Generation || s.Source.DescriptorVersion != logical.DescriptorVersion
	unversionedSource := s.Source.DescriptorVersion == 0 || s.Source.EncodingVersion == 0
	unversionedTarget := r.Target.DescriptorVersion == 0 || r.Target.EncodingVersion == 0
	if staleSource || unversionedSource || unversionedTarget {
		return invalid("stale scan object/generation/descriptor identity")
	}
	if s.CapabilityEpoch == 0 {
		return invalid("scan capability decision missing")
	}
	wrongIndex := r.IndexID == 0 || r.IndexID != model.IndexID(r.Target.ObjectID) || r.Target.ObjectID == s.Source.ObjectID
	if n.Algorithm == SecondaryScan && wrongIndex {
		return invalid("secondary scan index identity mismatch")
	}
	if n.Algorithm != SecondaryScan && (r.IndexID != 0 || r.Target != s.Source) {
		return invalid("primary/table scan names a secondary index")
	}
	if n.Algorithm != TableScan && len(r.KeyLayout) == 0 {
		return invalid("key scan lacks typed key layout")
	}
	if r.Direction != storage.ScanForward && r.Direction != storage.ScanReverse {
		return invalid("scan direction")
	}
	for i, column := range r.Layout {
		if column.ID == 0 {
			return invalid("zero scan column")
		}
		if err := (model.TypeLayout{Type: column.Type, Children: column.Children}).Validate(); err != nil {
			return err
		}
		for _, previous := range r.Layout[:i] {
			if previous.ID == column.ID {
				return invalid("duplicate scan column")
			}
		}
	}
	for i, key := range r.KeyLayout {
		column, ok := scanColumn(r.Layout, key.ColumnID)
		if !ok || !model.TypeLayoutsEqual(model.TypeLayout{Type: column.Type, Children: column.Children}, model.TypeLayout{Type: key.Type, Children: key.Children}) {
			return invalid("scan key type mismatch")
		}
		for _, previous := range r.KeyLayout[:i] {
			if previous.ColumnID == key.ColumnID {
				return invalid("duplicate scan key")
			}
		}
	}
	for _, span := range r.Spans {
		for _, bound := range [...]storage.KeyBound{span.Lower, span.Upper} {
			if bound.Unbounded {
				if len(bound.Values) != 0 || bound.Inclusive {
					return invalid("unbounded scan endpoint contains values")
				}
				continue
			}
			if len(bound.Values) == 0 || len(bound.Values) > len(r.KeyLayout) {
				return invalid("scan endpoint key width")
			}
			for i, value := range bound.Values {
				if value.Type != r.KeyLayout[i].Type.Storage {
					return invalid("scan endpoint type mismatch")
				}
			}
		}
	}
	for _, projection := range [][]model.ColumnID{r.ColumnIDs, s.Projection} {
		for i, id := range projection {
			if _, ok := scanColumn(r.Layout, id); !ok || slices.Contains(projection[:i], id) {
				return invalid("invalid scan projection")
			}
		}
	}
	for _, id := range r.ColumnIDs {
		if !slices.Contains(s.Projection, id) {
			return invalid("accepted projection omits requested column")
		}
	}
	if len(s.ColumnMap) == 0 && !s.Lookup.Enabled && len(n.Output) != 0 {
		return invalid("scan column mapping missing")
	}
	for i, mapping := range s.ColumnMap {
		if mapping.Source == 0 || mapping.Target == 0 {
			return invalid("zero scan column mapping")
		}
		if _, ok := scanColumn(r.Layout, mapping.Target); !ok {
			return invalid("scan mapping target missing")
		}
		for _, previous := range s.ColumnMap[:i] {
			if previous.Source == mapping.Source || previous.Target == mapping.Target {
				return invalid("duplicate scan column mapping")
			}
		}
		if n.Algorithm != SecondaryScan && mapping.Source != mapping.Target {
			return invalid("primary scan mapping is not identity")
		}
	}
	g, _ := p.Graph(n.Logical.Graph)
	for _, output := range n.Output {
		info, _ := g.Slot(output.ID)
		target := s.PhysicalColumn(info.Column)
		request, projection := r, s.Projection
		if target == 0 && s.Lookup.Enabled {
			target = s.Lookup.PhysicalColumn(info.Column)
			request, projection = s.Lookup.BaseRequest, s.Lookup.BaseProjection
		}
		column, ok := scanColumn(request.Layout, target)
		if !ok {
			return invalid("scan output not supplied by accepted projection/layout")
		}
		wrongSource := info.Object != s.Source.ObjectID || column.Nullable != info.Nullable
		missingProjection := !slices.Contains(request.ColumnIDs, target) || !slices.Contains(projection, target)
		if wrongSource || missingProjection || !layoutMatches(g, info.Layout, model.TypeLayout{Type: column.Type, Children: column.Children}) {
			return invalid("scan output not supplied by accepted projection/layout")
		}
	}
	for _, predicate := range []storage.Predicate{r.Predicate, s.Accepted, s.Residual} {
		for i := range predicate.NodeCount() {
			node := predicate.Node(uint32(i))
			if node.Kind != storage.PredicateColumn {
				continue
			}
			column, ok := scanColumn(r.Layout, node.ColumnID)
			if !ok || column.Type != node.Type {
				return invalid("predicate column type mismatch")
			}
		}
	}
	for i := range s.Residual.NodeCount() {
		node := s.Residual.Node(uint32(i))
		if node.Kind == storage.PredicateColumn && !slices.Contains(s.Projection, node.ColumnID) {
			return invalid("projection omits residual predicate input")
		}
	}
	if err := validateLookupScan(s, n.Algorithm); err != nil {
		return err
	}
	unsafeLimit := !r.SafeLimit.Enabled || s.AppliedLimit.Count != r.SafeLimit.Count || !s.Residual.Empty() || s.LimitOwner == 0
	if s.AppliedLimit.Enabled && unsafeLimit {
		return invalid("unsafe storage limit")
	}
	if r.SafeLimit.Enabled != s.AppliedLimit.Enabled || r.SafeLimit.Enabled != (s.LimitOwner != 0) {
		return invalid("scan limit decision incomplete")
	}
	if !finite(s.Estimate.Rows, s.Estimate.ReadBytes, s.Estimate.DecodeBytes, s.Estimate.VisibilityChecks, s.Estimate.IndexProbes) {
		return invalid("invalid scan estimate")
	}
	if n.Delivered.Distribution.Kind != SingleNode || n.Delivered.Rewindability != OnePass {
		return invalid("scan has no represented partition or rewind capability")
	}
	if len(n.Delivered.Ordering) > len(s.Ordering) {
		return invalid("scan ordering claim exceeds acceptance")
	}
	for i, order := range s.Ordering {
		if int(order.KeyOrdinal) >= len(r.KeyLayout) {
			return invalid("accepted scan key ordinal")
		}
		key := r.KeyLayout[order.KeyOrdinal]
		if order.Type != key.Type || order.NullsFirst != (key.NullsFirst != (r.Direction == storage.ScanReverse)) || order.Descending != (key.Descending != (r.Direction == storage.ScanReverse)) || int(order.KeyOrdinal) != i {
			return invalid("accepted scan ordering differs from key prefix")
		}
		if i >= len(n.Delivered.Ordering) {
			continue
		}
		delivered := n.Delivered.Ordering[i]
		info, _ := g.Slot(delivered.Key.Slot.ID)
		if s.PhysicalColumn(info.Column) != key.ColumnID || delivered.Key.Type != order.Type || (delivered.Direction == ir.Descending) != order.Descending || (delivered.Nulls == ir.NullsFirst) != order.NullsFirst {
			return invalid("scan ordering delivery mismatch")
		}
	}
	return nil
}

// PhysicalColumn returns zero when the logical column is not stored by this scan.
func (s ScanSpec) PhysicalColumn(source model.ColumnID) model.ColumnID {
	for _, column := range s.ColumnMap {
		if column.Source == source {
			return column.Target
		}
	}
	return 0
}

// LogicalColumn returns zero for internal storage columns.
func (s ScanSpec) LogicalColumn(target model.ColumnID) model.ColumnID {
	for _, column := range s.ColumnMap {
		if column.Target == target {
			return column.Source
		}
	}
	return 0
}

func (s LookupScanSpec) PhysicalColumn(source model.ColumnID) model.ColumnID {
	for _, column := range s.BaseColumnMap {
		if column.Source == source {
			return column.Target
		}
	}
	return 0
}

func validateLookupScan(scan ScanSpec, algorithm Algorithm) error {
	lookup := scan.Lookup
	if !lookup.Enabled {
		if !emptyLookupScan(lookup) {
			return invalid("disabled secondary lookup carries metadata")
		}
		return nil
	}
	if algorithm != SecondaryScan || lookup.OwnerColumn == 0 {
		return invalid("secondary lookup belongs only to a secondary scan")
	}
	request := lookup.BaseRequest
	if err := request.Target.Validate(); err != nil {
		return err
	}
	for i, column := range request.Layout {
		if column.ID == 0 {
			return invalid("zero secondary lookup base column")
		}
		if err := (model.TypeLayout{Type: column.Type, Children: column.Children}).Validate(); err != nil {
			return err
		}
		for _, previous := range request.Layout[:i] {
			if previous.ID == column.ID {
				return invalid("duplicate secondary lookup base column")
			}
		}
	}
	if request.Target != scan.Source || request.IndexID != 0 || len(request.KeyLayout) == 0 || len(request.ColumnIDs) == 0 || len(lookup.BaseProjection) == 0 || len(lookup.BaseColumnMap) == 0 || request.Direction != storage.ScanForward {
		return invalid("secondary lookup base request identity")
	}
	if request.SafeLimit != (storage.ScanLimit{}) || !request.Predicate.Empty() || !lookup.BaseAccepted.Empty() || !lookup.BaseResidual.Empty() {
		return invalid("secondary lookup base request is not an exact key probe")
	}
	if len(request.Spans) != 1 {
		return invalid("secondary lookup base request lacks an unbounded planning span")
	}
	span := request.Spans[0]
	if !span.Lower.Unbounded || span.Lower.Inclusive || len(span.Lower.Values) != 0 || !span.Upper.Unbounded || span.Upper.Inclusive || len(span.Upper.Values) != 0 {
		return invalid("secondary lookup base request lacks an unbounded planning span")
	}
	owner, ok := scanColumn(scan.Request.Layout, lookup.OwnerColumn)
	if !ok || owner.Nullable || owner.Type.Storage != model.TypeBytes || !slices.Contains(scan.Request.ColumnIDs, lookup.OwnerColumn) || !slices.Contains(scan.Projection, lookup.OwnerColumn) {
		return invalid("secondary lookup owner key is not projected")
	}
	for _, projection := range [][]model.ColumnID{request.ColumnIDs, lookup.BaseProjection} {
		for i, id := range projection {
			if _, ok := scanColumn(request.Layout, id); !ok || slices.Contains(projection[:i], id) {
				return invalid("secondary lookup base projection")
			}
		}
	}
	for _, id := range request.ColumnIDs {
		if !slices.Contains(lookup.BaseProjection, id) || lookup.PhysicalColumn(id) == 0 {
			return invalid("secondary lookup base projection omits requested column")
		}
	}
	for i, mapping := range lookup.BaseColumnMap {
		if mapping.Source == 0 || mapping.Target != mapping.Source {
			return invalid("secondary lookup base mapping is not identity")
		}
		if _, ok := scanColumn(request.Layout, mapping.Target); !ok {
			return invalid("secondary lookup base mapping target missing")
		}
		for _, previous := range lookup.BaseColumnMap[:i] {
			if previous.Source == mapping.Source {
				return invalid("duplicate secondary lookup base mapping")
			}
		}
	}
	for i, key := range request.KeyLayout {
		column, ok := scanColumn(request.Layout, key.ColumnID)
		if !ok || !model.TypeLayoutsEqual(model.TypeLayout{Type: column.Type, Children: column.Children}, model.TypeLayout{Type: key.Type, Children: key.Children}) {
			return invalid("secondary lookup base key layout")
		}
		for _, previous := range request.KeyLayout[:i] {
			if previous.ColumnID == key.ColumnID {
				return invalid("duplicate secondary lookup base key")
			}
		}
	}
	return nil
}

func emptyLookupScan(s LookupScanSpec) bool {
	r := s.BaseRequest
	return !s.Enabled && s.OwnerColumn == 0 && len(s.BaseColumnMap) == 0 &&
		r.Target == (storage.ObjectRef{}) && r.Occurrence == 0 && r.IndexID == 0 && len(r.Layout) == 0 &&
		len(r.KeyLayout) == 0 && len(r.ColumnIDs) == 0 && len(r.Spans) == 0 &&
		r.Direction == storage.ScanForward && r.Predicate.Empty() && r.SafeLimit == (storage.ScanLimit{}) &&
		s.BaseAccepted.Empty() && s.BaseResidual.Empty() && len(s.BaseProjection) == 0
}

func scanColumn(layout []storage.ColumnLayout, id model.ColumnID) (storage.ColumnLayout, bool) {
	for _, column := range layout {
		if column.ID == id {
			return column, true
		}
	}
	return storage.ColumnLayout{}, false
}
func layoutMatches(g *ir.Graph, id ir.LayoutID, layout model.TypeLayout) bool {
	typ, ok := g.Type(id)
	if !ok || typ != layout.Type || int(g.LayoutChildCount(id)) != len(layout.Children) {
		return false
	}
	for i, child := range layout.Children {
		childID, _ := g.LayoutChild(id, uint32(i))
		if !layoutMatches(g, childID, child) {
			return false
		}
	}
	return true
}

func emptyScan(s ScanSpec) bool {
	r := s.Request
	return s.Source == (storage.ObjectRef{}) && len(s.ColumnMap) == 0 && s.CapabilityEpoch == 0 && s.LimitOwner == 0 && r.Target == (storage.ObjectRef{}) && r.Occurrence == 0 && r.IndexID == 0 && len(r.Layout) == 0 && len(r.KeyLayout) == 0 && len(r.ColumnIDs) == 0 && len(r.Spans) == 0 && r.Direction == storage.ScanForward && r.Predicate.Empty() && r.SafeLimit == (storage.ScanLimit{}) && s.Accepted.Empty() && s.Residual.Empty() && len(s.Projection) == 0 && len(s.Ordering) == 0 && s.Estimate == (storage.ScanEstimate{}) && s.AppliedLimit == (storage.ScanLimit{}) && emptyLookupScan(s.Lookup)
}
