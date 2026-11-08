package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type aggregateDetails struct {
	kind, finalModify, movingFinalModify byte
	directArguments uint16
	transition, final, combine, serial, deserial uint32
	movingTransition, movingInverse, movingFinal uint32
	finalExtra, movingFinalExtra bool
	sortOperator uint32
	transitionType, movingTransitionType uint32
	transitionSpace, movingTransitionSpace int32
	initialValue, movingInitialValue string
	hasInitialValue, hasMovingInitialValue bool
}

func loadAggregateDetails(profile manifest, manifestPath string) (map[uint32]aggregateDetails, error) {
	root := filepath.Join(filepath.Dir(manifestPath), profile.Source["catalog"])
	procedures, err := readCatalog(filepath.Join(root, "pg_proc.dat"))
	if err != nil { return nil, err }
	types, err := readCatalog(filepath.Join(root, "pg_type.dat"))
	if err != nil { return nil, err }
	operators, err := readCatalog(filepath.Join(root, "pg_operator.dat"))
	if err != nil { return nil, err }
	aggregates, err := readCatalog(filepath.Join(root, "pg_aggregate.dat"))
	if err != nil { return nil, err }
	typeIDs := make(map[string]uint32)
	for _, record := range types {
		oid, err := parseCatalogUint(record["oid"]); if err != nil { return nil, err }
		typeIDs[record["typname"]] = oid
		if array := record["array_type_oid"]; array != "" { value, err := parseCatalogUint(array); if err != nil { return nil, err }; typeIDs["_"+record["typname"]] = value }
	}
	procedureSignatures := make(map[string]uint32)
	procedureNames := make(map[string]uint32)
	ambiguousProcedures := make(map[string]bool)
	for _, record := range procedures {
		oid, err := parseCatalogUint(record["oid"]); if err != nil { return nil, err }
		name := record["proname"]
		procedureSignatures[name+"("+strings.ReplaceAll(record["proargtypes"], " ", ",")+")"] = oid
		if previous, ok := procedureNames[name]; ok && previous != oid { ambiguousProcedures[name] = true } else { procedureNames[name] = oid }
	}
	operatorIDs := make(map[string]uint32)
	for _, record := range operators {
		oid, err := parseCatalogUint(record["oid"]); if err != nil { return nil, err }
		operatorIDs[record["oprname"]+"("+record["oprleft"]+","+record["oprright"]+")"] = oid
	}
	function := func(name string) (uint32, error) {
		if name == "" || name == "-" { return 0, nil }
		if id := procedureSignatures[name]; id != 0 { return id, nil }
		if ambiguousProcedures[name] || procedureNames[name] == 0 { return 0, fmt.Errorf("%w: unresolved aggregate function %q", errInvalidManifest, name) }
		return procedureNames[name], nil
	}
	result := make(map[uint32]aggregateDetails, len(aggregates))
	for _, record := range aggregates {
		aggregate, err := function(record["aggfnoid"]); if err != nil { return nil, err }
		if aggregate == 0 { return nil, fmt.Errorf("%w: unresolved aggregate identity %q", errInvalidManifest, record["aggfnoid"]) }
		resolve := func(field string) (uint32, error) { return function(record[field]) }
		transition, err := resolve("aggtransfn"); if err != nil { return nil, err }
		final, err := resolve("aggfinalfn"); if err != nil { return nil, err }
		combine, err := resolve("aggcombinefn"); if err != nil { return nil, err }
		serial, err := resolve("aggserialfn"); if err != nil { return nil, err }
		deserial, err := resolve("aggdeserialfn"); if err != nil { return nil, err }
		movingTransition, err := resolve("aggmtransfn"); if err != nil { return nil, err }
		movingInverse, err := resolve("aggminvtransfn"); if err != nil { return nil, err }
		movingFinal, err := resolve("aggmfinalfn"); if err != nil { return nil, err }
		transitionType := typeIDs[record["aggtranstype"]]
		if transitionType == 0 { return nil, fmt.Errorf("%w: unresolved aggregate transition type %q", errInvalidManifest, record["aggtranstype"]) }
		movingTransitionType := typeIDs[record["aggmtranstype"]]
		transitionSpace, err := parseCatalogInt32(cmp.Or(record["aggtransspace"], "0")); if err != nil { return nil, err }
		movingTransitionSpace, err := parseCatalogInt32(cmp.Or(record["aggmtransspace"], "0")); if err != nil { return nil, err }
		direct, err := strconv.ParseUint(cmp.Or(record["aggnumdirectargs"], "0"), 10, 16); if err != nil { return nil, err }
		kind, finalModify, movingFinalModify := cmp.Or(record["aggkind"], "n"), cmp.Or(record["aggfinalmodify"], "r"), cmp.Or(record["aggmfinalmodify"], "r")
		if len(kind) != 1 || len(finalModify) != 1 || len(movingFinalModify) != 1 { return nil, errInvalidManifest }
		var sortOperator uint32
		if name := record["aggsortop"]; name != "" && name != "0" { sortOperator = operatorIDs[name]; if sortOperator == 0 { return nil, fmt.Errorf("%w: unresolved aggregate sort operator %q", errInvalidManifest, name) } }
		initial, hasInitial := record["agginitval"]
		movingInitial, hasMovingInitial := record["aggminitval"]
		result[aggregate] = aggregateDetails{kind: kind[0], finalModify: finalModify[0], movingFinalModify: movingFinalModify[0], directArguments: uint16(direct), transition: transition, final: final, combine: combine, serial: serial, deserial: deserial, movingTransition: movingTransition, movingInverse: movingInverse, movingFinal: movingFinal, finalExtra: record["aggfinalextra"] == "t", movingFinalExtra: record["aggmfinalextra"] == "t", sortOperator: sortOperator, transitionType: transitionType, movingTransitionType: movingTransitionType, transitionSpace: transitionSpace, movingTransitionSpace: movingTransitionSpace, initialValue: initial, movingInitialValue: movingInitial, hasInitialValue: hasInitial, hasMovingInitialValue: hasMovingInitial}
	}
	return result, nil
}

func parseCatalogInt32(value string) (int32, error) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	return int32(parsed), err
}
