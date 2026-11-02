package bind

import (
	"bytes"
	"context"
	"encoding/json"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const routineDefaultVersion = 2

type BoundRoutineDefault struct {
	Definition model.ExpressionDefinition
	Constant   *model.Value
	Warnings   []model.Error
}

type routineDefaultLayout struct {
	Type     model.Type
	Children []ir.LayoutID
}
type routineDefaultScalar struct {
	Scalar   ir.Scalar
	Operands []ir.ScalarID
	Constant []byte
}
type routineDefaultEncoding struct {
	Format            string
	Version           uint32
	Root              ir.ScalarID
	Collation         model.CollationID
	CollationStrength collationStrength
	Layouts           []routineDefaultLayout
	Signatures        []ir.SignatureSpec
	Scalars           []routineDefaultScalar
	Identities        []routineDefaultIdentity `json:",omitempty"`
}

// BindRoutineDefault freezes parameter-free routine and column defaults into
// owned semantic bytes. Literals and callable identities are frozen; volatile
// calls remain IR. Limits bound binding, copied metadata and encoding separately.
func BindRoutineDefault(ctx context.Context, ast *zpgparse.Node, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*BoundRoutineDefault, error) {
	return defineDefault(ctx, ast, target, snapshot, options, false)
}

func defineDefault(ctx context.Context, ast *zpgparse.Node, target model.TypeLayout, snapshot CatalogSnapshot, options Options, assignment bool) (*BoundRoutineDefault, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	if ast == nil {
		return nil, bindError("42601", "missing routine default expression", 0)
	}
	remaining, payload := options.Limits.MaxNodes, options.Limits.MaxBytes
	if err := b.checkAST(ast.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	if err := validateDefaultExpression(ctx, ast, 0); err != nil {
		return nil, err
	}
	scope, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	scope.allowAggregate, scope.allowWindow = false, false
	value, err := b.scalar(ast, scope)
	if err != nil {
		return nil, err
	}
	if assignment {
		value, err = b.coerceColumnAssignment(value, scope, model.ColumnDescriptor{Layout: target})
	} else {
		value, err = b.coerce(scope, value, target, ir.ImplicitCast)
	}
	if err != nil {
		return nil, err
	}
	slot, err := b.slot(scope, value.typ, value.nullable, 0, 0)
	if err != nil {
		return nil, err
	}
	root, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 1, Effects: value.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{value.id}})
	if err != nil {
		return nil, err
	}
	graph, err := b.builder.Build(ctx, root)
	if err != nil {
		return nil, err
	}
	dependencies, identities, err := b.defaultIdentityDependencies(graph)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeRoutineDefault(ctx, graph, value.id, b.expressionCollation(value), options.Limits, identities)
	if err != nil {
		return nil, err
	}
	result := &BoundRoutineDefault{Definition: model.ExpressionDefinition{SemanticVersion: routineDefaultVersion, Encoding: encoded, Dependencies: dependencies}, Warnings: b.warnings}
	if scalar, ok := graph.Scalar(value.id); ok && scalar.Kind == ir.Const && value.effects.Volatility == ir.Immutable {
		constant := value.value.Clone()
		result.Constant = &constant
	}
	return result, nil
}

func redefineColumnDefault(ctx context.Context, definition *model.ExpressionDefinition, source, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	scope, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	value, err := b.routineDefault(definition, source, scope, 0)
	if err != nil {
		return nil, err
	}
	converted, err := b.coerceColumnAssignment(value, scope, model.ColumnDescriptor{Layout: target})
	if err != nil {
		return nil, err
	}
	slot, err := b.slot(scope, converted.typ, converted.nullable, 0, 0)
	if err != nil {
		return nil, err
	}
	root, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 1, Effects: converted.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{converted.id}})
	if err != nil {
		return nil, err
	}
	graph, err := b.builder.Build(ctx, root)
	if err != nil {
		return nil, err
	}
	dependencies, identities, err := b.defaultIdentityDependencies(graph)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeRoutineDefault(ctx, graph, converted.id, b.expressionCollation(converted), options.Limits, identities)
	if err != nil {
		return nil, err
	}
	return &model.ExpressionDefinition{SemanticVersion: routineDefaultVersion, Encoding: encoded, Dependencies: dependencies}, nil
}

func redefineConstantDefault(ctx context.Context, value model.Value, source, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	b, err := newExpressionBinder(ctx, snapshot, options)
	if err != nil {
		return nil, err
	}
	scope, err := b.newScope(nil)
	if err != nil {
		return nil, err
	}
	constant, err := b.addScalar(scope, expression{typ: source, nullable: value.IsNull, value: value.Clone()}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return nil, err
	}
	converted, err := b.coerceColumnAssignment(constant, scope, model.ColumnDescriptor{Layout: target})
	if err != nil {
		return nil, err
	}
	slot, err := b.slot(scope, converted.typ, converted.nullable, 0, 0)
	if err != nil {
		return nil, err
	}
	root, err := b.builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 1, Effects: converted.effects}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{converted.id}})
	if err != nil {
		return nil, err
	}
	graph, err := b.builder.Build(ctx, root)
	if err != nil {
		return nil, err
	}
	dependencies, identities, err := b.defaultIdentityDependencies(graph)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeRoutineDefault(ctx, graph, converted.id, b.expressionCollation(converted), options.Limits, identities)
	if err != nil {
		return nil, err
	}
	return &model.ExpressionDefinition{SemanticVersion: routineDefaultVersion, Encoding: encoded, Dependencies: dependencies}, nil
}

type routineDefaultBudget struct{ remaining uint64 }

func (budget *routineDefaultBudget) charge(bytes uint64) error {
	if bytes > budget.remaining {
		return bindError("54000", "routine default metadata limit exceeded", 0)
	}
	budget.remaining -= bytes
	return nil
}

type routineDefaultCounter struct{ bytes uint64 }

func (counter *routineDefaultCounter) Write(data []byte) (int, error) {
	counter.bytes += uint64(len(data))
	return len(data), nil
}

func encodeRoutineDefault(ctx context.Context, graph *ir.Graph, root ir.ScalarID, collation collationState, limits ir.Limits, identities []routineDefaultIdentity) ([]byte, error) {
	budget := routineDefaultBudget{remaining: limits.MaxBytes}
	headers := uint64(graph.LayoutCount())*uint64(unsafe.Sizeof(routineDefaultLayout{})) + uint64(graph.SignatureCount())*uint64(unsafe.Sizeof(ir.SignatureSpec{})) + uint64(graph.ScalarCount())*uint64(unsafe.Sizeof(routineDefaultScalar{}))
	headers += uint64(len(identities)) * uint64(unsafe.Sizeof(routineDefaultIdentity{}))
	if err := budget.charge(headers); err != nil {
		return nil, err
	}
	encoding := routineDefaultEncoding{Format: "supersql.routine-default", Version: routineDefaultVersion, Root: root, Layouts: make([]routineDefaultLayout, graph.LayoutCount()), Signatures: make([]ir.SignatureSpec, graph.SignatureCount()), Scalars: make([]routineDefaultScalar, graph.ScalarCount())}
	encoding.Collation, encoding.CollationStrength = collation.id, collation.strength
	encoding.Identities = identities
	jsonBound := uint64(256) + uint64(len(encoding.Layouts))*256 + uint64(len(encoding.Signatures))*512 + uint64(len(encoding.Scalars))*1024
	jsonBound += uint64(len(identities)) * 512
	for i := range encoding.Layouts {
		id := ir.LayoutID(i + 1)
		typ, _ := graph.Type(id)
		count := graph.LayoutChildCount(id)
		if err := budget.charge(uint64(count) * 4); err != nil {
			return nil, err
		}
		children := make([]ir.LayoutID, count)
		for j := range children {
			children[j], _ = graph.LayoutChild(id, uint32(j))
		}
		encoding.Layouts[i] = routineDefaultLayout{typ, children}
		jsonBound += uint64(count) * 12
	}
	for i := range encoding.Signatures {
		id := ir.SignatureID(i + 1)
		signature, _ := graph.Signature(id)
		if err := budget.charge(uint64(signature.ParameterCount()) * 4); err != nil {
			return nil, err
		}
		parameters := make([]ir.LayoutID, signature.ParameterCount())
		for j := range parameters {
			parameters[j], _ = graph.SignatureParameter(id, uint32(j))
		}
		encoding.Signatures[i] = ir.SignatureSpec{Signature: signature.Metadata(), Parameters: parameters}
		jsonBound += uint64(len(parameters)) * 12
	}
	for i := range encoding.Scalars {
		spec, err := graph.CloneScalarSpec(ctx, ir.ScalarID(i+1), budget.remaining)
		if err != nil {
			return nil, err
		}
		if err := budget.charge(uint64(len(spec.Operands)) * 4); err != nil {
			return nil, err
		}
		jsonBound += uint64(len(spec.Operands)) * 12
		record := routineDefaultScalar{Scalar: spec.Scalar.Metadata(), Operands: spec.Operands}
		record.Scalar.Span = ir.Span{}
		if spec.Kind == ir.Const {
			size, err := model.ValueBytes(ctx, spec.Value, budget.remaining)
			if err != nil {
				return nil, err
			}
			if err := budget.charge(size); err != nil {
				return nil, err
			}
			var counter routineDefaultCounter
			if err := storage.EncodeValue(ctx, &counter, spec.Value, budget.remaining); err != nil {
				return nil, err
			}
			if err := budget.charge(counter.bytes); err != nil {
				return nil, err
			}
			buffer := bytes.NewBuffer(make([]byte, 0, int(counter.bytes)))
			if err := storage.EncodeValue(ctx, buffer, spec.Value, counter.bytes); err != nil {
				return nil, err
			}
			record.Constant = buffer.Bytes()
			jsonBound += counter.bytes * 2
		}
		encoding.Scalars[i] = record
	}
	if jsonBound > budget.remaining/3 {
		return nil, bindError("54000", "routine default encoding limit exceeded", 0)
	}
	if err := budget.charge(jsonBound * 3); err != nil {
		return nil, err
	}
	data, err := json.Marshal(encoding)
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > jsonBound {
		return nil, bindError("XX000", "routine default JSON exceeded admitted bound", 0)
	}
	return data, ctx.Err()
}
