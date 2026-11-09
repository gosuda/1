package main

import (
	"bytes"
	"cmp"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type rowComparisonKey struct {
	family, left, right uint32
}

type rowComparisonRow struct {
	rowComparisonKey
	operator, function uint32
	strategy           uint8
}

type rowComparisonOperator struct {
	id, left, right, result uint32
	record                  map[string]string
}

func readRowComparisonCatalog(directory string, types map[string]uint32) ([]rowComparisonRow, error) {
	families, err := readOperatorFamilies(directory)
	if err != nil {
		return nil, err
	}
	functions, err := readIdentifiedCatalog(directory, "pg_proc")
	if err != nil {
		return nil, err
	}
	functionIDs := make(map[uint32]map[string]string, len(functions))
	for _, record := range functions {
		id, err := catalogOID(record["oid"])
		if err != nil {
			return nil, err
		}
		functionIDs[id] = record
	}
	operators, operatorRows, err := readRowComparisonOperators(directory, types)
	if err != nil {
		return nil, err
	}
	procedures, err := readCatalog(filepath.Join(directory, "pg_amproc.dat"))
	if err != nil {
		return nil, err
	}
	if _, err := assignCatalogOIDs(procedures, "pg_amproc"); err != nil {
		return nil, err
	}
	support := make(map[rowComparisonKey]uint32)
	procedureKeys := make(map[struct {
		rowComparisonKey
		number uint16
	}]bool)
	for _, record := range procedures {
		if !strings.HasPrefix(record["amprocfamily"], "btree/") {
			continue
		}
		key, err := rowComparisonMemberKey(families, types, record["amprocfamily"], record["amproclefttype"], record["amprocrighttype"])
		if err != nil {
			return nil, err
		}
		number, err := strconv.ParseUint(record["amprocnum"], 10, 16)
		if err != nil || number == 0 {
			return nil, fmt.Errorf("%w: B-tree support number %q", errInvalidManifest, record["amprocnum"])
		}
		procedureKey := struct {
			rowComparisonKey
			number uint16
		}{key, uint16(number)}
		if procedureKeys[procedureKey] {
			return nil, fmt.Errorf("%w: duplicate B-tree support procedure %d for %+v", errInvalidManifest, number, key)
		}
		procedureKeys[procedureKey] = true
		if number != 1 {
			if _, err := castFunction(functions, record["amproc"]); err != nil {
				return nil, err
			}
			continue
		}
		function, err := rowComparisonFunction(functions, functionIDs, record["amproc"], []string{record["amproclefttype"], record["amprocrighttype"]}, "int4")
		if err != nil {
			return nil, err
		}
		support[key] = function
	}
	members, err := readCatalog(filepath.Join(directory, "pg_amop.dat"))
	if err != nil {
		return nil, err
	}
	if _, err := assignCatalogOIDs(members, "pg_amop"); err != nil {
		return nil, err
	}
	byOperator := make(map[uint32][]rowComparisonRow)
	strategies := make(map[struct {
		rowComparisonKey
		strategy uint8
	}]bool)
	familyOperators := make(map[[2]uint32]bool)
	var rows []rowComparisonRow
	for _, record := range members {
		isBtree := strings.HasPrefix(record["amopfamily"], "btree/")
		if isBtree != (record["amopmethod"] == "btree") {
			return nil, fmt.Errorf("%w: mismatched B-tree member method %q", errInvalidManifest, record["amopfamily"])
		}
		if !isBtree {
			continue
		}
		key, err := rowComparisonMemberKey(families, types, record["amopfamily"], record["amoplefttype"], record["amoprighttype"])
		if err != nil {
			return nil, err
		}
		strategy, err := strconv.ParseUint(record["amopstrategy"], 10, 8)
		if err != nil || strategy < 1 || strategy > 5 {
			return nil, fmt.Errorf("%w: B-tree strategy %q", errInvalidManifest, record["amopstrategy"])
		}
		if cmp.Or(record["amoppurpose"], "s") != "s" || cmp.Or(record["amopsortfamily"], "0") != "0" {
			return nil, fmt.Errorf("%w: B-tree member must be a search operator", errInvalidManifest)
		}
		operator, exists := operators[record["amopopr"]]
		if !exists || operator.left != key.left || operator.right != key.right || operator.result != types["bool"] {
			return nil, fmt.Errorf("%w: unresolved or mistyped B-tree operator %q", errInvalidManifest, record["amopopr"])
		}
		if _, err := rowComparisonFunction(functions, functionIDs, operator.record["oprcode"], []string{record["amoplefttype"], record["amoprighttype"]}, "bool"); err != nil {
			return nil, err
		}
		strategyKey := struct {
			rowComparisonKey
			strategy uint8
		}{key, uint8(strategy)}
		familyOperator := [2]uint32{key.family, operator.id}
		if strategies[strategyKey] || familyOperators[familyOperator] {
			return nil, fmt.Errorf("%w: duplicate B-tree member %q in family %d", errInvalidManifest, record["amopopr"], key.family)
		}
		strategies[strategyKey], familyOperators[familyOperator] = true, true
		row := rowComparisonRow{rowComparisonKey: key, operator: operator.id, function: support[key], strategy: uint8(strategy)}
		rows = append(rows, row)
		byOperator[operator.id] = append(byOperator[operator.id], row)
	}
	for _, operator := range operatorRows {
		if len(byOperator[operator.id]) != 0 {
			continue
		}
		negator := operators[operator.record["oprnegate"]]
		for _, member := range byOperator[negator.id] {
			if member.strategy != 3 {
				continue
			}
			if _, err := rowComparisonFunction(functions, functionIDs, operator.record["oprcode"], []string{operator.record["oprleft"], operator.record["oprright"]}, "bool"); err != nil {
				return nil, err
			}
			member.operator, member.strategy = operator.id, 6
			rows = append(rows, member)
		}
	}
	slices.SortFunc(rows, func(a, b rowComparisonRow) int {
		return cmp.Or(cmp.Compare(a.operator, b.operator), cmp.Compare(a.strategy, b.strategy), cmp.Compare(a.family, b.family), cmp.Compare(a.left, b.left), cmp.Compare(a.right, b.right))
	})
	return rows, nil
}

func readIdentifiedCatalog(directory, name string) ([]map[string]string, error) {
	records, err := readCatalog(filepath.Join(directory, name+".dat"))
	if err != nil {
		return nil, err
	}
	seen := make(map[uint32]bool, len(records))
	for i, record := range records {
		id, err := catalogOID(record["oid"])
		if err != nil {
			return nil, err
		}
		if id == 0 || seen[id] {
			return nil, fmt.Errorf("%w: %s row %d has absent or duplicate OID %d", errInvalidManifest, name, i+1, id)
		}
		seen[id] = true
	}
	return records, nil
}

func readRowComparisonOperators(directory string, types map[string]uint32) (map[string]rowComparisonOperator, []rowComparisonOperator, error) {
	records, err := readIdentifiedCatalog(directory, "pg_operator")
	if err != nil {
		return nil, nil, err
	}
	operators := make(map[string]rowComparisonOperator, len(records))
	rows := make([]rowComparisonOperator, 0, len(records))
	for _, record := range records {
		id, err := catalogOID(record["oid"])
		if err != nil {
			return nil, nil, err
		}
		left, err := referencedType(types, record["oprleft"])
		if err != nil {
			return nil, nil, err
		}
		right, err := referencedType(types, record["oprright"])
		if err != nil {
			return nil, nil, err
		}
		result, err := referencedType(types, record["oprresult"])
		if err != nil {
			return nil, nil, err
		}
		reference := record["oprname"] + "(" + cmp.Or(record["oprleft"], "0") + "," + cmp.Or(record["oprright"], "0") + ")"
		if record["oprname"] == "" || result == 0 || operators[reference].id != 0 || cmp.Or(record["oprnamespace"], "pg_catalog") != "pg_catalog" {
			return nil, nil, fmt.Errorf("%w: invalid or duplicate operator %q", errInvalidManifest, reference)
		}
		operator := rowComparisonOperator{id: id, left: left, right: right, result: result, record: record}
		operators[reference] = operator
		rows = append(rows, operator)
	}
	for _, operator := range rows {
		reference := operator.record["oprnegate"]
		if reference == "" || reference == "0" || reference == "-" {
			continue
		}
		negator, exists := operators[reference]
		if !exists || negator.left != operator.left || negator.right != operator.right || negator.result != types["bool"] || operator.result != types["bool"] {
			return nil, nil, fmt.Errorf("%w: unresolved or mistyped negator %q", errInvalidManifest, reference)
		}
	}
	return operators, rows, nil
}

func rowComparisonMemberKey(families, types map[string]uint32, family, left, right string) (rowComparisonKey, error) {
	familyID, exists := families[family]
	if !exists {
		return rowComparisonKey{}, fmt.Errorf("%w: unknown B-tree family %q", errInvalidManifest, family)
	}
	leftID, err := referencedType(types, left)
	if err != nil {
		return rowComparisonKey{}, err
	}
	rightID, err := referencedType(types, right)
	if err != nil {
		return rowComparisonKey{}, err
	}
	if leftID == 0 || rightID == 0 {
		return rowComparisonKey{}, fmt.Errorf("%w: B-tree member requires two input types", errInvalidManifest)
	}
	return rowComparisonKey{familyID, leftID, rightID}, nil
}

func rowComparisonFunction(functions []map[string]string, byID map[uint32]map[string]string, reference string, args []string, result string) (uint32, error) {
	id, err := functionFor(functions, reference, args, result)
	if err != nil {
		return 0, err
	}
	record := byID[id]
	if cmp.Or(record["prokind"], "f") != "f" || cmp.Or(record["proretset"], "f") != "f" || cmp.Or(record["provariadic"], "0") != "0" || cmp.Or(record["pronamespace"], "pg_catalog") != "pg_catalog" {
		return 0, fmt.Errorf("%w: comparison function %d must be a scalar pg_catalog function", errInvalidManifest, id)
	}
	return id, nil
}

func generateRowComparisonCatalog(directory string, types map[string]uint32, output string) error {
	rows, err := readRowComparisonCatalog(directory, types)
	if err != nil {
		return err
	}
	var generated bytes.Buffer
	generated.WriteString("// Code generated by genfeatures from PostgreSQL REL_18_4 catalogs. DO NOT EDIT.\n" + provenance + "package builtin\n\nvar rowComparisonCatalog = [...]rowComparisonEntry{\n")
	for _, row := range rows {
		fmt.Fprintf(&generated, "{operator:%d,RowComparisonInfo:RowComparisonInfo{Family:%d,Strategy:%d,Left:%d,Right:%d,CompareFunction:%d}},\n", row.operator, row.family, row.strategy, row.left, row.right, row.function)
	}
	generated.WriteString("}\n")
	formatted, err := format.Source(generated.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(output, formatted, 0644)
}
