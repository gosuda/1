package session

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type TextSearchCatalog interface {
	bind.CatalogSnapshot
	ResolveOID(context.Context, model.OID) (catalog.PublishedDescriptor, error)
}

type TextSearchRewrite func(context.Context, []byte, string, uint64) ([]byte, error)
type TextSearchStatistics func(context.Context, string, string, uint64) ([]model.TextSearchStatistic, error)

type textSearchRuntime struct {
	catalog    TextSearchCatalog
	path       []string
	rewrite    TextSearchRewrite
	statistics TextSearchStatistics
	stemmer    model.TextSearchStemmer
}

// NewTextSearchRuntime borrows the pinned invocation catalog and owns the
// semantic search path used by regconfig and regdictionary.
func NewTextSearchRuntime(snapshot TextSearchCatalog, path []string, pathBytes uint64, rewrite TextSearchRewrite, statistics TextSearchStatistics) (model.TextSearchRuntime, error) {
	if historicalNil(snapshot) || pathBytes == 0 {
		return nil, errPreparedInvocation
	}
	header := uint64(unsafe.Sizeof(""))
	if uint64(len(path)) > pathBytes/header {
		return nil, model.ErrValueBudget
	}
	remaining := pathBytes - uint64(len(path))*header
	owned := make([]string, len(path))
	for i, name := range path {
		if !utf8.ValidString(name) || uint64(len(name)) > remaining {
			return nil, model.ErrValueBudget
		}
		remaining -= uint64(len(name))
		owned[i] = strings.Clone(name)
	}
	return &textSearchRuntime{catalog: snapshot, path: owned, rewrite: rewrite, statistics: statistics, stemmer: builtin.NativeTextSearchStemmer{}}, nil
}

func (r *textSearchRuntime) resolve(ctx context.Context, text string, kind model.ObjectKind) (*model.Descriptor, error) {
	parts, err := builtin.ParseQualifiedName(ctx, text, 1<<20)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 || len(parts) > 2 {
		return nil, &model.Error{Code: "42602", Message: "invalid name syntax"}
	}
	resolve := func(schema, name string) (*model.Descriptor, error) {
		namespace, err := r.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
		if err != nil {
			return nil, err
		}
		published, err := r.catalog.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: name})
		if err != nil {
			return nil, err
		}
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return nil, catalog.ErrUnknownObject
		}
		return published.Descriptor, nil
	}
	if len(parts) == 2 {
		descriptor, err := resolve(parts[0], parts[1])
		if err != nil {
			return nil, textSearchLookupError(err)
		}
		return descriptor, nil
	}
	for _, schema := range r.path {
		descriptor, err := resolve(schema, parts[0])
		if err == nil {
			return descriptor, nil
		}
		if !errors.Is(err, catalog.ErrUnknownObject) {
			return nil, err
		}
	}
	return nil, &model.Error{Code: "42704", Message: "text search object does not exist"}
}
func textSearchLookupError(err error) error {
	if errors.Is(err, catalog.ErrUnknownObject) {
		return &model.Error{Code: "42704", Message: "text search object does not exist"}
	}
	return err
}

func staticRuntimeDictionary(oid model.OID) (model.TextSearchDictionary, bool) {
	if oid == 3765 {
		return model.TextSearchDictionary{OID: 3765, Name: "pg_catalog.simple", Kind: model.TextSearchSimpleDictionary, Accept: true}, true
	}
	return model.TextSearchDictionary{}, false
}

func (r *textSearchRuntime) dictionaryDescriptor(ctx context.Context, descriptor *model.Descriptor) (model.TextSearchDictionary, error) {
	metadata, ok := descriptor.TextSearchDictionary()
	if !ok {
		return model.TextSearchDictionary{}, &model.Error{Code: "42809", Message: "object is not a text search dictionary"}
	}
	return r.buildRuntimeDictionary(ctx, descriptor, metadata)
}

// buildRuntimeDictionary compiles the dictionary's lexizer once per
// resolution. Compilation reads only pinned tsearch_data files, so the cost is
// bounded and the result is shared by every token the dictionary lexizes.
func (r *textSearchRuntime) buildRuntimeDictionary(ctx context.Context, descriptor *model.Descriptor, metadata model.TextSearchDictionaryMetadata) (model.TextSearchDictionary, error) {
	dictionary := model.TextSearchDictionary{
		OID: descriptor.OID(), Name: descriptor.Name().Name, Kind: metadata.Kind,
		Language: metadata.Language, StopWords: metadata.StopWords,
		StopWordsLanguage: metadata.StopWordsLanguage, Accept: metadata.Accept, Stemmer: r.stemmer,
	}
	if metadata.Kind == model.TextSearchSimpleDictionary || metadata.Kind == model.TextSearchSnowballDictionary {
		return dictionary, nil
	}
	kernel, err := r.compileKernel(ctx, metadata)
	if err != nil {
		return model.TextSearchDictionary{}, err
	}
	dictionary.Kernel = kernel
	return dictionary, nil
}

// compileKernel builds the lexizer for a file-backed dictionary. Thesaurus
// needs its named sub-dictionary, which is resolved through the same catalog
// this runtime already uses; the dependency edge makes that dictionary
// reachable, so a missing one is a corrupt catalog rather than a user error.
func (r *textSearchRuntime) compileKernel(ctx context.Context, metadata model.TextSearchDictionaryMetadata) (model.TextSearchDictionaryKernel, error) {
	return builtin.CompileTextSearchDictionaryKernel(ctx, metadata, func(subOID model.OID) (model.TextSearchDictionary, error) {
		published, err := r.catalog.ResolveOID(ctx, subOID)
		if err != nil {
			return model.TextSearchDictionary{}, textSearchLookupError(err)
		}
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return model.TextSearchDictionary{}, &model.Error{Code: "42704", Message: "text search dictionary does not exist"}
		}
		subMetadata, ok := published.Descriptor.TextSearchDictionary()
		if !ok {
			return model.TextSearchDictionary{}, &model.Error{Code: "42809", Message: "object is not a text search dictionary"}
		}
		return r.buildRuntimeDictionary(ctx, published.Descriptor, subMetadata)
	})
}
func (r *textSearchRuntime) ResolveTextSearchDictionary(ctx context.Context, name string) (model.TextSearchDictionary, error) {
	descriptor, err := r.resolve(ctx, name, model.ObjectTextSearchDictionary)
	if err != nil {
		return model.TextSearchDictionary{}, err
	}
	return r.dictionaryDescriptor(ctx, descriptor)
}
func (r *textSearchRuntime) TextSearchDictionary(ctx context.Context, oid model.OID) (model.TextSearchDictionary, error) {
	if dictionary, ok := staticRuntimeDictionary(oid); ok {
		return dictionary, nil
	}
	published, err := r.catalog.ResolveOID(ctx, oid)
	if err != nil {
		return model.TextSearchDictionary{}, textSearchLookupError(err)
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return model.TextSearchDictionary{}, &model.Error{Code: "42704", Message: "text search dictionary does not exist"}
	}
	return r.dictionaryDescriptor(ctx, published.Descriptor)
}

func (r *textSearchRuntime) configurationDescriptor(ctx context.Context, descriptor *model.Descriptor) (model.TextSearchConfiguration, error) {
	metadata, ok := descriptor.TextSearchConfiguration()
	if !ok {
		return model.TextSearchConfiguration{}, &model.Error{Code: "42809", Message: "object is not a text search configuration"}
	}
	configuration := model.TextSearchConfiguration{OID: descriptor.OID(), Name: descriptor.Name().Name, Parser: metadata.Parser, Mappings: make([]model.TextSearchMapping, len(metadata.Mappings))}
	for i, mapping := range metadata.Mappings {
		configuration.Mappings[i].TokenType = mapping.TokenType
		configuration.Mappings[i].Dictionaries = make([]model.TextSearchDictionary, len(mapping.Dictionaries))
		for j, oid := range mapping.Dictionaries {
			dictionary, err := r.TextSearchDictionary(ctx, oid)
			if err != nil {
				return model.TextSearchConfiguration{}, err
			}
			configuration.Mappings[i].Dictionaries[j] = dictionary
		}
	}
	return configuration, nil
}
func (r *textSearchRuntime) ResolveTextSearchConfiguration(ctx context.Context, name string) (model.TextSearchConfiguration, error) {
	descriptor, err := r.resolve(ctx, name, model.ObjectTextSearchConfiguration)
	if err != nil {
		return model.TextSearchConfiguration{}, err
	}
	return r.configurationDescriptor(ctx, descriptor)
}
func (r *textSearchRuntime) TextSearchConfiguration(ctx context.Context, oid model.OID) (model.TextSearchConfiguration, error) {
	published, err := r.catalog.ResolveOID(ctx, oid)
	if err != nil {
		return model.TextSearchConfiguration{}, textSearchLookupError(err)
	}
	if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
		return model.TextSearchConfiguration{}, &model.Error{Code: "42704", Message: "text search configuration does not exist"}
	}
	return r.configurationDescriptor(ctx, published.Descriptor)
}

func (r *textSearchRuntime) objectName(ctx context.Context, oid model.OID, kind model.ObjectKind, maxBytes uint64) (string, error) {
	published, err := r.catalog.ResolveOID(ctx, oid)
	if err != nil {
		return "", textSearchLookupError(err)
	}
	descriptor := published.Descriptor
	if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Kind() != kind {
		return "", &model.Error{Code: "42704", Message: "text search object does not exist"}
	}
	name := descriptor.Name()
	for _, schema := range r.path {
		namespace, resolveErr := r.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schema})
		if resolveErr != nil {
			if errors.Is(resolveErr, catalog.ErrUnknownObject) {
				continue
			}
			return "", resolveErr
		}
		visible, resolveErr := r.catalog.Resolve(ctx, model.NamespaceName{Parent: namespace.Descriptor.ID(), Kind: kind, Name: name.Name})
		if resolveErr != nil {
			if errors.Is(resolveErr, catalog.ErrUnknownObject) {
				continue
			}
			return "", resolveErr
		}
		if visible.Descriptor != nil && visible.Descriptor.ID() == descriptor.ID() {
			return builtin.QuoteQualifiedName(ctx, []string{name.Name}, maxBytes)
		}
	}
	namespace, err := r.catalog.Descriptor(ctx, name.Parent)
	if err != nil {
		return "", err
	}
	return builtin.QuoteQualifiedName(ctx, []string{namespace.Descriptor.Name().Name, name.Name}, maxBytes)
}
func (r *textSearchRuntime) TextSearchConfigurationName(ctx context.Context, oid model.OID, maxBytes uint64) (string, error) {
	return r.objectName(ctx, oid, model.ObjectTextSearchConfiguration, maxBytes)
}
func (r *textSearchRuntime) TextSearchDictionaryName(ctx context.Context, oid model.OID, maxBytes uint64) (string, error) {
	return r.objectName(ctx, oid, model.ObjectTextSearchDictionary, maxBytes)
}
func (r *textSearchRuntime) TextSearchTemplateName(ctx context.Context, oid model.OID, maxBytes uint64) (string, error) {
	return r.objectName(ctx, oid, model.ObjectTextSearchTemplate, maxBytes)
}
func (r *textSearchRuntime) RewriteTextSearchQuery(ctx context.Context, query []byte, command string, maxBytes uint64) ([]byte, error) {
	if r.rewrite == nil {
		return nil, &model.Error{Code: "55000", Message: "text search rewrite query runtime is unavailable"}
	}
	return r.rewrite(ctx, query, command, maxBytes)
}
func (r *textSearchRuntime) TextSearchStatistics(ctx context.Context, query, weights string, maxBytes uint64) ([]model.TextSearchStatistic, error) {
	if r.statistics == nil {
		return nil, &model.Error{Code: "55000", Message: "text search statistics runtime is unavailable"}
	}
	return r.statistics(ctx, query, weights, maxBytes)
}
