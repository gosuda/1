package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func roleDescriptor(t *testing.T, id model.ObjectID, oid model.OID, name string, meta model.RoleMetadata) *model.Descriptor {
	t.Helper()
	d, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID:      id,
		OID:     oid,
		Name:    model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: name},
		Version: 1,
		State:   model.StatePublic,
		Role:    &meta,
	}, catalogLimits().Descriptor)
	if err != nil {
		t.Fatalf("roleDescriptor(%s): %v", name, err)
	}
	return d
}

func TestCodecRoleAndTableRLSRoundTripAndBackwardCompat(t *testing.T) {
	// 1. ObjectRole codec round-trip.
	role := roleDescriptor(t, 40, 400, "tenant_admin", model.RoleMetadata{
		Superuser: false,
		BypassRLS: true,
		MemberOf:  []string{"base_user", "auditor"},
	})
	encodedRole, err := catalog.EncodeDescriptor(t.Context(), role, catalogLimits())
	if err != nil {
		t.Fatalf("EncodeDescriptor(role): %v", err)
	}
	decodedRole, err := catalog.DecodeDescriptor(t.Context(), encodedRole, catalogLimits())
	if err != nil {
		t.Fatalf("DecodeDescriptor(role): %v", err)
	}
	gotRole, ok := decodedRole.Role()
	if !ok || gotRole.Superuser || !gotRole.BypassRLS || !slices.Equal(gotRole.MemberOf, []string{"base_user", "auditor"}) {
		t.Fatalf("decoded role = %+v, ok=%v", gotRole, ok)
	}

	// 2. TableMetadata with OwnerName, RowSecurity, ForceRowSecurity, and Policies round-trip.
	base := tableDescriptor(t, 41, "docs", 1)
	withRLS, err := base.WithTableRLS(true, true, []model.PolicyDescriptor{
		{
			ID:         1001,
			OID:        2001,
			Name:       "tenant_iso",
			Permissive: true,
			Command:    "ALL",
			Roles:      []string{"tenant_admin"},
			Using:      &model.ExpressionDefinition{SQL: "owner = current_user", SemanticVersion: 1, Encoding: []byte{1}},
			WithCheck:  &model.ExpressionDefinition{SQL: "owner = current_user", SemanticVersion: 1, Encoding: []byte{2}},
		},
	})
	if err != nil {
		t.Fatalf("WithTableRLS: %v", err)
	}
	withOwner, err := withRLS.WithTableOwner(400, "tenant_admin")
	if err != nil {
		t.Fatalf("WithTableOwner: %v", err)
	}
	encodedTable, err := catalog.EncodeDescriptor(t.Context(), withOwner, catalogLimits())
	if err != nil {
		t.Fatalf("EncodeDescriptor(table): %v", err)
	}
	decodedTable, err := catalog.DecodeDescriptor(t.Context(), encodedTable, catalogLimits())
	if err != nil {
		t.Fatalf("DecodeDescriptor(table): %v", err)
	}
	gotTable, ok := decodedTable.Table()
	if !ok || gotTable.Owner != 400 || gotTable.OwnerName != "tenant_admin" || !gotTable.RowSecurity || !gotTable.ForceRowSecurity || len(gotTable.Policies) != 1 {
		t.Fatalf("decoded table RLS = %+v", gotTable)
	}
	p := gotTable.Policies[0]
	if p.Name != "tenant_iso" || !p.Permissive || p.Command != "ALL" || !slices.Equal(p.Roles, []string{"tenant_admin"}) || p.Using == nil || p.Using.SQL != "owner = current_user" || p.WithCheck == nil || p.WithCheck.SQL != "owner = current_user" {
		t.Fatalf("decoded policy = %+v", p)
	}

	// 3. Backward compatibility: legacy encoded table JSON lacking new fields decodes with zero values.
	legacyEncoded, err := catalog.EncodeDescriptor(t.Context(), base, catalogLimits())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(legacyEncoded, &doc); err != nil {
		t.Fatal(err)
	}
	descObj := doc["Descriptor"].(map[string]any)
	tableObj := descObj["Table"].(map[string]any)
	delete(descObj, "Role")
	delete(tableObj, "OwnerName")
	delete(tableObj, "RowSecurity")
	delete(tableObj, "ForceRowSecurity")
	delete(tableObj, "Policies")
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	decodedLegacy, err := catalog.DecodeDescriptor(t.Context(), stripped, catalogLimits())
	if err != nil {
		t.Fatalf("DecodeDescriptor(legacy): %v", err)
	}
	legacyTbl, ok := decodedLegacy.Table()
	if !ok || legacyTbl.OwnerName != "" || legacyTbl.RowSecurity || legacyTbl.ForceRowSecurity || len(legacyTbl.Policies) != 0 {
		t.Fatalf("legacy table decoded unexpected RLS fields: %+v", legacyTbl)
	}
}

func TestCatalogRoleLookupSavepointRollbackAndTableLifecycle(t *testing.T) {
	f := newCatalogFixture(t)

	// Create roles alice and bob in a committed transaction.
	alice := roleDescriptor(t, 60, 600, "alice", model.RoleMetadata{Superuser: false, BypassRLS: false})
	bob := roleDescriptor(t, 61, 601, "bob", model.RoleMetadata{Superuser: false, BypassRLS: true, MemberOf: []string{"alice"}})
	snap0 := f.snapshot(t, 1)
	c1, err := f.store.Create(t.Context(), snap0, alice)
	if err != nil {
		t.Fatalf("Create(alice): %v", err)
	}
	ts1 := f.commit(t, c1)
	snap1 := f.snapshot(t, ts1)
	c2, err := f.store.Create(t.Context(), snap1, bob)
	if err != nil {
		t.Fatalf("Create(bob): %v", err)
	}
	ts2 := f.commit(t, c2)

	// Verify Role (case-folded) and ListRoles.
	snap2 := f.snapshot(t, ts2)
	gotAlice, err := snap2.Role(t.Context(), "ALICE")
	if err != nil || gotAlice.ID() != alice.ID() {
		t.Fatalf("Role(ALICE) = %v, %v", gotAlice, err)
	}
	listed, err := snap2.ListRoles(t.Context())
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListRoles = %d roles, err=%v", len(listed), err)
	}
	if _, err := snap2.Role(t.Context(), "missing"); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("Role(missing) = %v, want ErrUnknownObject", err)
	}

	// Verify MVCC savepoint & rollback on ObjectRole and TableMetadata RLS updates.
	tbl := tableDescriptor(t, 70, "secrets", 1)
	tblWithRLS, err := tbl.WithTableRLS(true, false, []model.PolicyDescriptor{
		{Name: "p_read", Permissive: true, Command: "SELECT", Using: &model.ExpressionDefinition{SQL: "true", SemanticVersion: 1, Encoding: []byte{1}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reset Version to 1 for initial Create.
	spec, err := tblWithRLS.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Version = 1
	tbl1, err := model.NewDescriptor(t.Context(), spec, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	c3, err := f.store.Create(t.Context(), snap2, tbl1)
	if err != nil {
		t.Fatalf("Create(secrets): %v", err)
	}
	ts3 := f.commit(t, c3)

	// Start an explicit transaction, stage role creation + RLS change inside a savepoint, roll back savepoint.
	tx := f.begin(t)
	roleScanDep, err := f.store.NamespaceScanDependency(t.Context(), 0, model.ObjectRole)
	if err != nil {
		t.Fatal(err)
	}
	carol := roleDescriptor(t, 62, 602, "carol", model.RoleMetadata{Superuser: true})
	carolDeps, err := f.store.CreateDependencies(t.Context(), carol)
	if err != nil {
		t.Fatal(err)
	}
	allDeps := append([]txn.ReadDependency{roleScanDep, f.store.DescriptorDependency(tbl1.ID())}, carolDeps...)

	sp, err := tx.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Statement(t.Context(), func(sctx context.Context) error {
		view, err := tx.ReadView(sctx, allDeps)
		if err != nil {
			return err
		}
		defer view.Close()
		tsnap, err := f.store.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return err
		}
		defer tsnap.Close()
		ch, err := f.store.Create(sctx, tsnap, carol)
		if err != nil {
			return err
		}
		return tx.ApplyMutations(sctx, ch.Mutations)
	}); err != nil {
		t.Fatalf("stage carol: %v", err)
	}
	// Rollback to savepoint: carol must vanish!
	if err := tx.RollbackTo(t.Context(), sp); err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	// Stage table rename & schema move in the same transaction; RLS metadata must survive!
	renameSpec, err := tbl1.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	renameSpec.Version++
	renameSpec.Name = model.NamespaceName{Parent: 20, Kind: model.ObjectTable, Name: "secrets_moved"}
	tblMoved, err := model.NewDescriptor(t.Context(), renameSpec, catalogLimits().Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	destDep, err := f.store.NamespaceDependency(t.Context(), tblMoved.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Statement(t.Context(), func(sctx context.Context) error {
		view, err := tx.ReadView(sctx, append(allDeps, destDep))
		if err != nil {
			return err
		}
		defer view.Close()
		tsnap, err := f.store.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return err
		}
		defer tsnap.Close()
		if _, err := tsnap.Role(sctx, "carol"); !errors.Is(err, catalog.ErrUnknownObject) {
			t.Fatalf("carol survived savepoint rollback: %v", err)
		}
		ch, err := f.store.Update(sctx, tsnap, tbl1.Version(), tblMoved)
		if err != nil {
			return err
		}
		return tx.ApplyMutations(sctx, ch.Mutations)
	}); err != nil {
		t.Fatalf("rename/move table: %v", err)
	}
	res, err := tx.Commit(t.Context())
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	snapAfter := f.snapshot(t, res.CommitTS)
	if _, err := snapAfter.Role(t.Context(), "carol"); !errors.Is(err, catalog.ErrUnknownObject) {
		t.Fatalf("rolled-back role carol committed: %v", err)
	}
	movedPub, err := snapAfter.Resolve(t.Context(), tblMoved.Name())
	if err != nil {
		t.Fatalf("Resolve(secrets_moved): %v", err)
	}
	movedTbl, _ := movedPub.Descriptor.Table()
	if !movedTbl.RowSecurity || len(movedTbl.Policies) != 1 || movedTbl.Policies[0].Name != "p_read" {
		t.Fatalf("RLS metadata lost across rename/set-schema: %+v", movedTbl)
	}
	// Historical snapshot at ts3 still sees original name.
	snapOld := f.snapshot(t, ts3)
	if _, err := snapOld.Resolve(t.Context(), tbl1.Name()); err != nil {
		t.Fatalf("historical resolve at ts3 failed: %v", err)
	}
}
