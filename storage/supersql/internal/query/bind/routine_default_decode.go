package bind

import (
	"bytes"
	"encoding/json"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) routineDefault(definition *model.ExpressionDefinition, target model.TypeLayout, scope *scope, pos int) (expression, error) {
	if definition == nil || definition.SemanticVersion != routineDefaultVersion || definition.SQL != "" {
		return expression{}, missing("frozen routine default encoding", pos)
	}
	if uint64(len(definition.Dependencies)) > uint64(b.options.Limits.MaxOperands) {
		return expression{}, bindError("54000", "routine default dependency limit exceeded", pos)
	}
	if err := checkIndexJSON(b.ctx, definition.Encoding, b.options.Limits); err != nil {
		return expression{}, err
	}
	var encoding routineDefaultEncoding
	decoder := json.NewDecoder(bytes.NewReader(definition.Encoding))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&encoding); err != nil {
		return expression{}, bindError("22023", "invalid routine default encoding", pos)
	}
	if encoding.Format != "supersql.routine-default" || encoding.Version != routineDefaultVersion || encoding.Root == 0 || int(encoding.Root) > len(encoding.Scalars) {
		return expression{}, bindError("22023", "invalid routine default root", pos)
	}
	if encoding.CollationStrength > collationExplicit {
		return expression{}, bindError("22023", "invalid routine default collation strength", pos)
	}
	if encoding.Collation != 0 {
		if err := ValidateBuiltinCollation(encoding.Collation, pos); err != nil {
			return expression{}, err
		}
	}
	canonical, err := json.Marshal(encoding)
	if err != nil || !bytes.Equal(canonical, definition.Encoding) {
		return expression{}, bindError("22023", "noncanonical routine default encoding", pos)
	}
	if err := b.validateDefaultDependencies(definition.Dependencies, encoding.Identities, pos); err != nil {
		return expression{}, err
	}
	return b.importRoutineDefault(encoding, definition.Dependencies, target, scope, pos)
}

func (b *binder) importRoutineDefault(encoding routineDefaultEncoding, dependencies []model.Dependency, target model.TypeLayout, scope *scope, pos int) (expression, error) {
	limits := b.options.Limits
	if uint64(len(encoding.Layouts)+len(encoding.Signatures)+len(encoding.Scalars)) > uint64(limits.MaxNodes) {
		return expression{}, bindError("54000", "routine default node limit exceeded", pos)
	}
	budget := routineDefaultBudget{remaining: limits.MaxBytes}
	headers := uint64(len(encoding.Layouts))*(uint64(unsafe.Sizeof(model.TypeLayout{}))+4) + uint64(len(encoding.Signatures))*4 + uint64(len(encoding.Scalars))*4
	if err := budget.charge(headers); err != nil {
		return expression{}, err
	}
	layouts := make([]model.TypeLayout, len(encoding.Layouts))
	for i, record := range encoding.Layouts {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		if err := budget.charge(uint64(len(record.Children)) * uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
			return expression{}, err
		}
		children := make([]model.TypeLayout, len(record.Children))
		for j, id := range record.Children {
			if id == 0 || int(id) > i {
				return expression{}, bindError("22023", "invalid routine default layout reference", pos)
			}
			children[j] = layouts[id-1]
		}
		layouts[i] = model.TypeLayout{Type: record.Type, Children: children}
	}
	mappedLayouts := make([]ir.LayoutID, len(layouts))
	layoutID := func(id ir.LayoutID) (ir.LayoutID, error) {
		if id == 0 || int(id) > len(layouts) {
			return 0, bindError("22023", "invalid routine default type handle", pos)
		}
		if mappedLayouts[id-1] != 0 {
			return mappedLayouts[id-1], nil
		}
		result, err := b.layout(layouts[id-1])
		if err != nil {
			return 0, err
		}
		mappedLayouts[id-1] = result
		return result, nil
	}
	signatures := make([]ir.SignatureID, len(encoding.Signatures))
	for i, source := range encoding.Signatures {
		if err := b.validateRoutineDefaultSignature(source, dependencies); err != nil {
			return expression{}, err
		}
		result, err := layoutID(source.Result)
		if err != nil {
			return expression{}, err
		}
		if err := budget.charge(uint64(len(source.Parameters)) * 4); err != nil {
			return expression{}, err
		}
		parameters := make([]ir.LayoutID, len(source.Parameters))
		for j, id := range source.Parameters {
			parameters[j], err = layoutID(id)
			if err != nil {
				return expression{}, err
			}
		}
		source.Result, source.Parameters = result, parameters
		signatures[i], err = b.builder.AddSignature(b.ctx, source)
		if err != nil {
			return expression{}, err
		}
	}
	scalars := make([]ir.ScalarID, len(encoding.Scalars))
	for i, source := range encoding.Scalars {
		if source.Scalar.Scope != 1 || source.Scalar.Relation != 0 || source.Scalar.Slot != 0 || source.Scalar.Parameter != 0 {
			return expression{}, bindError("22023", "routine default references invocation state", pos)
		}
		if source.Scalar.Kind == ir.Param || source.Scalar.Kind == ir.Slot || source.Scalar.Kind == ir.Subquery {
			return expression{}, bindError("22023", "invalid routine default scalar kind", pos)
		}
		layout, err := layoutID(source.Scalar.Layout)
		if err != nil {
			return expression{}, err
		}
		if err := budget.charge(uint64(len(source.Operands)) * 4); err != nil {
			return expression{}, err
		}
		operands := make([]ir.ScalarID, len(source.Operands))
		for j, id := range source.Operands {
			if id == 0 || int(id) > i {
				return expression{}, bindError("22023", "invalid routine default operand", pos)
			}
			operands[j] = scalars[id-1]
		}
		spec := ir.ScalarSpec{Scalar: source.Scalar.Metadata(), Operands: operands}
		spec.Scope, spec.Layout, spec.Span = scope.id, layout, sourceSpan(pos)
		if spec.Signature != 0 {
			if int(spec.Signature) > len(signatures) {
				return expression{}, bindError("22023", "invalid routine default signature handle", pos)
			}
			spec.Signature = signatures[spec.Signature-1]
		}
		if spec.Kind == ir.Const {
			spec.Value, err = storage.DecodeValue(b.ctx, source.Constant, budget.remaining)
			if err != nil {
				return expression{}, err
			}
			size, err := model.ValueBytes(b.ctx, spec.Value, budget.remaining)
			if err != nil {
				return expression{}, err
			}
			if err := budget.charge(size); err != nil {
				return expression{}, err
			}
		} else if len(source.Constant) != 0 {
			return expression{}, bindError("22023", "unexpected routine default constant", pos)
		}
		scalars[i], err = b.builder.AddScalar(b.ctx, spec)
		if err != nil {
			return expression{}, err
		}
	}
	root := encoding.Scalars[encoding.Root-1].Scalar
	layout := layouts[root.Layout-1]
	comparison := layout
	comparison.Type.Collation = target.Type.Collation
	if !sameType(comparison, target) {
		return expression{}, bindError("42804", "routine default result type changed", pos)
	}
	result := expression{id: scalars[encoding.Root-1], typ: layout, nullable: root.Nullable, effects: root.Effects, position: pos}
	result.collation = collationState{id: encoding.Collation, strength: encoding.CollationStrength, position: pos}
	if layout.Type.Storage == model.TypeComposite {
		result.record = recordDeclared
	}
	return result, nil
}

func (b *binder) validateRoutineDefaultSignature(signature ir.SignatureSpec, dependencies []model.Dependency) error {
	function := signature.Function
	if signature.Operator != 0 {
		operator, ok := builtin.LookupOperator(signature.Operator)
		if !ok {
			return bindError("22023", "unknown routine default operator", 0)
		}
		function = operator.Function
	}
	if known, ok := b.lookupSignature(function); ok {
		if err := ValidateBuiltinFunction(function, 0); err != nil {
			return err
		}
		if signature.Strict != known.Strict || signature.Nullable != functionNullable(known) || signature.Effects != signatureEffects(known) {
			return bindError("22023", "routine default function traits changed", 0)
		}
		return nil
	}
	for _, dependency := range dependencies {
		published, err := b.catalog.Descriptor(b.ctx, dependency.Object)
		if err != nil {
			return err
		}
		if model.FunctionID(published.Descriptor.OID()) != function {
			continue
		}
		if published.Descriptor.Kind() != model.ObjectRoutine {
			return bindError("22023", "routine default function identity changed", 0)
		}
		spec, err := b.descriptorSpec(published.Descriptor)
		if err != nil {
			return err
		}
		volatility := ir.Immutable
		switch spec.Routine.Volatility {
		case "stable":
			volatility = ir.Stable
		case "volatile":
			volatility = ir.Volatile
		}
		if signature.Strict != spec.Routine.Strict || !signature.Nullable || signature.Effects != (ir.Effects{Volatility: volatility, MayError: true}) {
			return bindError("22023", "routine default routine traits changed", 0)
		}
		return nil
	}
	return bindError("22023", "undeclared routine default function", 0)
}
