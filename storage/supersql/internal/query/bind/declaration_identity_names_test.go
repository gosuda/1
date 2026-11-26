package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestOwnedSequenceNamesAvoidEarlierOwnedSequences(t *testing.T) {
	snapshot := tableSnapshot(t)
	pending, err := bindTableDeclaration(t, `CREATE TABLE owned (a integer GENERATED ALWAYS AS IDENTITY (SEQUENCE NAME owned_b_seq), b serial)`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	bound := finalizeOwnedDeclaration(t, pending, snapshot)
	names := make(map[model.ColumnID]string)
	for _, descriptor := range bound.Descriptors() {
		if sequence, ok := descriptor.Sequence(); ok {
			names[sequence.OwnerColumn] = descriptor.Name().Name
		}
	}
	if names[1] != "owned_b_seq" || names[2] != "owned_b_seq1" {
		t.Fatalf("owned names=%v", names)
	}
}

func TestSerialAliasesResolveOnlyAsUnqualifiedScalarDeclarations(t *testing.T) {
	for _, test := range []struct {
		name string
		typ  model.TypeID
	}{
		{"smallserial", 21}, {"serial2", 21}, {"serial", 23}, {"serial4", 23}, {"bigserial", 20}, {"serial8", 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending, err := bindTableDeclaration(t, "CREATE TABLE owned (id "+test.name+")", tableSnapshot(t))
			if err != nil {
				t.Fatal(err)
			}
			if got := pending.PendingSequences()[0].Configuration().Type.ID; got != test.typ {
				t.Fatalf("sequence type=%d want %d", got, test.typ)
			}
		})
	}
	for _, sql := range []string{`CREATE TABLE owned (id pg_catalog.serial)`, `CREATE TABLE owned (id serial[])`, `CREATE TABLE owned (id text GENERATED ALWAYS AS IDENTITY)`} {
		if _, err := bindTableDeclaration(t, sql, tableSnapshot(t)); err == nil {
			t.Fatalf("accepted invalid declaration: %s", sql)
		}
	}
}
