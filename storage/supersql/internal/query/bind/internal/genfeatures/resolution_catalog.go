package main

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type resolutionType struct {
	id                                             uint32
	name                                           string
	category, kind                                 string
	preferred                                      bool
	delimiter                                      string
	element, array, base, collation                 uint32
	input, output, receive, send                    uint32
	modifierInput, modifierOutput, subscript, analyze uint32
}
type resolutionOperator struct {
	id                                                 uint32
	name                                               string
	left, right, result, function, commutator, negator, restriction, join uint32
	hash, merge                                        bool
	reference                                          map[string]string
}
type resolutionCast struct {
	oid, source, target, function uint32
	context, method          string
}
type resolutionCollation struct {
	id            uint32
	name          string
	provider      byte
	deterministic bool
	encoding      int32
}

func catalogOID(text string) (uint32, error) {
	if text == "" || text == "0" || text == "-" {
		return 0, nil
	}
	id, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: catalog OID %q: %w", errInvalidManifest, text, err)
	}
	return uint32(id), nil
}
func referencedType(ids map[string]uint32, name string) (uint32, error) {
	if name == "" || name == "0" || name == "-" {
		return 0, nil
	}
	id, ok := ids[name]
	if !ok {
		return 0, fmt.Errorf("%w: unknown catalog type %q", errInvalidManifest, name)
	}
	return id, nil
}
func routineReference(text string) (string, []string, error) {
	name, rest, found := strings.Cut(text, "(")
	if !found || !strings.HasSuffix(rest, ")") {
		return "", nil, fmt.Errorf("%w: routine reference %q", errInvalidManifest, text)
	}
	arguments := strings.TrimSuffix(rest, ")")
	if arguments == "" {
		return name, nil, nil
	}
	return name, strings.Split(arguments, ","), nil
}
func functionFor(functions []map[string]string, name string, args []string, result string) (uint32, error) {
	if strings.Contains(name, "(") {
		resolved, inputs, err := routineReference(name)
		if err != nil {
			return 0, err
		}
		if !slices.Equal(inputs, args) {
			return 0, fmt.Errorf("%w: operator function argument mismatch at %s", errInvalidManifest, name)
		}
		name = resolved
	}
	var found uint32
	for _, entry := range functions {
		if entry["proname"] != name || !slices.Equal(strings.Fields(entry["proargtypes"]), args) {
			continue
		}
		if result != "" && entry["prorettype"] != result {
			continue
		}
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return 0, err
		}
		if found != 0 {
			return 0, fmt.Errorf("%w: ambiguous function %s(%s)", errInvalidManifest, name, strings.Join(args, ","))
		}
		found = id
	}
	if found == 0 {
		return 0, fmt.Errorf("%w: unresolved function %s(%s)", errInvalidManifest, name, strings.Join(args, ","))
	}
	return found, nil
}
func generateResolutionCatalog(profile manifest, path, output, opclassOutput, rowComparisonOutput string) error {
	directory := filepath.Join(filepath.Dir(path), profile.Source["catalog"])
	records, err := readCatalog(filepath.Join(directory, "pg_type.dat"))
	if err != nil {
		return err
	}
	typeIDs := make(map[string]uint32, len(records)*2)
	var types []resolutionType
	for _, entry := range records {
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return err
		}
		array, err := catalogOID(entry["array_type_oid"])
		if err != nil {
			return err
		}
		name := entry["typname"]
		if id == 0 || name == "" || typeIDs[name] != 0 {
			return errInvalidManifest
		}
		typeIDs[name] = id
		if array != 0 {
			typeIDs["_"+name] = array
		}
		delimiter := cmp.Or(entry["typdelim"], ",")
		if len(delimiter) != 1 {
			return errInvalidManifest
		}
		types = append(types, resolutionType{id: id, name: name, category: cmp.Or(entry["typcategory"], "U"), kind: cmp.Or(entry["typtype"], "b"), preferred: entry["typispreferred"] == "t", array: array, delimiter: delimiter})
		if len(types[len(types)-1].category) != 1 || len(types[len(types)-1].kind) != 1 {
			return errInvalidManifest
		}
		if array != 0 {
			types = append(types, resolutionType{id: array, name: "_" + name, category: "A", kind: "b", element: id, delimiter: delimiter})
		}
	}
	byType := make(map[uint32]int, len(types))
	for i, info := range types {
		if _, exists := byType[info.id]; exists {
			return errInvalidManifest
		}
		byType[info.id] = i
	}
	for _, entry := range records {
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return err
		}
		info := &types[byType[id]]
		if entry["typarray"] != "" {
			array, err := referencedType(typeIDs, entry["typarray"])
			if err != nil {
				return err
			}
			if info.array != 0 && info.array != array {
				return errInvalidManifest
			}
			info.array = array
		}
		info.element, err = referencedType(typeIDs, entry["typelem"])
		if err != nil {
			return err
		}
		info.base, err = referencedType(typeIDs, entry["typbasetype"])
		if err != nil {
			return err
		}
		switch entry["typcollation"] {
		case "", "0":
		case "default":
			info.collation = 100
		case "C":
			info.collation = 950
		case "POSIX":
			info.collation = 951
		default:
			return fmt.Errorf("%w: catalog collation %q", errInvalidManifest, entry["typcollation"])
		}
		if info.array != 0 {
			types[byType[info.array]].collation = info.collation
		}
	}
	functions, err := readCatalog(filepath.Join(directory, "pg_proc.dat"))
	if err != nil {
		return err
	}
	var arrayFunctions resolutionType
	for _, function := range []struct {
		name        string
		destination *uint32
	}{
		{"array_in", &arrayFunctions.input},
		{"array_out", &arrayFunctions.output},
		{"array_recv", &arrayFunctions.receive},
		{"array_send", &arrayFunctions.send},
		{"array_subscript_handler", &arrayFunctions.subscript},
		{"array_typanalyze", &arrayFunctions.analyze},
	} {
		*function.destination, err = castFunction(functions, function.name)
		if err != nil {
			return err
		}
	}
	for _, entry := range records {
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return err
		}
		info := &types[byType[id]]
		for _, function := range []struct {
			reference   string
			destination *uint32
		}{
			{entry["typinput"], &info.input},
			{entry["typoutput"], &info.output},
			{entry["typreceive"], &info.receive},
			{entry["typsend"], &info.send},
			{entry["typmodin"], &info.modifierInput},
			{entry["typmodout"], &info.modifierOutput},
			{entry["typsubscript"], &info.subscript},
			{entry["typanalyze"], &info.analyze},
		} {
			*function.destination, err = optionalCatalogFunction(functions, function.reference)
			if err != nil {
				return err
			}
		}
		if info.input == 0 || info.output == 0 {
			return fmt.Errorf("%w: type %s has no text I/O functions", errInvalidManifest, info.name)
		}
		if info.array != 0 {
			array := &types[byType[info.array]]
			array.input, array.output = arrayFunctions.input, arrayFunctions.output
			array.receive, array.send = arrayFunctions.receive, arrayFunctions.send
			array.subscript, array.analyze = arrayFunctions.subscript, arrayFunctions.analyze
			array.modifierInput, array.modifierOutput = info.modifierInput, info.modifierOutput
		}
	}
	operatorRows, err := readCatalog(filepath.Join(directory, "pg_operator.dat"))
	if err != nil {
		return err
	}
	operators := make([]resolutionOperator, 0, len(operatorRows))
	operatorIDs := make(map[string]uint32, len(operatorRows))
	for _, entry := range operatorRows {
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return err
		}
		left, err := referencedType(typeIDs, entry["oprleft"])
		if err != nil {
			return err
		}
		right, err := referencedType(typeIDs, entry["oprright"])
		if err != nil {
			return err
		}
		result, err := referencedType(typeIDs, entry["oprresult"])
		if err != nil {
			return err
		}
		var args []string
		if left != 0 {
			args = append(args, entry["oprleft"])
		}
		if right != 0 {
			args = append(args, entry["oprright"])
		}
		function, err := functionFor(functions, entry["oprcode"], args, entry["oprresult"])
		if err != nil {
			return err
		}
		var restriction, join uint32
		for field, destination := range map[string]*uint32{"oprrest": &restriction, "oprjoin": &join} {
			if reference := entry[field]; reference != "" && reference != "-" {
				*destination, err = castFunction(functions, reference)
				if err != nil {
					return err
				}
			}
		}
		reference := entry["oprname"] + "(" + cmp.Or(entry["oprleft"], "0") + "," + cmp.Or(entry["oprright"], "0") + ")"
		if id == 0 || operatorIDs[reference] != 0 {
			return errInvalidManifest
		}
		operatorIDs[reference] = id
		operators = append(operators, resolutionOperator{id: id, name: entry["oprname"], left: left, right: right, result: result, function: function, restriction: restriction, join: join, hash: entry["oprcanhash"] == "t", merge: entry["oprcanmerge"] == "t", reference: entry})
	}
	for i := range operators {
		operator := &operators[i]
		for field, destination := range map[string]*uint32{"oprcom": &operator.commutator, "oprnegate": &operator.negator} {
			reference := operator.reference[field]
			if reference == "" || reference == "0" {
				continue
			}
			id, found := operatorIDs[reference]
			if !found {
				return fmt.Errorf("%w: unresolved operator %q", errInvalidManifest, reference)
			}
			*destination = id
		}
	}
	castRows, err := readCatalog(filepath.Join(directory, "pg_cast.dat"))
	if err != nil {
		return err
	}
	castOIDs, err := assignCatalogOIDs(castRows, "pg_cast")
	if err != nil {
		return err
	}
	casts := make([]resolutionCast, 0, len(castRows))
	for i, entry := range castRows {
		source, err := referencedType(typeIDs, entry["castsource"])
		if err != nil {
			return err
		}
		target, err := referencedType(typeIDs, entry["casttarget"])
		if err != nil {
			return err
		}
		var function uint32
		if entry["castfunc"] != "0" && entry["castfunc"] != "" {
			function, err = castFunction(functions, entry["castfunc"])
			if err != nil {
				return err
			}
		}
		casts = append(casts, resolutionCast{oid: castOIDs[i], source: source, target: target, function: function, context: entry["castcontext"], method: entry["castmethod"]})
	}
	collations, err := readResolutionCollations(filepath.Join(directory, "pg_collation.dat"))
	if err != nil {
		return err
	}
	accessMethodOperators, accessMethodProcedures, err := readAccessMethodCatalog(directory, typeIDs, operatorIDs, functions)
	if err != nil {
		return err
	}
	slices.SortFunc(types, func(a, b resolutionType) int { return cmp.Compare(a.id, b.id) })
	slices.SortFunc(operators, func(a, b resolutionOperator) int { return cmp.Compare(a.id, b.id) })
	slices.SortFunc(casts, func(a, b resolutionCast) int {
		if order := cmp.Compare(a.source, b.source); order != 0 {
			return order
		}
		return cmp.Compare(a.target, b.target)
	})
	if len(types) > math.MaxUint16 || len(operators) > math.MaxUint16 {
		return errInvalidManifest
	}
	var generated bytes.Buffer
	generated.WriteString("// Code generated by genfeatures from PostgreSQL REL_18_4 catalogs. DO NOT EDIT.\n" + provenance + "package builtin\n\nvar typeCatalog = [...]TypeInfo{\n")
	for _, info := range types {
		fmt.Fprintf(&generated, "{ID:%d,Name:%q,Category:%q,Kind:%q,Preferred:%t,Element:%d,Array:%d,Base:%d,Collation:%d,Input:%d,Output:%d,Receive:%d,Send:%d,ModifierInput:%d,ModifierOutput:%d,Subscript:%d,Analyze:%d,Delimiter:%q},\n", info.id, info.name, info.category[0], info.kind[0], info.preferred, info.element, info.array, info.base, info.collation, info.input, info.output, info.receive, info.send, info.modifierInput, info.modifierOutput, info.subscript, info.analyze, info.delimiter[0])
	}
	generated.WriteString("}\nvar operatorCatalog = [...]Operator{\n")
	for _, operator := range operators {
		fmt.Fprintf(&generated, "{ID:%d,Name:%q,Left:%d,Right:%d,Result:%d,Function:%d,Commutator:%d,Negator:%d,Restriction:%d,Join:%d,HashJoinable:%t,MergeJoinable:%t},\n", operator.id, operator.name, operator.left, operator.right, operator.result, operator.function, operator.commutator, operator.negator, operator.restriction, operator.join, operator.hash, operator.merge)
	}
	generated.WriteString("}\nvar castCatalog = [...]CastInfo{\n")
	for _, cast := range casts {
		if len(cast.context) != 1 || len(cast.method) != 1 {
			return errInvalidManifest
		}
		fmt.Fprintf(&generated, "{OID:%d,Source:%d,Target:%d,Function:%d,Context:%q,Method:%q},\n", cast.oid, cast.source, cast.target, cast.function, cast.context[0], cast.method[0])
	}
	generated.WriteString("}\nvar typeNames = [...]uint16{\n")
	typeOrder := make([]uint16, len(types))
	for i := range typeOrder {
		typeOrder[i] = uint16(i)
	}
	slices.SortFunc(typeOrder, func(a, b uint16) int {
		if order := strings.Compare(types[a].name, types[b].name); order != 0 {
			return order
		}
		return cmp.Compare(types[a].id, types[b].id)
	})
	for _, index := range typeOrder {
		fmt.Fprintf(&generated, "%d,\n", index)
	}
	generated.WriteString("}\nvar operatorNames = [...]uint16{\n")
	operatorOrder := make([]uint16, len(operators))
	for i := range operatorOrder {
		operatorOrder[i] = uint16(i)
	}
	slices.SortFunc(operatorOrder, func(a, b uint16) int {
		if order := strings.Compare(operators[a].name, operators[b].name); order != 0 {
			return order
		}
		return cmp.Compare(operators[a].id, operators[b].id)
	})
	for _, index := range operatorOrder {
		fmt.Fprintf(&generated, "%d,\n", index)
	}
	generated.WriteString("}\nvar collationCatalog = [...]CollationInfo{\n")
	for _, info := range collations {
		fmt.Fprintf(&generated, "{ID:%d,Name:%q,Provider:%q,Deterministic:%t,Encoding:%d},\n", info.id, info.name, info.provider, info.deterministic, info.encoding)
	}
	generated.WriteString("}\n")
	generated.WriteString("var accessMethodOperatorCatalogData = [...]AccessMethodOperatorInfo{\n")
	for _, info := range accessMethodOperators {
		fmt.Fprintf(&generated, "{OID:%d,Family:%d,Left:%d,Right:%d,Strategy:%d,Purpose:%q,Operator:%d,Method:%d,SortFamily:%d},\n", info.oid, info.family, info.left, info.right, info.strategy, info.purpose, info.operator, info.method, info.sortFamily)
	}
	generated.WriteString("}\nvar accessMethodProcedureCatalogData = [...]AccessMethodProcedureInfo{\n")
	for _, info := range accessMethodProcedures {
		fmt.Fprintf(&generated, "{OID:%d,Family:%d,Left:%d,Right:%d,Number:%d,Procedure:%d},\n", info.oid, info.family, info.left, info.right, info.number, info.procedure)
	}
	generated.WriteString("}\nfunc init() { registerAccessMethodCatalog(accessMethodOperatorCatalogData[:], accessMethodProcedureCatalogData[:]) }\n")
	formatted, err := format.Source(generated.Bytes())
	if err != nil {
		return err
	}
	if opclassOutput != "" {
		if err := generateOperatorClassCatalog(directory, typeIDs, opclassOutput); err != nil {
			return err
		}
	}
	if rowComparisonOutput != "" {
		if err := generateRowComparisonCatalog(directory, typeIDs, rowComparisonOutput); err != nil {
			return err
		}
	}
	if output == "" {
		return nil
	}
	return os.WriteFile(output, formatted, 0644)
}

func castFunction(functions []map[string]string, reference string) (uint32, error) {
	if strings.Contains(reference, "(") {
		name, args, err := routineReference(reference)
		if err != nil {
			return 0, err
		}
		return functionFor(functions, name, args, "")
	}
	var found uint32
	for _, entry := range functions {
		if entry["proname"] != reference {
			continue
		}
		if found != 0 {
			return 0, fmt.Errorf("%w: ambiguous cast function %q", errInvalidManifest, reference)
		}
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return 0, err
		}
		found = id
	}
	if found == 0 {
		return 0, fmt.Errorf("%w: unresolved cast function %q", errInvalidManifest, reference)
	}
	return found, nil
}

func optionalCatalogFunction(functions []map[string]string, reference string) (uint32, error) {
	if reference == "" || reference == "-" || reference == "0" || reference == "_null_" {
		return 0, nil
	}
	return castFunction(functions, reference)
}

func readResolutionCollations(path string) ([]resolutionCollation, error) {
	records, err := readCatalog(path)
	if err != nil {
		return nil, err
	}
	collations := make([]resolutionCollation, 0, len(records))
	ids := make(map[uint32]bool, len(records))
	names := make(map[string]bool, len(records))
	for _, entry := range records {
		id, err := catalogOID(entry["oid"])
		if err != nil {
			return nil, err
		}
		name, provider := entry["collname"], entry["collprovider"]
		if id == 0 || name == "" || ids[id] || names[name] || cmp.Or(entry["collnamespace"], "pg_catalog") != "pg_catalog" {
			return nil, fmt.Errorf("%w: collation identity %q (%d)", errInvalidManifest, name, id)
		}
		if len(provider) != 1 || !strings.ContainsAny(provider, "dbic") {
			return nil, fmt.Errorf("%w: collation provider %q", errInvalidManifest, provider)
		}
		// pg_collation.h supplies the deterministic default omitted by .dat rows.
		deterministic := cmp.Or(entry["collisdeterministic"], "t")
		if deterministic != "t" && deterministic != "f" {
			return nil, fmt.Errorf("%w: collation deterministic flag %q", errInvalidManifest, deterministic)
		}
		encoding, err := strconv.ParseInt(entry["collencoding"], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: collation encoding %q: %w", errInvalidManifest, entry["collencoding"], err)
		}
		ids[id], names[name] = true, true
		collations = append(collations, resolutionCollation{id: id, name: name, provider: provider[0], deterministic: deterministic == "t", encoding: int32(encoding)})
	}
	slices.SortFunc(collations, func(a, b resolutionCollation) int { return cmp.Compare(a.id, b.id) })
	return collations, nil
}
