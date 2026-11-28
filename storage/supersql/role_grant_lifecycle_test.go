package supersql

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestDropRoleRetainsObjectGrants(t *testing.T) {
	for _, test := range []struct {
		name   string
		grant  string
		revoke string
		mask   model.PrivilegeMask
	}{
		{"all-including-maintain", "ALL", "SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, MAINTAIN", model.PrivilegeAllTable},
		{"select-and-trigger", "SELECT, TRIGGER", "SELECT", model.PrivilegeSelect | model.PrivilegeTrigger},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			db, err := Open(ctx, OpenOptions{Directory: t.TempDir(), Clock: newTestClock()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			exec := func(query string, state string) {
				t.Helper()
				_, err := db.exec(ctx, query, 10000)
				if got := regressionSQLState(err); got != state {
					t.Fatalf("%s: state=%s want=%s (%v)", query, got, state, err)
				}
			}
			mask := func(want model.PrivilegeMask) {
				t.Helper()
				view, err := db.manager.CurrentReadView(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := view.Close(); err != nil {
						t.Error(err)
					}
				}()
				snapshot, err := db.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := snapshot.Close(); err != nil {
						t.Error(err)
					}
				}()
				if _, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectRole, Name: "grant_user"}); err != nil {
					t.Fatalf("role disappeared after rejected drop: %v", err)
				}
				schema, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: "public"})
				if err != nil {
					t.Fatal(err)
				}
				table, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: schema.Descriptor.ID(), Kind: model.ObjectTable, Name: "grant_table"})
				if err != nil {
					t.Fatal(err)
				}
				var actual model.PrivilegeMask
				for _, grant := range table.Descriptor.ACL() {
					if grant.Grantee == "grant_user" {
						actual |= grant.Privileges
					}
				}
				if actual != want {
					t.Fatalf("persisted grant mask=%x want=%x", actual, want)
				}
			}
			exec("CREATE ROLE grant_user", "")
			exec("CREATE TABLE grant_table(n int)", "")
			exec("GRANT "+test.grant+" ON grant_table TO grant_user", "")
			exec("DROP ROLE grant_user", "2BP01")
			mask(test.mask)
			exec("REVOKE "+test.revoke+" ON grant_table FROM grant_user", "")
			mask(model.PrivilegeTrigger)
			exec("DROP ROLE grant_user", "2BP01")
			mask(model.PrivilegeTrigger)
			exec("CREATE ROLE other_user", "")
			exec("DROP ROLE other_user, grant_user", "2BP01")
			exec("ALTER ROLE other_user NOLOGIN", "")
			mask(model.PrivilegeTrigger)
			exec("REVOKE TRIGGER ON grant_table FROM grant_user", "")
			mask(0)
			exec("DROP ROLE grant_user", "")
			exec("DROP ROLE grant_user", "42704")
			exec("DROP ROLE other_user", "")
		})
	}
}
