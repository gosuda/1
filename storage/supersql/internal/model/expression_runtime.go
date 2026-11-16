package model

import "context"

// SequenceRuntime is borrowed by one evaluation, never by a shared program.
// The invocation owner pins catalog identities and checks write mode.
type SequenceRuntime interface {
	NextSequence(context.Context, OID) (int64, error)
	CurrentSequence(context.Context, OID) (int64, error)
	LastSequence(context.Context) (int64, error)
	SetSequence(context.Context, OID, int64, bool) (int64, error)
}

// RelationRuntime borrows inputs and returns owned output under maxBytes.
// Catalog readers and namespace dependencies belong to the invocation owner.
type RelationRuntime interface {
	ResolveRelation(context.Context, string, uint64) (OID, error)
	RelationName(context.Context, OID, uint64) (string, error)
}

// SQLTimestamp counts PostgreSQL microseconds since 2000-01-01, not SI nanoseconds.
type SQLTimestamp int64

// TemporalRuntime borrows a clock and immutable invocation timestamps.
type TemporalRuntime interface {
	TransactionTimestamp(context.Context) (SQLTimestamp, error)
	StatementTimestamp(context.Context) (SQLTimestamp, error)
	ClockTimestamp(context.Context) (SQLTimestamp, error)
}
// SettingsRuntime is borrowed by one evaluation. CurrentSetting returns an
// owned text value; SetConfig checks the canonical result against maxResultBytes
// before publishing any settings mutation. FormatLocale returns immutable data
// borrowed only for the synchronous scalar invocation.
type SettingsRuntime interface {
	CurrentSetting(context.Context, string, bool) (Value, error)
	SetConfig(context.Context, string, *string, bool, uint64) (Value, error)
	FormatLocale(context.Context) (FormatLocale, error)
}
// FormatLocale is immutable locale data for one formatting invocation.
// Names identify the settings used to resolve it; text fields are borrowed.
type FormatLocale struct {
	NumericName           string
	MonetaryName          string
	TimeName              string
	DecimalPoint          string
	ThousandsSeparator    string
	PositiveSign          string
	NegativeSign          string
	CurrencySymbol        string
	ISOCurrencySymbol     string
	MonthNames            [12]string
	AbbreviatedMonthNames [12]string
	DayNames              [7]string
	AbbreviatedDayNames   [7]string
}


type TextSearchDictionaryKind uint8

const (
	TextSearchSimpleDictionary TextSearchDictionaryKind = iota + 1
	TextSearchSnowballDictionary
	TextSearchIspellDictionary
	TextSearchSynonymDictionary
	TextSearchThesaurusDictionary
)

// TextSearchStemmer applies the database's pinned native stemming resources.
type TextSearchStemmer interface {
	StemTextSearch(context.Context, string, string) (string, error)
	IsTextSearchStopWord(context.Context, string, string) (bool, error)
}

// TextSearchDictionaryKernel is the compiled lexical kernel of one
// dictionary. Implementations are immutable after construction and borrowed
// only for the synchronous calls that use them.
type TextSearchDictionaryKernel interface {
	// Lexize resolves one word. A nil result means the word is unrecognized;
	// an empty non-nil result means a stop word.
	Lexize(context.Context, string) ([]string, error)
}

// TextSearchVariantKernel is implemented by dictionaries that report how their
// lexemes group into split variants, which PostgreSQL records in
// TSLexeme.nvariant. Thesaurus matching needs that grouping to disambiguate
// multi-word rules; a dictionary that does not implement it is treated as one
// variant per lexeme, which is exact for every single-lexeme dictionary.
type TextSearchVariantKernel interface {
	// LexizeVariants resolves one word into contiguous variant groups. A nil
	// result means the word is unrecognized; an empty non-nil result means a
	// stop word.
	LexizeVariants(context.Context, string) ([][]string, error)
}

// TextSearchPhraseKernel is implemented by dictionaries that substitute
// multi-token phrases (PostgreSQL thesaurus). Match begins a phrase match at
// word; a nil result means word cannot begin any entry. The driver offers every
// following token of a mapped type and stops as soon as WantMore reports false.
type TextSearchPhraseKernel interface {
	Match(context.Context, string) TextSearchPhrase
}

// TextSearchPhrase is one in-progress phrase match. Extend consumes the next
// word and reports whether the dictionary needs another word before it can
// decide; Result returns the substitution completed on the most recent word.
type TextSearchPhrase interface {
	Extend(context.Context, string) bool
	Result() []string
}

// ValidTextSearchSnowballLanguage reports whether the native provider includes language.
func ValidTextSearchSnowballLanguage(language string) bool {
	switch language {
	case "arabic", "armenian", "basque", "catalan", "danish", "dutch", "english", "estonian", "finnish", "french", "german", "greek", "hindi", "hungarian", "indonesian", "irish", "italian", "lithuanian", "nepali", "norwegian", "portuguese", "romanian", "russian", "serbian", "spanish", "swedish", "tamil", "turkish", "yiddish":
		return true
	default:
		return false
	}
}

// ValidTextSearchDataName reports whether name is an acceptable
// PostgreSQL tsearch_data basename. An empty name is accepted because it
// means the associated option was omitted; PostgreSQL applies the same
// restricted alphabet in get_tsearch_config_filename so a basename can never
// escape the tsearch_data directory.
func ValidTextSearchDataName(name string) bool {
	if name == "" {
		return true
	}
	for i := range name {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
		default:
			return false
		}
	}
	return true
}

// HasNativeTextSearchStopWords reports whether PostgreSQL ships a stop-word list for language.
func HasNativeTextSearchStopWords(language string) bool {
	switch language {
	case "danish", "dutch", "english", "finnish", "french", "german", "hungarian", "italian", "nepali", "norwegian", "portuguese", "russian", "spanish", "swedish", "turkish":
		return true
	default:
		return false
	}
}

// TextSearchDictionary is immutable metadata borrowed from an invocation's
// pinned catalog snapshot. Kernel is the compiled lexical implementation of a
// non-Snowball dictionary; it is nil for the builtin kinds whose behavior is
// fully described by the remaining fields.
type TextSearchDictionary struct {
	OID               OID
	Name              string
	Kind              TextSearchDictionaryKind
	Language          string
	StopWords         []string
	StopWordsLanguage string
	Accept            bool
	Stemmer           TextSearchStemmer
	Kernel            TextSearchDictionaryKernel
}

type TextSearchMapping struct {
	TokenType    uint16
	Dictionaries []TextSearchDictionary
}

// TextSearchConfiguration is immutable metadata borrowed from an invocation's
// pinned catalog snapshot. Parser identifies a native parser implementation.
type TextSearchConfiguration struct {
	OID      OID
	Name     string
	Parser   OID
	Mappings []TextSearchMapping
}

type TextSearchStatistic struct {
	Word      string
	Documents int64
	Entries   int64
}

// TextSearchRuntime resolves native configurations, dictionaries, and templates against
// the invocation's immutable catalog snapshot. Returned metadata is borrowed
// only for the synchronous call.
type TextSearchRuntime interface {
	ResolveTextSearchConfiguration(context.Context, string) (TextSearchConfiguration, error)
	TextSearchConfiguration(context.Context, OID) (TextSearchConfiguration, error)
	ResolveTextSearchDictionary(context.Context, string) (TextSearchDictionary, error)
	TextSearchDictionary(context.Context, OID) (TextSearchDictionary, error)
	TextSearchConfigurationName(context.Context, OID, uint64) (string, error)
	TextSearchDictionaryName(context.Context, OID, uint64) (string, error)
	TextSearchTemplateName(context.Context, OID, uint64) (string, error)
	RewriteTextSearchQuery(context.Context, []byte, string, uint64) ([]byte, error)
	TextSearchStatistics(context.Context, string, string, uint64) ([]TextSearchStatistic, error)
}


type TriggerEvent uint8

const (
	TriggerInsert TriggerEvent = iota + 1
	TriggerUpdate
	TriggerDelete
	TriggerTruncate
)

// TriggerRuntime owns trigger recursion and transition rows for one statement;
// transaction-scoped deferred events are transferred to its SQL executor.
// Returned row images are owned by the caller.
type TriggerRuntime interface {
	BeginStatement(context.Context, *Descriptor, TriggerEvent, []ColumnID) error
	BeforeRow(context.Context, *Descriptor, TriggerEvent, *Row, *Row, uint64) (*Row, bool, error)
	AfterRow(context.Context, *Descriptor, TriggerEvent, *Row, *Row, uint64) error
	FinishStatement(context.Context, *Descriptor, TriggerEvent) error
	AbortStatement(*Descriptor, TriggerEvent)
}
// DatabaseIdentity describes the current database from a pinned catalog source.
type DatabaseIdentity struct {
	Name     string
	Encoding int32
}

// DatabaseRuntime returns database identity owned by the caller.
type DatabaseRuntime interface {
	DatabaseIdentity(context.Context) (DatabaseIdentity, error)
}

// RandomRuntime owns the session-local PostgreSQL pseudo-random sequence.
type RandomRuntime interface {
	RandomUint64(context.Context) (uint64, error)
	SeedRandom(context.Context, float64) error
}

// TypeInputRuntime validates text through a snapshot-pinned type input codec.
// A returned Error is limited to class 22 input/data failures and domain
// not-null/check violations (23502/23514). Resolution failures, cancellation,
// resource exhaustion, and unsafe callback failures return as err.
// Implementations invoke input typmods, not explicit truncating SQL coercions.
type TypeInputRuntime interface {
	ValidateTypeInput(context.Context, string, string, int64, uint64) (*Error, error)
}

// NativeScalarRuntime groups narrow services used by scalar builtins.
type NativeScalarRuntime struct {
	Database  DatabaseRuntime
	Random    RandomRuntime
	TypeInput TypeInputRuntime
	Triggers  TriggerRuntime
}
// ExpressionRuntime supplies borrowed per-evaluation effects. Zero provides none.
// Programs must not retain these capabilities after evaluation returns.
type ExpressionRuntime struct {
	Sequences  SequenceRuntime
	Relations  RelationRuntime
	Temporal   TemporalRuntime
	Settings   SettingsRuntime
	TextSearch TextSearchRuntime
	Advisory   AdvisoryLockRuntime
	Native     NativeScalarRuntime
}
