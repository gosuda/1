package bind

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestHasObjectPrivilegeAndGrantOption(t *testing.T) {
	roles := map[string]model.RoleMetadata{
		"admin":  {Superuser: true},
		"owner":  {},
		"base":   {},
		"mid":    {MemberOf: []string{"base"}},
		"leaf":   {MemberOf: []string{"mid"}},
		"nobody": {},
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 16}
	intType := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	tableDesc, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 100, OID: 10100, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "orders"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			Owner: 1, OwnerName: "owner", Persistence: "p",
			ColumnIDHighWatermark: 1,
			Columns:               []model.ColumnDescriptor{{ID: 1, Name: "id", Layout: model.TypeLayout{Type: intType}, State: model.StatePublic}},
			PrimaryKey:            []model.KeyElement{{Column: 1, Type: intType}},
			ACL: []model.PrivilegeGrant{
				{Grantee: "base", Privileges: model.PrivilegeSelect | model.PrivilegeInsert, GrantOption: model.PrivilegeSelect, Grantor: "owner"},
			},
		},
	}, limits)
	if err != nil {
		t.Fatal(err)
	}

	// Default superuser (""), explicit superuser ("admin"), and owner hold all privileges and grant option.
	for _, role := range []string{"", "supersql", "admin", "owner"} {
		if !hasObjectPrivilege(roles, role, tableDesc, model.PrivilegeAllTable, false) {
			t.Fatalf("role %q denied PrivilegeAllTable", role)
		}
		if !hasGrantOption(roles, role, tableDesc, model.PrivilegeAllTable) {
			t.Fatalf("role %q denied grant option", role)
		}
	}

	// Transitive member "leaf" -> "mid" -> "base" inherits SELECT+INSERT and SELECT grant option, but not DELETE.
	if !hasObjectPrivilege(roles, "leaf", tableDesc, model.PrivilegeSelect|model.PrivilegeInsert, false) {
		t.Fatal("leaf denied inherited SELECT|INSERT")
	}
	if hasObjectPrivilege(roles, "leaf", tableDesc, model.PrivilegeDelete, false) {
		t.Fatal("leaf unexpectedly granted DELETE")
	}
	if !hasGrantOption(roles, "leaf", tableDesc, model.PrivilegeSelect) {
		t.Fatal("leaf denied inherited SELECT grant option")
	}
	if hasGrantOption(roles, "leaf", tableDesc, model.PrivilegeInsert) {
		t.Fatal("leaf unexpectedly has INSERT grant option")
	}

	// Unrelated role "nobody" has no privileges.
	if hasObjectPrivilege(roles, "nobody", tableDesc, model.PrivilegeSelect, false) {
		t.Fatal("nobody unexpectedly granted SELECT")
	}

	// PUBLIC grant applies to nobody.
	withPublic, err := tableDesc.WithACL(applyGrantToACL(tableDesc.ACL(), []string{"public"}, model.PrivilegeSelect, "owner", true, false, true))
	if err != nil {
		t.Fatal(err)
	}
	if !hasObjectPrivilege(roles, "nobody", withPublic, model.PrivilegeSelect, false) {
		t.Fatal("nobody denied PUBLIC SELECT")
	}

	// Non-owner "base" (holding SELECT WITH GRANT OPTION) grants SELECT to "nobody",
	// creating a distinct (nobody, base) entry alongside (public, owner).
	withDelegated, err := withPublic.WithACL(applyGrantToACL(withPublic.ACL(), []string{"nobody"}, model.PrivilegeSelect, "base", true, false, false))
	if err != nil {
		t.Fatal(err)
	}
	// Non-owner "base" revoking SELECT from "public" MUST NOT remove owner's grant to "public".
	afterNonOwnerRevoke, err := withDelegated.WithACL(applyGrantToACL(withDelegated.ACL(), []string{"public"}, model.PrivilegeSelect, "base", false, false, false))
	if err != nil {
		t.Fatal(err)
	}
	if !hasObjectPrivilege(roles, "nobody", afterNonOwnerRevoke, model.PrivilegeSelect, false) {
		t.Fatal("non-owner revoke unexpectedly stripped owner's PUBLIC grant")
	}
	// Non-owner "base" CAN revoke its own (nobody, base) grant.
	afterOwnRevoke, err := afterNonOwnerRevoke.WithACL(applyGrantToACL(afterNonOwnerRevoke.ACL(), []string{"nobody"}, model.PrivilegeSelect, "base", false, false, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range afterOwnRevoke.ACL() {
		if g.Grantee == "nobody" && g.Grantor == "base" {
			t.Fatalf("non-owner failed to revoke its own grant: %+v", afterOwnRevoke.ACL())
		}
	}

	// REVOKE GRANT OPTION FOR SELECT keeps SELECT on base while removing grantability.
	revokedOpt, err := afterOwnRevoke.WithACL(applyGrantToACL(afterOwnRevoke.ACL(), []string{"base"}, model.PrivilegeSelect, "owner", false, true, true))
	if err != nil {
		t.Fatal(err)
	}
	if !hasObjectPrivilege(roles, "base", revokedOpt, model.PrivilegeSelect, false) {
		t.Fatal("base lost SELECT after REVOKE GRANT OPTION FOR")
	}
	if hasGrantOption(roles, "base", revokedOpt, model.PrivilegeSelect) {
		t.Fatal("base retained SELECT grant option after REVOKE GRANT OPTION FOR")
	}

	// REVOKE ALL removes grantee entry completely (keeping ACL valid).
	revokedAll, err := revokedOpt.WithACL(applyGrantToACL(revokedOpt.ACL(), []string{"base", "public"}, model.PrivilegeAllTable, "owner", false, false, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(revokedAll.ACL()) != 0 {
		t.Fatalf("expected empty ACL after revoking all, got %+v", revokedAll.ACL())
	}
	if hasObjectPrivilege(roles, "leaf", revokedAll, model.PrivilegeSelect, false) {
		t.Fatal("leaf retained SELECT after REVOKE ALL")
	}
}
