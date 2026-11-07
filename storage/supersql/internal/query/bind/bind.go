package bind

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// CatalogSnapshot is borrowed for Bind; returned statements retain no reader.
type CatalogSnapshot interface {
	Timestamp() model.Timestamp
	Resolve(context.Context, model.NamespaceName) (catalog.PublishedDescriptor, error)
	Descriptor(context.Context, model.ObjectID) (catalog.PublishedDescriptor, error)
	ScanNamespace(context.Context, model.ObjectID, model.ObjectKind, func(catalog.PublishedDescriptor) error) error
}

type Options struct {
	// SearchPath contains exact normalized namespace names, not SQL fragments.
	SearchPath       []string
	CurrentRole      string
	Collation        model.CollationID
	DefaultCollation model.CollationID
	// Builtin captures input-codec settings while SQL literals are bound.
	Builtin builtin.ResolveOptions
	// Temporal is borrowed only while resolving temporal input literals.
	Temporal model.TemporalRuntime
	// CatalogRelations is the immutable per-database logical relation registry.
	CatalogRelations *catalogrel.Registry
	// MaterializedViewTarget authorizes internal population of exactly one
	// materialized-view descriptor; ordinary SQL leaves it zero.
	MaterializedViewTarget model.ObjectID
	// Limits bound the AST, descriptor copies, and final immutable IR separately.
	Limits      ir.Limits
	declaration *declarationScope
	// allowPseudoTypes admits a declared pseudo-type identity (anyelement,
	// internal, anyarray, ...) as a childless marker layout. It is set only while
	// binding a CREATE declaration, where PostgreSQL genuinely permits such a
	// type: a routine parameter list and an aggregate's argument or state type.
	// Ordinary queries never set it, so a pseudo-type reference in SQL still
	// fails as an unresolvable physical layout. The marker is a DECLARATION
	// identity only and must never reach execution, storage, key layouts, or the
	// PAX/encoding paths.
	allowPseudoTypes bool
}

// AdmitPseudoTypes marks this as a declaration or routine-body bind, where a
// pseudo-type identity may appear as a parameter, operand, or result layout in
// marker form. Executable statements must not set it: a marker reaching
// execution is a defect, so ordinary binds keep the physical-layout contract.
func (o *Options) AdmitPseudoTypes() { o.allowPseudoTypes = true }

type ResultColumn struct {
	Name     string
	Layout   model.TypeLayout
	Nullable bool
	Slot     model.SlotID
}

type BoundStatement struct {
	graph         *ir.Graph
	command       Command
	columns       []ResultColumn
	copy          *qcopy.Plan
	parameters    []model.TypeLayout
	dependencies  []model.Dependency
	provisional   []*model.ProvisionalBinding
	effects       ir.Effects
	timestamp     model.Timestamp
	warnings      []model.Error
	procedureCall bool
}

func (s *BoundStatement) Graph() *ir.Graph                 { return s.graph }
func (s *BoundStatement) Effects() ir.Effects              { return s.effects }
func (s *BoundStatement) Copy() *qcopy.Plan                { return s.copy.Clone() }
func (s *BoundStatement) Timestamp() model.Timestamp       { return s.timestamp }
func (s *BoundStatement) Dependencies() []model.Dependency { return slices.Clone(s.dependencies) }
func (s *BoundStatement) DependencyCount() int             { return len(s.dependencies) }
func (s *BoundStatement) ProcedureCall() bool              { return s.procedureCall }
func (s *BoundStatement) ProvisionalBindingsUsable(owner *model.PublicationOwner) bool {
	for _, binding := range s.provisional {
		if !binding.Usable(owner) {
			return false
		}
	}
	return true
}
func (s *BoundStatement) Dependency(index int) (model.Dependency, bool) {
	if index < 0 || index >= len(s.dependencies) {
		return model.Dependency{}, false
	}
	return s.dependencies[index], true
}
func (s *BoundStatement) Warnings() []model.Error { return slices.Clone(s.warnings) }
func (s *BoundStatement) Columns() []ResultColumn {
	out := slices.Clone(s.columns)
	for i := range out {
		out[i].Layout = cloneLayout(out[i].Layout)
	}
	return out
}
func (s *BoundStatement) Parameters() []model.TypeLayout {
	out := slices.Clone(s.parameters)
	for i := range out {
		out[i] = cloneLayout(out[i])
	}
	return out
}
func cloneLayout(l model.TypeLayout) model.TypeLayout {
	l.FieldNames = slices.Clone(l.FieldNames)
	for i, name := range l.FieldNames {
		l.FieldNames[i] = strings.Clone(name)
	}
	l.Children = slices.Clone(l.Children)
	for i := range l.Children {
		l.Children[i] = cloneLayout(l.Children[i])
	}
	return l
}

func (b *binder) clearParameterFieldNames(layout *model.TypeLayout) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if layout.Type.Storage == model.TypeComposite && layout.Type.ID != 2249 {
		layout.FieldNames = nil
	}
	for i := range layout.Children {
		if err := b.clearParameterFieldNames(&layout.Children[i]); err != nil {
			return err
		}
	}
	return nil
}

// MissingBindingError identifies required SQL still missing an implementation.
// It is not a compatibility-profile exclusion.
type MissingBindingError struct {
	Surface  string
	Position int
}

func (e *MissingBindingError) Error() string {
	return fmt.Sprintf("required SQL binding not implemented: %s (position %d)", e.Surface, e.Position)
}

func Bind(ctx context.Context, parsed *zpgparse.RawStmt, snapshot CatalogSnapshot, options Options, paramTypes []model.TypeLayout) (*BoundStatement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parsed == nil || parsed.Stmt == nil {
		return nil, bindError("42601", "expected one statement", 0)
	}
	if err := ValidateStatementFeatures(ctx, parsed.Stmt); err != nil {
		return nil, err
	}
	if options.Collation == 0 || options.DefaultCollation == 0 || options.DefaultCollation == 100 {
		return nil, bindError("22023", "binding requires resolved collation settings", 0)
	}
	if err := ValidateBuiltinCollation(options.Collation, 0); err != nil {
		return nil, err
	}
	if err := ValidateBuiltinCollation(options.DefaultCollation, 0); err != nil {
		return nil, err
	}
	newBuilder := ir.NewBuilder
	if options.allowPseudoTypes {
		// Routine declarations bind bodies whose signature types are still
		// declared pseudo-type markers; the graph is checked, not executed.
		newBuilder = ir.NewDeclaredBuilder
	}
	builder, err := newBuilder(options.Limits)
	if err != nil {
		return nil, err
	}
	if uint64(len(paramTypes)) > uint64(options.Limits.MaxOperands) {
		return nil, bindError("54000", "parameter limit exceeded", 0)
	}
	if uint64(len(options.SearchPath)) > uint64(options.Limits.MaxOperands) {
		return nil, bindError("54000", "search path limit exceeded", 0)
	}
	for _, schema := range options.SearchPath {
		if schema == "" {
			return nil, bindError("22023", "empty search path entry", 0)
		}
		if uint64(len(schema)) > options.Limits.MaxBytes {
			return nil, bindError("54000", "search path byte limit exceeded", 0)
		}
	}
	options.SearchPath = slices.Clone(options.SearchPath)
	b := binder{ctx: ctx, catalog: snapshot, options: options, builder: builder, params: make(map[uint32]model.TypeLayout), layouts: make(map[model.Type]ir.LayoutID), views: make(map[model.ObjectID]bool), dependencySet: make(map[model.Dependency]struct{})}
	b.topWith = statementWith(parsed.Stmt)
	b.slotScopes = make(map[model.SlotID]ir.ScopeID)
	b.captures = make(map[ir.ScopeID][]model.SlotID)
	remaining, payloadBytes := options.Limits.MaxNodes, options.Limits.MaxBytes
	if err := b.checkAST(parsed.Stmt.ProtoReflect(), 0, &remaining, &payloadBytes); err != nil {
		return nil, err
	}
	layoutBytes := options.Limits.MaxBytes
	for i, typ := range paramTypes {
		if typ.Type.ID == 0 {
			continue
		}
		owned, used, err := model.CopyDeclaredTypeLayout(ctx, typ, layoutBytes)
		if err != nil {
			return nil, err
		}
		layoutBytes -= used
		if err := b.clearParameterFieldNames(&owned); err != nil {
			return nil, err
		}
		owned, err = b.presentationLayout(owned, 0)
		if err != nil {
			return nil, err
		}
		b.params[uint32(i+1)] = owned
	}
	command, columns, err := b.command(parsed.Stmt)
	if err != nil {
		return nil, err
	}
	if command != nil {
		if len(paramTypes) != 0 {
			return nil, bindError("42P02", "utility command does not accept parameters", 0)
		}
		return b.commandStatement(command, columns), nil
	}
	s, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	relation, err := b.statement(parsed.Stmt, s)
	if err != nil {
		return nil, err
	}
	parameters := make([]model.TypeLayout, max(b.maxParam, uint32(len(paramTypes))))
	for i := range parameters {
		typ, ok := b.params[uint32(i+1)]
		if !ok {
			if i < len(paramTypes) && paramTypes[i].Type.ID == 0 {
				// A zero-ID declared slot is deliberately untyped: procedural
				// record fields resolve their layout per execution. PostgreSQL
				// reports such parameters as unknown, so the slot carries the
				// unknown layout; references to it infer a concrete type.
				typ, ok = builtin.BuiltinTypeLayout(705, -1)
				if !ok {
					return nil, bindError("XX000", "unknown parameter type layout unavailable", 0)
				}
			} else {
				return nil, bindError("42P08", "could not determine parameter type", 0)
			}
		}
		parameters[i] = typ
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Match PostgreSQL's analysis, rewrite, then arbiter-planning error phases.
	// No invalid diagnostic draft reaches graph verification or execution.
	if b.writeError != nil {
		return nil, b.writeError
	}
	if b.arbiterError != nil {
		return nil, b.arbiterError
	}
	graph, err := builder.Build(ctx, relation.id)
	if err != nil {
		return nil, err
	}
	result := &BoundStatement{graph: graph, copy: b.copy, parameters: parameters, dependencies: b.dependencies, provisional: b.provisional, effects: relation.effects, warnings: b.warnings, procedureCall: b.procedureCall}
	for _, c := range relation.columns {
		result.columns = append(result.columns, c.ResultColumn)
	}
	if snapshot != nil {
		result.timestamp = snapshot.Timestamp()
	}
	return result, nil
}

type binder struct {
	ctx                      context.Context
	catalog                  CatalogSnapshot
	options                  Options
	builder                  *ir.Builder
	params                   map[uint32]model.TypeLayout
	maxParam                 uint32
	layouts                  map[model.Type]ir.LayoutID
	copy                     *qcopy.Plan
	dependencies             []model.Dependency
	dependencySet            map[model.Dependency]struct{}
	catalogDependenciesBound bool
	provisional              []*model.ProvisionalBinding
	rowRegclassLiterals      map[*zpgparse.Node]model.OID
	rowTemporalLiterals      map[*zpgparse.Node]string
	rowTemporalBytes         uint64
	views                    map[model.ObjectID]bool
	depth                    uint32
	nodes                    uint32
	slotScopes               map[model.SlotID]ir.ScopeID
	captures                 map[ir.ScopeID][]model.SlotID
	wholeRowNames            map[relationQualifier]bool
	namespaceBytes           uint64
	nextScanOccurrence       model.ScanOccurrence
	pushedRowLocks           map[*zpgparse.SelectStmt][]rowLockPolicy
	rangeRowLocks            map[*zpgparse.RangeVar][]rowLockPolicy
	warnings                 []model.Error
	writeError, arbiterError error
	// casts caches user-defined cast probes for this binding pass; the value is
	// a point lookup under pg_catalog, so a probe result is stable for one pass.
	casts   map[[2]model.TypeID]userCastResolution
	castErr error
	// namedTypes caches catalog type descriptors resolved by OID during this
	// binding pass. Each resolution is a full type-namespace scan against the
	// pinned snapshot, so repeating it inside overload scoring is quadratic;
	// the snapshot cannot change mid-bind, making the result stable for one
	// pass. A stored nil is a cached miss.
	namedTypes map[model.TypeID]*model.Descriptor
	// domainBases caches declared-domain unwrapping for this binding pass.
	// Overload scoring consults it once per argument per candidate, so the
	// result must not require a catalog lookup after the first resolution.
	domainBases map[model.TypeID]domainBaseResult
	// definitionFunctions caches routine descriptors resolved from a stored
	// executable FunctionID during this binding pass.
	definitionFunctions map[model.FunctionID]catalog.PublishedDescriptor
	topWith             *zpgparse.WithClause
	allowTargetSRF      bool
	srfRestriction      string
	allowWindowKeySRF   bool
	procedureCall       bool
	mutationProvenance  model.ObjectID
}

func bindError(code, message string, position int) error {
	return &model.Error{Code: code, Message: message, Position: position}
}
func missing(surface string, position int) error { return &MissingBindingError{surface, position} }
func (b *binder) enter() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	b.depth++
	b.nodes++
	if b.depth > b.options.Limits.MaxDepth || b.nodes > b.options.Limits.MaxNodes {
		return bindError("54000", "binding resource limit exceeded", 0)
	}
	return nil
}
func (b *binder) dependency(d *model.Descriptor, column model.ColumnID) {
	if binding := d.ProvisionalBinding(); binding != nil && !slices.Contains(b.provisional, binding) {
		b.provisional = append(b.provisional, binding)
	}
	dep := model.Dependency{Object: d.ID(), Version: d.Version(), Column: column}
	if _, exists := b.dependencySet[dep]; !exists {
		b.dependencies = append(b.dependencies, dep)
		b.dependencySet[dep] = struct{}{}
	}
}
func isMissingObject(err error) bool { return errors.Is(err, catalog.ErrUnknownObject) }
func sourceSpan(position int) ir.Span {
	if position <= 0 {
		return ir.Span{}
	}
	return ir.Span{Start: uint32(position - 1), End: uint32(position)}
}
