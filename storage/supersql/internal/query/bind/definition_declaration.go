package bind

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// definitionKind is the logical object family owned by this binder.
type definitionKind uint8

const (
	definitionCast definitionKind = iota + 1
	definitionOperator
	definitionAggregate
)

// objectKind maps a definition family to its catalog kind. Aggregates share
// PostgreSQL's pg_proc name/signature namespace with routines, so they are
// ObjectRoutine descriptors carrying AggregateMetadata instead of RoutineMetadata.
func (k definitionKind) objectKind() model.ObjectKind {
	switch k {
	case definitionCast:
		return model.ObjectCast
	case definitionOperator:
		return model.ObjectOperator
	default:
		return model.ObjectRoutine
	}
}

func (k definitionKind) label() string {
	switch k {
	case definitionCast:
		return "CAST"
	case definitionOperator:
		return "OPERATOR"
	default:
		return "AGGREGATE"
	}
}

// definitionCommandKind classifies a statement this binder owns. Zero means the
// statement belongs to another domain.
func definitionCommandKind(node *zpgparse.Node) (definitionKind, bool) {
	if node == nil {
		return 0, false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_CreateCastStmt:
		return definitionCast, statement.CreateCastStmt != nil
	case *zpgparse.Node_AlterOperatorStmt:
		return definitionOperator, statement.AlterOperatorStmt != nil
	case *zpgparse.Node_DefineStmt:
		if statement.DefineStmt == nil {
			return 0, false
		}
		switch statement.DefineStmt.Kind {
		case zpgparse.ObjectType_OBJECT_OPERATOR:
			return definitionOperator, true
		case zpgparse.ObjectType_OBJECT_AGGREGATE:
			return definitionAggregate, true
		}
	case *zpgparse.Node_DropStmt:
		if statement.DropStmt == nil {
			return 0, false
		}
		switch statement.DropStmt.RemoveType {
		case zpgparse.ObjectType_OBJECT_CAST:
			return definitionCast, true
		case zpgparse.ObjectType_OBJECT_OPERATOR:
			return definitionOperator, true
		case zpgparse.ObjectType_OBJECT_AGGREGATE:
			return definitionAggregate, true
		}
	case *zpgparse.Node_RenameStmt:
		if statement.RenameStmt != nil && statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_AGGREGATE {
			return definitionAggregate, true
		}
	case *zpgparse.Node_AlterObjectSchemaStmt:
		if statement.AlterObjectSchemaStmt == nil {
			return 0, false
		}
		switch statement.AlterObjectSchemaStmt.ObjectType {
		case zpgparse.ObjectType_OBJECT_OPERATOR:
			return definitionOperator, true
		case zpgparse.ObjectType_OBJECT_AGGREGATE:
			return definitionAggregate, true
		}
	case *zpgparse.Node_CommentStmt:
		if statement.CommentStmt == nil {
			return 0, false
		}
		switch statement.CommentStmt.Objtype {
		case zpgparse.ObjectType_OBJECT_CAST:
			return definitionCast, true
		case zpgparse.ObjectType_OBJECT_OPERATOR:
			return definitionOperator, true
		case zpgparse.ObjectType_OBJECT_AGGREGATE:
			return definitionAggregate, true
		}
	}
	return 0, false
}

// IsDefinitionCommand reports whether this binder owns the statement.
func IsDefinitionCommand(node *zpgparse.Node) bool {
	_, handled := definitionCommandKind(node)
	return handled
}

// BindDefinitionCommand binds SQL-defined CAST, OPERATOR, and AGGREGATE
// declarations plus their DROP/RENAME/SET SCHEMA/COMMENT lifecycle forms to real
// catalog descriptors. These objects are consumers of the existing
// routine/cast/operator machinery: an aggregate is a pg_proc identity executing
// real SQL/PLpgSQL support routines, an operator is an overloadable name
// resolving to one function, and a cast is a coercion rule reached by ordinary
// expression binding.
func BindDefinitionCommand(ctx context.Context, node *zpgparse.Node, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, bool, error) {
	kind, handled := definitionCommandKind(node)
	if !handled {
		return nil, false, nil
	}
	var command *BoundObjectCommand
	var err error
	switch statement := node.Node.(type) {
	case *zpgparse.Node_CreateCastStmt:
		command, err = bindCreateCast(ctx, statement.CreateCastStmt, snapshot, options)
	case *zpgparse.Node_AlterOperatorStmt:
		command, err = bindAlterOperator(ctx, statement.AlterOperatorStmt, snapshot, options)
	case *zpgparse.Node_DefineStmt:
		switch statement.DefineStmt.Kind {
		case zpgparse.ObjectType_OBJECT_OPERATOR:
			command, err = bindCreateOperator(ctx, statement.DefineStmt, snapshot, options)
		default:
			command, err = bindCreateAggregate(ctx, statement.DefineStmt, snapshot, options)
		}
	case *zpgparse.Node_DropStmt:
		command, err = bindDropDefinition(ctx, statement.DropStmt, kind, snapshot, options)
	case *zpgparse.Node_RenameStmt:
		command, err = bindRenameDefinition(ctx, statement.RenameStmt, snapshot, options)
	case *zpgparse.Node_AlterObjectSchemaStmt:
		command, err = bindSetDefinitionSchema(ctx, statement.AlterObjectSchemaStmt, kind, snapshot, options)
	case *zpgparse.Node_CommentStmt:
		command, err = bindCommentDefinition(ctx, statement.CommentStmt, kind, snapshot, options)
	}
	return command, true, err
}

// definitionSchema resolves the namespace that owns a declaration. Casts are
// not schema-qualified in PostgreSQL and are recorded under pg_catalog.
func definitionSchema(ctx context.Context, b *binder, snapshot CatalogSnapshot, parts []string, kind definitionKind) (*model.Descriptor, error) {
	name := "pg_catalog"
	if kind != definitionCast {
		path := b.options.SearchPath
		if len(parts) == 2 {
			path = parts[:1]
		}
		if len(path) == 0 {
			return nil, bindError("3F000", "no schema has been selected to create in", 0)
		}
		name = path[0]
	}
	schema, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	b.dependency(schema.Descriptor, 0)
	return schema.Descriptor, nil
}

func definitionName(parts []string) (string, error) {
	if len(parts) == 0 || len(parts) > 2 {
		return "", bindError("42601", "invalid object name", 0)
	}
	name := parts[len(parts)-1]
	if err := declarationName(name); err != nil {
		return "", err
	}
	return name, nil
}

// definitionDescriptor builds one version-1 descriptor for a new definition
// object with its owning schema dependency already recorded. The metadata
// callback returns an error so a shape or conflict problem found while filling
// the spec still fails the declaration.
func definitionDescriptor(ctx context.Context, b *binder, options CreateDeclarationOptions, identity definitionIdentity, schema *model.Descriptor, kind definitionKind, name, signature string, metadata func(*model.DescriptorSpec) error) (*model.Descriptor, error) {
	id, oid := identity.id, identity.oid
	if id == 0 || oid == 0 {
		allocated, allocatedOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, err
		}
		id, oid = allocated, allocatedOID
	}
	spec := model.DescriptorSpec{
		ID: id, OID: oid, Version: 1, State: model.StatePublic,
		Name:         model.NamespaceName{Parent: schema.ID(), Kind: kind.objectKind(), Name: name, Signature: signature},
		Dependencies: slices.Clone(b.dependencies),
	}
	if err := metadata(&spec); err != nil {
		return nil, err
	}
	return model.NewDescriptor(ctx, spec, options.Descriptors)
}

// definitionIdentity is a declaration identity allocated before the descriptor
// is built. A self-referential declaration option (an operator that is its own
// commutator) must record the declaration's own OID, which does not exist until
// an identity is allocated, so the allocation is hoisted for that case. The zero
// value asks definitionDescriptor to allocate on demand.
type definitionIdentity struct {
	id  model.ObjectID
	oid model.OID
}

// definitionMutation advances one existing definition descriptor. The metadata
// callback runs on the versioned spec and may adjust both metadata and
// dependencies; the resulting dependency set is the union of the callback's
// edits and the binder's recorded dependencies, so neither the new schema edge
// nor an existing cascade edge is lost.
func definitionMutation(ctx context.Context, b *binder, base *model.Descriptor, options CreateDeclarationOptions, dropped []model.ObjectID, metadata func(*model.DescriptorSpec) error) (*model.Descriptor, error) {
	if base == nil || base.Version() == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	b.dependency(base, 0)
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	spec.Version++
	err = metadata(&spec)
	spec.Dependencies = mergeDependencies(spec.Dependencies, descriptorDependencies(b.dependencies, base.ID()), base.ID(), dropped)
	if err != nil {
		return nil, err
	}
	return model.NewDescriptor(ctx, spec, options.Descriptors)
}

// mergeDependencies unions two dependency sets, removing any edge that points at
// the descriptor itself, and returns a sorted, deduplicated result.
func mergeDependencies(edited, recorded []model.Dependency, self model.ObjectID, dropped []model.ObjectID) []model.Dependency {
	result := make([]model.Dependency, 0, len(edited)+len(recorded))
	result = append(result, edited...)
	result = append(result, recorded...)
	// A dependency the caller deliberately severed (for example the old schema
	// of a SET SCHEMA move) must not be restored from the recorded set, or the
	// previous schema could never be dropped afterwards.
	if len(dropped) != 0 {
		result = slices.DeleteFunc(result, func(dependency model.Dependency) bool {
			return slices.Contains(dropped, dependency.Object)
		})
	}
	slices.SortFunc(result, func(a, b model.Dependency) int {
		if order := cmp.Compare(a.Object, b.Object); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Version, b.Version); order != 0 {
			return order
		}
		return cmp.Compare(a.Column, b.Column)
	})
	result = slices.Compact(result)
	return slices.DeleteFunc(result, func(dependency model.Dependency) bool {
		return dependency.Object == self
	})
}

// ---------------------------------------------------------------------------
// Support function references
// ---------------------------------------------------------------------------

// definitionFunction is a resolved callable used by a definition object. It
// carries both the executable identity and, for user-defined routines, the
// descriptor so dependency and DROP closure stay real.
type definitionFunction struct {
	descriptor *model.Descriptor
	id         model.FunctionID
	layout     model.TypeLayout
}

// resolveDefinitionFunctionPath resolves a callable in the search path before
// falling back to pinned builtins. PostgreSQL resolves CREATE OPERATOR/
// AGGREGATE support functions through the current search path, so a user
// routine shadows an equally named builtin.
func resolveDefinitionFunctionPath(ctx context.Context, b *binder, snapshot CatalogSnapshot, name string) (definitionFunction, bool, error) {
	if name == "" {
		return definitionFunction{}, false, nil
	}
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return definitionFunction{}, false, bindError("42883", "function does not exist", 0)
	}
	if strings.Contains(name, "(") {
		return definitionFunction{}, false, bindError("0A000", "support function arguments must be inferred from the declaration", 0)
	}
	bare := parts[len(parts)-1]
	for _, schemaName := range b.functionPath(parts) {
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return definitionFunction{}, false, err
		}
		var selected *model.Descriptor
		scanErr := snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(published catalog.PublishedDescriptor) error {
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Name().Name != bare {
				return nil
			}
			spec, err := descriptor.Spec(ctx)
			if err != nil {
				return err
			}
			if spec.Routine == nil || spec.Routine.Procedure {
				return nil
			}
			if selected != nil {
				return bindError("42725", "support function reference is ambiguous", 0)
			}
			selected = descriptor
			return nil
		})
		if scanErr != nil {
			return definitionFunction{}, false, scanErr
		}
		if selected != nil {
			spec, err := b.descriptorSpec(selected)
			if err != nil {
				return definitionFunction{}, false, err
			}
			b.dependency(namespace.Descriptor, 0)
			b.dependency(selected, 0)
			return definitionFunction{descriptor: selected, id: model.FunctionID(selected.OID()), layout: spec.Routine.Result}, true, nil
		}
		if len(parts) == 2 {
			break
		}
	}
	// No user routine: only a pinned builtin unambiguous by bare name qualifies.
	var match builtin.Signature
	count := 0
	for signature := range builtin.SignaturesNamed(bare) {
		if signature.Kind != 'f' || signature.ReturnsSet || signature.Variadic != 0 {
			continue
		}
		match = signature
		count++
	}
	if count != 1 {
		return definitionFunction{}, false, nil
	}
	if err := ValidateBuiltinFunction(match.ID, 0); err != nil {
		return definitionFunction{}, false, err
	}
	return definitionFunction{id: match.ID, layout: b.primitive(match.Result)}, true, nil
}

// supportPolymorphicClass maps a polymorphic pseudo-type onto the class whose
// concrete instantiation must agree across every position of that class. This
// is the coupling PostgreSQL enforces: two anyelement positions in one
// signature must resolve to the same type, and an anyarray position must be the
// array of that same type. A zero class is uncoupled, which is `any`.
func supportPolymorphicClass(id model.TypeID) model.TypeID {
	switch id {
	case 2283, 2776, 3500, 5077, 5079:
		return 2283 // element-like: plain, non-array, or enum
	case 2277, 5078:
		return 5078 // array-like
	case 3831, 5080:
		return 3831 // range-like
	case 4537, 4538:
		return 4537 // multirange-like
	}
	return 0
}

// supportPseudoTypesCompatible reports whether a declaration's pseudo-type
// position may be satisfied by another pseudo-type position. `any` (2276)
// constrains nothing, which is how PostgreSQL's own ordered-set support
// functions declare their argument; otherwise both sides must belong to the
// same instantiation class, so an anyarray position can never satisfy an
// anyelement one.
func supportPseudoTypesCompatible(declared, resolved model.TypeID) bool {
	if declared == resolved {
		return true
	}
	if declared == 2276 || resolved == 2276 {
		return true
	}
	left, right := supportPolymorphicClass(declared), supportPolymorphicClass(resolved)
	return left != 0 && left == right
}

// supportPolymorphicShape reports whether one concrete layout may instantiate a
// declared pseudo-type. Each pseudo-type admits a type family, so this is a
// constraint rather than a wildcard: anyarray must be an array, anynonarray
// must not be, and the enum/range/multirange pseudo-types require the matching
// declared kind. `any` and anyelement accept any concrete type.
func supportPolymorphicShape(b *binder, declared model.TypeID, resolved model.TypeLayout) bool {
	// Both sides may still be declarations rather than values: a declaration's
	// support routine can itself be declared over a pseudo-type. Identity and
	// class are the only contracts available there, and no storage exists to
	// inspect, so this is stricter than the concrete case rather than looser.
	if model.IsPseudoTypeID(resolved.Type.ID) {
		return supportPseudoTypesCompatible(declared, resolved.Type.ID)
	}
	switch declared {
	case 2277, 5078:
		return resolved.Type.Storage == model.TypeArray && len(resolved.Children) == 1
	case 2776, 5079:
		return resolved.Type.Storage != model.TypeArray
	case 3500:
		return b.declaredTypeKind(resolved.Type.ID, model.TypeKindEnum)
	case 3831, 5080:
		return b.declaredTypeKind(resolved.Type.ID, model.TypeKindRange)
	case 4537, 4538:
		return b.declaredTypeKind(resolved.Type.ID, model.TypeKindMultirange)
	}
	return true
}

// supportArgumentsMatch reports whether a support function's declared inputs can
// receive the argument list a declaration supplies. Concrete positions must
// agree exactly. The `internal` pseudo-type matches by identity, since it has
// two admissible representations: the persisted transition layout carries the
// executor's bounded byte payload while a pinned signature reports the
// pseudo-type.
//
// A polymorphic position is instantiated by the concrete layout at that
// position, subject to its family constraint, and every position of the same
// class must agree on one instantiation. A VARIADIC parameter absorbs every
// remaining argument, which is the shape rank_final(internal, VARIADIC
// anyelement) and ordered_set_transition_multi(internal, VARIADIC anyelement)
// declare and a hypothetical-set aggregate with several arguments relies on.
func supportArgumentsMatch(b *binder, expected, inputs []model.TypeLayout, variadic model.TypeLayout) bool {
	if variadic.Type.ID == 0 && len(inputs) != len(expected) {
		return false
	}
	if variadic.Type.ID != 0 && len(expected) < len(inputs) {
		return false
	}
	var element, array, rangeType, multirange model.TypeLayout
	for i, declared := range expected {
		var resolved model.TypeLayout
		switch {
		case i < len(inputs):
			resolved = inputs[i]
		case variadic.Type.ID != 0:
			resolved = variadic
		default:
			return false
		}
		if declared.Type.ID == 2281 || resolved.Type.ID == 2281 {
			if declared.Type.ID != resolved.Type.ID {
				return false
			}
			continue
		}
		if !polymorphic(declared.Type.ID) {
			if !sameType(declared, resolved) {
				return false
			}
			continue
		}
		if declared.Type.ID == 2276 {
			// `any` accepts any concrete type and imposes no coupling.
			continue
		}
		if !supportPolymorphicShape(b, declared.Type.ID, resolved) {
			return false
		}
		var slot *model.TypeLayout
		switch supportPolymorphicClass(declared.Type.ID) {
		case 2283:
			slot = &element
		case 5078:
			slot = &array
		case 3831:
			slot = &rangeType
		case 4537:
			slot = &multirange
		default:
			return false
		}
		if slot.Type.ID == 0 {
			*slot = resolved
		} else if !sameType(*slot, resolved) {
			return false
		}
	}
	// A coupled anyarray position must be the array of the element position's
	// instantiation, which is what makes the two classes one family rather than
	// two independent wildcards. When the array position is still a declared
	// pseudo-type, the only expressible relationship is the declared one: the
	// element must be that array pseudo-type's element pseudo-type.
	if array.Type.ID != 0 && element.Type.ID != 0 {
		if model.IsPseudoTypeID(array.Type.ID) {
			want, ok := model.PseudoTypeArrayElement(array.Type.ID)
			if !ok || element.Type.ID != want {
				return false
			}
			return true
		}
		if len(array.Children) != 1 || !sameType(array.Children[0], element) {
			return false
		}
	}
	return true
}

// supportResultMatches compares a support function's result layout with the
// layout the declaration requires. The `internal` pseudo-type has two
// admissible representations (the pinned pseudo-type and the executor's bounded
// byte payload), so it compares by identity; every other layout compares
// exactly, which is what keeps a moving final function pinned to the aggregate
// result type.
func supportResultMatches(declared, resolved model.TypeLayout) bool {
	if declared.Type.ID == 2281 || resolved.Type.ID == 2281 {
		return declared.Type.ID == resolved.Type.ID
	}
	return sameType(declared, resolved)
}

// definitionSupport resolves a support function and requires the exact layout
// PostgreSQL demands for its position in the operator or aggregate definition.
// User routines are shape-checked from their descriptor too, so an ill-typed
// SQL or PL/pgSQL support function cannot silently enter the catalog.
func definitionSupport(ctx context.Context, b *binder, snapshot CatalogSnapshot, name string, expected []model.TypeLayout, result model.TypeLayout) (definitionFunction, error) {
	function, found, err := resolveDefinitionFunctionPath(ctx, b, snapshot, name)
	if err != nil {
		return definitionFunction{}, err
	}
	if !found {
		return definitionFunction{}, bindError("42883", "support function does not exist", 0)
	}
	if function.descriptor != nil {
		spec, err := b.descriptorSpec(function.descriptor)
		if err != nil {
			return definitionFunction{}, err
		}
		if spec.Routine == nil {
			return definitionFunction{}, bindError("42883", "support function is not a routine", 0)
		}
		if spec.Routine.ReturnsSet || spec.Routine.Procedure {
			return definitionFunction{}, bindError("42P13", "support function must be a scalar function", 0)
		}
		inputs := routineInputs(spec.Routine.Parameters)
		// A VARIADIC support parameter absorbs every remaining argument, so the
		// declared list is matched against the fixed parameters followed by the
		// variadic element type rather than requiring equal arity.
		var variadic model.TypeLayout
		if len(inputs) != 0 && inputs[len(inputs)-1].Mode == "variadic" {
			variadic = inputs[len(inputs)-1].Layout
			if len(variadic.Children) == 1 {
				variadic = variadic.Children[0]
			}
			inputs = inputs[:len(inputs)-1]
		}
		inputLayouts := make([]model.TypeLayout, len(inputs))
		for i := range inputs {
			inputLayouts[i] = inputs[i].Layout
		}
		if !supportArgumentsMatch(b, expected, inputLayouts, variadic) {
			return definitionFunction{}, bindError("42P13", "support function argument count does not match the declaration", 0)
		}
		if result.Type.ID != 0 && !sameType(spec.Routine.Result, result) {
			return definitionFunction{}, bindError("42P13", "support function result type does not match the declaration", 0)
		}
		return function, nil
	}
	signature, builtinSignature := b.lookupSignature(function.id)
	if !builtinSignature {
		return function, nil
	}
	builtinInputs := make([]model.TypeLayout, int(signature.ArgumentCount))
	for i := range builtinInputs {
		builtinInputs[i] = b.primitive(signature.Arguments[i])
	}
	var builtinVariadic model.TypeLayout
	if signature.Variadic != 0 {
		builtinVariadic = b.primitive(signature.Variadic)
		builtinInputs = builtinInputs[:len(builtinInputs)-1]
	}
	if !supportArgumentsMatch(b, expected, builtinInputs, builtinVariadic) {
		return definitionFunction{}, bindError("42P13", "support function argument count does not match the declaration", 0)
	}
	if result.Type.ID != 0 && !sameType(b.primitive(signature.Result), result) {
		return definitionFunction{}, bindError("42P13", "support function result type does not match the declaration", 0)
	}
	return function, nil
}

// ---------------------------------------------------------------------------
// CREATE CAST
// ---------------------------------------------------------------------------

// castDescriptorName is the stable catalog identity of a cast. PostgreSQL keys
// casts by (source,target) type pair and does not schema-qualify them.
func castDescriptorName(source, target model.TypeID) string {
	return strconv.FormatUint(uint64(source), 10) + ":" + strconv.FormatUint(uint64(target), 10)
}

func bindCreateCast(ctx context.Context, statement *zpgparse.CreateCastStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE CAST", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateCastStmt{CreateCastStmt: statement}})
	if err != nil {
		return nil, err
	}
	source, err := b.typeName(statement.Sourcetype)
	if err != nil {
		return nil, err
	}
	target, err := b.typeName(statement.Targettype)
	if err != nil {
		return nil, err
	}
	if source.Type.ID == 0 || target.Type.ID == 0 {
		return nil, bindError("42704", "cast source and target types must be resolved", 0)
	}
	schema, err := definitionSchema(ctx, b, snapshot, nil, definitionCast)
	if err != nil {
		return nil, err
	}
	name := castDescriptorName(source.Type.ID, target.Type.ID)
	identity := model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectCast, Name: name}
	if published, conflictErr := snapshot.Resolve(ctx, identity); conflictErr == nil && published.Descriptor != nil {
		return nil, bindError("42710", "cast from type to type already exists", 0)
	} else if conflictErr != nil && !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	// A builtin pair already has an admissible cast; declaring a second one
	// would silently redefine fixed semantics.
	if _, builtinPair := builtin.LookupCast(source.Type.ID, target.Type.ID); builtinPair {
		return nil, bindError("42710", "cast from type to type already exists", 0)
	}
	metadata := model.CastMetadata{Owner: options.Owner, Source: source, Target: target}
	switch {
	case statement.Inout:
		metadata.Method = 'i'
		metadata.Context = castContext(statement.Context)
	case statement.Func == nil:
		metadata.Method = 'b'
		metadata.Context = castContext(statement.Context)
	default:
		if statement.Func.ArgsUnspecified || len(statement.Func.Objargs) != 1 {
			return nil, bindError("42804", "cast function must take one argument", 0)
		}
		parts, err := names(statement.Func.Objname)
		if err != nil {
			return nil, err
		}
		function, found, resolveErr := resolveDefinitionFunctionPath(ctx, b, snapshot, strings.Join(parts, "."))
		if resolveErr != nil {
			return nil, resolveErr
		}
		if !found {
			return nil, bindError("42883", "cast function does not exist", 0)
		}
		argument, err := b.typeName(statement.Func.Objargs[0].GetTypeName())
		if err != nil {
			return nil, err
		}
		if !sameType(argument, source) {
			return nil, bindError("42P17", "cast function argument type does not match the source type", 0)
		}
		if !sameType(function.layout, target) {
			return nil, bindError("42P17", "cast function result type does not match the target type", 0)
		}
		metadata.Method = 'f'
		metadata.Context = castContext(statement.Context)
		metadata.Function = function.id
	}
	if metadata.Method == 'b' {
		// A binary-coercible cast must agree on physical representation.
		if source.Type.Storage != target.Type.Storage {
			return nil, bindError("42P17", "a binary-coercible cast requires the same physical representation", 0)
		}
	}
	descriptor, err := definitionDescriptor(ctx, b, options, definitionIdentity{}, schema, definitionCast, name, "", func(spec *model.DescriptorSpec) error {
		spec.Cast = &metadata
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "CREATE CAST", creates: []*model.Descriptor{descriptor}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func castContext(context zpgparse.CoercionContext) byte {
	switch context {
	case zpgparse.CoercionContext_COERCION_IMPLICIT:
		return 'i'
	case zpgparse.CoercionContext_COERCION_ASSIGNMENT:
		return 'a'
	case zpgparse.CoercionContext_COERCION_PLPGSQL:
		// PL/pgSQL-only casts are assignment casts for ordinary SQL.
		return 'a'
	default:
		return 'e'
	}
}

// ---------------------------------------------------------------------------
// CREATE OPERATOR / ALTER OPERATOR
// ---------------------------------------------------------------------------

func bindCreateOperator(ctx context.Context, statement *zpgparse.DefineStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Oldstyle || statement.IfNotExists {
		return nil, bindError("42601", "invalid CREATE OPERATOR", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DefineStmt{DefineStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Defnames)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid operator name", 0)
	}
	name := parts[len(parts)-1]
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return nil, bindError("42601", "invalid operator name", 0)
	}
	schema, err := definitionSchema(ctx, b, snapshot, parts, definitionOperator)
	if err != nil {
		return nil, err
	}
	options_ := operatorOptionsFields{}
	var left, right *zpgparse.TypeName
	commutator, negator := "", ""
	restrict, join := "", ""
	for _, node := range statement.Definition {
		option := node.GetDefElem()
		if option == nil {
			return nil, bindError("42601", "invalid operator option", 0)
		}
		switch strings.ToLower(option.Defname) {
		case "leftarg":
			left = option.Arg.GetTypeName()
		case "rightarg":
			right = option.Arg.GetTypeName()
		case "function", "procedure":
			options_.function = operatorOptionText(option)
		case "commutator":
			commutator = operatorOptionText(option)
		case "negator":
			negator = operatorOptionText(option)
		case "restrict":
			restrict = operatorOptionText(option)
		case "join":
			join = operatorOptionText(option)
		case "sort1", "sort2", "ltcmp", "gtcmp":
			// Obsolete ordering flags: the pinned grammar and PostgreSQL accept
			// them. This model records no ordering operator, so they are inert.
		case "hashes":
			options_.hashes = true
		case "merges":
			options_.merges = true
		default:
			// PostgreSQL warns and ignores an unrecognized operator attribute
			// rather than failing the declaration.
			b.warnings = append(b.warnings, model.Error{Code: "01000", Message: "operator attribute \"" + option.Defname + "\" not recognized"})
		}
	}
	if options_.function == "" {
		return nil, bindError("42P13", "operator function must be specified", 0)
	}
	if left == nil && right == nil {
		return nil, bindError("42P13", "operator argument types must be specified", 0)
	}
	// PostgreSQL removed postfix operators: a left argument without a right
	// argument is only valid for the binary form.
	if left != nil && right == nil {
		return nil, bindError("42P13", "postfix operators are not supported", 0)
	}
	// The declaration's identity is reserved before its options are resolved so a
	// self-referential option records the real OID rather than a placeholder.
	reserved, operatorOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	metadata := model.OperatorMetadata{Owner: options.Owner, Name: name}
	var expected []model.TypeLayout
	if left != nil {
		metadata.Left, err = b.typeName(left)
		if err != nil {
			return nil, err
		}
		if left.Setof {
			return nil, bindError("42P13", "SETOF type not allowed for operator argument", 0)
		}
		expected = append(expected, metadata.Left)
	}
	metadata.Right, err = b.typeName(right)
	if err != nil {
		return nil, err
	}
	if right.Setof {
		return nil, bindError("42P13", "SETOF type not allowed for operator argument", 0)
	}
	expected = append(expected, metadata.Right)
	function, found, resolveErr := resolveDefinitionFunctionPath(ctx, b, snapshot, options_.function)
	if resolveErr != nil {
		return nil, resolveErr
	}
	if !found {
		return nil, bindError("42883", "operator function does not exist", 0)
	}
	if signature, ok := builtin.LookupSignature(function.id); ok {
		if int(signature.ArgumentCount) != len(expected) {
			return nil, bindError("42P13", "operator function argument count does not match the declaration", 0)
		}
		for i := range expected {
			if !sameType(b.primitive(signature.Arguments[i]), expected[i]) {
				return nil, bindError("42P13", "operator function argument type does not match the declaration", 0)
			}
		}
	}
	metadata.Function = function.id
	metadata.Result = function.layout
	if metadata.Result.Type.ID == 0 || metadata.Result.Type.Storage == model.TypeAny {
		return nil, bindError("42P13", "operator function must return a concrete type", 0)
	}
	signature := operatorSignature(metadata.Left, metadata.Right)
	identity := model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectOperator, Name: name, Signature: signature}
	if published, conflictErr := snapshot.Resolve(ctx, identity); conflictErr == nil && published.Descriptor != nil {
		return nil, bindError("42710", "operator already exists", 0)
	} else if conflictErr != nil && !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	if _, builtinPair := builtinOperatorNamed(name, metadata.Left, metadata.Right); builtinPair {
		return nil, bindError("42710", "operator already exists", 0)
	}
	if commutator != "" {
		// A commutator names the operator with the operands exchanged, so the
		// reference resolves against the reversed operand types. An operator may
		// be its own commutator (the textbook equality case); that records the
		// declaration's own identity, which the reserved OID supplies.
		resolved, resolveErr := resolveOperatorReference(ctx, b, snapshot, commutator, metadata.Right, metadata.Left)
		if resolveErr != nil {
			if !isMissingObject(resolveErr) {
				return nil, resolveErr
			}
			if !sameOperatorReference(commutator, name) {
				return nil, resolveErr
			}
			resolved = model.OperatorID(operatorOID)
		}
		if operatorPairClaimed(resolved, true) {
			return nil, bindError("42P13", "commutator operator "+commutator+" is already the commutator of operator "+name, 0)
		}
		metadata.Commutator = resolved
	}
	if negator != "" {
		// An operator cannot be its own negator. PostgreSQL reports this both for
		// the direct form and for the form where the reference names this
		// declaration through a shell operator's identity; our engine compares
		// against the reserved OID, which is what a shell would have carried.
		resolved, resolveErr := resolveOperatorReference(ctx, b, snapshot, negator, metadata.Left, metadata.Right)
		if resolveErr != nil {
			if !isMissingObject(resolveErr) {
				return nil, resolveErr
			}
			if !sameOperatorReference(negator, name) {
				return nil, resolveErr
			}
			resolved = model.OperatorID(operatorOID)
		}
		if resolved == model.OperatorID(operatorOID) {
			return nil, bindError("42P13", "operator cannot be its own negator", 0)
		}
		if operatorPairClaimed(resolved, false) {
			return nil, bindError("42P13", "negator operator "+negator+" is already the negator of operator "+name, 0)
		}
		metadata.Negator = resolved
	}
	// RESTRICT and JOIN are independent operator options. Each is resolved and
	// shape-checked on its own; a declaration may legally specify either, both,
	// or neither. Their pinned signatures differ, so the argument lists are not
	// interchangeable: restrict takes (internal, oid, internal, int4) and join
	// takes (internal, oid, internal, int2, internal), both returning float8.
	if restrict != "" {
		function, err := definitionSupport(ctx, b, snapshot, restrict, operatorRestrictSupportLayouts(b), b.primitive(701))
		if err != nil {
			return nil, err
		}
		if !sameType(function.layout, b.primitive(701)) {
			return nil, bindError("42P13", "operator restrict function must return float8", 0)
		}
		metadata.Restrict = function.id
	}
	if join != "" {
		function, err := definitionSupport(ctx, b, snapshot, join, operatorJoinSupportLayouts(b), b.primitive(701))
		if err != nil {
			return nil, err
		}
		if !sameType(function.layout, b.primitive(701)) {
			return nil, bindError("42P13", "operator join function must return float8", 0)
		}
		metadata.Join = function.id
	}
	// HASHES and MERGES are declaration flags recorded on the operator. They are
	// not a promise that this engine can execute hash or merge joins for the
	// operator, and PostgreSQL does not reject them for lacking a support
	// function, so they are persisted as declared.
	metadata.CanHash = options_.hashes
	metadata.CanMerge = options_.merges
	descriptor, err := definitionDescriptor(ctx, b, options, definitionIdentity{id: reserved, oid: operatorOID}, schema, definitionOperator, name, signature, func(spec *model.DescriptorSpec) error {
		spec.Operator = &metadata
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "CREATE OPERATOR", creates: []*model.Descriptor{descriptor}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

type operatorOptionsFields struct {
	function string
	hashes   bool
	merges   bool
}

func operatorOptionText(option *zpgparse.DefElem) string {
	if option == nil || option.Arg == nil {
		return ""
	}
	if value := option.Arg.GetString_(); value != nil {
		return value.Sval
	}
	if list := option.Arg.GetList(); list != nil {
		parts := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			value := item.GetString_()
			if value == nil {
				return ""
			}
			parts = append(parts, value.Sval)
		}
		return strings.Join(parts, ".")
	}
	return ""
}

// operatorOptionBoolean reads a valued boolean operator option. PostgreSQL uses
// defGetBoolean for ALTER OPERATOR HASHES/MERGES, so a missing or non-boolean
// value is an error rather than an implicit false.
func operatorOptionBoolean(option *zpgparse.DefElem) (bool, error) {
	if option == nil || option.Arg == nil {
		return false, bindError("42601", "operator attribute requires a boolean value", 0)
	}
	if value := option.Arg.GetBoolean(); value != nil {
		return value.Boolval, nil
	}
	if text := operatorOptionText(option); text != "" {
		switch strings.ToLower(text) {
		case "true", "on", "yes", "1":
			return true, nil
		case "false", "off", "no", "0":
			return false, nil
		}
	}
	return false, bindError("42601", "operator attribute requires a boolean value", 0)
}

// currentInternal is the pg_catalog internal pseudo-type that PostgreSQL passes
// to planner support functions.
func currentInternal(b *binder) model.TypeLayout { return b.primitive(2281) }

// operatorRestrictSupportLayouts is the pinned signature of an operator's
// RESTRICT function: (internal, oid, internal, int4) -> float8. It is not the
// same list as JOIN's, so the two options cannot share a shape check.
func operatorRestrictSupportLayouts(b *binder) []model.TypeLayout {
	return []model.TypeLayout{currentInternal(b), b.primitive(26), currentInternal(b), b.primitive(23)}
}

// operatorJoinSupportLayouts is the pinned signature of an operator's JOIN
// function: (internal, oid, internal, int2, internal) -> float8.
func operatorJoinSupportLayouts(b *binder) []model.TypeLayout {
	return []model.TypeLayout{currentInternal(b), b.primitive(26), currentInternal(b), b.primitive(21), currentInternal(b)}
}

// operatorSignature keys an operator namespace entry by its operand types.
// PostgreSQL separates prefix and binary operators by the absent left operand.
func operatorSignature(left, right model.TypeLayout) string {
	return strconv.FormatUint(uint64(left.Type.ID), 10) + "," + strconv.FormatUint(uint64(right.Type.ID), 10)
}

// sameOperatorReference reports whether a textual operator reference names the
// operator being declared. PostgreSQL rejects a declaration whose negator is the
// operator itself, before the operator is published, so the check is textual: an
// unqualified reference with the same name is this declaration. A qualified
// form resolves normally and is caught by the identity check after publish.
func sameOperatorReference(reference, name string) bool {
	parts := strings.Split(reference, ".")
	return len(parts) == 1 && parts[0] == name
}

// operatorPairClaimed reports whether a referenced operator already has its
// commutator or negator slot filled. PostgreSQL refuses to reuse a referenced
// operator as a commutator or negator once it is itself paired, which is also
// why an operator like int4 "=" (already its own commutator) is rejected as a
// commutator for a new operator. A reference to the declaration being created is
// not yet published, so it is never claimed.
func operatorPairClaimed(reference model.OperatorID, commutator bool) bool {
	if reference == 0 {
		return false
	}
	operator, ok := builtin.LookupOperator(reference)
	if !ok {
		return false
	}
	if commutator {
		return operator.Commutator != 0
	}
	return operator.Negator != 0
}

// builtinOperatorNamed reports whether a pinned builtin already claims this
// (name, left, right) operator identity.
func builtinOperatorNamed(name string, left, right model.TypeLayout) (builtin.Operator, bool) {
	for operator := range builtin.OperatorsNamed(name) {
		if operator.Left == left.Type.ID && operator.Right == right.Type.ID {
			return operator, true
		}
	}
	return builtin.Operator{}, false
}

// resolveOperatorReference resolves an operator identity to its OperatorID.
func resolveOperatorReference(ctx context.Context, b *binder, snapshot CatalogSnapshot, name string, left, right model.TypeLayout) (model.OperatorID, error) {
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return 0, bindError("42883", "operator does not exist", 0)
	}
	bare := parts[len(parts)-1]
	for _, schemaName := range b.functionPath(parts) {
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return 0, err
		}
		identity := model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectOperator, Name: bare, Signature: operatorSignature(left, right)}
		published, resolveErr := snapshot.Resolve(ctx, identity)
		if resolveErr == nil && published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			b.dependency(namespace.Descriptor, 0)
			b.dependency(published.Descriptor, 0)
			return model.OperatorID(published.Descriptor.OID()), nil
		}
		if resolveErr != nil && !isMissingObject(resolveErr) {
			return 0, resolveErr
		}
		if len(parts) == 2 {
			break
		}
	}
	if operator, ok := builtinOperatorNamed(bare, left, right); ok {
		return operator.ID, nil
	}
	return 0, bindError("42883", "operator does not exist", 0)
}

func bindAlterOperator(ctx context.Context, statement *zpgparse.AlterOperatorStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Opername == nil {
		return nil, bindError("42601", "invalid ALTER OPERATOR", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterOperatorStmt{AlterOperatorStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveOperatorDescriptor(ctx, b, snapshot, statement.Opername)
	if err != nil {
		return nil, err
	}
	next, err := definitionMutation(ctx, b, base, options, nil, func(spec *model.DescriptorSpec) error {
		if spec.Operator == nil {
			return model.ErrInvalidDescriptor
		}
		metadata := *spec.Operator
		restrict, join := metadata.Restrict, metadata.Join
		hashes, merges := metadata.CanHash, metadata.CanMerge
		for _, node := range statement.Options {
			option := node.GetDefElem()
			if option == nil {
				return bindError("42601", "invalid ALTER OPERATOR option", 0)
			}
			value := option.Arg
			set := value != nil && value.GetString_() != nil || value != nil && value.GetList() != nil
			switch strings.ToLower(option.Defname) {
			case "restrict":
				if !set {
					restrict = 0
					continue
				}
				function, resolveErr := resolvePlannerFunction(ctx, b, snapshot, operatorOptionText(option), operatorRestrictSupportLayouts(b), b.primitive(701))
				if resolveErr != nil {
					return resolveErr
				}
				restrict = function
			case "join":
				if !set {
					join = 0
					continue
				}
				function, resolveErr := resolvePlannerFunction(ctx, b, snapshot, operatorOptionText(option), operatorJoinSupportLayouts(b), b.primitive(701))
				if resolveErr != nil {
					return resolveErr
				}
				join = function
			case "hashes":
				// ALTER reads HASHES/MERGES with defGetBoolean, so a bare flag
				// is an error rather than an implicit false.
				value, boolErr := operatorOptionBoolean(option)
				if boolErr != nil {
					return boolErr
				}
				hashes = value
			case "merges":
				value, boolErr := operatorOptionBoolean(option)
				if boolErr != nil {
					return boolErr
				}
				merges = value
			default:
				return bindError("42601", "unrecognized ALTER OPERATOR option", 0)
			}
		}
		metadata.Restrict, metadata.Join = restrict, join
		metadata.CanHash, metadata.CanMerge = hashes, merges
		spec.Operator = &metadata
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "ALTER OPERATOR", updates: []ObjectMutation{{Base: base, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

// resolvePlannerFunction resolves a planner support function by name and checks
// its full pinned argument list and result; restrict and join have different
// argument lists, so the caller supplies the exact expected shape.
func resolvePlannerFunction(ctx context.Context, b *binder, snapshot CatalogSnapshot, name string, expected []model.TypeLayout, result model.TypeLayout) (model.FunctionID, error) {
	function, err := definitionSupport(ctx, b, snapshot, name, expected, result)
	if err != nil {
		return 0, err
	}
	return function.id, nil
}

func resolveOperatorDescriptor(ctx context.Context, b *binder, snapshot CatalogSnapshot, object *zpgparse.ObjectWithArgs) (*model.Descriptor, error) {
	if object == nil {
		return nil, bindError("42601", "invalid operator reference", 0)
	}
	parts, err := names(object.Objname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid operator reference", 0)
	}
	if object.ArgsUnspecified || len(object.Objargs) != 2 {
		return nil, bindError("42601", "operator reference requires both argument types", 0)
	}
	var left, right model.TypeLayout
	if argument := object.Objargs[0].GetTypeName(); argument != nil {
		left, err = b.typeName(argument)
		if err != nil {
			return nil, err
		}
		if left.Type.ID != 0 {
			return nil, bindError("42809", "postfix operators are not supported", 0)
		}
	}
	right, err = b.typeName(object.Objargs[1].GetTypeName())
	if err != nil {
		return nil, err
	}
	if right.Type.ID == 0 {
		return nil, bindError("42601", "operator reference requires both argument types", 0)
	}
	signature := operatorSignature(left, right)
	for _, schemaName := range b.functionPath(parts) {
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		published, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectOperator, Name: parts[len(parts)-1], Signature: signature})
		if resolveErr == nil && published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			b.dependency(namespace.Descriptor, 0)
			b.dependency(published.Descriptor, 0)
			return published.Descriptor, nil
		}
		if resolveErr != nil && !isMissingObject(resolveErr) {
			return nil, resolveErr
		}
		if len(parts) == 2 {
			break
		}
	}
	return nil, bindError("42883", "operator does not exist", 0)
}

// ---------------------------------------------------------------------------
// CREATE AGGREGATE
// ---------------------------------------------------------------------------

func bindCreateAggregate(ctx context.Context, statement *zpgparse.DefineStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.IfNotExists {
		return nil, bindError("42601", "invalid CREATE AGGREGATE", 0)
	}
	// An aggregate names a pseudo-type in its argument list (anyelement), its
	// transition state (internal, anyelement), or its result, so the binder
	// admits declaration pseudo-type identities for this statement.
	b, err := declarationBinderDeclaredTypes(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DefineStmt{DefineStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Defnames)
	if err != nil {
		return nil, err
	}
	name, err := definitionName(parts)
	if err != nil {
		return nil, err
	}
	arguments, argumentNames, variadic, direct, hasStar, orderedSet, err := aggregateArguments(ctx, b, statement)
	if err != nil {
		return nil, err
	}
	kind := model.AggregateOrdinary
	if orderedSet {
		kind = model.AggregateOrderedSet
	}
	if statement.Replace {
		// CREATE OR REPLACE AGGREGATE is not a PostgreSQL CREATE AGGREGATE form;
		// the grammar admits it for the ordered-set syntax only via CREATE
		// AGGREGATE, so an explicit replace is a distinct operation.
		return nil, unsupported(AggregateOrReplace, 0)
	}
	schema, err := definitionSchema(ctx, b, snapshot, parts, definitionAggregate)
	if err != nil {
		return nil, err
	}
	signature := aggregateSignature(arguments, orderedSet)
	identity := model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectRoutine, Name: name, Signature: signature}
	if published, conflictErr := snapshot.Resolve(ctx, identity); conflictErr == nil && published.Descriptor != nil {
		return nil, bindError("42723", "function already exists", 0)
	} else if conflictErr != nil && !isMissingObject(conflictErr) {
		return nil, conflictErr
	}
	metadata, err := bindAggregateOptions(ctx, b, snapshot, statement, options, arguments, direct, hasStar, kind)
	if err != nil {
		return nil, err
	}
	metadata.ArgumentNames, metadata.Variadic = normalizeAggregateNames(argumentNames), variadic
	descriptor, err := definitionDescriptor(ctx, b, options, definitionIdentity{}, schema, definitionAggregate, name, signature, func(spec *model.DescriptorSpec) error {
		spec.Aggregate = &metadata
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "CREATE AGGREGATE", creates: []*model.Descriptor{descriptor}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

// aggregateArguments returns the declared argument layouts, the count of
// direct (ordered-set) arguments, and whether the form is the zero-argument
// '*' spelling.
func aggregateArguments(ctx context.Context, b *binder, statement *zpgparse.DefineStmt) (layouts []model.TypeLayout, names []string, variadic model.TypeID, direct uint16, star bool, orderedSet bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, 0, false, false, err
	}
	if statement.Oldstyle {
		// The old-style form declares its base type through the option list.
		return nil, nil, 0, 0, false, false, nil
	}
	if len(statement.Args) != 2 {
		return nil, nil, 0, 0, false, false, bindError("42601", "invalid aggregate argument list", 0)
	}
	if list := statement.Args[0].GetList(); list != nil {
		for _, node := range list.Items {
			parameter := node.GetFunctionParameter()
			if parameter == nil || parameter.ArgType == nil {
				return nil, nil, 0, 0, false, false, bindError("42601", "invalid aggregate argument", 0)
			}
			layout, typeErr := b.typeName(parameter.ArgType)
			if typeErr != nil {
				return nil, nil, 0, 0, false, false, typeErr
			}
			switch routineParameterMode(parameter.Mode) {
			case "in":
			case "variadic":
				// PostgreSQL permits VARIADIC on the final argument only. The
				// declared layout stays as written, while pg_proc.provariadic
				// records its element: the array's element for a concrete type,
				// and the matching element pseudo-type for anyarray,
				// anycompatiblearray, and anycompatiblemultirange.
				if node != list.Items[len(list.Items)-1] {
					return nil, nil, 0, 0, false, false, bindError("42P13", "VARIADIC argument must be last", int(parameter.Location)+1)
				}
				element, ok := aggregateVariadicElement(layout)
				if !ok {
					return nil, nil, 0, 0, false, false, bindError("42P13", "VARIADIC argument must be an array type", int(parameter.Location)+1)
				}
				variadic = element
			default:
				// Aggregate arguments are inputs; OUT/TABLE/INOUT have no meaning
				// in a call signature and are rejected rather than dropped.
				return nil, nil, 0, 0, false, false, bindError("42P13", "aggregate argument mode must be IN or VARIADIC", int(parameter.Location)+1)
			}
			names = append(names, parameter.Name)
			layouts = append(layouts, layout)
		}
	} else if statement.Args[0] != nil {
		return nil, nil, 0, 0, false, false, bindError("42601", "invalid aggregate argument list", 0)
	} else {
		// The zero-argument '*' spelling omits the parameter list entirely.
		star = true
	}
	integer := statement.Args[1].GetInteger()
	if integer == nil {
		return nil, nil, 0, 0, false, false, bindError("42601", "invalid aggregate argument list", 0)
	}
	value := int64(integer.Ival)
	if value < 0 {
		// The grammar marks the plain (non-ORDER BY) forms with a negative count.
		return layouts, names, variadic, 0, star, false, nil
	}
	if value > int64(len(layouts)) {
		return nil, nil, 0, 0, false, false, bindError("42883", "aggregate direct argument count exceeds its argument list", 0)
	}
	// A nonnegative count means the ORDER BY form was used, including the
	// zero-direct-argument spelling.
	return layouts, names, variadic, uint16(value), star, true, nil
}

// normalizeAggregateNames drops an all-empty name list so an unnamed declaration
// is distinguishable from a named one. PostgreSQL omits proargnames entirely when
// no argument is named, while a single named argument makes every position
// significant (including the empty names before it).
func normalizeAggregateNames(names []string) []string {
	for _, name := range names {
		if name != "" {
			return names
		}
	}
	return nil
}

// aggregateSignature keys an aggregate in the shared pg_proc namespace using the
// same input-type encoding as routines.
func aggregateSignature(arguments []model.TypeLayout, orderedSet bool) string {
	if len(arguments) == 0 {
		// The zero-argument form is keyed by PostgreSQL's "*" spelling.
		return "*"
	}
	parameters := make([]model.RoutineParameter, len(arguments))
	for i := range arguments {
		parameters[i] = model.RoutineParameter{Mode: "in", Layout: arguments[i]}
	}
	return routineSignature(parameters)
}

// ---------------------------------------------------------------------------
// Aggregate option binding
// ---------------------------------------------------------------------------

// aggregateStateLayout resolves the transition state layout. PostgreSQL's
// `internal` state has no SQL-level representation; the executor carries it as
// a bounded byte payload, so the persisted layout is the byte form.
func aggregateStateLayout(layout model.TypeLayout) model.TypeLayout {
	if layout.Type.ID == 2281 {
		return model.TypeLayout{Type: model.Type{ID: 2281, Modifier: -1, Storage: model.TypeBytes}}
	}
	return layout
}

func bindAggregateOptions(ctx context.Context, b *binder, snapshot CatalogSnapshot, statement *zpgparse.DefineStmt, options CreateDeclarationOptions, arguments []model.TypeLayout, direct uint16, star bool, kind model.AggregateKind) (model.AggregateMetadata, error) {
	metadata := model.AggregateMetadata{
		Owner: options.Owner, Kind: kind, Arguments: arguments, DirectArguments: direct,
		Volatility: "immutable", Parallel: "unsafe",
		FinalModify: model.AggregateFinalReadOnly,
	}
	if statement.Oldstyle {
		if len(arguments) != 0 {
			return metadata, bindError("42601", "old-style aggregate declares its base type through options", 0)
		}
	}
	values := make(map[string]*zpgparse.DefElem, len(statement.Definition))
	for _, node := range statement.Definition {
		option := node.GetDefElem()
		if option == nil {
			return metadata, bindError("42601", "invalid aggregate option", 0)
		}
		key := strings.ToLower(option.Defname)
		switch key {
		case "sfunc", "stype", "sspace", "finalfunc", "finalfunc_extra", "finalfunc_modify",
			"combinefunc", "serialfunc", "deserialfunc", "initcond", "msfunc", "minvfunc",
			"mstype", "msspace", "minitcond", "mfinalfunc", "mfinalfunc_extra", "mfinalfunc_modify",
			"sortop", "parallel", "hypothetical",
			"sfunc1", "basetype", "stype1", "initcond1":
		default:
			return metadata, bindError("42601", "unrecognized aggregate parameter", 0)
		}
		if _, duplicate := values[key]; duplicate {
			return metadata, bindError("42601", "aggregate parameter is specified twice", 0)
		}
		values[key] = option
	}
	// The old-style spellings are exact aliases of the modern ones.
	aliases := [][2]string{{"sfunc", "sfunc1"}, {"stype", "stype1"}, {"initcond", "initcond1"}}
	for _, alias := range aliases {
		modern, legacy := values[alias[0]], values[alias[1]]
		if modern != nil && legacy != nil {
			return metadata, bindError("42601", "aggregate parameter is specified twice", 0)
		}
		if legacy != nil {
			values[alias[0]] = legacy
			delete(values, alias[1])
		}
	}
	baseType := values["basetype"]
	switch {
	case star:
		metadata.Arguments = nil
	case baseType != nil:
		name := aggregateOptionText(baseType)
		if strings.EqualFold(name, "any") {
			// "any" is a genuine pseudo-type input marker, not a concrete type.
			metadata.Arguments = []model.TypeLayout{{Type: model.Type{ID: 2276, Modifier: -1, Storage: model.TypeAny}}}
		} else {
			layout, err := aggregateOptionType(b, baseType)
			if err != nil {
				return metadata, err
			}
			if layout.Type.ID == 0 {
				return metadata, bindError("42704", "aggregate base type does not exist", 0)
			}
			metadata.Arguments = []model.TypeLayout{layout}
		}
	}
	if len(metadata.Arguments) == 0 && !star {
		return metadata, bindError("42P13", "aggregate input type is missing", 0)
	}
	stateOption := values["stype"]
	if stateOption == nil {
		return metadata, bindError("42P13", "aggregate transition state type is missing", 0)
	}
	state, err := aggregateOptionType(b, stateOption)
	if err != nil {
		return metadata, err
	}
	if state.Type.ID == 0 {
		return metadata, bindError("42704", "aggregate transition state type does not exist", 0)
	}
	// The transition state keeps PostgreSQL's aggtranstype semantics. A state
	// declared over a pseudo-type is recorded by its DECLARED identity, which is
	// what pg_proc stores and what the corpus reports; it is instantiated from a
	// call's actual argument layouts when one is bound. A concrete instantiation
	// is recorded directly, so a declaration whose own arguments already fix the
	// state (the ordinary case) stays exactly as it was.
	switch state.Type.ID {
	case 2281:
		// internal keeps its declared identity for support-signature matching,
		// which is what ordered_set_transition and percentile_disc_final declare;
		// the persisted layout carries the executor's bounded byte payload, which
		// aggregateStateLayout below supplies.
	case 2283, 5077: // anyelement, anycompatible: the polymorphic input type.
		if resolved, ok := polymorphicInputElement(arguments); ok {
			state = resolved
		}
	case 2277, 5078: // anyarray, anycompatiblearray: its array type.
		if resolved, ok := polymorphicInputElement(arguments); ok {
			array, arrayErr := b.polymorphicArrayType(resolved.Type.ID, 0)
			if arrayErr != nil {
				return metadata, arrayErr
			}
			state = b.primitive(array)
		}
	default:
		if state.Type.Storage == model.TypeAny {
			return metadata, bindError("42P13", "aggregate transition state must have a storable type", 0)
		}
	}
	// The persisted layout keeps `internal`'s bounded byte payload, which is the
	// executor's state representation; the declared identity is retained
	// separately for support-signature matching above.
	metadata.TransitionLayout = aggregateStateLayout(state)
	// The transition function receives the state and the aggregated (ORDER BY)
	// arguments; an ordered-set declaration's direct arguments reach the final
	// function instead, never the transition. ordered_set_transition(internal,
	// anyelement) is that shape for percentile_disc(float8 ORDER BY anyelement).
	transitionArguments := []model.TypeLayout{metadata.TransitionLayout}
	transitionArguments = append(transitionArguments, metadata.Arguments[min(int(direct), len(metadata.Arguments)):]...)
	if transition := values["sfunc"]; transition != nil {
		function, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(transition), transitionArguments, state)
		if err != nil {
			return metadata, err
		}
		metadata.Transition = function.id
	} else {
		return metadata, bindError("42P13", "aggregate transition function is missing", 0)
	}
	if option := values["sspace"]; option != nil {
		space, err := aggregateOptionInt(option)
		if err != nil || space < 0 {
			return metadata, bindError("22023", "SSPACE must be a nonnegative integer", 0)
		}
		metadata.TransitionSpace = int32(space)
	}
	// The result type is the final function's result when the declaration has
	// one, otherwise the transition state type itself (pg_proc.prorettype).
	metadata.Result = metadata.TransitionLayout
	if final := values["finalfunc"]; final != nil {
		// PostgreSQL passes the transition state, the direct arguments, then a
		// NULL placeholder for each aggregated argument. FINALFUNC_EXTRA only
		// decides whether those trailing arguments are supplied at all:
		// percentile_disc_final(internal, float8, anyelement) is exactly
		// (state, direct fraction, aggregated placeholder). The trailing
		// placeholders are untyped, so the resolved signature is what supplies
		// their types.
		finalArguments := []model.TypeLayout{state}
		if values["finalfunc_extra"] != nil && aggregateOptionBool(values["finalfunc_extra"]) {
			metadata.FinalExtra = true
			finalArguments = append(finalArguments, metadata.Arguments...)
		}
		function, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(final), finalArguments, model.TypeLayout{})
		if err != nil {
			return metadata, err
		}
		if function.layout.Type.ID == 0 {
			return metadata, bindError("42P13", "final function result type is unresolved", 0)
		}
		metadata.Final = function.id
		metadata.Result = function.layout
	} else if values["finalfunc_extra"] != nil {
		return metadata, bindError("42P13", "FINALFUNC_EXTRA requires a final function", 0)
	}
	if modify := values["finalfunc_modify"]; modify != nil {
		parsed, err := aggregateFinalModify(aggregateOptionText(modify))
		if err != nil {
			return metadata, err
		}
		metadata.FinalModify = parsed
	}
	if combine := values["combinefunc"]; combine != nil {
		function, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(combine), []model.TypeLayout{metadata.TransitionLayout, metadata.TransitionLayout}, state)
		if err != nil {
			return metadata, err
		}
		metadata.Combine = function.id
	}
	serial, deserial := values["serialfunc"], values["deserialfunc"]
	if (serial == nil) != (deserial == nil) {
		return metadata, bindError("42P13", "serialfunc and deserialfunc must be specified together", 0)
	}
	if serial != nil {
		serialFunction, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(serial), []model.TypeLayout{state}, b.primitive(17))
		if err != nil {
			return metadata, err
		}
		deserialFunction, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(deserial), []model.TypeLayout{b.primitive(17)}, state)
		if err != nil {
			return metadata, err
		}
		metadata.Serial, metadata.Deserial = serialFunction.id, deserialFunction.id
	}
	if initial := values["initcond"]; initial != nil {
		metadata.InitialValue = aggregateOptionText(initial)
		metadata.HasInitialValue = true
	}
	// Moving-aggregate options must be complete or absent.
	movingTransition, movingInverse, movingState := values["msfunc"], values["minvfunc"], values["mstype"]
	present := 0
	for _, option := range []*zpgparse.DefElem{movingTransition, movingInverse, movingState} {
		if option != nil {
			present++
		}
	}
	if present != 0 && present != 3 {
		return metadata, bindError("42P13", "MSFUNC, MINVFUNC and MSTYPE must be specified together", 0)
	}
	if present == 3 {
		movingLayout, err := aggregateOptionType(b, movingState)
		if err != nil {
			return metadata, err
		}
		if movingLayout.Type.ID == 0 {
			return metadata, bindError("42704", "aggregate moving state type does not exist", 0)
		}
		metadata.MovingTransitionLayout = aggregateStateLayout(movingLayout)
		if metadata.MovingTransitionLayout.Type.ID != metadata.TransitionLayout.Type.ID {
			return metadata, bindError("42P13", "MSTYPE must match the aggregate transition state type", 0)
		}
		// MSFUNC receives the moving state and the aggregated arguments, exactly
		// as SFUNC does; direct arguments never reach a transition function.
		movingArguments := []model.TypeLayout{metadata.MovingTransitionLayout}
		movingArguments = append(movingArguments, metadata.Arguments[min(int(direct), len(metadata.Arguments)):]...)
		forward, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(movingTransition), movingArguments, movingLayout)
		if err != nil {
			return metadata, err
		}
		inverse, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(movingInverse), []model.TypeLayout{metadata.MovingTransitionLayout}, movingLayout)
		if err != nil {
			return metadata, err
		}
		metadata.MovingTransition, metadata.MovingInverse = forward.id, inverse.id
		if option := values["msspace"]; option != nil {
			space, err := aggregateOptionInt(option)
			if err != nil || space < 0 {
				return metadata, bindError("22023", "MSSPACE must be a nonnegative integer", 0)
			}
			metadata.MovingTransitionSpace = int32(space)
		}
		if option := values["minitcond"]; option != nil {
			metadata.MovingInitialValue = aggregateOptionText(option)
			metadata.HasMovingInitialValue = true
		}
	} else if values["msspace"] != nil || values["minitcond"] != nil {
		return metadata, bindError("42P13", "MSSPACE and MINITCOND require moving-aggregate functions", 0)
	}
	// The moving final function returns the aggregate's result type, which is
	// already fixed by the ordinary final function (or the transition state), so
	// the two must agree.
	if movingFinal := values["mfinalfunc"]; movingFinal != nil {
		if present != 3 {
			return metadata, bindError("42P13", "MFINALFUNC requires moving-aggregate functions", 0)
		}
		movingArguments := []model.TypeLayout{metadata.MovingTransitionLayout}
		if values["mfinalfunc_extra"] != nil && aggregateOptionBool(values["mfinalfunc_extra"]) {
			metadata.MovingFinalExtra = true
			// MFINALFUNC_EXTRA receives the direct arguments followed by a
			// placeholder for each aggregated argument, matching FINALFUNC_EXTRA.
			movingArguments = append(movingArguments, metadata.Arguments...)
		}
		function, err := definitionSupport(ctx, b, snapshot, aggregateOptionText(movingFinal), movingArguments, model.TypeLayout{})
		if err != nil {
			return metadata, err
		}
		if !supportResultMatches(metadata.Result, function.layout) {
			return metadata, bindError("42P13", "moving final function must return the aggregate result type", 0)
		}
		metadata.MovingFinal = function.id
		// PostgreSQL defaults MFINALFUNC_MODIFY to READ_ONLY.
		metadata.MovingFinalModify = model.AggregateFinalReadOnly
		if modify := values["mfinalfunc_modify"]; modify != nil {
			parsed, err := aggregateFinalModify(aggregateOptionText(modify))
			if err != nil {
				return metadata, err
			}
			metadata.MovingFinalModify = parsed
		}
	} else if values["mfinalfunc_extra"] != nil || values["mfinalfunc_modify"] != nil {
		return metadata, bindError("42P13", "MFINALFUNC_EXTRA and MFINALFUNC_MODIFY require a moving final function", 0)
	}
	if sortop := values["sortop"]; sortop != nil {
		name := aggregateOptionText(sortop)
		left, right := model.TypeLayout{}, model.TypeLayout{}
		if !star {
			left = metadata.Arguments[0]
			if len(metadata.Arguments) == 2 {
				right = metadata.Arguments[1]
			} else {
				right = metadata.Arguments[0]
			}
		}
		operator, err := resolveOperatorReference(ctx, b, snapshot, name, left, right)
		if err != nil {
			return metadata, err
		}
		metadata.SortOperator = operator
	}
	if parallel := values["parallel"]; parallel != nil {
		value := strings.ToLower(aggregateOptionText(parallel))
		if value != "unsafe" && value != "restricted" && value != "safe" {
			return metadata, bindError("22023", "invalid PARALLEL option", 0)
		}
		metadata.Parallel = value
	}
	if hypothetical := values["hypothetical"]; hypothetical != nil {
		if !aggregateOptionBool(hypothetical) {
			return metadata, bindError("42P13", "HYPOTHETICAL must be true", 0)
		}
		if kind != model.AggregateOrderedSet {
			return metadata, bindError("42P13", "HYPOTHETICAL requires the ordered-set form", 0)
		}
		if int(direct)*2 != len(metadata.Arguments) {
			return metadata, bindError("42883", "hypothetical-set direct and ORDER BY argument counts must match", 0)
		}
		metadata.Kind = model.AggregateHypotheticalSet
	}
	// PostgreSQL marks an ordered-set aggregate's ordering state as internal.
	return metadata, ctx.Err()
}

// polymorphicInputElement resolves a polymorphic state declared over
// anyelement/anycompatible to the concrete element of the declaration's own
// arguments. When an argument is itself still an unresolved pseudo-type marker,
// the declaration has no concrete element to record, so the state keeps its
// declared identity and the call resolves it later.
func polymorphicInputElement(arguments []model.TypeLayout) (model.TypeLayout, bool) {
	if len(arguments) == 0 || arguments[0].Type.Storage == model.TypeAny {
		return model.TypeLayout{}, false
	}
	return arguments[0], true
}

// aggregateVariadicElement reports the element type PostgreSQL records in
// pg_proc.provariadic for a VARIADIC aggregate argument. A concrete declaration
// records the array's element; the polymorphic array pseudo-types record their
// corresponding element pseudo-type, which is how `VARIADIC anyarray` reports
// anyelement and `VARIADIC anycompatiblearray` reports anycompatible. `any`
// stays itself, because PostgreSQL keeps provariadic = any for that spelling.
func aggregateVariadicElement(layout model.TypeLayout) (model.TypeID, bool) {
	if layout.Type.ID == 2276 {
		return 2276, true
	}
	if element, ok := model.PseudoTypeArrayElement(layout.Type.ID); ok {
		return element, true
	}
	if layout.Type.Storage == model.TypeArray && len(layout.Children) == 1 {
		return layout.Children[0].Type.ID, true
	}
	return 0, false
}

func aggregateOptionType(b *binder, option *zpgparse.DefElem) (model.TypeLayout, error) {
	if option == nil || option.Arg == nil {
		return model.TypeLayout{}, bindError("42601", "aggregate option requires a value", 0)
	}
	if value := option.Arg.GetString_(); value != nil {
		// A bare (unquoted or quoted) name resolves through the search path.
		return b.typeNameByName(value.Sval)
	}
	if name := option.Arg.GetTypeName(); name != nil {
		return b.typeName(name)
	}
	if list := option.Arg.GetList(); list != nil && len(list.Items) == 1 {
		if value := list.Items[0].GetString_(); value != nil {
			return b.typeNameByName(value.Sval)
		}
	}
	return model.TypeLayout{}, bindError("42601", "aggregate option requires a type name", 0)
}

// aggregateOptionText extracts the routine or operator name an aggregate option
// names. PostgreSQL's grammar routes these through def_arg, whose func_type
// alternative arrives here as a TypeName node carrying a possibly qualified
// name; a quoted literal arrives as a plain string. GRAMMAR NOTE: this is the
// reason sfunc/finalfunc/combinefunc must not be read with GetString_ alone --
// the TypeName form is the normal one for an unquoted function name.
func aggregateOptionText(option *zpgparse.DefElem) string {
	if option == nil || option.Arg == nil {
		return ""
	}
	if value := option.Arg.GetString_(); value != nil {
		return value.Sval
	}
	if name := option.Arg.GetTypeName(); name != nil {
		parts, err := names(name.Names)
		if err != nil {
			return ""
		}
		return strings.Join(parts, ".")
	}
	if list := option.Arg.GetList(); list != nil {
		parts := make([]string, 0, len(list.Items))
		for _, item := range list.Items {
			value := item.GetString_()
			if value == nil {
				return ""
			}
			parts = append(parts, value.Sval)
		}
		return strings.Join(parts, ".")
	}
	return ""
}

func aggregateOptionBool(option *zpgparse.DefElem) bool {
	if option == nil || option.Arg == nil {
		return false
	}
	if value := option.Arg.GetBoolean(); value != nil {
		return value.Boolval
	}
	return strings.EqualFold(aggregateOptionText(option), "true")
}

func aggregateOptionInt(option *zpgparse.DefElem) (int64, error) {
	if option == nil || option.Arg == nil {
		return 0, model.ErrInvalidDescriptor
	}
	if integer := option.Arg.GetInteger(); integer != nil {
		return int64(integer.Ival), nil
	}
	text := aggregateOptionText(option)
	if text == "" {
		return 0, model.ErrInvalidDescriptor
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, model.ErrInvalidDescriptor
	}
	return value, nil
}

func aggregateFinalModify(value string) (model.AggregateFinalModify, error) {
	switch strings.ToLower(value) {
	case "read_only":
		return model.AggregateFinalReadOnly, nil
	case "shareable":
		return model.AggregateFinalShareable, nil
	case "read_write":
		return model.AggregateFinalReadWrite, nil
	default:
		return 0, bindError("22023", "invalid FINALFUNC_MODIFY option", 0)
	}
}

// typeNameByName resolves an unqualified or qualified type name string.
func (b *binder) typeNameByName(name string) (model.TypeLayout, error) {
	parts := strings.Split(name, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return model.TypeLayout{}, bindError("42704", "type does not exist", 0)
	}
	nodes := make([]*zpgparse.Node, len(parts))
	for i := range parts {
		nodes[i] = &zpgparse.Node{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: parts[i]}}}
	}
	return b.typeName(&zpgparse.TypeName{Names: nodes})
}

// ---------------------------------------------------------------------------
// Definition lifecycle
// ---------------------------------------------------------------------------

func bindDropDefinition(ctx context.Context, statement *zpgparse.DropStmt, kind definitionKind, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Concurrent {
		return nil, bindError("42601", "invalid DROP "+kind.label(), 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := &BoundObjectCommand{tag: "DROP " + kind.label(), cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE}
	seen := make(map[model.ObjectID]bool)
	for _, node := range statement.Objects {
		descriptor, resolveErr := resolveDefinitionDescriptor(ctx, b, snapshot, node, kind)
		if resolveErr != nil {
			var diagnostic *model.Error
			if statement.MissingOk && errors.As(resolveErr, &diagnostic) && (diagnostic.Code == "42704" || diagnostic.Code == "42883") {
				command.warnings = append(command.warnings, model.Error{Code: diagnostic.Code, Message: kind.label() + " does not exist, skipping"})
				continue
			}
			return nil, resolveErr
		}
		if seen[descriptor.ID()] {
			continue
		}
		seen[descriptor.ID()] = true
		command.drops = append(command.drops, descriptor)
	}
	command.dependencies = slices.Clone(b.dependencies)
	command.warnings = append(command.warnings, b.warnings...)
	return command, ctx.Err()
}

func bindRenameDefinition(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newname == "" || statement.RenameType != zpgparse.ObjectType_OBJECT_AGGREGATE {
		return nil, bindError("42601", "invalid aggregate rename", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	base, err := resolveAggregateReference(ctx, b, snapshot, statement.Object.GetObjectWithArgs())
	if err != nil {
		return nil, err
	}
	next, err := definitionMutation(ctx, b, base, options, nil, func(spec *model.DescriptorSpec) error {
		identity := spec.Name
		identity.Name = statement.Newname
		if published, conflictErr := snapshot.Resolve(ctx, identity); conflictErr == nil && published.Descriptor.ID() != base.ID() {
			return bindError("42723", "function already exists", 0)
		} else if conflictErr != nil && !isMissingObject(conflictErr) {
			return conflictErr
		}
		spec.Name = identity
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "ALTER AGGREGATE", updates: []ObjectMutation{{Base: base, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func bindSetDefinitionSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, kind definitionKind, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid ALTER "+kind.label()+" SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveDefinitionDescriptor(ctx, b, snapshot, statement.Object, kind)
	if err != nil {
		return nil, err
	}
	destination, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	b.dependency(destination.Descriptor, 0)
	oldParent := base.Name().Parent
	next, err := definitionMutation(ctx, b, base, options, []model.ObjectID{oldParent}, func(spec *model.DescriptorSpec) error {
		spec.Name.Parent = destination.Descriptor.ID()
		if published, conflictErr := snapshot.Resolve(ctx, spec.Name); conflictErr == nil && published.Descriptor.ID() != base.ID() {
			return bindError("42710", "object already exists in destination schema", 0)
		} else if conflictErr != nil && !isMissingObject(conflictErr) {
			return conflictErr
		}
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			return dependency.Object == oldParent && dependency.Column == 0
		})
		spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: destination.Descriptor.ID(), Version: destination.Descriptor.Version()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "ALTER " + kind.label(), updates: []ObjectMutation{{Base: base, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func bindCommentDefinition(ctx context.Context, statement *zpgparse.CommentStmt, kind definitionKind, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Object == nil {
		return nil, bindError("42601", "invalid COMMENT ON "+kind.label(), 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CommentStmt{CommentStmt: statement}})
	if err != nil {
		return nil, err
	}
	base, err := resolveDefinitionDescriptor(ctx, b, snapshot, statement.Object, kind)
	if err != nil {
		return nil, err
	}
	next, err := definitionMutation(ctx, b, base, options, nil, func(spec *model.DescriptorSpec) error {
		spec.Description = statement.Comment
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{tag: "COMMENT", updates: []ObjectMutation{{Base: base, Next: next}}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

// resolveDefinitionDescriptor resolves any definition reference node to its
// descriptor for the given family.
func resolveDefinitionDescriptor(ctx context.Context, b *binder, snapshot CatalogSnapshot, node *zpgparse.Node, kind definitionKind) (*model.Descriptor, error) {
	switch kind {
	case definitionCast:
		return resolveCastReference(ctx, b, snapshot, node)
	case definitionOperator:
		return resolveOperatorDescriptor(ctx, b, snapshot, node.GetObjectWithArgs())
	default:
		return resolveAggregateReference(ctx, b, snapshot, node.GetObjectWithArgs())
	}
}

// resolveCastReference resolves COMMENT ON CAST (src AS tgt) and DROP CAST.
func resolveCastReference(ctx context.Context, b *binder, snapshot CatalogSnapshot, node *zpgparse.Node) (*model.Descriptor, error) {
	if node == nil {
		return nil, bindError("42601", "invalid cast reference", 0)
	}
	items := node.GetList()
	if items == nil || len(items.Items) != 2 {
		return nil, bindError("42601", "cast reference requires source and target types", 0)
	}
	source, err := b.typeName(items.Items[0].GetTypeName())
	if err != nil {
		return nil, err
	}
	target, err := b.typeName(items.Items[1].GetTypeName())
	if err != nil {
		return nil, err
	}
	schema, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: "pg_catalog"})
	if err != nil {
		return nil, errors.Join(bindError("3F000", "pg_catalog schema is unavailable", 0), err)
	}
	published, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: schema.Descriptor.ID(), Kind: model.ObjectCast, Name: castDescriptorName(source.Type.ID, target.Type.ID)})
	if resolveErr != nil {
		if isMissingObject(resolveErr) {
			return nil, bindError("42704", "cast from type to type does not exist", 0)
		}
		return nil, resolveErr
	}
	b.dependency(schema.Descriptor, 0)
	b.dependency(published.Descriptor, 0)
	return published.Descriptor, nil
}

// resolveAggregateReference resolves an aggregate reference by name and input
// types within the shared pg_proc namespace.
func resolveAggregateReference(ctx context.Context, b *binder, snapshot CatalogSnapshot, object *zpgparse.ObjectWithArgs) (*model.Descriptor, error) {
	if object == nil {
		return nil, bindError("42601", "invalid aggregate reference", 0)
	}
	parts, err := names(object.Objname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid aggregate reference", 0)
	}
	name := parts[len(parts)-1]
	if object.ArgsUnspecified {
		return nil, bindError("42883", "aggregate does not exist", 0)
	}
	var arguments []model.TypeLayout
	for _, argument := range object.Objargs {
		layout, err := b.typeName(argument.GetTypeName())
		if err != nil {
			return nil, err
		}
		if layout.Type.ID == 0 {
			return nil, bindError("42883", "aggregate does not exist", 0)
		}
		arguments = append(arguments, layout)
	}
	signature := routineInputsSignature(arguments)
	for _, schemaName := range b.functionPath(parts) {
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		published, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: model.ObjectRoutine, Name: name, Signature: signature})
		if resolveErr == nil && published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			spec, specErr := b.descriptorSpec(published.Descriptor)
			if specErr != nil {
				return nil, specErr
			}
			if spec.Aggregate == nil {
				return nil, bindError("42809", "object is not an aggregate", 0)
			}
			b.dependency(namespace.Descriptor, 0)
			b.dependency(published.Descriptor, 0)
			return published.Descriptor, nil
		}
		if resolveErr != nil && !isMissingObject(resolveErr) {
			return nil, resolveErr
		}
		if len(parts) == 2 {
			break
		}
	}
	return nil, bindError("42883", "aggregate does not exist", 0)
}

// routineInputsSignature encodes input types the way routineSignature does,
// without requiring RoutineParameter values.
func routineInputsSignature(arguments []model.TypeLayout) string {
	parts := make([]string, len(arguments))
	for i := range arguments {
		parts[i] = strconv.FormatUint(uint64(arguments[i].Type.ID), 10) + ":" + strconv.FormatInt(int64(arguments[i].Type.Modifier), 10)
	}
	return strings.Join(parts, ",")
}
