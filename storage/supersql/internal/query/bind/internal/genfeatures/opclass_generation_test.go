package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOperatorClassGeneratedOIDsIncludeAllMethods(t *testing.T) {
	directory := t.TempDir()
	family := `[
{ oid => '1976', opfmethod => 'btree', opfname => 'integer_ops' },
{ opfmethod => 'hash', opfname => 'integer_ops' }
]`
	classes := `[
{ opcmethod => 'hash', opcname => 'int8_ops', opcfamily => 'hash/integer_ops', opcintype => 'int8' },
{ oid => '3124', opcmethod => 'btree', opcname => 'int8_ops', opcfamily => 'btree/integer_ops', opcintype => 'int8' },
{ opcmethod => 'btree', opcname => 'int4_ops', opcfamily => 'btree/integer_ops', opcintype => 'int4' },
{ opcmethod => 'btree', opcname => 'int4_alias', opcfamily => 'btree/integer_ops', opcintype => 'int4', opcdefault => 'f' }
]`
	for name, text := range map[string]string{"pg_opfamily.dat": family, "pg_opclass.dat": classes} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	classesRead, err := readOperatorClasses(directory, map[string]uint32{"int8": 20, "int4": 23})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint32{"hash/int8_ops": 10000, "btree/int8_ops": 3124, "btree/int4_ops": 10001, "btree/int4_alias": 10002}
	for _, class := range classesRead {
		key := class.method + "/" + class.name
		id, found := want[key]
		if !found || class.id != id {
			t.Fatalf("class %s ID %d, want %d", key, class.id, id)
		}
		if class.method == "hash" && class.family != 10000 {
			t.Fatalf("family catalog does not have independent generated OIDs: %d", class.family)
		}
		if class.method == "btree" && class.family != 1976 {
			t.Fatalf("fixed family identity changed: %d", class.family)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing classes: %v", want)
	}
}
