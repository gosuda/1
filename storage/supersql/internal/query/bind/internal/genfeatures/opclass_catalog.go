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
	"strings"
)

type resolutionOperatorClass struct {
	id, input, family, key uint32
	name, method, familyName string
	isDefault              bool
}

func assignCatalogOIDs(records []map[string]string, catalog string) ([]uint32, error) {
	// REL_18_4 genbki.pl assigns only missing OIDs, independently per catalog.
	// The bounds are FirstGenbkiObjectId and FirstUnpinnedObjectId in transam.h.
	const firstGenbkiObjectID, firstUnpinnedObjectID = 10000, 12000
	next := uint32(firstGenbkiObjectID)
	ids := make([]uint32, len(records))
	seen := make(map[uint32]bool, len(records))
	for i, entry := range records {
		var id uint32
		if text, present := entry["oid"]; present {
			var err error
			id, err = catalogOID(text)
			if err != nil {
				return nil, fmt.Errorf("%s row %d: %w", catalog, i+1, err)
			}
		} else {
			if next >= firstUnpinnedObjectID {
				return nil, fmt.Errorf("%w: %s generated OID %d reaches FirstUnpinnedObjectId", errInvalidManifest, catalog, next)
			}
			id = next
			next++
		}
		if id == 0 || seen[id] {
			return nil, fmt.Errorf("%w: %s row %d has absent or duplicate OID %d", errInvalidManifest, catalog, i+1, id)
		}
		seen[id], ids[i] = true, id
	}
	return ids, nil
}

func operatorCatalogIdentity(entry map[string]string, prefix string) (method, name string, err error) {
	for field := range entry {
		switch field {
		case "oid", "oid_symbol", "descr", prefix + "method", prefix + "name", prefix + "namespace", prefix + "owner":
		case "opcfamily", "opcintype", "opcdefault", "opckeytype":
			if prefix != "opc" {
				return "", "", fmt.Errorf("%w: unknown operator catalog field %q", errInvalidManifest, field)
			}
		default:
			return "", "", fmt.Errorf("%w: unknown operator catalog field %q", errInvalidManifest, field)
		}
	}
	method, name = entry[prefix+"method"], entry[prefix+"name"]
	// These are the index methods in REL_18_4 pg_am.dat, not codec capabilities.
	switch method {
	case "btree", "hash", "gist", "gin", "spgist", "brin":
	default:
		return "", "", fmt.Errorf("%w: operator catalog access method %q", errInvalidManifest, method)
	}
	if name == "" || len(name) > 63 || strings.ContainsAny(name, "/\x00") {
		return "", "", fmt.Errorf("%w: operator catalog name %q", errInvalidManifest, name)
	}
	if namespace, present := entry[prefix+"namespace"]; present && namespace != "pg_catalog" {
		return "", "", fmt.Errorf("%w: operator catalog namespace %q", errInvalidManifest, namespace)
	}
	if owner, present := entry[prefix+"owner"]; present && owner != "POSTGRES" {
		return "", "", fmt.Errorf("%w: operator catalog owner %q", errInvalidManifest, owner)
	}
	return method, name, nil
}

func readOperatorFamilies(directory string) (map[string]uint32, error) {
	records, err := readCatalog(filepath.Join(directory, "pg_opfamily.dat"))
	if err != nil {
		return nil, err
	}
	ids, err := assignCatalogOIDs(records, "pg_opfamily")
	if err != nil {
		return nil, err
	}
	families := make(map[string]uint32, len(records))
	for i, entry := range records {
		method, name, err := operatorCatalogIdentity(entry, "opf")
		if err != nil {
			return nil, fmt.Errorf("pg_opfamily row %d: %w", i+1, err)
		}
		reference := method + "/" + name
		if _, exists := families[reference]; exists {
			return nil, fmt.Errorf("%w: duplicate operator family %q", errInvalidManifest, reference)
		}
		families[reference] = ids[i]
	}
	return families, nil
}

func readOperatorClasses(directory string, typeIDs map[string]uint32) ([]resolutionOperatorClass, error) {
	families, err := readOperatorFamilies(directory)
	if err != nil {
		return nil, err
	}
	records, err := readCatalog(filepath.Join(directory, "pg_opclass.dat"))
	if err != nil {
		return nil, err
	}
	ids, err := assignCatalogOIDs(records, "pg_opclass")
	if err != nil {
		return nil, err
	}
	if len(records) == 0 || len(records) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: operator class index capacity", errInvalidManifest)
	}
	classes := make([]resolutionOperatorClass, 0, len(records))
	names := make(map[[2]string]bool, len(records))
	defaults := make(map[struct {
		method string
		input  uint32
	}]bool)
	for i, entry := range records {
		method, name, err := operatorCatalogIdentity(entry, "opc")
		if err != nil {
			return nil, fmt.Errorf("pg_opclass row %d: %w", i+1, err)
		}
		identity := [2]string{method, name}
		if names[identity] {
			return nil, fmt.Errorf("%w: duplicate operator class %s/%s", errInvalidManifest, method, name)
		}
		names[identity] = true
		familyReference := entry["opcfamily"]
		family, exists := families[familyReference]
		if !exists || !strings.HasPrefix(familyReference, method+"/") {
			return nil, fmt.Errorf("%w: operator class %s/%s has unknown or mismatched family %q", errInvalidManifest, method, name, familyReference)
		}
		input, err := referencedType(typeIDs, entry["opcintype"])
		if err != nil {
			return nil, fmt.Errorf("operator class %s/%s input: %w", method, name, err)
		}
		if input == 0 {
			return nil, fmt.Errorf("%w: operator class %s/%s has no input type", errInvalidManifest, method, name)
		}
		// pg_opclass.h supplies opcdefault=t and opckeytype=0 when omitted.
		defaultText, present := entry["opcdefault"]
		if !present {
			defaultText = "t"
		}
		if defaultText != "t" && defaultText != "f" {
			return nil, fmt.Errorf("%w: operator class %s/%s default flag %q", errInvalidManifest, method, name, defaultText)
		}
		keyName, present := entry["opckeytype"]
		if !present {
			keyName = "0"
		}
		if keyName == "" || keyName == "-" {
			return nil, fmt.Errorf("%w: operator class %s/%s key type %q", errInvalidManifest, method, name, keyName)
		}
		key, err := referencedType(typeIDs, keyName)
		if err != nil {
			return nil, fmt.Errorf("operator class %s/%s key: %w", method, name, err)
		}
		isDefault := defaultText == "t"
		if isDefault {
			identity := struct {
				method string
				input  uint32
			}{method, input}
			if defaults[identity] {
				return nil, fmt.Errorf("%w: duplicate default operator class for %s input %d", errInvalidManifest, method, input)
			}
			defaults[identity] = true
		}
		classes = append(classes, resolutionOperatorClass{id: ids[i], name: name, method: method, familyName: strings.TrimPrefix(familyReference, method+"/"), input: input, family: family, isDefault: isDefault, key: key})
	}
	slices.SortFunc(classes, func(a, b resolutionOperatorClass) int { return cmp.Compare(a.id, b.id) })
	return classes, nil
}

func generateOperatorClassCatalog(directory string, typeIDs map[string]uint32, output string) error {
	classes, err := readOperatorClasses(directory, typeIDs)
	if err != nil {
		return err
	}
	members, err := readOperatorClassMembers(directory, typeIDs)
	if err != nil {
		return err
	}
	var generated bytes.Buffer
	generated.WriteString("// Code generated by genfeatures from PostgreSQL REL_18_4 catalogs. DO NOT EDIT.\n" + provenance + "package builtin\n\nvar operatorClassCatalog = [...]OperatorClassInfo{\n")
	names := make([]uint16, len(classes))
	defaults := make([]uint16, 0, len(classes))
	for i, info := range classes {
		member := members[operatorClassMemberKey{family: info.family, input: info.input}]
		fmt.Fprintf(&generated, "{ID:%d,Name:%q,Method:%q,InputType:%d,Family:%d,FamilyName:%q,Default:%t,KeyType:%d,EqualityFunction:%d,CompareFunction:%d,HashFunction:%d,HashExtendedFunction:%d},\n", info.id, info.name, info.method, info.input, info.family, info.familyName, info.isDefault, info.key, member.equality, member.comparison, member.hash, member.hashExtended)
		names[i] = uint16(i)
		if info.isDefault {
			defaults = append(defaults, uint16(i))
		}
	}
	slices.SortFunc(names, func(a, b uint16) int {
		if order := strings.Compare(classes[a].method, classes[b].method); order != 0 {
			return order
		}
		return strings.Compare(classes[a].name, classes[b].name)
	})
	slices.SortFunc(defaults, func(a, b uint16) int {
		if order := strings.Compare(classes[a].method, classes[b].method); order != 0 {
			return order
		}
		return cmp.Compare(classes[a].input, classes[b].input)
	})
	generated.WriteString("}\nvar operatorClassNames = [...]uint16{\n")
	for _, index := range names {
		fmt.Fprintf(&generated, "%d,\n", index)
	}
	generated.WriteString("}\nvar operatorClassDefaults = [...]uint16{\n")
	for _, index := range defaults {
		fmt.Fprintf(&generated, "%d,\n", index)
	}
	generated.WriteString("}\n")
	formatted, err := format.Source(generated.Bytes())
	if err != nil {
		return err
	}
	return os.WriteFile(output, formatted, 0644)
}
