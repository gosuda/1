package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func triggerAlterSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	column := model.ColumnDescriptor{ID: 1, Name: "id", State: model.StatePublic, Layout: integer}
	trigger := func(id model.ObjectID, name string, internal bool) model.TriggerDescriptor {
		return model.TriggerDescriptor{ID: id, OID: model.OID(30000 + id), Name: name, SchemaName: "public", Function: model.FunctionID(40000 + id), Timing: "after", Events: []string{"insert"}, ForEachRow: true, EnableMode: model.TriggerEnabledOrigin, Internal: internal}
	}
	schema := testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}})
	parent := testDescriptor(t, model.DescriptorSpec{ID: 11, OID: 10011, Version: 3, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "parent"}, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{Owner: 10, ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{column}, Triggers: []model.TriggerDescriptor{trigger(21, "audit", false), trigger(22, "constraint_internal", true)}, InheritanceChildren: 1}})
	childColumn := column
	childColumn.InheritedFrom = []model.ColumnInheritance{{Table: 11, Column: 1}}
	child := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 10012, Version: 2, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "child"}, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{Owner: 10, ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{childColumn}, Triggers: []model.TriggerDescriptor{trigger(23, "audit", false)}, Parents: []model.ObjectID{11}}})
	return metadataSnapshot{descriptors: []*model.Descriptor{schema, parent, child}}
}
func bindTriggerAlter(t *testing.T, snapshot metadataSnapshot, sql string) (*bind.BoundObjectCommand, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	command, handled, err := bind.BindObjectCommand(t.Context(), parsed.Stmts[0].Stmt, snapshot, bind.CreateDeclarationOptions{Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}, Owner: 10})
	if !handled {
		t.Fatal("trigger state ALTER was not handled")
	}
	return command, err
}

func triggerMode(t *testing.T, descriptor *model.Descriptor, name string) model.TriggerEnableMode {
	t.Helper()
	metadata, ok := descriptor.Table()
	if !ok {
		t.Fatal("updated descriptor is not a table")
	}
	for _, trigger := range metadata.Triggers {
		if trigger.Name == name {
			return trigger.EnableMode
		}
	}
	t.Fatalf("trigger %s is missing", name)
	return 0
}

func TestAlterTriggerStateModes(t *testing.T) {
	for _, scenario := range []struct {
		sql  string
		mode model.TriggerEnableMode
	}{
		{`ALTER TABLE ONLY public.parent DISABLE TRIGGER audit`, model.TriggerDisabled},
		{`ALTER TABLE ONLY public.parent ENABLE TRIGGER audit`, model.TriggerEnabledOrigin},
		{`ALTER TABLE ONLY public.parent ENABLE REPLICA TRIGGER audit`, model.TriggerEnabledReplica},
		{`ALTER TABLE ONLY public.parent ENABLE ALWAYS TRIGGER audit`, model.TriggerEnabledAlways},
	} {
		command, err := bindTriggerAlter(t, triggerAlterSnapshot(t), scenario.sql)
		if err != nil {
			t.Fatalf("%s: %v", scenario.sql, err)
		}
		updates := command.Updates()
		if len(updates) != 1 || triggerMode(t, updates[0].Next, "audit") != scenario.mode {
			t.Fatalf("%s produced %#v", scenario.sql, updates)
		}
	}
}

func TestAlterTriggerStateRecursesUnlessOnly(t *testing.T) {
	command, err := bindTriggerAlter(t, triggerAlterSnapshot(t), `ALTER TABLE public.parent DISABLE TRIGGER audit`)
	if err != nil {
		t.Fatal(err)
	}
	updates := command.Updates()
	if len(updates) != 2 || updates[0].Base.ID() != 11 || updates[1].Base.ID() != 12 || triggerMode(t, updates[0].Next, "audit") != model.TriggerDisabled || triggerMode(t, updates[1].Next, "audit") != model.TriggerDisabled {
		t.Fatalf("recursive updates = %#v", updates)
	}
}

func TestAlterTriggerUserSkipsInternalAndAllRejectsIt(t *testing.T) {
	command, err := bindTriggerAlter(t, triggerAlterSnapshot(t), `ALTER TABLE ONLY public.parent DISABLE TRIGGER USER`)
	if err != nil {
		t.Fatal(err)
	}
	updated := command.Updates()[0].Next
	if triggerMode(t, updated, "audit") != model.TriggerDisabled || triggerMode(t, updated, "constraint_internal") != model.TriggerEnabledOrigin {
		t.Fatal("USER changed an internal trigger or retained a user trigger")
	}
	_, err = bindTriggerAlter(t, triggerAlterSnapshot(t), `ALTER TABLE ONLY public.parent DISABLE TRIGGER ALL`)
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "42501" {
		t.Fatalf("ALL internal trigger error = %v", err)
	}
}
