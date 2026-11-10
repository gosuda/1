package bind

import (
	"context"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type descriptorBindingOverlay struct {
	CatalogSnapshot
	descriptors []*model.Descriptor
}

func (o descriptorBindingOverlay) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	for _, descriptor := range o.descriptors {
		if descriptor != nil && descriptor.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return o.CatalogSnapshot.Resolve(ctx, name)
}

func (o descriptorBindingOverlay) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	for _, descriptor := range o.descriptors {
		if descriptor != nil && descriptor.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return o.CatalogSnapshot.Descriptor(ctx, id)
}

func ownedSequenceDefault(parent, name string) (*zpgparse.Node, error) {
	qualified := `"` + strings.ReplaceAll(parent, `"`, `""`) + `"."` + strings.ReplaceAll(name, `"`, `""`) + `"`
	parsed, err := zpgparse.Parse("SELECT pg_catalog.nextval('" + strings.ReplaceAll(qualified, "'", "''") + "'::pg_catalog.regclass)")
	if err != nil {
		return nil, err
	}
	node := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
	call := node.GetFuncCall()
	call.Location = -1
	cast := call.Args[0].GetTypeCast()
	cast.Location, cast.TypeName.Location = -1, -1
	cast.Arg.GetAConst().Location = -1
	return node, nil
}

func bindOwnedSequenceAddColumn(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, column *zpgparse.ColumnDef, spec model.DescriptorSpec, declaration model.ColumnDescriptor, serial bool, identity *zpgparse.Constraint, relational []*zpgparse.Constraint) (*BoundSchemaDDL, error) {
	if b == nil || snapshot == nil || table == nil || column == nil || spec.Table == nil || serial == (identity != nil) {
		return nil, model.ErrInvalidDescriptor
	}
	declaration.Nullable = true
	spec.Table.Columns = append(spec.Table.Columns, declaration)
	spec.Table.ColumnIDHighWatermark = declaration.ID
	state := declarationState{
		b: b, options: options, parent: table.Name().Parent, tableName: table.Name().Name,
		names: make(map[string]bool), reserved: make(map[string]bool),
		ids: map[model.ObjectID]bool{table.ID(): true}, oids: map[model.OID]bool{table.OID(): true},
	}
	for _, constraint := range spec.Table.Constraints {
		state.reserved[constraint.Name] = constraint.Name != ""
		state.ids[constraint.ID], state.oids[constraint.OID] = true, true
	}
	serials := map[model.ColumnID]bool(nil)
	var declarations []tableConstraint
	if serial {
		serials = map[model.ColumnID]bool{declaration.ID: true}
	} else {
		declarations = []tableConstraint{{column: declaration.ID, raw: identity}}
	}
	defaults := make(map[model.ColumnID]*zpgparse.Node)
	sequences, _, err := state.ownedSequences(spec, serials, declarations, defaults)
	if err != nil {
		return nil, err
	}
	if len(sequences) != 1 {
		return nil, model.ErrInvalidDescriptor
	}
	placeholder, err := sequences[0].Finalize(ctx, 1)
	if err != nil {
		return nil, err
	}
	parent, err := snapshot.Descriptor(ctx, table.Name().Parent)
	if err != nil {
		return nil, err
	}
	raw := defaults[declaration.ID]
	if raw == nil {
		raw, err = ownedSequenceDefault(parent.Descriptor.Name().Name, placeholder.Name().Name)
		if err != nil {
			return nil, err
		}
	}
	overlay := descriptorBindingOverlay{CatalogSnapshot: snapshot, descriptors: []*model.Descriptor{placeholder}}
	current, found := declarationColumn(spec.Table.Columns, declaration.ID)
	if !found || current.Nullable {
		return nil, model.ErrInvalidDescriptor
	}
	transform, _, err := DefineGenerationTransform(ctx, raw, table, spec.Table.Columns, declaration.ID, current.Layout, overlay, options.Bind)
	if err != nil {
		return nil, err
	}
	for _, dependency := range transform.Dependencies {
		b.dependencies = appendIndexDependency(b.dependencies, dependency)
	}
	if serial {
		bound, bindErr := defineDefault(ctx, raw, current.Layout, overlay, options.Bind, true)
		if bindErr != nil {
			return nil, bindErr
		}
		b.warnings = append(b.warnings, bound.Warnings...)
		definition := bound.Definition
		current.Default = &definition
		_, current.DefaultOID, err = options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, err
		}
	}
	spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: placeholder.ID(), Version: placeholder.Version()})
	if len(spec.Table.Constraints) >= options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	name, err := state.constraintName("", current.Name, "not_null", false)
	if err != nil {
		return nil, err
	}
	id, oid, err := state.allocate()
	if err != nil {
		return nil, err
	}
	spec.Table.Constraints = append(spec.Table.Constraints, model.ConstraintDescriptor{
		ID: id, OID: oid, Name: name, Kind: model.ConstraintNotNull, Columns: []model.ColumnID{current.ID},
		Validated: true, Enforced: true, Local: true,
	})
	spec.Version = table.Version() + 3
	indexCreates, validations, err := bindAddedColumnConstraints(ctx, b, overlay, options, table, &spec, current.ID, relational)
	if err != nil {
		return nil, err
	}
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	creates := append([]*model.Descriptor{placeholder}, indexCreates...)
	return &BoundSchemaDDL{
		source: table, mutation: model.ObjectID(current.ID),
		definition: schema.JobDefinition{
			Operation: schema.SchemaAddColumn, SourceTable: table, TargetTable: target,
			TargetIndexes: slices.Clone(indexCreates), Transforms: []schema.GenerationTransform{{Column: current.ID, Expression: transform}},
		},
		transactionalCreates: creates, transactionalIndexCreates: len(indexCreates), transactionalValidations: validations, sequences: sequences,
		dependencies: b.dependencies, warnings: b.warnings,
	}, ctx.Err()
}

func (d *BoundSchemaDDL) FinalizeSequences(ctx context.Context, descriptors []*model.Descriptor, limits model.DescriptorLimits) (*BoundSchemaDDL, error) {
	if d == nil || len(d.sequences) == 0 || len(descriptors) != len(d.sequences) || d.definition.TargetTable == nil {
		return nil, bindError("22023", "sequence finalization requires all owned sequences", 0)
	}
	next := *d
	next.transactionalCreates = slices.Clone(d.transactionalCreates)
	for _, pending := range d.sequences {
		var actual *model.Descriptor
		for _, descriptor := range descriptors {
			if descriptor != nil && descriptor.ID() == pending.draft.ID {
				if actual != nil {
					return nil, bindError("22023", "duplicate owned sequence", 0)
				}
				actual = descriptor
			}
		}
		if actual == nil {
			return nil, bindError("22023", "missing owned sequence", 0)
		}
		metadata, ok := actual.Sequence()
		if !ok || metadata.Identity == 0 || metadata.Identity != metadata.Incarnation {
			return nil, bindError("22023", "owned sequence has no new committed allocation identity", 0)
		}
		expected, err := pending.Finalize(ctx, metadata.Incarnation)
		if err != nil {
			return nil, err
		}
		actualSpec, err := actual.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		expectedSpec, err := expected.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		if actual.OID() != expected.OID() || actual.Name() != expected.Name() || actual.Version() != expected.Version() ||
			actual.State() != expected.State() || actualSpec.Physical != expectedSpec.Physical || *actualSpec.Sequence != *expectedSpec.Sequence ||
			!slices.Equal(actualSpec.Dependencies, expectedSpec.Dependencies) {
			return nil, bindError("22023", "owned sequence does not match its reserved declaration", 0)
		}
		for i := range actualSpec.Dependencies {
			if actualSpec.Dependencies[i].Object == next.definition.TargetTable.ID() {
				actualSpec.Dependencies[i].Version = next.definition.TargetTable.Version()
			}
		}
		rebased, err := model.NewDescriptor(ctx, actualSpec, limits)
		if err != nil {
			return nil, err
		}
		position := slices.IndexFunc(next.transactionalCreates, func(candidate *model.Descriptor) bool {
			return candidate != nil && candidate.ID() == rebased.ID()
		})
		if position < 0 {
			return nil, model.ErrInvalidDescriptor
		}
		next.transactionalCreates[position] = rebased
	}
	next.sequences = nil
	return &next, ctx.Err()
}
