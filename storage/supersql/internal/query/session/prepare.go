package session

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// PrepareOptions supplies bounded stage configuration and immutable planning
// collaborators. Preparing a statement takes no transaction or reader ownership.
type PrepareOptions struct {
	Bind      bind.Options
	Normalize normalize.Options
	Optimize  opt.Options
	Fragment  fragment.Limits
	// ValidateBound runs before normalization can remove unreachable effects.
	ValidateBound func(context.Context, *bind.BoundStatement) error
}

// Prepared owns immutable bound semantics and the resulting physical pipelines.
// It retains no catalog snapshot, storage reader or execution state.
type Prepared struct {
	bound             *bind.BoundStatement
	graph             *fragment.FragmentGraph
	requiredTimestamp model.Timestamp
	builtins          builtin.ResolveOptions
	statistics        []opt.StatisticVersion
	estimatedCost     float64
	planningDuration  time.Duration
	budgetExhausted   bool
}

func Prepare(ctx context.Context, parsed *zpgparse.RawStmt, snapshot bind.CatalogSnapshot, options PrepareOptions, parameters []model.TypeLayout) (*Prepared, error) {
	options.Bind.Builtin = options.Normalize.Builtin
	bound, err := bind.Bind(ctx, parsed, snapshot, options.Bind, parameters)
	if err != nil {
		return nil, err
	}
	return prepareBound(ctx, bound, options)
}

func prepareBound(ctx context.Context, bound *bind.BoundStatement, options PrepareOptions) (*Prepared, error) {
	if bound == nil {
		return nil, errPreparedInvocation
	}
	if options.ValidateBound != nil {
		if err := options.ValidateBound(ctx, bound); err != nil {
			return nil, err
		}
	}
	if bound.Command() != nil {
		return &Prepared{bound: bound, builtins: options.Normalize.Builtin}, nil
	}
	logical, err := normalize.Normalize(ctx, bound.Graph(), options.Normalize)
	if err != nil {
		return nil, err
	}
	if len(options.Optimize.Parameters) != 0 {
		options.Optimize.Parameters = filterSpanParametersForGraph(logical, options.Optimize.Parameters)
	}
	optimized, err := opt.Optimize(ctx, logical, options.Optimize)
	if err != nil {
		return nil, err
	}
	graph, err := fragment.Build(ctx, optimized.Plan, options.Fragment)
	if err != nil {
		return nil, err
	}
	root, ok := optimized.Plan.Node(optimized.Plan.Root())
	if !ok {
		return nil, errPreparedInvocation
	}
	return &Prepared{
		bound: bound, graph: graph, builtins: options.Normalize.Builtin,
		statistics:    slices.Clone(optimized.Statistics),
		estimatedCost: root.Metadata().Cost.Total, planningDuration: optimized.PlanningDuration,
		budgetExhausted: optimized.BudgetExhausted,
	}, nil
}

func (p *Prepared) Columns() []bind.ResultColumn     { return p.bound.Columns() }
func (p *Prepared) Parameters() []model.TypeLayout   { return p.bound.Parameters() }
func (p *Prepared) Dependencies() []model.Dependency { return p.bound.Dependencies() }
func (p *Prepared) Copy() *qcopy.Plan                { return p.bound.Copy() }
func (p *Prepared) Timestamp() model.Timestamp       { return p.bound.Timestamp() }
func (p *Prepared) Graph() *fragment.FragmentGraph   { return p.graph }
func (p *Prepared) Warnings() []model.Error          { return p.bound.Warnings() }

// Builtin returns the resolve options this statement planned with, including
// the declared-type resolver pinned to the statement's snapshot. Result
// rendering uses it to resolve the output metadata of enum, domain, and other
// declared-typed columns after execution has released its own pin.
func (p *Prepared) Builtin() builtin.ResolveOptions { return p.builtins }

var errPreparedSnapshot = &model.Error{Code: "22023", Message: "historical execution snapshot differs from prepared query"}

var errPreparedSemantics = &model.Error{Code: "0A000", Message: "prepared query semantic settings changed; prepare again"}

var errPreparedPublication = &model.Error{Code: "0A000", Message: "prepared query has unpublished or abandoned catalog bindings; prepare again"}

func (p *Prepared) validatePublication(owner *model.PublicationOwner) error {
	if p != nil && p.bound != nil && !p.bound.ProvisionalBindingsUsable(owner) {
		return errPreparedPublication
	}
	return nil
}

// Open creates independent invocation state. Environment transfers its Query
// and Reader on every outcome, exactly as exec.Open does.
// Builtin semantics must match preparation so folded and runtime values agree.
func (p *Prepared) Open(ctx context.Context, environment exec.Environment) (*exec.Portal, error) {
	if p == nil || p.graph == nil {
		return exec.Open(ctx, nil, environment)
	}
	var owner *model.PublicationOwner
	if source, ok := environment.Reader.(interface {
		PublicationOwner() *model.PublicationOwner
	}); ok {
		owner = source.PublicationOwner()
	}
	if err := p.validatePublication(owner); err != nil {
		return nil, rejectPreparedEnvironment(environment, err)
	}
	if !environment.Builtin.SameSemantics(p.builtins) {
		return nil, rejectPreparedEnvironment(environment, errPreparedSemantics)
	}
	if p.requiredTimestamp != 0 {
		reader, ok := environment.Reader.(interface{ Timestamp() model.Timestamp })
		if !ok || reader.Timestamp() != p.requiredTimestamp {
			return nil, rejectPreparedEnvironment(environment, errPreparedSnapshot)
		}
	}
	return exec.Open(ctx, p.graph, environment)
}

func rejectPreparedEnvironment(environment exec.Environment, err error) error {
	if environment.Reader != nil && !environment.BorrowReader {
		err = errors.Join(err, environment.Reader.Close())
	}
	if environment.Query != nil && !environment.BorrowQuery {
		err = errors.Join(err, environment.Query.Close())
	}
	return err
}

func spanParameters(parameters []expr.ParameterValue) []span.Parameter {
	if len(parameters) == 0 {
		return nil
	}
	result := make([]span.Parameter, len(parameters))
	for i, parameter := range parameters {
		result[i] = span.Parameter{Number: parameter.Number, Layout: parameter.Layout, Value: parameter.Value}
	}
	return result
}

// filterSpanParametersForGraph narrows Optimize.Parameters to the exact
// ir.Param set present in the normalized graph. Constant folding or routine
// frame passing can leave declared parameters unreferenced in the normalized
// IR; span.Extract rejects unreferenced or mismatched parameters as ErrInvalid,
// so filtering here keeps span specialization exact without false rejections.
func filterSpanParametersForGraph(graph *ir.Graph, parameters []span.Parameter) []span.Parameter {
	if graph == nil || len(parameters) == 0 {
		return nil
	}
	needed := make(map[uint32]ir.Scalar)
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		node, ok := graph.Scalar(id)
		if !ok || node.Kind != ir.Param {
			continue
		}
		if existing, exists := needed[node.Parameter]; !exists || (existing.Nullable && !node.Nullable) {
			needed[node.Parameter] = node
		}
	}
	if len(needed) == 0 {
		return nil
	}
	matched := make([]span.Parameter, 0, len(needed))
	seen := make(map[uint32]struct{}, len(needed))
	for _, parameter := range parameters {
		node, ok := needed[parameter.Number]
		if !ok {
			continue
		}
		if _, duplicate := seen[parameter.Number]; duplicate {
			return nil
		}
		if (!node.Nullable && parameter.Value.IsNull) || !graphLayoutEquals(graph, node.Layout, parameter.Layout, 0) || !validSpanValue(parameter.Layout, parameter.Value, 0) {
			return nil
		}
		seen[parameter.Number] = struct{}{}
		matched = append(matched, parameter)
	}
	if len(matched) != len(needed) {
		return nil
	}
	return matched
}

func graphLayoutEquals(graph *ir.Graph, id ir.LayoutID, layout model.TypeLayout, depth int) bool {
	if depth > 64 {
		return false
	}
	typ, ok := graph.Type(id)
	if !ok || typ != layout.Type || uint64(graph.LayoutChildCount(id)) != uint64(len(layout.Children)) {
		return false
	}
	for i, child := range layout.Children {
		childID, _ := graph.LayoutChild(id, uint32(i))
		if !graphLayoutEquals(graph, childID, child, depth+1) {
			return false
		}
	}
	return true
}

func validSpanValue(layout model.TypeLayout, value model.Value, depth int) bool {
	if depth > 64 || value.Type != layout.Type.Storage {
		return false
	}
	if value.IsNull {
		return true
	}
	switch value.Type {
	case model.TypeArray:
		if len(layout.Children) != 1 || model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))) != nil {
			return false
		}
		for _, item := range value.ArrayVal {
			if !validSpanValue(layout.Children[0], item, depth+1) {
				return false
			}
		}
	case model.TypeComposite:
		if len(value.ArrayVal) != len(layout.Children) {
			return false
		}
		for i, item := range value.ArrayVal {
			if !validSpanValue(layout.Children[i], item, depth+1) {
				return false
			}
		}
	}
	return true
}

