package bind

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	defaultTextSearchParserOID    model.OID = 3722
	simpleTextSearchTemplateOID   model.OID = 3727
	synonymTextSearchTemplateOID  model.OID = 3730
	ispellTextSearchTemplateOID   model.OID = 3733
	thesaurusTextSearchTemplateOID model.OID = 3742
	simpleTextSearchDictionaryOID model.OID = 3765
)

// BindCreateTextSearch binds native CREATE TEXT SEARCH DICTIONARY and
// CONFIGURATION declarations. Parser and template declarations are external
// code and are rejected by statement feature validation.
func BindCreateTextSearch(ctx context.Context, statement *zpgparse.DefineStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE TEXT SEARCH declaration", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DefineStmt{DefineStmt: statement}})
	if err != nil {
		return nil, err
	}
	var kind model.ObjectKind
	switch statement.Kind {
	case zpgparse.ObjectType_OBJECT_TSDICTIONARY:
		kind = model.ObjectTextSearchDictionary
	case zpgparse.ObjectType_OBJECT_TSCONFIGURATION:
		kind = model.ObjectTextSearchConfiguration
	default:
		return nil, bindError("42809", "declaration is not a native text search dictionary or configuration", 0)
	}
	parts, err := names(statement.Defnames)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid text search object name", 0)
	}
	name := parts[len(parts)-1]
	if err := declarationName(name); err != nil {
		return nil, err
	}
	schema, err := resolveRoutineSchema(ctx, snapshot, options.Bind.SearchPath, parts)
	if err != nil {
		return nil, err
	}
	b.dependency(schema, 0)
	identity := model.NamespaceName{Parent: schema.ID(), Kind: kind, Name: name}
	published, err := snapshot.Resolve(ctx, identity)
	if err == nil && published.Descriptor != nil {
		return nil, bindError("42710", "text search object already exists", 0)
	}
	if err != nil && !isMissingObject(err) {
		return nil, err
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	spec := model.DescriptorSpec{ID: id, OID: oid, Name: identity, Version: 1, State: model.StatePublic}
	semanticDependencies := []*model.Descriptor{schema}
	switch kind {
	case model.ObjectTextSearchDictionary:
		metadata, template, parseErr := bindTextSearchDictionaryDefinition(b, statement.Definition, options.Owner)
		if parseErr != nil {
			return nil, parseErr
		}
		if template != nil {
			semanticDependencies = append(semanticDependencies, template)
		}
		spec.TextSearchDictionary = &metadata
	case model.ObjectTextSearchConfiguration:
		metadata, dependencies, parseErr := bindTextSearchConfigurationDefinition(ctx, b, snapshot, statement.Definition, options.Owner)
		if parseErr != nil {
			return nil, parseErr
		}
		for _, dependency := range dependencies {
			b.dependency(dependency, 0)
		}
		semanticDependencies = append(semanticDependencies, dependencies...)
		spec.TextSearchConfiguration = &metadata
	}
	spec.Dependencies = textSearchDescriptorDependencies(semanticDependencies)
	descriptor, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{descriptor}, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func BindAlterTextSearchDictionary(ctx context.Context, statement *zpgparse.AlterTSDictionaryStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil {
		return nil, bindError("42601", "missing ALTER TEXT SEARCH DICTIONARY", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTsdictionaryStmt{AlterTsdictionaryStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Dictname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid text search dictionary name", 0)
	}
	descriptor, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchDictionary}, "42704", 0)
	if err != nil {
		return nil, err
	}
	if descriptor.State() != model.StatePublic {
		return nil, bindError("42704", "text search dictionary does not exist", 0)
	}
	b.dependency(descriptor, 0)
	spec, err := descriptor.Spec(ctx)
	if err != nil {
		return nil, err
	}
	metadata := spec.TextSearchDictionary
	if metadata == nil {
		return nil, bindError("42809", "object is not a text search dictionary", 0)
	}
	merged, err := mergeTextSearchDictionaryOptions(metadata.InitOptions, statement.Options)
	if err != nil {
		return nil, err
	}
	entries, err := applyTextSearchDictionaryOptions(b, metadata, merged, false)
	if err != nil {
		return nil, err
	}
	metadata.InitOptions = textSearchDictionaryInitOptions(entries)
	if err := verifyNativeTextSearchDictionary(metadata); err != nil {
		return nil, err
	}
	spec.Version = descriptor.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{next}, base: descriptor, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

// mergeTextSearchDictionaryOptions reproduces ALTER TEXT SEARCH DICTIONARY's
// option handling: decode the stored dictinitoption, drop every entry the
// statement also names, and append the statement's own entries in order. An
// option written without a value removes the stored one and adds nothing.
func mergeTextSearchDictionaryOptions(stored string, definition []*zpgparse.Node) ([]textSearchOptionInput, error) {
	merged, err := decodeTextSearchDictionaryOptions(stored)
	if err != nil {
		return nil, err
	}
	for _, node := range definition {
		option := node.GetDefElem()
		if option == nil {
			return nil, bindError("42601", "invalid text search dictionary option", 0)
		}
		merged = slices.DeleteFunc(merged, func(existing textSearchOptionInput) bool {
			return strings.EqualFold(existing.defname, option.Defname)
		})
		if option.Arg == nil {
			continue
		}
		value, err := textSearchDefinitionValue(option.Arg)
		if err != nil {
			return nil, err
		}
		merged = append(merged, textSearchOptionInput{
			defname:  option.Defname,
			value:    value,
			numeric:  isTextSearchNumericArg(option.Arg),
			location: int(option.Location) + 1,
		})
	}
	return merged, nil
}

// decodeTextSearchDictionaryOptions parses the subset of deserialize_deflist
// this engine emits: an identifier or quoted name, '=', then a bare number or
// a single-quoted string with optional E prefix and doubled quotes.
func decodeTextSearchDictionaryOptions(stored string) ([]textSearchOptionInput, error) {
	options := make([]textSearchOptionInput, 0, 4)
	remaining := stored
	for {
		remaining = strings.TrimLeft(remaining, " \t\n\r")
		if remaining == "" {
			return options, nil
		}
		name, rest, err := decodeTextSearchOptionName(remaining)
		if err != nil {
			return nil, err
		}
		rest = strings.TrimLeft(rest, " \t\n\r")
		if !strings.HasPrefix(rest, "=") {
			return nil, bindError("XX000", "unrecognized deserialize_deflist state", 0)
		}
		rest = strings.TrimLeft(rest[1:], " \t\n\r")
		value, numeric, rest, err := decodeTextSearchOptionValue(rest)
		if err != nil {
			return nil, err
		}
		options = append(options, textSearchOptionInput{defname: name, value: value, numeric: numeric})
		rest = strings.TrimLeft(rest, " \t\n\r")
		if rest == "" {
			return options, nil
		}
		if !strings.HasPrefix(rest, ",") {
			return nil, bindError("XX000", "unrecognized deserialize_deflist state", 0)
		}
		remaining = rest[1:]
	}
}

func decodeTextSearchOptionName(input string) (name, rest string, err error) {
	if strings.HasPrefix(input, `"`) {
		var builder strings.Builder
		for index := 1; index < len(input); index++ {
			if input[index] != '"' {
				builder.WriteByte(input[index])
				continue
			}
			if index+1 < len(input) && input[index+1] == '"' {
				builder.WriteByte('"')
				index++
				continue
			}
			return builder.String(), input[index+1:], nil
		}
		return "", "", bindError("XX000", "unrecognized deserialize_deflist state", 0)
	}
	end := strings.IndexAny(input, " \t\n\r=")
	if end <= 0 {
		return "", "", bindError("XX000", "unrecognized deserialize_deflist state", 0)
	}
	return input[:end], input[end:], nil
}

func decodeTextSearchOptionValue(input string) (value string, numeric bool, rest string, err error) {
	if input == "" {
		return "", false, "", bindError("XX000", "unrecognized deserialize_deflist state", 0)
	}
	if input[0] != '\'' {
		end := strings.IndexAny(input, " \t\n\r,")
		if end < 0 {
			return input, true, "", nil
		}
		return input[:end], true, input[end:], nil
	}
	escaped := false
	start := 0
	if strings.HasPrefix(input, "E'") {
		escaped = true
		start = 1
	}
	var builder strings.Builder
	for index := start + 1; index < len(input); index++ {
		if input[index] != '\'' {
			builder.WriteByte(input[index])
			continue
		}
		if index+1 < len(input) && input[index+1] == '\'' {
			builder.WriteByte('\'')
			index++
			continue
		}
		value = builder.String()
		if escaped {
			value = decodeTextSearchEscapeString(value)
		}
		return value, false, input[index+1:], nil
	}
	return "", false, "", bindError("XX000", "unrecognized deserialize_deflist state", 0)
}

// decodeTextSearchEscapeString undoes the C-style escapes serialize_deflist's
// E prefix implies, so an option value containing a backslash round-trips.
func decodeTextSearchEscapeString(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 == len(value) {
			builder.WriteByte(value[index])
			continue
		}
		index++
		switch value[index] {
		case 'n':
			builder.WriteByte('\n')
		case 't':
			builder.WriteByte('\t')
		case 'r':
			builder.WriteByte('\r')
		default:
			builder.WriteByte(value[index])
		}
	}
	return builder.String()
}

func BindAlterTextSearchConfiguration(ctx context.Context, statement *zpgparse.AlterTSConfigurationStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil {
		return nil, bindError("42601", "missing ALTER TEXT SEARCH CONFIGURATION", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTsconfigurationStmt{AlterTsconfigurationStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Cfgname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid text search configuration name", 0)
	}
	descriptor, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchConfiguration}, "42704", 0)
	if err != nil {
		return nil, err
	}
	if descriptor.State() != model.StatePublic {
		return nil, bindError("42704", "text search configuration does not exist", 0)
	}
	b.dependency(descriptor, 0)
	spec, err := descriptor.Spec(ctx)
	if err != nil {
		return nil, err
	}
	metadata := spec.TextSearchConfiguration
	if metadata == nil {
		return nil, bindError("42809", "object is not a text search configuration", 0)
	}
	resolved := make(map[model.OID]*model.Descriptor)
	if err := alterTextSearchMappings(ctx, b, statement, metadata, resolved); err != nil {
		return nil, err
	}
	dependencies, err := textSearchConfigurationDependencies(ctx, snapshot, spec.Name.Parent, metadata.Mappings, spec.Dependencies, resolved)
	if err != nil {
		return nil, err
	}
	for _, dependency := range dependencies {
		b.dependency(dependency, 0)
	}
	spec.Dependencies = make([]model.Dependency, len(dependencies))
	for i, dependency := range dependencies {
		spec.Dependencies[i] = model.Dependency{Object: dependency.ID(), Version: dependency.Version()}
	}
	slices.SortFunc(spec.Dependencies, compareTextSearchDependency)
	spec.Dependencies = slices.Compact(spec.Dependencies)
	spec.Version = descriptor.Version() + 1
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{next}, base: descriptor, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func bindTextSearchDictionaryDefinition(b *binder, definition []*zpgparse.Node, owner model.OID) (model.TextSearchDictionaryMetadata, *model.Descriptor, error) {
	metadata := model.TextSearchDictionaryMetadata{Owner: owner}
	var dependency *model.Descriptor
	seenTemplate := false
	for _, node := range definition {
		option := node.GetDefElem()
		if option == nil {
			return metadata, nil, bindError("42601", "invalid text search dictionary option", 0)
		}
		if !strings.EqualFold(option.Defname, "template") {
			continue
		}
		if seenTemplate {
			return metadata, nil, bindError("42601", "text search dictionary template specified more than once", int(option.Location)+1)
		}
		seenTemplate = true
		value, err := textSearchDefinitionValue(option.Arg)
		if err != nil {
			return metadata, nil, err
		}
		if oid, kind, ok := pinnedTextSearchTemplate(value); ok {
			metadata.Template, metadata.Kind = oid, kind
			continue
		}
		parts, err := parseTextSearchQualifiedValue(value)
		if err != nil {
			return metadata, nil, err
		}
		template, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchTemplate}, "42704", int(option.Location)+1)
		if err != nil {
			return metadata, nil, err
		}
		if _, ok := template.TextSearchTemplate(); !ok {
			return metadata, nil, bindError("42809", "object is not a text search template", int(option.Location)+1)
		}
		metadata.Template, metadata.Kind, dependency = template.OID(), model.TextSearchSnowballDictionary, template
	}
	if !seenTemplate {
		return metadata, nil, bindError("42601", "text search dictionary template is required", 0)
	}
	options, err := textSearchNodeOptions(definition)
	if err != nil {
		return metadata, nil, err
	}
	entries, err := applyTextSearchDictionaryOptions(b, &metadata, options, true)
	if err != nil {
		return metadata, nil, err
	}
	metadata.InitOptions = textSearchDictionaryInitOptions(entries)
	if err := verifyNativeTextSearchDictionary(&metadata); err != nil {
		return metadata, nil, err
	}
	return metadata, dependency, nil
}

// pinnedTextSearchTemplate recognizes the initdb templates whose init and
// lexize functions are compiled into this engine. They share the byte layout
// PostgreSQL's initdb installs, so CREATE TEXT SEARCH DICTIONARY can name them
// directly instead of requiring a pg_ts_template row this engine never writes.
func pinnedTextSearchTemplate(name string) (model.OID, model.TextSearchDictionaryKind, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "simple", "pg_catalog.simple":
		return simpleTextSearchTemplateOID, model.TextSearchSimpleDictionary, true
	case "synonym", "pg_catalog.synonym":
		return synonymTextSearchTemplateOID, model.TextSearchSynonymDictionary, true
	case "ispell", "pg_catalog.ispell":
		return ispellTextSearchTemplateOID, model.TextSearchIspellDictionary, true
	case "thesaurus", "pg_catalog.thesaurus":
		return thesaurusTextSearchTemplateOID, model.TextSearchThesaurusDictionary, true
	default:
		return 0, 0, false
	}
}

// textSearchOptionInput is one dictionary option already reduced to its
// serializable form. CREATE builds these from the parse tree; ALTER merges them
// with the options decoded from the stored dictinitoption.
type textSearchOptionInput struct {
	defname string
	value   string
	numeric bool
	// location is the 1-based byte offset of the option, used in diagnostics.
	location int
}

// applyTextSearchDictionaryOptions validates the option list the way each
// template's init function does and returns it in the form serialize_deflist
// stores. PostgreSQL validates at CREATE time, so the diagnostics and
// SQLSTATEs must match here rather than at lexize.
func applyTextSearchDictionaryOptions(b *binder, metadata *model.TextSearchDictionaryMetadata, options []textSearchOptionInput, creating bool) ([]textSearchInitOption, error) {
	entries := make([]textSearchInitOption, 0, len(options))
	seen := make(map[string]struct{}, len(options))
	for _, option := range options {
		if strings.EqualFold(option.defname, "template") && creating {
			continue
		}
		name := strings.ToLower(option.defname)
		if _, duplicate := seen[name]; duplicate {
			return nil, bindError("42601", "text search dictionary option specified more than once", option.location)
		}
		seen[name] = struct{}{}
		value := option.value
		location := option.location
		entry := textSearchInitOption{name: name, value: value, numeric: option.numeric}
		switch metadata.Kind {
		case model.TextSearchSimpleDictionary:
			switch name {
			case "accept":
				accept, ok := textSearchBoolean(value)
				if !ok {
					return nil, bindError("22023", "Accept must be a Boolean value", location)
				}
				metadata.Accept = accept
			case "stopwords":
				language := strings.ToLower(value)
				if !model.HasNativeTextSearchStopWords(language) {
					return nil, bindError("0A000", "external text search stop-word files are unavailable", location)
				}
				metadata.StopWordsLanguage = language
			default:
				return nil, bindError("22023", "unrecognized simple dictionary parameter: \""+option.defname+"\"", location)
			}
		case model.TextSearchSnowballDictionary:
			switch name {
			case "language":
				language := strings.ToLower(value)
				if !model.ValidTextSearchSnowballLanguage(language) {
					return nil, bindError("22023", "unknown native Snowball language", location)
				}
				metadata.Language = language
			case "stopwords":
				language := strings.ToLower(value)
				if !model.HasNativeTextSearchStopWords(language) {
					return nil, bindError("0A000", "external text search stop-word files are unavailable", location)
				}
				metadata.StopWordsLanguage = language
			default:
				return nil, bindError("22023", "unrecognized Snowball parameter: \""+option.defname+"\"", location)
			}
		case model.TextSearchIspellDictionary:
			switch name {
			case "dictfile":
				metadata.DictionaryFile = value
			case "afffile":
				metadata.AffixFile = value
			case "stopwords":
				language := strings.ToLower(value)
				if !model.HasNativeTextSearchStopWords(language) {
					return nil, bindError("0A000", "external text search stop-word files are unavailable", location)
				}
				metadata.StopWordsLanguage = language
			default:
				return nil, bindError("22023", "unrecognized Ispell parameter: \""+option.defname+"\"", location)
			}
		case model.TextSearchSynonymDictionary:
			switch name {
			case "synonyms":
				metadata.SynonymsFile = value
			case "casesensitive":
				flag, ok := textSearchBoolean(value)
				if !ok {
					return nil, bindError("22023", "casesensitive requires a Boolean value", location)
				}
				metadata.CaseSensitive = flag
			default:
				return nil, bindError("22023", "unrecognized synonym parameter: \""+option.defname+"\"", location)
			}
		case model.TextSearchThesaurusDictionary:
			switch name {
			case "dictfile":
				metadata.ThesaurusFile = value
			case "dictionary":
				parts, parseErr := parseTextSearchQualifiedValue(value)
				if parseErr != nil {
					return nil, parseErr
				}
				descriptor, _, resolveErr := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchDictionary}, "42704", location)
				if resolveErr != nil {
					return nil, resolveErr
				}
				if _, ok := descriptor.TextSearchDictionary(); !ok {
					return nil, bindError("42809", "object is not a text search dictionary", location)
				}
				metadata.SubDictionary = descriptor.OID()
			default:
				return nil, bindError("22023", "unrecognized Thesaurus parameter: \""+option.defname+"\"", location)
			}
		default:
			return nil, bindError("0A000", "unsupported native text search dictionary template", 0)
		}
		entries = append(entries, entry)
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "DictFile", value: metadata.DictionaryFile},
		{name: "AffFile", value: metadata.AffixFile},
		{name: "Synonyms", value: metadata.SynonymsFile},
		{name: "Dictionary", value: metadata.ThesaurusFile},
	} {
		if field.value != "" && !model.ValidTextSearchDataName(field.value) {
			return nil, bindError("22023", "invalid text search configuration file name \""+field.value+"\"", 0)
		}
	}
	if metadata.Kind == model.TextSearchSimpleDictionary && metadata.Language != "" {
		return nil, bindError("22023", "simple dictionary does not accept a Language option", 0)
	}
	if metadata.Kind == model.TextSearchSnowballDictionary && !model.ValidTextSearchSnowballLanguage(metadata.Language) {
		return nil, bindError("22023", "Snowball dictionary requires a supported Language option", 0)
	}
	return entries, nil
}

// textSearchNodeOptions reduces a CREATE option list to serializable inputs;
// an option with no argument keeps an empty value, matching deserialize_deflist.
func textSearchNodeOptions(definition []*zpgparse.Node) ([]textSearchOptionInput, error) {
	options := make([]textSearchOptionInput, 0, len(definition))
	for _, node := range definition {
		option := node.GetDefElem()
		if option == nil {
			return nil, bindError("42601", "invalid text search dictionary option", 0)
		}
		input := textSearchOptionInput{defname: option.Defname, location: int(option.Location) + 1, numeric: isTextSearchNumericArg(option.Arg)}
		if option.Arg != nil {
			value, err := textSearchDefinitionValue(option.Arg)
			if err != nil {
				return nil, err
			}
			input.value = value
		}
		options = append(options, input)
	}
	return options, nil
}

// isTextSearchNumericArg reports whether serialization must emit the value
// bare, matching serialize_deflist's Integer/Float test.
func isTextSearchNumericArg(node *zpgparse.Node) bool {
	return node.GetInteger() != nil || node.GetFloat() != nil
}

// verifyNativeTextSearchDictionary runs each template's init function against
// the compiled-in data files, so a bad affix or dictionary file is reported at
// CREATE time exactly as CREATE TEXT SEARCH DICTIONARY reports it.
func verifyNativeTextSearchDictionary(metadata *model.TextSearchDictionaryMetadata) error {
	switch metadata.Kind {
	case model.TextSearchIspellDictionary:
		if metadata.DictionaryFile == "" || metadata.AffixFile == "" {
			return bindError("F0000", "ispell dictionary requires both DictFile and AffFile", 0)
		}
	case model.TextSearchSynonymDictionary:
		if metadata.SynonymsFile == "" {
			return bindError("F0000", "synonym dictionary requires a Synonyms option", 0)
		}
	case model.TextSearchThesaurusDictionary:
		if metadata.ThesaurusFile == "" {
			return bindError("F0000", "thesaurus dictionary requires a DictFile option", 0)
		}
	}
	return nil
}

// textSearchSnowballInitOptions renders the option list initdb's Snowball
// dictionaries carry, in the order template_init receives them.
func textSearchSnowballInitOptions(language, stopWords string) []textSearchInitOption {
	options := []textSearchInitOption{{name: "language", value: language}}
	if stopWords != "" {
		options = append(options, textSearchInitOption{name: "stopwords", value: stopWords})
	}
	return options
}

// textSearchDictionaryInitOptions renders the option list the way
// PostgreSQL's serialize_deflist does for pg_ts_dict.dictinitoption: an
// Integer or Float keeps its bare form, every other node type is single-quoted
// with backslashes escaped, and entries are joined by ", ".
func textSearchDictionaryInitOptions(entries []textSearchInitOption) string {
	var builder strings.Builder
	for index, entry := range entries {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(quoteTextSearchIdentifier(entry.name))
		builder.WriteString(" = ")
		if entry.numeric {
			builder.WriteString(entry.value)
			continue
		}
		if strings.ContainsRune(entry.value, '\\') {
			builder.WriteByte('E')
		}
		builder.WriteByte('\'')
		for _, character := range entry.value {
			if character == '\'' {
				builder.WriteRune(character)
			}
			builder.WriteRune(character)
		}
		builder.WriteByte('\'')
	}
	return builder.String()
}

// textSearchInitOption is one serialized dictionary option. numeric records
// that the CREATE option was an Integer or Float, which serialize_deflist
// emits without quotes so deserialize_deflist reconstructs the same node type.
type textSearchInitOption struct {
	name    string
	value   string
	numeric bool
}

func quoteTextSearchIdentifier(name string) string {
	if name == "" {
		return `""`
	}
	simple := true
	for index := range len(name) {
		character := name[index]
		if character >= 'a' && character <= 'z' || character == '_' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		simple = false
		break
	}
	if simple {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}



func bindTextSearchConfigurationDefinition(ctx context.Context, b *binder, snapshot CatalogSnapshot, definition []*zpgparse.Node, owner model.OID) (model.TextSearchConfigurationMetadata, []*model.Descriptor, error) {
	metadata := model.TextSearchConfigurationMetadata{Owner: owner}
	var dependencies []*model.Descriptor
	if len(definition) != 1 || definition[0].GetDefElem() == nil {
		return metadata, nil, bindError("42601", "text search configuration requires exactly one PARSER or COPY option", 0)
	}
	option := definition[0].GetDefElem()
	value, err := textSearchDefinitionValue(option.Arg)
	if err != nil {
		return metadata, nil, err
	}
	switch strings.ToLower(option.Defname) {
	case "parser":
		if !strings.EqualFold(value, "default") && !strings.EqualFold(value, "pg_catalog.default") {
			return metadata, nil, bindError("0A000", "only the native default text search parser is supported", int(option.Location)+1)
		}
		metadata.Parser = defaultTextSearchParserOID
	case "copy":
		if builtin, ok := builtinTextSearchConfigurationMetadata(value, owner); ok {
			metadata.Parser, metadata.Mappings = builtin.Parser, builtin.Mappings
			break
		}
		parts, parseErr := parseTextSearchQualifiedValue(value)
		if parseErr != nil {
			return metadata, nil, parseErr
		}
		descriptor, _, resolveErr := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchConfiguration}, "42704", int(option.Location)+1)
		if resolveErr != nil {
			return metadata, nil, resolveErr
		}
		copiedSpec, copyErr := descriptor.Spec(ctx)
		if copyErr != nil {
			return metadata, nil, copyErr
		}
		if copiedSpec.TextSearchConfiguration == nil {
			return metadata, nil, bindError("42809", "COPY source is not a text search configuration", int(option.Location)+1)
		}
		metadata.Parser = copiedSpec.TextSearchConfiguration.Parser
		metadata.Mappings = cloneTextSearchMappings(copiedSpec.TextSearchConfiguration.Mappings)
		allDependencies, dependencyErr := textSearchConfigurationDependencies(ctx, snapshot, descriptor.Name().Parent, metadata.Mappings, copiedSpec.Dependencies, nil)
		if dependencyErr != nil {
			return metadata, nil, dependencyErr
		}
		dependencies = allDependencies[1:]
	default:
		return metadata, nil, bindError("22023", "unrecognized text search configuration option", int(option.Location)+1)
	}
	return metadata, dependencies, nil
}

func builtinTextSearchConfigurationMetadata(name string, owner model.OID) (model.TextSearchConfigurationMetadata, bool) {
	value := strings.ToLower(strings.TrimSpace(name))
	if value != "simple" && value != "pg_catalog.simple" {
		return model.TextSearchConfigurationMetadata{}, false
	}
	metadata := model.TextSearchConfigurationMetadata{Owner: owner, Parser: defaultTextSearchParserOID}
	for token := uint16(1); token <= 23; token++ {
		if token == 12 || token == 13 || token == 14 || token == 23 {
			continue
		}
		metadata.Mappings = append(metadata.Mappings, model.TextSearchMappingReference{TokenType: token, Dictionaries: []model.OID{simpleTextSearchDictionaryOID}})
	}
	return metadata, true
}

func alterTextSearchMappings(ctx context.Context, b *binder, statement *zpgparse.AlterTSConfigurationStmt, metadata *model.TextSearchConfigurationMetadata, resolved map[model.OID]*model.Descriptor) error {
	tokens, err := textSearchTokenTypes(statement.Tokentype)
	if err != nil {
		return err
	}
	switch statement.Kind {
	case zpgparse.AlterTSConfigType_ALTER_TSCONFIG_ADD_MAPPING, zpgparse.AlterTSConfigType_ALTER_TSCONFIG_ALTER_MAPPING_FOR_TOKEN:
		dictionaries, resolveErr := resolveTextSearchDictionaries(ctx, b, statement.Dicts, resolved)
		if resolveErr != nil {
			return resolveErr
		}
		for _, token := range tokens {
			index := textSearchMappingIndex(metadata.Mappings, token)
			if index >= 0 && statement.Kind == zpgparse.AlterTSConfigType_ALTER_TSCONFIG_ADD_MAPPING {
				return bindError("42710", "text search mapping already exists", 0)
			}
			if index < 0 && statement.Kind == zpgparse.AlterTSConfigType_ALTER_TSCONFIG_ALTER_MAPPING_FOR_TOKEN {
				return bindError("42704", "text search mapping does not exist", 0)
			}
			mapping := model.TextSearchMappingReference{TokenType: token, Dictionaries: slices.Clone(dictionaries)}
			if index < 0 {
				metadata.Mappings = append(metadata.Mappings, mapping)
			} else {
				metadata.Mappings[index] = mapping
			}
		}
	case zpgparse.AlterTSConfigType_ALTER_TSCONFIG_REPLACE_DICT, zpgparse.AlterTSConfigType_ALTER_TSCONFIG_REPLACE_DICT_FOR_TOKEN:
		if len(statement.Dicts) != 2 {
			return bindError("42601", "ALTER MAPPING REPLACE requires two dictionaries", 0)
		}
		dictionaries, resolveErr := resolveTextSearchDictionaries(ctx, b, statement.Dicts, resolved)
		if resolveErr != nil {
			return resolveErr
		}
		selected := make(map[uint16]struct{}, len(tokens))
		for _, token := range tokens {
			selected[token] = struct{}{}
		}
		replaced := false
		for i := range metadata.Mappings {
			if statement.Kind == zpgparse.AlterTSConfigType_ALTER_TSCONFIG_REPLACE_DICT_FOR_TOKEN {
				if _, ok := selected[metadata.Mappings[i].TokenType]; !ok {
					continue
				}
			}
			for j, oid := range metadata.Mappings[i].Dictionaries {
				if oid == dictionaries[0] {
					metadata.Mappings[i].Dictionaries[j] = dictionaries[1]
					replaced = true
				}
			}
			metadata.Mappings[i].Dictionaries = compactTextSearchDictionaries(metadata.Mappings[i].Dictionaries)
		}
		if !replaced {
			return bindError("42704", "text search dictionary is not used by the selected mappings", 0)
		}
	case zpgparse.AlterTSConfigType_ALTER_TSCONFIG_DROP_MAPPING:
		for _, token := range tokens {
			index := textSearchMappingIndex(metadata.Mappings, token)
			if index < 0 {
				if statement.MissingOk {
					b.warnings = append(b.warnings, model.Error{Code: "42704", Message: "text search mapping does not exist, skipping"})
					continue
				}
				return bindError("42704", "text search mapping does not exist", 0)
			}
			metadata.Mappings = append(metadata.Mappings[:index], metadata.Mappings[index+1:]...)
		}
	default:
		return bindError("0A000", "unsupported ALTER TEXT SEARCH CONFIGURATION action", 0)
	}
	slices.SortFunc(metadata.Mappings, func(a, b model.TextSearchMappingReference) int { return int(a.TokenType) - int(b.TokenType) })
	return nil
}

func resolveTextSearchDictionaries(ctx context.Context, b *binder, nodes []*zpgparse.Node, resolved map[model.OID]*model.Descriptor) ([]model.OID, error) {
	if len(nodes) == 0 {
		return nil, bindError("42601", "text search mapping requires at least one dictionary", 0)
	}
	result := make([]model.OID, 0, len(nodes))
	for _, node := range nodes {
		parts, err := textSearchQualifiedNode(node)
		if err != nil {
			return nil, err
		}
		joined := strings.Join(parts, ".")
		switch strings.ToLower(joined) {
		case "simple", "pg_catalog.simple":
			result = append(result, simpleTextSearchDictionaryOID)
			continue
		}
		descriptor, _, resolveErr := b.resolve(parts, []model.ObjectKind{model.ObjectTextSearchDictionary}, "42704", 0)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if _, ok := descriptor.TextSearchDictionary(); !ok {
			return nil, bindError("42809", "object is not a text search dictionary", 0)
		}
		b.dependency(descriptor, 0)
		resolved[descriptor.OID()] = descriptor
		result = append(result, descriptor.OID())
	}
	if len(compactTextSearchDictionaries(result)) != len(result) {
		return nil, bindError("42710", "text search dictionary specified more than once", 0)
	}
	return result, ctx.Err()
}

func textSearchConfigurationDependencies(ctx context.Context, snapshot CatalogSnapshot, schema model.ObjectID, mappings []model.TextSearchMappingReference, old []model.Dependency, resolved map[model.OID]*model.Descriptor) ([]*model.Descriptor, error) {
	parent, err := snapshot.Descriptor(ctx, schema)
	if err != nil {
		return nil, err
	}
	dependencies := []*model.Descriptor{parent.Descriptor}
	oldByOID := make(map[model.OID]*model.Descriptor, len(old))
	for _, dependency := range old {
		published, lookupErr := snapshot.Descriptor(ctx, dependency.Object)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if published.Descriptor != nil {
			oldByOID[published.Descriptor.OID()] = published.Descriptor
		}
	}
	seen := make(map[model.OID]struct{})
	for _, mapping := range mappings {
		for _, oid := range mapping.Dictionaries {
			if oid == simpleTextSearchDictionaryOID {
				continue
			}
			if _, duplicate := seen[oid]; duplicate {
				continue
			}
			seen[oid] = struct{}{}
			descriptor := resolved[oid]
			if descriptor == nil {
				descriptor = oldByOID[oid]
			}
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil, bindError("42704", "text search mapping dictionary does not exist", 0)
			}
			dependencies = append(dependencies, descriptor)
		}
	}
	return dependencies, ctx.Err()
}

func textSearchTokenTypes(nodes []*zpgparse.Node) ([]uint16, error) {
	if len(nodes) == 0 {
		return nil, bindError("42601", "text search mapping requires a token type", 0)
	}
	result := make([]uint16, 0, len(nodes))
	seen := make(map[uint16]struct{}, len(nodes))
	for _, node := range nodes {
		value := node.GetString_()
		if value == nil {
			return nil, bindError("42601", "invalid text search token type", 0)
		}
		token := textSearchTokenType(value.Sval)
		if token == 0 {
			return nil, bindError("42704", "text search token type does not exist", 0)
		}
		if _, duplicate := seen[token]; duplicate {
			return nil, bindError("42710", "text search token type specified more than once", 0)
		}
		seen[token] = struct{}{}
		result = append(result, token)
	}
	return result, nil
}

func textSearchTokenType(alias string) uint16 {
	switch strings.ToLower(alias) {
	case "asciiword": return 1
	case "word": return 2
	case "numword": return 3
	case "email": return 4
	case "url": return 5
	case "host": return 6
	case "sfloat": return 7
	case "version": return 8
	case "hword_numpart": return 9
	case "hword_part": return 10
	case "hword_asciipart": return 11
	case "blank": return 12
	case "tag": return 13
	case "protocol": return 14
	case "numhword": return 15
	case "asciihword": return 16
	case "hword": return 17
	case "url_path": return 18
	case "file": return 19
	case "float": return 20
	case "int": return 21
	case "uint": return 22
	case "entity": return 23
	default: return 0
	}
}

func textSearchMappingIndex(mappings []model.TextSearchMappingReference, token uint16) int {
	return slices.IndexFunc(mappings, func(mapping model.TextSearchMappingReference) bool { return mapping.TokenType == token })
}

func compactTextSearchDictionaries(dictionaries []model.OID) []model.OID {
	seen := make(map[model.OID]struct{}, len(dictionaries))
	result := dictionaries[:0]
	for _, oid := range dictionaries {
		if _, duplicate := seen[oid]; duplicate {
			continue
		}
		seen[oid] = struct{}{}
		result = append(result, oid)
	}
	return result
}

func cloneTextSearchMappings(mappings []model.TextSearchMappingReference) []model.TextSearchMappingReference {
	result := make([]model.TextSearchMappingReference, len(mappings))
	copy(result, mappings)
	for i := range result {
		result[i].Dictionaries = slices.Clone(result[i].Dictionaries)
	}
	return result
}

func compareTextSearchDependency(a, b model.Dependency) int {
	if a.Object < b.Object { return -1 }
	if a.Object > b.Object { return 1 }
	if a.Column < b.Column { return -1 }
	if a.Column > b.Column { return 1 }
	if a.Version < b.Version { return -1 }
	if a.Version > b.Version { return 1 }
	return 0
}

func textSearchDescriptorDependencies(descriptors []*model.Descriptor) []model.Dependency {
	result := make([]model.Dependency, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor != nil {
			result = append(result, model.Dependency{Object: descriptor.ID(), Version: descriptor.Version()})
		}
	}
	slices.SortFunc(result, compareTextSearchDependency)
	return slices.Compact(result)
}

func textSearchDefinitionValue(node *zpgparse.Node) (string, error) {
	if node == nil {
		return "", bindError("42601", "text search option requires a value", 0)
	}
	if value := node.GetString_(); value != nil {
		return value.Sval, nil
	}
	if typ := node.GetTypeName(); typ != nil {
		parts, err := names(typ.Names)
		if err != nil || len(parts) == 0 {
			return "", bindError("42601", "invalid text search option value", int(typ.Location)+1)
		}
		return strings.Join(parts, "."), nil
	}
	if boolean := node.GetBoolean(); boolean != nil {
		return strconv.FormatBool(boolean.Boolval), nil
	}
	if integer := node.GetInteger(); integer != nil {
		return strconv.FormatInt(int64(integer.Ival), 10), nil
	}
	if number := node.GetFloat(); number != nil {
		return number.Fval, nil
	}
	return "", bindError("42601", "invalid text search option value", 0)
}

func textSearchBoolean(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "on", "t", "true", "y", "yes":
		return true, true
	case "0", "off", "f", "false", "n", "no":
		return false, true
	default:
		return false, false
	}
}

func textSearchQualifiedNode(node *zpgparse.Node) ([]string, error) {
	if node == nil {
		return nil, bindError("42601", "missing text search object name", 0)
	}
	if list := node.GetList(); list != nil {
		parts, err := names(list.Items)
		if err == nil && len(parts) > 0 && len(parts) <= 2 {
			return parts, nil
		}
	}
	if value := node.GetString_(); value != nil {
		return []string{value.Sval}, nil
	}
	return nil, bindError("42601", "invalid text search object name", 0)
}

func parseTextSearchQualifiedValue(value string) ([]string, error) {
	parts := strings.Split(value, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid text search object name", 0)
	}
	for _, part := range parts {
		if err := declarationName(part); err != nil {
			return nil, err
		}
	}
	return parts, nil
}
