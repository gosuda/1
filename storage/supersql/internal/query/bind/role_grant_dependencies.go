package bind

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Object ACLs store role names, so their references are checked explicitly
// before DROP ROLE stages any deletion or membership cleanup.
func checkDroppedRoleGrants(ctx context.Context, snapshot CatalogSnapshot, dropped map[string]struct{}, limit uint64) error {
	var count uint64
	check := func(published catalog.PublishedDescriptor) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if limit > 0 && count > limit {
			return bindError("54000", "role grant dependency scan limit exceeded", 0)
		}
		d := published.Descriptor
		if d == nil || d.State() != model.StatePublic {
			return nil
		}
		for _, grant := range d.ACL() {
			if grant.Privileges == 0 {
				continue
			}
			for _, name := range []string{grant.Grantee, grant.Grantor} {
				if _, dropping := dropped[strings.ToLower(name)]; dropping {
					return bindError("2BP01", fmt.Sprintf("role %q cannot be dropped because some objects depend on it", name), 0)
				}
			}
		}
		return nil
	}
	return snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if err := check(schema); err != nil {
			return err
		}
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		for _, kind := range [...]model.ObjectKind{
			model.ObjectTable, model.ObjectView, model.ObjectMaterializedView,
			model.ObjectSequence, model.ObjectRoutine,
		} {
			if err := snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), kind, check); err != nil {
				return err
			}
		}
		return nil
	})
}
