package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type generatedAccessMethodOperator struct {
	oid, family, left, right, strategy, operator, method, sortFamily uint32
	purpose byte
}

type generatedAccessMethodProcedure struct {
	oid, family, left, right, number, procedure uint32
}

func readAccessMethodCatalog(directory string, types, operators map[string]uint32, functions []map[string]string) ([]generatedAccessMethodOperator, []generatedAccessMethodProcedure, error) {
	families, err := readOperatorFamilies(directory)
	if err != nil { return nil, nil, err }
	methods := map[string]uint32{"btree": 403, "hash": 405, "gist": 783, "gin": 2742, "spgist": 4000, "brin": 3580}
	amopRows, err := readCatalog(filepath.Join(directory, "pg_amop.dat"))
	if err != nil { return nil, nil, err }
	amopOIDs, err := assignCatalogOIDs(amopRows, "pg_amop")
	if err != nil { return nil, nil, err }
	amops := make([]generatedAccessMethodOperator, len(amopRows))
	for i, row := range amopRows {
		family, ok := families[row["amopfamily"]]
		if !ok { return nil, nil, fmt.Errorf("%w: unknown pg_amop family %q", errInvalidManifest, row["amopfamily"]) }
		left, err := referencedType(types, row["amoplefttype"])
		if err != nil { return nil, nil, err }
		right, err := referencedType(types, row["amoprighttype"])
		if err != nil { return nil, nil, err }
		strategy, err := strconv.ParseUint(row["amopstrategy"], 10, 16)
		if err != nil || strategy == 0 { return nil, nil, fmt.Errorf("%w: invalid pg_amop strategy", errInvalidManifest) }
		operator, ok := operators[row["amopopr"]]
		if !ok { return nil, nil, fmt.Errorf("%w: unknown pg_amop operator %q", errInvalidManifest, row["amopopr"]) }
		method, ok := methods[row["amopmethod"]]
		if !ok { return nil, nil, fmt.Errorf("%w: unknown pg_amop method %q", errInvalidManifest, row["amopmethod"]) }
		purpose := byte('s')
		if text := row["amoppurpose"]; text != "" {
			if len(text) != 1 { return nil, nil, errInvalidManifest }
			purpose = text[0]
		}
		var sortFamily uint32
		if reference := row["amopsortfamily"]; reference != "" && reference != "0" {
			sortFamily, ok = families[reference]
			if !ok { return nil, nil, fmt.Errorf("%w: unknown pg_amop sort family %q", errInvalidManifest, reference) }
		}
		amops[i] = generatedAccessMethodOperator{oid: amopOIDs[i], family: family, left: left, right: right, strategy: uint32(strategy), purpose: purpose, operator: operator, method: method, sortFamily: sortFamily}
	}
	amprocRows, err := readCatalog(filepath.Join(directory, "pg_amproc.dat"))
	if err != nil { return nil, nil, err }
	amprocOIDs, err := assignCatalogOIDs(amprocRows, "pg_amproc")
	if err != nil { return nil, nil, err }
	amprocs := make([]generatedAccessMethodProcedure, len(amprocRows))
	for i, row := range amprocRows {
		family, ok := families[row["amprocfamily"]]
		if !ok { return nil, nil, fmt.Errorf("%w: unknown pg_amproc family %q", errInvalidManifest, row["amprocfamily"]) }
		left, err := referencedType(types, row["amproclefttype"])
		if err != nil { return nil, nil, err }
		right, err := referencedType(types, row["amprocrighttype"])
		if err != nil { return nil, nil, err }
		number, err := strconv.ParseUint(row["amprocnum"], 10, 16)
		if err != nil || number == 0 { return nil, nil, fmt.Errorf("%w: invalid pg_amproc number", errInvalidManifest) }
		procedure, err := accessMethodProcedure(functions, row["amproc"])
		if err != nil { return nil, nil, err }
		amprocs[i] = generatedAccessMethodProcedure{oid: amprocOIDs[i], family: family, left: left, right: right, number: uint32(number), procedure: procedure}
	}
	slices.SortFunc(amops, func(a, b generatedAccessMethodOperator) int { return cmp.Compare(a.oid, b.oid) })
	slices.SortFunc(amprocs, func(a, b generatedAccessMethodProcedure) int { return cmp.Compare(a.oid, b.oid) })
	return amops, amprocs, nil
}

func accessMethodProcedure(functions []map[string]string, reference string) (uint32, error) {
	if strings.Contains(reference, "(") {
		name, arguments, err := routineReference(reference)
		if err != nil { return 0, err }
		return functionFor(functions, name, arguments, "")
	}
	var found uint32
	for _, function := range functions {
		if function["proname"] != reference { continue }
		oid, err := catalogOID(function["oid"])
		if err != nil { return 0, err }
		if found != 0 { return 0, fmt.Errorf("%w: ambiguous pg_amproc function %q", errInvalidManifest, reference) }
		found = oid
	}
	if found == 0 { return 0, fmt.Errorf("%w: unknown pg_amproc function %q", errInvalidManifest, reference) }
	return found, nil
}
