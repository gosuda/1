package builtin

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ICUCollationSpec describes an executable ICU or Unicode collation.
type ICUCollationSpec struct {
	ID            model.CollationID
	Name          string
	Locale        string
	Deterministic bool
}

// CollationResolver resolves user-created MVCC catalog collations by OID.
type CollationResolver interface {
	ResolveCollation(id model.CollationID) (ICUCollationSpec, bool)
	CollationFingerprint() [sha256.Size]byte
}

func effectiveCollationResolver(options ResolveOptions) CollationResolver {
	if options.Collations != nil {
		return options.Collations
	}
	if cr, ok := options.Types.(CollationResolver); ok {
		return cr
	}
	return nil
}

// Well-known predefined ICU collation OIDs (below FirstNormalObjectId 16384 so
// they never collide with user-created catalog descriptors or pg_collation.dat).
const (
	CollationUndICU       model.CollationID = 16300
	CollationEnUSICU      model.CollationID = 16301
	CollationKoKRICU      model.CollationID = 16302
	CollationJaJPICU      model.CollationID = 16303
	CollationDeDEICU      model.CollationID = 16304
	CollationFrFRICU      model.CollationID = 16305
	CollationEsESICU      model.CollationID = 16306
	CollationZhCNICU      model.CollationID = 16307
	CollationUndCI        model.CollationID = 16308
	CollationEnUSCI       model.CollationID = 16309
	CollationKoKRCI       model.CollationID = 16310
	CollationUndNumeric   model.CollationID = 16311
	CollationEnUSNumeric  model.CollationID = 16312
)

var predefinedICUCollations = []ICUCollationSpec{
	{ID: 963, Name: "unicode", Locale: "und", Deterministic: true},
	{ID: 6411, Name: "pg_unicode_fast", Locale: "und", Deterministic: true},
	{ID: CollationUndICU, Name: "und-x-icu", Locale: "und", Deterministic: true},
	{ID: CollationEnUSICU, Name: "en-US-x-icu", Locale: "en-US", Deterministic: true},
	{ID: CollationKoKRICU, Name: "ko-KR-x-icu", Locale: "ko-KR", Deterministic: true},
	{ID: CollationJaJPICU, Name: "ja-JP-x-icu", Locale: "ja-JP", Deterministic: true},
	{ID: CollationDeDEICU, Name: "de-DE-x-icu", Locale: "de-DE", Deterministic: true},
	{ID: CollationFrFRICU, Name: "fr-FR-x-icu", Locale: "fr-FR", Deterministic: true},
	{ID: CollationEsESICU, Name: "es-ES-x-icu", Locale: "es-ES", Deterministic: true},
	{ID: CollationZhCNICU, Name: "zh-CN-x-icu", Locale: "zh-CN", Deterministic: true},
	{ID: CollationUndCI, Name: "und-u-ks-level2", Locale: "und-u-ks-level2", Deterministic: false},
	{ID: CollationEnUSCI, Name: "en-US-u-ks-level2", Locale: "en-US-u-ks-level2", Deterministic: false},
	{ID: CollationKoKRCI, Name: "ko-KR-u-ks-level2", Locale: "ko-KR-u-ks-level2", Deterministic: false},
	{ID: CollationUndNumeric, Name: "und-u-kn-true", Locale: "und-u-kn-true", Deterministic: true},
	{ID: CollationEnUSNumeric, Name: "en-US-u-kn-true", Locale: "en-US-u-kn-true", Deterministic: true},
}

// PredefinedICUCollations returns all built-in ICU collations (excluding the
// pinned pg_collation.dat entries, which live in collationCatalog).
func PredefinedICUCollations() []ICUCollationSpec {
	out := make([]ICUCollationSpec, 0, len(predefinedICUCollations))
	for _, c := range predefinedICUCollations {
		if c.ID >= CollationUndICU {
			out = append(out, c)
		}
	}
	return out
}

// LookupICUCollation resolves a built-in Unicode/ICU collation by OID.
func LookupICUCollation(id model.CollationID, resolver CollationResolver) (ICUCollationSpec, bool) {
	for _, spec := range predefinedICUCollations {
		if spec.ID == id {
			return spec, true
		}
	}
	if resolver != nil {
		return resolver.ResolveCollation(id)
	}
	return ICUCollationSpec{}, false
}

// LookupICUCollationName resolves a predefined ICU collation by SQL name.
func LookupICUCollationName(name string) (ICUCollationSpec, bool) {
	for _, spec := range predefinedICUCollations {
		if spec.Name == name {
			return spec, true
		}
	}
	return ICUCollationSpec{}, false
}

// IsBinaryCollation reports whether a resolved collation uses raw UTF-8 byte order.
func IsBinaryCollation(id model.CollationID) bool {
	return id == 950 || id == 951 || id == 811 || id == 962
}

// IsSupportedCollation reports whether a resolved collation can be executed.
func IsSupportedCollation(id model.CollationID, resolver CollationResolver) bool {
	if IsBinaryCollation(id) {
		return true
	}
	_, ok := LookupICUCollation(id, resolver)
	return ok
}

// ValidateICULocale validates an ICU/BCP-47 locale string and returns its parsed
// language tag and collate options.
func ValidateICULocale(locale string) (language.Tag, []collate.Option, error) {
	trimmed := strings.TrimSpace(locale)
	if trimmed == "" {
		return language.Und, nil, sqlError("22023", "ICU locale cannot be empty")
	}
	// Strip PostgreSQL "-x-icu" suffix if supplied in LOCALE.
	normalized := strings.TrimSuffix(trimmed, "-x-icu")
	// Normalize legacy underscore form (e.g. "ko_KR" -> "ko-KR") while keeping BCP-47 extensions.
	normalized = strings.ReplaceAll(normalized, "_", "-")

	var opts []collate.Option
	base := normalized
	if idx := strings.Index(strings.ToLower(normalized), "-u-"); idx >= 0 {
		base = normalized[:idx]
		ext := strings.ToLower(normalized[idx+3:])
		parts := strings.Split(ext, "-")
		for i := 0; i < len(parts); i++ {
			switch parts[i] {
			case "ks":
				if i+1 < len(parts) {
					i++
					switch parts[i] {
					case "level1":
						opts = append(opts, collate.Loose)
					case "level2":
						opts = append(opts, collate.IgnoreCase)
					case "level3", "level4", "identic":
						// Default strength in golang.org/x/text/collate
					default:
						return language.Und, nil, sqlError("22023", "unsupported ICU collation strength: "+parts[i])
					}
				}
			case "kn":
				if i+1 < len(parts) {
					i++
					if parts[i] == "true" {
						opts = append(opts, collate.Numeric)
					}
				} else {
					opts = append(opts, collate.Numeric)
				}
			case "kc":
				if i+1 < len(parts) {
					i++
					if parts[i] == "false" {
						opts = append(opts, collate.IgnoreCase)
					}
				}
			case "ka":
				if i+1 < len(parts) {
					i++
					// Alternate handling (shifted/non-ignorable) accepted; default behavior used.
				}
			}
		}
	}
	if base == "" {
		base = "und"
	}
	tag, err := language.Parse(base)
	if err != nil {
		return language.Und, nil, sqlError("22023", "invalid ICU locale "+locale+": "+err.Error())
	}
	return tag, opts, nil
}

type pooledCollator struct {
	collator *collate.Collator
	buf      collate.Buffer
}

type compiledICUCollation struct {
	spec ICUCollationSpec
	pool sync.Pool
}

var icuCollationCache sync.Map // map[string]*compiledICUCollation

func getCompiledICUCollation(spec ICUCollationSpec) (*compiledICUCollation, error) {
	cacheKey := spec.Locale
	if spec.Deterministic {
		cacheKey += "|det"
	} else {
		cacheKey += "|nondet"
	}
	if cached, ok := icuCollationCache.Load(cacheKey); ok {
		return cached.(*compiledICUCollation), nil
	}
	tag, opts, err := ValidateICULocale(spec.Locale)
	if err != nil {
		return nil, err
	}
	compiled := &compiledICUCollation{
		spec: spec,
		pool: sync.Pool{
			New: func() any {
				return &pooledCollator{
					collator: collate.New(tag, opts...),
				}
			},
		},
	}
	actual, _ := icuCollationCache.LoadOrStore(cacheKey, compiled)
	return actual.(*compiledICUCollation), nil
}

// CompareCollationText compares two validated UTF-8 strings under collationID.
// Binary collations (C/POSIX/pg_c_utf8/ucs_basic) take the fast byte-order path.
func CompareCollationText(ctx context.Context, collationID model.CollationID, resolver CollationResolver, a, b string) (int, error) {
	if IsBinaryCollation(collationID) {
		return compareText(ctx, a, b)
	}
	spec, ok := LookupICUCollation(collationID, resolver)
	if !ok {
		return 0, sqlError("42P22", "unsupported collation")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	compiled, err := getCompiledICUCollation(spec)
	if err != nil {
		return 0, err
	}
	entry := compiled.pool.Get().(*pooledCollator)
	order := entry.collator.CompareString(a, b)
	compiled.pool.Put(entry)
	if order == 0 && spec.Deterministic {
		return strings.Compare(a, b), nil
	}
	return order, nil
}

// CollationSortKey computes the UCA sort key for s under an ICU collation.
// The returned slice is freshly owned by the caller.
func CollationSortKey(collationID model.CollationID, resolver CollationResolver, s string) ([]byte, ICUCollationSpec, error) {
	spec, ok := LookupICUCollation(collationID, resolver)
	if !ok {
		return nil, ICUCollationSpec{}, sqlError("42P22", "unsupported collation")
	}
	compiled, err := getCompiledICUCollation(spec)
	if err != nil {
		return nil, ICUCollationSpec{}, err
	}
	entry := compiled.pool.Get().(*pooledCollator)
	entry.buf.Reset()
	raw := entry.collator.KeyFromString(&entry.buf, s)
	owned := make([]byte, len(raw))
	copy(owned, raw)
	compiled.pool.Put(entry)
	return owned, spec, nil
}
