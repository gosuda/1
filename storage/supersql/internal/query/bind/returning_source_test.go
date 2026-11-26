package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBindReturningSourceNamespaceErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{"UPDATE target and source column conflict", `UPDATE public.t SET x=7 FROM other.t AS src RETURNING x`, "42702"},
		{"DELETE target and source column conflict", `DELETE FROM public.t USING other.t AS src RETURNING x`, "42702"},
		{"UPDATE source alias hides relation name", `UPDATE public.t AS dst SET x=7 FROM other.t AS src RETURNING t.x`, "42P01"},
		{"DELETE source alias hides schema name", `DELETE FROM public.t USING other.t AS src RETURNING other.t.x`, "42P01"},
		{"UPDATE source alias hides schema star", `UPDATE public.t SET x=7 FROM other.t AS src RETURNING other.t.*`, "42P01"},
		{"DELETE same source alias hides schema star", `DELETE FROM public.t AS dst USING other.t AS t RETURNING other.t.*`, "42P01"},
		{"UPDATE source schemas have ambiguous qualifier", `UPDATE public.t AS dst SET x=7 FROM public.t, other.t RETURNING t.y`, "42P09"},
		{"DELETE source schemas have ambiguous qualifier", `DELETE FROM public.t AS dst USING public.t, other.t RETURNING t.y`, "42P09"},
		{"UPDATE source schemas have ambiguous star", `UPDATE public.t AS dst SET x=7 FROM public.t, other.t RETURNING t.*`, "42P09"},
		{"DELETE source schemas have ambiguous star", `DELETE FROM public.t AS dst USING public.t, other.t RETURNING t.*`, "42P09"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestBindReturningSourceQualificationSelectsExactColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "UPDATE schema qualification distinguishes target from source",
			sql:  `UPDATE public.t SET x=7 FROM other.t RETURNING other.t.x AS source_value, public.t.x AS target_value, y AS source_only`,
		},
		{
			name: "DELETE schema qualification distinguishes target from source",
			sql:  `DELETE FROM public.t USING other.t RETURNING other.t.x AS source_value, public.t.x AS target_value, y AS source_only`,
		},
		{
			name: "UPDATE bare source relation remains visible",
			sql:  `UPDATE public.t AS dst SET x=7 FROM other.t RETURNING t.x AS source_value, dst.x AS target_value, t.y AS source_only`,
		},
		{
			name: "DELETE source alias remains visible",
			sql:  `DELETE FROM public.t AS dst USING other.t AS src RETURNING src.x AS source_value, dst.x AS target_value, src.y AS source_only`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []struct {
				name string
				typ  model.TypeID
			}{{"source_value", 25}, {"target_value", 23}, {"source_only", 16}}
			columns := statement.Columns()
			if len(columns) != len(want) {
				t.Fatalf("RETURNING columns = %+v, want %d columns", columns, len(want))
			}
			for i, expected := range want {
				if columns[i].Name != expected.name || columns[i].Layout.Type.ID != expected.typ {
					t.Errorf("RETURNING column %d = %+v, want %s with type %d", i, columns[i], expected.name, expected.typ)
				}
			}
		})
	}
}

func TestBindReturningSourceStarOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []struct {
			name string
			typ  model.TypeID
		}
	}{
		{
			name: "UPDATE bare star emits target then sources",
			sql:  `UPDATE public.t AS dst SET x=7 FROM other.t, public.t RETURNING *`,
			want: []struct {
				name string
				typ  model.TypeID
			}{{"x", 23}, {"x", 25}, {"y", 16}, {"x", 23}},
		},
		{
			name: "DELETE bare star emits target then sources",
			sql:  `DELETE FROM public.t AS dst USING other.t, public.t RETURNING *`,
			want: []struct {
				name string
				typ  model.TypeID
			}{{"x", 23}, {"x", 25}, {"y", 16}, {"x", 23}},
		},
		{
			name: "UPDATE qualified stars select exact source schemas",
			sql:  `UPDATE public.t AS dst SET x=7 FROM public.t, other.t RETURNING other.t.*, dst.*, public.t.*`,
			want: []struct {
				name string
				typ  model.TypeID
			}{{"x", 25}, {"y", 16}, {"x", 23}, {"x", 23}},
		},
		{
			name: "DELETE qualified stars select exact source schemas",
			sql:  `DELETE FROM public.t AS dst USING public.t, other.t RETURNING other.t.*, dst.*, public.t.*`,
			want: []struct {
				name string
				typ  model.TypeID
			}{{"x", 25}, {"y", 16}, {"x", 23}, {"x", 23}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != len(tc.want) {
				t.Fatalf("RETURNING columns = %+v, want %d columns", columns, len(tc.want))
			}
			for i, expected := range tc.want {
				if columns[i].Name != expected.name || columns[i].Layout.Type.ID != expected.typ {
					t.Errorf("RETURNING column %d = %+v, want %s with type %d", i, columns[i], expected.name, expected.typ)
				}
			}
		})
	}
}

func TestBindReturningSourceAliasesSuppressImageDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "FROM old alias replaces OLD image only",
			sql:  `UPDATE public.t SET x=7 FROM other.t AS old RETURNING old.x, old.*, new.x`,
		},
		{
			name: "USING new alias replaces NEW image only",
			sql:  `DELETE FROM public.t USING other.t AS new RETURNING new.x, new.*, old.x`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			want := []struct {
				name string
				typ  model.TypeID
			}{{"x", 25}, {"x", 25}, {"y", 16}, {"x", 23}}
			columns := statement.Columns()
			if len(columns) != len(want) {
				t.Fatalf("RETURNING columns = %+v, want %d columns", columns, len(want))
			}
			for i, expected := range want {
				if columns[i].Name != expected.name || columns[i].Layout.Type.ID != expected.typ {
					t.Errorf("RETURNING column %d = %+v, want %s with type %d", i, columns[i], expected.name, expected.typ)
				}
			}
		})
	}
}
