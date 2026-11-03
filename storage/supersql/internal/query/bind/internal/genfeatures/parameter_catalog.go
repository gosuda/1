package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type parameterDeclaration struct {
	id              uint32
	count           int
	names, defaults []string
	allTypes        []uint32
	allModes        []byte
	allNames        []string
	allTypesPresent, allModesPresent, allNamesPresent bool
}

func generateParameterCatalog(profile manifest, manifestPath, output string) error {
	if output == "" {
		return nil
	}
	root := filepath.Join(filepath.Dir(manifestPath), profile.Source["catalog"])
	types, err := readCatalog(filepath.Join(root, "pg_type.dat"))
	if err != nil {
		return err
	}
	typeIDs := make(map[string]uint32)
	for _, entry := range types {
		id, err := strconv.ParseUint(entry["oid"], 10, 32)
		if err != nil {
			return err
		}
		typeIDs[entry["typname"]] = uint32(id)
		if text := entry["array_type_oid"]; text != "" {
			id, err := strconv.ParseUint(text, 10, 32)
			if err != nil {
				return err
			}
			typeIDs["_"+entry["typname"]] = uint32(id)
		}
	}
	functions, err := readCatalog(filepath.Join(root, "pg_proc.dat"))
	if err != nil {
		return err
	}
	declarations := make(map[string]*parameterDeclaration)
	for _, function := range functions {
		id, err := strconv.ParseUint(function["oid"], 10, 32)
		if err != nil {
			return err
		}
		arguments := strings.Fields(function["proargtypes"])
		ids := make([]uint32, len(arguments))
		for i, name := range arguments {
			ids[i] = typeIDs[name]
		}
		names, err := inputParameterNames(function, len(arguments))
		if err != nil {
			return err
		}
		allTypeNames, err := catalogParameterList(function["proallargtypes"])
		if err != nil {
			return err
		}
		allModeNames, err := catalogParameterList(function["proargmodes"])
		if err != nil {
			return err
		}
		allNames, err := catalogParameterList(function["proargnames"])
		if err != nil {
			return err
		}
		allTypesPresent := len(allTypeNames) != 0
		allModesPresent := len(allModeNames) != 0
		allNamesPresent := len(allNames) != 0
		allCount := len(arguments)
		if allTypesPresent {
			allCount = len(allTypeNames)
		}
		if allCount > 100 || allTypesPresent && !allModesPresent ||
			allModesPresent && len(allModeNames) != allCount ||
			allNamesPresent && len(allNames) != allCount {
			return fmt.Errorf("%w: all-argument metadata for %s", errInvalidManifest, function["proname"])
		}
		effectiveTypeNames := arguments
		if allTypesPresent {
			effectiveTypeNames = allTypeNames
		}
		allTypes := make([]uint32, allCount)
		allModes := make([]byte, allCount)
		for i, name := range effectiveTypeNames {
			allTypes[i] = typeIDs[name]
			if allTypes[i] == 0 {
				return fmt.Errorf("%w: all-argument type for %s", errInvalidManifest, function["proname"])
			}
			allModes[i] = 'i'
		}
		inputModes := 0
		if allModesPresent {
			for i, name := range allModeNames {
				if len(name) != 1 || !strings.ContainsRune("iobvt", rune(name[0])) {
					return fmt.Errorf("%w: all-argument mode for %s", errInvalidManifest, function["proname"])
				}
				allModes[i] = name[0]
				if name == "i" || name == "b" || name == "v" {
					inputModes++
				}
			}
			if inputModes != len(arguments) {
				return fmt.Errorf("%w: input argument modes for %s", errInvalidManifest, function["proname"])
			}
		}
		normalizedNames := make([]string, allCount)
		if allNamesPresent {
			copy(normalizedNames, allNames)
		}
		if !allTypesPresent && !allModesPresent && !allNamesPresent {
			allTypes, allModes, normalizedNames = nil, nil, nil
		}
		declarations[parameterKey(function["proname"], ids)] = &parameterDeclaration{
			id: uint32(id), count: len(arguments), names: names,
			allTypes: allTypes, allModes: allModes, allNames: normalizedNames,
			allTypesPresent: allTypesPresent, allModesPresent: allModesPresent, allNamesPresent: allNamesPresent,
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "system_functions.sql"))
	if err != nil {
		return err
	}
	tree, err := zpgparse.Parse(string(data))
	if err != nil {
		return fmt.Errorf("parse bootstrap function declarations: %w", err)
	}
	for _, statement := range tree.Stmts {
		function := statement.Stmt.GetCreateFunctionStmt()
		if function == nil {
			continue
		}
		if err := applyBootstrapParameters(function, typeIDs, declarations); err != nil {
			return err
		}
	}
	entries := make([]*parameterDeclaration, 0, len(declarations))
	for _, entry := range declarations {
		if len(entry.names) != 0 || len(entry.defaults) != 0 || len(entry.allTypes) != 0 {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b *parameterDeclaration) int {
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	var code bytes.Buffer
	code.WriteString("// Code generated from PostgreSQL 18.4 pg_proc.dat and system_functions.sql. DO NOT EDIT.\n" + provenance + "package builtin\n\nvar functionParameterMetadata = [...]functionParameterInfo{\n")
	names := []string{""}
	var defaults []string
	for _, entry := range entries {
		nameStart := 0
		if len(entry.names) != 0 {
			nameStart = len(names)
			names = append(names, entry.names...)
		}
		fmt.Fprintf(&code, "{ID:%d,Names:%d,Defaults:%d,Count:%d,DefaultCount:%d", entry.id, nameStart, len(defaults), entry.count, len(entry.defaults))
		if len(entry.allTypes) != 0 {
			code.WriteString(",AllTypes:[MaxCallArguments]uint32{")
			for _, id := range entry.allTypes {
				fmt.Fprintf(&code, "%d,", id)
			}
			code.WriteString("},AllModes:[MaxCallArguments]byte{")
			for _, mode := range entry.allModes {
				fmt.Fprintf(&code, "%s,", strconv.QuoteRune(rune(mode)))
			}
			code.WriteString("},AllNames:[MaxCallArguments]string{")
			for _, name := range entry.allNames {
				fmt.Fprintf(&code, "%q,", name)
			}
			fmt.Fprintf(&code, "},AllCount:%d,AllTypesPresent:%t,AllModesPresent:%t,AllNamesPresent:%t", len(entry.allTypes), entry.allTypesPresent, entry.allModesPresent, entry.allNamesPresent)
		}
		code.WriteString("},\n")
		defaults = append(defaults, entry.defaults...)
	}
	code.WriteString("}\nvar functionParameterNames = [...]string{\n")
	for _, name := range names {
		fmt.Fprintf(&code, "%q,\n", name)
	}
	code.WriteString("}\nvar functionParameterDefaults = [...]string{\n")
	for _, value := range defaults {
		fmt.Fprintf(&code, "%q,\n", value)
	}
	code.WriteString("}\n")
	formatted, err := format.Source(code.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(output, formatted, 0644)
}

func parameterKey(name string, arguments []uint32) string {
	var key strings.Builder
	key.WriteString(name)
	for _, id := range arguments {
		key.WriteByte('/')
		key.WriteString(strconv.FormatUint(uint64(id), 10))
	}
	return key.String()
}

func catalogParameterList(text string) ([]string, error) {
	if text == "" || text == "{}" {
		return nil, nil
	}
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return nil, fmt.Errorf("%w: malformed parameter array", errInvalidManifest)
	}
	reader := csv.NewReader(strings.NewReader(text[1 : len(text)-1]))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	values, err := reader.Read()
	if err != nil {
		return nil, err
	}
	if _, err = reader.Read(); err != io.EOF {
		return nil, fmt.Errorf("%w: multiline parameter array", errInvalidManifest)
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return values, nil
}

func inputParameterNames(function map[string]string, count int) ([]string, error) {
	names, err := catalogParameterList(function["proargnames"])
	if err != nil || len(names) == 0 {
		return names, err
	}
	modes, err := catalogParameterList(function["proargmodes"])
	if err != nil {
		return nil, err
	}
	if len(modes) != 0 {
		if len(modes) != len(names) {
			return nil, fmt.Errorf("%w: parameter mode/name count", errInvalidManifest)
		}
		selected := make([]string, 0, count)
		for i, mode := range modes {
			if mode == "i" || mode == "b" || mode == "v" {
				selected = append(selected, names[i])
			}
		}
		names = selected
	}
	if len(names) != count {
		return nil, fmt.Errorf("%w: input parameter names for %s", errInvalidManifest, function["proname"])
	}
	return names, nil
}

func applyBootstrapParameters(function *zpgparse.CreateFunctionStmt, typeIDs map[string]uint32, declarations map[string]*parameterDeclaration) error {
	name := ""
	for _, part := range function.Funcname {
		name = part.GetString_().GetSval()
	}
	var parameters []*zpgparse.FunctionParameter
	var arguments []uint32
	for _, node := range function.Parameters {
		parameter := node.GetFunctionParameter()
		if parameter == nil {
			return fmt.Errorf("%w: malformed bootstrap parameter", errInvalidManifest)
		}
		if parameter.Mode == zpgparse.FunctionParameterMode_FUNC_PARAM_OUT || parameter.Mode == zpgparse.FunctionParameterMode_FUNC_PARAM_TABLE {
			continue
		}
		id, err := bootstrapParameterType(parameter.ArgType, typeIDs)
		if err != nil {
			return err
		}
		parameters = append(parameters, parameter)
		arguments = append(arguments, id)
	}
	entry, ok := declarations[parameterKey(name, arguments)]
	if !ok {
		return fmt.Errorf("%w: bootstrap function identity %s%v", errInvalidManifest, name, arguments)
	}
	names := make([]string, len(parameters))
	var defaults []string
	named := false
	for i, parameter := range parameters {
		names[i] = parameter.Name
		named = named || parameter.Name != ""
		if parameter.Defexpr == nil {
			if len(defaults) != 0 {
				return fmt.Errorf("%w: non-trailing bootstrap default", errInvalidManifest)
			}
			continue
		}
		value, err := bootstrapDefaultSQL(parameter.Defexpr)
		if err != nil {
			return err
		}
		defaults = append(defaults, value)
	}
	if named {
		entry.names = names
	}
	entry.defaults = defaults
	return nil
}

func bootstrapParameterType(typ *zpgparse.TypeName, typeIDs map[string]uint32) (uint32, error) {
	if typ == nil || len(typ.Names) == 0 {
		return 0, fmt.Errorf("%w: absent bootstrap parameter type", errInvalidManifest)
	}
	name := typ.Names[len(typ.Names)-1].GetString_().GetSval()
	if len(typ.ArrayBounds) != 0 {
		name = "_" + name
	}
	id, ok := typeIDs[name]
	if !ok {
		return 0, fmt.Errorf("%w: unknown bootstrap parameter type %s", errInvalidManifest, name)
	}
	return id, nil
}

func bootstrapDefaultSQL(expression *zpgparse.Node) (string, error) {
	tree := &zpgparse.ParseResult{Version: 180004, Stmts: []*zpgparse.RawStmt{{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{Op: zpgparse.SetOperation_SETOP_NONE, TargetList: []*zpgparse.Node{zpgparse.MakeResTargetNodeWithVal(expression, -1)}}}}}}}
	sql, err := zpgparse.Deparse(tree)
	if err != nil {
		return "", err
	}
	value, ok := strings.CutPrefix(sql, "SELECT ")
	if !ok {
		return "", fmt.Errorf("%w: scalar bootstrap default", errInvalidManifest)
	}
	return value, nil
}
