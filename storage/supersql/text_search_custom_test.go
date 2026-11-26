package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
)

func execTSText(t *testing.T, db *Catalog, query string) string {
	t.Helper()
	out, err := db.exec(t.Context(), query, 0)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(out.Rows) != 1 || len(out.Rows[0]) != 1 || out.Rows[0][0].IsNull {
		t.Fatalf("%s: unexpected rows %+v", query, out.Rows)
	}
	return out.Rows[0][0].StrVal
}

func execTSBool(t *testing.T, db *Catalog, query string) bool {
	t.Helper()
	out, err := db.exec(t.Context(), query, 0)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(out.Rows) != 1 || len(out.Rows[0]) != 1 || out.Rows[0][0].IsNull {
		t.Fatalf("%s: unexpected rows %+v", query, out.Rows)
	}
	return out.Rows[0][0].IntVal != 0
}

func execTSFloat(t *testing.T, db *Catalog, query string) float64 {
	t.Helper()
	out, err := db.exec(t.Context(), query, 0)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if len(out.Rows) != 1 || len(out.Rows[0]) != 1 || out.Rows[0][0].IsNull {
		t.Fatalf("%s: unexpected rows %+v", query, out.Rows)
	}
	return out.Rows[0][0].FloatVal
}

func TestTextSearchBuiltinSnowballConfigurations(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Verify PostgreSQL-compatible English stemming and stopword filtering.
	got := execTSText(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly')::text`)
	want := `'fat':2 'quick':5 'ran':4 'rat':3`
	if got != want {
		t.Fatalf("to_tsvector('english', ...) = %q, want %q", got, want)
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ to_tsquery('english', 'rat & quick')`) {
		t.Fatal("expected english tsvector to match 'rat & quick'")
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ to_tsquery('english', 'rats & quickly')`) {
		t.Fatal("expected english tsvector to match stemmed 'rats & quickly'")
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ plainto_tsquery('english', 'The running rats') IS FALSE`) {
		t.Fatal("expected 'running' not to match 'ran'")
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ plainto_tsquery('english', 'The rats quickly')`) {
		t.Fatal("expected plainto_tsquery('english', ...) to drop stopword 'the' and stem")
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ phraseto_tsquery('english', 'fat rats ran quickly')`) {
		t.Fatal("expected phraseto_tsquery('english', ...) to match exact phrase")
	}
	if !execTSBool(t, db, `SELECT to_tsvector('english', 'The fat rats ran quickly') @@ websearch_to_tsquery('english', '"fat rats" -dog')`) {
		t.Fatal("expected websearch_to_tsquery('english', ...) to match")
	}

	// Verify French and German built-in Snowball stemmers and stopwords.
	frVec := execTSText(t, db, `SELECT to_tsvector('french', 'Les chats mangent rapidement')::text`)
	if strings.Contains(frVec, `'les'`) || !strings.Contains(frVec, `'chat'`) {
		t.Fatalf("unexpected french tsvector: %q", frVec)
	}
	if !execTSBool(t, db, `SELECT to_tsvector('french', 'Les chats mangent rapidement') @@ to_tsquery('french', 'chat & mangent')`) {
		t.Fatal("expected french query to match french vector")
	}

	deVec := execTSText(t, db, `SELECT to_tsvector('german', 'Die Katzen laufen schnell')::text`)
	if strings.Contains(deVec, `'die'`) || !strings.Contains(deVec, `'katz'`) {
		t.Fatalf("unexpected german tsvector: %q", deVec)
	}
	if !execTSBool(t, db, `SELECT to_tsvector('german', 'Die Katzen laufen schnell') @@ to_tsquery('german', 'Katze & laufen')`) {
		t.Fatal("expected german query to match german vector")
	}

	// Verify Russian ASCII words use english_stem just like PostgreSQL.
	ruVec := execTSText(t, db, `SELECT to_tsvector('russian', 'running quickly')::text`)
	if !strings.Contains(ruVec, `'run'`) || !strings.Contains(ruVec, `'quick'`) {
		t.Fatalf("expected russian ASCII tokens to stem with english_stem, got %q", ruVec)
	}

	// Verify regconfig round-trip and ts_lexize with built-in Snowball dictionary.
	if cfgName := execTSText(t, db, `SELECT 'english'::regconfig::text`); cfgName != "english" {
		t.Fatalf("'english'::regconfig::text = %q, want %q", cfgName, "english")
	}
	if lex := execTSText(t, db, `SELECT array_to_string(ts_lexize('english_stem', 'running'), ',')`); lex != "run" {
		t.Fatalf("ts_lexize('english_stem', 'running') = %q, want %q", lex, "run")
	}
	if !execTSBool(t, db, `SELECT ts_lexize('english_stem', 'the') = ARRAY[]::text[]`) {
		t.Fatal("expected 'the' to be recognized as an English stopword")
	}

	// Verify SET default_text_search_config = 'english' + 1-arg to_tsvector / to_tsquery.
	mustExecGIN(t, db, `SET default_text_search_config = 'english'`)
	if show := execTSText(t, db, `SHOW default_text_search_config`); show != "pg_catalog.english" {
		t.Fatalf("SHOW default_text_search_config = %q, want %q", show, "pg_catalog.english")
	}
	if cur := execTSText(t, db, `SELECT get_current_ts_config()::text`); cur != "english" {
		t.Fatalf("get_current_ts_config() = %q, want %q", cur, "english")
	}
	if v := execTSText(t, db, `SELECT to_tsvector('The fat rats ran quickly')::text`); v != want {
		t.Fatalf("1-arg to_tsvector with english default = %q, want %q", v, want)
	}
	if !execTSBool(t, db, `SELECT to_tsvector('The fat rats ran quickly') @@ to_tsquery('rats & quickly')`) {
		t.Fatal("expected 1-arg to_tsvector/to_tsquery to use english default")
	}

	// Verify GIN tsvector_ops index works with built-in 'english' configuration.
	mustExecGIN(t, db,
		`CREATE TABLE en_docs (id bigint PRIMARY KEY, body text NOT NULL, tsv tsvector NOT NULL)`,
		`CREATE INDEX idx_en_docs_tsv ON en_docs USING gin (tsv)`,
		`INSERT INTO en_docs VALUES
			(1, 'The fat rats ran quickly', to_tsvector('english', 'The fat rats ran quickly')),
			(2, 'Embedded SQL database engines run fast', to_tsvector('english', 'Embedded SQL database engines run fast')),
			(3, 'Quiet cats slept soundly', to_tsvector('english', 'Quiet cats slept soundly'))`,
	)

	query := `SELECT id FROM en_docs WHERE tsv @@ to_tsquery('english', 'rat & quickly') ORDER BY id`
	plan := explainPlanText(t, db, query)
	if !strings.Contains(plan, "GIN Index Scan using idx_en_docs_tsv on en_docs") {
		t.Fatalf("expected GIN Index Scan for english query, got:\n%s", plan)
	}
	if ids := execGINInt64IDs(t, db, query); !slices.Equal(ids, []int64{1}) {
		t.Fatalf("english GIN query = %v, want [1]", ids)
	}

	// Verify all 29 Snowball languages resolve cleanly.
	allLangs := []string{
		"arabic", "armenian", "basque", "catalan", "danish", "dutch", "english",
		"estonian", "finnish", "french", "german", "greek", "hindi", "hungarian",
		"indonesian", "irish", "italian", "lithuanian", "nepali", "norwegian",
		"portuguese", "romanian", "russian", "serbian", "spanish", "swedish",
		"tamil", "turkish", "yiddish",
	}
	for _, lang := range allLangs {
		q := fmt.Sprintf(`SELECT to_tsvector('%s', 'hello world')::text`, lang)
		if got := execTSText(t, db, q); got == "" {
			t.Fatalf("empty tsvector for built-in Snowball language %q", lang)
		}
	}
}

// koreanMorphemeAnalyzer simulates a Korean morphological/compound analyzer:
// strips common particles/stopwords, splits compound words like "데이터베이스"
// into morphemes, and emits sequential 1-based positions.
func koreanMorphemeAnalyzer(ctx context.Context, input string) ([]TextSearchToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stopwords := map[string]struct{}{
		"은": {}, "는": {}, "이": {}, "가": {}, "을": {}, "를": {}, "의": {}, "및": {},
	}
	compounds := map[string][]string{
		"데이터베이스": {"데이터", "베이스"},
	}
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	var tokens []TextSearchToken
	for _, raw := range fields {
		w := strings.ToLower(strings.TrimSpace(raw))
		if w == "" {
			continue
		}
		// Trim simple Korean trailing particles when attached to a longer stem.
		for _, suffix := range []string{"은", "는", "이", "가", "을", "를"} {
			if len([]rune(w)) > 2 && strings.HasSuffix(w, suffix) {
				w = strings.TrimSuffix(w, suffix)
				break
			}
		}
		if _, stop := stopwords[w]; stop {
			continue
		}
		if parts, ok := compounds[w]; ok {
			for _, part := range parts {
				tokens = append(tokens, TextSearchToken{Lexeme: part})
			}
			continue
		}
		tokens = append(tokens, TextSearchToken{Lexeme: w})
	}
	return tokens, nil
}

func TestCustomTextSearchValidationAndLifecycle(t *testing.T) {
	ctx := t.Context()

	// Validate OpenOptions.TextSearchAnalyzers rejections (SQLSTATE 22023).
	badOpenCases := []struct {
		name      string
		analyzers map[string]TextSearchAnalyzer
	}{
		{"empty_name", map[string]TextSearchAnalyzer{"": koreanMorphemeAnalyzer}},
		{"whitespace_name", map[string]TextSearchAnalyzer{"   ": koreanMorphemeAnalyzer}},
		{"too_long", map[string]TextSearchAnalyzer{strings.Repeat("a", 64): koreanMorphemeAnalyzer}},
		{"nul_byte", map[string]TextSearchAnalyzer{"bad\x00name": koreanMorphemeAnalyzer}},
		{"dotted_name", map[string]TextSearchAnalyzer{"public.bad": koreanMorphemeAnalyzer}},
		{"invalid_utf8", map[string]TextSearchAnalyzer{string([]byte{0xff, 0xfe}): koreanMorphemeAnalyzer}},
		{"nil_analyzer", map[string]TextSearchAnalyzer{"ok_name": nil}},
		{"nil_simple_adapter", map[string]TextSearchAnalyzer{"ok_name": SimpleTextSearchAnalyzer(nil)}},
		{"case_folded_duplicate", map[string]TextSearchAnalyzer{"ko_dup": koreanMorphemeAnalyzer, "KO_DUP": koreanMorphemeAnalyzer}},
	}
	for _, tc := range badOpenCases {
		t.Run("Open_"+tc.name, func(t *testing.T) {
			db, err := Open(ctx, OpenOptions{
				Directory:           testDirectory(t),
				Clock:               newTestClock(),
				TextSearchAnalyzers: tc.analyzers,
			})
			if err == nil {
				_ = db.Close()
				t.Fatalf("expected Open error for %s, got nil", tc.name)
			}
			var sqlErr *Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22023" {
				t.Fatalf("Open(%s) error = %v, want *supersql.Error SQLSTATE 22023", tc.name, err)
			}
		})
	}

	// Open with a pre-registered analyzer using SimpleTextSearchAnalyzer.
	db, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     newTestClock(),
		TextSearchAnalyzers: map[string]TextSearchAnalyzer{
			"KO_Bigram": SimpleTextSearchAnalyzer(func(ctx context.Context, input string) ([]string, error) {
				var out []string
				for _, field := range strings.Fields(strings.ToLower(input)) {
					runes := []rune(field)
					if len(runes) <= 2 {
						out = append(out, field)
						continue
					}
					for i := 0; i+2 <= len(runes); i++ {
						out = append(out, string(runes[i:i+2]))
					}
				}
				return out, nil
			}),
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Case-folded registration matches both lowercase and uppercase unquoted names.
	if got := execTSText(t, db, `SELECT to_tsvector('ko_bigram', '한국어')::text`); got != `'국어':2 '한국':1` {
		t.Fatalf("ko_bigram tsvector = %q, want %q", got, `'국어':2 '한국':1`)
	}
	if !execTSBool(t, db, `SELECT to_tsvector('KO_BIGRAM', '한국어 검색') @@ to_tsquery('ko_bigram', '한국어 & 검색')`) {
		t.Fatal("expected KO_BIGRAM bigram query to match")
	}

	// Dynamic RegisterTextSearchAnalyzer validation rejections.
	for _, bad := range []struct {
		name     string
		analyzer TextSearchAnalyzer
	}{
		{"", koreanMorphemeAnalyzer},
		{"   ", koreanMorphemeAnalyzer},
		{strings.Repeat("x", 64), koreanMorphemeAnalyzer},
		{"a\x00b", koreanMorphemeAnalyzer},
		{"valid_name", nil},
	} {
		err := db.RegisterTextSearchAnalyzer(bad.name, bad.analyzer)
		var sqlErr *Error
		if err == nil || !errors.As(err, &sqlErr) || sqlErr.Code != "22023" {
			t.Fatalf("RegisterTextSearchAnalyzer(%q, %v) = %v, want SQLSTATE 22023", bad.name, bad.analyzer != nil, err)
		}
	}

	// Dynamic register -> use -> replace -> unregister lifecycle.
	if err := db.RegisterTextSearchAnalyzer("dyn_cfg", SimpleTextSearchAnalyzer(func(_ context.Context, s string) ([]string, error) {
		return []string{"v1_" + strings.ToLower(strings.TrimSpace(s))}, nil
	})); err != nil {
		t.Fatalf("RegisterTextSearchAnalyzer: %v", err)
	}
	if got := execTSText(t, db, `SELECT to_tsvector('dyn_cfg', 'Alpha')::text`); got != `'v1_alpha':1` {
		t.Fatalf("v1 dyn_cfg = %q, want %q", got, `'v1_alpha':1`)
	}

	// Replace existing analyzer.
	if err := db.RegisterTextSearchAnalyzer("DYN_CFG", SimpleTextSearchAnalyzer(func(_ context.Context, s string) ([]string, error) {
		return []string{"v2_" + strings.ToLower(strings.TrimSpace(s))}, nil
	})); err != nil {
		t.Fatalf("RegisterTextSearchAnalyzer replace: %v", err)
	}
	if got := execTSText(t, db, `SELECT to_tsvector('dyn_cfg', 'Alpha')::text`); got != `'v2_alpha':1` {
		t.Fatalf("v2 dyn_cfg = %q, want %q", got, `'v2_alpha':1`)
	}

	// Unregister removes the config and unknown names are a safe no-op.
	db.UnregisterTextSearchAnalyzer("never_registered")
	db.UnregisterTextSearchAnalyzer("dyn_cfg")
	if _, err := db.exec(ctx, `SELECT to_tsvector('dyn_cfg', 'Alpha')`, 0); err == nil {
		t.Fatal("expected error after UnregisterTextSearchAnalyzer, got nil")
	}
}

func TestCustomTextSearchKoreanFullWorkflowAndGIN(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     newTestClock(),
		TextSearchAnalyzers: map[string]TextSearchAnalyzer{
			"ko_custom": koreanMorphemeAnalyzer,
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// 1. to_tsvector / to_tsquery / plainto_tsquery / phraseto_tsquery / websearch_to_tsquery
	doc := "순수 Go 임베디드 PostgreSQL 데이터베이스 엔진"
	if got := execTSText(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s')::text`, doc)); !strings.Contains(got, `'데이터':5`) || !strings.Contains(got, `'베이스':6`) || !strings.Contains(got, `'엔진':7`) {
		t.Fatalf("unexpected ko_custom tsvector: %q", got)
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ to_tsquery('ko_custom', '데이터베이스 & 엔진')`, doc)) {
		t.Fatal("expected to_tsquery('ko_custom', '데이터베이스 & 엔진') to match")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ to_tsquery('ko_custom', '임베디드 <-> postgresql')`, doc)) {
		t.Fatal("expected phrase operator <-> in to_tsquery('ko_custom', ...) to match")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ to_tsquery('ko_custom', '데이터:* & !외부')`, doc)) {
		t.Fatal("expected prefix :* and negation ! in to_tsquery('ko_custom', ...) to match")
	}
	// Stopword leaf inside to_tsquery is dropped cleanly just like PostgreSQL.
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ to_tsquery('ko_custom', '은 & 엔진')`, doc)) {
		t.Fatal("expected stopword leaf '은' to be dropped cleanly")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ plainto_tsquery('ko_custom', '임베디드 데이터베이스')`, doc)) {
		t.Fatal("expected plainto_tsquery('ko_custom', ...) to match")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ phraseto_tsquery('ko_custom', 'PostgreSQL 데이터베이스 엔진')`, doc)) {
		t.Fatal("expected phraseto_tsquery('ko_custom', ...) to match adjacent phrase")
	}
	if execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ phraseto_tsquery('ko_custom', '엔진 데이터베이스')`, doc)) {
		t.Fatal("expected reversed phraseto_tsquery('ko_custom', ...) not to match")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ websearch_to_tsquery('ko_custom', '"PostgreSQL 데이터베이스" -외부')`, doc)) {
		t.Fatal("expected websearch_to_tsquery('ko_custom', ...) to match")
	}
	if execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('ko_custom', '%s') @@ websearch_to_tsquery('ko_custom', '"PostgreSQL 데이터베이스" -엔진')`, doc)) {
		t.Fatal("expected negated term in websearch_to_tsquery('ko_custom', ...) to exclude match")
	}

	// 2. jsonb_to_tsvector and ts_headline
	if !execTSBool(t, db, `SELECT jsonb_to_tsvector('ko_custom', '{"title":"임베디드 데이터베이스","count":42}'::jsonb, '["string"]') @@ to_tsquery('ko_custom', '데이터베이스')`) {
		t.Fatal("expected jsonb_to_tsvector with ko_custom to match")
	}
	if headline := execTSText(t, db, `SELECT ts_headline('ko_custom', '순수 Go 임베디드 데이터베이스 엔진', to_tsquery('ko_custom', '데이터베이스 & 엔진'))`); !strings.Contains(headline, "<b>데이터베이스</b>") || !strings.Contains(headline, "<b>엔진</b>") {
		t.Fatalf("unexpected ts_headline output: %q", headline)
	}

	// 3. SET default_text_search_config = 'ko_custom' + 1-arg to_tsvector / to_tsquery
	mustExecGIN(t, db, `SET default_text_search_config = 'ko_custom'`)
	if show := execTSText(t, db, `SHOW default_text_search_config`); show != "ko_custom" {
		t.Fatalf("SHOW default_text_search_config = %q, want %q", show, "ko_custom")
	}
	if cur := execTSText(t, db, `SELECT get_current_ts_config()::text`); cur != "ko_custom" {
		t.Fatalf("get_current_ts_config() = %q, want %q", cur, "ko_custom")
	}
	if !execTSBool(t, db, fmt.Sprintf(`SELECT to_tsvector('%s') @@ to_tsquery('데이터베이스 & 엔진')`, doc)) {
		t.Fatal("expected 1-arg to_tsvector/to_tsquery to use default ko_custom")
	}

	// 4. GIN index lookup + EXPLAIN confirmation + ts_rank ordering
	mustExecGIN(t, db,
		`CREATE TABLE posts (id bigint PRIMARY KEY, body text NOT NULL, tsv tsvector NOT NULL)`,
		`CREATE INDEX idx_posts_tsv ON posts USING gin (tsv)`,
		`INSERT INTO posts VALUES
			(1, '순수 Go 임베디드 PostgreSQL 데이터베이스 엔진', to_tsvector('ko_custom', '순수 Go 임베디드 PostgreSQL 데이터베이스 엔진')),
			(2, '데이터베이스 인덱스 및 검색 엔진 최적화 데이터베이스', to_tsvector('ko_custom', '데이터베이스 인덱스 및 검색 엔진 최적화 데이터베이스')),
			(3, '프론트엔드 웹 브라우저 렌더링', to_tsvector('ko_custom', '프론트엔드 웹 브라우저 렌더링'))`,
	)

	ginQuery := `SELECT id FROM posts WHERE tsv @@ to_tsquery('ko_custom', '데이터베이스 & 엔진') ORDER BY id`
	plan := explainPlanText(t, db, ginQuery)
	if !strings.Contains(plan, "GIN Index Scan using idx_posts_tsv on posts") {
		t.Fatalf("expected GIN Index Scan for ko_custom query, got:\n%s", plan)
	}
	if ids := execGINInt64IDs(t, db, ginQuery); !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("ko_custom GIN query = %v, want [1 2]", ids)
	}

	// Verify parameterized database/sql query with $1::regconfig.
	sqlDB := sql.OpenDB(NewConnectorWithCatalog(db))
	defer func() { _ = sqlDB.Close() }()
	var matched bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_tsvector($1::regconfig, $2) @@ to_tsquery($1::regconfig, $3)`, "ko_custom", doc, "데이터베이스 & 엔진").Scan(&matched); err != nil {
		t.Fatalf("parameterized regconfig query: %v", err)
	}
	if !matched {
		t.Fatal("expected parameterized regconfig query to match")
	}
}

func TestCustomTextSearchWeightsPositionsAndErrors(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Analyzer emitting explicit weights ('A' and 'b') and explicit synonym positions.
	if err := db.RegisterTextSearchAnalyzer("weighted_syn", func(ctx context.Context, input string) ([]TextSearchToken, error) {
		switch input {
		case "title body":
			return []TextSearchToken{
				{Lexeme: "title", Position: 1, Weight: 'A'},
				{Lexeme: "headline", Position: 1, Weight: 'a'}, // same position synonym
				{Lexeme: "body", Position: 2, Weight: 'b'},     // lowercase accepted
			}, nil
		case "headline body":
			return []TextSearchToken{
				{Lexeme: "headline", Position: 1},
				{Lexeme: "body", Position: 2},
			}, nil
		default:
			return []TextSearchToken{{Lexeme: strings.ToLower(input), Position: 1}}, nil
		}
	}); err != nil {
		t.Fatalf("RegisterTextSearchAnalyzer weighted_syn: %v", err)
	}

	vec := execTSText(t, db, `SELECT to_tsvector('weighted_syn', 'title body')::text`)
	if vec != `'body':2B 'headline':1A 'title':1A` {
		t.Fatalf("weighted_syn vector = %q, want %q", vec, `'body':2B 'headline':1A 'title':1A`)
	}
	// Phrase match across synonym at same position 1 followed by position 2!
	if !execTSBool(t, db, `SELECT to_tsvector('weighted_syn', 'title body') @@ phraseto_tsquery('weighted_syn', 'headline body')`) {
		t.Fatal("expected phrase match across same-position synonym")
	}
	// Weight A ranks higher than weight B in ts_rank, and setweight overrides weights cleanly.
	rankA := execTSFloat(t, db, `SELECT ts_rank(to_tsvector('weighted_syn', 'title body'), to_tsquery('weighted_syn', 'title'))`)
	rankB := execTSFloat(t, db, `SELECT ts_rank(to_tsvector('weighted_syn', 'title body'), to_tsquery('weighted_syn', 'body'))`)
	if !(rankA > rankB) {
		t.Fatalf("expected weight A rank (%v) > weight B rank (%v)", rankA, rankB)
	}
	if setW := execTSText(t, db, `SELECT setweight(to_tsvector('weighted_syn', 'title body'), 'C')::text`); setW != `'body':2C 'headline':1C 'title':1C` {
		t.Fatalf("setweight result = %q", setW)
	}

	// Invalid weight byte returns SQLSTATE 22023.
	if err := db.RegisterTextSearchAnalyzer("bad_weight", func(_ context.Context, _ string) ([]TextSearchToken, error) {
		return []TextSearchToken{{Lexeme: "foo", Weight: 'Z'}}, nil
	}); err != nil {
		t.Fatalf("RegisterTextSearchAnalyzer bad_weight: %v", err)
	}
	if _, err := db.exec(ctx, `SELECT to_tsvector('bad_weight', 'anything')`, 0); err == nil {
		t.Fatal("expected error for invalid token weight 'Z'")
	} else {
		var sqlErr *Error
		if !errors.As(wrapError(err), &sqlErr) || sqlErr.Code != "22023" {
			t.Fatalf("bad weight error = %v, want SQLSTATE 22023", err)
		}
	}

	// Custom analyzer error propagation.
	sentinel := errors.New("morphological analyzer backend failed")
	if err := db.RegisterTextSearchAnalyzer("failing_cfg", func(_ context.Context, _ string) ([]TextSearchToken, error) {
		return nil, sentinel
	}); err != nil {
		t.Fatalf("RegisterTextSearchAnalyzer failing_cfg: %v", err)
	}
	if _, err := db.exec(ctx, `SELECT to_tsvector('failing_cfg', 'boom')`, 0); !errors.Is(err, sentinel) {
		t.Fatalf("failing_cfg error = %v, want errors.Is(_, sentinel)", err)
	}
}

func TestCustomTextSearchConcurrentRegistrationAndQueries(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{
		Directory: testDirectory(t),
		Clock:     newTestClock(),
		TextSearchAnalyzers: map[string]TextSearchAnalyzer{
			"ko_race": koreanMorphemeAnalyzer,
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sqlDB := sql.OpenDB(NewConnectorWithCatalog(db))
	defer func() { _ = sqlDB.Close() }()

	var wg sync.WaitGroup
	errCh := make(chan error, 16)

	// Writer goroutines concurrently registering/replacing/unregistering analyzers.
	for g := range 3 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			name := fmt.Sprintf("dynamic_worker_%d", id)
			for iter := range 25 {
				tag := fmt.Sprintf("w%d_%d", id, iter)
				if err := db.RegisterTextSearchAnalyzer(name, SimpleTextSearchAnalyzer(func(_ context.Context, _ string) ([]string, error) {
					return []string{tag}, nil
				})); err != nil {
					errCh <- fmt.Errorf("register %s: %w", name, err)
					return
				}
				if iter%5 == 0 {
					db.UnregisterTextSearchAnalyzer(name)
				}
			}
		}(g)
	}

	// Reader goroutines concurrently executing full-text queries via database/sql.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				var ok bool
				if err := sqlDB.QueryRowContext(ctx, `SELECT to_tsvector('ko_race', '임베디드 데이터베이스 엔진') @@ to_tsquery('ko_race', '데이터베이스 & 엔진')`).Scan(&ok); err != nil {
					errCh <- fmt.Errorf("concurrent query: %w", err)
					return
				}
				if !ok {
					errCh <- errors.New("concurrent query returned false")
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
