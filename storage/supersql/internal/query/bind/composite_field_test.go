package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCompositeFieldRequiresAnAvailableDescriptor(t *testing.T) {
	for _, tc := range []struct{ sql, code string }{
		{`SELECT (ROW(1)).bad`, "42703"},
		{`SELECT (ROW(1)).f01`, "42703"},
		{`SELECT (1).f1`, "42809"},
		{`SELECT (NULL::record).f1`, "42703"},
		{`SELECT (ROW(ROW(1,2))).f1.f2`, "42703"},
		{`SELECT (ARRAY[ROW(1)])[1].f1`, "42703"},
		{`SELECT ((SELECT ROW(1))).f1`, "42703"},
		{`SELECT (any_value(ROW(1))).f1`, "42703"},
		{`SELECT (coalesce(ROW(1),ROW(2))).f1`, "42703"},
		{`SELECT (v.r).f1 FROM (VALUES(ROW(1)))v(r)`, "42809"},
	} {
		t.Run(tc.sql, func(t *testing.T) { _, err := bindSQL(t, tc.sql, nil, nil); requireCode(t, err, tc.code) })
	}
}

func TestCompositeFieldDefaultColumnNameFollowsLastField(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (ROW(1,'x'::text)).f2,(ROW(ARRAY[3,4])).f1[2]`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 || columns[0].Name != "f2" || columns[1].Name != "f1" {
		t.Fatalf("column names=%v", columns)
	}
}

func TestNamedCompositeFieldsPreserveNestedLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	fields := []model.ColumnDescriptor{{ID: 1, Name: "number", State: model.StatePublic, Layout: integer}, {ID: 2, Name: "numbers", State: model.StatePublic, Layout: integers}}
	snapshot, array := unnestNamedSnapshot(t, fields, []model.TypeLayout{integer, integers})
	statement, err := bindSQL(t, `SELECT ($1)[1].number,($1)[2].numbers[1]`, snapshot, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 {
		t.Fatalf("columns=%v", columns)
	}
	for _, column := range columns {
		requireArrayAggregateLayout(t, column.Layout, integer)
	}
	_, err = bindSQL(t, `SELECT ($1)[1].absent`, snapshot, []model.TypeLayout{array})
	requireCode(t, err, "42703")
}
