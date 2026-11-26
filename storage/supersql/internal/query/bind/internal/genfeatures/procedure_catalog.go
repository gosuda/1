package main

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strconv"
)

type procedureDetails struct {
	language        uint32
	cost            float64
	rows            float64
	support         uint32
	securityDefiner bool
	leakproof       bool
	parallel        byte
	source          string
}

func loadProcedureDetails(profile manifest, manifestPath string) (map[uint32]procedureDetails, error) {
	catalogRoot := profile.Source["catalog"]
	if catalogRoot == "" {
		return nil, errInvalidManifest
	}
	records, err := readCatalog(filepath.Join(filepath.Dir(manifestPath), catalogRoot, "pg_proc.dat"))
	if err != nil {
		return nil, err
	}
	byName := make(map[string]uint32)
	ambiguous := make(map[string]bool)
	for _, record := range records {
		oid, err := parseCatalogUint(record["oid"])
		if err != nil {
			return nil, err
		}
		name := record["proname"]
		if previous, ok := byName[name]; ok && previous != oid {
			ambiguous[name] = true
		} else {
			byName[name] = oid
		}
	}
	result := make(map[uint32]procedureDetails, len(records))
	for _, record := range records {
		oid, err := parseCatalogUint(record["oid"])
		if err != nil {
			return nil, err
		}
		language, ok := map[string]uint32{"internal": 12, "c": 13, "sql": 14}[cmp.Or(record["prolang"], "internal")]
		if !ok {
			return nil, fmt.Errorf("%w: unknown procedure language %q", errInvalidManifest, record["prolang"])
		}
		cost, err := strconv.ParseFloat(cmp.Or(record["procost"], "1"), 64)
		if err != nil {
			return nil, err
		}
		rows, err := strconv.ParseFloat(cmp.Or(record["prorows"], "0"), 64)
		if err != nil {
			return nil, err
		}
		var support uint32
		if name := record["prosupport"]; name != "" && name != "-" {
			if ambiguous[name] || byName[name] == 0 {
				return nil, fmt.Errorf("%w: unresolved support function %q", errInvalidManifest, name)
			}
			support = byName[name]
		}
		parallel := cmp.Or(record["proparallel"], "u")
		if len(parallel) != 1 {
			return nil, errInvalidManifest
		}
		result[oid] = procedureDetails{
			language: language,
			cost: cost,
			rows: rows,
			support: support,
			securityDefiner: record["prosecdef"] == "t",
			leakproof: record["proleakproof"] == "t",
			parallel: parallel[0],
			source: record["prosrc"],
		}
	}
	return result, nil
}



func parseCatalogUint(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(value, 10, 32)
	return uint32(parsed), err
}
