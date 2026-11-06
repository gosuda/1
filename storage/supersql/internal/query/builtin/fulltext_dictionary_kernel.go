package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// CompileTextSearchDictionaryKernel builds the executable lexizer for a
// dictionary descriptor. The returned kernel is immutable, so a caller may
// build it once per resolution and reuse it for every token of an evaluation.
//
// resolveSubDictionary supplies the normalizing dictionary a thesaurus names;
// it is only consulted for that kind, so other kinds may pass nil.
func CompileTextSearchDictionaryKernel(ctx context.Context, metadata model.TextSearchDictionaryMetadata, resolveSubDictionary func(model.OID) (model.TextSearchDictionary, error)) (model.TextSearchDictionaryKernel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch metadata.Kind {
	case model.TextSearchSynonymDictionary:
		return newNativeSynonymDictionary(ctx, metadata.SynonymsFile, metadata.CaseSensitive)
	case model.TextSearchIspellDictionary:
		return newNativeIspellDictionary(ctx, metadata.DictionaryFile, metadata.AffixFile)
	case model.TextSearchThesaurusDictionary:
		if resolveSubDictionary == nil {
			return nil, sqlError("55000", "thesaurus sub-dictionary resolver is unavailable")
		}
		subDictionary, err := resolveSubDictionary(metadata.SubDictionary)
		if err != nil {
			return nil, err
		}
		if subDictionary.Kernel == nil {
			return nil, sqlError("55000", "thesaurus sub-dictionary is not executable")
		}
		return newNativeThesaurusDictionary(ctx, metadata.ThesaurusFile, subDictionary.Kernel)
	default:
		// Simple and Snowball are described entirely by their descriptor and
		// are resolved by lexizeTextSearch, so they have no separate kernel.
		return nil, nil
	}
}
