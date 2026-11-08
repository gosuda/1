package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindAlterNotNull(ctx context.Context, b *binder, options CreateDeclarationOptions, spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd) (model.ObjectID, error) {
	if spec == nil || spec.Table == nil || command == nil || command.Name == "" || command.Def != nil || command.Recurse || command.MissingOk {
		return 0, bindError("42601", "invalid ALTER COLUMN nullability command", 0)
	}
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if position < 0 {
		return 0, bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[position]
	constraint := slices.IndexFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Kind == model.ConstraintNotNull && len(constraint.Columns) == 1 && constraint.Columns[0] == column.ID
	})
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_SetNotNull:
		if constraint >= 0 {
			existing := &spec.Table.Constraints[constraint]
			if !existing.Enforced {
				return 0, bindError("55000", "NOT NULL constraint is not enforced", 0)
			}
			if column.Nullable || !existing.Validated {
				column.Nullable = false
				existing.Validated = true
				return existing.ID, nil
			}
			return 0, nil
		}
		if len(spec.Table.Constraints) >= options.Descriptors.MaxItems {
			return 0, model.ErrDescriptorLimit
		}
		state := declarationState{
			b: b, options: options, parent: spec.Name.Parent, tableName: spec.Name.Name,
			names: make(map[string]bool), reserved: make(map[string]bool),
			ids: map[model.ObjectID]bool{spec.ID: true}, oids: map[model.OID]bool{spec.OID: true},
		}
		for _, existing := range spec.Table.Constraints {
			if existing.Name != "" {
				state.reserved[existing.Name] = true
			}
			state.ids[existing.ID], state.oids[existing.OID] = true, true
		}
		name, err := state.constraintName("", column.Name, "not_null", false)
		if err != nil {
			return 0, err
		}
		id, oid, err := state.allocate()
		if err != nil {
			return 0, err
		}
		column.Nullable = false
		spec.Table.Constraints = append(spec.Table.Constraints, model.ConstraintDescriptor{
			ID: id, OID: oid, Name: name, Kind: model.ConstraintNotNull, Columns: []model.ColumnID{column.ID},
			Validated: true, Enforced: true, Local: true,
		})
		return id, ctx.Err()
	case zpgparse.AlterTableType_AT_DropNotNull:
		if column.IdentitySequence != 0 {
			return 0, bindError("55000", "identity column cannot be nullable", 0)
		}
		if constraint >= 0 && !spec.Table.Constraints[constraint].Local {
			return 0, bindError("42P16", "cannot drop inherited NOT NULL constraint", 0)
		}
		if constraint >= 0 {
			spec.Table.Constraints = slices.Delete(spec.Table.Constraints, constraint, constraint+1)
		}
		column.Nullable = true
		return 0, ctx.Err()
	default:
		return 0, bindError("42601", "invalid ALTER COLUMN nullability command", 0)
	}
}
