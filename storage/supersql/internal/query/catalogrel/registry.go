package catalogrel

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"iter"
	"math"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

const (
	IDPgAggregate RelationID = 1
	IDPgAm RelationID = 2
	IDPgAmop RelationID = 3
	IDPgAmproc RelationID = 4
	IDPgAttrdef RelationID = 5
	IDPgAttribute RelationID = 6
	IDPgCast RelationID = 7
	IDPgClass RelationID = 8
	IDPgCollation RelationID = 9
	IDPgConstraint RelationID = 10
	IDPgDatabase RelationID = 11
	IDPgDepend RelationID = 12
	IDPgDescription RelationID = 13
	IDPgEnum RelationID = 14
	IDPgIndex RelationID = 15
	IDPgIndexes RelationID = 16
	IDPgInherits RelationID = 17
	IDPgLanguage RelationID = 18
	IDPgMatviews RelationID = 19
	IDPgNamespace RelationID = 20
	IDPgOpclass RelationID = 21
	IDPgOperator RelationID = 22
	IDPgOpfamily RelationID = 23
	IDPgPartitionedTable RelationID = 24
	IDPgProc RelationID = 25
	IDPgRange RelationID = 26
	IDPgRoles RelationID = 27
	IDPgSequence RelationID = 28
	IDPgSequences RelationID = 29
	IDPgSettings RelationID = 30
	IDPgStatistic RelationID = 31
	IDPgStatisticExt RelationID = 32
	IDPgStatisticExtData RelationID = 33
	IDPgStats RelationID = 34
	IDPgTables RelationID = 35
	IDPgTrigger RelationID = 36
	IDPgType RelationID = 37
	IDPgUser RelationID = 38
	IDPgViews RelationID = 39
	IDAttributes RelationID = 40
	IDCharacterSets RelationID = 41
	IDCheckConstraintRoutineUsage RelationID = 42
	IDCheckConstraints RelationID = 43
	IDCollationCharacterSetApplicability RelationID = 44
	IDCollations RelationID = 45
	IDColumnColumnUsage RelationID = 46
	IDColumnDomainUsage RelationID = 47
	IDColumnUdtUsage RelationID = 48
	IDColumns RelationID = 49
	IDConstraintColumnUsage RelationID = 50
	IDConstraintTableUsage RelationID = 51
	IDDomainConstraints RelationID = 52
	IDDomainUdtUsage RelationID = 53
	IDDomains RelationID = 54
	IDElementTypes RelationID = 55
	IDInformationSchemaCatalogName RelationID = 56
	IDKeyColumnUsage RelationID = 57
	IDParameters RelationID = 58
	IDReferentialConstraints RelationID = 59
	IDRoutineColumnUsage RelationID = 60
	IDRoutineRoutineUsage RelationID = 61
	IDRoutineSequenceUsage RelationID = 62
	IDRoutineTableUsage RelationID = 63
	IDRoutines RelationID = 64
	IDSchemata RelationID = 65
	IDSequences RelationID = 66
	IDTableConstraints RelationID = 67
	IDTables RelationID = 68
	IDTriggeredUpdateColumns RelationID = 69
	IDTriggers RelationID = 70
	IDUserDefinedTypes RelationID = 71
	IDViewColumnUsage RelationID = 72
	IDViewRoutineUsage RelationID = 73
	IDViewTableUsage RelationID = 74
	IDViews RelationID = 75
	IDSqlFeatures RelationID = 76
	IDSqlImplementationInfo RelationID = 77
	IDSqlParts RelationID = 78
	IDSqlSizing RelationID = 79
	IDPgTSConfig RelationID = 80
	IDPgTSConfigMap RelationID = 81
	IDPgTSDict RelationID = 82
	IDPgTSParser RelationID = 83
	IDPgTSTemplate RelationID = 84
)

type DeclarationKind uint8

const (
	DeclarationView DeclarationKind = iota + 1
	DeclarationTable
	DeclarationDomain
	DeclarationFunction
)

type Declaration struct {
	Name string
	Kind DeclarationKind
}

var registry = buildRegistry()

var registryByName = func() map[string]*Relation {
	byName := make(map[string]*Relation, len(registry))
	for i := range registry {
		relation := &registry[i]
		byName[relation.Namespace+"."+relation.Name] = relation
	}
	return byName
}()

var registryByID = func() map[RelationID]int {
	index := make(map[RelationID]int, len(registry))
	for i := range registry {
		index[registry[i].ID] = i
	}
	return index
}()

func Lookup(namespace, name string) (Relation, bool) {
	relation, ok := registryByName[namespace+"."+name]
	if !ok {
		return Relation{}, false
	}
	return relation.Clone(), true
}

func LookupID(id RelationID) (Relation, bool) {
	index, ok := registryByID[id]
	if !ok {
		return Relation{}, false
	}
	return registry[index].Clone(), true
}

func All() []Relation {
	relations := make([]Relation, len(registry))
	for i := range registry {
		relations[i] = registry[i].Clone()
	}
	return relations
}

type registryType struct {
	relation int
	array    bool
	domain   bool
}

type Registry struct {
	relations                    []Relation
	informationDomains           []RegisteredInformationDomain
	informationFunctions         []builtin.InformationSchemaFunctionBinding
	byName                       map[string]int
	byRelationOID                map[model.OID]int
	byTypeName                   map[string]registryType
	typesByID                    map[model.TypeID]registryType
	arraysByElement              map[model.TypeID]model.TypeID
	informationFunctionsByID     map[model.FunctionID]builtin.InformationSchemaFunctionBinding
	informationFunctionsByName   map[string][]builtin.Signature
	informationFunctionFingerprint [sha256.Size]byte
	informationTypeFingerprint [sha256.Size]byte
	informationSchema            NamespaceIdentity
	identityComplete               bool
	sealed                         bool
	registryVersion               RegistryVersion
}

const NativeViewSemanticVersion uint32 = 1
func writeFingerprintUint64(out hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = out.Write(encoded[:])
}

func writeFingerprintString(out hash.Hash, value string) {
	writeFingerprintUint64(out, uint64(len(value)))
	_, _ = out.Write([]byte(value))
}

func writeFingerprintBool(out hash.Hash, value bool) {
	if value {
		writeFingerprintUint64(out, 1)
		return
	}
	writeFingerprintUint64(out, 0)
}

func writeFingerprintLayout(out hash.Hash, layout model.TypeLayout) {
	writeFingerprintUint64(out, uint64(layout.Type.ID))
	writeFingerprintUint64(out, uint64(uint32(layout.Type.Modifier)))
	writeFingerprintUint64(out, uint64(layout.Type.Collation))
	writeFingerprintUint64(out, uint64(layout.Type.Storage))
	writeFingerprintUint64(out, uint64(len(layout.FieldNames)))
	for _, name := range layout.FieldNames {
		writeFingerprintString(out, name)
	}
	writeFingerprintUint64(out, uint64(len(layout.Children)))
	for _, child := range layout.Children {
		writeFingerprintLayout(out, child)
	}
}

func writeFingerprintSignature(out hash.Hash, signature builtin.Signature) {
	writeFingerprintUint64(out, uint64(signature.ID))
	writeFingerprintString(out, signature.Name)
	for _, argument := range signature.Arguments {
		writeFingerprintUint64(out, uint64(argument))
	}
	for _, value := range [...]uint64{
		uint64(signature.ArgumentCount), uint64(signature.Result), uint64(signature.Variadic),
		uint64(signature.Volatility), uint64(signature.Kind), uint64(signature.Language),
		math.Float64bits(signature.Cost), math.Float64bits(signature.Rows), uint64(signature.Support),
		uint64(signature.Parallel), uint64(signature.AggregateKind), uint64(signature.AggregateDirectArguments),
		uint64(signature.AggregateTransition), uint64(signature.AggregateFinal), uint64(signature.AggregateCombine),
		uint64(signature.AggregateSerial), uint64(signature.AggregateDeserial), uint64(signature.AggregateMovingTransition),
		uint64(signature.AggregateMovingInverse), uint64(signature.AggregateMovingFinal),
		uint64(signature.AggregateFinalModify), uint64(signature.AggregateMovingFinalModify),
		uint64(signature.AggregateSortOperator), uint64(signature.AggregateTransitionType),
		uint64(uint32(signature.AggregateTransitionSpace)), uint64(signature.AggregateMovingTransitionType),
		uint64(uint32(signature.AggregateMovingTransitionSpace)),
	} {
		writeFingerprintUint64(out, value)
	}
	for _, value := range [...]bool{
		signature.Strict, signature.ReturnsSet, signature.SecurityDefiner, signature.Leakproof,
		signature.AggregateFinalExtra, signature.AggregateMovingFinalExtra,
		signature.AggregateHasInitialValue, signature.AggregateHasMovingInitialValue,
	} {
		writeFingerprintBool(out, value)
	}
	writeFingerprintString(out, signature.Source)
	writeFingerprintString(out, signature.AggregateInitialValue)
	writeFingerprintString(out, signature.AggregateMovingInitialValue)
}

func NewRegistry(config RegistryConfig) (*Registry, error) {
	definitions := config.ViewDefinitions
	config.ViewDefinitions = nil
	bootstrap, err := NewBootstrapRegistry(config)
	if err != nil {
		return nil, err
	}
	return bootstrap.Seal(definitions)
}

func NewBootstrapRegistry(config RegistryConfig) (*Registry, error) {
	if len(config.ViewDefinitions) != 0 {
		return nil, &model.Error{Code: "22023", Message: "bootstrap catalog registry cannot contain bound view definitions"}
	}
	validated, err := validateRelationIdentities(config.Relations, config.InformationSchema)
	if err != nil {
		return nil, err
	}
	domains, functions, err := validateInformationSchemaIdentities(config, validated)
	if err != nil {
		return nil, err
	}
	// relations is ID-indexed: slot 0 is an unused sentinel and relations[id]
	// holds the relation whose ID is id. The base declaration literal is not
	// ID-ascending, so every ID lookup indexes by identity, not by position.
	relations := make([]Relation, len(registry)+1)
	byName := make(map[string]int, len(registry))
	byRelationOID := make(map[model.OID]int, len(registry))
	byTypeName := make(map[string]registryType, len(registry)*2+len(domains)*2)
	typesByID := make(map[model.TypeID]registryType, len(registry)*2+len(domains)*2)
	arraysByElement := make(map[model.TypeID]model.TypeID, len(registry)+len(domains))
	addType := func(name string, id model.TypeID, typ registryType) error {
		if _, exists := byTypeName[name]; exists {
			return &model.Error{Code: "22023", Message: "duplicate logical catalog type name", Detail: name}
		}
		if _, exists := typesByID[id]; exists {
			return &model.Error{Code: "22023", Message: "duplicate logical catalog type identity", Detail: name}
		}
		byTypeName[name], typesByID[id] = typ, typ
		return nil
	}
	domainLayouts := make([]model.TypeLayout, builtin.InformationDomainCount+1)
	for _, domain := range domains {
		domainLayouts[domain.Definition.Kind] = cloneLayout(domain.Layout)
	}
	for i := range registry {
		id := registry[i].ID
		relations[id] = registry[i].Clone()
		relations[id].Identity = validated[id]
		relationTypeName, arrayTypeName := registryTypeNames(relations[id])
		if err := addType(relations[id].Namespace+"."+relationTypeName, model.TypeID(relations[id].Identity.RowTypeOID), registryType{relation: int(id)}); err != nil {
			return nil, err
		}
		if relations[id].Namespace == "information_schema" {
			for j := range relations[id].Columns {
				kind := informationColumnDomain(relations[id].Name, relations[id].Columns[j])
				if kind == 0 || int(kind) >= len(domainLayouts) || domainLayouts[kind].Type.ID == 0 {
					return nil, &model.Error{Code: "XX000", Message: "information schema column domain is unavailable", Detail: relations[id].Name + "." + relations[id].Columns[j].Name}
				}
				relations[id].Columns[j].Layout = cloneLayout(domainLayouts[kind])
			}
		}
		if err := addType(relations[id].Namespace+"."+arrayTypeName, model.TypeID(relations[id].Identity.ArrayTypeOID), registryType{relation: int(id), array: true}); err != nil {
			return nil, err
		}
		arraysByElement[model.TypeID(relations[id].Identity.RowTypeOID)] = model.TypeID(relations[id].Identity.ArrayTypeOID)
		byRelationOID[relations[id].Identity.RelationOID] = int(id)
		byName[relations[id].Namespace+"."+relations[id].Name] = int(id)
	}
	for i, domain := range domains {
		name := "information_schema." + domain.Definition.Name
		if err := addType(name, domain.Layout.Type.ID, registryType{relation: i, domain: true}); err != nil {
			return nil, err
		}
		if err := addType("information_schema._"+domain.Definition.Name, domain.Array.Type.ID, registryType{relation: i, array: true, domain: true}); err != nil {
			return nil, err
		}
		arraysByElement[domain.Layout.Type.ID] = domain.Array.Type.ID
	}
	functionsByID := make(map[model.FunctionID]builtin.InformationSchemaFunctionBinding, len(functions))
	functionsByName := make(map[string][]builtin.Signature, len(functions))
	functionHash := sha256.New()
	typeHash := sha256.New()
	registryHash := sha256.New()
	writeFingerprintUint64(registryHash, uint64(config.InformationSchema.ObjectID))
	writeFingerprintUint64(registryHash, uint64(config.InformationSchema.OID))
	writeFingerprintUint64(registryHash, uint64(NativeViewSemanticVersion))
	writeFingerprintUint64(registryHash, uint64(len(registry)))
	viewSources := make(map[RelationID]bool, len(nativeViewSources))
	viewCount := 0
	for id := 1; id <= len(registry); id++ {
		if logicalRelationDeclaration(relations[id]) == DeclarationView {
			viewCount++
		}
	}
	for _, source := range nativeViewSources {
		if source.relation == 0 || int(source.relation) > len(registry) ||
			logicalRelationDeclaration(relations[source.relation]) != DeclarationView || viewSources[source.relation] {
			return nil, &model.Error{Code: "XX000", Message: "native view source inventory is invalid"}
		}
		viewSources[source.relation] = true
	}
	if len(nativeViewSources) != viewCount {
		return nil, &model.Error{Code: "XX000", Message: "native view source inventory is incomplete"}
	}
	for id := 1; id <= len(registry); id++ {
		logical := relations[id]
		writeFingerprintUint64(registryHash, uint64(logical.ID))
		writeFingerprintString(registryHash, logical.Namespace)
		writeFingerprintString(registryHash, logical.Name)
		writeFingerprintUint64(registryHash, uint64(logicalRelationDeclaration(logical)))
		for _, oid := range [...]model.OID{logical.Identity.RelationOID, logical.Identity.RowTypeOID, logical.Identity.ArrayTypeOID} {
			writeFingerprintUint64(registryHash, uint64(oid))
		}
		writeFingerprintUint64(registryHash, uint64(len(logical.Columns)))
		for _, column := range logical.Columns {
			writeFingerprintUint64(registryHash, uint64(column.ID))
			writeFingerprintString(registryHash, column.Name)
			writeFingerprintLayout(registryHash, column.Layout)
			writeFingerprintBool(registryHash, column.Nullable)
			writeFingerprintString(registryHash, column.UnsupportedFeature)
		}
		if logicalRelationDeclaration(logical) == DeclarationView {
			sql, ok := canonicalViewSQL(logical.ID)
			if !ok || sql == "" {
				return nil, &model.Error{Code: "XX000", Message: "native view canonical definition is unavailable", Detail: logical.Namespace + "." + logical.Name}
			}
			writeFingerprintString(registryHash, sql)
		}
	}
	writeFingerprintUint64(typeHash, uint64(len(domains)))
	for _, domain := range domains {
		writeFingerprintUint64(typeHash, uint64(domain.Definition.Kind))
		writeFingerprintString(typeHash, domain.Definition.Name)
		writeFingerprintString(typeHash, domain.Definition.CheckSQL)
		writeFingerprintString(typeHash, domain.Definition.ConstraintName)
		writeFingerprintString(typeHash, domain.Definition.DefaultSQL)
		writeFingerprintBool(typeHash, domain.Definition.NotNull)
		writeFingerprintLayout(typeHash, domain.Definition.Layout)
		writeFingerprintLayout(typeHash, domain.Definition.Base)
		writeFingerprintLayout(typeHash, domain.Layout)
		writeFingerprintLayout(typeHash, domain.Array)
		for _, oid := range [...]model.OID{domain.Identity.TypeOID, domain.Identity.ArrayTypeOID, domain.Identity.ConstraintOID} {
			writeFingerprintUint64(typeHash, uint64(oid))
		}
	}
	writeFingerprintUint64(functionHash, uint64(len(functions)))
	for _, function := range functions {
		functionsByID[function.Signature.ID] = function
		functionsByName[function.Signature.Name] = append(functionsByName[function.Signature.Name], function.Signature)
		writeFingerprintUint64(functionHash, uint64(function.Operation))
		writeFingerprintSignature(functionHash, function.Signature)
	}
	_, _ = registryHash.Write(typeHash.Sum(nil))
	_, _ = registryHash.Write(functionHash.Sum(nil))
	var functionFingerprint, typeFingerprint [sha256.Size]byte
	var registryVersion RegistryVersion
	copy(functionFingerprint[:], functionHash.Sum(nil))
	copy(typeFingerprint[:], typeHash.Sum(nil))
	copy(registryVersion[:], registryHash.Sum(nil))
	result := &Registry{
		relations: relations, informationDomains: domains, informationFunctions: functions,
		byName: byName, byRelationOID: byRelationOID, byTypeName: byTypeName, typesByID: typesByID, arraysByElement: arraysByElement,
		informationFunctionsByID: functionsByID, informationFunctionsByName: functionsByName,
		informationFunctionFingerprint: functionFingerprint, informationTypeFingerprint: typeFingerprint, informationSchema: config.InformationSchema, registryVersion: registryVersion,
	}
	if !result.validIdentityRegistry() {
		return nil, &model.Error{Code: "XX000", Message: "catalog identity registry construction failed"}
	}
	result.identityComplete = true
	return result, nil
}

func (r *Registry) validIdentityRegistry() bool {
	domainCount, functionCount := builtin.InformationDomainCount, builtin.InformationSchemaFunctionCount
	if r == nil || r.informationSchema.ObjectID == 0 || r.informationSchema.OID == 0 || r.informationSchema.OID == 11 ||
		len(r.relations) != len(registry)+1 || len(r.informationDomains) != domainCount || len(r.informationFunctions) != functionCount ||
		len(r.byName) != len(registry) || len(r.byRelationOID) != len(registry) ||
		len(r.byTypeName) != len(registry)*2+domainCount*2 || len(r.typesByID) != len(registry)*2+domainCount*2 ||
		len(r.arraysByElement) != len(registry)+domainCount || len(r.informationFunctionsByID) != functionCount ||
		len(r.informationFunctionsByName) != functionCount {
		return false
	}
	if r.relations[0].ID != 0 || r.relations[0].Namespace != "" || r.relations[0].Name != "" || len(r.relations[0].Columns) != 0 {
		return false
	}
	for id := 1; id <= len(registry); id++ {
		identity := r.relations[id].Identity
		if r.relations[id].ID != RelationID(id) || identity.RelationOID == 0 || identity.RowTypeOID == 0 || identity.ArrayTypeOID == 0 || identity.RelationOID == identity.RowTypeOID || identity.RelationOID == identity.ArrayTypeOID || identity.RowTypeOID == identity.ArrayTypeOID {
			return false
		}
	}
	return true
}
func (r *Registry) IdentityComplete() bool {
	return r != nil && r.identityComplete
}
func (r *Registry) validSealedRegistry() bool {
	if !r.validIdentityRegistry() {
		return false
	}
	for id := 1; id < len(r.relations); id++ {
		relation := r.relations[id]
		view := logicalRelationDeclaration(relation) == DeclarationView
		definition := relation.ViewDefinition
		if view != (definition.SQL != "") {
			return false
		}
		if view && (definition.Bound.Relation != relation.ID ||
			definition.Bound.RegistryVersion != r.registryVersion ||
			definition.Bound.Semantic.SemanticVersion != NativeViewSemanticVersion ||
			definition.Bound.Semantic.SQL != "" || len(definition.Bound.Semantic.Encoding) == 0) {
			return false
		}
	}
	return true
}
func (r *Registry) Complete() bool {
	return r != nil && r.sealed
}

func (r *Registry) Version() RegistryVersion {
	if !r.IdentityComplete() {
		return RegistryVersion{}
	}
	return r.registryVersion
}
func (r *Registry) BoundView(source NativeViewSource, semantic model.ExpressionDefinition, outputs []ViewOutputColumn) (BoundViewDefinition, error) {
	if !r.IdentityComplete() || r.Complete() {
		return BoundViewDefinition{}, &model.Error{Code: "55000", Message: "catalog bootstrap registry is unavailable"}
	}
	if source.Relation == 0 || int(source.Relation) >= len(r.relations) {
		return BoundViewDefinition{}, &model.Error{Code: "22023", Message: "native view source does not match the catalog registry"}
	}
	relation, ok := r.LookupID(source.Relation)
	canonical, canonicalOK := canonicalViewSQL(source.Relation)
	if !ok || logicalRelationDeclaration(relation) != DeclarationView || !canonicalOK ||
		source.Namespace != relation.Namespace || source.Name != relation.Name ||
		source.SemanticVersion != NativeViewSemanticVersion || source.SQL != canonical {
		return BoundViewDefinition{}, &model.Error{Code: "22023", Message: "native view source does not match the catalog registry"}
	}
	if len(outputs) != len(relation.Columns) {
		return BoundViewDefinition{}, &model.Error{Code: "22023", Message: "native view result columns do not match the catalog registry", Detail: relation.Namespace + "." + relation.Name}
	}
	for i, output := range outputs {
		column := relation.Columns[i]
		if output.Name != column.Name || !sameRegistryLayout(output.Layout, column.Layout) {
			return BoundViewDefinition{}, &model.Error{
				Code:    "22023",
				Message: "native view result column does not match the catalog registry",
				Detail: fmt.Sprintf(
					"%s.%s column %d: expected name %q layout %+v; got name %q layout %+v",
					relation.Namespace, relation.Name, i+1, column.Name, column.Layout, output.Name, output.Layout,
				),
			}
		}
	}
	definition := BoundViewDefinition{Relation: relation.ID, RegistryVersion: r.registryVersion, Semantic: semantic}
	if err := r.validateBoundViewDefinition(relation, definition); err != nil {
		return BoundViewDefinition{}, err
	}
	return cloneBoundViewDefinition(definition), nil
}
func sameRegistryLayout(left, right model.TypeLayout) bool {
	return left.Type == right.Type &&
		slices.Equal(left.FieldNames, right.FieldNames) &&
		slices.EqualFunc(left.Children, right.Children, sameRegistryLayout)
}

func (r *Registry) validateBoundViewDefinition(relation Relation, definition BoundViewDefinition) error {
	if definition.Relation != relation.ID || definition.RegistryVersion != r.registryVersion ||
		definition.Semantic.SemanticVersion != NativeViewSemanticVersion || definition.Semantic.SQL != "" ||
		len(definition.Semantic.Encoding) == 0 {
		return &model.Error{Code: "22023", Message: "native view semantic definition is invalid", Detail: relation.Namespace + "." + relation.Name}
	}
	seen := make(map[model.ObjectID]uint64, len(definition.Semantic.Dependencies))
	for _, dependency := range definition.Semantic.Dependencies {
		if dependency.Object == 0 || dependency.Version == 0 {
			return &model.Error{Code: "22023", Message: "native view dependency identity is invalid", Detail: relation.Namespace + "." + relation.Name}
		}
		if version, ok := seen[dependency.Object]; ok && version != dependency.Version {
			return &model.Error{Code: "22023", Message: "native view dependency versions conflict", Detail: relation.Namespace + "." + relation.Name}
		}
		seen[dependency.Object] = dependency.Version
	}
	return nil
}


func (r *Registry) Seal(definitions map[RelationID]BoundViewDefinition) (*Registry, error) {
	if !r.IdentityComplete() || r.Complete() {
		return nil, &model.Error{Code: "55000", Message: "catalog bootstrap registry is unavailable"}
	}
	expected := 0
	for _, relation := range r.relations {
		if logicalRelationDeclaration(relation) == DeclarationView {
			expected++
		}
	}
	if len(definitions) != expected {
		return nil, &model.Error{Code: "22023", Message: "native view definitions are incomplete"}
	}
	sealed := *r
	sealed.relations = make([]Relation, len(r.relations))
	for i, relation := range r.relations {
		sealed.relations[i] = relation.Clone()
	}
	for id, definition := range definitions {
		if id == 0 || int(id) >= len(sealed.relations) {
			return nil, &model.Error{Code: "22023", Message: "unknown native view definition identity"}
		}
		relation := &sealed.relations[id]
		if relation.ID != id || logicalRelationDeclaration(*relation) != DeclarationView {
			return nil, &model.Error{Code: "22023", Message: "native view definition targets a non-view", Detail: relation.Namespace + "." + relation.Name}
		}
		if err := r.validateBoundViewDefinition(*relation, definition); err != nil {
			return nil, err
		}
		sql, ok := canonicalViewSQL(id)
		if !ok || sql == "" {
			return nil, &model.Error{Code: "XX000", Message: "native view canonical definition is unavailable", Detail: relation.Namespace + "." + relation.Name}
		}
		relation.ViewDefinition = ViewDefinition{SQL: sql, Bound: cloneBoundViewDefinition(definition)}
	}
	if !sealed.validSealedRegistry() {
		return nil, &model.Error{Code: "XX000", Message: "sealed catalog registry is incomplete"}
	}
	sealed.sealed = true
	return &sealed, nil
}

func cloneBoundViewDefinition(definition BoundViewDefinition) BoundViewDefinition {
	definition.Semantic.SQL = strings.Clone(definition.Semantic.SQL)
	definition.Semantic.Encoding = slices.Clone(definition.Semantic.Encoding)
	definition.Semantic.Dependencies = slices.Clone(definition.Semantic.Dependencies)
	return definition
}

func (r *Registry) ViewDefinitionByOID(oid model.OID) (ViewDefinition, bool) {
	if !r.Complete() {
		return ViewDefinition{}, false
	}
	index, ok := r.byRelationOID[oid]
	if !ok || logicalRelationDeclaration(r.relations[index]) != DeclarationView {
		return ViewDefinition{}, false
	}
	return cloneViewDefinition(r.relations[index].ViewDefinition), true
}

func (r *Registry) ViewDefinition(namespace, name string) (ViewDefinition, bool) {
	if !r.Complete() {
		return ViewDefinition{}, false
	}
	index, ok := r.byName[namespace+"."+name]
	if !ok || logicalRelationDeclaration(r.relations[index]) != DeclarationView {
		return ViewDefinition{}, false
	}
	return cloneViewDefinition(r.relations[index].ViewDefinition), true
}

func cloneViewDefinition(definition ViewDefinition) ViewDefinition {
	definition.SQL = strings.Clone(definition.SQL)
	definition.Bound = cloneBoundViewDefinition(definition.Bound)
	return definition
}


func (r *Registry) Lookup(namespace, name string) (Relation, bool) {
	if r == nil {
		return Relation{}, false
	}
	index, ok := r.byName[namespace+"."+name]
	if !ok {
		return Relation{}, false
	}
	return r.relations[index].Clone(), true
}
func (r *Registry) LookupType(namespace, name string) (model.TypeLayout, bool) {
	if r == nil {
		return model.TypeLayout{}, false
	}
	typ, ok := r.byTypeName[namespace+"."+name]
	if !ok {
		return model.TypeLayout{}, false
	}
	return r.typeLayout(typ), true
}

func (r *Registry) LookupTypeID(id model.TypeID) (model.TypeLayout, bool) {
	if r == nil {
		return model.TypeLayout{}, false
	}
	typ, ok := r.typesByID[id]
	if !ok {
		return model.TypeLayout{}, false
	}
	return r.typeLayout(typ), true
}

func (r *Registry) typeLayout(typ registryType) model.TypeLayout {
	if typ.domain {
		domain := r.informationDomains[typ.relation]
		if typ.array {
			return cloneLayout(domain.Array)
		}
		return cloneLayout(domain.Layout)
	}
	if typ.relation == 0 || typ.relation >= len(r.relations) {
		return model.TypeLayout{}
	}
	relation := r.relations[typ.relation]
	row := relationRowTypeLayout(relation)
	if !typ.array {
		return row
	}
	return model.TypeLayout{
		Type:     model.Type{ID: model.TypeID(relation.Identity.ArrayTypeOID), Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{row},
	}
}

func (r *Registry) CompanionArray(element model.TypeID) (model.TypeID, bool) {
	if r == nil {
		return 0, false
	}
	array, ok := r.arraysByElement[element]
	return array, ok
}
func (r *Registry) InformationDomains() []RegisteredInformationDomain {
	if r == nil {
		return nil
	}
	domains := make([]RegisteredInformationDomain, len(r.informationDomains))
	for i := range r.informationDomains {
		domains[i] = cloneInformationDomain(r.informationDomains[i])
	}
	return domains
}

func (r *Registry) InformationDomain(kind builtin.InformationDomainKind) (RegisteredInformationDomain, bool) {
	if r == nil {
		return RegisteredInformationDomain{}, false
	}
	for _, domain := range r.informationDomains {
		if domain.Definition.Kind == kind {
			return cloneInformationDomain(domain), true
		}
	}
	return RegisteredInformationDomain{}, false
}

func (r *Registry) InformationDomainByType(id model.TypeID) (RegisteredInformationDomain, bool) {
	if r == nil {
		return RegisteredInformationDomain{}, false
	}
	for _, domain := range r.informationDomains {
		if domain.Layout.Type.ID == id {
			return cloneInformationDomain(domain), true
		}
	}
	return RegisteredInformationDomain{}, false
}

func cloneInformationDomain(domain RegisteredInformationDomain) RegisteredInformationDomain {
	domain.Definition.Layout = cloneLayout(domain.Definition.Layout)
	domain.Definition.Base = cloneLayout(domain.Definition.Base)
	domain.Layout = cloneLayout(domain.Layout)
	domain.Array = cloneLayout(domain.Array)
	return domain
}

func (r *Registry) ResolveInformationSchemaFunction(id model.FunctionID) (builtin.InformationSchemaFunctionBinding, bool) {
	if r == nil {
		return builtin.InformationSchemaFunctionBinding{}, false
	}
	function, ok := r.informationFunctionsByID[id]
	return function, ok
}

func (r *Registry) ResolveInformationSchemaType(id model.TypeID) (builtin.InformationSchemaTypeBinding, bool) {
	if r == nil {
		return builtin.InformationSchemaTypeBinding{}, false
	}
	typ, ok := r.typesByID[id]
	if !ok || !typ.domain {
		return builtin.InformationSchemaTypeBinding{}, false
	}
	domain := r.informationDomains[typ.relation]
	if typ.array {
		return builtin.InformationSchemaTypeBinding{
			Domain: domain.Definition.Kind, Kind: model.TypeKindArray,
			Layout: cloneLayout(domain.Array), BaseLayout: cloneLayout(domain.Layout),
			TypeIO: builtin.TypeIO{Input: 750, Output: 751, Parameter: domain.Layout.Type.ID, Delimiter: ','},
		}, true
	}
	base, ok := builtin.LookupType(domain.Definition.Base.Type.ID)
	if !ok {
		return builtin.InformationSchemaTypeBinding{}, false
	}
	return builtin.InformationSchemaTypeBinding{
		Domain: domain.Definition.Kind, Kind: model.TypeKindDomain,
		Layout: cloneLayout(domain.Layout), BaseLayout: cloneLayout(domain.Definition.Base),
		TypeIO: builtin.TypeIO{Input: 2597, Output: base.Output, Parameter: domain.Layout.Type.ID, Delimiter: base.Delimiter},
	}, true
}

func (r *Registry) InformationSchemaTypeFingerprint() [sha256.Size]byte {
	if r == nil {
		return [sha256.Size]byte{}
	}
	return r.informationTypeFingerprint
}

func (r *Registry) InformationSchemaFunction(id model.FunctionID) (builtin.InformationSchemaFunction, bool) {
	function, ok := r.ResolveInformationSchemaFunction(id)
	return function.Operation, ok
}

func (r *Registry) InformationSchemaFunctionsNamed(name string) iter.Seq[builtin.Signature] {
	return func(yield func(builtin.Signature) bool) {
		if r == nil {
			return
		}
		for _, signature := range r.informationFunctionsByName[name] {
			if !yield(signature) {
				return
			}
		}
	}
}

func (r *Registry) InformationSchemaFunctions() []builtin.InformationSchemaFunctionBinding {
	if r == nil {
		return nil
	}
	return slices.Clone(r.informationFunctions)
}

func (r *Registry) InformationSchemaFunctionFingerprint() [sha256.Size]byte {
	if r == nil {
		return [sha256.Size]byte{}
	}
	return r.informationFunctionFingerprint
}


func (r *Registry) LookupID(id RelationID) (Relation, bool) {
	if r == nil || id == 0 || int(id) >= len(r.relations) {
		return Relation{}, false
	}
	return r.relations[id].Clone(), true
}

func (r *Registry) Identity(id RelationID) (RelationIdentity, bool) {
	if r == nil || id == 0 || int(id) >= len(r.relations) {
		return RelationIdentity{}, false
	}
	return r.relations[id].Identity, true
}
func (r *Registry) NamespaceIdentity(namespace string) (NamespaceIdentity, bool) {
	if r == nil {
		return NamespaceIdentity{}, false
	}
	switch namespace {
	case "pg_catalog":
		return NamespaceIdentity{OID: 11}, true
	case "information_schema":
		return r.informationSchema, true
	default:
		return NamespaceIdentity{}, false
	}
}

func (r *Registry) All() []Relation {
	if r == nil {
		return nil
	}
	relations := make([]Relation, 0, len(r.relations)-1)
	for id := 1; id < len(r.relations); id++ {
		relations = append(relations, r.relations[id].Clone())
	}
	return relations
}

func registryTypeNames(relation Relation) (string, string) {
	relationName := relation.Name
	if info, ok := builtin.LookupType(model.TypeID(relation.Identity.RowTypeOID)); ok {
		relationName = info.Name
	}
	arrayName := "_" + relation.Name
	if info, ok := builtin.LookupType(model.TypeID(relation.Identity.ArrayTypeOID)); ok {
		arrayName = info.Name
	}
	return relationName, arrayName
}

func logicalRelationDeclaration(relation Relation) DeclarationKind {
	if relation.Namespace == "pg_catalog" {
		switch relation.ID {
		case IDPgIndexes, IDPgMatviews, IDPgRoles, IDPgSequences, IDPgSettings, IDPgStats, IDPgTables, IDPgUser, IDPgViews:
			return DeclarationView
		default:
			return DeclarationTable
		}
	}
	if relation.Namespace == "information_schema" {
		for _, declaration := range informationDeclarations {
			if declaration.Name == relation.Name {
				return declaration.Kind
			}
		}
	}
	return 0
}

func informationColumnDomain(relation string, column Column) builtin.InformationDomainKind {
	switch column.Layout.Type.ID {
	case 23:
		if column.Layout.Type.Modifier == -1 {
			return builtin.InformationCardinalNumber
		}
	case 1043:
		switch column.Layout.Type.Modifier {
		case -1:
			if informationCharacterDataColumn(relation, column.Name) {
				return builtin.InformationCharacterData
			}
			return builtin.InformationSQLIdentifier
		case 7:
			return builtin.InformationYesOrNo
		}
	case 1184:
		if column.Layout.Type.Modifier == 2 {
			return builtin.InformationTimeStamp
		}
	}
	return 0
}

func informationCharacterDataColumn(relation, column string) bool {
	switch relation {
	case "attributes":
		switch column {
		case "attribute_default", "data_type", "interval_type":
			return true
		}
	case "check_constraints":
		return column == "check_clause"
	case "collations":
		return column == "pad_attribute"
	case "columns":
		switch column {
		case "column_default", "data_type", "interval_type", "identity_generation", "identity_start", "identity_increment", "identity_maximum", "identity_minimum", "is_generated", "generation_expression":
			return true
		}
	case "domains":
		switch column {
		case "data_type", "interval_type", "domain_default":
			return true
		}
	case "element_types":
		switch column {
		case "object_type", "data_type", "interval_type":
			return true
		}
	case "parameters":
		switch column {
		case "parameter_mode", "data_type", "interval_type", "parameter_default":
			return true
		}
	case "referential_constraints":
		switch column {
		case "match_option", "update_rule", "delete_rule":
			return true
		}
	case "routines":
		switch column {
		case "routine_type", "data_type", "interval_type", "routine_body", "routine_definition", "external_name", "external_language", "parameter_style", "sql_data_access", "sql_path", "security_type", "result_cast_from_data_type", "result_cast_interval_type":
			return true
		}
	case "schemata":
		return column == "sql_path"
	case "sequences":
		switch column {
		case "data_type", "start_value", "minimum_value", "maximum_value", "increment":
			return true
		}
	case "sql_features":
		switch column {
		case "feature_id", "feature_name", "sub_feature_id", "sub_feature_name", "is_verified_by", "comments":
			return true
		}
	case "sql_implementation_info":
		switch column {
		case "implementation_info_id", "implementation_info_name", "character_value", "comments":
			return true
		}
	case "sql_parts":
		switch column {
		case "feature_id", "feature_name", "is_verified_by", "comments":
			return true
		}
	case "sql_sizing":
		switch column {
		case "sizing_name", "comments":
			return true
		}
	case "table_constraints":
		return column == "constraint_type"
	case "tables":
		switch column {
		case "table_type", "reference_generation", "commit_action":
			return true
		}
	case "triggers":
		switch column {
		case "event_manipulation", "action_condition", "action_statement", "action_orientation", "action_timing":
			return true
		}
	case "user_defined_types":
		switch column {
		case "user_defined_type_category", "ordering_form", "ordering_category", "reference_type", "data_type", "interval_type":
			return true
		}
	case "views":
		switch column {
		case "view_definition", "check_option":
			return true
		}
	}
	return false
}

func relationRowTypeLayout(relation Relation) model.TypeLayout {
	layout := model.TypeLayout{
		Type:       model.Type{ID: model.TypeID(relation.Identity.RowTypeOID), Modifier: -1, Storage: model.TypeComposite},
		FieldNames: make([]string, len(relation.Columns)),
		Children:   make([]model.TypeLayout, len(relation.Columns)),
	}
	for i := range relation.Columns {
		layout.FieldNames[i] = relation.Columns[i].Name
		layout.Children[i] = cloneLayout(relation.Columns[i].Layout)
	}
	return layout
}

func InformationDeclarations() []Declaration {
	return slices.Clone(informationDeclarations)
}

func buildRegistry() []Relation {
	return []Relation{
		r(IDPgAggregate, "pg_catalog", "pg_aggregate", 2600, c("aggfnoid", 24, false, "", -1), c("aggkind", 18, false, "", -1), c("aggnumdirectargs", 21, false, "", -1), c("aggtransfn", 24, false, "", -1), c("aggfinalfn", 24, false, "", -1), c("aggcombinefn", 24, false, "", -1), c("aggserialfn", 24, false, "", -1), c("aggdeserialfn", 24, false, "", -1), c("aggmtransfn", 24, false, "", -1), c("aggminvtransfn", 24, false, "", -1), c("aggmfinalfn", 24, false, "", -1), c("aggfinalextra", 16, false, "", -1), c("aggmfinalextra", 16, false, "", -1), c("aggfinalmodify", 18, false, "", -1), c("aggmfinalmodify", 18, false, "", -1), c("aggsortop", 26, false, "", -1), c("aggtranstype", 26, false, "", -1), c("aggtransspace", 23, false, "", -1), c("aggmtranstype", 26, false, "", -1), c("aggmtransspace", 23, false, "", -1), c("agginitval", 25, true, "", -1), c("aggminitval", 25, true, "", -1)),
		r(IDPgAm, "pg_catalog", "pg_am", 2601, c("oid", 26, false, "", -1), c("amname", 19, false, "", -1), c("amhandler", 24, false, "", -1), c("amtype", 18, false, "", -1)),
		r(IDPgAmop, "pg_catalog", "pg_amop", 2602, c("oid", 26, false, "", -1), c("amopfamily", 26, false, "", -1), c("amoplefttype", 26, false, "", -1), c("amoprighttype", 26, false, "", -1), c("amopstrategy", 21, false, "", -1), c("amoppurpose", 18, false, "", -1), c("amopopr", 26, false, "", -1), c("amopmethod", 26, false, "", -1), c("amopsortfamily", 26, false, "", -1)),
		r(IDPgAmproc, "pg_catalog", "pg_amproc", 2603, c("oid", 26, false, "", -1), c("amprocfamily", 26, false, "", -1), c("amproclefttype", 26, false, "", -1), c("amprocrighttype", 26, false, "", -1), c("amprocnum", 21, false, "", -1), c("amproc", 24, false, "", -1)),
		r(IDPgAttrdef, "pg_catalog", "pg_attrdef", 2604, c("oid", 26, false, "", -1), c("adrelid", 26, false, "", -1), c("adnum", 21, false, "", -1), c("adbin", 194, false, "", -1)),
		r(IDPgAttribute, "pg_catalog", "pg_attribute", 1249, c("attrelid", 26, false, "", -1), c("attname", 19, false, "", -1), c("atttypid", 26, false, "", -1), c("attlen", 21, false, "physical_storage", -1), c("attnum", 21, false, "", -1), c("atttypmod", 23, false, "", -1), c("attndims", 21, false, "", -1), c("attbyval", 16, false, "physical_storage", -1), c("attalign", 18, false, "physical_storage", -1), c("attstorage", 18, false, "physical_storage", -1), c("attcompression", 18, false, "physical_storage", -1), c("attnotnull", 16, false, "", -1), c("atthasdef", 16, false, "", -1), c("atthasmissing", 16, false, "", -1), c("attidentity", 18, false, "", -1), c("attgenerated", 18, false, "", -1), c("attisdropped", 16, false, "", -1), c("attislocal", 16, false, "", -1), c("attinhcount", 21, false, "", -1), c("attcollation", 26, false, "", -1), c("attstattarget", 21, false, "", -1), c("attacl", 1034, true, "", -1), c("attoptions", 1009, true, "", -1), c("attfdwoptions", 1009, true, "", -1), c("attmissingval", 2277, true, "", -1)),
		r(IDPgCast, "pg_catalog", "pg_cast", 2605, c("oid", 26, false, "", -1), c("castsource", 26, false, "", -1), c("casttarget", 26, false, "", -1), c("castfunc", 26, false, "", -1), c("castcontext", 18, false, "", -1), c("castmethod", 18, false, "", -1)),
		r(IDPgClass, "pg_catalog", "pg_class", 1259, c("oid", 26, false, "", -1), c("relname", 19, false, "", -1), c("relnamespace", 26, false, "", -1), c("reltype", 26, false, "", -1), c("reloftype", 26, false, "", -1), c("relowner", 26, false, "", -1), c("relam", 26, false, "", -1), c("relfilenode", 26, false, "physical_storage", -1), c("reltablespace", 26, false, "physical_storage", -1), c("relpages", 23, false, "physical_storage", -1), c("reltuples", 700, false, "", -1), c("relallvisible", 23, false, "physical_storage", -1), c("relallfrozen", 23, false, "physical_storage", -1), c("reltoastrelid", 26, false, "physical_storage", -1), c("relhasindex", 16, false, "", -1), c("relisshared", 16, false, "", -1), c("relpersistence", 18, false, "", -1), c("relkind", 18, false, "", -1), c("relnatts", 21, false, "", -1), c("relchecks", 21, false, "", -1), c("relhasrules", 16, false, "", -1), c("relhastriggers", 16, false, "", -1), c("relhassubclass", 16, false, "", -1), c("relrowsecurity", 16, false, "", -1), c("relforcerowsecurity", 16, false, "", -1), c("relispopulated", 16, false, "", -1), c("relreplident", 18, false, "replication", -1), c("relispartition", 16, false, "", -1), c("relrewrite", 26, false, "physical_storage", -1), c("relfrozenxid", 28, false, "physical_storage", -1), c("relminmxid", 28, false, "physical_storage", -1), c("relacl", 1034, true, "", -1), c("reloptions", 1009, true, "", -1), c("relpartbound", 194, true, "", -1)),
		r(IDPgCollation, "pg_catalog", "pg_collation", 3456, c("oid", 26, false, "", -1), c("collname", 19, false, "", -1), c("collnamespace", 26, false, "", -1), c("collowner", 26, false, "", -1), c("collprovider", 18, false, "", -1), c("collisdeterministic", 16, false, "", -1), c("collencoding", 23, false, "", -1), c("collcollate", 25, true, "", -1), c("collctype", 25, true, "", -1), c("colllocale", 25, true, "", -1), c("collicurules", 25, true, "", -1), c("collversion", 25, true, "", -1)),
		r(IDPgConstraint, "pg_catalog", "pg_constraint", 2606, c("oid", 26, false, "", -1), c("conname", 19, false, "", -1), c("connamespace", 26, false, "", -1), c("contype", 18, false, "", -1), c("condeferrable", 16, false, "", -1), c("condeferred", 16, false, "", -1), c("conenforced", 16, false, "", -1), c("convalidated", 16, false, "", -1), c("conrelid", 26, false, "", -1), c("contypid", 26, false, "", -1), c("conindid", 26, false, "", -1), c("conparentid", 26, false, "", -1), c("confrelid", 26, false, "", -1), c("confupdtype", 18, false, "", -1), c("confdeltype", 18, false, "", -1), c("confmatchtype", 18, false, "", -1), c("conislocal", 16, false, "", -1), c("coninhcount", 21, false, "", -1), c("connoinherit", 16, false, "", -1), c("conperiod", 16, false, "", -1), c("conkey", 1005, true, "", -1), c("confkey", 1005, true, "", -1), c("conpfeqop", 1028, true, "", -1), c("conppeqop", 1028, true, "", -1), c("conffeqop", 1028, true, "", -1), c("confdelsetcols", 1005, true, "", -1), c("conexclop", 1028, true, "", -1), c("conbin", 194, true, "", -1)),
		r(IDPgDatabase, "pg_catalog", "pg_database", 1262, c("oid", 26, false, "", -1), c("datname", 19, false, "", -1), c("datdba", 26, false, "", -1), c("encoding", 23, false, "", -1), c("datlocprovider", 18, false, "", -1), c("datistemplate", 16, false, "", -1), c("datallowconn", 16, false, "", -1), c("dathasloginevt", 16, false, "", -1), c("datconnlimit", 23, false, "", -1), c("datfrozenxid", 28, false, "physical_storage", -1), c("datminmxid", 28, false, "physical_storage", -1), c("dattablespace", 26, false, "physical_storage", -1), c("datcollate", 25, false, "", -1), c("datctype", 25, false, "", -1), c("datlocale", 25, true, "", -1), c("daticurules", 25, true, "", -1), c("datcollversion", 25, true, "", -1), c("datacl", 1034, true, "", -1)),
		r(IDPgDepend, "pg_catalog", "pg_depend", 2608, c("classid", 26, false, "", -1), c("objid", 26, false, "", -1), c("objsubid", 23, false, "", -1), c("refclassid", 26, false, "", -1), c("refobjid", 26, false, "", -1), c("refobjsubid", 23, false, "", -1), c("deptype", 18, false, "", -1)),
		r(IDPgDescription, "pg_catalog", "pg_description", 2609, c("objoid", 26, false, "", -1), c("classoid", 26, false, "", -1), c("objsubid", 23, false, "", -1), c("description", 25, false, "", -1)),
		r(IDPgEnum, "pg_catalog", "pg_enum", 3501, c("oid", 26, false, "", -1), c("enumtypid", 26, false, "", -1), c("enumsortorder", 700, false, "", -1), c("enumlabel", 19, false, "", -1)),
		r(IDPgIndex, "pg_catalog", "pg_index", 2610, c("indexrelid", 26, false, "", -1), c("indrelid", 26, false, "", -1), c("indnatts", 21, false, "", -1), c("indnkeyatts", 21, false, "", -1), c("indisunique", 16, false, "", -1), c("indnullsnotdistinct", 16, false, "", -1), c("indisprimary", 16, false, "", -1), c("indisexclusion", 16, false, "", -1), c("indimmediate", 16, false, "", -1), c("indisclustered", 16, false, "", -1), c("indisvalid", 16, false, "", -1), c("indcheckxmin", 16, false, "physical_storage", -1), c("indisready", 16, false, "", -1), c("indislive", 16, false, "", -1), c("indisreplident", 16, false, "replication", -1), c("indkey", 22, false, "", -1), c("indcollation", 30, false, "", -1), c("indclass", 30, false, "", -1), c("indoption", 22, false, "", -1), c("indexprs", 194, true, "", -1), c("indpred", 194, true, "", -1)),
		r(IDPgIndexes, "pg_catalog", "pg_indexes", 0, c("schemaname", 19, false, "", -1), c("tablename", 19, false, "", -1), c("indexname", 19, false, "", -1), c("tablespace", 19, true, "", -1), c("indexdef", 25, false, "", -1)),
		r(IDPgInherits, "pg_catalog", "pg_inherits", 2611, c("inhrelid", 26, false, "", -1), c("inhparent", 26, false, "", -1), c("inhseqno", 23, false, "", -1), c("inhdetachpending", 16, false, "", -1)),
		r(IDPgLanguage, "pg_catalog", "pg_language", 2612, c("oid", 26, false, "", -1), c("lanname", 19, false, "", -1), c("lanowner", 26, false, "", -1), c("lanispl", 16, false, "", -1), c("lanpltrusted", 16, false, "", -1), c("lanplcallfoid", 26, false, "", -1), c("laninline", 26, false, "", -1), c("lanvalidator", 26, false, "", -1), c("lanacl", 1034, true, "", -1)),
		r(IDPgMatviews, "pg_catalog", "pg_matviews", 0, c("schemaname", 19, false, "", -1), c("matviewname", 19, false, "", -1), c("matviewowner", 19, false, "", -1), c("tablespace", 19, true, "", -1), c("hasindexes", 16, false, "", -1), c("ispopulated", 16, false, "", -1), c("definition", 25, false, "", -1)),
		r(IDPgNamespace, "pg_catalog", "pg_namespace", 2615, c("oid", 26, false, "", -1), c("nspname", 19, false, "", -1), c("nspowner", 26, false, "", -1), c("nspacl", 1034, true, "", -1)),
		r(IDPgOpclass, "pg_catalog", "pg_opclass", 2616, c("oid", 26, false, "", -1), c("opcmethod", 26, false, "", -1), c("opcname", 19, false, "", -1), c("opcnamespace", 26, false, "", -1), c("opcowner", 26, false, "", -1), c("opcfamily", 26, false, "", -1), c("opcintype", 26, false, "", -1), c("opcdefault", 16, false, "", -1), c("opckeytype", 26, false, "", -1)),
		r(IDPgOperator, "pg_catalog", "pg_operator", 2617, c("oid", 26, false, "", -1), c("oprname", 19, false, "", -1), c("oprnamespace", 26, false, "", -1), c("oprowner", 26, false, "", -1), c("oprkind", 18, false, "", -1), c("oprcanmerge", 16, false, "", -1), c("oprcanhash", 16, false, "", -1), c("oprleft", 26, false, "", -1), c("oprright", 26, false, "", -1), c("oprresult", 26, false, "", -1), c("oprcom", 26, false, "", -1), c("oprnegate", 26, false, "", -1), c("oprcode", 24, false, "", -1), c("oprrest", 24, false, "", -1), c("oprjoin", 24, false, "", -1)),
		r(IDPgOpfamily, "pg_catalog", "pg_opfamily", 2753, c("oid", 26, false, "", -1), c("opfmethod", 26, false, "", -1), c("opfname", 19, false, "", -1), c("opfnamespace", 26, false, "", -1), c("opfowner", 26, false, "", -1)),
		r(IDPgPartitionedTable, "pg_catalog", "pg_partitioned_table", 3350, c("partrelid", 26, false, "", -1), c("partstrat", 18, false, "", -1), c("partnatts", 21, false, "", -1), c("partdefid", 26, false, "", -1), c("partattrs", 22, false, "", -1), c("partclass", 30, false, "", -1), c("partcollation", 30, false, "", -1), c("partexprs", 194, true, "", -1)),
		r(IDPgProc, "pg_catalog", "pg_proc", 1255, c("oid", 26, false, "", -1), c("proname", 19, false, "", -1), c("pronamespace", 26, false, "", -1), c("proowner", 26, false, "", -1), c("prolang", 26, false, "", -1), c("procost", 700, false, "", -1), c("prorows", 700, false, "", -1), c("provariadic", 26, false, "", -1), c("prosupport", 24, false, "", -1), c("prokind", 18, false, "", -1), c("prosecdef", 16, false, "", -1), c("proleakproof", 16, false, "", -1), c("proisstrict", 16, false, "", -1), c("proretset", 16, false, "", -1), c("provolatile", 18, false, "", -1), c("proparallel", 18, false, "", -1), c("pronargs", 21, false, "", -1), c("pronargdefaults", 21, false, "", -1), c("prorettype", 26, false, "", -1), c("proargtypes", 30, false, "", -1), c("proallargtypes", 1028, true, "", -1), c("proargmodes", 1002, true, "", -1), c("proargnames", 1009, true, "", -1), c("proargdefaults", 194, true, "", -1), c("protrftypes", 1028, true, "", -1), c("prosrc", 25, false, "", -1), c("probin", 25, true, "", -1), c("prosqlbody", 194, true, "", -1), c("proconfig", 1009, true, "", -1), c("proacl", 1034, true, "", -1)),
		r(IDPgRange, "pg_catalog", "pg_range", 3541, c("rngtypid", 26, false, "", -1), c("rngsubtype", 26, false, "", -1), c("rngmultitypid", 26, false, "", -1), c("rngcollation", 26, false, "", -1), c("rngsubopc", 26, false, "", -1), c("rngcanonical", 24, false, "", -1), c("rngsubdiff", 24, false, "", -1)),
		r(IDPgRoles, "pg_catalog", "pg_roles", 0, c("rolname", 19, false, "", -1), c("rolsuper", 16, false, "", -1), c("rolinherit", 16, false, "", -1), c("rolcreaterole", 16, false, "", -1), c("rolcreatedb", 16, false, "", -1), c("rolcanlogin", 16, false, "", -1), c("rolreplication", 16, false, "", -1), c("rolconnlimit", 23, false, "", -1), c("rolpassword", 25, true, "", -1), c("rolvaliduntil", 1184, true, "", -1), c("rolbypassrls", 16, false, "", -1), c("rolconfig", 1009, true, "", -1), c("oid", 26, false, "", -1)),
		r(IDPgSequence, "pg_catalog", "pg_sequence", 2224, c("seqrelid", 26, false, "", -1), c("seqtypid", 26, false, "", -1), c("seqstart", 20, false, "", -1), c("seqincrement", 20, false, "", -1), c("seqmax", 20, false, "", -1), c("seqmin", 20, false, "", -1), c("seqcache", 20, false, "", -1), c("seqcycle", 16, false, "", -1)),
		r(IDPgSequences, "pg_catalog", "pg_sequences", 0, c("schemaname", 19, false, "", -1), c("sequencename", 19, false, "", -1), c("sequenceowner", 19, false, "", -1), c("data_type", 2206, false, "", -1), c("start_value", 20, false, "", -1), c("min_value", 20, false, "", -1), c("max_value", 20, false, "", -1), c("increment_by", 20, false, "", -1), c("cycle", 16, false, "", -1), c("cache_size", 20, false, "", -1), c("last_value", 20, true, "", -1)),
		r(IDPgSettings, "pg_catalog", "pg_settings", 0, c("name", 25, false, "", -1), c("setting", 25, false, "", -1), c("unit", 25, true, "", -1), c("category", 25, false, "", -1), c("short_desc", 25, false, "", -1), c("extra_desc", 25, true, "", -1), c("context", 25, false, "", -1), c("vartype", 25, false, "", -1), c("source", 25, false, "", -1), c("min_val", 25, true, "", -1), c("max_val", 25, true, "", -1), c("enumvals", 1009, true, "", -1), c("boot_val", 25, false, "", -1), c("reset_val", 25, false, "", -1), c("sourcefile", 25, true, "", -1), c("sourceline", 23, true, "", -1), c("pending_restart", 16, false, "", -1)),
		r(IDPgStatistic, "pg_catalog", "pg_statistic", 2619, c("starelid", 26, false, "", -1), c("staattnum", 21, false, "", -1), c("stainherit", 16, false, "", -1), c("stanullfrac", 700, false, "", -1), c("stawidth", 23, false, "", -1), c("stadistinct", 700, false, "", -1), c("stakind1", 21, false, "", -1), c("stakind2", 21, false, "", -1), c("stakind3", 21, false, "", -1), c("stakind4", 21, false, "", -1), c("stakind5", 21, false, "", -1), c("staop1", 26, false, "", -1), c("staop2", 26, false, "", -1), c("staop3", 26, false, "", -1), c("staop4", 26, false, "", -1), c("staop5", 26, false, "", -1), c("stacoll1", 26, false, "", -1), c("stacoll2", 26, false, "", -1), c("stacoll3", 26, false, "", -1), c("stacoll4", 26, false, "", -1), c("stacoll5", 26, false, "", -1), c("stanumbers1", 1021, true, "", -1), c("stanumbers2", 1021, true, "", -1), c("stanumbers3", 1021, true, "", -1), c("stanumbers4", 1021, true, "", -1), c("stanumbers5", 1021, true, "", -1), c("stavalues1", 2277, true, "", -1), c("stavalues2", 2277, true, "", -1), c("stavalues3", 2277, true, "", -1), c("stavalues4", 2277, true, "", -1), c("stavalues5", 2277, true, "", -1)),
		r(IDPgStatisticExt, "pg_catalog", "pg_statistic_ext", 3381, c("oid", 26, false, "", -1), c("stxrelid", 26, false, "", -1), c("stxname", 19, false, "", -1), c("stxnamespace", 26, false, "", -1), c("stxowner", 26, false, "", -1), c("stxstattarget", 21, true, "", -1), c("stxkeys", 22, false, "", -1), c("stxkind", 1002, false, "", -1), c("stxexprs", 194, true, "", -1)),
		r(IDPgStatisticExtData, "pg_catalog", "pg_statistic_ext_data", 3429, c("stxoid", 26, false, "", -1), c("stxdinherit", 16, false, "", -1), c("stxdndistinct", 3361, true, "", -1), c("stxddependencies", 3402, true, "", -1), c("stxdmcv", 5017, true, "", -1), c("stxdexpr", 2287, true, "", -1)),
		r(IDPgStats, "pg_catalog", "pg_stats", 0, c("schemaname", 19, false, "", -1), c("tablename", 19, false, "", -1), c("attname", 19, false, "", -1), c("inherited", 16, false, "", -1), c("null_frac", 700, false, "", -1), c("avg_width", 23, false, "", -1), c("n_distinct", 700, false, "", -1), c("most_common_vals", 2277, true, "", -1), c("most_common_freqs", 1021, true, "", -1), c("histogram_bounds", 2277, true, "", -1), c("correlation", 700, true, "", -1), c("most_common_elems", 2277, true, "", -1), c("most_common_elem_freqs", 1021, true, "", -1), c("elem_count_histogram", 1021, true, "", -1), c("range_length_histogram", 2277, true, "", -1), c("range_empty_frac", 700, true, "", -1), c("range_bounds_histogram", 2277, true, "", -1)),
		r(IDPgTables, "pg_catalog", "pg_tables", 0, c("schemaname", 19, false, "", -1), c("tablename", 19, false, "", -1), c("tableowner", 19, false, "", -1), c("tablespace", 19, true, "", -1), c("hasindexes", 16, false, "", -1), c("hasrules", 16, false, "", -1), c("hastriggers", 16, false, "", -1), c("rowsecurity", 16, false, "", -1)),
		r(IDPgTrigger, "pg_catalog", "pg_trigger", 2620, c("oid", 26, false, "", -1), c("tgrelid", 26, false, "", -1), c("tgparentid", 26, false, "", -1), c("tgname", 19, false, "", -1), c("tgfoid", 26, false, "", -1), c("tgtype", 21, false, "", -1), c("tgenabled", 18, false, "", -1), c("tgisinternal", 16, false, "", -1), c("tgconstrrelid", 26, false, "", -1), c("tgconstrindid", 26, false, "", -1), c("tgconstraint", 26, false, "", -1), c("tgdeferrable", 16, false, "", -1), c("tginitdeferred", 16, false, "", -1), c("tgnargs", 21, false, "", -1), c("tgattr", 22, false, "", -1), c("tgargs", 17, false, "", -1), c("tgqual", 194, true, "", -1), c("tgoldtable", 19, true, "", -1), c("tgnewtable", 19, true, "", -1)),
		r(IDPgTSConfig, "pg_catalog", "pg_ts_config", 3602, c("oid", 26, false, "", -1), c("cfgname", 19, false, "", -1), c("cfgnamespace", 26, false, "", -1), c("cfgowner", 26, false, "", -1), c("cfgparser", 26, false, "", -1)),
		r(IDPgTSConfigMap, "pg_catalog", "pg_ts_config_map", 3603, c("mapcfg", 26, false, "", -1), c("maptokentype", 23, false, "", -1), c("mapseqno", 23, false, "", -1), c("mapdict", 26, false, "", -1)),
		r(IDPgTSDict, "pg_catalog", "pg_ts_dict", 3600, c("oid", 26, false, "", -1), c("dictname", 19, false, "", -1), c("dictnamespace", 26, false, "", -1), c("dictowner", 26, false, "", -1), c("dicttemplate", 26, false, "", -1), c("dictinitoption", 25, true, "", -1)),
		r(IDPgTSParser, "pg_catalog", "pg_ts_parser", 3601, c("oid", 26, false, "", -1), c("prsname", 19, false, "", -1), c("prsnamespace", 26, false, "", -1), c("prsstart", 24, false, "", -1), c("prstoken", 24, false, "", -1), c("prsend", 24, false, "", -1), c("prsheadline", 24, false, "", -1), c("prslextype", 24, false, "", -1)),
		r(IDPgTSTemplate, "pg_catalog", "pg_ts_template", 3764, c("oid", 26, false, "", -1), c("tmplname", 19, false, "", -1), c("tmplnamespace", 26, false, "", -1), c("tmplinit", 24, false, "", -1), c("tmpllexize", 24, false, "", -1)),
		r(IDPgType, "pg_catalog", "pg_type", 1247, c("oid", 26, false, "", -1), c("typname", 19, false, "", -1), c("typnamespace", 26, false, "", -1), c("typowner", 26, false, "", -1), c("typlen", 21, false, "physical_storage", -1), c("typbyval", 16, false, "physical_storage", -1), c("typtype", 18, false, "", -1), c("typcategory", 18, false, "", -1), c("typispreferred", 16, false, "", -1), c("typisdefined", 16, false, "", -1), c("typdelim", 18, false, "", -1), c("typrelid", 26, false, "", -1), c("typsubscript", 24, false, "", -1), c("typelem", 26, false, "", -1), c("typarray", 26, false, "", -1), c("typinput", 24, false, "", -1), c("typoutput", 24, false, "", -1), c("typreceive", 24, false, "", -1), c("typsend", 24, false, "", -1), c("typmodin", 24, false, "", -1), c("typmodout", 24, false, "", -1), c("typanalyze", 24, false, "", -1), c("typalign", 18, false, "physical_storage", -1), c("typstorage", 18, false, "physical_storage", -1), c("typnotnull", 16, false, "", -1), c("typbasetype", 26, false, "", -1), c("typtypmod", 23, false, "", -1), c("typndims", 23, false, "", -1), c("typcollation", 26, false, "", -1), c("typdefaultbin", 194, true, "", -1), c("typdefault", 25, true, "", -1), c("typacl", 1034, true, "", -1)),
		r(IDPgUser, "pg_catalog", "pg_user", 0, c("usename", 19, false, "", -1), c("usesysid", 26, false, "", -1), c("usecreatedb", 16, false, "", -1), c("usesuper", 16, false, "", -1), c("userepl", 16, false, "", -1), c("usebypassrls", 16, false, "", -1), c("passwd", 25, true, "", -1), c("valuntil", 1184, true, "", -1), c("useconfig", 1009, true, "", -1)),
		r(IDPgViews, "pg_catalog", "pg_views", 0, c("schemaname", 19, false, "", -1), c("viewname", 19, false, "", -1), c("viewowner", 19, false, "", -1), c("definition", 25, false, "", -1)),
		r(IDAttributes, "information_schema", "attributes", 0, c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("attribute_name", 1043, true, "", -1), c("ordinal_position", 23, true, "", -1), c("attribute_default", 1043, true, "", -1), c("is_nullable", 1043, true, "", 7), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("attribute_udt_catalog", 1043, true, "", -1), c("attribute_udt_schema", 1043, true, "", -1), c("attribute_udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1), c("is_derived_reference_attribute", 1043, true, "", 7)),
		r(IDCharacterSets, "information_schema", "character_sets", 0, c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("character_repertoire", 1043, true, "", -1), c("form_of_use", 1043, true, "", -1), c("default_collate_catalog", 1043, true, "", -1), c("default_collate_schema", 1043, true, "", -1), c("default_collate_name", 1043, true, "", -1)),
		r(IDCheckConstraintRoutineUsage, "information_schema", "check_constraint_routine_usage", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1)),
		r(IDCheckConstraints, "information_schema", "check_constraints", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("check_clause", 1043, true, "", -1)),
		r(IDCollationCharacterSetApplicability, "information_schema", "collation_character_set_applicability", 0, c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1)),
		r(IDCollations, "information_schema", "collations", 0, c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("pad_attribute", 1043, true, "", -1)),
		r(IDColumnColumnUsage, "information_schema", "column_column_usage", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1), c("dependent_column", 1043, true, "", -1)),
		r(IDColumnDomainUsage, "information_schema", "column_domain_usage", 0, c("domain_catalog", 1043, true, "", -1), c("domain_schema", 1043, true, "", -1), c("domain_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1)),
		r(IDColumnUdtUsage, "information_schema", "column_udt_usage", 0, c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1)),
		r(IDColumns, "information_schema", "columns", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1), c("ordinal_position", 23, true, "", -1), c("column_default", 1043, true, "", -1), c("is_nullable", 1043, true, "", 7), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("domain_catalog", 1043, true, "", -1), c("domain_schema", 1043, true, "", -1), c("domain_name", 1043, true, "", -1), c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1), c("is_self_referencing", 1043, true, "", 7), c("is_identity", 1043, true, "", 7), c("identity_generation", 1043, true, "", -1), c("identity_start", 1043, true, "", -1), c("identity_increment", 1043, true, "", -1), c("identity_maximum", 1043, true, "", -1), c("identity_minimum", 1043, true, "", -1), c("identity_cycle", 1043, true, "", 7), c("is_generated", 1043, true, "", -1), c("generation_expression", 1043, true, "", -1), c("is_updatable", 1043, true, "", 7)),
		r(IDConstraintColumnUsage, "information_schema", "constraint_column_usage", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1), c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1)),
		r(IDConstraintTableUsage, "information_schema", "constraint_table_usage", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1)),
		r(IDDomainConstraints, "information_schema", "domain_constraints", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("domain_catalog", 1043, true, "", -1), c("domain_schema", 1043, true, "", -1), c("domain_name", 1043, true, "", -1), c("is_deferrable", 1043, true, "", 7), c("initially_deferred", 1043, true, "", 7)),
		r(IDDomainUdtUsage, "information_schema", "domain_udt_usage", 0, c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("domain_catalog", 1043, true, "", -1), c("domain_schema", 1043, true, "", -1), c("domain_name", 1043, true, "", -1)),
		r(IDDomains, "information_schema", "domains", 0, c("domain_catalog", 1043, true, "", -1), c("domain_schema", 1043, true, "", -1), c("domain_name", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("domain_default", 1043, true, "", -1), c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1)),
		r(IDElementTypes, "information_schema", "element_types", 0, c("object_catalog", 1043, true, "", -1), c("object_schema", 1043, true, "", -1), c("object_name", 1043, true, "", -1), c("object_type", 1043, true, "", -1), c("collection_type_identifier", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1)),
		r(IDInformationSchemaCatalogName, "information_schema", "information_schema_catalog_name", 0, c("catalog_name", 1043, true, "", -1)),
		r(IDKeyColumnUsage, "information_schema", "key_column_usage", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1), c("ordinal_position", 23, true, "", -1), c("position_in_unique_constraint", 23, true, "", -1)),
		r(IDParameters, "information_schema", "parameters", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("ordinal_position", 23, true, "", -1), c("parameter_mode", 1043, true, "", -1), c("is_result", 1043, true, "", 7), c("as_locator", 1043, true, "", 7), c("parameter_name", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1), c("parameter_default", 1043, true, "", -1)),
		r(IDReferentialConstraints, "information_schema", "referential_constraints", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("unique_constraint_catalog", 1043, true, "", -1), c("unique_constraint_schema", 1043, true, "", -1), c("unique_constraint_name", 1043, true, "", -1), c("match_option", 1043, true, "", -1), c("update_rule", 1043, true, "", -1), c("delete_rule", 1043, true, "", -1)),
		r(IDRoutineColumnUsage, "information_schema", "routine_column_usage", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("routine_catalog", 1043, true, "", -1), c("routine_schema", 1043, true, "", -1), c("routine_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1)),
		r(IDRoutineRoutineUsage, "information_schema", "routine_routine_usage", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("routine_catalog", 1043, true, "", -1), c("routine_schema", 1043, true, "", -1), c("routine_name", 1043, true, "", -1)),
		r(IDRoutineSequenceUsage, "information_schema", "routine_sequence_usage", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("routine_catalog", 1043, true, "", -1), c("routine_schema", 1043, true, "", -1), c("routine_name", 1043, true, "", -1), c("sequence_catalog", 1043, true, "", -1), c("sequence_schema", 1043, true, "", -1), c("sequence_name", 1043, true, "", -1)),
		r(IDRoutineTableUsage, "information_schema", "routine_table_usage", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("routine_catalog", 1043, true, "", -1), c("routine_schema", 1043, true, "", -1), c("routine_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1)),
		r(IDRoutines, "information_schema", "routines", 0, c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1), c("routine_catalog", 1043, true, "", -1), c("routine_schema", 1043, true, "", -1), c("routine_name", 1043, true, "", -1), c("routine_type", 1043, true, "", -1), c("module_catalog", 1043, true, "", -1), c("module_schema", 1043, true, "", -1), c("module_name", 1043, true, "", -1), c("udt_catalog", 1043, true, "", -1), c("udt_schema", 1043, true, "", -1), c("udt_name", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("type_udt_catalog", 1043, true, "", -1), c("type_udt_schema", 1043, true, "", -1), c("type_udt_name", 1043, true, "", -1), c("scope_catalog", 1043, true, "", -1), c("scope_schema", 1043, true, "", -1), c("scope_name", 1043, true, "", -1), c("maximum_cardinality", 23, true, "", -1), c("dtd_identifier", 1043, true, "", -1), c("routine_body", 1043, true, "", -1), c("routine_definition", 1043, true, "", -1), c("external_name", 1043, true, "", -1), c("external_language", 1043, true, "", -1), c("parameter_style", 1043, true, "", -1), c("is_deterministic", 1043, true, "", 7), c("sql_data_access", 1043, true, "", -1), c("is_null_call", 1043, true, "", 7), c("sql_path", 1043, true, "", -1), c("schema_level_routine", 1043, true, "", 7), c("max_dynamic_result_sets", 23, true, "", -1), c("is_user_defined_cast", 1043, true, "", 7), c("is_implicitly_invocable", 1043, true, "", 7), c("security_type", 1043, true, "", -1), c("to_sql_specific_catalog", 1043, true, "", -1), c("to_sql_specific_schema", 1043, true, "", -1), c("to_sql_specific_name", 1043, true, "", -1), c("as_locator", 1043, true, "", 7), c("created", 1184, true, "", 2), c("last_altered", 1184, true, "", 2), c("new_savepoint_level", 1043, true, "", 7), c("is_udt_dependent", 1043, true, "", 7), c("result_cast_from_data_type", 1043, true, "", -1), c("result_cast_as_locator", 1043, true, "", 7), c("result_cast_char_max_length", 23, true, "", -1), c("result_cast_char_octet_length", 23, true, "", -1), c("result_cast_char_set_catalog", 1043, true, "", -1), c("result_cast_char_set_schema", 1043, true, "", -1), c("result_cast_char_set_name", 1043, true, "", -1), c("result_cast_collation_catalog", 1043, true, "", -1), c("result_cast_collation_schema", 1043, true, "", -1), c("result_cast_collation_name", 1043, true, "", -1), c("result_cast_numeric_precision", 23, true, "", -1), c("result_cast_numeric_precision_radix", 23, true, "", -1), c("result_cast_numeric_scale", 23, true, "", -1), c("result_cast_datetime_precision", 23, true, "", -1), c("result_cast_interval_type", 1043, true, "", -1), c("result_cast_interval_precision", 23, true, "", -1), c("result_cast_type_udt_catalog", 1043, true, "", -1), c("result_cast_type_udt_schema", 1043, true, "", -1), c("result_cast_type_udt_name", 1043, true, "", -1), c("result_cast_scope_catalog", 1043, true, "", -1), c("result_cast_scope_schema", 1043, true, "", -1), c("result_cast_scope_name", 1043, true, "", -1), c("result_cast_maximum_cardinality", 23, true, "", -1), c("result_cast_dtd_identifier", 1043, true, "", -1)),
		r(IDSchemata, "information_schema", "schemata", 0, c("catalog_name", 1043, true, "", -1), c("schema_name", 1043, true, "", -1), c("schema_owner", 1043, true, "", -1), c("default_character_set_catalog", 1043, true, "", -1), c("default_character_set_schema", 1043, true, "", -1), c("default_character_set_name", 1043, true, "", -1), c("sql_path", 1043, true, "", -1)),
		r(IDSequences, "information_schema", "sequences", 0, c("sequence_catalog", 1043, true, "", -1), c("sequence_schema", 1043, true, "", -1), c("sequence_name", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("start_value", 1043, true, "", -1), c("minimum_value", 1043, true, "", -1), c("maximum_value", 1043, true, "", -1), c("increment", 1043, true, "", -1), c("cycle_option", 1043, true, "", 7)),
		r(IDTableConstraints, "information_schema", "table_constraints", 0, c("constraint_catalog", 1043, true, "", -1), c("constraint_schema", 1043, true, "", -1), c("constraint_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("constraint_type", 1043, true, "", -1), c("is_deferrable", 1043, true, "", 7), c("initially_deferred", 1043, true, "", 7), c("enforced", 1043, true, "", 7), c("nulls_distinct", 1043, true, "", 7)),
		r(IDTables, "information_schema", "tables", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("table_type", 1043, true, "", -1), c("self_referencing_column_name", 1043, true, "", -1), c("reference_generation", 1043, true, "", -1), c("user_defined_type_catalog", 1043, true, "", -1), c("user_defined_type_schema", 1043, true, "", -1), c("user_defined_type_name", 1043, true, "", -1), c("is_insertable_into", 1043, true, "", 7), c("is_typed", 1043, true, "", 7), c("commit_action", 1043, true, "", -1)),
		r(IDTriggeredUpdateColumns, "information_schema", "triggered_update_columns", 0, c("trigger_catalog", 1043, true, "", -1), c("trigger_schema", 1043, true, "", -1), c("trigger_name", 1043, true, "", -1), c("event_object_catalog", 1043, true, "", -1), c("event_object_schema", 1043, true, "", -1), c("event_object_table", 1043, true, "", -1), c("event_object_column", 1043, true, "", -1)),
		r(IDTriggers, "information_schema", "triggers", 0, c("trigger_catalog", 1043, true, "", -1), c("trigger_schema", 1043, true, "", -1), c("trigger_name", 1043, true, "", -1), c("event_manipulation", 1043, true, "", -1), c("event_object_catalog", 1043, true, "", -1), c("event_object_schema", 1043, true, "", -1), c("event_object_table", 1043, true, "", -1), c("action_order", 23, true, "", -1), c("action_condition", 1043, true, "", -1), c("action_statement", 1043, true, "", -1), c("action_orientation", 1043, true, "", -1), c("action_timing", 1043, true, "", -1), c("action_reference_old_table", 1043, true, "", -1), c("action_reference_new_table", 1043, true, "", -1), c("action_reference_old_row", 1043, true, "", -1), c("action_reference_new_row", 1043, true, "", -1), c("created", 1184, true, "", 2)),
		r(IDUserDefinedTypes, "information_schema", "user_defined_types", 0, c("user_defined_type_catalog", 1043, true, "", -1), c("user_defined_type_schema", 1043, true, "", -1), c("user_defined_type_name", 1043, true, "", -1), c("user_defined_type_category", 1043, true, "", -1), c("is_instantiable", 1043, true, "", 7), c("is_final", 1043, true, "", 7), c("ordering_form", 1043, true, "", -1), c("ordering_category", 1043, true, "", -1), c("ordering_routine_catalog", 1043, true, "", -1), c("ordering_routine_schema", 1043, true, "", -1), c("ordering_routine_name", 1043, true, "", -1), c("reference_type", 1043, true, "", -1), c("data_type", 1043, true, "", -1), c("character_maximum_length", 23, true, "", -1), c("character_octet_length", 23, true, "", -1), c("character_set_catalog", 1043, true, "", -1), c("character_set_schema", 1043, true, "", -1), c("character_set_name", 1043, true, "", -1), c("collation_catalog", 1043, true, "", -1), c("collation_schema", 1043, true, "", -1), c("collation_name", 1043, true, "", -1), c("numeric_precision", 23, true, "", -1), c("numeric_precision_radix", 23, true, "", -1), c("numeric_scale", 23, true, "", -1), c("datetime_precision", 23, true, "", -1), c("interval_type", 1043, true, "", -1), c("interval_precision", 23, true, "", -1), c("source_dtd_identifier", 1043, true, "", -1), c("ref_dtd_identifier", 1043, true, "", -1)),
		r(IDViewColumnUsage, "information_schema", "view_column_usage", 0, c("view_catalog", 1043, true, "", -1), c("view_schema", 1043, true, "", -1), c("view_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("column_name", 1043, true, "", -1)),
		r(IDViewRoutineUsage, "information_schema", "view_routine_usage", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("specific_catalog", 1043, true, "", -1), c("specific_schema", 1043, true, "", -1), c("specific_name", 1043, true, "", -1)),
		r(IDViewTableUsage, "information_schema", "view_table_usage", 0, c("view_catalog", 1043, true, "", -1), c("view_schema", 1043, true, "", -1), c("view_name", 1043, true, "", -1), c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1)),
		r(IDViews, "information_schema", "views", 0, c("table_catalog", 1043, true, "", -1), c("table_schema", 1043, true, "", -1), c("table_name", 1043, true, "", -1), c("view_definition", 1043, true, "", -1), c("check_option", 1043, true, "", -1), c("is_updatable", 1043, true, "", 7), c("is_insertable_into", 1043, true, "", 7), c("is_trigger_updatable", 1043, true, "", 7), c("is_trigger_deletable", 1043, true, "", 7), c("is_trigger_insertable_into", 1043, true, "", 7)),
		r(IDSqlFeatures, "information_schema", "sql_features", 0, c("feature_id", 1043, true, "", -1), c("feature_name", 1043, true, "", -1), c("sub_feature_id", 1043, true, "", -1), c("sub_feature_name", 1043, true, "", -1), c("is_supported", 1043, true, "", 7), c("is_verified_by", 1043, true, "", -1), c("comments", 1043, true, "", -1)),
		r(IDSqlImplementationInfo, "information_schema", "sql_implementation_info", 0, c("implementation_info_id", 1043, true, "", -1), c("implementation_info_name", 1043, true, "", -1), c("integer_value", 23, true, "", -1), c("character_value", 1043, true, "", -1), c("comments", 1043, true, "", -1)),
		r(IDSqlParts, "information_schema", "sql_parts", 0, c("feature_id", 1043, true, "", -1), c("feature_name", 1043, true, "", -1), c("is_supported", 1043, true, "", 7), c("is_verified_by", 1043, true, "", -1), c("comments", 1043, true, "", -1)),
		r(IDSqlSizing, "information_schema", "sql_sizing", 0, c("sizing_id", 23, true, "", -1), c("sizing_name", 1043, true, "", -1), c("supported_value", 23, true, "", -1), c("comments", 1043, true, "", -1)),
	}
}

var informationDeclarations = []Declaration{
	{Name: "attributes", Kind: DeclarationView},
	{Name: "character_sets", Kind: DeclarationView},
	{Name: "check_constraint_routine_usage", Kind: DeclarationView},
	{Name: "check_constraints", Kind: DeclarationView},
	{Name: "collation_character_set_applicability", Kind: DeclarationView},
	{Name: "collations", Kind: DeclarationView},
	{Name: "column_column_usage", Kind: DeclarationView},
	{Name: "column_domain_usage", Kind: DeclarationView},
	{Name: "column_udt_usage", Kind: DeclarationView},
	{Name: "columns", Kind: DeclarationView},
	{Name: "constraint_column_usage", Kind: DeclarationView},
	{Name: "constraint_table_usage", Kind: DeclarationView},
	{Name: "domain_constraints", Kind: DeclarationView},
	{Name: "domain_udt_usage", Kind: DeclarationView},
	{Name: "domains", Kind: DeclarationView},
	{Name: "element_types", Kind: DeclarationView},
	{Name: "information_schema_catalog_name", Kind: DeclarationView},
	{Name: "key_column_usage", Kind: DeclarationView},
	{Name: "parameters", Kind: DeclarationView},
	{Name: "referential_constraints", Kind: DeclarationView},
	{Name: "routine_column_usage", Kind: DeclarationView},
	{Name: "routine_routine_usage", Kind: DeclarationView},
	{Name: "routine_sequence_usage", Kind: DeclarationView},
	{Name: "routine_table_usage", Kind: DeclarationView},
	{Name: "routines", Kind: DeclarationView},
	{Name: "schemata", Kind: DeclarationView},
	{Name: "sequences", Kind: DeclarationView},
	{Name: "table_constraints", Kind: DeclarationView},
	{Name: "tables", Kind: DeclarationView},
	{Name: "triggered_update_columns", Kind: DeclarationView},
	{Name: "triggers", Kind: DeclarationView},
	{Name: "user_defined_types", Kind: DeclarationView},
	{Name: "view_column_usage", Kind: DeclarationView},
	{Name: "view_routine_usage", Kind: DeclarationView},
	{Name: "view_table_usage", Kind: DeclarationView},
	{Name: "views", Kind: DeclarationView},
	{Name: "sql_features", Kind: DeclarationTable},
	{Name: "sql_implementation_info", Kind: DeclarationTable},
	{Name: "sql_parts", Kind: DeclarationTable},
	{Name: "sql_sizing", Kind: DeclarationTable},
	{Name: "cardinal_number", Kind: DeclarationDomain},
	{Name: "character_data", Kind: DeclarationDomain},
	{Name: "sql_identifier", Kind: DeclarationDomain},
	{Name: "time_stamp", Kind: DeclarationDomain},
	{Name: "yes_or_no", Kind: DeclarationDomain},
	{Name: "_pg_char_max_length", Kind: DeclarationFunction},
	{Name: "_pg_char_octet_length", Kind: DeclarationFunction},
	{Name: "_pg_datetime_precision", Kind: DeclarationFunction},
	{Name: "_pg_expandarray", Kind: DeclarationFunction},
	{Name: "_pg_index_position", Kind: DeclarationFunction},
	{Name: "_pg_interval_type", Kind: DeclarationFunction},
	{Name: "_pg_numeric_precision", Kind: DeclarationFunction},
	{Name: "_pg_numeric_precision_radix", Kind: DeclarationFunction},
	{Name: "_pg_numeric_scale", Kind: DeclarationFunction},
	{Name: "_pg_truetypid", Kind: DeclarationFunction},
	{Name: "_pg_truetypmod", Kind: DeclarationFunction},
}

func r(id RelationID, namespace, name string, oid model.OID, columns ...Column) Relation {
	for i := range columns {
		columns[i].ID = model.ColumnID(i + 1)
	}
	identity := RelationIdentity{RelationOID: oid}
	if namespace == "pg_catalog" {
		for info := range builtin.TypesNamed(name) {
			if info.Kind == 'c' {
				identity.RowTypeOID = model.OID(info.ID)
				identity.ArrayTypeOID = model.OID(info.Array)
				break
			}
		}
	}
	return Relation{ID: id, Namespace: namespace, Name: name, Identity: identity, Columns: columns}
}

func c(name string, typeID model.TypeID, nullable bool, unsupported string, modifier int32) Column {
	return Column{Name: name, Layout: catalogLayout(typeID, modifier), Nullable: nullable, UnsupportedFeature: unsupported}
}

func catalogLayout(typeID model.TypeID, modifier int32) model.TypeLayout {
	layout := model.TypeLayout{Type: model.Type{ID: typeID, Modifier: modifier}}
	// Preserve pg_catalog.default (100); execution resolves it to the configured concrete collation.
	if info, ok := builtin.LookupType(typeID); ok {
		layout.Type.Collation = info.Collation
	}
	switch typeID {
	case 16:
		layout.Type.Storage = model.TypeBoolean
	case 17, 3361, 3402, 5017:
		layout.Type.Storage = model.TypeBytes
	case 18, 19, 25, 194, 1043:
		layout.Type.Storage = model.TypeString
	case 20, 21, 23, 24, 26, 28, 2206:
		layout.Type.Storage = model.TypeInt64
	case 22:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(21, -1)}
	case 30:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(26, -1)}
	case 700:
		layout.Type.Storage = model.TypeFloat64
	case 1184:
		layout.Type.Storage = model.TypeTimestamp
	case 1002:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(18, -1)}
	case 1005:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(21, -1)}
	case 1009:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(25, -1)}
	case 1021:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(700, -1)}
	case 1028:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(26, -1)}
	case 1034:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{{Type: model.Type{ID: 1033, Storage: model.TypeString, Modifier: -1}}}
	case 2277:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{catalogLayout(25, -1)}
	case 2287:
		layout.Type.Storage = model.TypeArray
		layout.Children = []model.TypeLayout{{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}}}
	default:
		if info, ok := builtin.LookupType(typeID); ok && info.IsArray() {
			layout.Type.Storage = model.TypeArray
			layout.Type.Collation = info.Collation
			layout.Children = []model.TypeLayout{catalogLayout(info.Element, -1)}
		}
	}
	return layout
}
