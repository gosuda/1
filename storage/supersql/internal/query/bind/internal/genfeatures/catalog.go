package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PostgreSQL .dat catalogs use Perl single-quoted values, not SQL strings.
// Only escaped quote and escaped backslash lose their escape character.
type catalogInput struct {
	text   string
	offset int
}

func (p *catalogInput) skipSpace() {
	for p.offset < len(p.text) {
		switch p.text[p.offset] {
		case ' ', '\t', '\r', '\n':
			p.offset++
		case '#':
			for p.offset < len(p.text) && p.text[p.offset] != '\n' {
				p.offset++
			}
		default:
			return
		}
	}
}

func (p *catalogInput) consume(token byte) bool {
	p.skipSpace()
	if p.offset == len(p.text) || p.text[p.offset] != token {
		return false
	}
	p.offset++
	return true
}

func (p *catalogInput) quoted() (string, error) {
	if !p.consume('\'') {
		return "", fmt.Errorf("%w: expected quoted catalog value at %d", errInvalidManifest, p.offset)
	}
	start := p.offset
	var value strings.Builder
	for p.offset < len(p.text) {
		ch := p.text[p.offset]
		if ch == '\'' {
			if value.Len() == 0 {
				result := p.text[start:p.offset]
				p.offset++
				return result, nil
			}
			value.WriteString(p.text[start:p.offset])
			p.offset++
			return value.String(), nil
		}
		if ch == '\\' && p.offset+1 < len(p.text) {
			next := p.text[p.offset+1]
			if next == '\\' || next == '\'' {
				value.WriteString(p.text[start:p.offset])
				value.WriteByte(next)
				p.offset += 2
				start = p.offset
				continue
			}
		}
		p.offset++
	}
	return "", fmt.Errorf("%w: unterminated catalog value", errInvalidManifest)
}

func readCatalog(path string) ([]map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p := catalogInput{text: string(data)}
	if !p.consume('[') {
		return nil, errInvalidManifest
	}
	var records []map[string]string
	for !p.consume(']') {
		if !p.consume('{') {
			return nil, fmt.Errorf("%w: expected catalog record at %d", errInvalidManifest, p.offset)
		}
		record := make(map[string]string)
		for !p.consume('}') {
			p.skipSpace()
			start := p.offset
			for p.offset < len(p.text) {
				ch := p.text[p.offset]
				if ch != '_' && (ch < 'a' || ch > 'z') {
					break
				}
				p.offset++
			}
			key := p.text[start:p.offset]
			if key == "" || !p.consume('=') || !p.consume('>') {
				return nil, errInvalidManifest
			}
			value, err := p.quoted()
			if err != nil {
				return nil, err
			}
			if _, exists := record[key]; exists {
				return nil, errInvalidManifest
			}
			record[key] = value
			if !p.consume(',') {
				if !p.consume('}') {
					return nil, errInvalidManifest
				}
				break
			}
		}
		records = append(records, record)
		if !p.consume(',') {
			if !p.consume(']') {
				return nil, errInvalidManifest
			}
			break
		}
	}
	p.skipSpace()
	if p.offset != len(p.text) {
		return nil, errInvalidManifest
	}
	return records, nil
}

func validateReference(profile manifest, manifestPath string) error {
	catalogPath := profile.Source["catalog"]
	if catalogPath == "" {
		return errInvalidManifest
	}
	catalogPath = filepath.Join(filepath.Dir(manifestPath), catalogPath)
	types, err := readCatalog(filepath.Join(catalogPath, "pg_type.dat"))
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
		if array := entry["array_type_oid"]; array != "" {
			id, err := strconv.ParseUint(array, 10, 32)
			if err != nil {
				return err
			}
			typeIDs["_"+entry["typname"]] = uint32(id)
		}
	}
	functions, err := readCatalog(filepath.Join(catalogPath, "pg_proc.dat"))
	if err != nil {
		return err
	}
	byID := make(map[uint32]object)
	for _, entry := range profile.Features {
		for _, obj := range entry.Objects {
			if obj.Kind == "function" {
				byID[obj.OID] = obj
			}
		}
	}
	if len(byID) != len(functions) {
		return fmt.Errorf("%w: manifest has %d signatures, reference has %d", errInvalidManifest, len(byID), len(functions))
	}
	for _, reference := range functions {
		id, err := strconv.ParseUint(reference["oid"], 10, 32)
		if err != nil {
			return err
		}
		obj, found := byID[uint32(id)]
		if !found || obj.Name != reference["proname"] {
			return fmt.Errorf("%w: missing or renamed reference function %d", errInvalidManifest, id)
		}
		arguments := strings.Fields(reference["proargtypes"])
		if len(obj.Arguments) != len(arguments) || obj.Result != typeIDs[reference["prorettype"]] || obj.ResultName != reference["prorettype"] {
			return fmt.Errorf("%w: reference signature mismatch at %d", errInvalidManifest, id)
		}
		for i, name := range arguments {
			if obj.Arguments[i] != typeIDs[name] || obj.ArgumentNames[i] != name {
				return fmt.Errorf("%w: reference argument mismatch at %d", errInvalidManifest, id)
			}
		}
		if err := validateFunctionEffects(obj, reference, typeIDs); err != nil {
			return err
		}
	}
	return nil
}

func validateFunctionEffects(obj object, reference map[string]string, typeIDs map[string]uint32) error {
	// Defaults are defined in PostgreSQL REL_18_4 src/include/catalog/pg_proc.h.
	strict := cmp.Or(reference["proisstrict"], "t")
	returnsSet := cmp.Or(reference["proretset"], "f")
	volatility := cmp.Or(reference["provolatile"], "i")
	kind := cmp.Or(reference["prokind"], "f")
	namespace := cmp.Or(reference["pronamespace"], "pg_catalog")
	invalidStrict := strict != "t" && strict != "f"
	invalidReturnsSet := returnsSet != "t" && returnsSet != "f"
	if invalidStrict || invalidReturnsSet {
		return fmt.Errorf("%w: invalid reference boolean at function %d", errInvalidManifest, obj.OID)
	}
	if obj.Strict != (strict == "t") || obj.ReturnsSet != (returnsSet == "t") || obj.Volatility != volatility || obj.FunctionKind != kind || obj.Namespace != namespace {
		return fmt.Errorf("%w: reference effects mismatch at function %d", errInvalidManifest, obj.OID)
	}
	var variadic uint32
	if name := reference["provariadic"]; name != "" && name != "0" {
		var found bool
		variadic, found = typeIDs[name]
		if !found {
			return fmt.Errorf("%w: unknown reference variadic type %q", errInvalidManifest, name)
		}
	}
	if obj.Variadic != variadic {
		return fmt.Errorf("%w: reference variadic type mismatch at function %d", errInvalidManifest, obj.OID)
	}
	return nil
}
