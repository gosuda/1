package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestInsertSelectPreservesParameterInferenceConflicts(t *testing.T) {
	snapshot := indexClassSnapshot(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	integer := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	spec.Table.Columns[0].Layout = integer
	spec.Table.PrimaryKey[0].Type = integer.Type
	snapshot.descriptors[1] = testDescriptor(t, spec)
	sql := `INSERT INTO public."Mixed" ("X") SELECT $1 ORDER BY $1+1`
	_, err = bindSQL(t, sql, snapshot, nil)
	requireCode(t, err, "42P08")
	declared := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	bound, err := bindSQL(t, sql, snapshot, []model.TypeLayout{declared})
	if err != nil {
		t.Fatal(err)
	}
	parameters := bound.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != declared.Type.ID {
		t.Fatalf("declared parameter type changed: %+v", parameters)
	}
}
