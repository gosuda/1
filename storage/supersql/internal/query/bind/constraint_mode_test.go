package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

func TestSetConstraintsResolvesQuotedSchemaMatchesToStableIDs(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	makeTable := func(tableID, indexID, constraintID model.ObjectID, tableName string) (*model.Descriptor, *model.Descriptor) {
		index := testDescriptor(t, model.DescriptorSpec{
			ID: indexID, OID: model.OID(90000 + indexID), Version: 1, State: model.StatePublic,
			Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: tableName + "_key"},
			Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1},
			Index:    &model.IndexMetadata{Owner: 10, Table: tableID, Keys: []model.KeyElement{{Column: 1, Type: integer.Type}}, Unique: true, DeferredUnique: true},
		})
		table := testDescriptor(t, model.DescriptorSpec{
			ID: tableID, OID: model.OID(80000 + tableID), Version: 3, State: model.StatePublic,
			Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: tableName},
			Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1},
			Table: &model.TableMetadata{
				ColumnIDHighWatermark: 1,
				Columns:               []model.ColumnDescriptor{{ID: 1, Name: "value", State: model.StatePublic, Layout: integer}},
				Indexes:               []model.ObjectID{indexID},
				Constraints:           []model.ConstraintDescriptor{{ID: constraintID, OID: model.OID(70000 + constraintID), Name: "CaseSensitive", Kind: model.ConstraintUnique, Columns: []model.ColumnID{1}, Index: indexID, Deferrable: true, InitiallyDeferred: true, Enforced: true, Validated: true}},
			},
		})
		return table, index
	}
	first, firstIndex := makeTable(11, 21, 31, "first")
	second, secondIndex := makeTable(12, 22, 32, "second")
	snapshot.descriptors = []*model.Descriptor{snapshot.descriptors[0], first, firstIndex, second, secondIndex}
	statement, err := bindSQL(t, `SET CONSTRAINTS public."CaseSensitive" IMMEDIATE`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := statement.Command().(bind.ConstraintModeCommand)
	if !ok || command.Deferred || len(command.Names) != 2 {
		t.Fatalf("constraint mode command = %#v", statement.Command())
	}
	if command.Names[0].Object != 11 || command.Names[0].Constraint != 31 || command.Names[0].Schema != "public" || command.Names[0].Name != "CaseSensitive" || command.Names[1].Object != 12 || command.Names[1].Constraint != 32 {
		t.Fatalf("resolved constraints = %#v", command.Names)
	}
}

func TestSetConstraintsNondeferrableNamedMode(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	index := testDescriptor(t, model.DescriptorSpec{
		ID: 21, OID: 90021, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: "fixed_key"},
		Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1},
		Index:    &model.IndexMetadata{Owner: 10, Table: 11, Keys: []model.KeyElement{{Column: 1, Type: integer.Type}}, Unique: true},
	})
	table := testDescriptor(t, model.DescriptorSpec{
		ID: 11, OID: 80011, Version: 3, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "fixed"},
		Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1},
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 1,
			Columns:               []model.ColumnDescriptor{{ID: 1, Name: "value", State: model.StatePublic, Layout: integer}},
			Indexes:               []model.ObjectID{21},
			Constraints:           []model.ConstraintDescriptor{{ID: 31, OID: 70031, Name: "fixed_key", Kind: model.ConstraintUnique, Columns: []model.ColumnID{1}, Index: 21, Enforced: true, Validated: true}},
		},
	})
	snapshot.descriptors = []*model.Descriptor{snapshot.descriptors[0], table, index}
	statement, err := bindSQL(t, `SET CONSTRAINTS fixed_key IMMEDIATE`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := statement.Command().(bind.ConstraintModeCommand)
	if !ok || command.All || command.Deferred || len(command.Names) != 1 || command.Names[0].Constraint != 31 {
		t.Fatalf("constraint mode command = %#v", statement.Command())
	}
	_, err = bindSQL(t, `SET CONSTRAINTS fixed_key DEFERRED`, snapshot, nil)
	requireCode(t, err, "55000")
}
