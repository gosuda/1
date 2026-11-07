package bind

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// BoundSequenceDefinition owns a configuration without autonomous allocation state.
// Finalize requires the incarnation committed by the caller's sequence service.
type BoundSequenceDefinition struct {
	base, existing *model.Descriptor
	draft          model.DescriptorSpec
	limits         model.DescriptorLimits
	dependencies   []model.Dependency
	warnings       []model.Error
	restart        int64
	hasRestart     bool
	rewrite        bool
}

func (d *BoundSequenceDefinition) Base() *model.Descriptor     { return d.base }
func (d *BoundSequenceDefinition) Existing() *model.Descriptor { return d.existing }
func (d *BoundSequenceDefinition) Configuration() model.SequenceMetadata {
	if d.draft.Sequence == nil {
		return model.SequenceMetadata{}
	}
	return *d.draft.Sequence
}
func (d *BoundSequenceDefinition) Restart() (int64, bool) { return d.restart, d.hasRestart }
func (d *BoundSequenceDefinition) RewritesState() bool    { return d.rewrite }
func (d *BoundSequenceDefinition) Skipped() bool          { return d.draft.Sequence == nil }
func (d *BoundSequenceDefinition) Dependencies() []model.Dependency {
	return slices.Clone(d.dependencies)
}
func (d *BoundSequenceDefinition) Warnings() []model.Error { return slices.Clone(d.warnings) }
func (d *BoundSequenceDefinition) Finalize(ctx context.Context, incarnation uint64) (*model.Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.draft.Sequence == nil {
		return nil, nil
	}
	reused := d.base != nil && incarnation == d.draft.Sequence.Incarnation
	if incarnation == 0 || (d.base != nil && d.rewrite == reused) {
		return nil, bindError("22023", "sequence incarnation does not match rewrite ownership", 0)
	}
	spec := d.draft
	configuration := *spec.Sequence
	configuration.Incarnation = incarnation
	if d.base == nil {
		configuration.Identity = incarnation
	}
	spec.Sequence = &configuration
	next, err := model.NewDescriptor(ctx, spec, d.limits)
	if err != nil {
		return nil, err
	}
	if d.base != nil {
		if err := d.base.ValidateSuccessor(ctx, next); err != nil {
			return nil, err
		}
	}
	return next, nil
}

func sequenceDeclarationBinder(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node, relation *zpgparse.RangeVar) (*binder, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, bindError("22023", "sequence binding requires a catalog", 0)
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	remaining, payload := options.Bind.Limits.MaxNodes, options.Bind.Limits.MaxBytes
	if err := b.checkAST(node.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	if err := ValidateStatementFeatures(ctx, node); err != nil {
		return nil, err
	}
	if relation.Catalogname != "" {
		return nil, missing("cross-database sequence declaration", 0)
	}
	if relation.Alias != nil {
		return nil, missing("sequence declaration alias", 0)
	}
	if relation.Relpersistence == "t" {
		return nil, missing("temporary sequence declaration", 0)
	}
	if relation.Relpersistence != "" && relation.Relpersistence != "p" {
		return nil, missing("unlogged sequence persistence", 0)
	}
	if err := declarationName(relation.Relname); err != nil {
		return nil, err
	}
	if relation.Schemaname != "" {
		if err := declarationName(relation.Schemaname); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func BindCreateSequence(ctx context.Context, statement *zpgparse.CreateSeqStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSequenceDefinition, error) {
	if statement == nil || statement.Sequence == nil {
		return nil, bindError("42601", "missing CREATE SEQUENCE relation", 0)
	}
	b, err := sequenceDeclarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateSeqStmt{CreateSeqStmt: statement}}, statement.Sequence)
	if err != nil {
		return nil, err
	}
	if statement.ForIdentity || statement.OwnerId != 0 {
		return nil, missing("identity-internal sequence declaration", 0)
	}
	r := statement.Sequence
	path := options.Bind.SearchPath
	if r.Schemaname != "" {
		path = []string{r.Schemaname}
	}
	var parent *model.Descriptor
	for _, name := range path {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.Descriptor.State() == model.StatePublic {
			parent = p.Descriptor
			break
		}
	}
	if parent == nil {
		return nil, bindError("3F000", "no schema selected for creation", 0)
	}
	b.dependency(parent, 0)
	existing, err := b.indexNameConflict(parent.ID(), r.Relname)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !statement.IfNotExists {
			return nil, bindError("42P07", "relation already exists", 0)
		}
		b.dependency(existing, 0)
		return &BoundSequenceDefinition{existing: existing, dependencies: b.dependencies, warnings: []model.Error{{Code: "42P07", Message: "relation already exists, skipping"}}}, ctx.Err()
	}
	persistence := r.Relpersistence
	if persistence == "" {
		persistence = "p"
	}
	d := &BoundSequenceDefinition{limits: options.Descriptors, draft: model.DescriptorSpec{Name: model.NamespaceName{Parent: parent.ID(), Kind: model.ObjectSequence, Name: r.Relname}, Version: 1, State: model.StatePublic, Physical: options.Physical, Sequence: &model.SequenceMetadata{Owner: options.Owner, Type: b.primitive(20).Type, Increment: 1, Cache: 1, Persistence: persistence}}}
	if err := b.sequenceOptions(d, statement.Options); err != nil {
		return nil, err
	}
	if options.Physical.Generation == 0 || options.Physical.EncodingVersion == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	if len(b.dependencies) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	if options.Identities == nil {
		return nil, bindError("22023", "sequence creation requires identity allocation", 0)
	}
	state := declarationState{b: b, options: options, parent: parent.ID(), ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool)}
	id, oid, err := state.allocate()
	if err != nil {
		return nil, err
	}
	d.draft.ID, d.draft.OID = id, oid
	d.dependencies = b.dependencies
	d.draft.Dependencies = slices.DeleteFunc(slices.Clone(b.dependencies), func(dep model.Dependency) bool {
		return dep.Object == d.draft.Sequence.OwnerTable && dep.Column == 0
	})
	return d, ctx.Err()
}

func BindAlterSequence(ctx context.Context, statement *zpgparse.AlterSeqStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSequenceDefinition, error) {
	if statement == nil || statement.Sequence == nil {
		return nil, bindError("42601", "missing ALTER SEQUENCE relation", 0)
	}
	b, err := sequenceDeclarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterSeqStmt{AlterSeqStmt: statement}}, statement.Sequence)
	if err != nil {
		return nil, err
	}
	if statement.ForIdentity {
		return nil, missing("identity-internal sequence alteration", 0)
	}
	parts := []string{statement.Sequence.Relname}
	if statement.Sequence.Schemaname != "" {
		parts = []string{statement.Sequence.Schemaname, statement.Sequence.Relname}
	}
	base, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable, model.ObjectSequence, model.ObjectIndex, model.ObjectView, model.ObjectMaterializedView}, "42P01", 0)
	if err != nil {
		var sqlError *model.Error
		if statement.MissingOk && errors.As(err, &sqlError) && sqlError.Code == "42P01" {
			return &BoundSequenceDefinition{dependencies: b.dependencies, warnings: []model.Error{{Code: "42P01", Message: "sequence does not exist, skipping"}}}, ctx.Err()
		}
		return nil, err
	}
	if base.Kind() != model.ObjectSequence {
		return nil, bindError("42809", "relation is not a sequence", 0)
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Version == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	spec.Version++
	d := &BoundSequenceDefinition{base: base, draft: spec, limits: options.Descriptors}
	oldType := spec.Sequence.Type
	if err := b.sequenceOptions(d, statement.Options); err != nil {
		return nil, err
	}
	if oldType != d.draft.Sequence.Type {
		if d.draft.Physical.Generation == math.MaxUint64 {
			return nil, model.ErrInvalidDescriptor
		}
		d.draft.Physical.Generation++
	}
	for i, dep := range d.draft.Dependencies {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		published, err := snapshot.Descriptor(ctx, dep.Object)
		if err != nil {
			return nil, err
		}
		b.dependency(published.Descriptor, dep.Column)
		d.draft.Dependencies[i].Version = published.Descriptor.Version()
	}
	if owner := d.draft.Sequence.OwnerTable; owner != 0 {
		published, err := snapshot.Descriptor(ctx, owner)
		if err != nil {
			return nil, err
		}
		b.dependency(published.Descriptor, d.draft.Sequence.OwnerColumn)
	}
	if len(d.draft.Dependencies) > options.Descriptors.MaxItems || len(b.dependencies) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	d.dependencies = b.dependencies
	return d, ctx.Err()
}

func sequenceTypeBounds(id model.TypeID) (int64, int64) {
	switch id {
	case 21:
		return math.MinInt16, math.MaxInt16
	case 23:
		return math.MinInt32, math.MaxInt32
	default:
		return math.MinInt64, math.MaxInt64
	}
}

func sequenceInteger(option *zpgparse.DefElem) (int64, error) {
	if n := option.Arg.GetInteger(); n != nil {
		return int64(n.Ival), nil
	}
	if n := option.Arg.GetFloat(); n != nil {
		value, err := strconv.ParseInt(n.Fval, 10, 64)
		if err == nil {
			return value, nil
		}
		return 0, bindError("22003", "sequence option is outside bigint range", int(option.Location)+1)
	}
	return 0, bindError("42601", "sequence option requires an integer", int(option.Location)+1)
}

func (b *binder) sequenceOptions(d *BoundSequenceDefinition, nodes []*zpgparse.Node) error {
	if len(nodes) > d.limits.MaxItems {
		return model.ErrDescriptorLimit
	}
	d.rewrite = d.base == nil
	options := make(map[string]*zpgparse.DefElem, len(nodes))
	for _, node := range nodes {
		if err := b.ctx.Err(); err != nil {
			return err
		}
		option := node.GetDefElem()
		if option == nil {
			return bindError("42601", "invalid sequence option", 0)
		}
		if option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC {
			return missing("sequence option action or namespace", int(option.Location)+1)
		}
		switch option.Defname {
		case "as", "increment", "minvalue", "maxvalue", "start", "restart", "cache", "cycle", "owned_by":
		default:
			return missing("sequence option "+option.Defname, int(option.Location)+1)
		}
		if options[option.Defname] != nil {
			return bindError("42601", "conflicting or redundant sequence options", int(option.Location)+1)
		}
		options[option.Defname] = option
		if option.Defname != "owned_by" {
			d.rewrite = true
		}
	}
	s := d.draft.Sequence
	oldMin, oldMax := sequenceTypeBounds(s.Type.ID)
	resetMin, resetMax := false, false
	if option := options["as"]; option != nil {
		typ, err := b.typeName(option.Arg.GetTypeName())
		if err != nil {
			return err
		}
		integer := typ.Type.ID == 20 || typ.Type.ID == 21 || typ.Type.ID == 23
		if len(typ.Children) != 0 || !integer {
			return bindError("22023", "sequence type must be smallint, integer, or bigint", int(option.Location)+1)
		}
		resetMin, resetMax = d.base != nil && s.Min == oldMin, d.base != nil && s.Max == oldMax
		s.Type = typ.Type
	}
	for _, field := range []struct {
		name  string
		value *int64
	}{{"increment", &s.Increment}, {"cache", &s.Cache}} {
		if option := options[field.name]; option != nil {
			value, err := sequenceInteger(option)
			if err != nil {
				return err
			}
			*field.value = value
		}
	}
	minType, maxType := sequenceTypeBounds(s.Type.ID)
	for _, field := range []struct {
		name     string
		value    *int64
		reset    bool
		fallback int64
	}{{"minvalue", &s.Min, resetMin, 1}, {"maxvalue", &s.Max, resetMax, -1}} {
		option := options[field.name]
		if option != nil && option.Arg != nil {
			value, err := sequenceInteger(option)
			if err != nil {
				return err
			}
			*field.value = value
		} else if d.base == nil || option != nil || field.reset {
			*field.value = field.fallback
			if field.name == "minvalue" && (s.Increment < 0 || resetMin) {
				*field.value = minType
			}
			if field.name == "maxvalue" && (s.Increment > 0 || resetMax) {
				*field.value = maxType
			}
		}
	}
	if option := options["start"]; option != nil {
		value, err := sequenceInteger(option)
		if err != nil {
			return err
		}
		s.Start = value
	} else if d.base == nil {
		s.Start = s.Min
		if s.Increment < 0 {
			s.Start = s.Max
		}
	}
	if option := options["cycle"]; option != nil {
		value := option.Arg.GetBoolean()
		if value == nil {
			return bindError("42601", "CYCLE requires a boolean", int(option.Location)+1)
		}
		s.Cycle = value.Boolval
	}
	if err := model.ValidateSequenceConfiguration(*s); err != nil {
		return bindError("22023", "invalid sequence range, start, increment, or cache", 0)
	}
	if option := options["restart"]; option != nil {
		d.restart, d.hasRestart = s.Start, true
		if option.Arg != nil {
			value, err := sequenceInteger(option)
			if err != nil {
				return err
			}
			d.restart = value
		}
		if d.restart < s.Min || d.restart > s.Max {
			return bindError("22023", "RESTART value is outside sequence bounds", int(option.Location)+1)
		}
	}
	if option := options["owned_by"]; option != nil {
		if err := b.sequenceOwner(d, option); err != nil {
			return sequenceOwnershipError(err)
		}
	}
	return b.ctx.Err()
}

func (b *binder) sequenceOwner(d *BoundSequenceDefinition, option *zpgparse.DefElem) error {
	list := option.Arg.GetList()
	if list == nil {
		return bindError("42601", "OWNED BY requires a column name", int(option.Location)+1)
	}
	parts, err := names(list.Items)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if err := declarationName(part); err != nil {
			return err
		}
	}
	s := d.draft.Sequence
	oldTable, oldColumn := s.OwnerTable, s.OwnerColumn
	if len(parts) == 1 && parts[0] == "none" {
		s.OwnerTable, s.OwnerColumn = 0, 0
	} else {
		if len(parts) < 2 || len(parts) > 3 {
			return bindError("42601", "OWNED BY requires table.column or schema.table.column", int(option.Location)+1)
		}
		table, _, err := b.resolve(parts[:len(parts)-1], []model.ObjectKind{model.ObjectTable, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView}, "42P01", int(option.Location)+1)
		if err != nil {
			return err
		}
		if table.Kind() != model.ObjectTable {
			return bindError("42809", "sequence owner must be a table column", int(option.Location)+1)
		}
		if table.Name().Parent != d.draft.Name.Parent {
			return bindError("55000", "sequence must be in same schema as table it is linked to", int(option.Location)+1)
		}
		spec, err := b.descriptorSpec(table)
		if err != nil {
			return err
		}
		found := false
		for _, column := range spec.Table.Columns {
			if err := b.ctx.Err(); err != nil {
				return err
			}
			if column.Name == parts[len(parts)-1] && column.State == model.StatePublic && !column.SystemIdentity {
				s.OwnerTable, s.OwnerColumn = table.ID(), column.ID
				b.dependency(table, column.ID)
				found = true
				break
			}
		}
		if !found {
			return bindError("42703", "sequence owner column does not exist", int(option.Location)+1)
		}
	}
	if err := b.rejectIdentityOwnerChange(d.draft.ID, oldTable, oldColumn); err != nil {
		return err
	}
	d.draft.Dependencies = slices.DeleteFunc(d.draft.Dependencies, func(dep model.Dependency) bool {
		return dep.Object == oldTable && oldColumn != 0 && (dep.Column == oldColumn || dep.Column == 0)
	})
	if s.OwnerTable != 0 {
		for _, dep := range b.dependencies {
			if dep.Object == s.OwnerTable && dep.Column == s.OwnerColumn && !slices.Contains(d.draft.Dependencies, dep) {
				d.draft.Dependencies = append(d.draft.Dependencies, dep)
			}
		}
	}
	return nil
}
