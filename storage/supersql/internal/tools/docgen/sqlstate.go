package main

import (
	"fmt"
	"go/ast"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sqlstatePattern is the five-character SQLSTATE shape: two class characters
// and three subclass characters, all digits or upper-case letters.
var sqlstatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// errorHelpers are the constructor helpers whose first argument carries the
// SQLSTATE. bindError and sqlError pass the code through; unsupported fixes it
// to 0A000, which the resolver below reads from the helper's own body.
var errorHelpers = map[string]bool{"bindError": true, "sqlError": true, "unsupported": true}

// errorTypes are the error composite literals whose Code field is a SQLSTATE.
var errorTypes = map[string]bool{"Error": true, "model.Error": true}

// sqlstateOccurrence is one collected SQLSTATE site.
type sqlstateOccurrence struct {
	code     string
	location string
	origin   string
}

// sqlstateFinding is one aggregate row of sqlstates.md.
type sqlstateFinding struct {
	code        string
	class       string
	occurrences int
	sample      string
	origins     []string
}

func renderSQLStates(g *generator) ([]byte, error) {
	occurrences, malformed, dynamic, err := collectSQLStates(g)
	if err != nil {
		return nil, err
	}
	findings := aggregateSQLStates(occurrences)
	writer := newMarkdownWriter()
	writer.heading(1, "SQLSTATE inventory")
	writer.paragraph(fmt.Sprintf(
		"%d distinct SQLSTATE codes appear in non-test Go files outside `testdata/`, collected from `Code` fields of error composite literals and from the first argument of `%s` calls.",
		len(findings), strings.Join(sortedKeys(errorHelpers), ", ")))
	writer.heading(2, "Codes")
	rows := make([][]string, 0, len(findings))
	for _, finding := range findings {
		rows = append(rows, []string{
			finding.code,
			finding.class,
			sqlstateClassName(finding.class),
			fmt.Sprint(finding.occurrences),
			finding.sample,
			strings.Join(finding.origins, ", "),
		})
	}
	writer.table([]string{"Code", "Class", "Class name", "Occurrences", "Sample location", "Origin"}, rows)
	writer.heading(2, "Malformed literals")
	writer.paragraph(plural(len(malformed), "string literal in a SQLSTATE position does not match", "string literals in a SQLSTATE position do not match") + " `^[0-9A-Z]{5}$`.")
	if len(malformed) > 0 {
		malformedRows := make([][]string, 0, len(malformed))
		for _, occurrence := range malformed {
			malformedRows = append(malformedRows, []string{occurrence.location, occurrence.origin, "`" + occurrence.code + "`"})
		}
		writer.table([]string{"Location", "Origin", "Literal"}, malformedRows)
	} else {
		writer.paragraph("None.")
	}
	writer.heading(2, "Non-literal code arguments")
	writer.paragraph(plural(len(dynamic), "site passes a value that is neither a string literal nor a package constant, so its SQLSTATE is only known at run time.", "sites pass a value that is neither a string literal nor a package constant, so their SQLSTATE is only known at run time."))
	if len(dynamic) > 0 {
		dynamicRows := make([][]string, 0, len(dynamic))
		for _, occurrence := range dynamic {
			dynamicRows = append(dynamicRows, []string{occurrence.location, occurrence.origin, "`" + occurrence.code + "`"})
		}
		writer.table([]string{"Location", "Origin", "Expression"}, dynamicRows)
	} else {
		writer.paragraph("None.")
	}
	g.note("sqlstates: %d distinct codes, %d occurrences, %d malformed, %d non-literal", len(findings), len(occurrences), len(malformed), len(dynamic))
	return writer.bytes(), nil
}

// collectSQLStates scans every non-test Go file for SQLSTATE literals and
// returns the well-formed sites, the malformed literals, and the sites whose
// code argument is not a literal at all.
func collectSQLStates(g *generator) (occurrences []sqlstateOccurrence, malformed []sqlstateOccurrence, dynamic []sqlstateOccurrence, err error) {
	constants, functions, err := g.packageSymbols()
	if err != nil {
		return nil, nil, nil, err
	}
	fixed := map[string]string{}
	classify := func(site sqlstateOccurrence, resolved string, fromLiteral bool, helper, raw string) {
		switch {
		case sqlstatePattern.MatchString(resolved):
			site.code = resolved
			occurrences = append(occurrences, site)
		case fromLiteral:
			// A string literal in a SQLSTATE position that is not a SQLSTATE.
			site.code = resolved
			malformed = append(malformed, site)
		default:
			// The helper may fix the code in its own body even though the call
			// passes a symbolic feature name (`unsupported(PhysicalStorage)`).
			if code := fixedHelperCode(g, fixed, helper); helper != "" && sqlstatePattern.MatchString(code) {
				site.code = code
				occurrences = append(occurrences, site)
				return
			}
			site.code = raw
			dynamic = append(dynamic, site)
		}
	}
	err = g.walkSources(func(rel string, file *ast.File) error {
		key := packageKey(rel, file)
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.CallExpr:
				name := callName(typed)
				// Only a package-level helper is a SQLSTATE constructor: a local
				// closure named like one (aggregate's `unsupported`) is not.
				if !errorHelpers[name] || !functions[key][name] || len(typed.Args) == 0 {
					return true
				}
				resolved, fromLiteral, _ := g.resolveCode(typed.Args[0], constants[key])
				classify(sqlstateOccurrence{location: g.position(typed.Pos()), origin: name + " call"}, resolved, fromLiteral, name, g.print(typed.Args[0]))
			case *ast.CompositeLit:
				if !errorTypes[exprName(typed.Type)] {
					return true
				}
				value, ok := compositeField(typed, "Code")
				if !ok {
					return true
				}
				resolved, fromLiteral, _ := g.resolveCode(value, constants[key])
				classify(sqlstateOccurrence{location: g.position(value.Pos()), origin: "model.Error Code field"}, resolved, fromLiteral, "", g.print(value))
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	sortOccurrences(occurrences)
	sortOccurrences(malformed)
	sortOccurrences(dynamic)
	return occurrences, malformed, dynamic, nil
}

// packageSymbols returns, per package, its string constants and its package-level
// function names. Both are needed to resolve a SQLSTATE argument honestly: an
// identifier resolves only inside its own package, and only a package-level
// function is a constructor.
func (g *generator) packageSymbols() (map[string]map[string]string, map[string]map[string]bool, error) {
	constants := map[string]map[string]string{}
	functions := map[string]map[string]bool{}
	err := g.walkSources(func(rel string, file *ast.File) error {
		key := packageKey(rel, file)
		if constants[key] == nil {
			constants[key] = map[string]string{}
			functions[key] = map[string]bool{}
		}
		for _, constant := range g.fileConstants(rel, file) {
			if value, ok := unquote(constant.value); ok {
				constants[key][constant.name] = value
			}
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			functions[key][function.Name.Name] = true
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return constants, functions, nil
}

// resolveCode resolves a SQLSTATE argument: a string literal directly, or an
// identifier naming a string constant of the same package. fromLiteral
// distinguishes the two, because only a literal is a SQLSTATE candidate: an
// identifier may name a profile feature instead.
func (g *generator) resolveCode(expr ast.Expr, constants map[string]string) (value string, fromLiteral bool, ok bool) {
	if value, ok := stringLiteral(expr); ok {
		return value, true, true
	}
	if identifier, ok := expr.(*ast.Ident); ok {
		if value, ok := constants[identifier.Name]; ok {
			return value, false, true
		}
	}
	return "", false, false
}

// fixedHelperCode returns the SQLSTATE a helper fixes in its own body (the
// `unsupported` helper always reports 0A000). Results are memoized because the
// scan visits the same helper for every call site.
func fixedHelperCode(g *generator, cache map[string]string, name string) string {
	if code, ok := cache[name]; ok {
		return code
	}
	code := ""
	found := false
	_ = g.walkSources(func(rel string, file *ast.File) error {
		if found {
			return nil
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != name || function.Recv != nil || function.Body == nil {
				continue
			}
			found = true
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok || !errorTypes[exprName(literal.Type)] {
					return true
				}
				if value, ok := compositeField(literal, "Code"); ok {
					if text, ok := stringLiteral(value); ok {
						code = text
						return false
					}
				}
				return true
			})
			return nil
		}
		return nil
	})
	cache[name] = code
	return code
}

// aggregateSQLStates folds occurrences into one row per code, ordered by code.
func aggregateSQLStates(occurrences []sqlstateOccurrence) []sqlstateFinding {
	byCode := map[string][]sqlstateOccurrence{}
	for _, occurrence := range occurrences {
		byCode[occurrence.code] = append(byCode[occurrence.code], occurrence)
	}
	findings := make([]sqlstateFinding, 0, len(byCode))
	for _, code := range sortedKeys(byCode) {
		sites := byCode[code]
		origins := make([]string, 0, len(sites))
		for _, site := range sites {
			origins = append(origins, site.origin)
		}
		findings = append(findings, sqlstateFinding{
			code:        code,
			class:       code[:2],
			occurrences: len(sites),
			sample:      sites[0].location,
			origins:     sortedUnique(origins),
		})
	}
	return findings
}

func sortOccurrences(occurrences []sqlstateOccurrence) {
	sort.Slice(occurrences, func(i, j int) bool {
		if occurrences[i].code != occurrences[j].code {
			return occurrences[i].code < occurrences[j].code
		}
		return occurrences[i].location < occurrences[j].location
	})
}

// packageKey identifies the package a file belongs to so that identifier
// resolution never crosses package boundaries.
func packageKey(rel string, file *ast.File) string {
	directory := rel
	if index := strings.LastIndex(rel, "/"); index >= 0 {
		directory = rel[:index]
	} else {
		directory = "."
	}
	return directory + ":" + file.Name.Name
}

// unquote returns the value of a quoted Go string literal.
func unquote(literal string) (string, bool) {
	if len(literal) < 2 {
		return "", false
	}
	switch literal[0] {
	case '`':
		return strings.Trim(literal, "`"), true
	case '"':
		value, err := strconv.Unquote(literal)
		if err != nil {
			return "", false
		}
		return value, true
	default:
		return "", false
	}
}

// sqlstateClasses names the two-character SQLSTATE classes the standard
// defines. Unknown classes are rendered by their code.
var sqlstateClasses = map[string]string{
	"00": "successful completion",
	"01": "warning",
	"02": "no data",
	"03": "SQL statement not yet complete",
	"08": "connection exception",
	"09": "triggered action exception",
	"0A": "feature not supported",
	"0B": "invalid transaction initiation",
	"0F": "locator exception",
	"0L": "invalid grantor",
	"0P": "invalid role specification",
	"0Z": "diagnostics exception",
	"20": "case not found",
	"21": "cardinality violation",
	"22": "data exception",
	"23": "integrity constraint violation",
	"24": "invalid cursor state",
	"25": "invalid transaction state",
	"26": "invalid SQL statement name",
	"27": "triggered data change violation",
	"28": "invalid authorization specification",
	"2B": "dependent privilege descriptors still exist",
	"2D": "invalid transaction termination",
	"2F": "SQL routine exception",
	"34": "invalid cursor name",
	"38": "external routine exception",
	"39": "external routine invocation exception",
	"3B": "savepoint exception",
	"3D": "invalid catalog name",
	"3F": "invalid schema name",
	"40": "transaction rollback",
	"42": "syntax error or access rule violation",
	"44": "with check option violation",
	"53": "insufficient resources",
	"54": "program limit exceeded",
	"55": "object not in prerequisite state",
	"57": "operator intervention",
	"58": "system error",
	"72": "snapshot failure",
	"F0": "configuration file error",
	"HV": "foreign data wrapper error",
	"P0": "PL/pgSQL error",
	"XX": "internal error",
}

// sqlstateClassName names a class, falling back to the raw class code.
func sqlstateClassName(class string) string {
	if name, ok := sqlstateClasses[class]; ok {
		return name
	}
	return "unknown class `" + class + "`"
}
