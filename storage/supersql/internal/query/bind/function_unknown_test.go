package bind_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBuiltinUnknownArgumentsUseCategoryPreferences(t *testing.T) {
	for _, test := range []struct {
		sql string
		ids []model.TypeID
	}{
		{`SELECT sqrt(NULL),sqrt('4'),ceil(NULL),abs(NULL),round(NULL)`, []model.TypeID{701, 701, 701, 701, 701}},
		{`SELECT length(NULL),length('abc'),substring('abc',NULL),power(NULL,3)`, []model.TypeID{23, 23, 25, 701}},
		{`SELECT sqrt($1),length($2)`, []model.TypeID{701, 23}},
	} {
		statement, err := bindSQL(t, test.sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		columns := statement.Columns()
		if len(columns) != len(test.ids) {
			t.Fatalf("columns=%v", columns)
		}
		for i, id := range test.ids {
			if columns[i].Layout.Type.ID != id {
				t.Errorf("%s column %d type=%d want %d", test.sql, i, columns[i].Layout.Type.ID, id)
			}
		}
	}
}

func TestBuiltinUnknownCategoryConflictRemainsAmbiguous(t *testing.T) {
	_, err := bindSQL(t, `SELECT * FROM generate_series(NULL,NULL)`, nil, nil)
	requireCode(t, err, "42725")
}
