package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// Expected catalog sizes. They are the pin of the embedded profile: a change in
// any of them is a deliberate catalog regeneration and must update this file.
const (
	expectedSignatures = 3397
	expectedOperators  = 799
	expectedCasts      = 235
	expectedTypes      = 193
)

// renderFunctions builds functions.md from the pinned builtin catalog. Unlike
// the other renderers this one reads the compiled package rather than source
// text, because the catalog is the authoritative, already-validated form.
func renderFunctions(g *generator) ([]byte, error) {
	signatures := collect(builtin.Signatures())
	operators := collect(builtin.Operators())
	casts := collect(builtin.Casts())
	types := collect(builtin.Types())
	collations := collect(builtin.Collations())
	operatorClasses := collect(builtin.OperatorClasses())
	rangeFamilies := collect(builtin.RangeFamilies())

	sort.Slice(signatures, func(i, j int) bool {
		if signatures[i].Name != signatures[j].Name {
			return signatures[i].Name < signatures[j].Name
		}
		return signatures[i].ID < signatures[j].ID
	})
	sort.Slice(operators, func(i, j int) bool {
		if operators[i].Name != operators[j].Name {
			return operators[i].Name < operators[j].Name
		}
		return operators[i].ID < operators[j].ID
	})
	sort.Slice(casts, func(i, j int) bool {
		if casts[i].Source != casts[j].Source {
			return casts[i].Source < casts[j].Source
		}
		return casts[i].Target < casts[j].Target
	})

	writer := newMarkdownWriter()
	writer.heading(1, "Builtin catalog reference")
	writer.paragraph("Every identity below is pinned by the embedded profile's generated catalog tables; those tables are the resolution surface for builtin names, operators, casts, types, collations, operator classes and range families.")
	writer.heading(2, "Catalog sizes")
	writer.table([]string{"Catalog", "Count", "Expected"}, [][]string{
		{"signatures", fmt.Sprint(len(signatures)), fmt.Sprint(expectedSignatures)},
		{"operators", fmt.Sprint(len(operators)), fmt.Sprint(expectedOperators)},
		{"casts", fmt.Sprint(len(casts)), fmt.Sprint(expectedCasts)},
		{"types", fmt.Sprint(len(types)), fmt.Sprint(expectedTypes)},
		{"collations", fmt.Sprint(len(collations)), "-"},
		{"operator classes", fmt.Sprint(len(operatorClasses)), "-"},
		{"range families", fmt.Sprint(len(rangeFamilies)), "-"},
	})
	writer.heading(2, "Type kinds")
	writeTypeKinds(writer, types)

	writer.heading(2, "Functions and aggregates")
	writer.paragraph(fmt.Sprintf("%d signatures. `Kind` is the catalog `prokind`: `f` function, `a` aggregate, `w` window, `p` procedure.", len(signatures)))
	rows := make([][]string, 0, len(signatures))
	for _, signature := range signatures {
		rows = append(rows, []string{
			signature.Name,
			fmt.Sprint(signature.ID),
			argumentList(signature),
			resultType(signature),
			volatilityName(signature.Volatility),
			yesNo(signature.Strict),
			yesNo(signature.ReturnsSet),
			yesNo(signature.Kind == 'a'),
			kindName(signature.Kind),
		})
	}
	writer.table([]string{"Name", "ID", "Arguments", "Returns", "Volatility", "Strict", "Returns set", "Aggregate", "Kind"}, rows)

	writer.heading(2, "Operators")
	writer.paragraph(fmt.Sprintf("%d operators.", len(operators)))
	rows = make([][]string, 0, len(operators))
	for _, operator := range operators {
		rows = append(rows, []string{
			operator.Name,
			fmt.Sprint(operator.ID),
			typeName(operator.Left),
			typeName(operator.Right),
			typeName(operator.Result),
			fmt.Sprint(operator.Function),
			fmt.Sprint(operator.Commutator),
			fmt.Sprint(operator.Negator),
			yesNo(operator.HashJoinable),
			yesNo(operator.MergeJoinable),
		})
	}
	writer.table([]string{"Name", "ID", "Left", "Right", "Result", "Function", "Commutator", "Negator", "Hash joinable", "Merge joinable"}, rows)

	writer.heading(2, "Casts")
	writer.paragraph(fmt.Sprintf("%d casts. `Context` is `pg_cast.castcontext` (`i` implicit, `a` assignment, `e` explicit); `Method` is `pg_cast.castmethod` (`f` function, `b` binary coercible, `i` inout).", len(casts)))
	rows = make([][]string, 0, len(casts))
	for _, cast := range casts {
		rows = append(rows, []string{
			fmt.Sprint(cast.OID),
			typeName(cast.Source),
			typeName(cast.Target),
			fmt.Sprint(cast.Function),
			castContextName(cast.Context),
			castMethodName(cast.Method),
		})
	}
	writer.table([]string{"OID", "Source", "Target", "Function", "Context", "Method"}, rows)

	writer.heading(2, "Types")
	writer.paragraph(fmt.Sprintf("%d types. `Category` is `pg_type.typcategory`, `Kind` is `pg_type.typtype`, and `Element` is `pg_type.typelem`.", len(types)))
	rows = make([][]string, 0, len(types))
	for _, info := range types {
		rows = append(rows, []string{
			info.Name,
			fmt.Sprint(info.ID),
			string(rune(info.Category)),
			typeKindName(info.Kind),
			typeName(info.Element),
			typeName(info.Array),
			yesNo(info.Preferred),
			yesNo(info.IsArray()),
			fmt.Sprint(info.Collation),
			fmt.Sprint(info.Input),
			fmt.Sprint(info.Output),
		})
	}
	writer.table([]string{"Name", "ID", "Category", "Kind", "Element", "Array", "Preferred", "Array type", "Collation", "Input", "Output"}, rows)

	writer.heading(2, "Collations")
	writer.paragraph(fmt.Sprintf("%d collations. `Provider` is `pg_collation.collprovider` (`d` default, `b` builtin, `c` libc, `i` icu).", len(collations)))
	rows = make([][]string, 0, len(collations))
	for _, info := range collations {
		rows = append(rows, []string{
			info.Name,
			fmt.Sprint(info.ID),
			collationProviderName(info.Provider),
			yesNo(info.Deterministic),
			fmt.Sprint(info.Encoding),
		})
	}
	writer.table([]string{"Name", "ID", "Provider", "Deterministic", "Encoding"}, rows)

	writer.heading(2, "Operator classes")
	writer.paragraph(fmt.Sprintf("%d operator classes.", len(operatorClasses)))
	rows = make([][]string, 0, len(operatorClasses))
	for _, info := range operatorClasses {
		rows = append(rows, []string{
			info.Name,
			fmt.Sprint(info.ID),
			info.Method,
			typeName(info.InputType),
			fmt.Sprint(info.Family),
			info.FamilyName,
			yesNo(info.Default),
			typeName(info.KeyType),
			fmt.Sprint(info.EqualityFunction),
			fmt.Sprint(info.CompareFunction),
			fmt.Sprint(info.HashFunction),
		})
	}
	writer.table([]string{"Name", "ID", "Method", "Input", "Family", "Family name", "Default", "Key type", "Equality", "Compare", "Hash"}, rows)

	writer.heading(2, "Range families")
	writer.paragraph(fmt.Sprintf("%d range families.", len(rangeFamilies)))
	rows = make([][]string, 0, len(rangeFamilies))
	for _, family := range rangeFamilies {
		rows = append(rows, []string{
			typeName(family.Range),
			typeName(family.Multirange),
			typeName(family.Subtype),
			fmt.Sprint(family.SubtypeClass),
			fmt.Sprint(family.Collation),
			fmt.Sprint(family.Canonical),
			fmt.Sprint(family.Difference),
		})
	}
	writer.table([]string{"Range", "Multirange", "Subtype", "Subtype class", "Collation", "Canonical", "Difference"}, rows)

	g.note("functions: %d signatures, %d operators, %d casts, %d types, %d collations, %d operator classes, %d range families",
		len(signatures), len(operators), len(casts), len(types), len(collations), len(operatorClasses), len(rangeFamilies))
	return writer.bytes(), nil
}

// writeTypeKinds counts the observed pg_type.typtype letters so the legend in
// the types table cannot drift from the catalog.
func writeTypeKinds(writer *mdWriter, types []builtin.TypeInfo) {
	counts := map[byte]int{}
	for _, info := range types {
		counts[info.Kind]++
	}
	kinds := make([]byte, 0, len(counts))
	for kind := range counts {
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	rows := make([][]string, 0, len(kinds))
	for _, kind := range kinds {
		rows = append(rows, []string{typeKindName(kind), fmt.Sprint(counts[kind])})
	}
	writer.table([]string{"pg_type.typtype", "Count"}, rows)
}

// argumentList renders the declared input arguments of one signature.
func argumentList(signature builtin.Signature) string {
	count := int(signature.ArgumentCount)
	if count > len(signature.Arguments) {
		count = len(signature.Arguments)
	}
	names := make([]string, 0, count+1)
	for index := range count {
		names = append(names, typeName(signature.Arguments[index]))
	}
	if signature.Variadic != 0 {
		names = append(names, "VARIADIC "+typeName(signature.Variadic))
	}
	return strings.Join(names, ", ")
}

// resultType renders the declared result type of one signature.
func resultType(signature builtin.Signature) string {
	name := typeName(signature.Result)
	if signature.ReturnsSet {
		return "setof " + name
	}
	return name
}

// typeName resolves a catalog type identity to its name, falling back to the
// numeric identity so an unresolved reference is visible rather than empty.
func typeName[T ~uint32](id T) string {
	value := uint32(id)
	if value == 0 {
		return "-"
	}
	if info, ok := builtin.LookupType(model.TypeID(value)); ok {
		return info.Name
	}
	return fmt.Sprintf("#%d", value)
}

// volatilityName maps pg_proc.provolatile to its documented word.
func volatilityName(volatility byte) string {
	switch volatility {
	case 'i':
		return "immutable"
	case 's':
		return "stable"
	case 'v':
		return "volatile"
	default:
		return fmt.Sprintf("unknown(%q)", rune(volatility))
	}
}

// kindName maps pg_proc.prokind to its documented word.
func kindName(kind byte) string {
	switch kind {
	case 'f':
		return "function"
	case 'a':
		return "aggregate"
	case 'w':
		return "window"
	case 'p':
		return "procedure"
	default:
		return fmt.Sprintf("unknown(%q)", rune(kind))
	}
}

// typeKindName maps pg_type.typtype to its documented word.
func typeKindName(kind byte) string {
	switch kind {
	case 'b':
		return "base"
	case 'c':
		return "composite"
	case 'd':
		return "domain"
	case 'e':
		return "enum"
	case 'm':
		return "multirange"
	case 'p':
		return "pseudo"
	case 'r':
		return "range"
	default:
		return fmt.Sprintf("unknown(%q)", rune(kind))
	}
}

// castContextName maps pg_cast.castcontext to its documented word.
func castContextName(context byte) string {
	switch context {
	case 'i':
		return "implicit"
	case 'a':
		return "assignment"
	case 'e':
		return "explicit"
	default:
		return fmt.Sprintf("unknown(%q)", rune(context))
	}
}

// castMethodName maps pg_cast.castmethod to its documented word.
func castMethodName(method byte) string {
	switch method {
	case 'f':
		return "function"
	case 'b':
		return "binary"
	case 'i':
		return "inout"
	default:
		return fmt.Sprintf("unknown(%q)", rune(method))
	}
}

// collationProviderName maps pg_collation.collprovider to its documented word.
func collationProviderName(provider byte) string {
	switch provider {
	case 'd':
		return "default"
	case 'b':
		return "builtin"
	case 'c':
		return "libc"
	case 'i':
		return "icu"
	default:
		return fmt.Sprintf("unknown(%q)", rune(provider))
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// collect drains a catalog iterator into a slice.
func collect[T any](sequence func(yield func(T) bool)) []T {
	var values []T
	for value := range sequence {
		values = append(values, value)
	}
	return values
}
