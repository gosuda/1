package bind_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func identityDMLSnapshot(t *testing.T, always bool) metadataSnapshot {
	t.Helper()
	snapshot := sequenceSnapshot(t)
	table, err := snapshot.descriptors[1].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sequenceIndex := len(snapshot.descriptors) - 1
	sequence, err := snapshot.descriptors[sequenceIndex].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	column := &table.Table.Columns[0]
	column.IdentitySequence = sequence.ID
	column.IdentityAlways = always
	table.Table.PrimaryKey = []model.KeyElement{{Column: column.ID, Type: column.Layout.Type}}
	table.Table.ColumnIDHighWatermark = 2
	table.Table.Columns = append(table.Table.Columns, model.ColumnDescriptor{ID: 2, Name: "payload", State: model.StatePublic, Layout: column.Layout, Nullable: true})
	sequence.Sequence.Type = column.Layout.Type
	snapshot.descriptors[1] = testDescriptor(t, table)
	snapshot.descriptors[sequenceIndex] = testDescriptor(t, sequence)
	return snapshot
}

func TestIdentityAlwaysRejectsExplicitAssignments(t *testing.T) {
	snapshot := identityDMLSnapshot(t, true)
	for _, sql := range []string{
		`INSERT INTO "Mixed" ("X") VALUES (1)`,
		`INSERT INTO "Mixed" ("X") VALUES (DEFAULT),(NULL)`,
		`INSERT INTO "Mixed" ("X") SELECT 1`,
		`UPDATE "Mixed" SET "X"=NULL`,
		`INSERT INTO "Mixed" ("X") VALUES (DEFAULT) ON CONFLICT ("X") DO UPDATE SET "X"=EXCLUDED."X"`,
		`MERGE INTO "Mixed" t USING (VALUES(1)) s(x) ON t."X"=s.x WHEN MATCHED THEN UPDATE SET "X"=s.x`,
		`MERGE INTO "Mixed" t USING (VALUES(1)) s(x) ON t."X"=s.x WHEN NOT MATCHED THEN INSERT ("X") VALUES (s.x)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, snapshot, nil)
			requireCode(t, err, "428C9")
		})
	}
}

func TestIdentityGenerationTracksOwnedSequenceDependency(t *testing.T) {
	for _, always := range []bool{false, true} {
		snapshot := identityDMLSnapshot(t, always)
		sequence := snapshot.descriptors[len(snapshot.descriptors)-1]
		for _, sql := range []string{
			`INSERT INTO "Mixed" DEFAULT VALUES`,
			`INSERT INTO "Mixed" (payload) VALUES (1)`,
			`INSERT INTO "Mixed" ("X") VALUES (DEFAULT),(DEFAULT)`,
			`INSERT INTO "Mixed" ("X") OVERRIDING SYSTEM VALUE VALUES (DEFAULT),(9),(NULL)`,
			`INSERT INTO "Mixed" ("X") OVERRIDING USER VALUE VALUES (DEFAULT),(9),(NULL)`,
			`INSERT INTO "Mixed" ("X") OVERRIDING USER VALUE SELECT 9`,
			`UPDATE "Mixed" SET "X"=DEFAULT`,
			`INSERT INTO "Mixed" ("X") VALUES (DEFAULT) ON CONFLICT ("X") DO UPDATE SET "X"=DEFAULT`,
			`MERGE INTO "Mixed" t USING (VALUES(1)) s(x) ON t."X"=s.x WHEN MATCHED THEN UPDATE SET "X"=DEFAULT WHEN NOT MATCHED THEN INSERT ("X") OVERRIDING USER VALUE VALUES (s.x)`,
			`MERGE INTO "Mixed" t USING (VALUES(1)) s(x) ON t."X"=s.x WHEN NOT MATCHED THEN INSERT ("X") OVERRIDING SYSTEM VALUE VALUES (s.x)`,
		} {
			t.Run(sql, func(t *testing.T) {
				bound, err := bindSQL(t, sql, snapshot, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(bound.Dependencies(), model.Dependency{Object: sequence.ID(), Version: sequence.Version()}) {
					t.Fatal("identity generation omitted its owned sequence dependency")
				}
			})
		}
	}
}

func TestIdentityOverrideStillChecksInputTypes(t *testing.T) {
	snapshot := identityDMLSnapshot(t, true)
	for _, sql := range []string{
		`INSERT INTO "Mixed" ("X") OVERRIDING USER VALUE VALUES (true)`,
		`INSERT INTO "Mixed" ("X") OVERRIDING USER VALUE SELECT true`,
		`INSERT INTO "Mixed" ("X") VALUES (true)`,
		`UPDATE "Mixed" SET "X"=true`,
		`UPDATE "Mixed" SET "X"=1,payload=true`,
		`MERGE INTO "Mixed" t USING (VALUES(1)) s(x) ON t."X"=s.x WHEN NOT MATCHED THEN INSERT ("X") OVERRIDING USER VALUE VALUES (true)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, snapshot, nil)
			requireCode(t, err, "42804")
		})
	}
}

func TestIdentityRejectsMismatchedSequenceMetadata(t *testing.T) {
	for _, change := range []string{"owner table", "owner column"} {
		t.Run(change, func(t *testing.T) {
			snapshot := identityDMLSnapshot(t, true)
			index := len(snapshot.descriptors) - 1
			spec, err := snapshot.descriptors[index].Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "owner table":
				spec.Sequence.OwnerTable++
			case "owner column":
				spec.Sequence.OwnerColumn++
			}
			snapshot.descriptors[index] = testDescriptor(t, spec)
			_, err = bindSQL(t, `INSERT INTO "Mixed" DEFAULT VALUES`, snapshot, nil)
			if !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("mismatched %s: got %v, want invalid descriptor", change, err)
			}
		})
	}
}

func TestIdentityAllowsIndependentIntegerSequenceType(t *testing.T) {
	snapshot := identityDMLSnapshot(t, false)
	index := len(snapshot.descriptors) - 1
	spec, err := snapshot.descriptors[index].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Sequence.Type.ID = 20
	snapshot.descriptors[index] = testDescriptor(t, spec)
	bound, err := bindSQL(t, `INSERT INTO "Mixed" ("X") VALUES (DEFAULT),(9),(NULL)`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(bound.Dependencies(), model.Dependency{Object: spec.ID, Version: spec.Version}) {
		t.Fatal("independently typed sequence lost its identity dependency")
	}
}

func TestIdentityOverridePreservesParameterInference(t *testing.T) {
	snapshot := identityDMLSnapshot(t, true)
	for _, source := range []string{`VALUES ($1)`, `VALUES ($1),(DEFAULT)`, `SELECT $1`} {
		sql := `INSERT INTO "Mixed" ("X") OVERRIDING USER VALUE ` + source
		bound, err := bindSQL(t, sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		parameters := bound.Parameters()
		if len(parameters) != 1 || parameters[0].Type.ID != 23 {
			t.Fatalf("%s: overridden source parameter types = %+v, want integer", source, parameters)
		}
	}
}
