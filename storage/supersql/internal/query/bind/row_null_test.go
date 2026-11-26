package bind_test

import (
	"testing"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestBindExplicitRowNullMetadata(t *testing.T) {
	const sql = `SELECT ROW(NULL::integer,1) IS NULL,ROW(NULL::integer,1) IS NOT NULL`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range parsed.Stmts[0].Stmt.GetSelectStmt().TargetList {
		target.GetResTarget().Val.GetNullTest().Argisrow = true
	}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bindingOptions(sql), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 {
		t.Fatalf("columns=%v", columns)
	}
	for _, column := range columns {
		if column.Layout.Type.ID != 16 || column.Nullable {
			t.Fatalf("row-null result contract=%v", column)
		}
	}
}
