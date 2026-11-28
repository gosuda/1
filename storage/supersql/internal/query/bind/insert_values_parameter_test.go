package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestInsertValuesPreservesPerRowParameterInferenceOrder(t *testing.T) {
	snapshot := indexClassSnapshot(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	wide := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	spec.Table.ColumnIDHighWatermark = 2
	spec.Table.Columns = append(spec.Table.Columns, model.ColumnDescriptor{ID: 2, Name: "wide", State: model.StatePublic, Layout: wide})
	snapshot.descriptors[1] = testDescriptor(t, spec)
	sameRow := `INSERT INTO public."Mixed" ("X",wide) VALUES ($1,$1)`
	_, err = bindSQL(t, sameRow, snapshot, nil)
	requireCode(t, err, "42P08")
	declared := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	if _, err := bindSQL(t, sameRow, snapshot, []model.TypeLayout{declared}); err != nil {
		t.Fatal(err)
	}
	bound, err := bindSQL(t, `INSERT INTO public."Mixed" ("X",wide) VALUES (1,$1),($1,2)`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := bound.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != 20 {
		t.Fatalf("prior row parameter inference was lost: %+v", parameters)
	}
}
