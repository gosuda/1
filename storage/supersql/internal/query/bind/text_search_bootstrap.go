package bind

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	nativeSnowballInitHandler   = "text_search.snowball_init"
	nativeSnowballLexizeHandler = "text_search.snowball_lexize"
)

var nativeSnowballLanguages = [...]string{
	"arabic", "armenian", "basque", "catalan", "danish", "dutch", "english", "estonian", "finnish", "french", "german", "greek", "hindi", "hungarian", "indonesian", "irish", "italian", "lithuanian", "nepali", "norwegian", "portuguese", "romanian", "russian", "serbian", "spanish", "swedish", "tamil", "turkish", "yiddish",
}

// BindBootstrapNativeTextSearch creates the initdb-defined native Snowball
// routines, template, dictionaries, and configurations under pg_catalog. Every
// non-core identity comes from the durable catalog allocator; callers publish
// the returned declaration in their bootstrap transaction.
func BindBootstrapNativeTextSearch(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if snapshot == nil || options.Identities == nil {
		return nil, bindError("22023", "text search bootstrap requires catalog and identity allocation", 0)
	}
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: "pg_catalog"})
	if err != nil || published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		if err == nil { err = catalog.ErrUnknownObject }
		return nil, errors.Join(bindError("3F000", "pg_catalog schema is unavailable", 0), err)
	}
	schema := published.Descriptor
	state := nativeTextSearchBootstrap{ctx: ctx, snapshot: snapshot, options: options, schema: schema}
	internal := model.TypeLayout{Type: model.Type{ID: 2281, Modifier: -1, Storage: model.TypeAny}}
	initRoutine := model.RoutineMetadata{
		Owner: options.Owner, LanguageOID: 12, Language: "internal", Volatility: "volatile", Parallel: "unsafe", Cost: 100, Strict: true,
		Parameters: []model.RoutineParameter{{Layout: internal, Mode: "in"}}, Result: internal,
		ExternalCode: nativeSnowballInitHandler,
	}
	initDescriptor, err := state.routine("dsnowball_init", initRoutine)
	if err != nil { return nil, err }
	if !nativeSnowballRoutineMatches(initDescriptor, initRoutine) {
		return nil, bindError("42710", "incompatible pg_catalog.dsnowball_init bootstrap routine", 0)
	}
	lexizeRoutine := model.RoutineMetadata{
		Owner: options.Owner, LanguageOID: 12, Language: "internal", Volatility: "volatile", Parallel: "unsafe", Cost: 100, Strict: true,
		Parameters: []model.RoutineParameter{{Layout: internal, Mode: "in"}, {Layout: internal, Mode: "in"}, {Layout: internal, Mode: "in"}, {Layout: internal, Mode: "in"}}, Result: internal,
		ExternalCode: nativeSnowballLexizeHandler,
	}
	lexizeDescriptor, err := state.routine("dsnowball_lexize", lexizeRoutine)
	if err != nil { return nil, err }
	if !nativeSnowballRoutineMatches(lexizeDescriptor, lexizeRoutine) {
		return nil, bindError("42710", "incompatible pg_catalog.dsnowball_lexize bootstrap routine", 0)
	}
	template, err := state.object(model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchTemplate, Name: "snowball"}, func(id model.ObjectID, oid model.OID) model.DescriptorSpec {
		return model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchTemplate, Name: "snowball"}, Description: "snowball stemmer", Version: 1, State: model.StatePublic,
			Dependencies: textSearchDescriptorDependencies([]*model.Descriptor{schema, initDescriptor, lexizeDescriptor}),
			TextSearchTemplate: &model.TextSearchTemplateMetadata{Owner: options.Owner, Init: initDescriptor.OID(), Lexize: lexizeDescriptor.OID()},
		}
	})
	if err != nil { return nil, err }
	templateMetadata, ok := template.TextSearchTemplate()
	if !ok || templateMetadata.Owner != options.Owner || templateMetadata.Init != initDescriptor.OID() || templateMetadata.Lexize != lexizeDescriptor.OID() {
		return nil, bindError("42710", "incompatible pg_catalog.snowball bootstrap template", 0)
	}
	dictionaries := make(map[string]*model.Descriptor, len(nativeSnowballLanguages))
	for _, language := range nativeSnowballLanguages {
		stopWords := ""
		if model.HasNativeTextSearchStopWords(language) { stopWords = language }
		metadata := model.TextSearchDictionaryMetadata{Owner: options.Owner, Template: template.OID(), Kind: model.TextSearchSnowballDictionary, Language: language, StopWordsLanguage: stopWords}
		metadata.InitOptions = textSearchDictionaryInitOptions(textSearchSnowballInitOptions(language, stopWords))
		name := language + "_stem"
		dictionary, createErr := state.object(model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchDictionary, Name: name}, func(id model.ObjectID, oid model.OID) model.DescriptorSpec {
			return model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchDictionary, Name: name}, Description: "snowball stemmer for " + language + " language", Version: 1, State: model.StatePublic,
				Dependencies: textSearchDescriptorDependencies([]*model.Descriptor{schema, template}), TextSearchDictionary: &metadata,
			}
		})
		if createErr != nil { return nil, createErr }
		actual, ok := dictionary.TextSearchDictionary()
		if !ok || !equalNativeTextSearchDictionary(actual, metadata) {
			return nil, bindError("42710", "incompatible native Snowball bootstrap dictionary", 0)
		}
		dictionaries[language] = dictionary
	}
	for _, language := range nativeSnowballLanguages {
		asciiLanguage := language
		if language == "hindi" || language == "russian" { asciiLanguage = "english" }
		metadata := model.TextSearchConfigurationMetadata{Owner: options.Owner, Parser: defaultTextSearchParserOID}
		metadata.Mappings = nativeSnowballMappings(dictionaries[language].OID(), dictionaries[asciiLanguage].OID())
		dependencies := []*model.Descriptor{schema, dictionaries[language]}
		if asciiLanguage != language { dependencies = append(dependencies, dictionaries[asciiLanguage]) }
		configuration, createErr := state.object(model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchConfiguration, Name: language}, func(id model.ObjectID, oid model.OID) model.DescriptorSpec {
			return model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectTextSearchConfiguration, Name: language}, Description: "configuration for " + language + " language", Version: 1, State: model.StatePublic,
				Dependencies: textSearchDescriptorDependencies(dependencies), TextSearchConfiguration: &metadata,
			}
		})
		if createErr != nil { return nil, createErr }
		actual, ok := configuration.TextSearchConfiguration()
		if !ok || !equalNativeTextSearchConfiguration(actual, metadata) {
			return nil, bindError("42710", "incompatible native Snowball bootstrap configuration", 0)
		}
	}
	if err := ctx.Err(); err != nil { return nil, err }
	state.finalizeDependencies()
	if len(state.created) == 0 {
		return &BoundDeclaration{existing: template, dependencies: state.dependencies}, nil
	}
	return &BoundDeclaration{descriptors: state.created, dependencies: state.dependencies}, nil
}

type nativeTextSearchBootstrap struct {
	ctx context.Context
	snapshot CatalogSnapshot
	options CreateDeclarationOptions
	schema *model.Descriptor
	created []*model.Descriptor
	dependencies []model.Dependency
}

func (s *nativeTextSearchBootstrap) routine(name string, metadata model.RoutineMetadata) (*model.Descriptor, error) {
	identity := model.NamespaceName{Parent: s.schema.ID(), Kind: model.ObjectRoutine, Name: name, Signature: routineSignature(metadata.Parameters)}
	return s.object(identity, func(id model.ObjectID, oid model.OID) model.DescriptorSpec {
		return model.DescriptorSpec{ID: id, OID: oid, Name: identity, Version: 1, State: model.StatePublic,
			Dependencies: textSearchDescriptorDependencies([]*model.Descriptor{s.schema}), Routine: &metadata,
		}
	})
}

func (s *nativeTextSearchBootstrap) object(identity model.NamespaceName, create func(model.ObjectID, model.OID) model.DescriptorSpec) (*model.Descriptor, error) {
	if err := s.ctx.Err(); err != nil { return nil, err }
	published, err := s.snapshot.Resolve(s.ctx, identity)
	if err == nil {
		descriptor := published.Descriptor
		if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != identity.Kind {
			return nil, bindError("42710", "incompatible native text search bootstrap object", 0)
		}
		s.dependencies = append(s.dependencies, model.Dependency{Object: descriptor.ID(), Version: descriptor.Version()})
		return descriptor, nil
	}
	if !isMissingObject(err) { return nil, err }
	id, oid, err := s.options.Identities.AllocateDeclarationIdentity(s.ctx)
	if err != nil { return nil, err }
	descriptor, err := model.NewDescriptor(s.ctx, create(id, oid), s.options.Descriptors)
	if err != nil { return nil, err }
	s.created = append(s.created, descriptor)
	return descriptor, nil
}

func nativeSnowballRoutineMatches(descriptor *model.Descriptor, expected model.RoutineMetadata) bool {
	actual, ok := descriptor.Routine()
	if !ok {
		return false
	}
	if actual.Owner != expected.Owner || actual.LanguageOID != expected.LanguageOID || actual.Language != expected.Language || actual.Volatility != expected.Volatility || actual.Parallel != expected.Parallel || actual.Cost != expected.Cost || actual.Strict != expected.Strict || actual.ExternalCode != expected.ExternalCode || actual.Source != "" || actual.Binary != "" || len(actual.Settings) != 0 || len(actual.Parameters) != len(expected.Parameters) {
		return false
	}
	if actual.Result.Type != expected.Result.Type || len(actual.Result.Children) != 0 || len(actual.Result.FieldNames) != 0 {
		return false
	}
	for i := range actual.Parameters {
		if actual.Parameters[i].Name != "" || actual.Parameters[i].Mode != expected.Parameters[i].Mode || actual.Parameters[i].Layout.Type != expected.Parameters[i].Layout.Type || len(actual.Parameters[i].Layout.Children) != 0 || len(actual.Parameters[i].Layout.FieldNames) != 0 {
			return false
		}
	}
	return !actual.ReturnsSet && !actual.Procedure && !actual.Atomic && !actual.SecurityDefiner && !actual.Leakproof && actual.Support == 0 && actual.SupportFunction == 0 && actual.Rows == 0 && len(actual.Body.Encoding) == 0
}

func equalNativeTextSearchDictionary(actual, expected model.TextSearchDictionaryMetadata) bool {
	return actual.Owner == expected.Owner && actual.Template == expected.Template && actual.Kind == expected.Kind && actual.Language == expected.Language && actual.StopWordsLanguage == expected.StopWordsLanguage && actual.Accept == expected.Accept && actual.InitOptions == expected.InitOptions && slices.Equal(actual.StopWords, expected.StopWords)
}

func equalNativeTextSearchConfiguration(actual, expected model.TextSearchConfigurationMetadata) bool {
	if actual.Owner != expected.Owner || actual.Parser != expected.Parser || len(actual.Mappings) != len(expected.Mappings) {
		return false
	}
	for i := range actual.Mappings {
		if actual.Mappings[i].TokenType != expected.Mappings[i].TokenType || !slices.Equal(actual.Mappings[i].Dictionaries, expected.Mappings[i].Dictionaries) {
			return false
		}
	}
	return true
}

func nativeSnowballMappings(dictionary, asciiDictionary model.OID) []model.TextSearchMappingReference {
	mappings := make([]model.TextSearchMappingReference, 0, 19)
	for token := uint16(1); token <= 23; token++ {
		var oid model.OID
		switch token {
		case 3, 4, 5, 6, 7, 8, 9, 15, 18, 19, 20, 21, 22:
			oid = simpleTextSearchDictionaryOID
		case 1, 11, 16:
			oid = asciiDictionary
		case 2, 10, 17:
			oid = dictionary
		}
		if oid != 0 { mappings = append(mappings, model.TextSearchMappingReference{TokenType: token, Dictionaries: []model.OID{oid}}) }
	}
	return mappings
}

func (s *nativeTextSearchBootstrap) finalizeDependencies() {
	slices.SortFunc(s.dependencies, compareTextSearchDependency)
	s.dependencies = slices.Compact(s.dependencies)
}
