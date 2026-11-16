package builtin

import (
	"context"
	"strings"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/model"
	tsArabic "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/arabic"
	tsArmenian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/armenian"
	tsBasque "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/basque"
	tsCatalan "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/catalan"
	tsDanish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/danish"
	tsDutch "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/dutch"
	tsEnglish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/english"
	tsEstonian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/estonian"
	tsFinnish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/finnish"
	tsFrench "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/french"
	tsGerman "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/german"
	tsGreek "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/greek"
	tsHindi "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/hindi"
	tsHungarian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/hungarian"
	tsIndonesian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/indonesian"
	tsIrish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/irish"
	tsItalian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/italian"
	tsLithuanian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/lithuanian"
	tsNepali "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/nepali"
	tsNorwegian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/norwegian"
	tsPortuguese "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/portuguese"
	tsRomanian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/romanian"
	tsRussian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/russian"
	tsRuntime "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/runtime"
	tsSerbian "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/serbian"
	tsSpanish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/spanish"
	tsSwedish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/swedish"
	tsTamil "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/tamil"
	tsTurkish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/turkish"
	tsYiddish "github.com/gosuda/supersql-alpha/internal/query/builtin/snowball/yiddish"
)

const (
	tsDefaultParserOID model.OID = 3722
	tsSimpleConfigOID  model.OID = 3748
	tsSimpleDictOID    model.OID = 3765
)


func simpleTextSearchDictionary() model.TextSearchDictionary {
	return model.TextSearchDictionary{OID: tsSimpleDictOID, Name: "pg_catalog.simple", Kind: model.TextSearchSimpleDictionary, Accept: true}
}


func builtinTextSearchConfiguration(oid model.OID) (model.TextSearchConfiguration, bool) {
	if oid != tsSimpleConfigOID {
		return model.TextSearchConfiguration{}, false
	}
	dictionary := simpleTextSearchDictionary()
	mappings := make([]model.TextSearchMapping, 0, 19)
	for token := uint16(1); token <= 23; token++ {
		if token == 12 || token == 13 || token == 14 || token == 23 { continue }
		mappings = append(mappings, model.TextSearchMapping{TokenType: token, Dictionaries: []model.TextSearchDictionary{dictionary}})
	}
	return model.TextSearchConfiguration{OID: oid, Name: "pg_catalog.simple", Parser: tsDefaultParserOID, Mappings: mappings}, true
}

func builtinTextSearchConfigurationName(oid model.OID) (string, bool) {
	configuration, ok := builtinTextSearchConfiguration(oid)
	if !ok { return "", false }
	return configuration.Name, true
}

func builtinTextSearchDictionary(oid model.OID) (model.TextSearchDictionary, bool) {
	if oid == tsSimpleDictOID { return simpleTextSearchDictionary(), true }
	return model.TextSearchDictionary{}, false
}

func builtinTextSearchDictionaryName(oid model.OID) (string, bool) {
	dictionary, ok := builtinTextSearchDictionary(oid)
	if !ok { return "", false }
	return dictionary.Name, true
}

func builtinTextSearchConfigurationByName(name string) (model.TextSearchConfiguration, bool) {
	if value := strings.ToLower(strings.TrimSpace(name)); value == "simple" || value == "pg_catalog.simple" {
		return builtinTextSearchConfiguration(tsSimpleConfigOID)
	}
	return model.TextSearchConfiguration{}, false
}

func builtinTextSearchDictionaryByName(name string) (model.TextSearchDictionary, bool) {
	if value := strings.ToLower(strings.TrimSpace(name)); value == "simple" || value == "pg_catalog.simple" {
		return simpleTextSearchDictionary(), true
	}
	return model.TextSearchDictionary{}, false
}

func lexizeTextSearch(ctx context.Context, dictionary model.TextSearchDictionary, token string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The builtin simple and Snowball kinds are described entirely by the
	// descriptor, so they resolve here; every other kind carries a compiled
	// kernel that already applies its own stop list.
	switch dictionary.Kind {
	case model.TextSearchSimpleDictionary, model.TextSearchSnowballDictionary:
	default:
		variants, err := lexizeTextSearchVariants(ctx, dictionary.Kernel, token)
		if err != nil || variants == nil {
			return nil, err
		}
		return flattenTextSearchVariants(variants), nil
	}
	word := strings.ToLower(token)
	for _, stop := range dictionary.StopWords {
		if word == stop {
			return []string{}, nil
		}
	}
	if dictionary.StopWordsLanguage != "" {
		if dictionary.Stemmer == nil {
			return nil, sqlError("55000", "native text search stop-word runtime is unavailable")
		}
		stop, err := dictionary.Stemmer.IsTextSearchStopWord(ctx, dictionary.StopWordsLanguage, word)
		if err != nil {
			return nil, err
		}
		if stop {
			return []string{}, nil
		}
	}
	switch dictionary.Kind {
	case model.TextSearchSimpleDictionary:
		if !dictionary.Accept {
			return nil, nil
		}
		return []string{word}, nil
	case model.TextSearchSnowballDictionary:
		if dictionary.Stemmer == nil {
			return nil, sqlError("55000", "native Snowball stemmer runtime is unavailable")
		}
		if len(word) > 1000 {
			return []string{word}, nil
		}
		stem, err := dictionary.Stemmer.StemTextSearch(ctx, dictionary.Language, word)
		if err != nil {
			return nil, err
		}
		return []string{stem}, nil
	default:
		// Two switch arms plus this one cover every TextSearchDictionaryKind; a
		// new kind must not silently resolve to a fabricated lexeme.
		return nil, sqlError("0A000", "unsupported native text search dictionary")
	}
}

// lexizeTextSearchVariants resolves token into contiguous variant groups. A nil
// result means the word is unrecognized and an empty result means it is a stop
// word, matching the TSLexeme protocol every PostgreSQL dictionary speaks.
func lexizeTextSearchVariants(ctx context.Context, kernel model.TextSearchDictionaryKernel, token string) ([][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch dictionary := kernel.(type) {
	case nil:
		return nil, sqlError("55000", "native text search dictionary kernel is unavailable")
	case model.TextSearchVariantKernel:
		return dictionary.LexizeVariants(ctx, token)
	default:
		lexemes, err := dictionary.Lexize(ctx, token)
		if err != nil || lexemes == nil {
			return nil, err
		}
		variants := make([][]string, 0, len(lexemes))
		for _, lexeme := range lexemes {
			variants = append(variants, []string{lexeme})
		}
		return variants, nil
	}
}

func flattenTextSearchVariants(variants [][]string) []string {
	total := 0
	for _, variant := range variants {
		total += len(variant)
	}
	lexemes := make([]string, 0, total)
	for _, variant := range variants {
		lexemes = append(lexemes, variant...)
	}
	return lexemes
}

// NativeTextSearchStemmer runs pure-Go code generated from PostgreSQL
// REL_18_4's Snowball revision d19326ac6c1b9a417fc872f7c2f845265a5e9ece.
type NativeTextSearchStemmer struct{}

type nativeTextSearchStem func(*tsRuntime.Env) bool

var nativeTextSearchEnvironmentPool = sync.Pool{New: func() any { return tsRuntime.NewEnv("") }}

func (NativeTextSearchStemmer) StemTextSearch(ctx context.Context, language, word string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	stem, ok := nativeTextSearchStemmer(language)
	if !ok {
		return "", sqlError("22023", "unknown native Snowball language")
	}
	word = strings.ToLower(word)
	if len(word) > 1000 {
		return word, nil
	}
	environment := nativeTextSearchEnvironmentPool.Get().(*tsRuntime.Env)
	environment.SetCurrent(word)
	stem(environment)
	result := environment.Current()
	environment.SetCurrent("")
	nativeTextSearchEnvironmentPool.Put(environment)
	return result, nil
}

func (NativeTextSearchStemmer) IsTextSearchStopWord(ctx context.Context, language, word string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	found, ok := nativeTextSearchStopWord(language, strings.ToLower(word))
	if !ok {
		return false, sqlError("22023", "unknown native text search stop-word list")
	}
	return found, nil
}

func nativeTextSearchStemmer(language string) (nativeTextSearchStem, bool) {
	switch language {
	case "arabic":
		return tsArabic.Stem, true
	case "armenian":
		return tsArmenian.Stem, true
	case "basque":
		return tsBasque.Stem, true
	case "catalan":
		return tsCatalan.Stem, true
	case "danish":
		return tsDanish.Stem, true
	case "dutch":
		return tsDutch.Stem, true
	case "english":
		return tsEnglish.Stem, true
	case "estonian":
		return tsEstonian.Stem, true
	case "finnish":
		return tsFinnish.Stem, true
	case "french":
		return tsFrench.Stem, true
	case "german":
		return tsGerman.Stem, true
	case "greek":
		return tsGreek.Stem, true
	case "hindi":
		return tsHindi.Stem, true
	case "hungarian":
		return tsHungarian.Stem, true
	case "indonesian":
		return tsIndonesian.Stem, true
	case "irish":
		return tsIrish.Stem, true
	case "italian":
		return tsItalian.Stem, true
	case "lithuanian":
		return tsLithuanian.Stem, true
	case "nepali":
		return tsNepali.Stem, true
	case "norwegian":
		return tsNorwegian.Stem, true
	case "portuguese":
		return tsPortuguese.Stem, true
	case "romanian":
		return tsRomanian.Stem, true
	case "russian":
		return tsRussian.Stem, true
	case "serbian":
		return tsSerbian.Stem, true
	case "spanish":
		return tsSpanish.Stem, true
	case "swedish":
		return tsSwedish.Stem, true
	case "tamil":
		return tsTamil.Stem, true
	case "turkish":
		return tsTurkish.Stem, true
	case "yiddish":
		return tsYiddish.Stem, true
	default:
		return nil, false
	}
}



