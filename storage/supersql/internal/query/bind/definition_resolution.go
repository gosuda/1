package bind

import (
	"errors"
	"slices"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// errDefinitionScanComplete stops a catalog scan once the wanted routine is
// found; callers treat it as success.
var errDefinitionScanComplete = errors.New("definition scan complete")

// resolveDefinitionFunctionDescriptor resolves a stored callable identity to its
// descriptor using only the borrowed CatalogSnapshot interface, which exposes
// descriptor lookup by ObjectID but not by OID. The OID is the executable
// identity of a routine descriptor, so the routine namespace is scanned once per
// identity and the result cached for the binding pass. Callers repeat this per
// candidate and per cast, so the cache is what keeps it off the hot path.
func resolveDefinitionFunctionDescriptor(b *binder, id model.FunctionID) (catalog.PublishedDescriptor, error) {
	if b == nil || b.catalog == nil || id == 0 {
		return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
	}
	if b.definitionFunctions == nil {
		b.definitionFunctions = make(map[model.FunctionID]catalog.PublishedDescriptor)
	}
	if cached, ok := b.definitionFunctions[id]; ok {
		return cached, nil
	}
	var found catalog.PublishedDescriptor
	err := b.catalog.ScanNamespace(b.ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		return b.catalog.ScanNamespace(b.ctx, schema.Descriptor.ID(), model.ObjectRoutine, func(published catalog.PublishedDescriptor) error {
			if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
				return nil
			}
			if model.FunctionID(published.Descriptor.OID()) != id {
				return nil
			}
			found = published
			return errDefinitionScanComplete
		})
	})
	if err != nil && !errors.Is(err, errDefinitionScanComplete) {
		return catalog.PublishedDescriptor{}, err
	}
	if found.Descriptor == nil {
		return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
	}
	b.definitionFunctions[id] = found
	return found, nil
}

// userCastResolution is the cached outcome of one user-defined cast probe.
type userCastResolution struct {
	metadata model.CastMetadata
	found    bool
	err      error
}

// castLookup resolves the catalog object for a user-defined cast. PostgreSQL
// does not schema-qualify casts: they live under pg_catalog keyed by the
// source/target type-ID pair, so this is a single point lookup. A hit records
// the real descriptor and schema dependencies on the binding.
func (b *binder) castLookup(source, target model.TypeID) userCastResolution {
	if b.catalog == nil || source == 0 || target == 0 {
		return userCastResolution{}
	}
	if b.casts == nil {
		b.casts = make(map[[2]model.TypeID]userCastResolution)
	}
	key := [2]model.TypeID{source, target}
	if cached, ok := b.casts[key]; ok {
		return cached
	}
	entry := b.probeCast(source, target)
	b.casts[key] = entry
	return entry
}

func (b *binder) probeCast(source, target model.TypeID) userCastResolution {
	schema, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: "pg_catalog"})
	if err != nil {
		if isMissingObject(err) {
			return userCastResolution{}
		}
		return userCastResolution{err: err}
	}
	name := strconv.FormatUint(uint64(source), 10) + ":" + strconv.FormatUint(uint64(target), 10)
	published, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Parent: schema.Descriptor.ID(), Kind: model.ObjectCast, Name: name})
	if err != nil {
		if isMissingObject(err) {
			return userCastResolution{}
		}
		return userCastResolution{err: err}
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return userCastResolution{}
	}
	spec, err := b.descriptorSpec(published.Descriptor)
	if err != nil {
		return userCastResolution{err: err}
	}
	if spec.Cast == nil || spec.Cast.Source.Type.ID != source || spec.Cast.Target.Type.ID != target {
		return userCastResolution{err: model.ErrInvalidDescriptor}
	}
	b.dependency(schema.Descriptor, 0)
	b.dependency(published.Descriptor, 0)
	return userCastResolution{metadata: *spec.Cast, found: true}
}

// userImplicitCast reports whether a user-defined cast admits implicit coercion.
// Builtin admissibility is evaluated first by the callers, so this only widens
// the set of coercions the catalog can justify.
func (b *binder) userImplicitCast(source, target model.TypeID) bool {
	cast := b.castLookup(source, target)
	return cast.err == nil && cast.found && cast.metadata.Context == 'i' &&
		(cast.metadata.Method == 'f' || cast.metadata.Method == 'b')
}

// implicitCastWithUser extends the package-level implicitCast with
// user-defined casts for the binder's pinned catalog. It preserves builtin
// precedence and array element recursion.
func (b *binder) implicitCastWithUser(from, to model.TypeID) bool {
	if from == to {
		return true
	}
	if implicitCast(from, to) {
		return true
	}
	if b.userImplicitCast(from, to) {
		return true
	}
	if to == 22 || to == 30 {
		return false
	}
	source, sourceKnown := builtin.LookupType(from)
	target, targetKnown := builtin.LookupType(to)
	return sourceKnown && targetKnown && source.IsArray() && target.IsArray() &&
		b.implicitCastWithUser(source.Element, target.Element)
}

// coerceUserBase lowers a user-defined cast to executable IR. Method 'f'
// invokes the recorded routine; 'b' is a physical relabel; 'i' re-enters I/O
// conversion, which is PostgreSQL's WITH INOUT semantics.
func (b *binder) coerceUserBase(s *scope, e expression, target model.TypeLayout, cast model.CastMetadata, context ir.CastContext) (expression, error) {
	switch cast.Method {
	case 'b':
		return b.addScalar(s, expression{typ: target, nullable: e.nullable, position: e.position, collation: b.expressionCollation(e)}, ir.Scalar{Kind: ir.Relabel, CastContext: context}, []expression{e})
	case 'i':
		return b.coerceViaIO(s, e, target)
	case 'f':
		return b.invokeUserCastFunction(s, e, target, cast.Function, context)
	default:
		return e, missing("cast method", e.position)
	}
}

// invokeUserCastFunction calls the SQL/PLpgSQL routine recorded by a cast. The
// routine is resolved from the pinned snapshot by its catalog OID, so the cast
// executes through the same procedural runtime as any other user routine.
func (b *binder) invokeUserCastFunction(s *scope, e expression, target model.TypeLayout, function model.FunctionID, context ir.CastContext) (expression, error) {
	if b.catalog == nil {
		return e, missing("cast function catalog", e.position)
	}
	published, err := resolveDefinitionFunctionDescriptor(b, function)
	if err != nil {
		if isMissingObject(err) {
			return e, bindError("42883", "cast function does not exist", e.position)
		}
		return e, err
	}
	descriptor := published.Descriptor
	if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != model.ObjectRoutine {
		return e, bindError("42883", "cast function does not exist", e.position)
	}
	spec, err := b.descriptorSpec(descriptor)
	if err != nil {
		return e, err
	}
	if spec.Routine == nil {
		return e, bindError("42883", "cast function is not a routine", e.position)
	}
	b.dependency(descriptor, 0)
	inputs := routineInputs(spec.Routine.Parameters)
	if len(inputs) != 1 {
		return e, bindError("42P17", "cast function must take exactly one argument", e.position)
	}
	if spec.Routine.ReturnsSet || spec.Routine.Procedure {
		return e, bindError("42P17", "cast function must be a scalar function", e.position)
	}
	argument, err := b.coerce(s, e, inputs[0].Layout, ir.ImplicitCast)
	if err != nil {
		return e, err
	}
	result := spec.Routine.Result
	result.Type.Collation = target.Type.Collation
	return b.invokeUserRoutine(s, descriptor, spec.Routine, []expression{argument}, result, context, e.position)
}

// invokeUserRoutine emits the IR call for a pinned user routine. The routine
// identity is its catalog OID, matching query-time resolution, so the cast
// function executes through the procedural resolver rather than a second path.
func (b *binder) invokeUserRoutine(s *scope, descriptor *model.Descriptor, metadata *model.RoutineMetadata, args []expression, result model.TypeLayout, context ir.CastContext, pos int) (expression, error) {
	function := model.FunctionID(descriptor.OID())
	if _, collision := b.lookupSignature(function); collision {
		return expression{}, bindError("XX000", "cast function OID collides with builtin function", pos)
	}
	effects := ir.Effects{MayError: true}
	switch metadata.Volatility {
	case "immutable":
	case "stable":
		effects.Volatility = ir.Stable
	case "volatile":
		effects.Volatility = ir.Volatile
	default:
		return expression{}, bindError("XX000", "invalid routine volatility metadata", pos)
	}
	parameters := make([]ir.LayoutID, len(args))
	for i := range args {
		layout, err := b.layout(args[i].typ)
		if err != nil {
			return expression{}, err
		}
		parameters[i] = layout
	}
	resultID, err := b.layout(result)
	if err != nil {
		return expression{}, err
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, err
	}
	resultType, resultState := b.resultCollation(result, state)
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{Signature: ir.Signature{Function: function, Result: resultID, InputCollation: state.id, Nullable: true, Strict: metadata.Strict, Effects: effects}, Parameters: parameters})
	if err != nil {
		return expression{}, err
	}
	nullable := metadata.Strict
	if !nullable {
		for _, arg := range args {
			if arg.nullable {
				nullable = true
				break
			}
		}
	}
	bound := expression{typ: resultType, collation: resultState, nullable: nullable, effects: effects, position: pos}
	return b.addScalar(s, bound, ir.Scalar{Kind: ir.Cast, Function: function, Signature: signature, Strict: metadata.Strict, CastContext: context}, args)
}

// userOperator is one SQL-defined operator overload visible on the search path.
type userOperator struct {
	descriptor *model.Descriptor
	metadata   model.OperatorMetadata
}

// userOperatorsNamed returns user-defined operator overloads with this name in
// search-path order. A schema resolution failure is recorded once; scans never
// silently drop a candidate.
func (b *binder) userOperatorsNamed(name string) []userOperator {
	if b.catalog == nil || name == "" {
		return nil
	}
	var found []userOperator
	seenSchema := make(map[model.ObjectID]bool)
	for _, schemaName := range b.operatorPath() {
		namespace, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if err != nil {
			if isMissingObject(err) {
				continue
			}
			b.castErr = errors.Join(b.castErr, err)
			return nil
		}
		if namespace.Descriptor == nil || seenSchema[namespace.Descriptor.ID()] {
			continue
		}
		seenSchema[namespace.Descriptor.ID()] = true
		scanErr := b.catalog.ScanNamespace(b.ctx, namespace.Descriptor.ID(), model.ObjectOperator, func(published catalog.PublishedDescriptor) error {
			if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Name().Name != name {
				return nil
			}
			spec, specErr := b.descriptorSpec(published.Descriptor)
			if specErr != nil {
				return specErr
			}
			if spec.Operator == nil {
				return model.ErrInvalidDescriptor
			}
			found = append(found, userOperator{descriptor: published.Descriptor, metadata: *spec.Operator})
			return nil
		})
		if scanErr != nil {
			b.castErr = errors.Join(b.castErr, scanErr)
			return nil
		}
	}
	return found
}

// operatorPath is operator lookup order: an explicit pg_catalog entry wins,
// otherwise pg_catalog is searched last, like routine resolution.
func (b *binder) operatorPath() []string {
	for _, name := range b.options.SearchPath {
		if name == "pg_catalog" {
			return b.options.SearchPath
		}
	}
	return append([]string{"pg_catalog"}, b.options.SearchPath...)
}

// userOperatorScore reports whether one user operator matches the argument
// types under the same implicit-coercion admissibility as builtin overloads.
// Unary and binary arity must match the operator's declared operand sides.
func (b *binder) userOperatorScore(operator userOperator, args []expression, pos int) int {
	unary := len(args) == 1
	left, right := operator.metadata.Left.Type.ID, operator.metadata.Right.Type.ID
	if unary != (left == 0) {
		return -1
	}
	if !unary && right == 0 {
		return -1
	}
	score := 0
	for i, arg := range args {
		target := right
		if i == 0 {
			target = left
		}
		source := b.argumentID(arg)
		if source == 0 || source == 705 {
			// An unresolved literal takes the operator's own operand type.
			continue
		}
		if source == target {
			score += 10
			continue
		}
		if !b.implicitCastWithUser(source, target) {
			return -1
		}
	}
	// The implementation must be a routine present in the pinned snapshot.
	if _, err := resolveDefinitionFunctionDescriptor(b, operator.metadata.Function); err != nil {
		return -1
	}
	return score
}

// userOperatorSignature registers the IR signature for a selected user
// operator and returns the bound call expression. The scope is required: the
// operator's implementation is a routine with its own parameter types, so
// argument resolution and coercion bind against it.
func (b *binder) userOperator(s *scope, descriptor *model.Descriptor, metadata model.OperatorMetadata, args []expression, pos int) (expression, error) {
	b.dependency(descriptor, 0)
	if b.castErr != nil {
		return expression{}, b.castErr
	}
	published, err := resolveDefinitionFunctionDescriptor(b, metadata.Function)
	if err != nil {
		return expression{}, errors.Join(bindError("42883", "operator function does not exist", pos), err)
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic || published.Descriptor.Kind() != model.ObjectRoutine {
		return expression{}, bindError("42883", "operator function does not exist", pos)
	}
	b.dependency(published.Descriptor, 0)
	spec, err := b.descriptorSpec(published.Descriptor)
	if err != nil {
		return expression{}, err
	}
	if spec.Routine == nil || spec.Routine.Procedure || spec.Routine.ReturnsSet {
		return expression{}, bindError("42809", "operator function must be a scalar function", pos)
	}
	inputs := routineInputs(spec.Routine.Parameters)
	if len(inputs) != len(args) {
		return expression{}, bindError("42883", "operator function arity does not match", pos)
	}
	converted := make([]model.TypeLayout, len(args))
	for i := range args {
		if args[i].typ.Type.ID == 0 {
			args[i], err = b.resolveUnknown(s, args[i], inputs[i].Layout)
			if err != nil {
				return expression{}, err
			}
		}
		target := inputs[i].Layout
		target.Type.Collation = args[i].typ.Type.Collation
		target.Type.Modifier = args[i].typ.Type.Modifier
		args[i], err = b.coerce(s, args[i], target, ir.ImplicitCast)
		if err != nil {
			return expression{}, err
		}
		converted[i] = args[i].typ
	}
	state, err := b.commonCollation(args)
	if err != nil {
		return expression{}, err
	}
	result, resultState := b.resultCollation(spec.Routine.Result, state)
	resultID, err := b.layout(result)
	if err != nil {
		return expression{}, err
	}
	parameters := make([]ir.LayoutID, len(args))
	for i := range args {
		parameters[i], err = b.layout(converted[i])
		if err != nil {
			return expression{}, err
		}
	}
	function := model.FunctionID(published.Descriptor.OID())
	effects := ir.Effects{MayError: true}
	switch spec.Routine.Volatility {
	case "stable":
		effects.Volatility = ir.Stable
	case "volatile":
		effects.Volatility = ir.Volatile
	}
	signature, err := b.builder.AddSignature(b.ctx, ir.SignatureSpec{
		Signature:  ir.Signature{Operator: model.OperatorID(descriptor.OID()), Result: resultID, InputCollation: state.id, Nullable: true, Strict: spec.Routine.Strict, Effects: effects},
		Parameters: parameters,
	})
	if err != nil {
		return expression{}, err
	}
	// A strict routine returns NULL rather than being called for a NULL
	// argument; a non-strict routine can return NULL for non-NULL input.
	nullable := true
	if spec.Routine.Strict {
		nullable = slices.ContainsFunc(args, func(arg expression) bool { return arg.nullable })
	}
	bound := expression{typ: result, collation: resultState, nullable: nullable, effects: effects, position: pos}
	return b.addScalar(s, bound, ir.Scalar{Kind: ir.Call, Function: function, Operator: model.OperatorID(descriptor.OID()), Signature: signature, Strict: spec.Routine.Strict}, args)
}
