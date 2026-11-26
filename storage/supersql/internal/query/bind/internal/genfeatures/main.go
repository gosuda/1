package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

type manifest struct {
	Profile           string            `json:"profile"`
	PostgreSQLVersion int               `json:"postgres_version_num"`
	Source            map[string]string `json:"source"`
	Features          []feature         `json:"features"`
}

type feature struct {
	ID          string            `json:"id"`
	Disposition string            `json:"disposition"`
	Reason      string            `json:"reason"`
	ASTMatch    []string          `json:"ast_match"`
	Objects     []object          `json:"resolved_objects"`
	SQLState    string            `json:"sqlstate"`
	Statements  []json.RawMessage `json:"regression_statements"`
}

type object struct {
	Kind          string   `json:"kind"`
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	OID           uint32   `json:"oid"`
	Arguments     []uint32 `json:"argument_types"`
	ArgumentNames []string `json:"argument_type_names"`
	Result        uint32   `json:"result_type"`
	ResultName    string   `json:"result_type_name"`
	Strict        bool     `json:"strict"`
	Volatility    string   `json:"volatility"`
	FunctionKind  string   `json:"function_kind"`
	ReturnsSet    bool     `json:"returns_set"`
	Variadic      uint32   `json:"variadic_type"`
	Mutable       bool     `json:"mutable"`
}

var errInvalidManifest = errors.New("invalid embedded feature manifest")

// provenance is the Source:/License: header pair of every file this generator
// writes. The tables are derived from the pinned PostgreSQL REL_18_4 catalogs
// recorded in THIRD_PARTY_NOTICES.md.
const provenance = "// Source: PostgreSQL REL_18_4 catalogs (pg_type, pg_operator, pg_opclass, pg_amop, pg_range, pg_proc.dat, system_functions.sql) and testdata/postgres/embedded_features.json\n" +
	"// License: PostgreSQL (see testdata/postgres184/COPYRIGHT)\n"

func main() {
	manifestPath := flag.String("manifest", "", "profile manifest")
	bindOutput := flag.String("bind-output", "", "generated binding policy")
	builtinOutput := flag.String("builtin-output", "", "generated builtin signatures")
	catalogOutput := flag.String("catalog-output", "", "generated operator, cast and type metadata")
	opclassOutput := flag.String("opclass-output", "", "generated operator class metadata")
	rowComparisonOutput := flag.String("row-comparison-output", "", "generated B-tree row comparison metadata")
	parameterOutput := flag.String("parameter-output", "", "generated function parameter metadata")
	rangeOutput := flag.String("range-output", "", "generated range family metadata")
	flag.Parse()
	if err := generate(*manifestPath, *bindOutput, *builtinOutput, *catalogOutput, *opclassOutput, *rowComparisonOutput, *parameterOutput); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := generateRangeCatalog(*manifestPath, *rangeOutput); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(path, bindPath, builtinPath, catalogPath, opclassPath, rowComparisonPath, parameterPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var profile manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON content", errInvalidManifest)
	}
	if profile.Profile != "embedded_pg18" || profile.PostgreSQLVersion != 180004 {
		return errInvalidManifest
	}
	features := make(map[string]bool)
	functions := make(map[uint32]bool)
	var signatures []object
	for _, entry := range profile.Features {
		if features[entry.ID] || entry.Reason == "" {
			return fmt.Errorf("%w: feature %s", errInvalidManifest, entry.ID)
		}
		features[entry.ID] = true
		if err := validateFeature(entry); err != nil {
			return err
		}
		for _, obj := range entry.Objects {
			if obj.Kind != "function" {
				continue
			}
			if obj.OID == 0 || functions[obj.OID] {
				return fmt.Errorf("%w: absent or duplicate function OID %d", errInvalidManifest, obj.OID)
			}
			if obj.Namespace != "pg_catalog" || obj.Name == "" {
				return fmt.Errorf("%w: builtin function identity %s.%s", errInvalidManifest, obj.Namespace, obj.Name)
			}
			if len(obj.Arguments) > 8 || len(obj.Arguments) != len(obj.ArgumentNames) {
				return fmt.Errorf("%w: argument layout for function %d", errInvalidManifest, obj.OID)
			}
			if obj.Result == 0 || len(obj.Volatility) != 1 || len(obj.FunctionKind) != 1 {
				return fmt.Errorf("%w: result/effects for function %d", errInvalidManifest, obj.OID)
			}
			functions[obj.OID] = true
			signatures = append(signatures, obj)
		}
	}
	if len(features) != 25 {
		return fmt.Errorf("%w: fixed profile feature set changed", errInvalidManifest)
	}
	if err := validateReference(profile, path); err != nil {
		return err
	}
	if len(signatures) == 0 || len(signatures) > math.MaxUint16 {
		return fmt.Errorf("%w: builtin signature index capacity", errInvalidManifest)
	}
	slices.SortFunc(signatures, func(a, b object) int {
		if a.OID < b.OID {
			return -1
		}
		if a.OID > b.OID {
			return 1
		}
		return 0
	})
	procedureCatalog, err := loadProcedureDetails(profile, path)
	if err != nil {
		return err
	}
	aggregateCatalog, err := loadAggregateDetails(profile, path)
	if err != nil {
		return err
	}
	var catalog bytes.Buffer
	catalog.WriteString("// Code generated by genfeatures from embedded_features.json. DO NOT EDIT.\n" + provenance + "package builtin\n\nimport \"github.com/gosuda/supersql-alpha/internal/model\"\n\nvar signatures = [...]Signature{\n")
	for _, obj := range signatures {
		details, ok := procedureCatalog[obj.OID]
		if !ok {
			return fmt.Errorf("%w: missing procedure catalog row %d", errInvalidManifest, obj.OID)
		}
		fmt.Fprintf(&catalog, "{ID:%d, Name:%q, Arguments:[8]model.TypeID{", obj.OID, obj.Name)
		for _, id := range obj.Arguments {
			fmt.Fprintf(&catalog, "%d,", id)
		}
		fmt.Fprintf(&catalog, "}, ArgumentCount:%d, Result:%d, Variadic:%d, Strict:%t, ReturnsSet:%t, Volatility:%s, Kind:%s, Language:%d, Cost:%g, Rows:%g, Support:%d, SecurityDefiner:%t, Leakproof:%t, Parallel:%s, Source:%q",
			len(obj.Arguments), obj.Result, obj.Variadic, obj.Strict, obj.ReturnsSet, strconv.QuoteRune(rune(obj.Volatility[0])), strconv.QuoteRune(rune(obj.FunctionKind[0])),
			details.language, details.cost, details.rows, details.support, details.securityDefiner, details.leakproof, strconv.QuoteRune(rune(details.parallel)), details.source)
		aggregate, isAggregate := aggregateCatalog[obj.OID]
		if (obj.FunctionKind == "a") != isAggregate {
			return fmt.Errorf("%w: aggregate catalog classification mismatch for %d", errInvalidManifest, obj.OID)
		}
		if isAggregate {
			fmt.Fprintf(&catalog, ", AggregateKind:%s, AggregateDirectArguments:%d, AggregateTransition:%d, AggregateFinal:%d, AggregateCombine:%d, AggregateSerial:%d, AggregateDeserial:%d, AggregateMovingTransition:%d, AggregateMovingInverse:%d, AggregateMovingFinal:%d, AggregateFinalExtra:%t, AggregateMovingFinalExtra:%t, AggregateFinalModify:%s, AggregateMovingFinalModify:%s, AggregateSortOperator:%d, AggregateTransitionType:%d, AggregateTransitionSpace:%d, AggregateMovingTransitionType:%d, AggregateMovingTransitionSpace:%d, AggregateInitialValue:%q, AggregateMovingInitialValue:%q, AggregateHasInitialValue:%t, AggregateHasMovingInitialValue:%t",
				strconv.QuoteRune(rune(aggregate.kind)), aggregate.directArguments, aggregate.transition, aggregate.final, aggregate.combine, aggregate.serial, aggregate.deserial,
				aggregate.movingTransition, aggregate.movingInverse, aggregate.movingFinal, aggregate.finalExtra, aggregate.movingFinalExtra,
				strconv.QuoteRune(rune(aggregate.finalModify)), strconv.QuoteRune(rune(aggregate.movingFinalModify)), aggregate.sortOperator,
				aggregate.transitionType, aggregate.transitionSpace, aggregate.movingTransitionType, aggregate.movingTransitionSpace,
				aggregate.initialValue, aggregate.movingInitialValue, aggregate.hasInitialValue, aggregate.hasMovingInitialValue)
		}
		catalog.WriteString("},\n")
	}
	catalog.WriteString("}\nvar signatureNames = [...]uint16{\n")
	indices := make([]uint16, len(signatures))
	for i := range indices {
		indices[i] = uint16(i)
	}
	slices.SortFunc(indices, func(a, b uint16) int {
		if order := strings.Compare(signatures[a].Name, signatures[b].Name); order != 0 {
			return order
		}
		return int(a) - int(b)
	})
	for _, index := range indices {
		fmt.Fprintf(&catalog, "%d,\n", index)
	}
	catalog.WriteString("}\n")
	var policy bytes.Buffer
	policy.WriteString("// Code generated by genfeatures from embedded_features.json. DO NOT EDIT.\n" + provenance + "package bind\n\nimport \"github.com/gosuda/supersql-alpha/internal/model\"\n")
	for _, kind := range []string{"function", "type", "collation"} {
		idType := "model.FunctionID"
		switch kind {
		case "type":
			idType = "model.TypeID"
		case "collation":
			idType = "model.CollationID"
		}
		fmt.Fprintf(&policy, "\nfunc builtin%sFeature(id %s) FeatureID {\nswitch id {\n", strings.ToUpper(kind[:1])+kind[1:], idType)
		for _, entry := range profile.Features {
			if entry.Disposition != "unsupported" {
				continue
			}
			var ids []uint32
			for _, obj := range entry.Objects {
				if obj.Kind == kind {
					ids = append(ids, obj.OID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			slices.Sort(ids)
			policy.WriteString("case ")
			for i, id := range ids {
				if i != 0 {
					policy.WriteByte(',')
				}
				fmt.Fprint(&policy, id)
			}
			fmt.Fprintf(&policy, ":\nreturn %q\n", entry.ID)
		}
		policy.WriteString("}\nreturn \"\"\n}\n")
	}
	policy.WriteString("\nfunc builtinRelationFeature(name string) FeatureID {\nswitch name {\n")
	for _, entry := range profile.Features {
		if entry.Disposition != "unsupported" {
			continue
		}
		for _, obj := range entry.Objects {
			if obj.Kind == "relation" {
				fmt.Fprintf(&policy, "case %q: return %q\n", obj.Namespace+"."+obj.Name, entry.ID)
			}
		}
	}
	policy.WriteString("}\nreturn \"\"\n}\n")
	policy.WriteString("\nfunc builtinSettingFeature(name string) (FeatureID, bool) {\nswitch name {\n")
	for _, entry := range profile.Features {
		id := entry.ID
		if entry.Disposition == "required" {
			id = ""
		}
		for _, obj := range entry.Objects {
			if obj.Kind == "setting" {
				fmt.Fprintf(&policy, "case %q: return %q,true\n", obj.Name, id)
			}
		}
	}
	policy.WriteString("}\nreturn \"\",false\n}\n")
	catalogBytes, err := format.Source(catalog.Bytes())
	if err != nil {
		return err
	}
	policyBytes, err := format.Source(policy.Bytes())
	if err != nil {
		return err
	}
	if catalogPath != "" || opclassPath != "" || rowComparisonPath != "" {
		if err := generateResolutionCatalog(profile, path, catalogPath, opclassPath, rowComparisonPath); err != nil {
			return err
		}
	}
	if err := generateParameterCatalog(profile, path, parameterPath); err != nil {
		return err
	}
	if err := os.WriteFile(builtinPath, catalogBytes, 0644); err != nil {
		return err
	}
	return os.WriteFile(bindPath, policyBytes, 0644)
}
