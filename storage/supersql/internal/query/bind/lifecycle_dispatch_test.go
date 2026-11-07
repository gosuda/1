package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

var lifecycleInteger = model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}

func bindLifecycleCommand(t *testing.T, sql string, snapshot metadataSnapshot) (*bind.BoundObjectCommand, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bind.CreateDeclarationOptions{
		Bind:        bindingOptions(sql),
		Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		Identities:  &declarationIdentities{next: 100},
		Owner:       (model.Principal{}).OID(),
	}
	command, handled, err := bind.BindObjectCommand(t.Context(), parsed.Stmts[0].Stmt, snapshot, options)
	if err != nil {
		return nil, err
	}
	if !handled {
		t.Fatal("utility lifecycle statement was not routed")
	}
	return command, nil
}

// lifecycleSchema returns the owning namespace every fixture table lives in.
func lifecycleSchema(t *testing.T) *model.Descriptor {
	t.Helper()
	return testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}})
}

// lifecycleCheck is one inheritable CHECK constraint; parentTable is nonzero
// only for a copy inherited from that table's parentConstraint.
func lifecycleCheck(id model.ObjectID, oid model.OID, name string, parentTable, parentConstraint model.ObjectID, inheritanceCount uint32) model.ConstraintDescriptor {
	constraint := model.ConstraintDescriptor{
		ID: id, OID: oid, Name: name, Kind: model.ConstraintCheck,
		Columns: []model.ColumnID{1}, Validated: true, Enforced: true,
		Expression: &model.ExpressionDefinition{SQL: "value > 0", SemanticVersion: 1, Encoding: []byte{1}},
	}
	if parentTable != 0 {
		constraint.ParentTable = parentTable
		constraint.ParentConstraint = parentConstraint
		constraint.InheritanceCount = inheritanceCount
	}
	return constraint
}

func lifecycleTable(t *testing.T, id model.ObjectID, oid model.OID, name string, parents []model.ObjectID, children uint32, constraints []model.ConstraintDescriptor, indexes []model.ObjectID) *model.Descriptor {
	t.Helper()
	return testDescriptor(t, model.DescriptorSpec{
		ID: id, OID: oid, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: name},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			Owner:                 10,
			ColumnIDHighWatermark: 1,
			Columns:               []model.ColumnDescriptor{{ID: 1, Name: "value", State: model.StatePublic, Layout: lifecycleInteger}},
			Parents:               parents,
			InheritanceChildren:   children,
			Constraints:           constraints,
			Indexes:               indexes,
		},
	})
}

func renamedConstraintName(t *testing.T, update bind.ObjectMutation, tableID model.ObjectID) string {
	t.Helper()
	spec, err := update.Next.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != tableID {
		t.Fatalf("update for table %d, want %d", spec.ID, tableID)
	}
	return spec.Table.Constraints[0].Name
}

func renamedTableIDs(t *testing.T, command *bind.BoundObjectCommand) map[model.ObjectID]string {
	t.Helper()
	renamed := make(map[model.ObjectID]string)
	for _, update := range command.Updates() {
		spec, err := update.Next.Spec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if spec.Table == nil {
			continue
		}
		renamed[spec.ID] = spec.Table.Constraints[0].Name
	}
	return renamed
}

// TestRenameTableConstraintPropagatesThroughRealLineage pins that a recursive
// rename follows each descendant's own parent edge: a grandchild records its
// immediate parent's constraint, not the root's, so matching on the root
// identity alone would silently skip it.
func TestRenameTableConstraintPropagatesThroughRealLineage(t *testing.T) {
	schema := lifecycleSchema(t)
	root := lifecycleTable(t, 11, 10011, "chain_root", nil, 1,
		[]model.ConstraintDescriptor{lifecycleCheck(21, 20021, "con1", 0, 0, 0)}, nil)
	child := lifecycleTable(t, 12, 10012, "chain_child", []model.ObjectID{11}, 1,
		[]model.ConstraintDescriptor{lifecycleCheck(22, 20022, "con1", 11, 21, 1)}, nil)
	grandchild := lifecycleTable(t, 13, 10013, "chain_grandchild", []model.ObjectID{12}, 0,
		[]model.ConstraintDescriptor{lifecycleCheck(23, 20023, "con1", 12, 22, 1)}, nil)
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{schema, root, child, grandchild}}

	command, err := bindLifecycleCommand(t, `ALTER TABLE chain_root RENAME CONSTRAINT con1 TO con1foo`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	renamed := renamedTableIDs(t, command)
	for _, id := range []model.ObjectID{11, 12, 13} {
		if renamed[id] != "con1foo" {
			t.Fatalf("table %d constraint = %q, want con1foo (all=%v)", id, renamed[id], renamed)
		}
	}
}

// TestRenameTableConstraintAccountsForSharedParents pins the inherited-count
// guard: a descendant reached through several parents inside the hierarchy is
// renamed, while one that also inherits from outside it is refused.
func TestRenameTableConstraintAccountsForSharedParents(t *testing.T) {
	schema := lifecycleSchema(t)
	root := lifecycleTable(t, 11, 10011, "diamond_root", nil, 2, []model.ConstraintDescriptor{lifecycleCheck(21, 20021, "con1", 0, 0, 0)}, nil)
	left := lifecycleTable(t, 12, 10012, "diamond_left", []model.ObjectID{11}, 1, []model.ConstraintDescriptor{lifecycleCheck(22, 20022, "con1", 11, 21, 1)}, nil)
	right := lifecycleTable(t, 13, 10013, "diamond_right", []model.ObjectID{11}, 0, []model.ConstraintDescriptor{lifecycleCheck(23, 20023, "con1", 11, 21, 1)}, nil)
	shared := lifecycleTable(t, 14, 10014, "diamond_shared", []model.ObjectID{12, 13}, 0, []model.ConstraintDescriptor{lifecycleCheck(24, 20024, "con1", 12, 22, 2)}, nil)
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{schema, root, left, right, shared}}

	command, err := bindLifecycleCommand(t, `ALTER TABLE diamond_root RENAME CONSTRAINT con1 TO con1foo`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	renamed := renamedTableIDs(t, command)
	if len(renamed) != 4 || renamed[14] != "con1foo" {
		t.Fatalf("shared descendant was not renamed: %v", renamed)
	}

	outside := lifecycleTable(t, 15, 10015, "diamond_outside", nil, 1, []model.ConstraintDescriptor{lifecycleCheck(25, 20025, "con1", 0, 0, 0)}, nil)
	mixed := lifecycleTable(t, 16, 10016, "diamond_mixed", []model.ObjectID{12, 15}, 0, []model.ConstraintDescriptor{lifecycleCheck(26, 20026, "con1", 12, 22, 2)}, nil)
	leftWithMixed := lifecycleTable(t, 12, 10012, "diamond_left", []model.ObjectID{11}, 2, []model.ConstraintDescriptor{lifecycleCheck(22, 20022, "con1", 11, 21, 1)}, nil)
	snapshot = metadataSnapshot{descriptors: []*model.Descriptor{schema, root, leftWithMixed, right, shared, outside, mixed}}

	_, err = bindLifecycleCommand(t, `ALTER TABLE diamond_root RENAME CONSTRAINT con1 TO con1foo`, snapshot)
	requireCode(t, err, "42P16")
}

// TestRenameTableConstraintRespectsOnlyAndInheritedCopy pins the two refusals
// PostgreSQL raises before any change: ONLY while children still carry the
// constraint, and a rename targeting an inherited copy.
func TestRenameTableConstraintRespectsOnlyAndInheritedCopy(t *testing.T) {
	schema := lifecycleSchema(t)
	root := lifecycleTable(t, 11, 10011, "only_root", nil, 1, []model.ConstraintDescriptor{lifecycleCheck(21, 20021, "con1", 0, 0, 0)}, nil)
	child := lifecycleTable(t, 12, 10012, "only_child", []model.ObjectID{11}, 0, []model.ConstraintDescriptor{lifecycleCheck(22, 20022, "con1", 11, 21, 1)}, nil)
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{schema, root, child}}

	_, err := bindLifecycleCommand(t, `ALTER TABLE ONLY only_root RENAME CONSTRAINT con1 TO con1foo`, snapshot)
	requireCode(t, err, "42P16")

	_, err = bindLifecycleCommand(t, `ALTER TABLE only_child RENAME CONSTRAINT con1 TO con1foo`, snapshot)
	requireCode(t, err, "42P16")

	// NO INHERIT constraints are local to the table, so ONLY is accepted.
	noInherit := lifecycleTable(t, 13, 10013, "only_local", nil, 1, []model.ConstraintDescriptor{
		{ID: 27, OID: 20027, Name: "con2", Kind: model.ConstraintCheck, Columns: []model.ColumnID{1}, Validated: true, Enforced: true, NoInherit: true, Expression: &model.ExpressionDefinition{SQL: "value > 1", SemanticVersion: 1, Encoding: []byte{1}}},
	}, nil)
	snapshot = metadataSnapshot{descriptors: []*model.Descriptor{schema, noInherit}}
	command, err := bindLifecycleCommand(t, `ALTER TABLE ONLY only_local RENAME CONSTRAINT con2 TO con2foo`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if name := renamedConstraintName(t, command.Updates()[0], 13); name != "con2foo" {
		t.Fatalf("NO INHERIT constraint = %q", name)
	}
}

// TestRenameTableConstraintMissingOkSkips pins the corpus behavior for
// ALTER TABLE IF EXISTS on a missing relation: a warning instead of an error,
// and no staged mutation.
func TestRenameTableConstraintMissingOkSkips(t *testing.T) {
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{lifecycleSchema(t)}}
	command, err := bindLifecycleCommand(t, `ALTER TABLE IF EXISTS absent_relation RENAME CONSTRAINT con3 TO con3foo`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(command.Updates()) != 0 {
		t.Fatalf("updates = %d, want none", len(command.Updates()))
	}
	warnings := command.Warnings()
	if len(warnings) != 1 || warnings[0].Code != "42P01" {
		t.Fatalf("warnings = %#v", warnings)
	}
}

// TestRenameTableConstraintRenamesOwningIndex pins that renaming an
// index-backed constraint renames the index carrying it.
func TestRenameTableConstraintRenamesOwningIndex(t *testing.T) {
	schema := lifecycleSchema(t)
	keyType := lifecycleInteger.Type
	constraint := model.ConstraintDescriptor{ID: 21, OID: 20021, Name: "pk_constraint", Kind: model.ConstraintPrimaryKey, Columns: []model.ColumnID{1}, Index: 31, Validated: true, Enforced: true}
	table := lifecycleTable(t, 11, 10011, "indexed", nil, 0, []model.ConstraintDescriptor{constraint}, []model.ObjectID{31})
	index := testDescriptor(t, model.DescriptorSpec{
		ID: 31, OID: 20031, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: "pk_constraint"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index:    &model.IndexMetadata{Owner: 10, Table: 11, Keys: []model.KeyElement{{Column: 1, Type: keyType}}, Unique: true},
	})
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{schema, table, index}}

	command, err := bindLifecycleCommand(t, `ALTER TABLE indexed RENAME CONSTRAINT pk_constraint TO pk_new`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var renamedIndex bool
	for _, update := range command.Updates() {
		spec, specErr := update.Next.Spec(t.Context())
		if specErr != nil {
			t.Fatal(specErr)
		}
		if spec.Index != nil && spec.Name.Kind == model.ObjectIndex && spec.Name.Name == "pk_new" {
			renamedIndex = true
		}
	}
	if !renamedIndex {
		t.Fatal("owning index was not renamed with its constraint")
	}
}

// TestTextSearchLifecycleStagesRealDescriptorMutations verifies rename and
// schema move mutate the persisted descriptor identity rather than a shim.
func TestTextSearchLifecycleStagesRealDescriptorMutations(t *testing.T) {
	public := testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}})
	alternate := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 10012, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "alternate"}, Schema: &model.SchemaMetadata{Owner: 10}})
	dictionary := testDescriptor(t, model.DescriptorSpec{
		ID: 41, OID: 20041, Version: 1, State: model.StatePublic,
		Name:                 model.NamespaceName{Parent: 10, Kind: model.ObjectTextSearchDictionary, Name: "snowball"},
		TextSearchDictionary: &model.TextSearchDictionaryMetadata{Owner: 10, Template: 3727, Kind: model.TextSearchSimpleDictionary, Accept: true},
	})
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{public, alternate, dictionary}}

	renamed, err := bindLifecycleCommand(t, `ALTER TEXT SEARCH DICTIONARY public.snowball RENAME TO snowball_two`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if spec, specErr := renamed.Updates()[0].Next.Spec(t.Context()); specErr != nil || spec.Name.Name != "snowball_two" || spec.TextSearchDictionary == nil {
		t.Fatalf("renamed dictionary = %#v err=%v", spec.Name, specErr)
	}

	moved, err := bindLifecycleCommand(t, `ALTER TEXT SEARCH DICTIONARY public.snowball SET SCHEMA alternate`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := moved.Updates()[0].Next.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name.Parent != alternate.ID() || spec.TextSearchDictionary == nil {
		t.Fatalf("moved dictionary parent = %d", spec.Name.Parent)
	}
	if !hasDependency(spec.Dependencies, alternate.ID()) {
		t.Fatalf("moved dictionary lost destination schema dependency: %#v", spec.Dependencies)
	}
}

func hasDependency(dependencies []model.Dependency, object model.ObjectID) bool {
	for _, dependency := range dependencies {
		if dependency.Object == object {
			return true
		}
	}
	return false
}
