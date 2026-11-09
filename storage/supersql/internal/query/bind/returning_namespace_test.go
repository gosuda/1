package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBindReturningNamespaceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{"duplicate OLD option", `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS a, OLD AS b) x`, "42601"},
		{"duplicate NEW option", `DELETE FROM public.t RETURNING WITH (NEW AS a, NEW AS b) x`, "42601"},
		{"duplicate image alias", `UPDATE public.t SET x=7 RETURNING WITH (OLD AS image, NEW AS image) x`, "42712"},
		{"unquoted aliases fold before collision", `UPDATE public.t SET x=7 RETURNING WITH (OLD AS image, NEW AS IMAGE) x`, "42712"},
		{"quoted alias matches folded alias", `UPDATE public.t SET x=7 RETURNING WITH (OLD AS "image", NEW AS IMAGE) x`, "42712"},
		{"OLD alias collides with target name", `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS t) x`, "42712"},
		{"NEW alias collides with target alias", `DELETE FROM public.t AS dst RETURNING WITH (NEW AS dst) x`, "42712"},
		{"explicit default name collides with target alias", `INSERT INTO public.t AS old VALUES (7) RETURNING WITH (OLD AS old) x`, "42712"},
		{"insert target alias hides bare name", `INSERT INTO public.t AS dst VALUES (7) RETURNING t.x`, "42P01"},
		{"update target alias hides bare star", `UPDATE public.t AS dst SET x=7 RETURNING t.*`, "42P01"},
		{"delete target alias hides schema column", `DELETE FROM public.t AS dst RETURNING public.t.x`, "42P01"},
		{"insert target alias hides schema star", `INSERT INTO public.t AS dst VALUES (7) RETURNING public.t.*`, "42P01"},
		{"unchanged target alias hides schema column", `UPDATE public.t AS t SET x=7 RETURNING public.t.x`, "42P01"},
		{"unchanged target alias hides schema star", `DELETE FROM public.t AS t RETURNING public.t.*`, "42P01"},
		{"dotted target alias is not schema qualification", `UPDATE public.t AS "public.t" SET x=7 RETURNING public.t.x`, "42P01"},
		{"schema qualification is not a dotted alias", `DELETE FROM public.t RETURNING "public.t".*`, "42P01"},
		{"OLD alias hides default column", `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS before_row) old.x`, "42P01"},
		{"OLD alias hides default star", `UPDATE public.t SET x=7 RETURNING WITH (OLD AS before_row) old.*`, "42P01"},
		{"NEW alias hides default column", `DELETE FROM public.t RETURNING WITH (NEW AS after_row) new.x`, "42P01"},
		{"NEW alias hides default star", `UPDATE public.t SET x=7 RETURNING WITH (NEW AS after_row) new.*`, "42P01"},
		{"quoted image alias is case sensitive", `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS "Before") before.x`, "42P01"},
		{"quoted target alias is case sensitive", `DELETE FROM public.t AS "Target" RETURNING target.*`, "42P01"},
		{"image alias has no schema qualification", `UPDATE public.t SET x=7 RETURNING WITH (OLD AS image) public.image.x`, "42P01"},
		{"FROM alias conflicts with OLD alias", `UPDATE public.t SET x=7 FROM other.t AS src RETURNING WITH (OLD AS src) x`, "42712"},
		{"USING alias conflicts with NEW alias", `DELETE FROM public.t USING other.t AS src RETURNING WITH (NEW AS src) x`, "42712"},
		{"FROM bare name conflicts with image alias", `UPDATE public.t AS dst SET x=7 FROM other.t RETURNING WITH (OLD AS t) x`, "42712"},
		{"USING bare name conflicts with image alias", `DELETE FROM public.t AS dst USING other.t RETURNING WITH (NEW AS t) x`, "42712"},
		{"FROM join alias conflicts with image alias", `UPDATE public.t AS dst SET x=7 FROM (other.t AS a CROSS JOIN other.t AS b) AS src RETURNING WITH (OLD AS src) x`, "42712"},
		{"USING join member conflicts with image alias", `DELETE FROM public.t AS dst USING other.t AS a CROSS JOIN other.t AS b RETURNING WITH (NEW AS b) x`, "42712"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestBindReturningNamespaceColumnTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sql      string
		nullable []bool
	}{
		{
			name:     "insert target qualification and default images",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING public.t.x, public.t.*, t.x, x, *, old.x, old.*, new.x, new.*`,
			nullable: []bool{false, false, false, false, false, true, true, false, false},
		},
		{
			name:     "update target alias and explicit images",
			sql:      `UPDATE public.t AS dst SET x=7 RETURNING WITH (OLD AS before_row, NEW AS after_row) dst.x, dst.*, x, *, before_row.x, before_row.*, after_row.x, after_row.*`,
			nullable: []bool{false, false, false, false, false, false, false, false},
		},
		{
			name:     "delete target qualification and default images",
			sql:      `DELETE FROM public.t RETURNING public.t.x, public.t.*, t.*, x, *, old.x, old.*, new.x, new.*`,
			nullable: []bool{false, false, false, false, false, false, false, true, true},
		},
		{
			name:     "OLD target alias suppresses absent insert image",
			sql:      `INSERT INTO public.t AS old VALUES (7) RETURNING old.x, old.*, new.x, x, *`,
			nullable: []bool{false, false, false, false, false},
		},
		{
			name:     "NEW target alias suppresses absent delete image",
			sql:      `DELETE FROM public.t AS new RETURNING new.x, new.*, old.x, x, *`,
			nullable: []bool{false, false, false, false, false},
		},
		{
			name:     "OLD alias uses opposite default name",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS new) new.x, new.*, x, *`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "NEW alias uses opposite default name",
			sql:      `DELETE FROM public.t RETURNING WITH (NEW AS old) old.x, old.*, x, *`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "both image aliases exchange default names",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS new, NEW AS old) new.x, new.*, old.x, old.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "explicit OLD alias preserves default NEW",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS before_row) before_row.x, before_row.*, new.x, new.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "explicit NEW alias preserves default OLD",
			sql:      `DELETE FROM public.t RETURNING WITH (NEW AS after_row) after_row.x, after_row.*, old.x, old.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "hidden target name is available for image alias",
			sql:      `INSERT INTO public.t AS dst VALUES (7) RETURNING WITH (OLD AS t) t.x, t.*, dst.x, dst.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "quoted uppercase target preserves lowercase default",
			sql:      `INSERT INTO public.t AS "OLD" VALUES (7) RETURNING "OLD".x, "OLD".*, old.x, old.*, new.x`,
			nullable: []bool{false, false, true, true, false},
		},
		{
			name:     "quoted image aliases remain case distinct",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS "Image", NEW AS image) "Image".x, "Image".*, image.x, image.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "dotted image alias is distinct from target schema",
			sql:      `INSERT INTO public.t VALUES (7) RETURNING WITH (OLD AS "public.t") "public.t".x, "public.t".*, public.t.x, public.t.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "hidden FROM name is available for image alias",
			sql:      `UPDATE public.t AS dst SET x=7 FROM other.t AS src RETURNING WITH (OLD AS t) t.x, t.*, dst.x, dst.*`,
			nullable: []bool{false, false, false, false},
		},
		{
			name:     "hidden USING name is available for image alias",
			sql:      `DELETE FROM public.t AS dst USING other.t AS src RETURNING WITH (NEW AS t) t.x, t.*, dst.x, dst.*`,
			nullable: []bool{true, true, false, false},
		},
		{
			name:     "whole FROM join alias hides member names",
			sql:      `UPDATE public.t AS dst SET x=7 FROM (other.t AS a CROSS JOIN other.t AS b) AS src RETURNING WITH (OLD AS a) a.x, a.*, dst.x, dst.*`,
			nullable: []bool{false, false, false, false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != len(tc.nullable) {
				t.Fatalf("RETURNING columns = %+v, want %d columns", columns, len(tc.nullable))
			}
			for i, nullable := range tc.nullable {
				omitsNullability := nullable && !columns[i].Nullable
				if columns[i].Name != "x" || columns[i].Layout.Type.ID != 23 || omitsNullability {
					t.Errorf("RETURNING column %d = %+v, want x integer; nullable required=%t", i, columns[i], nullable)
				}
			}
		})
	}
}

func TestBindReturningSchemaSelectsExactTarget(t *testing.T) {
	statement, err := bindSQL(t, `DELETE FROM other.t RETURNING other.t.*, new.*, old.x, new.y`, schemaNamespaceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name     string
		typ      model.TypeID
		nullable bool
	}{
		{"x", 25, false},
		{"y", 16, false},
		{"x", 25, true},
		{"y", 16, true},
		{"x", 25, false},
		{"y", 16, true},
	}
	columns := statement.Columns()
	if len(columns) != len(want) {
		t.Fatalf("RETURNING columns = %+v, want %d columns", columns, len(want))
	}
	for i, expected := range want {
		omitsNullability := expected.nullable && !columns[i].Nullable
		if columns[i].Name != expected.name || columns[i].Layout.Type.ID != expected.typ || omitsNullability {
			t.Errorf("RETURNING column %d = %+v, want %s with type %d; nullable required=%t", i, columns[i], expected.name, expected.typ, expected.nullable)
		}
	}
}
