package supersql

import (
	"context"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

const customTextSearchOIDBase model.OID = 0x70000000

// TextSearchToken is one normalized lexeme emitted by a user-registered
// TextSearchAnalyzer.
type TextSearchToken struct {
	// Lexeme is the normalized token text. Empty lexemes are skipped.
	Lexeme string
	// Position is the 1-based token position in the document (clamped to
	// PostgreSQL's 1..16383 range). A zero value auto-increments sequentially
	// from the previous token's position.
	Position uint16
	// Weight is the optional PostgreSQL lexeme weight ('A', 'B', 'C', or 'D';
	// lowercase is accepted). Zero defaults to 'D'.
	Weight byte
}

// TextSearchAnalyzer tokenizes and normalizes input text into lexemes for a
// custom text search configuration (for example, a Korean/Japanese
// morphological analyzer, n-gram tokenizer, or domain-specific stemmer).
type TextSearchAnalyzer func(ctx context.Context, input string) ([]TextSearchToken, error)

// SimpleTextSearchAnalyzer adapts a tokenizer returning normalized lexeme
// strings into a TextSearchAnalyzer with sequential 1-based positions and
// default weight 'D'.
func SimpleTextSearchAnalyzer(fn func(ctx context.Context, input string) ([]string, error)) TextSearchAnalyzer {
	if fn == nil {
		return nil
	}
	return func(ctx context.Context, input string) ([]TextSearchToken, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lexemes, err := fn(ctx, input)
		if err != nil {
			return nil, err
		}
		if len(lexemes) == 0 {
			return nil, nil
		}
		tokens := make([]TextSearchToken, 0, len(lexemes))
		for _, lexeme := range lexemes {
			if lexeme == "" {
				continue
			}
			tokens = append(tokens, TextSearchToken{Lexeme: lexeme})
		}
		return tokens, nil
	}
}

// RegisterTextSearchAnalyzer registers or replaces a custom Go text search
// configuration under name on this database. Configuration names follow
// PostgreSQL identifier rules (ASCII case-folded to lowercase, non-empty,
// at most 63 bytes, valid UTF-8, without NUL bytes or dots).
//
// Registration is safe for concurrent use alongside active SQL queries.
func (database *Catalog) RegisterTextSearchAnalyzer(name string, analyzer TextSearchAnalyzer) error {
	if database == nil {
		return ErrDatabaseClosed
	}
	if err := database.checkOpen(); err != nil {
		return err
	}
	if err := database.textSearchRegistry.register(name, analyzer); err != nil {
		return wrapError(err)
	}
	return nil
}

// UnregisterTextSearchAnalyzer removes a previously registered custom text
// search configuration by name. Unregistering an unknown name is a no-op.
//
// Unregistration is safe for concurrent use alongside active SQL queries.
func (database *Catalog) UnregisterTextSearchAnalyzer(name string) {
	if database == nil || database.textSearchRegistry == nil {
		return
	}
	database.textSearchRegistry.unregister(name)
}

type textSearchAnalyzerRegistry struct {
	mu        sync.RWMutex
	nextOID   model.OID
	nameToOID map[string]model.OID
	byName    map[string]model.TextSearchConfiguration
	byOID     map[model.OID]model.TextSearchConfiguration
}

var _ session.CustomTextSearchLookup = (*textSearchAnalyzerRegistry)(nil)

func newTextSearchAnalyzerRegistry() *textSearchAnalyzerRegistry {
	return &textSearchAnalyzerRegistry{
		nextOID:   customTextSearchOIDBase,
		nameToOID: make(map[string]model.OID),
		byName:    make(map[string]model.TextSearchConfiguration),
		byOID:     make(map[model.OID]model.TextSearchConfiguration),
	}
}

func newTextSearchAnalyzerRegistryFromOptions(analyzers map[string]TextSearchAnalyzer) (*textSearchAnalyzerRegistry, error) {
	registry := newTextSearchAnalyzerRegistry()
	if len(analyzers) == 0 {
		return registry, nil
	}
	type namedAnalyzer struct {
		normalized string
		analyzer   TextSearchAnalyzer
	}
	seen := make(map[string]struct{}, len(analyzers))
	entries := make([]namedAnalyzer, 0, len(analyzers))
	for rawName, analyzer := range analyzers {
		normalized, err := normalizeTextSearchAnalyzerName(rawName)
		if err != nil {
			return nil, err
		}
		if analyzer == nil {
			return nil, &model.Error{Code: "22023", Message: "text search analyzer must not be nil"}
		}
		if _, dup := seen[normalized]; dup {
			return nil, &model.Error{Code: "22023", Message: "duplicate text search analyzer name", Detail: normalized}
		}
		seen[normalized] = struct{}{}
		entries = append(entries, namedAnalyzer{normalized: normalized, analyzer: analyzer})
	}
	slices.SortFunc(entries, func(a, b namedAnalyzer) int {
		return strings.Compare(a.normalized, b.normalized)
	})
	for _, entry := range entries {
		if err := registry.registerNormalized(entry.normalized, entry.analyzer); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *textSearchAnalyzerRegistry) register(name string, analyzer TextSearchAnalyzer) error {
	normalized, err := normalizeTextSearchAnalyzerName(name)
	if err != nil {
		return err
	}
	if analyzer == nil {
		return &model.Error{Code: "22023", Message: "text search analyzer must not be nil"}
	}
	return r.registerNormalized(normalized, analyzer)
}

func (r *textSearchAnalyzerRegistry) registerNormalized(normalized string, analyzer TextSearchAnalyzer) error {
	adapted := adaptTextSearchAnalyzer(analyzer)
	r.mu.Lock()
	defer r.mu.Unlock()
	oid, exists := r.nameToOID[normalized]
	if !exists {
		r.nextOID++
		oid = r.nextOID
		r.nameToOID[normalized] = oid
	}
	cfg := model.TextSearchConfiguration{
		OID:      oid,
		Name:     normalized,
		Analyzer: adapted,
	}
	r.byName[normalized] = cfg
	r.byOID[oid] = cfg
	return nil
}

func (r *textSearchAnalyzerRegistry) unregister(name string) {
	normalized, err := normalizeTextSearchAnalyzerName(name)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg, ok := r.byName[normalized]; ok {
		delete(r.byName, normalized)
		delete(r.byOID, cfg.OID)
	}
}

func (r *textSearchAnalyzerRegistry) LookupByName(name string) (model.TextSearchConfiguration, bool) {
	if r == nil {
		return model.TextSearchConfiguration{}, false
	}
	r.mu.RLock()
	cfg, ok := r.byName[name]
	r.mu.RUnlock()
	return cfg, ok
}

func (r *textSearchAnalyzerRegistry) LookupByOID(oid model.OID) (model.TextSearchConfiguration, bool) {
	if r == nil {
		return model.TextSearchConfiguration{}, false
	}
	r.mu.RLock()
	cfg, ok := r.byOID[oid]
	r.mu.RUnlock()
	return cfg, ok
}

func normalizeTextSearchAnalyzerName(name string) (string, error) {
	if name == "" || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
		return "", &model.Error{Code: "22023", Message: "invalid text search analyzer name"}
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || len(trimmed) > 63 || strings.IndexByte(trimmed, '.') >= 0 {
		return "", &model.Error{Code: "22023", Message: "invalid text search analyzer name"}
	}
	var out strings.Builder
	out.Grow(len(trimmed))
	for i := range len(trimmed) {
		c := trimmed[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out.WriteByte(c)
	}
	return out.String(), nil
}

func encodeTextSearchWeight(weight byte) (uint8, error) {
	switch weight {
	case 0, 'D', 'd':
		return 0, nil
	case 'C', 'c':
		return 1, nil
	case 'B', 'b':
		return 2, nil
	case 'A', 'a':
		return 3, nil
	default:
		return 0, &model.Error{Code: "22023", Message: "unrecognized weight"}
	}
}

func adaptTextSearchAnalyzer(analyzer TextSearchAnalyzer) model.TextSearchCustomAnalyzer {
	return func(ctx context.Context, input string) ([]model.TextSearchCustomToken, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tokens, err := analyzer(ctx, input)
		if err != nil {
			return nil, err
		}
		if len(tokens) == 0 {
			return nil, nil
		}
		out := make([]model.TextSearchCustomToken, 0, len(tokens))
		for _, tok := range tokens {
			if tok.Lexeme == "" {
				continue
			}
			weight, weightErr := encodeTextSearchWeight(tok.Weight)
			if weightErr != nil {
				return nil, weightErr
			}
			out = append(out, model.TextSearchCustomToken{
				Lexeme:   tok.Lexeme,
				Position: tok.Position,
				Weight:   weight,
			})
		}
		return out, nil
	}
}

// customOnlyTextSearchRuntime serves custom Go text search analyzers on
// execution paths without a full session capability frame (such as historical
// snapshots or parameter reconciliation before capability attachment).
type customOnlyTextSearchRuntime struct {
	registry *textSearchAnalyzerRegistry
}

var _ model.TextSearchRuntime = customOnlyTextSearchRuntime{}

func (database *Catalog) customTextSearchRuntime() model.TextSearchRuntime {
	if database == nil || database.textSearchRegistry == nil {
		return nil
	}
	return customOnlyTextSearchRuntime{registry: database.textSearchRegistry}
}

func (r customOnlyTextSearchRuntime) ResolveTextSearchConfiguration(ctx context.Context, name string) (model.TextSearchConfiguration, error) {
	parts, err := builtin.ParseQualifiedName(ctx, name, 1<<20)
	if err != nil {
		return model.TextSearchConfiguration{}, err
	}
	if len(parts) == 1 {
		if cfg, ok := r.registry.LookupByName(parts[0]); ok {
			return cfg, nil
		}
	}
	return model.TextSearchConfiguration{}, &model.Error{Code: "42704", Message: "text search object does not exist"}
}

func (r customOnlyTextSearchRuntime) TextSearchConfiguration(_ context.Context, oid model.OID) (model.TextSearchConfiguration, error) {
	if cfg, ok := r.registry.LookupByOID(oid); ok {
		return cfg, nil
	}
	return model.TextSearchConfiguration{}, &model.Error{Code: "42704", Message: "text search configuration does not exist"}
}

func (r customOnlyTextSearchRuntime) ResolveTextSearchDictionary(_ context.Context, _ string) (model.TextSearchDictionary, error) {
	return model.TextSearchDictionary{}, &model.Error{Code: "42704", Message: "text search object does not exist"}
}

func (r customOnlyTextSearchRuntime) TextSearchDictionary(_ context.Context, oid model.OID) (model.TextSearchDictionary, error) {
	if dict, ok := builtin.BuiltinTextSearchDictionary(oid); ok {
		return dict, nil
	}
	return model.TextSearchDictionary{}, &model.Error{Code: "42704", Message: "text search dictionary does not exist"}
}

func (r customOnlyTextSearchRuntime) TextSearchConfigurationName(ctx context.Context, oid model.OID, maxBytes uint64) (string, error) {
	if cfg, ok := r.registry.LookupByOID(oid); ok {
		return builtin.QuoteQualifiedName(ctx, []string{cfg.Name}, maxBytes)
	}
	return "", &model.Error{Code: "42704", Message: "text search object does not exist"}
}

func (r customOnlyTextSearchRuntime) TextSearchDictionaryName(_ context.Context, _ model.OID, _ uint64) (string, error) {
	return "", &model.Error{Code: "42704", Message: "text search object does not exist"}
}

func (r customOnlyTextSearchRuntime) TextSearchTemplateName(_ context.Context, _ model.OID, _ uint64) (string, error) {
	return "", &model.Error{Code: "42704", Message: "text search object does not exist"}
}

func (r customOnlyTextSearchRuntime) RewriteTextSearchQuery(_ context.Context, _ []byte, _ string, _ uint64) ([]byte, error) {
	return nil, &model.Error{Code: "55000", Message: "text search rewrite query runtime is unavailable"}
}

func (r customOnlyTextSearchRuntime) TextSearchStatistics(_ context.Context, _, _ string, _ uint64) ([]model.TextSearchStatistic, error) {
	return nil, &model.Error{Code: "55000", Message: "text search statistics runtime is unavailable"}
}
