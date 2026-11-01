package bind

import (
	"context"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type generatedColumnDefinition struct {
	raw    *zpgparse.Node
	stored bool
}

type pendingTableDeclaration struct {
	spec         model.DescriptorSpec
	defaults     map[model.ColumnID]*zpgparse.Node
	generated    map[model.ColumnID]generatedColumnDefinition
	checks       []tableConstraint
	checkIndexes []int
	indexes      []*model.Descriptor
	inheritance  *tableInheritancePlan
	like         *tableLikePlan
	partitionSpec *zpgparse.PartitionSpec
	partitionBound *zpgparse.PartitionBoundSpec
	options      CreateDeclarationOptions
	rowTypeID    model.ObjectID
	rowTypeOID   model.OID
	arrayTypeID  model.ObjectID
	arrayTypeOID model.OID
	ids          map[model.ObjectID]bool
	dependencies []model.Dependency
	warnings     []model.Error
}
type pendingIdentityDeclaration struct {
	base         *model.Descriptor
	spec         model.DescriptorSpec
	options      CreateDeclarationOptions
	dependencies []model.Dependency
	warnings     []model.Error
	validation   model.ObjectID
}


// PendingSequences returns owned configurations whose allocation state must be
// initialized before the table's expressions can capture sequence identities.
func (d *BoundDeclaration) PendingSequences() []*BoundSequenceDefinition {
	return slices.Clone(d.sequences)
}

// FinalizeSequences binds against real initialized descriptors without publishing
// them. Failure leaves the pending declaration reusable and exposes no table.
func (d *BoundDeclaration) FinalizeSequences(ctx context.Context, snapshot CatalogSnapshot, sequences []*model.Descriptor) (*BoundDeclaration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if (d.pending == nil) == (d.identity == nil) || len(sequences) != len(d.sequences) || snapshot == nil {
		return nil, bindError("22023", "sequence finalization requires a pending declaration and all owned sequences", 0)
	}
	ordered := make([]*model.Descriptor, len(d.sequences))
	for i, pending := range d.sequences {
		for _, descriptor := range sequences {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if descriptor == nil || descriptor.ID() != pending.draft.ID {
				continue
			}
			if ordered[i] != nil {
				return nil, bindError("22023", "duplicate owned sequence", 0)
			}
			actual, ok := descriptor.Sequence()
			if !ok || actual.Identity == 0 || actual.Identity != actual.Incarnation {
				return nil, bindError("22023", "owned sequence has no new committed allocation identity", 0)
			}
			expected, err := pending.Finalize(ctx, actual.Incarnation)
			if err != nil {
				return nil, err
			}
			actualSpec, err := descriptor.SpecWithin(ctx, pending.limits)
			if err != nil {
				return nil, err
			}
			expectedSpec, err := expected.SpecWithin(ctx, pending.limits)
			if err != nil {
				return nil, err
			}
			identityMismatch := descriptor.OID() != expected.OID() || descriptor.Name() != expected.Name()
			versionMismatch := descriptor.Version() != expected.Version() || descriptor.State() != expected.State()
			allocationMismatch := actualSpec.Physical != expectedSpec.Physical || *actualSpec.Sequence != *expectedSpec.Sequence
			if identityMismatch || versionMismatch || allocationMismatch || !slices.Equal(actualSpec.Dependencies, expectedSpec.Dependencies) {
				return nil, bindError("22023", "owned sequence does not match its reserved declaration", 0)
			}
			ordered[i] = descriptor
		}
		if ordered[i] == nil {
			return nil, bindError("22023", "missing owned sequence", 0)
		}
	}
	if d.pending != nil {
		return d.pending.compile(ctx, snapshot, ordered)
	}
	return d.identity.compile(ctx, ordered)
}
func (d *pendingIdentityDeclaration) compile(ctx context.Context, sequences []*model.Descriptor) (*BoundDeclaration, error) {
	if d == nil || d.base == nil || len(sequences) != 1 {
		return nil, model.ErrInvalidDescriptor
	}
	sequence := sequences[0]
	if sequence == nil || sequence.Kind() != model.ObjectSequence {
		return nil, model.ErrInvalidDescriptor
	}
	if d.spec.Version == ^uint64(0) {
		return nil, model.ErrDescriptorLimit
	}
	d.spec.Version++
	d.spec.Dependencies = append(d.spec.Dependencies, model.Dependency{Object: sequence.ID(), Version: sequence.Version()})
	next, err := model.NewDescriptor(ctx, d.spec, d.options.Descriptors)
	if err != nil {
		return nil, err
	}
	var validations []model.ObjectID
	if d.validation != 0 {
		validations = []model.ObjectID{d.validation}
	}
	return &BoundDeclaration{
		descriptors:  []*model.Descriptor{sequence},
		updates:      []ObjectMutation{{Base: d.base, Next: next}},
		validations:  validations,
		dependencies: slices.Clone(d.dependencies),
		warnings:     slices.Clone(d.warnings),
	}, ctx.Err()
}


func (b *binder) declarationColumnType(raw *zpgparse.TypeName) (model.TypeLayout, bool, error) {
	if raw != nil && len(raw.Names) == 1 && !raw.PctType {
		var id model.TypeID
		switch raw.Names[0].GetString_().GetSval() {
		case "smallserial", "serial2":
			id = 21
		case "serial", "serial4":
			id = 23
		case "bigserial", "serial8":
			id = 20
		}
		if id != 0 {
			if len(raw.ArrayBounds) != 0 {
				return model.TypeLayout{}, false, missing("array of serial", int(raw.Location)+1)
			}
			if raw.Setof || raw.TypeOid != 0 || len(raw.Typmods) != 0 || raw.Typemod != -1 {
				return model.TypeLayout{}, false, bindError("42601", "invalid serial type decoration", int(raw.Location)+1)
			}
			return b.primitive(id), true, nil
		}
	}
	layout, err := b.typeName(raw)
	return layout, false, err
}

func (s *declarationState) ownedSequences(spec model.DescriptorSpec, serials map[model.ColumnID]bool, constraints []tableConstraint, defaults map[model.ColumnID]*zpgparse.Node) ([]*BoundSequenceDefinition, []tableConstraint, error) {
	identities := make(map[model.ColumnID]*zpgparse.Constraint)
	for _, item := range constraints {
		if err := s.b.ctx.Err(); err != nil {
			return nil, nil, err
		}
		if item.raw.Contype != zpgparse.ConstrType_CONSTR_IDENTITY {
			continue
		}
		if err := ordinaryConstraint(item.raw); err != nil {
			return nil, nil, err
		}
		if item.column == 0 || identities[item.column] != nil || serials[item.column] {
			return nil, nil, bindError("42601", "multiple or misplaced identity declarations", 0)
		}
		if item.raw.GeneratedWhen != "a" && item.raw.GeneratedWhen != "d" {
			return nil, nil, bindError("42601", "identity requires ALWAYS or BY DEFAULT", 0)
		}
		if item.raw.GeneratedKind != "" {
			return nil, nil, bindError("42601", "identity cannot have generated storage attributes", 0)
		}
		identities[item.column] = item.raw
	}
	var sequences []*BoundSequenceDefinition
	for i := range spec.Table.Columns {
		if err := s.b.ctx.Err(); err != nil {
			return nil, nil, err
		}
		column := &spec.Table.Columns[i]
		identity := identities[column.ID]
		if identity == nil && !serials[column.ID] {
			continue
		}
		if s.options.Physical.Generation == 0 || s.options.Physical.EncodingVersion == 0 {
			return nil, nil, model.ErrInvalidDescriptor
		}
		integerType := column.Layout.Type.ID == 20 || column.Layout.Type.ID == 21 || column.Layout.Type.ID == 23
		if !integerType || len(column.Layout.Children) != 0 {
			return nil, nil, bindError("22023", "identity type must be smallint, integer, or bigint", 0)
		}
		options, explicit, err := s.identityOptions(identity)
		if err != nil {
			return nil, nil, err
		}
		name, err := s.constraintName(explicit, column.Name, "seq", true)
		if err != nil {
			return nil, nil, err
		}
		id, oid, err := s.allocate()
		if err != nil {
			return nil, nil, err
		}
		configuration := &model.SequenceMetadata{Owner: s.options.Owner, Type: column.Layout.Type, Increment: 1, Cache: 1, OwnerTable: spec.ID, OwnerColumn: column.ID}
		if spec.Table != nil {
			configuration.Persistence = spec.Table.Persistence
		}
		sequence := &BoundSequenceDefinition{limits: s.options.Descriptors, draft: model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: s.parent, Kind: model.ObjectSequence, Name: name}, Version: 1, State: model.StatePublic, Physical: s.options.Physical, Sequence: configuration}}
		if err := s.b.sequenceOptions(sequence, options); err != nil {
			return nil, nil, err
		}
		sequence.dependencies = slices.Clone(s.b.dependencies)
		sequence.draft.Dependencies = append(slices.Clone(sequence.dependencies), model.Dependency{Object: spec.ID, Version: spec.Version, Column: column.ID})
		sequences = append(sequences, sequence)
		column.Nullable = false
		if identity != nil {
			column.IdentitySequence, column.IdentityAlways = id, identity.GeneratedWhen == "a"
		} else {
			parent, err := s.b.catalog.Descriptor(s.b.ctx, s.parent)
			if err != nil {
				return nil, nil, err
			}
			qualified := `"` + strings.ReplaceAll(parent.Descriptor.Name().Name, `"`, `""`) + `"."` + strings.ReplaceAll(name, `"`, `""`) + `"`
			parsed, err := zpgparse.Parse("SELECT pg_catalog.nextval('" + strings.ReplaceAll(qualified, "'", "''") + "'::pg_catalog.regclass)")
			if err != nil {
				return nil, nil, err
			}
			defaults[column.ID] = parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
			call := defaults[column.ID].GetFuncCall()
			call.Location = -1
			cast := call.Args[0].GetTypeCast()
			cast.Location, cast.TypeName.Location = -1, -1
			cast.Arg.GetAConst().Location = -1
		}
		constraints = append(constraints, tableConstraint{column: column.ID, raw: &zpgparse.Constraint{Contype: zpgparse.ConstrType_CONSTR_NOTNULL, IsEnforced: true}})
		if len(constraints) > s.options.Descriptors.MaxItems || len(sequences) > s.options.Descriptors.MaxItems {
			return nil, nil, model.ErrDescriptorLimit
		}
	}
	return sequences, constraints, nil
}

func (s *declarationState) identityOptions(identity *zpgparse.Constraint) ([]*zpgparse.Node, string, error) {
	if identity == nil {
		return nil, "", nil
	}
	if len(identity.Options) > s.options.Descriptors.MaxItems {
		return nil, "", model.ErrDescriptorLimit
	}
	var options []*zpgparse.Node
	var explicit string
	for _, node := range identity.Options {
		if err := s.b.ctx.Err(); err != nil {
			return nil, "", err
		}
		option := node.GetDefElem()
		if option == nil {
			return nil, "", bindError("42601", "invalid identity sequence option", 0)
		}
		switch option.Defname {
		case "sequence_name":
			if explicit != "" {
				return nil, "", bindError("42601", "redundant sequence name", 0)
			}
			var err error
			explicit, err = s.identitySequenceName(option)
			if err != nil {
				return nil, "", err
			}
		case "as":
			return nil, "", bindError("42601", "conflicting or redundant sequence type", 0)
		case "owned_by", "restart", "logged", "unlogged":
			return nil, "", missing("identity sequence option "+option.Defname, int(option.Location)+1)
		default:
			options = append(options, node)
		}
	}
	return options, explicit, nil
}

func (s *declarationState) identitySequenceName(option *zpgparse.DefElem) (string, error) {
	if option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC || option.Arg.GetList() == nil {
		return "", bindError("42601", "invalid sequence name option", 0)
	}
	parts, err := names(option.Arg.GetList().Items)
	if err != nil {
		return "", err
	}
	if len(parts) < 1 || len(parts) > 2 {
		return "", bindError("42601", "invalid owned sequence name", 0)
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	parent, err := s.b.catalog.Resolve(s.b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: parts[0]})
	if err != nil {
		return "", err
	}
	if parent.Descriptor.ID() != s.parent {
		return "", bindError("55000", "owned sequence must be in the same schema as its table", 0)
	}
	return parts[1], nil
}
