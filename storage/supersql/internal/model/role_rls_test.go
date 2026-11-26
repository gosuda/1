package model_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func roleSpec(name string, meta model.RoleMetadata) model.DescriptorSpec {
	return model.DescriptorSpec{
		ID:      50,
		OID:     500,
		Name:    model.NamespaceName{Parent: 0, Kind: model.ObjectRole, Name: name},
		Version: 1,
		State:   model.StatePublic,
		Role:    &meta,
	}
}

func TestRoleDescriptorValidationCopyAndSuccessor(t *testing.T) {
	members := []string{"readers", "writers"}
	spec := roleSpec("alice", model.RoleMetadata{Superuser: false, BypassRLS: true, MemberOf: members})
	desc, err := model.NewDescriptor(t.Context(), spec, descriptorLimits())
	if err != nil {
		t.Fatalf("NewDescriptor(role): %v", err)
	}
	// Mutate caller slice to verify deep copy isolation.
	members[0] = "mutated"
	got, ok := desc.Role()
	if !ok || got.Superuser || !got.BypassRLS || !slices.Equal(got.MemberOf, []string{"readers", "writers"}) {
		t.Fatalf("Role() = %+v, ok=%v; want BypassRLS=true MemberOf=[readers writers]", got, ok)
	}

	next, err := desc.WithRole(model.RoleMetadata{Superuser: true, BypassRLS: true, MemberOf: []string{"admins"}})
	if err != nil {
		t.Fatalf("WithRole: %v", err)
	}
	if err := desc.ValidateSuccessor(t.Context(), next); err != nil {
		t.Fatalf("ValidateSuccessor(role): %v", err)
	}
	if next.Version() != 2 {
		t.Fatalf("next.Version() = %d, want 2", next.Version())
	}
	nextRole, ok := next.Role()
	if !ok || !nextRole.Superuser || !slices.Equal(nextRole.MemberOf, []string{"admins"}) {
		t.Fatalf("next.Role() = %+v, ok=%v", nextRole, ok)
	}

	// Reject invalid ObjectRole variants.
	for _, tc := range []struct {
		name   string
		mutate func(*model.DescriptorSpec)
	}{
		{"non_zero_parent", func(s *model.DescriptorSpec) { s.Name.Parent = 10 }},
		{"empty_name", func(s *model.DescriptorSpec) { s.Name.Name = "" }},
		{"uppercase_name", func(s *model.DescriptorSpec) { s.Name.Name = "Alice" }},
		{"nil_role_metadata", func(s *model.DescriptorSpec) { s.Role = nil }},
		{"physical_identity_set", func(s *model.DescriptorSpec) { s.Physical = model.PhysicalIdentity{Generation: 1, EncodingVersion: 1} }},
		{"empty_member", func(s *model.DescriptorSpec) { s.Role.MemberOf = []string{""} }},
		{"uppercase_member", func(s *model.DescriptorSpec) { s.Role.MemberOf = []string{"Admins"} }},
		{"duplicate_member", func(s *model.DescriptorSpec) { s.Role.MemberOf = []string{"admins", "admins"} }},
		{"self_membership", func(s *model.DescriptorSpec) { s.Role.MemberOf = []string{"alice"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := roleSpec("alice", model.RoleMetadata{MemberOf: []string{"readers"}})
			tc.mutate(&bad)
			if _, err := model.NewDescriptor(t.Context(), bad, descriptorLimits()); !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("%s: got %v, want ErrInvalidDescriptor", tc.name, err)
			}
		})
	}
}

func TestTableMetadataRLSAndPolicyValidationAndHelpers(t *testing.T) {
	base, err := model.NewDescriptor(t.Context(), descriptorSpec(), descriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{"alice", "bob"}
	usingExpr := &model.ExpressionDefinition{SQL: "tenant_id = 1", SemanticVersion: 1, Encoding: []byte{1, 2}}
	checkExpr := &model.ExpressionDefinition{SQL: "tenant_id > 0", SemanticVersion: 1, Encoding: []byte{3, 4}}
	policies := []model.PolicyDescriptor{
		{
			ID:         900,
			OID:        9000,
			Name:       "tenant_select",
			Permissive: true,
			Command:    "SELECT",
			Roles:      roles,
			Using:      usingExpr,
		},
		{
			ID:         901,
			OID:        9001,
			Name:       "tenant_write",
			Permissive: false,
			Command:    "ALL",
			Using:      usingExpr,
			WithCheck:  checkExpr,
		},
	}

	withRLS, err := base.WithTableRLS(true, true, policies)
	if err != nil {
		t.Fatalf("WithTableRLS: %v", err)
	}
	// Mutate caller buffers to verify deep copy isolation.
	roles[0] = "tampered"
	usingExpr.Encoding[0] = 99
	checkExpr.Encoding[0] = 88

	if err := base.ValidateSuccessor(t.Context(), withRLS); err != nil {
		t.Fatalf("ValidateSuccessor(WithTableRLS): %v", err)
	}
	tbl, ok := withRLS.Table()
	if !ok || !tbl.RowSecurity || !tbl.ForceRowSecurity || len(tbl.Policies) != 2 {
		t.Fatalf("withRLS.Table() = %+v", tbl)
	}
	if !slices.Equal(tbl.Policies[0].Roles, []string{"alice", "bob"}) {
		t.Fatalf("policy roles aliased caller: %v", tbl.Policies[0].Roles)
	}
	if tbl.Policies[0].Using.Encoding[0] != 1 || tbl.Policies[1].WithCheck.Encoding[0] != 3 {
		t.Fatalf("policy expressions aliased caller")
	}

	withOwner, err := withRLS.WithTableOwner(777, "alice")
	if err != nil {
		t.Fatalf("WithTableOwner: %v", err)
	}
	if err := withRLS.ValidateSuccessor(t.Context(), withOwner); err != nil {
		t.Fatalf("ValidateSuccessor(WithTableOwner): %v", err)
	}
	ownedTbl, _ := withOwner.Table()
	if ownedTbl.Owner != 777 || ownedTbl.OwnerName != "alice" || !ownedTbl.RowSecurity || len(ownedTbl.Policies) != 2 {
		t.Fatalf("withOwner.Table() = %+v", ownedTbl)
	}

	// Reject invalid policies and owner names.
	for _, tc := range []struct {
		name   string
		mutate func(*model.TableMetadata)
	}{
		{"uppercase_owner_name", func(tm *model.TableMetadata) { tm.OwnerName = "Alice" }},
		{"empty_policy_name", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "", Command: "ALL", Permissive: true}}
		}},
		{"duplicate_policy_name", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{
				{Name: "p1", Command: "SELECT", Permissive: true},
				{Name: "p1", Command: "DELETE", Permissive: true},
			}
		}},
		{"duplicate_policy_id", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{
				{ID: 1, Name: "p1", Command: "SELECT", Permissive: true},
				{ID: 1, Name: "p2", Command: "DELETE", Permissive: true},
			}
		}},
		{"duplicate_policy_oid", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{
				{OID: 10, Name: "p1", Command: "SELECT", Permissive: true},
				{OID: 10, Name: "p2", Command: "DELETE", Permissive: true},
			}
		}},
		{"invalid_command", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "p1", Command: "MERGE", Permissive: true}}
		}},
		{"lowercase_command", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "p1", Command: "select", Permissive: true}}
		}},
		{"uppercase_policy_role", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "p1", Command: "ALL", Permissive: true, Roles: []string{"Alice"}}}
		}},
		{"duplicate_policy_role", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "p1", Command: "ALL", Permissive: true, Roles: []string{"alice", "alice"}}}
		}},
		{"invalid_using_expression", func(tm *model.TableMetadata) {
			tm.Policies = []model.PolicyDescriptor{{Name: "p1", Command: "ALL", Permissive: true, Using: &model.ExpressionDefinition{SQL: "true"}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := descriptorSpec()
			tc.mutate(spec.Table)
			if _, err := model.NewDescriptor(t.Context(), spec, descriptorLimits()); !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("%s: got %v, want ErrInvalidDescriptor", tc.name, err)
			}
		})
	}
}
