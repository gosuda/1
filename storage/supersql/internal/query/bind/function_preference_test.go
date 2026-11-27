package bind_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBuiltinPreferredTypesDoNotOverrideExactMatches(t *testing.T) {
	statement, err := bindSQL(t, `SELECT sqrt(4),ceil(1),floor(1),power(2,3),sqrt(4::numeric),abs(1)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	want := []model.TypeID{701, 701, 701, 701, 1700, 23}
	if len(columns) != len(want) {
		t.Fatalf("columns=%v", columns)
	}
	for i, id := range want {
		if columns[i].Layout.Type.ID != id {
			t.Errorf("column %d type=%d want %d", i, columns[i].Layout.Type.ID, id)
		}
	}
}

func TestBuiltinPreferredTypePreservesDeclaredParameterType(t *testing.T) {
	parameter := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	statement, err := bindSQL(t, `SELECT sqrt($1)`, nil, []model.TypeLayout{parameter})
	if err != nil {
		t.Fatal(err)
	}
	if statement.Columns()[0].Layout.Type.ID != 701 {
		t.Fatalf("result=%v", statement.Columns())
	}
	parameters := statement.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != 23 {
		t.Fatalf("parameter contract changed: %v", parameters)
	}
}
