package zpgparse_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestParseExtendedOptions(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		sql                      string
		options                  pg_query.ParseOptions
		wantTemporal, wantOnline int
		wantError                bool
	}{
		{"relation disabled", "SELECT * FROM t AS OF SYSTEM TIME 42", pg_query.ParseOptions{}, 0, 0, true},
		{"transaction disabled", "SET TRANSACTION AS OF SYSTEM TIME $1", pg_query.ParseOptions{}, 0, 0, true},
		{"online disabled", "ALTER TABLE t ADD COLUMN x int ONLINE", pg_query.ParseOptions{}, 0, 0, true},
		{"relation enabled", "SELECT * FROM t AS OF SYSTEM TIME 42", pg_query.ParseOptions{AllowSystemTime: true}, 1, 0, false},
		{"transaction enabled", "SET TRANSACTION AS OF SYSTEM TIME $1", pg_query.ParseOptions{AllowSystemTime: true}, 1, 0, false},
		{"online enabled", "ALTER TABLE t ADD COLUMN x int ONLINE", pg_query.ParseOptions{AllowOnlineDDL: true}, 0, 1, false},
		{"online does not enable temporal", "SELECT * FROM t AS OF SYSTEM TIME 42", pg_query.ParseOptions{AllowOnlineDDL: true}, 0, 0, true},
		{"temporal does not enable online", "ALTER TABLE t ADD COLUMN x int ONLINE", pg_query.ParseOptions{AllowSystemTime: true}, 0, 0, true},
		{"both", "SELECT * FROM t AS OF SYSTEM TIME 42; ALTER TABLE t DROP COLUMN x ONLINE", pg_query.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true}, 1, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := pg_query.ParseExtended(tt.sql, tt.options)
			if tt.wantError {
				if err == nil || result != nil {
					t.Fatalf("ParseExtended(%q) = %v, %v; want nil result and error", tt.sql, result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(result.TemporalClauses) != tt.wantTemporal || len(result.OnlineDDLClauses) != tt.wantOnline {
				t.Fatalf("sidecars = temporal %d, online %d; want %d, %d", len(result.TemporalClauses), len(result.OnlineDDLClauses), tt.wantTemporal, tt.wantOnline)
			}
			if len(result.TemporalClauses) != 0 {
				clause := result.TemporalClauses[0]
				if clause.StatementIndex != 0 || clause.Expression == nil {
					t.Fatalf("temporal clause = %+v", clause)
				}
				if strings.HasPrefix(tt.sql, "SET") {
					if clause.RelationLocation != -1 || clause.Expression.GetParamRef().GetNumber() != 1 || result.ParseResult.Stmts[0].Stmt.GetVariableSetStmt().GetName() != "TRANSACTION" {
						t.Fatalf("transaction form lost its discriminator or expression: %+v", clause)
					}
				} else if clause.Expression.GetAConst().GetIval().GetIval() != 42 {
					t.Fatalf("time expression = %v; want integer 42", clause.Expression)
				}
			}
		})
	}
}

func TestParseBareDefinitionFlags(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sql      string
		argState map[string]bool
	}{
		{
			name: "operator flags",
			sql: `CREATE OPERATOR === (
				LEFTARG = boolean, RIGHTARG = boolean, PROCEDURE = fn_op2,
				SORT1, SORT2, LTCMP, GTCMP, HASHES, MERGES
			)`,
			argState: map[string]bool{
				"sort1": false, "sort2": false, "ltcmp": false,
				"gtcmp": false, "hashes": false, "merges": false,
			},
		},
		{
			name: "aggregate flags",
			sql: `CREATE AGGREGATE my_rank(VARIADIC "any" ORDER BY VARIADIC "any") (
				STYPE = internal, SFUNC = ordered_set_transition_multi,
				FINALFUNC = rank_final, FINALFUNC_EXTRA = true, HYPOTHETICAL
			)`,
			argState: map[string]bool{"finalfunc_extra": true, "hypothetical": false},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := pg_query.Parse(tt.sql)
			if err != nil {
				t.Fatal(err)
			}
			define := result.Stmts[0].Stmt.GetDefineStmt()
			if define == nil {
				t.Fatal("statement is not a DefineStmt")
			}
			definition := define.Definition
			found := make(map[string]bool, len(definition))
			for _, node := range definition {
				option := node.GetDefElem()
				if option != nil {
					found[option.Defname] = option.Arg != nil
				}
			}
			for name, wantArg := range tt.argState {
				if gotArg, ok := found[name]; !ok || gotArg != wantArg {
					t.Errorf("definition %q argument state = %t, %t; want %t, true", name, gotArg, ok, wantArg)
				}
			}
		})
	}
}

func TestParseExtendedAppliesStringLexicalSettings(t *testing.T) {
	for _, tt := range []struct {
		name        string
		sql         string
		options     pg_query.ParseOptions
		want        string
		wantWarnings int
		wantPolicy  pg_query.LexicalPolicy
	}{
		{
			name: "zero value keeps ordinary backslashes",
			sql:  `SELECT 'a\nb'`,
			want: `a\nb`,
			wantPolicy: pg_query.LexicalPolicy{
				StandardConformingStrings: true,
				EscapeStringWarning:       true,
				BackslashQuote:            pg_query.BackslashQuoteSafeEncoding,
			},
		},
		{
			name: "ordinary strings decode when disabled",
			sql:  `SELECT 'a\nb'`,
			options: pg_query.ParseOptions{
				StandardConformingStrings: pg_query.BooleanFalse,
			},
			want:         "a\nb",
			wantWarnings: 1,
			wantPolicy: pg_query.LexicalPolicy{
				StandardConformingStrings: false,
				EscapeStringWarning:       true,
				BackslashQuote:            pg_query.BackslashQuoteSafeEncoding,
			},
		},
		{
			name: "ordinary string warning can be disabled",
			sql:  `SELECT 'a\nb'`,
			options: pg_query.ParseOptions{
				StandardConformingStrings: pg_query.BooleanFalse,
				EscapeStringWarning:       pg_query.BooleanFalse,
			},
			want: "a\nb",
			wantPolicy: pg_query.LexicalPolicy{
				StandardConformingStrings: false,
				EscapeStringWarning:       false,
				BackslashQuote:            pg_query.BackslashQuoteSafeEncoding,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, value := parseExtendedString(t, tt.sql, tt.options)
			if value != tt.want {
				t.Fatalf("string value = %q; want %q", value, tt.want)
			}
			if len(result.Diagnostics) != tt.wantWarnings {
				t.Fatalf("diagnostics = %+v; want %d warnings", result.Diagnostics, tt.wantWarnings)
			}
			if result.LexicalPolicy != tt.wantPolicy {
				t.Fatalf("lexical policy = %+v; want %+v", result.LexicalPolicy, tt.wantPolicy)
			}
		})
	}

	ordinary, err := pg_query.Parse(`SELECT 'a\nb'`)
	if err != nil {
		t.Fatal(err)
	}
	extended, _ := parseExtendedString(t, `SELECT 'a\nb'`, pg_query.ParseOptions{})
	if !proto.Equal(ordinary, extended.ParseResult) {
		t.Fatal("zero-value ParseExtended differs from Parse")
	}
}

func TestEscapeStringsIgnoreOrdinaryStringMode(t *testing.T) {
	for _, standard := range []pg_query.BooleanOption{pg_query.BooleanTrue, pg_query.BooleanFalse} {
		result, value := parseExtendedString(t, `SELECT E'a\nb'`, pg_query.ParseOptions{
			StandardConformingStrings: standard,
		})
		if value != "a\nb" {
			t.Fatalf("standard_conforming_strings %d produced %q; want newline", standard, value)
		}
		if len(result.Diagnostics) != 0 {
			t.Fatalf("E string produced warnings: %+v", result.Diagnostics)
		}
	}
}

func TestOrdinaryStringWarningsAreOwnedPerLiteralAndParse(t *testing.T) {
	const sql = "SELECT '前', 'a\\tb\\\\c', 'd\\'e', 'f\\\\g'"
	options := pg_query.ParseOptions{StandardConformingStrings: pg_query.BooleanFalse}
	result, err := pg_query.ParseExtended(sql, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Diagnostics) != 3 {
		t.Fatalf("diagnostics = %+v; want one warning per escaped literal", result.Diagnostics)
	}
	want := []struct {
		location int
		message  string
		hint     string
		funcname string
	}{
		{
			location: strings.Index(sql, "'a"),
			message:  "nonstandard use of escape in a string literal",
			hint:     "Use the escape string syntax for escapes, e.g., E'\\r\\n'.",
			funcname: "check_escape_warning",
		},
		{
			location: strings.Index(sql, "'d"),
			message:  "nonstandard use of \\' in a string literal",
			hint:     "Use '' to write quotes in strings, or use the escape string syntax (E'...').",
			funcname: "check_string_escape_warning",
		},
		{
			location: strings.Index(sql, "'f"),
			message:  "nonstandard use of \\\\ in a string literal",
			hint:     "Use the escape string syntax for backslashes, e.g., E'\\\\'.",
			funcname: "check_string_escape_warning",
		},
	}
	for i, diagnostic := range result.Diagnostics {
		expected := want[i]
		if diagnostic.Severity != pg_query.SeverityWarning || diagnostic.Code != "22P06" ||
			diagnostic.Message != expected.message || diagnostic.Hint != expected.hint ||
			diagnostic.Funcname != expected.funcname || diagnostic.Filename != "scan.l" ||
			diagnostic.Location != expected.location ||
			diagnostic.Cursorpos != len([]rune(sql[:expected.location]))+1 {
			t.Errorf("diagnostic %d = %+v; want warning at byte %d", i, diagnostic, expected.location)
		}
	}
	second, err := pg_query.ParseExtended(`SELECT 'plain'`, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Diagnostics) != 0 {
		t.Fatalf("warnings leaked to a later parse: %+v", second.Diagnostics)
	}
}

func TestBackslashQuotePolicyUsesFixedUTF8Encoding(t *testing.T) {
	const sql = `SELECT E'it\'s'`
	for _, mode := range []pg_query.BackslashQuoteMode{pg_query.BackslashQuoteOn, pg_query.BackslashQuoteSafeEncoding} {
		_, value := parseExtendedString(t, sql, pg_query.ParseOptions{BackslashQuote: mode})
		if value != "it's" {
			t.Fatalf("backslash_quote mode %d produced %q", mode, value)
		}
	}
	_, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{BackslashQuote: pg_query.BackslashQuoteOff})
	parseErr := requireParseError(t, err, sql, "22P06", "unsafe use of \\' in a string literal", strings.Index(sql, "E'"))
	const hint = "Use '' to write quotes in strings. \\' is insecure in client-only encodings."
	if parseErr.Hint != hint {
		t.Fatalf("hint = %q; want %q", parseErr.Hint, hint)
	}
	const ordinary = `SELECT 'it\'s'`
	_, err = pg_query.ParseExtended(ordinary, pg_query.ParseOptions{
		StandardConformingStrings: pg_query.BooleanFalse,
		BackslashQuote:            pg_query.BackslashQuoteOff,
	})
	parseErr = requireParseError(t, err, ordinary, "22P06", "unsafe use of \\' in a string literal", strings.Index(ordinary, "'"))
	if parseErr.Hint != hint {
		t.Fatalf("hint = %q; want %q", parseErr.Hint, hint)
	}
}

func TestUnicodeStringRequiresStandardConformingStrings(t *testing.T) {
	const sql = `SELECT U&'\0061'`
	_, value := parseExtendedString(t, sql, pg_query.ParseOptions{})
	if value != "a" {
		t.Fatalf("Unicode string value = %q; want a", value)
	}

	_, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{
		StandardConformingStrings: pg_query.BooleanFalse,
	})
	parseErr := requireParseError(t, err, sql, "0A000", "unsafe use of string constant with Unicode escapes", strings.Index(sql, "U&"))
	if parseErr.Detail != `String constants with Unicode escapes cannot be used when "standard_conforming_strings" is off.` {
		t.Fatalf("detail = %q", parseErr.Detail)
	}
}

func TestStringLexerPreservesPostgreSQLSQLStatesAndLocations(t *testing.T) {
	for _, tt := range []struct {
		name, sql, code, message string
		hint                     string
		location                 int
	}{
		{
			name:     "malformed Unicode escape",
			sql:      `SELECT E'\u12'`,
			code:     "22025",
			message:  "invalid Unicode escape",
			location: strings.Index(`SELECT E'\u12'`, `\`),
			hint:     `Unicode escapes must be \uXXXX or \UXXXXXXXX.`,
		},
		{
			name:     "zero Unicode value",
			sql:      `SELECT E'\u0000'`,
			code:     "42601",
			message:  "invalid Unicode escape value",
			location: strings.Index(`SELECT E'\u0000'`, `\`),
		},
		{
			name:     "escape creates invalid UTF8",
			sql:      `SELECT E'\x80'`,
			code:     "22021",
			message:  `invalid byte sequence for encoding "UTF8": 0x80`,
			location: strings.Index(`SELECT E'\x80'`, "E'"),
		},
		{
			name:     "source contains invalid UTF8",
			sql:      "SELECT '" + string([]byte{0xff}) + "'",
			code:     "22021",
			message:  `invalid byte sequence for encoding "UTF8": 0xff`,
			location: strings.Index("SELECT '", "'"),
		},
		{
			name:     "malformed Unicode quoted string escape",
			sql:      "SELECT '前', U&'\\12'",
			code:     "42601",
			message:  "invalid Unicode escape",
			hint:     `Unicode escapes must be \XXXX or \+XXXXXX.`,
			location: strings.Index("SELECT '前', U&'\\12'", `\`),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pg_query.ParseExtended(tt.sql, pg_query.ParseOptions{})
			parseErr := requireParseError(t, err, tt.sql, tt.code, tt.message, tt.location)
			if parseErr.Hint != tt.hint {
				t.Fatalf("hint = %q; want %q", parseErr.Hint, tt.hint)
			}
		})
	}
}

func TestParseExtendedRejectsInvalidLexicalOptions(t *testing.T) {
	for _, options := range []pg_query.ParseOptions{
		{StandardConformingStrings: pg_query.BooleanOption(99)},
		{EscapeStringWarning: pg_query.BooleanOption(99)},
		{BackslashQuote: pg_query.BackslashQuoteMode(99)},
	} {
		result, err := pg_query.ParseExtended("SELECT 1", options)
		if result != nil {
			t.Fatalf("invalid options returned result: %+v", result)
		}
		var parseErr *pg_query.Error
		if !errors.As(err, &parseErr) || parseErr.Code != "22023" || parseErr.Location != -1 {
			t.Fatalf("invalid options error = %#v", err)
		}
	}
	if _, err := pg_query.ParseBackslashQuote("unsupported"); err == nil {
		t.Fatal("ParseBackslashQuote accepted an unsupported value")
	}
}

func TestParseExtendedConcurrentLexicalPoliciesAreIsolated(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options pg_query.ParseOptions
		want    string
		warnings int
	}{
		{"standard", pg_query.ParseOptions{StandardConformingStrings: pg_query.BooleanTrue}, `a\nb`, 0},
		{"legacy", pg_query.ParseOptions{StandardConformingStrings: pg_query.BooleanFalse}, "a\nb", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for range 50 {
				result, value := parseExtendedString(t, `SELECT 'a\nb'`, tt.options)
				if value != tt.want || len(result.Diagnostics) != tt.warnings {
					t.Fatalf("value/diagnostics = %q/%+v; want %q/%d", value, result.Diagnostics, tt.want, tt.warnings)
				}
			}
		})
	}
}

func parseExtendedString(t testing.TB, sql string, options pg_query.ParseOptions) (*pg_query.ExtendedParseResult, string) {
	t.Helper()
	result, err := pg_query.ParseExtended(sql, options)
	if err != nil {
		t.Fatal(err)
	}
	statements := result.ParseResult.Stmts
	if len(statements) != 1 {
		t.Fatalf("statement count = %d; want 1", len(statements))
	}
	value := statements[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val.GetAConst().GetSval().GetSval()
	return result, value
}

func requireParseError(t testing.TB, err error, sql, code, message string, location int) *pg_query.Error {
	t.Helper()
	var parseErr *pg_query.Error
	if !errors.As(err, &parseErr) {
		t.Fatalf("error = %#v; want *zpgparse.Error", err)
	}
	if parseErr.Code != code || parseErr.Message != message || parseErr.Location != location ||
		parseErr.Cursorpos != len([]rune(sql[:location]))+1 || parseErr.Funcname != "scanner_yyerror" ||
		parseErr.Filename != "scan.l" {
		t.Fatalf("parse error = %+v; want code %s, message %q, byte %d", parseErr, code, message, location)
	}
	return parseErr
}

func TestParseExtendedRetainsStatementIndicesAndByteOffsets(t *testing.T) {
	const sql = ";; /* 前 */ SELECT 'é'; ; WITH c AS (SELECT * FROM \"表\" AS /*時*/ OF SYSTEM TIME $2 AS p) SELECT * FROM c JOIN (SELECT * FROM t AS OF SYSTEM TIME 40 + 2 q) s ON true; ; SET TRANSACTION AS OF SYSTEM TIME '2026-01-01'::timestamptz; ALTER TABLE \"表\" ADD COLUMN x int ONLINE; ;"
	result, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ParseResult.Stmts) != 4 || len(result.TemporalClauses) != 3 || len(result.OnlineDDLClauses) != 1 {
		t.Fatalf("statement/temporal/online counts = %d/%d/%d; want 4/3/1", len(result.ParseResult.Stmts), len(result.TemporalClauses), len(result.OnlineDDLClauses))
	}
	for i, want := range []struct {
		statement        int
		relation, clause string
	}{
		{1, "\"表\" AS", "AS /*時*/"},
		{1, "t AS OF", "AS OF SYSTEM TIME 40"},
		{2, "", "AS OF SYSTEM TIME '2026"},
	} {
		clause := result.TemporalClauses[i]
		relation := int32(-1)
		if want.relation != "" {
			relation = int32(strings.Index(sql, want.relation))
		}
		if clause.StatementIndex != want.statement || clause.RelationLocation != relation || clause.Location != int32(strings.Index(sql, want.clause)) {
			t.Errorf("clause %d = %+v; want statement %d, relation byte %d, clause byte %d", i, clause, want.statement, relation, strings.Index(sql, want.clause))
		}
	}
	param := result.TemporalClauses[0].Expression.GetParamRef()
	if param.GetNumber() != 2 || param.GetLocation() != int32(strings.Index(sql, "$2")) {
		t.Fatalf("parameter = %v; want $2 at original byte position", param)
	}
	sum := result.TemporalClauses[1].Expression.GetAExpr()
	if sum == nil || sum.Lexpr.GetAConst().GetIval().GetIval() != 40 || sum.Rexpr.GetAConst().GetIval().GetIval() != 2 || sum.Location != int32(strings.Index(sql, "+")) {
		t.Fatalf("arithmetic expression changed: %v", sum)
	}
	cast := result.TemporalClauses[2].Expression.GetTypeCast()
	if cast == nil || cast.Arg.GetAConst().GetSval().GetSval() != "2026-01-01" || cast.Arg.GetAConst().Location != int32(strings.Index(sql, "'2026")) {
		t.Fatalf("timestamp expression changed: %v", cast)
	}
	outer := result.ParseResult.Stmts[1].Stmt.GetSelectStmt()
	cte := outer.WithClause.Ctes[0].GetCommonTableExpr().Ctequery.GetSelectStmt().FromClause[0].GetRangeVar()
	join := outer.FromClause[0].GetJoinExpr()
	nested := join.Rarg.GetRangeSubselect()
	if cte.Alias.Aliasname != "p" || nested.Alias.Aliasname != "s" || nested.Subquery.GetSelectStmt().FromClause[0].GetRangeVar().Alias.Aliasname != "q" {
		t.Fatalf("CTE or nested join aliases changed: %v", outer)
	}
	online := result.OnlineDDLClauses[0]
	if online.StatementIndex != 3 || online.Location != int32(strings.Index(sql, "ONLINE")) {
		t.Fatalf("online marker = %+v", online)
	}
}

func TestParseExtendedPreservesExpressionSyntax(t *testing.T) {
	for _, expression := range []string{
		"$1", "-42", "(40 + 2)", "'AS OF SYSTEM TIME ONLINE'", "$tag$AS OF SYSTEM TIME ONLINE;$tag$",
		"TIMESTAMP '2026-01-01'", "now() - INTERVAL '1' DAY", "1 BETWEEN (0) AND 2", "1 BETWEEN ASYMMETRIC 0 AND 2",
		"sum(x) FILTER (WHERE x > 0) OVER ()", "CAST(1 AS DOUBLE PRECISION)",
	} {
		t.Run(expression, func(t *testing.T) {
			const prefix = "SELECT * FROM t AS OF SYSTEM TIME "
			result, err := pg_query.ParseExtended(prefix+expression+" AS alias", pg_query.ParseOptions{AllowSystemTime: true})
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := pg_query.Parse(strings.Repeat(" ", len(prefix)-len("SELECT ")) + "SELECT " + expression)
			if err != nil {
				t.Fatal(err)
			}
			want := ordinary.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
			if len(result.TemporalClauses) != 1 || !proto.Equal(result.TemporalClauses[0].Expression, want) {
				t.Fatalf("temporal expression differs from PostgreSQL expression AST: got %v, want %v", result.TemporalClauses, want)
			}
			if got := result.ParseResult.Stmts[0].Stmt.GetSelectStmt().FromClause[0].GetRangeVar().Alias.Aliasname; got != "alias" {
				t.Fatalf("alias = %q; want alias", got)
			}
		})
	}
}

func TestParseExtendedPreservesPostgreSQLIdentifiersAndQuotedText(t *testing.T) {
	for _, sql := range []string{
		"SELECT online, system, time FROM online AS of",
		"SELECT 'AS OF SYSTEM TIME 1', $$ALTER TABLE t ADD x int ONLINE$$ -- AS OF SYSTEM TIME 2\n",
		"SELECT 1 /* AS OF SYSTEM TIME 2 /* ONLINE */ */",
		"CREATE TABLE online (online int)",
		"ALTER TABLE online ADD online int",
		"ALTER TABLE online ADD x online",
		"ALTER TABLE online ALTER x TYPE online",
		"ALTER TABLE online ALTER x SET DEFAULT online",
		"ALTER TABLE online DROP COLUMN online",
		"ALTER TABLE online RENAME COLUMN x TO online",
		"ALTER TABLE online ADD x int DEFAULT 'ONLINE' /* ONLINE */",
	} {
		t.Run(sql, func(t *testing.T) {
			ordinary, err := pg_query.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			for _, options := range []pg_query.ParseOptions{{}, {AllowSystemTime: true, AllowOnlineDDL: true}} {
				result, err := pg_query.ParseExtended(sql, options)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.TemporalClauses) != 0 || len(result.OnlineDDLClauses) != 0 || !proto.Equal(result.ParseResult, ordinary) {
					t.Fatalf("PostgreSQL statement acquired extensions or changed AST: %+v", result)
				}
			}
		})
	}
}

func TestParseExtendedOnlineCommandBoundary(t *testing.T) {
	for _, sql := range []string{
		"ALTER TABLE t ADD x int ONLINE",
		"ALTER TABLE IF EXISTS t DROP COLUMN x ONLINE;",
		"ALTER TABLE ONLY t ALTER COLUMN x TYPE bigint USING x::bigint ONLINE",
		"ALTER TABLE t ADD PRIMARY KEY (x) ONLINE",
		"ALTER TABLE t ADD x int, DROP COLUMN y ONLINE /* end */",
		"ALTER TABLE online ADD online online oNlInE",
	} {
		t.Run(sql, func(t *testing.T) {
			result, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{AllowOnlineDDL: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.OnlineDDLClauses) != 1 || result.OnlineDDLClauses[0].StatementIndex != 0 || result.ParseResult.Stmts[0].Stmt.GetAlterTableStmt() == nil {
				t.Fatalf("ONLINE did not mark ALTER TABLE: %+v", result)
			}
		})
	}
}

func TestParseExtendedRejectsMalformedOrLegacyExtensions(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM t AS OF 42", "SELECT * FROM t FOR SYSTEM_TIME AS OF 42",
		"SELECT * FROM t AS OF SYSTEM TIME", "SELECT * FROM t AS OF SYSTEM TIME (1 +)",
		"SELECT * FROM t alias AS OF SYSTEM TIME 42", "SELECT * FROM (SELECT 1) s AS OF SYSTEM TIME 42",
		"SET TRANSACTION AS OF SYSTEM TIME", "SET LOCAL TRANSACTION AS OF SYSTEM TIME 42",
		"SET SESSION TRANSACTION AS OF SYSTEM TIME 42",
		"ALTER TABLE t ONLINE ADD x int", "ALTER TABLE t ADD x int ONLINE, DROP COLUMN y",
		"ALTER TABLE t ADD x int ONLINE ONLINE", "ALTER TABLE t ADD x int \"ONLINE\"",
		"ALTER TABLE t ADD x int U&\"online\"", "ALTER TABLE t ADD x int 'ONLINE'",
		"ALTER TABLE t ADD x int OFFLINE", "ALTER TABLE t ADD x int ONLINE SELECT 1",
		"ALTER TABLE t ADD x int $$ONLINE$$", "ALTER INDEX t ALTER COLUMN x TYPE int ONLINE",
		"CREATE TABLE t (x int) ONLINE", "ALTER TABLE t RENAME TO x ONLINE",
	} {
		t.Run(sql, func(t *testing.T) {
			result, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
			if err == nil || result != nil {
				t.Fatalf("ParseExtended(%q) = %v, %v; want syntax rejection", sql, result, err)
			}
		})
	}
}

func TestPostgreSQLAPIsRejectExtensions(t *testing.T) {
	for _, sql := range []string{"SELECT * FROM t AS OF SYSTEM TIME 42", "SET TRANSACTION AS OF SYSTEM TIME $1", "ALTER TABLE t ADD x int ONLINE"} {
		t.Run(sql, func(t *testing.T) {
			if _, err := pg_query.Parse(sql); err == nil {
				t.Error("Parse accepted extension")
			}
			if _, err := pg_query.ParseToJSON(sql); err == nil {
				t.Error("ParseToJSON accepted extension")
			}
			if _, err := pg_query.Normalize(sql); err == nil {
				t.Error("Normalize discarded extension identity")
			}
			if _, err := pg_query.NormalizeUtility(sql); err == nil {
				t.Error("NormalizeUtility discarded extension identity")
			}
			if _, err := pg_query.Fingerprint(sql); err == nil {
				t.Error("Fingerprint discarded extension identity")
			}
			if _, err := pg_query.FingerprintToUInt64(sql); err == nil {
				t.Error("FingerprintToUInt64 discarded extension identity")
			}
		})
	}
}

func TestParseExtendedConcurrentCallsOwnSidecars(t *testing.T) {
	for worker := range 16 {
		t.Run(fmt.Sprint(worker), func(t *testing.T) {
			t.Parallel()
			for iteration := range 20 {
				value := worker*100 + iteration
				sql := fmt.Sprintf(";; SELECT * FROM t AS OF SYSTEM TIME %d; ; ALTER TABLE t ADD x int ONLINE", value)
				result, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.TemporalClauses) != 1 || len(result.OnlineDDLClauses) != 1 {
					t.Fatalf("cross-call sidecars: %+v", result)
				}
				clause := result.TemporalClauses[0]
				if clause.StatementIndex != 0 || clause.Expression.GetAConst().GetIval().GetIval() != int32(value) || result.OnlineDDLClauses[0].StatementIndex != 1 {
					t.Fatalf("cross-call statement identity: %+v", result)
				}
				if _, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{}); err == nil {
					t.Fatal("extension flags leaked across calls")
				}
			}
		})
	}
}

func TestParseExtendedEmptyStatements(t *testing.T) {
	for _, sql := range []string{"", ";;", "; /* 前 */ ; -- ONLINE AS OF SYSTEM TIME\n;"} {
		result, err := pg_query.ParseExtended(sql, pg_query.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.ParseResult.Stmts) != 0 || len(result.TemporalClauses) != 0 || len(result.OnlineDDLClauses) != 0 {
			t.Fatalf("empty SQL %q produced statements or extensions: %+v", sql, result)
		}
	}
}
