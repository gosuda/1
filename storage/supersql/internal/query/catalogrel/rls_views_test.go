package catalogrel

import (
	"context"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type testRoleSettings struct {
	role string
}

func (s testRoleSettings) CatalogSettings(context.Context) ([]Setting, error) { return nil, nil }
func (s testRoleSettings) CurrentRole() string                                 { return s.role }

type testStatsReader struct {
	ts     model.Timestamp
	tables map[model.ObjectID]*statistics.Table
}

func (r testStatsReader) Timestamp() model.Timestamp { return r.ts }
func (r testStatsReader) Lookup(_ context.Context, ref storage.ObjectRef) (*statistics.Table, bool, error) {
	tbl, ok := r.tables[ref.ObjectID]
	return tbl, ok, nil
}

type testStatsFactory struct {
	reader Statistics
}

func (f testStatsFactory) OpenCatalogStatistics(context.Context, ReadView) (Statistics, error) {
	return f.reader, nil
}

type testReadView struct {
	storage.SnapshotReader
	ts model.Timestamp
}

func (v testReadView) Timestamp() model.Timestamp { return v.ts }

func TestCatalogRelPgClassPgRolesPgPoliciesAndPgStatsRLSMasking(t *testing.T) {
	manifest := nativeCatalogFixture(t)
	bootstrap, err := NewBootstrapRegistry(manifest.RegistryConfig())
	if err != nil {
		t.Fatal(err)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 128, MaxDepth: 16}
	mustDesc := func(spec model.DescriptorSpec) *model.Descriptor {
		t.Helper()
		d, err := model.NewDescriptor(t.Context(), spec, limits)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	const (
		publicSchemaID  model.ObjectID = 910000
		publicSchemaOID model.OID      = 910000
		plainTableID    model.ObjectID = 910001
		plainTableOID   model.OID      = 910001
		plainRowTypeID  model.ObjectID = 910002
		plainRowTypeOID model.OID      = 910002
		rlsTableID      model.ObjectID = 910003
		rlsTableOID     model.OID      = 910003
		rlsRowTypeID    model.ObjectID = 910004
		rlsRowTypeOID   model.OID      = 910004
		forceTableID    model.ObjectID = 910005
		forceTableOID   model.OID      = 910005
		forceRowTypeID  model.ObjectID = 910006
		forceRowTypeOID model.OID      = 910006
		roleAliceID     model.ObjectID = 910010
		roleAliceOID    model.OID      = 910010
		roleAliceSubID  model.ObjectID = 910011
		roleAliceSubOID model.OID      = 910011
		roleBobID       model.ObjectID = 910012
		roleBobOID      model.OID      = 910012
		roleBypassID    model.ObjectID = 910013
		roleBypassOID   model.OID      = 910013
		roleSuperID     model.ObjectID = 910014
		roleSuperOID    model.OID      = 910014
	)

	intLayout := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	makeRowType := func(id model.ObjectID, oid model.OID, relID model.ObjectID, name string) *model.Descriptor {
		compLayout := model.TypeLayout{
			Type:       model.Type{ID: model.TypeID(oid), Storage: model.TypeComposite, Modifier: -1},
			FieldNames: []string{"id", "val"},
			Children:   []model.TypeLayout{intLayout, intLayout},
		}
		return mustDesc(model.DescriptorSpec{
			ID: id, OID: oid, Version: 1, State: model.StatePublic,
			Name:         model.NamespaceName{Parent: publicSchemaID, Kind: model.ObjectType, Name: name},
			Dependencies: []model.Dependency{{Object: relID, Version: 1}},
			Type: &model.TypeMetadata{
				Kind: model.TypeKindComposite, Owner: 10, Layout: compLayout,
				Array: id + 1000, Relation: relID, FieldIDHighWatermark: 2,
				Fields: []model.ColumnDescriptor{
					{ID: 1, Name: "id", State: model.StatePublic, Layout: intLayout},
					{ID: 2, Name: "val", State: model.StatePublic, Layout: intLayout},
				},
			},
		})
	}
	const (
		plainExtStatOID model.OID = 930001
		rlsExtStatOID   model.OID = 930002
		forceExtStatOID model.OID = 930003
	)
	makeTable := func(id model.ObjectID, oid model.OID, rowTypeID model.ObjectID, name, ownerName string, rls, force bool, policies []model.PolicyDescriptor, acl []model.PrivilegeGrant, extOID model.OID) *model.Descriptor {
		return mustDesc(model.DescriptorSpec{
			ID: id, OID: oid, Version: 1, State: model.StatePublic,
			Name:     model.NamespaceName{Parent: publicSchemaID, Kind: model.ObjectTable, Name: name},
			Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
			Table: &model.TableMetadata{
				Owner: 10, OwnerName: ownerName, RowType: rowTypeID, ColumnIDHighWatermark: 2,
				Columns: []model.ColumnDescriptor{
					{ID: 1, Name: "id", State: model.StatePublic, Layout: intLayout},
					{ID: 2, Name: "val", State: model.StatePublic, Layout: intLayout},
				},
				RowSecurity:      rls,
				ForceRowSecurity: force,
				Policies:         policies,
				ACL:              acl,
				ExtendedStatistics: []model.ExtendedStatisticDescriptor{{
					OID: extOID, Name: name + "_ext", Namespace: publicSchemaID, Owner: 10,
					Keys: []model.ColumnID{1, 2}, Kinds: []byte{'d', 'm'},
				}},
			},
		})
	}

	policies := []model.PolicyDescriptor{
		{
			ID: 920001, OID: 920001, Name: "allow_public_select", Permissive: true, Command: "SELECT",
			Using: &model.ExpressionDefinition{SQL: "id > 0", SemanticVersion: 1, Encoding: []byte{1}},
		},
		{
			ID: 920002, OID: 920002, Name: "restrict_bob_write", Permissive: false, Command: "INSERT",
			Roles:     []string{"bob"},
			WithCheck: &model.ExpressionDefinition{SQL: "id < 100", SemanticVersion: 1, Encoding: []byte{2}},
		},
	}

	snapshot := nativeTypeCatalogSnapshot{descriptors: []*model.Descriptor{
		mustDesc(model.DescriptorSpec{
			ID: manifest.InformationSchema.ObjectID, OID: manifest.InformationSchema.OID, Version: 1, State: model.StatePublic,
			Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "information_schema"},
			Schema: &model.SchemaMetadata{Owner: 10},
		}),
		mustDesc(model.DescriptorSpec{
			ID: publicSchemaID, OID: publicSchemaOID, Version: 1, State: model.StatePublic,
			Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
			Schema: &model.SchemaMetadata{Owner: 10},
		}),
		makeRowType(plainRowTypeID, plainRowTypeOID, plainTableID, "plain_tbl"),
		makeRowType(rlsRowTypeID, rlsRowTypeOID, rlsTableID, "rls_tbl"),
		makeRowType(forceRowTypeID, forceRowTypeOID, forceTableID, "force_tbl"),
		makeTable(plainTableID, plainTableOID, plainRowTypeID, "plain_tbl", "", false, false, nil, []model.PrivilegeGrant{
			{Grantee: "alice", Privileges: model.PrivilegeSelect},
		}, plainExtStatOID),
		makeTable(rlsTableID, rlsTableOID, rlsRowTypeID, "rls_tbl", "alice", true, false, policies, []model.PrivilegeGrant{
			{Grantee: "auditor", Privileges: model.PrivilegeSelect},
			{Grantee: "bob", Privileges: model.PrivilegeSelect},
		}, rlsExtStatOID),
		makeTable(forceTableID, forceTableOID, forceRowTypeID, "force_tbl", "alice", true, true, nil, []model.PrivilegeGrant{
			{Grantee: "auditor", Privileges: model.PrivilegeSelect},
		}, forceExtStatOID),
		mustDesc(model.DescriptorSpec{
			ID: roleAliceID, OID: roleAliceOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: "alice"},
			Role: &model.RoleMetadata{},
		}),
		mustDesc(model.DescriptorSpec{
			ID: roleAliceSubID, OID: roleAliceSubOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: "alice_member"},
			Role: &model.RoleMetadata{MemberOf: []string{"alice"}},
		}),
		mustDesc(model.DescriptorSpec{
			ID: roleBobID, OID: roleBobOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: "bob"},
			Role: &model.RoleMetadata{},
		}),
		mustDesc(model.DescriptorSpec{
			ID: roleBypassID, OID: roleBypassOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: "auditor"},
			Role: &model.RoleMetadata{BypassRLS: true},
		}),
		mustDesc(model.DescriptorSpec{
			ID: roleSuperID, OID: roleSuperOID, Version: 1, State: model.StatePublic,
			Name: model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: "admin"},
			Role: &model.RoleMetadata{Superuser: true},
		}),
	}}

	statsReader := testStatsReader{
		ts: 42,
		tables: map[model.ObjectID]*statistics.Table{
			plainTableID: {
				LiveRows: statistics.Count{Known: true, Value: 10},
				Columns:  []statistics.Column{{ID: 1, Layout: intLayout, Distinct: 10}},
				Extended: []statistics.ExtendedStatistic{{OID: plainExtStatOID, NDistinct: []byte("plain"), MCV: []byte("plain-mcv")}},
			},
			rlsTableID: {
				LiveRows: statistics.Count{Known: true, Value: 20},
				Columns:  []statistics.Column{{ID: 1, Layout: intLayout, Distinct: 20}},
				Extended: []statistics.ExtendedStatistic{{OID: rlsExtStatOID, NDistinct: []byte("rls"), MCV: []byte("rls-mcv")}},
			},
			forceTableID: {
				LiveRows: statistics.Count{Known: true, Value: 30},
				Columns:  []statistics.Column{{ID: 1, Layout: intLayout, Distinct: 30}},
				Extended: []statistics.ExtendedStatistic{{OID: forceExtStatOID, NDistinct: []byte("force"), MCV: []byte("force-mcv")}},
			},
		},
	}
	baseOptions := SourceOptions{
		Registry:           bootstrap,
		Statistics:         testStatsFactory{reader: statsReader},
		Database:           Database{RoleOID: 10, RoleName: "embedded", RoleInherit: true, RoleCanLogin: true, RoleConnectionLimit: -1},
		MaxDescriptors:     128,
		MaxDescriptorBytes: limits.MaxBytes,
		MaxDescriptorItems: limits.MaxItems,
		MaxDescriptorDepth: limits.MaxDepth,
	}
	view := testReadView{ts: 42}

	colIndex := func(rel Relation, name string) int {
		t.Helper()
		for i, col := range rel.Columns {
			if col.Name == name {
				return i
			}
		}
		t.Fatalf("%s.%s column not found", rel.Name, name)
		return -1
	}

	// 1. pg_class relrowsecurity & relforcerowsecurity.
	pgClass, ok := bootstrap.Lookup("pg_catalog", "pg_class")
	if !ok {
		t.Fatal("pg_class missing")
	}
	nameIdx := colIndex(pgClass, "relname")
	rlsIdx := colIndex(pgClass, "relrowsecurity")
	forceIdx := colIndex(pgClass, "relforcerowsecurity")
	type rlsFlags struct{ rls, force bool }
	gotClass := make(map[string]rlsFlags)
	if err := generateRows(t.Context(), snapshot, view, pgClass, baseOptions, func(row []model.Value) error {
		gotClass[row[nameIdx].StrVal] = rlsFlags{rls: row[rlsIdx].IntVal != 0, force: row[forceIdx].IntVal != 0}
		return nil
	}); err != nil {
		t.Fatalf("pg_class: %v", err)
	}
	if gotClass["plain_tbl"] != (rlsFlags{false, false}) || gotClass["rls_tbl"] != (rlsFlags{true, false}) || gotClass["force_tbl"] != (rlsFlags{true, true}) {
		t.Fatalf("pg_class RLS flags mismatch: plain=%+v rls=%+v force=%+v", gotClass["plain_tbl"], gotClass["rls_tbl"], gotClass["force_tbl"])
	}

	// 2. pg_roles includes built-in principal + catalog ObjectRole descriptors.
	pgRoles, ok := bootstrap.Lookup("pg_catalog", "pg_roles")
	if !ok {
		t.Fatal("pg_roles missing")
	}
	rolNameIdx := colIndex(pgRoles, "rolname")
	rolSuperIdx := colIndex(pgRoles, "rolsuper")
	rolBypassIdx := colIndex(pgRoles, "rolbypassrls")
	type roleSummary struct{ super, bypass bool }
	gotRoles := make(map[string]roleSummary)
	if err := generateRows(t.Context(), snapshot, view, pgRoles, baseOptions, func(row []model.Value) error {
		gotRoles[row[rolNameIdx].StrVal] = roleSummary{super: row[rolSuperIdx].IntVal != 0, bypass: row[rolBypassIdx].IntVal != 0}
		return nil
	}); err != nil {
		t.Fatalf("pg_roles: %v", err)
	}
	if _, ok := gotRoles["embedded"]; !ok {
		t.Fatalf("pg_roles missing built-in embedded role: %+v", gotRoles)
	}
	if gotRoles["admin"] != (roleSummary{true, false}) || gotRoles["auditor"] != (roleSummary{false, true}) || gotRoles["bob"] != (roleSummary{false, false}) {
		t.Fatalf("pg_roles attributes mismatch: %+v", gotRoles)
	}

	// 3. pg_policies projects TableMetadata.Policies directly.
	pgPolicies, ok := bootstrap.Lookup("pg_catalog", "pg_policies")
	if !ok {
		t.Fatal("pg_policies missing from registry")
	}
	pSchemaIdx := colIndex(pgPolicies, "schemaname")
	pTableIdx := colIndex(pgPolicies, "tablename")
	pNameIdx := colIndex(pgPolicies, "policyname")
	pPermIdx := colIndex(pgPolicies, "permissive")
	pRolesIdx := colIndex(pgPolicies, "roles")
	pCmdIdx := colIndex(pgPolicies, "cmd")
	pQualIdx := colIndex(pgPolicies, "qual")
	pCheckIdx := colIndex(pgPolicies, "with_check")
	var gotPolicies [][]model.Value
	if err := generateRows(t.Context(), snapshot, view, pgPolicies, baseOptions, func(row []model.Value) error {
		gotPolicies = append(gotPolicies, row)
		return nil
	}); err != nil {
		t.Fatalf("pg_policies: %v", err)
	}
	if len(gotPolicies) != 2 {
		t.Fatalf("pg_policies rows = %d, want 2", len(gotPolicies))
	}
	p0 := gotPolicies[0]
	if p0[pSchemaIdx].StrVal != "public" || p0[pTableIdx].StrVal != "rls_tbl" || p0[pNameIdx].StrVal != "allow_public_select" ||
		p0[pPermIdx].StrVal != "PERMISSIVE" || p0[pCmdIdx].StrVal != "SELECT" || p0[pQualIdx].StrVal != "id > 0" || !p0[pCheckIdx].IsNull ||
		len(p0[pRolesIdx].ArrayVal) != 1 || p0[pRolesIdx].ArrayVal[0].StrVal != "public" {
		t.Fatalf("policy[0] mismatch: %+v", p0)
	}
	p1 := gotPolicies[1]
	if p1[pNameIdx].StrVal != "restrict_bob_write" || p1[pPermIdx].StrVal != "RESTRICTIVE" || p1[pCmdIdx].StrVal != "INSERT" ||
		!p1[pQualIdx].IsNull || p1[pCheckIdx].StrVal != "id < 100" ||
		len(p1[pRolesIdx].ArrayVal) != 1 || p1[pRolesIdx].ArrayVal[0].StrVal != "bob" {
		t.Fatalf("policy[1] mismatch: %+v", p1)
	}

	// 4. pg_stats enforces SELECT privilege + RLS rules for all tables.
	pgStats, ok := bootstrap.Lookup("pg_catalog", "pg_stats")
	if !ok {
		t.Fatal("pg_stats missing")
	}
	statsTableIdx := colIndex(pgStats, "tablename")
	visibleTablesFor := func(role string) []string {
		t.Helper()
		opts := baseOptions
		opts.Settings = testRoleSettings{role: role}
		var names []string
		if err := generateRows(t.Context(), snapshot, view, pgStats, opts, func(row []model.Value) error {
			names = append(names, row[statsTableIdx].StrVal)
			return nil
		}); err != nil {
			t.Fatalf("pg_stats(role=%q): %v", role, err)
		}
		slices.Sort(names)
		return names
	}

	// 5. pg_statistic and pg_statistic_ext_data are superuser-only.
	pgStatistic, ok := bootstrap.Lookup("pg_catalog", "pg_statistic")
	if !ok {
		t.Fatal("pg_statistic missing")
	}
	staRelOIDIdx := colIndex(pgStatistic, "starelid")
	relOIDToTable := map[int64]string{
		int64(plainTableOID): "plain_tbl",
		int64(rlsTableOID):   "rls_tbl",
		int64(forceTableOID): "force_tbl",
	}
	visibleStatisticTablesFor := func(role string) []string {
		t.Helper()
		opts := baseOptions
		opts.Settings = testRoleSettings{role: role}
		var names []string
		if err := generateRows(t.Context(), snapshot, view, pgStatistic, opts, func(row []model.Value) error {
			names = append(names, relOIDToTable[row[staRelOIDIdx].IntVal])
			return nil
		}); err != nil {
			t.Fatalf("pg_statistic(role=%q): %v", role, err)
		}
		slices.Sort(names)
		return names
	}

	pgStatExtData, ok := bootstrap.Lookup("pg_catalog", "pg_statistic_ext_data")
	if !ok {
		t.Fatal("pg_statistic_ext_data missing")
	}
	extStxOIDIdx := colIndex(pgStatExtData, "stxoid")
	oidToTable := map[int64]string{
		int64(plainExtStatOID): "plain_tbl",
		int64(rlsExtStatOID):   "rls_tbl",
		int64(forceExtStatOID): "force_tbl",
	}
	visibleExtTablesFor := func(role string) []string {
		t.Helper()
		opts := baseOptions
		opts.Settings = testRoleSettings{role: role}
		var names []string
		if err := generateRows(t.Context(), snapshot, view, pgStatExtData, opts, func(row []model.Value) error {
			names = append(names, oidToTable[row[extStxOIDIdx].IntVal])
			return nil
		}); err != nil {
			t.Fatalf("pg_statistic_ext_data(role=%q): %v", role, err)
		}
		slices.Sort(names)
		return names
	}

	allTables := []string{"force_tbl", "plain_tbl", "rls_tbl"}
	for _, tc := range []struct {
		role         string
		wantStats    []string
		wantRawStats []string
	}{
		{"", allTables, allTables},                         // default superuser sees all
		{"admin", allTables, allTables},                    // explicit superuser sees all
		{"auditor", []string{"force_tbl", "rls_tbl"}, nil}, // BYPASSRLS + SELECT sees RLS/forced tables, denied on plain_tbl (no SELECT) and raw stats
		{"alice", []string{"plain_tbl", "rls_tbl"}, nil},   // SELECT on plain_tbl + owner on non-forced RLS, hidden on forced RLS and raw stats
		{"alice_member", []string{"plain_tbl", "rls_tbl"}, nil},
		{"bob", nil, nil},          // no SELECT on plain_tbl, RLS active on rls_tbl despite SELECT
		{"unknown_role", nil, nil}, // fail-closed for unknown role
	} {
		got := visibleTablesFor(tc.role)
		if !slices.Equal(got, tc.wantStats) {
			t.Errorf("pg_stats visible tables for role %q = %v, want %v", tc.role, got, tc.wantStats)
		}
		gotSta := visibleStatisticTablesFor(tc.role)
		if !slices.Equal(gotSta, tc.wantRawStats) {
			t.Errorf("pg_statistic visible tables for role %q = %v, want %v", tc.role, gotSta, tc.wantRawStats)
		}
		gotExt := visibleExtTablesFor(tc.role)
		if !slices.Equal(gotExt, tc.wantRawStats) {
			t.Errorf("pg_statistic_ext_data visible tables for role %q = %v, want %v", tc.role, gotExt, tc.wantRawStats)
		}
	}
}
