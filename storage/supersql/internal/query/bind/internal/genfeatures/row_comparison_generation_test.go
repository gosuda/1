package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func rowComparisonFixture() map[string]string {
	return map[string]string{
		"pg_opfamily.dat": `[
{ oid => '1976', opfmethod => 'btree', opfname => 'integer_ops' },
{ oid => '9000', opfmethod => 'btree', opfname => 'other_ops' },
{ oid => '1977', opfmethod => 'hash', opfname => 'integer_ops' }
]`,
		"pg_proc.dat": `[
{ oid => '2192', proname => 'btint28cmp', prorettype => 'int4', proargtypes => 'int2 int8' },
{ oid => '9001', proname => 'other_cmp', prorettype => 'int4', proargtypes => 'int2 int8' },
{ oid => '1864', proname => 'int28lt', prorettype => 'bool', proargtypes => 'int2 int8' },
{ oid => '1862', proname => 'int28eq', prorettype => 'bool', proargtypes => 'int2 int8' },
{ oid => '1863', proname => 'int28ne', prorettype => 'bool', proargtypes => 'int2 int8' }
]`,
		"pg_operator.dat": `[
{ oid => '1864', oprname => '<', oprleft => 'int2', oprright => 'int8', oprresult => 'bool', oprcode => 'int28lt' },
{ oid => '1862', oprname => '=', oprleft => 'int2', oprright => 'int8', oprresult => 'bool', oprcode => 'int28eq', oprnegate => '<>(int2,int8)' },
{ oid => '1863', oprname => '<>', oprleft => 'int2', oprright => 'int8', oprresult => 'bool', oprcode => 'int28ne', oprnegate => '=(int2,int8)' }
]`,
		"pg_amop.dat": `[
{ amopfamily => 'btree/other_ops', amoplefttype => 'int2', amoprighttype => 'int8', amopstrategy => '3', amopopr => '=(int2,int8)', amopmethod => 'btree' },
{ amopfamily => 'btree/integer_ops', amoplefttype => 'int2', amoprighttype => 'int8', amopstrategy => '3', amopopr => '=(int2,int8)', amopmethod => 'btree' },
{ amopfamily => 'btree/integer_ops', amoplefttype => 'int2', amoprighttype => 'int8', amopstrategy => '1', amopopr => '<(int2,int8)', amopmethod => 'btree' },
{ amopfamily => 'hash/integer_ops', amoplefttype => 'int2', amoprighttype => 'int8', amopstrategy => '1', amopopr => '=(int2,int8)', amopmethod => 'hash' }
]`,
		"pg_amproc.dat": `[
{ amprocfamily => 'btree/other_ops', amproclefttype => 'int2', amprocrighttype => 'int8', amprocnum => '1', amproc => 'other_cmp' },
{ amprocfamily => 'btree/integer_ops', amproclefttype => 'int2', amprocrighttype => 'int8', amprocnum => '1', amproc => 'btint28cmp(int2,int8)' }
]`,
	}
}

func readRowComparisonFixture(t *testing.T, files map[string]string) ([]rowComparisonRow, error) {
	t.Helper()
	directory := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return readRowComparisonCatalog(directory, map[string]uint32{"int2": 21, "int4": 23, "int8": 20, "bool": 16})
}

func TestRowComparisonCatalogRetainsCrossTypeFamiliesAndNegators(t *testing.T) {
	rows, err := readRowComparisonFixture(t, rowComparisonFixture())
	if err != nil {
		t.Fatal(err)
	}
	want := []rowComparisonRow{
		{rowComparisonKey{1976, 21, 20}, 1862, 2192, 3},
		{rowComparisonKey{9000, 21, 20}, 1862, 9001, 3},
		{rowComparisonKey{1976, 21, 20}, 1863, 2192, 6},
		{rowComparisonKey{9000, 21, 20}, 1863, 9001, 6},
		{rowComparisonKey{1976, 21, 20}, 1864, 2192, 1},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("interpretations = %+v; want %+v", rows, want)
	}
}

func TestRowComparisonDirectMembershipPrecedesNegator(t *testing.T) {
	files := rowComparisonFixture()
	files["pg_amop.dat"] = strings.Replace(files["pg_amop.dat"], "amopstrategy => '1', amopopr => '<(int2,int8)'", "amopstrategy => '1', amopopr => '<>(int2,int8)'", 1)
	rows, err := readRowComparisonFixture(t, files)
	if err != nil {
		t.Fatal(err)
	}
	var interpretations []rowComparisonRow
	for _, row := range rows {
		if row.operator == 1863 {
			interpretations = append(interpretations, row)
		}
	}
	want := []rowComparisonRow{{rowComparisonKey{1976, 21, 20}, 1863, 2192, 1}}
	if !slices.Equal(interpretations, want) {
		t.Fatalf("direct <> interpretations = %+v; want %+v", interpretations, want)
	}
}

func TestRowComparisonAbsentSupportIsNotInferred(t *testing.T) {
	files := rowComparisonFixture()
	files["pg_amproc.dat"] = "[]"
	rows, err := readRowComparisonFixture(t, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatalf("missing support removed interpretations: %+v", rows)
	}
	for _, row := range rows {
		if row.function != 0 {
			t.Fatalf("invented support function for %+v", row)
		}
	}
}

func TestRowComparisonCatalogRejectsInvalidReferences(t *testing.T) {
	for _, tc := range []struct {
		name, file, old, replacement string
	}{
		{"unknown_family", "pg_amop.dat", "btree/other_ops", "btree/missing"},
		{"unknown_type", "pg_amop.dat", "amoplefttype => 'int2'", "amoplefttype => 'missing'"},
		{"absent_type", "pg_amop.dat", "amoplefttype => 'int2'", "amoplefttype => '0'"},
		{"operator_arguments", "pg_amop.dat", "amoplefttype => 'int2'", "amoplefttype => 'int4'"},
		{"unknown_operator", "pg_amop.dat", "=(int2,int8)", "=(int4,int8)"},
		{"unknown_negator", "pg_operator.dat", "oprnegate => '=(int2,int8)'", "oprnegate => '=(int4,int8)'"},
		{"mismatched_method", "pg_amop.dat", "amopmethod => 'btree'", "amopmethod => 'hash'"},
		{"invalid_strategy", "pg_amop.dat", "amopstrategy => '3'", "amopstrategy => '6'"},
		{"duplicate_membership", "pg_amop.dat", "btree/other_ops", "btree/integer_ops"},
		{"duplicate_support", "pg_amproc.dat", "btree/other_ops", "btree/integer_ops"},
		{"support_arguments", "pg_proc.dat", "proargtypes => 'int2 int8'", "proargtypes => 'int2 int2'"},
		{"support_return", "pg_proc.dat", "prorettype => 'int4'", "prorettype => 'int8'"},
		{"support_reference_arguments", "pg_amproc.dat", "btint28cmp(int2,int8)", "btint28cmp(int8,int2)"},
		{"unknown_support", "pg_amproc.dat", "other_cmp", "missing_cmp"},
		{"set_returning_support", "pg_proc.dat", "proname => 'other_cmp'", "proname => 'other_cmp', proretset => 't'"},
		{"aggregate_support", "pg_proc.dat", "proname => 'other_cmp'", "proname => 'other_cmp', prokind => 'a'"},
		{"set_returning_operator", "pg_proc.dat", "proname => 'int28eq'", "proname => 'int28eq', proretset => 't'"},
		{"duplicate_function_oid", "pg_proc.dat", "oid => '9001'", "oid => '2192'"},
		{"missing_function_oid", "pg_proc.dat", "oid => '9001'", "oid => '0'"},
		{"duplicate_operator_oid", "pg_operator.dat", "oid => '1862'", "oid => '1864'"},
		{"duplicate_function_signature", "pg_proc.dat", "proname => 'other_cmp'", "proname => 'btint28cmp'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := rowComparisonFixture()
			files[tc.file] = strings.Replace(files[tc.file], tc.old, tc.replacement, 1)
			_, err := readRowComparisonFixture(t, files)
			if !errors.Is(err, errInvalidManifest) {
				t.Fatalf("invalid reference accepted: %v", err)
			}
		})
	}
}
