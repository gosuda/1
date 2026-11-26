package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestCompositeStarSelectTargetsPreserveOrderAndNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sql   string
		names []string
		types []model.TypeID
	}{
		{
			name:  "mixed targets and hidden ordering expression",
			sql:   `SELECT 0 AS "leading",(ROW(1,'value'::text)).* AS ignored,t.*,false AS "trailing" FROM (VALUES(9))t(x) ORDER BY "leading",t.x+1 LIMIT 2`,
			names: []string{"leading", "f1", "f2", "x", "trailing"},
			types: []model.TypeID{23, 23, 25, 23, 16},
		},
		{
			name:  "distinct ordering recognizes expanded fields",
			sql:   `SELECT DISTINCT (ROW(1,2)).* ORDER BY (ROW(1,2)).f1`,
			names: []string{"f1", "f2"},
			types: []model.TypeID{23, 23},
		},
		{
			name:  "identical expanded names are not ambiguous",
			sql:   `SELECT (ROW(1,2)).*,(ROW(1,2)).* ORDER BY f1`,
			names: []string{"f1", "f2", "f1", "f2"},
			types: []model.TypeID{23, 23, 23, 23},
		},
		{
			name:  "ordinal counts expanded fields",
			sql:   `SELECT (ROW(1,true)).* ORDER BY 2`,
			names: []string{"f1", "f2"},
			types: []model.TypeID{23, 16},
		},
		{
			name:  "aggregate fields remain available",
			sql:   `SELECT (ROW(sum(x),count(*))).* FROM (VALUES(1),(2))t(x)`,
			names: []string{"f1", "f2"},
			types: []model.TypeID{20, 20},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), tc.names, tc.types)
		})
	}
}

func TestCompositeStarTargetsRetainRecordConstructorProvenance(t *testing.T) {
	for _, sql := range []string{
		`SELECT (q.r).* FROM (SELECT ROW(1,true) AS r)q`,
		`WITH q AS (SELECT ROW(7,true) AS r) SELECT (q.r).* FROM q`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), []string{"f1", "f2"}, []model.TypeID{23, 16})
		})
	}
}

func TestCompositeStarReturningUsesImageAndSourceNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sql   string
		names []string
		types []model.TypeID
	}{
		{
			name:  "insert image fields",
			sql:   `INSERT INTO public.t VALUES(7) RETURNING 0 AS leading,(ROW(old.x,new.x)).* AS ignored,t.*`,
			names: []string{"leading", "f1", "f2", "x"},
			types: []model.TypeID{23, 23, 23, 23},
		},
		{
			name:  "update images and source fields",
			sql:   `UPDATE public.t SET x=7 FROM other.t AS src RETURNING old.*,(ROW(old.x,new.x,src.x,src.y)).* AS ignored,new.*`,
			names: []string{"x", "f1", "f2", "f3", "f4", "x"},
			types: []model.TypeID{23, 23, 23, 25, 16, 23},
		},
		{
			name:  "delete image aliases and source fields",
			sql:   `DELETE FROM public.t AS dst USING other.t AS src RETURNING WITH (OLD AS before_image,NEW AS after_image) dst.*,(ROW(before_image.x,after_image.x,src.x,src.y)).* AS ignored`,
			names: []string{"x", "f1", "f2", "f3", "f4"},
			types: []model.TypeID{23, 23, 23, 25, 16},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), tc.names, tc.types)
		})
	}
}

func TestCompositeStarNamedTargetsPreservePublicLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	label := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 12, Storage: model.TypeString, Collation: 950}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	fields := []model.ColumnDescriptor{
		{ID: 1, Name: "number", State: model.StatePublic, Layout: integer},
		{ID: 3, Name: "label", State: model.StatePublic, Layout: label},
		{ID: 4, Name: "numbers", State: model.StatePublic, Layout: integers},
	}
	layouts := []model.TypeLayout{integer, label, integers}
	snapshot, array := compositeTargetSnapshot(t, fields, layouts)
	for _, sql := range []string{
		`SELECT (ROW($1)).f1[1].* AS ignored`,
		`INSERT INTO outside_search_path.items VALUES(7) RETURNING (ROW($1)).f1[1].* AS ignored`,
		`UPDATE outside_search_path.items SET x=7 RETURNING ($1)[1].*`,
		`DELETE FROM outside_search_path.items RETURNING ($1)[1].*`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, snapshot, []model.TypeLayout{array})
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			requireCompositeTargetColumns(t, columns, []string{"number", "label", "numbers"}, []model.TypeID{23, 1043, 1007})
			for i, layout := range layouts {
				requireArrayAggregateLayout(t, columns[i].Layout, layout)
				if !columns[i].Nullable {
					t.Errorf("expanded column %s must permit NULL", columns[i].Name)
				}
			}
		})
	}
}

func TestCompositeStarEmptyNamedTargetsAddNoColumns(t *testing.T) {
	snapshot, array := compositeTargetSnapshot(t, nil, nil)
	for _, sql := range []string{
		`SELECT 1 AS before,($1)[1].* AS ignored,2 AS after`,
		`DELETE FROM outside_search_path.items RETURNING 1 AS before,($1)[1].* AS ignored,2 AS after`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, snapshot, []model.TypeLayout{array})
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), []string{"before", "after"}, []model.TypeID{23, 23})
		})
	}
}

func TestCompositeStarNamedTargetsRejectInconsistentDescriptors(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	// The descriptor and the referenced value layout are independent inputs. A
	// declaration the catalog can hold is still rejected when the value's
	// physical children disagree with its attributes.
	snapshot, array := compositeTargetSnapshot(t, []model.ColumnDescriptor{{ID: 1, Name: "wrong_type", State: model.StatePublic, Layout: integer}}, []model.TypeLayout{integer})
	array.Children[0].Children = []model.TypeLayout{text}
	for _, sql := range []string{
		`SELECT ($1)[1].*`,
		`DELETE FROM outside_search_path.items RETURNING ($1)[1].*`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, snapshot, []model.TypeLayout{array})
			requireCode(t, err, "XX000")
		})
	}
}

// TestNamedCompositeDeclarationRejectsDroppedAttribute pins the invariant the
// tombstoned-attribute fixture used to violate: every composite attribute needs
// exactly one physical child, so a retired attribute cannot remain declared.
func TestNamedCompositeDeclarationRejectsDroppedAttribute(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	record := model.TypeLayout{Type: model.Type{ID: 90000, Storage: model.TypeComposite, Modifier: -1}}
	_, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 30, OID: 90000, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "wrong_type"},
		Type: &model.TypeMetadata{
			Kind: model.TypeKindComposite, Layout: record, Array: 31,
			Fields: []model.ColumnDescriptor{{ID: 1, Name: "dropped", State: model.StateDropDeleteOnly, Layout: integer}},
		},
	}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if !errors.Is(err, model.ErrInvalidDescriptor) {
		t.Fatalf("dropped attribute accepted in composite declaration: %v", err)
	}
}

func TestCompositeStarTargetsRejectUnavailableRecordsAndScalarContexts(t *testing.T) {
	for _, tc := range []struct{ sql, code string }{
		{`SELECT (1).*`, "42809"},
		{`SELECT (NULL::record).*`, "42809"},
		{`SELECT (ARRAY[ROW(1)])[1].*`, "42809"},
		{`SELECT (ROW(ROW(1))).f1.*`, "42809"},
		{`SELECT (v.r).* FROM (VALUES(ROW(1)))v(r)`, "42809"},
		{`SELECT ((ROW(1)).*) IS NULL`, "0A000"},
		{`SELECT ((1).*) IS NULL`, "0A000"},
		{`SELECT (ROW(1)).* AS ignored ORDER BY ignored`, "42703"},
		{`SELECT (ROW(1)).*,(ROW(2)).* ORDER BY f1`, "42702"},
		{`SELECT (ROW(x)).*,count(*) FROM public.t`, "42803"},
		{`UPDATE public.t SET x=1 RETURNING (x).*`, "42809"},
		{`DELETE FROM public.t RETURNING ((ROW(x)).*) IS NULL`, "0A000"},
		{`UPDATE public.t SET x=1 RETURNING WITH (OLD AS previous) (ROW(old.x)).*`, "42P01"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestCompositeStarTargetsPreserveVolatileEffects(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (ROW(random(),1)).*`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireCompositeTargetColumns(t, statement.Columns(), []string{"f1", "f2"}, []model.TypeID{701, 23})
	if effects := statement.Effects(); effects.Volatility != ir.Volatile || !effects.MayError {
		t.Fatalf("expanded target effects = %+v, want volatile and may-error", effects)
	}
}

func requireCompositeTargetColumns(t *testing.T, columns []bind.ResultColumn, names []string, types []model.TypeID) {
	t.Helper()
	if len(columns) != len(names) {
		t.Fatalf("columns = %+v, want names %v", columns, names)
	}
	slots := make(map[model.SlotID]bool, len(columns))
	for i, column := range columns {
		if column.Name != names[i] || column.Layout.Type.ID != types[i] {
			t.Errorf("column %d = %+v, want name %s type %d", i, column, names[i], types[i])
		}
		if column.Slot == 0 || slots[column.Slot] {
			t.Errorf("column %d has missing or reused output slot %d", i, column.Slot)
		}
		slots[column.Slot] = true
	}
}

func compositeTargetSnapshot(t *testing.T, fields []model.ColumnDescriptor, children []model.TypeLayout) (metadataSnapshot, model.TypeLayout) {
	t.Helper()
	snapshot, array := unnestNamedSnapshot(t, fields, children)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 31, OID: 90031, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "items"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{
			{ID: 1, Name: "x", State: model.StatePublic, Layout: integer},
		}},
	}))
	return snapshot, array
}
