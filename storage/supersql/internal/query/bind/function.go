package bind

import (
	"errors"
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func signatureEffects(sig builtin.Signature) ir.Effects {
	e := ir.Effects{MayError: builtin.ScalarMayError(sig.ID)}
	switch sig.Volatility {
	case 's':
		e.Volatility = ir.Stable
	case 'v':
		e.Volatility = ir.Volatile
	}
	return e
}
func (b *binder) signature(sig builtin.Signature, op model.OperatorID, args []model.TypeLayout, result model.TypeLayout, inputCollation model.CollationID, variadicArray bool) (ir.SignatureID, error) {
	parameters := make([]ir.LayoutID, len(args))
	for i, arg := range args {
		id, err := b.layout(arg)
		if err != nil {
			return 0, err
		}
		parameters[i] = id
	}
	layout, err := b.layout(result)
	if err != nil {
		return 0, err
	}
	spec := ir.Signature{Function: sig.ID, Operator: op, Result: layout, InputCollation: inputCollation, VariadicArray: variadicArray, Strict: sig.Strict, SetReturning: sig.ReturnsSet, Nullable: functionNullable(sig), Effects: signatureEffects(sig)}
	if op != 0 {
		spec.Function = 0
	}
	return b.builder.AddSignature(b.ctx, ir.SignatureSpec{Signature: spec, Parameters: parameters})
}
func (b *binder) invoke(s *scope, sig builtin.Signature, op model.OperatorID, args []expression, kind ir.ScalarKind, cast ir.CastContext, pos int) (expression, error) {
	var err error
	sig, err = b.specialize(sig, args, pos)
	if err != nil {
		return expression{}, err
	}
	result, err := b.functionResult(sig, args)
	if err != nil {
		return expression{}, err
	}
	return b.invokeResult(s, sig, op, args, kind, cast, pos, result)
}

func (b *binder) invokeResult(s *scope, sig builtin.Signature, op model.OperatorID, args []expression, kind ir.ScalarKind, cast ir.CastContext, pos int, result model.TypeLayout) (expression, error) {
	if err := ValidateBuiltinFunction(sig.ID, pos); err != nil {
		return expression{}, err
	}
	if result.Type.Storage == model.TypeAny {
		return expression{}, missing("physical layout for resolved function result", pos)
	}
	targets, err := b.functionArguments(sig, args, pos)
	if err != nil {
		return expression{}, err
	}
	return b.invokeArguments(s, sig, op, args, kind, cast, pos, result, targets[:len(args)], false)
}

func (b *binder) invokeArguments(s *scope, sig builtin.Signature, op model.OperatorID, args []expression, kind ir.ScalarKind, cast ir.CastContext, pos int, result model.TypeLayout, targets []model.TypeLayout, variadicArray bool) (expression, error) {
	types := make([]model.TypeLayout, len(args))
	nullable := functionNullable(sig)
	for i := range args {
		var target model.TypeLayout
		if sig.Variadic == 2276 && i >= int(sig.ArgumentCount)-1 {
			var err error
			target, err = b.variadicArgumentType(args[i], variadicArray)
			if err != nil {
				return expression{}, err
			}
		} else {
			target = targets[i]
		}
		if target.Type.Storage == model.TypeAny {
			return expression{}, missing("physical layout for resolved function argument", pos)
		}
		arg, err := b.coerce(s, args[i], target, ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		args[i] = arg
		types[i] = arg.typ
		nullable = nullable || arg.nullable
	}
	// A variadic "any" parameter (2276) is a non-collatable pseudo-type, so its
	// arguments attribute no collation to the result even when the actual value is
	// collatable. PostgreSQL derives the result from the remaining arguments; for
	// format(text, VARIADIC "any") that leaves the format string's collation.
	collationArgs := args
	if sig.Variadic == 2276 && len(args) >= int(sig.ArgumentCount) {
		collationArgs = args[:int(sig.ArgumentCount)-1]
	}
	state, err := b.commonCollation(collationArgs)
	if err != nil {
		return expression{}, err
	}
	result, resultState := b.resultCollation(result, state)
	id, err := b.signature(sig, op, types, result, state.id, variadicArray)
	if err != nil {
		return expression{}, err
	}
	payload := ir.Scalar{Kind: kind, Signature: id, Function: sig.ID, Operator: op, Strict: sig.Strict, CastContext: cast}
	if op != 0 {
		payload.Function = 0
	}
	return b.addScalar(s, expression{typ: result, collation: resultState, nullable: nullable, effects: signatureEffects(sig), position: pos}, payload, args)
}
func (b *binder) overloadScore(sig builtin.Signature, args []expression) int {
	if sig.Variadic == 2276 {
		return b.anyVariadicScore(sig, args)
	}
	if int(sig.ArgumentCount) != len(args) || len(args) > len(sig.Arguments) {
		return -1
	}
	_, _, compatibleErr := b.compatibleElement(sig, args, 0)
	var typeError *model.Error
	if errors.As(compatibleErr, &typeError) && typeError.Code == "42804" {
		return -1
	}
	if _, _, err := b.exactPolymorphicElement(sig, args, 0); err != nil {
		return -1
	}
	score := 0
	for i, arg := range args {
		target := sig.Arguments[i]
		source := b.argumentID(arg)
		if source == target {
			score += 10
			continue
		}
		if polymorphic(target) {
			if !b.polymorphicArgumentCompatible(target, source, arg) {
				return -1
			}
			continue
		}
		if source == 0 || source == 705 {
			if target == 25 {
				score++
			}
			continue
		}
		for depth := b.options.Limits.MaxDepth; depth > 0; depth-- {
			base, domain, err := b.domainBase(b.argumentType(source, arg))
			if err != nil {
				// Scoring cannot propagate errors; latch the failure so the
				// resolution aborts with the catalog error rather than a
				// misleading "operator does not exist".
				b.castErr = errors.Join(b.castErr, err)
				return -1
			}
			if !domain {
				break
			}
			source = base.Type.ID
		}
		if source == target {
			score += 10
			continue
		}
		if target == 2249 && b.argumentType(source, arg).Type.Storage == model.TypeComposite {
			continue
		}
		if !implicitCast(source, target) {
			return -1
		}
	}
	return score
}

func (b *binder) argumentID(arg expression) model.TypeID {
	if arg.typ.Type.ID != 0 {
		return arg.typ.Type.ID
	}
	if typ, ok := b.params[arg.parameter]; arg.parameter != 0 && ok {
		return typ.Type.ID
	}
	return arg.inferred
}
func (b *binder) function(n *zpgparse.FuncCall, s *scope) (expression, error) {
	pos := int(n.Location) + 1
	parts, err := names(n.Funcname)
	if err != nil {
		return expression{}, err
	}
	if len(parts) == 0 || len(parts) > 2 {
		return expression{}, bindError("42883", "function does not exist", pos)
	}
	name := parts[len(parts)-1]
	if n.AggWithinGroup {
		switch {
		case n.Over != nil:
			return expression{}, bindError("0A000", "ordered-set aggregates cannot be used as window functions", pos)
		case n.AggDistinct:
			return expression{}, bindError("42601", "DISTINCT is not allowed with WITHIN GROUP", pos)
		case n.AggStar:
			return expression{}, bindError("42601", "* is not allowed with WITHIN GROUP", pos)
		case n.FuncVariadic:
			return expression{}, bindError("42601", "VARIADIC is not allowed with WITHIN GROUP", pos)
		case len(n.AggOrder) == 0:
			return expression{}, bindError("42601", "WITHIN GROUP requires ORDER BY", pos)
		}
	}
	aggregateSyntax := n.AggStar || n.AggDistinct || n.AggFilter != nil || n.AggWithinGroup
	for sig := range b.signaturesNamed(name) {
		aggregateSyntax = aggregateSyntax || sig.Kind == 'a'
	}
	if n.Over != nil {
		aggregateSyntax = false
	}
	previousWindow := s.inWindow
	if n.Over != nil && previousWindow {
		return expression{}, bindError("42P20", "window function calls cannot be nested", pos)
	}
	if n.Over != nil {
		s.inWindow = true
	}
	previousAggregate, previousAllowWindow := s.inAggregate, s.allowWindow
	if aggregateSyntax && previousAggregate {
		return expression{}, bindError("42803", "aggregate calls cannot be nested", pos)
	}
	var call callArguments
	directCount := 0
	if n.AggWithinGroup {
		call, directCount, err = b.orderedSetArguments(n, s, pos)
	} else {
		if aggregateSyntax {
			s.inAggregate = true
			s.allowWindow = false
		}
		call, err = b.callArguments(n.Args, s, pos)
	}
	s.inAggregate = previousAggregate
	s.inWindow = previousWindow
	s.allowWindow = previousAllowWindow
	if err != nil {
		return expression{}, err
	}
	args := call.values
	search := b.functionPath(parts)
	for _, schema := range search {
		if schema != "pg_catalog" && schema != "information_schema" {
			// WITHIN GROUP selects the ordered-set or hypothetical-set form. A
			// declared aggregate may occupy that form, so the user catalog is
			// consulted here too; a plain routine cannot, so userFunction is
			// skipped rather than letting it match an ordinary function.
			if n.AggWithinGroup {
				e, found, err := b.userAggregate(schema, name, n, call, s, pos, directCount)
				if err != nil || found {
					return e, err
				}
				// Keep searching: the pinned ordered-set aggregates live under
				// pg_catalog and must still be reachable.
				continue
			}
			e, found, err := b.userFunction(schema, name, call, s, pos, n.FuncVariadic)
			if err != nil || found {
				return e, err
			}
			e, found, err = b.userAggregate(schema, name, n, call, s, pos, directCount)
			if err != nil || found {
				return e, err
			}
			e, found, err = b.declaredTypeConstructor(schema, name, call, s, pos, n.FuncVariadic)
			if err != nil || found {
				return e, err
			}
			continue
		}
		resolved, found, err := b.builtinFunction(name, call, s, pos, n.FuncVariadic)
		if err != nil {
			return expression{}, err
		}
		if !found {
			continue
		}
		_, informationFunction := b.options.CatalogRelations.InformationSchemaFunction(resolved.signature.ID)
		if informationFunction != (schema == "information_schema") {
			continue
		}
		selected := resolved.signature
		args = resolved.arguments
		metadata, orderedSet := builtin.LookupAggregateMetadata(selected.ID)
		if selected.Variadic != 0 && selected.Kind != 'f' && !orderedSet {
			return expression{}, missing("variadic non-scalar function", pos)
		}
		if selected.ReturnsSet {
			if err := b.checkTargetSRF(s, pos); err != nil {
				return expression{}, err
			}
			if n.AggStar || n.AggDistinct || n.AggFilter != nil || len(n.AggOrder) > 0 || n.AggWithinGroup || n.Over != nil {
				return expression{}, bindError("42809", "aggregate or window syntax used with set-returning function", pos)
			}
			if result, known, resultErr := b.setRowResult(selected.ID, args, pos); resultErr != nil {
				return expression{}, resultErr
			} else if known {
				targets, targetErr := b.functionArguments(selected, args, pos)
				if targetErr != nil {
					return expression{}, targetErr
				}
				return b.invokeArguments(s, selected, 0, args, ir.Call, 0, pos, result, targets[:len(args)], false)
			}
			if selected.Variadic == 2276 {
				return b.invokeVariadic(s, selected, args, n.FuncVariadic, pos)
			}
			if selected.Variadic != 0 {
				return b.invokeTypedVariadic(s, selected, args, resolved.expandVariadic, pos)
			}
			return b.invoke(s, selected, 0, args, ir.Call, 0, pos)
		}
		if n.Over != nil || selected.Kind == 'w' {
			return b.windowFunction(n, s, selected, args)
		}
		if selected.Kind == 'a' {
			if orderedSet && metadata.Kind == builtin.HypotheticalSetAggregate && directCount != len(n.AggOrder) {
				return expression{}, bindError("42883", "hypothetical-set direct and ORDER BY argument counts must match", pos)
			}
			return b.aggregateFunction(n, s, selected, args, directCount)
		}
		if n.AggStar || n.AggDistinct || n.AggFilter != nil || len(n.AggOrder) > 0 || n.AggWithinGroup {
			return expression{}, bindError("42809", "aggregate syntax used with non-aggregate function", pos)
		}
		if selected.Variadic == 2276 {
			return b.invokeVariadic(s, selected, args, n.FuncVariadic, pos)
		}
		if selected.Variadic != 0 {
			return b.invokeTypedVariadic(s, selected, args, resolved.expandVariadic, pos)
		}
		return b.invoke(s, selected, 0, args, ir.Call, 0, pos)
	}
	return expression{}, bindError("42883", "function does not exist", pos)
}
func (b *binder) functionPath(parts []string) []string {
	if len(parts) == 2 {
		return []string{parts[0]}
	}
	for _, name := range b.options.SearchPath {
		if name == "pg_catalog" {
			return b.options.SearchPath
		}
	}
	return append([]string{"pg_catalog"}, b.options.SearchPath...)
}

type userRowSource struct {
	function      model.FunctionID
	arguments     []expression
	result        model.TypeLayout
	outputs       []string
	set           bool
	strict        bool
	variadicArray bool
	collation     model.CollationID
	effects       ir.Effects
}

func (b *binder) userFunction(schema, name string, call callArguments, s *scope, pos int, variadicArray bool) (expression, bool, error) {
	return b.userRoutine(schema, name, call, s, pos, variadicArray, false, false, nil)
}
func (b *binder) userRowFunction(schema, name string, call callArguments, s *scope, pos int, variadicArray bool) (userRowSource, bool, error) {
	var source userRowSource
	_, found, err := b.userRoutine(schema, name, call, s, pos, variadicArray, false, true, &source)
	return source, found, err
}

func (b *binder) userRoutine(schema, name string, call callArguments, s *scope, pos int, variadicArray, procedure, setContext bool, rowSource *userRowSource) (expression, bool, error) {
	args := call.values
	if b.catalog == nil {
		return expression{}, false, nil
	}
	namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
	if isMissingObject(err) {
		return expression{}, false, nil
	}
	if err != nil {
		return expression{}, false, err
	}
	var selected *model.Descriptor
	var metadata *model.RoutineMetadata
	polymorphic := &userPolymorphic{inferred: map[model.TypeID]model.TypeLayout{}}
	// Candidates are ranked like user operators: each exactly typed argument
	// scores, an implicitly coercible one is admitted without scoring, and the
	// unique best score wins. Equal best scores are ambiguous.
	bestScore := -1
	ambiguous := false
	variadicCandidate := false
	err = b.catalog.ScanNamespace(b.ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(p catalog.PublishedDescriptor) error {
		if p.Descriptor.Name().Name != name {
			return nil
		}
		spec, err := b.descriptorSpec(p.Descriptor)
		if err != nil {
			return err
		}
		if spec.Routine == nil {
			return nil
		}
		if spec.Routine.Procedure != procedure {
			return nil
		}
		parameters := routineInputs(spec.Routine.Parameters)
		if procedure {
			parameters = spec.Routine.Parameters
		}
		variadic := len(parameters) != 0 && parameters[len(parameters)-1].Mode == "variadic"
		if variadic {
			if call.names != nil {
				return nil
			}
			prefix := len(parameters) - 1
			if variadicArray && len(args) != len(parameters) || !variadicArray && len(args) < prefix {
				return nil
			}
			for i, arg := range args {
				parameter := parameters[min(i, prefix)]
				if procedure && !routineParameterIsInput(parameter) {
					continue
				}
				target := parameter.Layout
				if !variadicArray && i >= prefix {
					if len(target.Children) != 1 {
						return nil
					}
					target = target.Children[0]
				}
				argument := arg.typ
				argument.Type.Collation = target.Type.Collation
				argument.Type.Modifier = target.Type.Modifier
				if arg.typ.Type.ID != 0 && !sameType(argument, target) {
					return nil
				}
			}
			// The call shape matches a variadic user routine, but there is no
			// executable binding for one yet. Record the candidate so the caller
			// reports a declared gap instead of "function does not exist".
			variadicCandidate = true
			return nil
		} else {
			if variadicArray {
				return nil
			}
			order, matches := call.routineOrder(parameters)
			if !matches {
				return nil
			}
			candidate := &userPolymorphic{inferred: map[model.TypeID]model.TypeLayout{}}
			score := 0
			for i, source := range order[:len(parameters)] {
				if source < 0 || procedure && !routineParameterIsInput(parameters[i]) {
					continue
				}
				arg := args[source]
				argument := arg.typ
				argument.Type.Collation = parameters[i].Layout.Type.Collation
				argument.Type.Modifier = parameters[i].Layout.Type.Modifier
				if arg.typ.Type.ID == 0 {
					continue
				}
				switch {
				case model.IsPseudoTypeID(parameters[i].Layout.Type.ID):
					if !b.matchPolymorphicParameter(parameters[i].Layout.Type.ID, arg, candidate) {
						return nil
					}
				case sameType(argument, parameters[i].Layout):
					score += 10
				case !b.implicitCastWithUser(arg.typ.Type.ID, parameters[i].Layout.Type.ID):
					return nil
				}
			}
			switch {
			case score > bestScore:
				bestScore, ambiguous = score, false
			case score == bestScore:
				ambiguous = true
				return nil
			default:
				return nil
			}
			polymorphic = candidate
		}
		selected = p.Descriptor
		metadata = spec.Routine
		return nil
	})
	if err != nil {
		return expression{}, false, err
	}
	if ambiguous {
		return expression{}, false, bindError("42725", "function call is ambiguous", pos)
	}
	if selected == nil {
		// A user routine with a VARIADIC parameter is bound as a variadic
		// candidate but has no executable binding yet. This is a declared gap
		// (UNMAPPED.json conditional_guards: variadic_user_routine), so report it
		// rather than silently reporting the function as nonexistent.
		if variadicCandidate {
			return expression{}, false, missing("variadic user routine", pos)
		}
		return expression{}, false, nil
	}
	b.dependency(namespace.Descriptor, 0)
	b.dependency(selected, 0)
	if metadata.SecurityDefiner {
		return expression{}, false, unsupported(Authorization, pos)
	}
	if metadata.Language != "sql" && metadata.Language != "plpgsql" {
		return expression{}, false, unsupported(ExternalCode, pos)
	}
	for _, dep := range metadata.Body.Dependencies {
		published, err := b.catalog.Descriptor(b.ctx, dep.Object)
		if err != nil {
			return expression{}, false, err
		}
		if published.Descriptor.Version() != dep.Version {
			return expression{}, false, bindError("0A000", "routine dependency version changed", pos)
		}
		b.dependency(published.Descriptor, dep.Column)
	}
	if metadata.Body.SemanticVersion != 1 || len(metadata.Body.Encoding) == 0 {
		return expression{}, false, bindError("0A000", "routine semantic definition encoding is unavailable", pos)
	}
	if metadata.ReturnsSet && !setContext && !procedure {
		if err := b.checkTargetSRF(s, pos); err != nil {
			return expression{}, false, err
		}
	}
	// A non-set routine in FROM evaluates once and yields its single result
	// row; it reaches the rowSource assignment below like a set routine.
	inputs := routineInputs(metadata.Parameters)
	for i := range inputs {
		if containsPseudoMarker(inputs[i].Layout) {
			resolved, err := b.resolvePolymorphicLayout(inputs[i].Layout, polymorphic, pos)
			if err != nil {
				return expression{}, false, err
			}
			inputs[i].Layout = resolved
		}
	}
	if procedure {
		args, err = b.completeProcedureArguments(metadata.Parameters, call, s, pos, variadicArray)
	} else {
		args, err = b.completeUserRoutineArguments(inputs, call, s, pos, variadicArray)
	}
	if err != nil {
		return expression{}, false, err
	}
	function := model.FunctionID(selected.OID())
	if _, collision := b.lookupSignature(function); collision {
		return expression{}, false, bindError("XX000", "user routine OID collides with builtin function", pos)
	}
	effects := ir.Effects{MayError: true}
	switch metadata.Volatility {
	case "immutable":
	case "stable":
		effects.Volatility = ir.Stable
	case "volatile":
		effects.Volatility = ir.Volatile
	default:
		return expression{}, false, bindError("XX000", "invalid routine volatility metadata", pos)
	}
	parameters := make([]ir.LayoutID, len(args))
	for i := range args {
		arg, err := b.coerce(s, args[i], inputs[i].Layout, ir.ImplicitCast)
		if err != nil {
			return expression{}, false, err
		}
		args[i] = arg
		parameters[i], err = b.layout(arg.typ)
		if err != nil {
			return expression{}, false, err
		}
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, false, err
	}
	resultLayout := metadata.Result
	if containsPseudoMarker(resultLayout) {
		resolved, err := b.resolvePolymorphicLayout(resultLayout, polymorphic, pos)
		if err != nil {
			return expression{}, false, err
		}
		resultLayout = resolved
	}
	resultType, resultState := b.resultCollation(resultLayout, state)
	if resultType.Type.Storage == model.TypeAny && model.IsPseudoTypeID(resultType.Type.ID) {
		// The declared result is a pseudo-type marker (void, trigger, ...); the
		// value a call produces uses the type's primitive storage convention.
		physical, ok := builtin.BuiltinTypeLayout(resultType.Type.ID, resultType.Type.Modifier)
		if !ok {
			return expression{}, false, missing("physical layout for resolved type", pos)
		}
		resultType = physical
	}
	if rowSource != nil {
		// Reloaded metadata strips FieldNames presentation, so the row shape
		// an OUT/TABLE declaration gives a record result must ride the call's
		// parameter list instead; PostgreSQL names anonymous output columns
		// column1, column2, ... in parameter order.
		outputs := routineOutputs(metadata.Parameters)
		names := make([]string, len(outputs))
		for i, output := range outputs {
			names[i] = output.Name
			if names[i] == "" {
				names[i] = fmt.Sprintf("column%d", i+1)
			}
		}
		*rowSource = userRowSource{function: function, arguments: args, result: resultType, outputs: names, set: metadata.ReturnsSet, strict: metadata.Strict, variadicArray: variadicArray, collation: state.id, effects: effects}
		return expression{}, true, nil
	}
	result, err := b.layout(resultType)
	if err != nil {
		return expression{}, false, err
	}
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: result, InputCollation: state.id, Nullable: true, SetReturning: metadata.ReturnsSet, VariadicArray: variadicArray, Strict: metadata.Strict, Effects: effects}, Parameters: parameters})
	if err != nil {
		return expression{}, false, err
	}
	bound := expression{typ: resultType, collation: resultState, nullable: true, effects: effects, position: pos}
	if procedure {
		outputs := routineOutputs(metadata.Parameters)
		for i := range outputs {
			if containsPseudoMarker(outputs[i].Layout) {
				resolved, err := b.resolvePolymorphicLayout(outputs[i].Layout, polymorphic, pos)
				if err != nil {
					return expression{}, false, err
				}
				outputs[i].Layout = resolved
			}
		}
		bound.routineOutputs = outputs
	}
	expr, err := b.addScalar(s, bound, ir.Scalar{Kind: ir.Call, Function: function, Signature: signature, Strict: metadata.Strict}, args)
	return expr, true, err
}
func (b *binder) completeUserRoutineArguments(parameters []model.RoutineParameter, call callArguments, scope *scope, pos int, variadicArray bool) ([]expression, error) {
	if len(parameters) == 0 || parameters[len(parameters)-1].Mode != "variadic" {
		return b.completeRoutineArguments(parameters, call, scope, pos)
	}
	if call.names != nil {
		return nil, bindError("42883", "named notation is not allowed for variadic routine expansion", pos)
	}
	prefix := len(parameters) - 1
	if variadicArray {
		if len(call.values) != len(parameters) {
			return nil, bindError("42883", "function does not exist", pos)
		}
		return call.values, nil
	}
	if len(call.values) < prefix {
		return nil, bindError("42883", "function does not exist", pos)
	}
	array := parameters[prefix].Layout
	if array.Type.Storage != model.TypeArray || len(array.Children) != 1 {
		return nil, model.ErrInvalidDescriptor
	}
	values := slices.Clone(call.values)
	effects := ir.Effects{}
	var err error
	for i := prefix; i < len(values); i++ {
		values[i], err = b.coerce(scope, values[i], array.Children[0], ir.ImplicitCast)
		if err != nil {
			return nil, err
		}
	}
	for _, value := range values[prefix:] {
		effects = effects.Union(value.effects)
	}
	position := pos
	if len(values) > prefix {
		position = values[prefix].position
	}
	packed, err := b.addScalar(scope, expression{typ: array, nullable: false, effects: effects, position: position}, ir.Scalar{Kind: ir.Array}, values[prefix:])
	if err != nil {
		return nil, err
	}
	values = append(values[:prefix], packed)
	return values, nil
}
func (b *binder) completeProcedureArguments(parameters []model.RoutineParameter, call callArguments, scope *scope, pos int, variadicArray bool) ([]expression, error) {
	if len(parameters) == 0 || parameters[len(parameters)-1].Mode != "variadic" {
		order, ok := call.routineOrder(parameters)
		if !ok {
			return nil, bindError("42883", "procedure does not exist", pos)
		}
		values := make([]expression, 0, len(routineInputs(parameters)))
		for i, parameter := range parameters {
			if !routineParameterIsInput(parameter) {
				continue
			}
			source := order[i]
			if source >= 0 {
				values = append(values, call.values[source])
				continue
			}
			value, err := b.routineDefault(parameter.Default, parameter.Layout, scope, pos)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	if call.names != nil {
		return nil, bindError("42883", "named notation is not allowed for variadic procedure expansion", pos)
	}
	prefix := len(parameters) - 1
	if variadicArray && len(call.values) != len(parameters) || !variadicArray && len(call.values) < prefix {
		return nil, bindError("42883", "procedure does not exist", pos)
	}
	values := make([]expression, 0, len(routineInputs(parameters)))
	for i, parameter := range parameters[:prefix] {
		if routineParameterIsInput(parameter) {
			values = append(values, call.values[i])
		}
	}
	array := parameters[prefix].Layout
	if variadicArray {
		return append(values, call.values[prefix]), nil
	}
	if array.Type.Storage != model.TypeArray || len(array.Children) != 1 {
		return nil, model.ErrInvalidDescriptor
	}
	elements := slices.Clone(call.values[prefix:])
	effects := ir.Effects{}
	for i := range elements {
		var err error
		elements[i], err = b.coerce(scope, elements[i], array.Children[0], ir.ImplicitCast)
		if err != nil {
			return nil, err
		}
		effects = effects.Union(elements[i].effects)
	}
	position := pos
	if len(elements) != 0 {
		position = elements[0].position
	}
	packed, err := b.addScalar(scope, expression{typ: array, nullable: false, effects: effects, position: position}, ir.Scalar{Kind: ir.Array}, elements)
	if err != nil {
		return nil, err
	}
	return append(values, packed), nil
}

func (b *binder) aggregateFunction(n *zpgparse.FuncCall, s *scope, sig builtin.Signature, args []expression, directCount int) (expression, error) {
	pos := int(n.Location) + 1
	if !s.allowAggregate || s.inAggregate {
		return expression{}, bindError("42803", "aggregate function is not allowed in this scope", pos)

	}
	metadata, orderedSet := builtin.LookupAggregateMetadata(sig.ID)
	if n.AggWithinGroup != orderedSet {
		if orderedSet {
			return expression{}, bindError("42809", "WITHIN GROUP is required for ordered-set aggregate", pos)
		}
		return expression{}, bindError("42809", "WITHIN GROUP is only valid for ordered-set aggregates", pos)
	}
	if orderedSet {
		if directCount+len(n.AggOrder) != len(args) {
			return expression{}, bindError("42883", "ordered-set aggregate argument count does not match ORDER BY", pos)
		}
		if metadata.Kind == builtin.OrderedSetAggregate && directCount != int(metadata.DirectArguments) {
			return expression{}, bindError("42883", "ordered-set aggregate direct argument count does not match", pos)
		}
		if metadata.Kind == builtin.HypotheticalSetAggregate {
			if directCount == 0 || directCount != len(n.AggOrder) {
				return expression{}, bindError("42883", "hypothetical-set direct and ORDER BY argument counts must match", pos)
			}
			for i := range directCount {
				target, err := b.commonType([]expression{args[i], args[directCount+i]})
				if err != nil {
					return expression{}, err
				}
				args[i], err = b.coerce(s, args[i], target, ir.ImplicitCast)
				if err != nil {
					return expression{}, err
				}
				args[directCount+i], err = b.coerce(s, args[directCount+i], target, ir.ImplicitCast)
				if err != nil {
					return expression{}, err
				}
			}
		}
	}
	var result model.TypeLayout
	var err error
	if orderedSet && metadata.Kind == builtin.HypotheticalSetAggregate {
		result = b.primitive(sig.Result)
	} else {
		result, err = b.functionResult(sig, args)
		if err != nil {
			return expression{}, err
		}
		targets, err := b.functionArguments(sig, args, pos)
		if err != nil {
			return expression{}, err
		}
		for i := range args {
			args[i], err = b.coerce(s, args[i], targets[i], ir.ImplicitCast)
			if err != nil {
				return expression{}, err
			}
		}
	}
	if result.Type.Storage == model.TypeAny {
		return expression{}, missing("aggregate result layout", pos)
	}
	types := make([]model.TypeLayout, len(args))
	ids := make([]ir.ScalarID, len(args))
	effects := signatureEffects(sig)
	for i, arg := range args {
		types[i] = arg.typ
		ids[i] = arg.id
		effects = effects.Union(arg.effects)
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, err
	}
	result, resultState := b.resultCollation(result, state)
	if n.AggDistinct {
		for _, arg := range args {
			if err := b.requireKeyCollation(arg); err != nil {
				return expression{}, err
			}
		}
	}
	signature, err := b.signature(sig, 0, types, result, state.id, false)
	if err != nil {
		return expression{}, err
	}
	nullable := functionNullable(sig)
	slot, err := b.slot(s, result, nullable, 0, 0)
	if err != nil {
		return expression{}, err
	}
	call := ir.AggregateSpec{
		AggregateCall: ir.AggregateCall{Signature: signature, Function: sig.ID, Result: slot, Distinct: n.AggDistinct, Effects: effects},
		Direct:        ids[:directCount],
		Args:          ids[directCount:],
	}
	previousAggregate, previousWindow, previousInput := s.allowAggregate, s.allowWindow, s.inAggregate
	s.allowAggregate, s.allowWindow, s.inAggregate = false, false, true
	defer func() {
		s.allowAggregate, s.allowWindow, s.inAggregate = previousAggregate, previousWindow, previousInput
	}()
	if n.AggFilter != nil {
		filter, err := b.scalar(n.AggFilter, s)
		if err != nil {
			return expression{}, err
		}
		filter, err = b.coerce(s, filter, b.primitive(16), ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		call.Filter = filter.id
		call.Effects = call.Effects.Union(filter.effects)
	}
	var order []ir.OrderKey
	var orderEffects ir.Effects
	if orderedSet {
		order, orderEffects, err = b.orderedSetOrder(n, args[directCount:])
	} else {
		order, orderEffects, err = b.aggregateOrder(n, s, args)
	}
	if err != nil {
		return expression{}, err
	}
	call.Order = order
	call.Effects = call.Effects.Union(orderEffects)
	c := column{ResultColumn: ResultColumn{Name: sig.Name, Layout: result, Nullable: nullable, Slot: slot}}
	s.aggregates = append(s.aggregates, call)
	s.aggregateColumns = append(s.aggregateColumns, c)
	s.aggregateSeen = true
	expr, err := b.reference(s, c, pos)
	expr.collation = resultState
	return expr, err
}

func functionNullable(sig builtin.Signature) bool {
	switch sig.ID {
	case 2147, 2803, 2818, 3100, 3101, 3102, 3103, 3104, 3986, 3988, 3990, 3992:
		return false
	}
	return true
}

type resolvedBuiltinCall struct {
	signature      builtin.Signature
	arguments      []expression
	expandVariadic bool
}

func (b *binder) builtinFunction(name string, call callArguments, s *scope, pos int, variadicArray bool) (resolvedBuiltinCall, bool, error) {
	selected, found, err := b.builtinCandidate(name, call, pos, variadicArray)
	if err != nil || !found {
		return resolvedBuiltinCall{}, found, err
	}
	expandVariadic := selected.Variadic != 0 && !variadicArray
	selected, found = b.lookupSignature(selected.ID)
	if !found {
		return resolvedBuiltinCall{}, false, bindError("XX000", "selected function signature disappeared", pos)
	}
	if variadicArray && call.names != nil && len(call.values) != 0 {
		parameters, _ := builtin.LookupFunctionParameters(selected.ID)
		order, matches := call.order(int(selected.ArgumentCount), parameters.ArgumentName, func(i int) bool { _, ok := parameters.DefaultSQL(i); return ok })
		last := len(call.values) - 1
		if !matches || order[int(selected.ArgumentCount)-1] != int16(last) {
			return resolvedBuiltinCall{}, true, bindError("42883", "function does not exist", pos)
		}
	}
	if err := ValidateBuiltinFunction(selected.ID, pos); err != nil {
		return resolvedBuiltinCall{}, true, err
	}
	args, err := b.completeBuiltinArguments(selected, call, s, pos)
	if err != nil {
		return resolvedBuiltinCall{}, true, err
	}
	if selected.Variadic != 0 {
		if selected.Variadic != 2276 && polymorphic(selected.Variadic) {
			return resolvedBuiltinCall{}, true, missing("polymorphic variadic element binding", pos)
		}
		return resolvedBuiltinCall{signature: selected, arguments: args, expandVariadic: expandVariadic}, true, nil
	}
	selected, err = b.specialize(selected, args, pos)
	return resolvedBuiltinCall{signature: selected, arguments: args}, true, err
}
