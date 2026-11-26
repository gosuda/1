package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type column struct {
	ResultColumn
	qualifier         string
	namespace         string
	hidden            bool
	field             *rowField
	collation         collationState
	record            recordSource
	recordFields      []column
	internal          bool
	system            bool
	unsupported       string
	unsupportedDetail string
}

func (c column) visibleToStar(qualifier relationQualifier) bool {
	if c.internal || c.system {
		return false
	}
	if qualifier.name == "" {
		return !c.hidden
	}
	return qualifier.matches(c)
}

func targetName(target *zpgparse.ResTarget) string {
	if target.Name != "" {
		return target.Name
	}
	return expressionName(target.Val)
}

type relation struct {
	id          ir.RelID
	columns     []column
	effects     ir.Effects
	occurrence  model.ScanOccurrence
	lockTargets []relationNamespace
	namespaces  []relationNamespace
}
type cteBinding struct {
	id          ir.CTEID
	columns     []column
	effects     ir.Effects
	recursive   bool
	noReturning bool
}
type scope struct {
	id               ir.ScopeID
	parent           *scope
	columns          []column
	namespaces       []relationNamespace
	ctes             map[string]cteBinding
	aggregates       []ir.AggregateSpec
	aggregateColumns []column
	// Empty record expansion can remove every call but retain aggregate cardinality.
	aggregateSeen     bool
	groupings         []ir.GroupingSpec
	groupingColumns   []column
	allowAggregate    bool
	inAggregate       bool
	groupSlots        map[model.SlotID]column
	groupNodes        []*zpgparse.Node
	groupColumns      []column
	windows           []boundWindow
	windowDefinitions map[string]windowDefinition
	allowWindow       bool
	inWindow          bool
	tailColumns       map[*zpgparse.Node]column
	rowLocks          []rowLockPolicy
	rowLockTargets    []relationNamespace
	lateralForbidden  bool
	mergeActionSlot   model.SlotID
}

func (b *binder) newScope(parent *scope) (*scope, error) {
	var parentID ir.ScopeID
	for current := parent; current != nil; current = current.parent {
		if !current.lateralForbidden {
			parentID = current.id
			break
		}
	}
	id, err := b.builder.AddScope(b.ctx, parentID, nil)
	if err != nil {
		return nil, err
	}
	return &scope{id: id, parent: parent, ctes: make(map[string]cteBinding)}, nil
}
func (s *scope) lookup(parts []string, pos int) (column, error) {
	if len(parts) < 1 || len(parts) > 3 {
		return column{}, bindError("42703", "invalid column qualification", pos)
	}
	var qualifier relationQualifier
	if len(parts) > 1 {
		qualifier.name = parts[len(parts)-2]
	}
	if len(parts) == 3 {
		qualifier.namespace = parts[0]
	}
	system := len(parts) > 0 && physicalSystemColumn(parts[len(parts)-1])
	for p := s; p != nil; p = p.parent {
		var resolved relationNamespace
		qualifierFound := false
		if len(parts) > 1 {
			var err error
			resolved, qualifierFound, err = p.relationNamespace(qualifier, pos)
			if err != nil {
				return column{}, err
			}
		}
		if p.lateralForbidden && qualifierFound {
			return column{}, bindError("42P10", "LATERAL reference requires INNER or LEFT join", pos)
		}
		var result column
		found := false
		for _, c := range p.columns {
			visible := !c.internal && (len(parts) == 1 && !c.hidden || len(parts) > 1 && qualifier.matches(c))
			if c.Name != parts[len(parts)-1] || !visible {
				continue
			}
			if found {
				return column{}, bindError("42702", "column reference is ambiguous", pos)
			}
			result, found = c, true
		}
		if p.lateralForbidden && found {
			return column{}, bindError("42P10", "LATERAL reference requires INNER or LEFT join", pos)
		}
		if found {
			if p != s && !p.inAggregate {
				if grouped, ok := p.groupSlots[result.Slot]; ok {
					result = grouped
				}
			}
			return result, nil
		}
		if qualifierFound {
			if system && resolved.systemColumns {
				return column{}, unsupported(PhysicalStorage, pos)
			}
			return column{}, bindError("42703", "column does not exist", pos)
		}
		if system && len(parts) == 1 {
			// Count distinct physical sources: a mutation's RETURNING images
			// (target, old, new) share the target's descriptor and occurrence,
			// so they expose the same system column once. Catalog relations
			// carry no descriptor and distinguish themselves by qualifier.
			type systemSource struct {
				object     model.ObjectID
				occurrence model.ScanOccurrence
				qualifier  relationQualifier
			}
			sources := make(map[systemSource]struct{}, len(p.namespaces))
			for _, n := range p.namespaces {
				if !n.systemColumns {
					continue
				}
				key := systemSource{occurrence: n.occurrence, qualifier: n.qualifier}
				if n.descriptor != nil {
					key.object, key.qualifier = n.descriptor.ID(), relationQualifier{}
				}
				sources[key] = struct{}{}
			}
			if len(sources) > 1 {
				return column{}, bindError("42702", "column reference is ambiguous", pos)
			}
			if len(sources) == 1 {
				return column{}, unsupported(PhysicalStorage, pos)
			}
		}
	}
	if len(parts) > 1 {
		return column{}, bindError("42P01", "relation is not visible in this scope", pos)
	}
	return column{}, bindError("42703", "column does not exist", pos)
}

// physicalSystemColumn reports whether name is a PostgreSQL physical system
// column embedded tables do not carry; tableoid is implemented and resolves
// ordinarily.
func physicalSystemColumn(name string) bool {
	switch name {
	case "ctid", "xmin", "cmin", "xmax", "cmax":
		return true
	}
	return false
}
func names(nodes []*zpgparse.Node) ([]string, error) {
	result := make([]string, len(nodes))
	for i, node := range nodes {
		if node.GetString_() == nil {
			return nil, bindError("42601", "expected identifier", 0)
		}
		result[i] = node.GetString_().Sval
	}
	return result, nil
}
func outputSlots(columns []column) []model.SlotID {
	out := make([]model.SlotID, len(columns))
	for i, c := range columns {
		out[i] = c.Slot
	}
	return out
}
func (b *binder) slot(s *scope, typ model.TypeLayout, nullable bool, object model.ObjectID, col model.ColumnID) (model.SlotID, error) {
	layout, err := b.layout(typ)
	if err != nil {
		return 0, err
	}
	slot, err := b.builder.AddSlot(b.ctx, ir.SlotInfo{Scope: s.id, Layout: layout, Nullable: nullable, Object: object, Column: col})
	if err == nil {
		b.slotScopes[slot] = s.id
	}
	return slot, err
}

// passThrough returns a binding expression that forwards an input column by slot
// without reading it. Catalog columns that are unavailable in this profile must
// not be referenced: that would report the unimplemented feature for statements
// that never name the column. Pass-through still binds every output, which
// Project and ProjectSet require, and leaves the column addressable if a target
// names it, at which point the feature is reported at that position.
func (b *binder) passThrough(s *scope, c column) (ir.ScalarID, error) {
	if err := b.capture(s, c.Slot); err != nil {
		return 0, err
	}
	pass := expression{typ: c.Layout, nullable: c.Nullable, reads: []model.SlotID{c.Slot}, record: c.record, recordFields: c.recordFields}
	ref, err := b.addScalar(s, pass, ir.Scalar{Kind: ir.Slot, Slot: c.Slot}, nil)
	if err != nil {
		return 0, err
	}
	return ref.id, nil
}

func (b *binder) reference(s *scope, c column, pos int) (expression, error) {
	if c.unsupported != "" {
		err := unsupported(FeatureID(c.unsupported), pos)
		if diagnostic, ok := err.(*model.Error); ok {
			diagnostic.Detail = c.unsupportedDetail
		}
		return expression{}, err
	}
	if c.field != nil {
		return b.fieldReference(s, c, pos)
	}
	if err := b.capture(s, c.Slot); err != nil {
		return expression{}, err
	}
	return b.addScalar(s, expression{typ: c.Layout, nullable: c.Nullable, position: pos, reads: []model.SlotID{c.Slot}, record: c.record, recordFields: c.recordFields}, ir.Scalar{Kind: ir.Slot, Slot: c.Slot}, nil)
}

func (b *binder) targetReference(s *scope, c column, pos int) (expression, error) {
	e, err := b.reference(s, c, pos)
	e.collation = c.collation
	return e, err
}

func aliasColumns(columns []column, alias *zpgparse.Alias, fallback string) ([]column, error) {
	return aliasOwnedColumns(slices.Clone(columns), alias, fallback)
}

func aliasOwnedColumns(out []column, alias *zpgparse.Alias, fallback string) ([]column, error) {
	qualifier := fallback
	if alias != nil {
		qualifier = alias.Aliasname
		next := 0
		for i := range out {
			if out[i].internal || out[i].system || next == len(alias.Colnames) {
				continue
			}
			out[i].Name = alias.Colnames[next].GetString_().GetSval()
			next++
		}
		if next != len(alias.Colnames) {
			return nil, bindError("42P10", "too many column aliases", 0)
		}
	}
	for i := range out {
		out[i].qualifier = qualifier
		out[i].namespace = ""
	}
	return out, nil
}

func (b *binder) capture(s *scope, slot model.SlotID) error {
	owner, ok := b.slotScopes[slot]
	if !ok {
		return bindError("XX000", "unowned binding slot", 0)
	}
	for p := s; p != nil && p.id != owner; p = p.parent {
		if p.lateralForbidden {
			continue
		}
		if err := b.builder.AddScopeCapture(b.ctx, p.id, slot); err != nil {
			return err
		}
		if !slices.Contains(b.captures[p.id], slot) {
			b.captures[p.id] = append(b.captures[p.id], slot)
		}
	}
	return nil
}

func selectScope(s *scope) *scope {
	local := *s
	local.columns = nil
	local.namespaces = nil
	local.aggregates = nil
	local.aggregateColumns = nil
	local.groupings = nil
	local.groupingColumns = nil
	local.ctes = make(map[string]cteBinding, len(s.ctes))
	local.groupNodes = nil
	local.groupSlots = nil
	local.groupColumns = nil
	local.windows = nil
	local.tailColumns = nil
	local.allowAggregate = false
	local.allowWindow = false
	local.inAggregate = false
	local.inWindow = false
	for name, cte := range s.ctes {
		local.ctes[name] = cte
	}
	return &local
}
