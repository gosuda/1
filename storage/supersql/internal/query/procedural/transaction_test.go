package procedural

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestTransactionControlEncodingRoundTrip(t *testing.T) {
	limits := DefaultLimits()
	definition, err := encodeProgram(t.Context(), programEncoding{
		Format: programFormat, Language: "plpgsql", Procedure: true,
		Result: model.TypeLayout{Type: model.Type{ID: 2278, Storage: model.TypeAny, Modifier: -1}},
		Statements: []statement{{Kind: stmtTransaction}, {Kind: stmtTransaction, Rollback: true, Chain: true}},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	program, err := Decode(t.Context(), definition, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(program.statements) != 2 || program.statements[0].Rollback || program.statements[0].Chain || !program.statements[1].Rollback || !program.statements[1].Chain {
		t.Fatalf("transaction statements lost commit/rollback chain semantics: %+v", program.statements)
	}
}
