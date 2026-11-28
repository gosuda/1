package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

type operatorClassMemberKey struct {
	family, input uint32
}

type operatorClassMembers struct {
	equality, comparison uint32
	hash, hashExtended   uint32
}

func readOperatorClassMembers(directory string, types map[string]uint32) (map[operatorClassMemberKey]operatorClassMembers, error) {
	families, err := readOperatorFamilies(directory)
	if err != nil {
		return nil, err
	}
	functions, err := readCatalog(filepath.Join(directory, "pg_proc.dat"))
	if err != nil {
		return nil, err
	}
	operators, err := readCatalog(filepath.Join(directory, "pg_operator.dat"))
	if err != nil {
		return nil, err
	}
	byReference := make(map[string]map[string]string, len(operators))
	for _, operator := range operators {
		reference := operator["oprname"] + "(" + operator["oprleft"] + "," + operator["oprright"] + ")"
		byReference[reference] = operator
	}
	members := make(map[operatorClassMemberKey]operatorClassMembers)
	amops, err := readCatalog(filepath.Join(directory, "pg_amop.dat"))
	if err != nil {
		return nil, err
	}
	for _, record := range amops {
		strategy := record["amopstrategy"]
		method := record["amopmethod"]
		isEquality := method == "btree" && strategy == "3" || method == "hash" && strategy == "1"
		if !isEquality || record["amoplefttype"] != record["amoprighttype"] {
			continue
		}
		key, err := comparisonKey(families, types, record["amopfamily"], record["amoplefttype"])
		if err != nil {
			return nil, err
		}
		operator, found := byReference[record["amopopr"]]
		if !found {
			return nil, fmt.Errorf("%w: unresolved equality operator %q", errInvalidManifest, record["amopopr"])
		}
		function, err := functionFor(functions, operator["oprcode"], []string{operator["oprleft"], operator["oprright"]}, "bool")
		if err != nil {
			return nil, err
		}
		member := members[key]
		if member.equality != 0 {
			return nil, fmt.Errorf("%w: duplicate equality member", errInvalidManifest)
		}
		member.equality = function
		members[key] = member
	}
	amprocs, err := readCatalog(filepath.Join(directory, "pg_amproc.dat"))
	if err != nil {
		return nil, err
	}
	for _, record := range amprocs {
		if strings.HasPrefix(record["amprocfamily"], "hash/") {
			if err := addHashSupport(members, record, functions, families, types); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasPrefix(record["amprocfamily"], "btree/") || record["amprocnum"] != "1" || record["amproclefttype"] != record["amprocrighttype"] {
			continue
		}
		key, err := comparisonKey(families, types, record["amprocfamily"], record["amproclefttype"])
		if err != nil {
			return nil, err
		}
		function, err := functionFor(functions, record["amproc"], []string{record["amproclefttype"], record["amprocrighttype"]}, "int4")
		if err != nil {
			return nil, err
		}
		member := members[key]
		if member.comparison != 0 {
			return nil, fmt.Errorf("%w: duplicate comparison member", errInvalidManifest)
		}
		member.comparison = function
		members[key] = member
	}
	return members, nil
}

func addHashSupport(members map[operatorClassMemberKey]operatorClassMembers, record map[string]string, functions []map[string]string, families, types map[string]uint32) error {
	if record["amproclefttype"] != record["amprocrighttype"] {
		return nil
	}
	number := record["amprocnum"]
	if number != "1" && number != "2" {
		return nil
	}
	key, err := comparisonKey(families, types, record["amprocfamily"], record["amproclefttype"])
	if err != nil {
		return err
	}
	arguments := []string{record["amproclefttype"]}
	result := "int4"
	if number == "2" {
		arguments = append(arguments, "int8")
		result = "int8"
	}
	function, err := functionFor(functions, record["amproc"], arguments, result)
	if err != nil {
		return err
	}
	member := members[key]
	target := &member.hash
	if number == "2" {
		target = &member.hashExtended
	}
	if *target != 0 {
		return fmt.Errorf("%w: duplicate hash support member", errInvalidManifest)
	}
	*target = function
	members[key] = member
	return nil
}

func comparisonKey(families, types map[string]uint32, family, input string) (operatorClassMemberKey, error) {
	familyID, found := families[family]
	if !found {
		return operatorClassMemberKey{}, fmt.Errorf("%w: unknown comparison family %q", errInvalidManifest, family)
	}
	inputID, err := referencedType(types, input)
	if err != nil {
		return operatorClassMemberKey{}, err
	}
	return operatorClassMemberKey{familyID, inputID}, nil
}
