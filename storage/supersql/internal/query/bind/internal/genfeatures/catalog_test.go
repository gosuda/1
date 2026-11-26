package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReferenceRejectsChangedFunctionEffects(t *testing.T) {
	directory := t.TempDir()
	types := "[{oid => '20', typname => 'int8', array_type_oid => '1016'}]"
	functions := "[{oid => '1', proname => 'effectful', prorettype => 'int8', proargtypes => 'int8', proisstrict => 'f', provolatile => 'v', prokind => 'w', proretset => 't', provariadic => 'int8'}]"
	for name, data := range map[string]string{"pg_type.dat": types, "pg_proc.dat": functions} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	original := object{Kind: "function", OID: 1, Namespace: "pg_catalog", Name: "effectful", Arguments: []uint32{20}, ArgumentNames: []string{"int8"}, Result: 20, ResultName: "int8", Volatility: "v", FunctionKind: "w", ReturnsSet: true, Variadic: 20}
	check := func(value object) error {
		return validateReference(manifest{Source: map[string]string{"catalog": "."}, Features: []feature{{Objects: []object{value}}}}, filepath.Join(directory, "profile.json"))
	}
	if err := check(original); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*object)
	}{
		{"volatile_mislabeled_immutable", func(obj *object) { obj.Volatility = "i" }},
		{"nonnull_shortcut_on_nonstrict", func(obj *object) { obj.Strict = true }},
		{"set_returning_mislabeled_scalar", func(obj *object) { obj.ReturnsSet = false }},
		{"window_mislabeled_function", func(obj *object) { obj.FunctionKind = "f" }},
		{"variadic_element_type", func(obj *object) { obj.Variadic = 1016 }},
		{"result_name", func(obj *object) { obj.ResultName = "_int8" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			changed := original
			scenario.change(&changed)
			if err := check(changed); !errors.Is(err, errInvalidManifest) {
				t.Fatalf("accepted conflicting reference metadata: %v", err)
			}
		})
	}
}

func TestReferenceAppliesPostgresProcedureDefaults(t *testing.T) {
	directory := t.TempDir()
	for name, data := range map[string]string{
		"pg_type.dat": "[{oid => '20', typname => 'int8'}]",
		"pg_proc.dat": "[{oid => '1', proname => 'identity', prorettype => 'int8', proargtypes => 'int8'}]",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	signature := object{Kind: "function", OID: 1, Namespace: "pg_catalog", Name: "identity", Arguments: []uint32{20}, ArgumentNames: []string{"int8"}, Result: 20, ResultName: "int8", Strict: true, Volatility: "i", FunctionKind: "f"}
	profile := manifest{Source: map[string]string{"catalog": "."}, Features: []feature{{Objects: []object{signature}}}}
	if err := validateReference(profile, filepath.Join(directory, "profile.json")); err != nil {
		t.Fatal(err)
	}
	profile.Features[0].Objects[0].Strict = false
	if err := validateReference(profile, filepath.Join(directory, "profile.json")); !errors.Is(err, errInvalidManifest) {
		t.Fatalf("ignored reference strict default: %v", err)
	}
}
