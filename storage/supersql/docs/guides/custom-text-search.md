# Full-Text Search & Custom `tsvector` Analyzers

SuperSQL ships with PostgreSQL-compatible full-text search (`tsvector`, `tsquery`, `@@`, `ts_rank`, `ts_rank_cd`, `ts_headline`, `GIN` `tsvector_ops` indexes), built-in pure-Go Snowball language configurations (`simple`, `english`, `french`, `german`, `spanish`, etc.), and a **Go API for registering custom `tsvector` analyzers** (such as Korean/Japanese morphological analyzers, n-gram tokenizers, or domain-specific normalizers).

---

## 1. Built-In Language Configurations

All 29 embedded Snowball stemmers and PostgreSQL stopword lists are available out of the box with `CGO_ENABLED=0`:

- `'simple'` (default `default_text_search_config`)
- `'english'`, `'french'`, `'german'`, `'spanish'`, `'italian'`, `'portuguese'`, `'russian'`, `'dutch'`, `'danish'`, `'finnish'`, `'hungarian'`, `'norwegian'`, `'romanian'`, `'swedish'`, `'turkish'`
- `'arabic'`, `'armenian'`, `'basque'`, `'catalan'`, `'estonian'`, `'greek'`, `'hindi'`, `'indonesian'`, `'irish'`, `'lithuanian'`, `'nepali'`, `'serbian'`, `'tamil'`, `'yiddish'`

```sql
SELECT to_tsvector('english', 'The fat rats ran quickly')
    @@ to_tsquery('english', 'rat & quick'); -- true
```

---

## 2. Registering a Custom Go `TextSearchAnalyzer`

For languages that require morphological analysis or custom tokenization (for example Korean with Kiwi/Kagome, Japanese with MeCab/Kagome, CJK bigrams, or product SKU/code tokenizers), register a Go function under any configuration name.

### Public API Overview

```go
// Full control: custom lexemes, optional 1-based positions, and weights ('A'..'D').
type TextSearchToken struct {
	Lexeme   string // normalized token text (empty tokens are skipped)
	Position uint16 // 1-based position; 0 auto-increments sequentially
	Weight   byte   // optional 'A', 'B', 'C', or 'D' (0 defaults to 'D')
}

type TextSearchAnalyzer func(ctx context.Context, input string) ([]TextSearchToken, error)

// Convenience adapter when you only need sequential normalized strings:
func SimpleTextSearchAnalyzer(fn func(ctx context.Context, input string) ([]string, error)) TextSearchAnalyzer
```

### Option A: Register at `Open` Time (`OpenOptions.TextSearchAnalyzers`)

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory: dir,
	Clock:     localclock.New(),
	TextSearchAnalyzers: map[string]supersql.TextSearchAnalyzer{
		"ko_bigram": supersql.SimpleTextSearchAnalyzer(func(ctx context.Context, input string) ([]string, error) {
			return extractKoreanTokens(input), nil
		}),
	},
})
```

### Option B: Register or Unregister Dynamically on `*Catalog`

```go
err := db.RegisterTextSearchAnalyzer("ko_morph", func(ctx context.Context, input string) ([]supersql.TextSearchToken, error) {
	// Example: plug in your morphological analyzer (Kiwi, Kagome, custom stemmer, etc.)
	var tokens []supersql.TextSearchToken
	for i, w := range strings.Fields(strings.ToLower(input)) {
		tokens = append(tokens, supersql.TextSearchToken{
			Lexeme:   w,
			Position: uint16(i + 1),
			Weight:   'D',
		})
	}
	return tokens, nil
})

// Later, if needed:
db.UnregisterTextSearchAnalyzer("ko_morph")
```

- Registration and unregistration are concurrency-safe (`sync.RWMutex`).
- Configuration names follow PostgreSQL identifier rules (case-normalized to lowercase, non-empty, `<= 63` bytes, no NUL bytes).

---

## 3. Using Custom Analyzers in SQL & GIN Indexes

Once registered, a custom analyzer works everywhere a PostgreSQL `regconfig` is accepted:

- `to_tsvector('ko_morph', doc)`
- `to_tsquery('ko_morph', '데이터베이스 & 엔진')` (preserves `&`, `|`, `!`, `<->`, `()`, `:*` operators while running each leaf term through your analyzer)
- `plainto_tsquery('ko_morph', '임베디드 데이터베이스')`
- `phraseto_tsquery('ko_morph', '임베디드 데이터베이스')`
- `websearch_to_tsquery('ko_morph', '"임베디드 데이터베이스" -외부')`
- `jsonb_to_tsvector('ko_morph', payload, '["string"]')`
- `SET default_text_search_config = 'ko_morph'` followed by 1-argument `to_tsvector(doc)` / `to_tsquery(q)`
- **`CREATE INDEX ... USING gin (tsv)`** accelerated by `GIN Index Scan`

```sql
CREATE TABLE posts (
    id BIGINT PRIMARY KEY,
    body TEXT NOT NULL,
    tsv TSVECTOR NOT NULL
);

CREATE INDEX idx_posts_tsv ON posts USING gin (tsv);

INSERT INTO posts (id, body, tsv)
VALUES (1, '순수 Go 임베디드 PostgreSQL 데이터베이스 엔진',
        to_tsvector('ko_morph', '순수 Go 임베디드 PostgreSQL 데이터베이스 엔진'));

-- Uses GIN Index Scan on idx_posts_tsv
SELECT id, ts_rank(tsv, q) AS score
FROM posts, to_tsquery('ko_morph', '데이터베이스 & 엔진') AS q
WHERE tsv @@ q
ORDER BY score DESC;
```

---

## 4. Weights, Positions & Error Handling

- **Weights**: Setting `TextSearchToken.Weight` to `'A'`, `'B'`, `'C'`, or `'D'` embeds PostgreSQL lexeme weights directly in the generated `tsvector`, immediately usable by `ts_rank`, `ts_rank_cd`, and `ts_filter`.
- **Positions**: Leaving `Position == 0` auto-increments sequentially (`1, 2, 3, ...`). Setting explicit positions lets synonym or morphological analyzers emit multiple lexemes at the same word position so phrase queries (`<->`) match accurately.
- **Stopwords / Empty Tokens**: Returning an empty `Lexeme` (or an empty slice for a stopword) skips that token—matching PostgreSQL dictionary stopword behavior in both `to_tsvector` and `to_tsquery`.
- **Cancellation & Errors**: Always respect `ctx.Err()`; any error returned by your analyzer aborts the SQL statement cleanly and surfaces to the caller.
