package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestInsertSelectInfersUnconstrainedParameterFromColumn(t *testing.T) {
	bound, err := bindSQL(t, `INSERT INTO public."Mixed" ("X") SELECT $1`, indexClassSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := bound.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != 23 {
		t.Fatalf("INSERT parameter types: %+v", parameters)
	}
}

func TestInsertContextDoesNotLeakIntoScalarSubquery(t *testing.T) {
	bound, err := bindSQL(t, `INSERT INTO public."Mixed" ("X") SELECT length((SELECT $1))`, indexClassSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := bound.Parameters()
	if len(parameters) != 1 || parameters[0].Type.ID != 25 {
		t.Fatalf("nested SELECT parameter types: %+v", parameters)
	}
}

func TestInsertPreservesSelectComparisonTyping(t *testing.T) {
	snapshot := indexClassSnapshot(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	typ := model.Type{ID: 1043, Modifier: -1, Storage: model.TypeString, Collation: 950}
	spec.Table.Columns[0].Layout = model.TypeLayout{Type: typ}
	spec.Table.PrimaryKey[0].Type = typ
	spec.Table.PrimaryKey[0].Collation = typ.Collation
	snapshot.descriptors[1] = testDescriptor(t, spec)
	for _, statement := range []struct {
		sql    string
		typeID model.TypeID
	}{
		{`INSERT INTO public."Mixed" ("X") SELECT $1`, 1043},
		{`INSERT INTO public."Mixed" ("X") SELECT $1 ORDER BY 1`, 25},
		{`INSERT INTO public."Mixed" ("X") SELECT DISTINCT $1`, 25},
	} {
		t.Run(statement.sql, func(t *testing.T) {
			bound, err := bindSQL(t, statement.sql, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			parameters := bound.Parameters()
			if len(parameters) != 1 || parameters[0].Type.ID != statement.typeID {
				t.Fatalf("parameter types %+v, want %d", parameters, statement.typeID)
			}
		})
	}
}
