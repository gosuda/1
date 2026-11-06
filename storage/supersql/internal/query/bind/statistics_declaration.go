package bind

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type StatisticsMutation struct {
	Base *model.Descriptor
	Next *model.Descriptor
}

type BoundStatisticsCommand struct {
	updates      []StatisticsMutation
	dependencies []model.Dependency
	warnings     []model.Error
	tag          string
}

func (c *BoundStatisticsCommand) Updates() []StatisticsMutation {
	return slices.Clone(c.updates)
}
func (c *BoundStatisticsCommand) Dependencies() []model.Dependency {
	return slices.Clone(c.dependencies)
}
func (c *BoundStatisticsCommand) Warnings() []model.Error { return slices.Clone(c.warnings) }
func (c *BoundStatisticsCommand) CommandTag() string      { return c.tag }

func BindStatisticsCommand(ctx context.Context, node *zpgparse.Node, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, bool, error) {
	if node == nil {
		return nil, false, bindError("42601", "missing statistics command", 0)
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_CreateStatsStmt:
		command, err := bindCreateStatistics(ctx, statement.CreateStatsStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterStatsStmt:
		command, err := bindAlterStatistics(ctx, statement.AlterStatsStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_RenameStmt:
		if statement.RenameStmt == nil || statement.RenameStmt.RenameType != zpgparse.ObjectType_OBJECT_STATISTIC_EXT {
			return nil, false, nil
		}
		command, err := bindRenameStatistics(ctx, statement.RenameStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterObjectSchemaStmt:
		if statement.AlterObjectSchemaStmt == nil || statement.AlterObjectSchemaStmt.ObjectType != zpgparse.ObjectType_OBJECT_STATISTIC_EXT {
			return nil, false, nil
		}
		command, err := bindSetStatisticsSchema(ctx, statement.AlterObjectSchemaStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_DropStmt:
		if statement.DropStmt == nil || statement.DropStmt.RemoveType != zpgparse.ObjectType_OBJECT_STATISTIC_EXT {
			return nil, false, nil
		}
		command, err := bindDropStatistics(ctx, statement.DropStmt, snapshot, options)
		return command, true, err
	default:
		return nil, false, nil
	}
}

func bindCreateStatistics(ctx context.Context, statement *zpgparse.CreateStatsStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, error) {
	if statement == nil || len(statement.Relations) != 1 || statement.Transformed {
		return nil, bindError("0A000", "CREATE STATISTICS requires one ordinary table", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateStatsStmt{CreateStatsStmt: statement}})
	if err != nil {
		return nil, err
	}
	relation := statement.Relations[0].GetRangeVar()
	if relation == nil || relation.Relname == "" {
		return nil, bindError("42601", "missing CREATE STATISTICS relation", 0)
	}
	parts := []string{relation.Relname}
	if relation.Schemaname != "" {
		parts = []string{relation.Schemaname, relation.Relname}
	}
	table, schema, err := resolveNamespaceObject(ctx, snapshot, options.Bind.SearchPath, parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(relation.Location)+1)
	if err != nil {
		return nil, err
	}
	if table.State() != model.StatePublic {
		return nil, bindError("42P01", "relation does not exist", int(relation.Location)+1)
	}
	b.dependency(schema, 0)
	b.dependency(table, 0)
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, bindError("42809", "statistics require a table", int(relation.Location)+1)
	}
	if spec.Table.Owner != options.Owner {
		return nil, bindError("42501", "must be owner of table to create statistics", int(relation.Location)+1)
	}
	nameParts, err := names(statement.Defnames)
	if err != nil {
		return nil, err
	}
	if len(nameParts) > 2 {
		return nil, bindError("42601", "statistics name requires an optionally qualified name", 0)
	}
	name := ""
	statisticsSchema := schema
	if len(nameParts) != 0 {
		name = nameParts[len(nameParts)-1]
		if err := declarationName(name); err != nil {
			return nil, err
		}
		if len(nameParts) == 2 {
			published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: nameParts[0]})
			if err != nil {
				if isMissingObject(err) {
					return nil, bindError("3F000", "statistics schema does not exist", 0)
				}
				return nil, err
			}
			statisticsSchema = published.Descriptor
		} else {
			for _, schemaName := range options.Bind.SearchPath {
				published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
				if isMissingObject(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if published.Descriptor.State() == model.StatePublic {
					statisticsSchema = published.Descriptor
					break
				}
			}
		}
	}
	b.dependency(statisticsSchema, 0)
	keys, expressions, bindings, err := bindStatisticsElements(ctx, b, statement.Exprs, table, spec.Table.Columns, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	if len(keys)+len(expressions) < 2 {
		return nil, bindError("42P17", "statistics require at least two columns or expressions", 0)
	}
	kinds, err := statisticsKinds(statement.StatTypes, len(expressions) != 0)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = automaticStatisticsName(table.Name().Name, keys, expressions, spec.Table.Columns)
	}
	conflict, err := findStatisticsInSchema(ctx, snapshot, statisticsSchema.ID(), name, options.Descriptors, options.Bind.Limits.MaxNodes)
	if err != nil {
		return nil, err
	}
	if conflict.table != nil {
		b.dependency(conflict.table, 0)
		if !statement.IfNotExists {
			return nil, bindError("42710", "statistics object already exists", 0)
		}
		return &BoundStatisticsCommand{dependencies: b.dependencies, warnings: []model.Error{{Code: "42710", Message: "statistics object already exists, skipping"}}, tag: "CREATE STATISTICS"}, nil
	}
	_, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	descriptor := model.ExtendedStatisticDescriptor{OID: oid, Name: name, Description: statement.Stxcomment, Owner: options.Owner, Namespace: statisticsSchema.ID(), Keys: keys, Expressions: expressions, Kinds: kinds}
	spec.Table.ExtendedStatistics = append(spec.Table.ExtendedStatistics, descriptor)
	slices.SortFunc(spec.Table.ExtendedStatistics, func(a, b model.ExtendedStatisticDescriptor) int { return cmp.Compare(a.OID, b.OID) })
	dependencies := slices.Clone(b.dependencies)
	spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: statisticsSchema.ID(), Version: statisticsSchema.Version()})
	for _, binding := range bindings {
		for _, dependency := range binding.Dependencies {
			if dependency.Object == table.ID() {
				continue
			}
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			dependencies = appendIndexDependency(dependencies, dependency)
		}
	}
	slices.SortFunc(spec.Dependencies, compareDependency)
	spec.Version++
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := table.ValidateSuccessor(ctx, next); err != nil {
		return nil, err
	}
	return &BoundStatisticsCommand{updates: []StatisticsMutation{{Base: table, Next: next}}, dependencies: dependencies, tag: "CREATE STATISTICS"}, ctx.Err()
}

type statisticsElementBinding struct {
	Dependencies []model.Dependency
}

func bindStatisticsElements(ctx context.Context, b *binder, elements []*zpgparse.Node, table *model.Descriptor, columns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options) ([]model.ColumnID, []*model.ExpressionDefinition, []statisticsElementBinding, error) {
	keys := make([]model.ColumnID, 0, len(elements))
	expressions := make([]*model.ExpressionDefinition, 0, len(elements))
	bindings := make([]statisticsElementBinding, 0, len(elements))
	for _, node := range elements {
		element := node.GetStatsElem()
		if element == nil || (element.Name == "") == (element.Expr == nil) {
			return nil, nil, nil, bindError("42601", "invalid statistics element", 0)
		}
		if element.Name != "" {
			column, err := publicIndexColumn(columns, element.Name)
			if err != nil {
				return nil, nil, nil, err
			}
			if slices.Contains(keys, column.ID) {
				return nil, nil, nil, bindError("42701", "statistics column specified more than once", 0)
			}
			keys = append(keys, column.ID)
			continue
		}
		definition, binding, err := DefineStatisticsExpression(ctx, element.Expr, table, columns, snapshot, options)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, existing := range expressions {
			if existing.SQL == definition.SQL {
				return nil, nil, nil, bindError("42701", "statistics expression specified more than once", 0)
			}
		}
		expressions = append(expressions, definition)
		bindings = append(bindings, statisticsElementBinding{Dependencies: binding.Dependencies})
	}
	slices.Sort(keys)
	return keys, expressions, bindings, nil
}

func statisticsKinds(nodes []*zpgparse.Node, expressions bool) ([]byte, error) {
	kinds := make([]byte, 0, 4)
	if len(nodes) == 0 {
		kinds = append(kinds, 'd', 'f', 'm')
	} else {
		for _, node := range nodes {
			name := node.GetString_().GetSval()
			kind := byte(0)
			switch name {
			case "ndistinct":
				kind = 'd'
			case "dependencies":
				kind = 'f'
			case "mcv":
				kind = 'm'
			default:
				return nil, bindError("0A000", "unsupported statistics kind", 0)
			}
			if slices.Contains(kinds, kind) {
				return nil, bindError("42701", "statistics kind specified more than once", 0)
			}
			kinds = append(kinds, kind)
		}
	}
	if expressions {
		kinds = append(kinds, 'e')
	}
	slices.Sort(kinds)
	return kinds, nil
}

func automaticStatisticsName(table string, keys []model.ColumnID, expressions []*model.ExpressionDefinition, columns []model.ColumnDescriptor) string {
	name := table
	for _, key := range keys {
		for _, column := range columns {
			if column.ID == key {
				name += "_" + column.Name
				break
			}
		}
	}
	if len(expressions) != 0 {
		name += "_expr"
	}
	return clipIndexIdentifier(name, 58) + "_stat"
}

type foundStatistic struct {
	table     *model.Descriptor
	statistic model.ExtendedStatisticDescriptor
}

func findStatisticsInSchema(ctx context.Context, snapshot CatalogSnapshot, namespace model.ObjectID, name string, limits model.DescriptorLimits, maximum uint32) (foundStatistic, error) {
	var found foundStatistic
	visited := uint32(0)
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if found.table != nil {
			return nil
		}
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if visited >= maximum {
				return bindError("54000", "statistics namespace scan limit exceeded", 0)
			}
			visited++
			spec, err := published.Descriptor.SpecWithin(ctx, limits)
			if err != nil {
				return err
			}
			if spec.Table == nil {
				return nil
			}
			for _, statistic := range spec.Table.ExtendedStatistics {
				if statistic.Namespace == namespace && statistic.Name == name {
					found = foundStatistic{table: published.Descriptor, statistic: statistic}
					break
				}
			}
			return nil
		})
	})
	return found, err
}

func resolveStatistics(ctx context.Context, b *binder, parts []string, limits model.DescriptorLimits) (foundStatistic, error) {
	if len(parts) == 0 || len(parts) > 2 {
		return foundStatistic{}, bindError("42704", "statistics object does not exist", 0)
	}
	path := b.options.SearchPath
	if len(parts) == 2 {
		path = []string{parts[0]}
	}
	for _, schemaName := range path {
		published, err := b.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return foundStatistic{}, err
		}
		found, err := findStatisticsInSchema(ctx, b.catalog, published.Descriptor.ID(), parts[len(parts)-1], limits, b.options.Limits.MaxNodes)
		if err != nil {
			return foundStatistic{}, err
		}
		if found.table != nil {
			b.dependency(published.Descriptor, 0)
			b.dependency(found.table, 0)
			return found, nil
		}
	}
	return foundStatistic{}, bindError("42704", "statistics object does not exist", 0)
}

func bindAlterStatistics(ctx context.Context, statement *zpgparse.AlterStatsStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, error) {
	if statement == nil || statement.Stxstattarget == nil {
		return nil, bindError("42601", "ALTER STATISTICS requires a target", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterStatsStmt{AlterStatsStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.Defnames)
	if err != nil {
		return nil, err
	}
	found, err := resolveStatistics(ctx, b, parts, options.Descriptors)
	if err != nil {
		var diagnostic *model.Error
		if statement.MissingOk && errors.As(err, &diagnostic) && diagnostic.Code == "42704" {
			return &BoundStatisticsCommand{dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: "statistics object does not exist, skipping"}}, tag: "ALTER STATISTICS"}, nil
		}
		return nil, err
	}
	if found.statistic.Owner != options.Owner {
		return nil, bindError("42501", "must be owner of statistics object", 0)
	}
	integer := statement.Stxstattarget.GetInteger()
	if integer == nil || integer.Ival < -1 || integer.Ival > 10000 {
		return nil, bindError("22023", "statistics target must be between 0 and 10000, or -1 to reset", 0)
	}
	spec, err := found.table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	for i := range spec.Table.ExtendedStatistics {
		if spec.Table.ExtendedStatistics[i].OID == found.statistic.OID {
			spec.Table.ExtendedStatistics[i].TargetSet = integer.Ival >= 0
			spec.Table.ExtendedStatistics[i].Target = int32(max(int64(0), int64(integer.Ival)))
		}
	}
	spec.Version++
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundStatisticsCommand{updates: []StatisticsMutation{{Base: found.table, Next: next}}, dependencies: b.dependencies, tag: "ALTER STATISTICS"}, ctx.Err()
}


func bindRenameStatistics(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, error) {
	if statement == nil || statement.Newname == "" {
		return nil, bindError("42601", "invalid ALTER STATISTICS RENAME", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	parts, err := renameObjectParts(statement)
	if err != nil {
		return nil, err
	}
	found, err := resolveStatistics(ctx, b, parts, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if found.statistic.Owner != options.Owner {
		return nil, bindError("42501", "must be owner of statistics object", 0)
	}
	conflict, err := findStatisticsInSchema(ctx, snapshot, found.statistic.Namespace, statement.Newname, options.Descriptors, options.Bind.Limits.MaxNodes)
	if err != nil {
		return nil, err
	}
	if conflict.table != nil && conflict.statistic.OID != found.statistic.OID {
		return nil, bindError("42710", "statistics object already exists", 0)
	}
	next, err := mutateStatisticsDescriptor(ctx, found, options, func(statistic *model.ExtendedStatisticDescriptor, _ *model.DescriptorSpec) {
		statistic.Name = statement.Newname
	})
	if err != nil {
		return nil, err
	}
	return &BoundStatisticsCommand{updates: []StatisticsMutation{{Base: found.table, Next: next}}, dependencies: b.dependencies, tag: "ALTER STATISTICS"}, ctx.Err()
}

func bindSetStatisticsSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, error) {
	if statement == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid ALTER STATISTICS SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := alterObjectParts(statement.Relation, statement.Object)
	if err != nil {
		return nil, err
	}
	found, err := resolveStatistics(ctx, b, parts, options.Descriptors)
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && diagnostic.Code == "42704" {
				return &BoundStatisticsCommand{dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: "statistics object does not exist, skipping"}}, tag: "ALTER STATISTICS"}, nil
			}
		}
		return nil, err
	}
	if found.statistic.Owner != options.Owner {
		return nil, bindError("42501", "must be owner of statistics object", 0)
	}
	destination, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	if destination.Descriptor.State() != model.StatePublic {
		return nil, bindError("3F000", "schema does not exist", 0)
	}
	b.dependency(destination.Descriptor, 0)
	conflict, err := findStatisticsInSchema(ctx, snapshot, destination.Descriptor.ID(), found.statistic.Name, options.Descriptors, options.Bind.Limits.MaxNodes)
	if err != nil {
		return nil, err
	}
	if conflict.table != nil && conflict.statistic.OID != found.statistic.OID {
		return nil, bindError("42710", "statistics object already exists in destination schema", 0)
	}
	next, err := mutateStatisticsDescriptor(ctx, found, options, func(statistic *model.ExtendedStatisticDescriptor, spec *model.DescriptorSpec) {
		previous := statistic.Namespace
		statistic.Namespace = destination.Descriptor.ID()
		spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: destination.Descriptor.ID(), Version: destination.Descriptor.Version()})
		if previous != spec.Name.Parent && !slices.ContainsFunc(spec.Table.ExtendedStatistics, func(candidate model.ExtendedStatisticDescriptor) bool {
			return candidate.Namespace == previous
		}) {
			spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
				return dependency.Object == previous && dependency.Column == 0
			})
		}
		slices.SortFunc(spec.Dependencies, compareDependency)
	})
	if err != nil {
		return nil, err
	}
	return &BoundStatisticsCommand{updates: []StatisticsMutation{{Base: found.table, Next: next}}, dependencies: b.dependencies, tag: "ALTER STATISTICS"}, ctx.Err()
}

func mutateStatisticsDescriptor(ctx context.Context, found foundStatistic, options CreateDeclarationOptions, mutate func(*model.ExtendedStatisticDescriptor, *model.DescriptorSpec)) (*model.Descriptor, error) {
	spec, err := found.table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(spec.Table.ExtendedStatistics, func(statistic model.ExtendedStatisticDescriptor) bool { return statistic.OID == found.statistic.OID })
	if index < 0 {
		return nil, bindError("42704", "statistics object does not exist", 0)
	}
	mutate(&spec.Table.ExtendedStatistics[index], &spec)
	spec.Version++
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := found.table.ValidateSuccessor(ctx, next); err != nil {
		return nil, err
	}
	return next, nil
}
func bindDropStatistics(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatisticsCommand, error) {
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	if statement.Concurrent {
		return nil, bindError("0A000", "DROP STATISTICS CONCURRENTLY is not supported", 0)
	}
	byTable := make(map[model.ObjectID][]model.OID)
	bases := make(map[model.ObjectID]*model.Descriptor)
	command := &BoundStatisticsCommand{tag: "DROP STATISTICS"}
	for _, object := range statement.Objects {
		list := object.GetList()
		if list == nil {
			return nil, bindError("42601", "invalid DROP STATISTICS name", 0)
		}
		parts, err := names(list.Items)
		if err != nil {
			return nil, err
		}
		found, err := resolveStatistics(ctx, b, parts, options.Descriptors)
		if err != nil {
			var diagnostic *model.Error
			if statement.MissingOk && errors.As(err, &diagnostic) && diagnostic.Code == "42704" {
				command.warnings = append(command.warnings, model.Error{Code: "42704", Message: "statistics object does not exist, skipping"})
				continue
			}
			return nil, err
		}
		if found.statistic.Owner != options.Owner {
			return nil, bindError("42501", "must be owner of statistics object", 0)
		}
		if slices.Contains(byTable[found.table.ID()], found.statistic.OID) {
			return nil, bindError("42701", "statistics object specified more than once", 0)
		}
		byTable[found.table.ID()] = append(byTable[found.table.ID()], found.statistic.OID)
		bases[found.table.ID()] = found.table
	}
	ids := make([]model.ObjectID, 0, len(byTable))
	for id := range byTable {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		base := bases[id]
		spec, err := base.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		remove := byTable[id]
		spec.Table.ExtendedStatistics = slices.DeleteFunc(spec.Table.ExtendedStatistics, func(statistic model.ExtendedStatisticDescriptor) bool { return slices.Contains(remove, statistic.OID) })
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		command.updates = append(command.updates, StatisticsMutation{Base: base, Next: next})
	}
	command.dependencies = b.dependencies
	return command, ctx.Err()
}

type AnalyzeTarget struct {
	Table   *model.Descriptor
	Indexes []*model.Descriptor
	Columns []model.ColumnID
}

type BoundAnalyze struct {
	targets      []AnalyzeTarget
	dependencies []model.Dependency
	verbose      bool
	skipLocked   bool
}

func (a *BoundAnalyze) Targets() []AnalyzeTarget {
	targets := slices.Clone(a.targets)
	for i := range targets {
		targets[i].Indexes = slices.Clone(targets[i].Indexes)
		targets[i].Columns = slices.Clone(targets[i].Columns)
	}
	return targets
}
func (a *BoundAnalyze) Dependencies() []model.Dependency { return slices.Clone(a.dependencies) }
func (a *BoundAnalyze) Verbose() bool                    { return a.verbose }
func (a *BoundAnalyze) SkipLocked() bool                 { return a.skipLocked }

// AnalyzeSkipLocked reports the parsed SKIP_LOCKED option before catalog
// binding so the session can perform nonblocking descriptor admission first.
func AnalyzeSkipLocked(statement *zpgparse.VacuumStmt) (bool, error) {
	if statement == nil || statement.IsVacuumcmd {
		return false, bindError("42601", "expected ANALYZE", 0)
	}
	found := false
	value := false
	for _, node := range statement.Options {
		option := node.GetDefElem()
		if option == nil || option.Defname != "skip_locked" {
			continue
		}
		if found {
			return false, bindError("42601", "ANALYZE option specified more than once", int(option.Location)+1)
		}
		found = true
		var err error
		value, err = statisticsBooleanOption(option)
		if err != nil {
			return false, err
		}
	}
	return value, nil
}

func BindAnalyze(ctx context.Context, statement *zpgparse.VacuumStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundAnalyze, error) {
	if statement == nil || statement.IsVacuumcmd {
		return nil, bindError("42601", "expected ANALYZE", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_VacuumStmt{VacuumStmt: statement}})
	if err != nil {
		return nil, err
	}
	bound := &BoundAnalyze{}
	seenOptions := make(map[string]bool, len(statement.Options))
	for _, node := range statement.Options {
		option := node.GetDefElem()
		if option == nil || option.Defnamespace != "" {
			return nil, bindError("42601", "invalid ANALYZE option", 0)
		}
		if seenOptions[option.Defname] {
			return nil, bindError("42601", "ANALYZE option specified more than once", int(option.Location)+1)
		}
		seenOptions[option.Defname] = true
		switch option.Defname {
		case "verbose":
			bound.verbose, err = statisticsBooleanOption(option)
		case "skip_locked":
			bound.skipLocked, err = statisticsBooleanOption(option)
		case "buffer_usage_limit":
			if option.Arg == nil || option.Arg.GetString_() == nil || option.Arg.GetString_().GetSval() == "" {
				return nil, bindError("22023", "ANALYZE BUFFER_USAGE_LIMIT requires one value", int(option.Location)+1)
			}
			return nil, unsupported(PhysicalStorage, int(option.Location)+1)
		default:
			return nil, bindError("0A000", "unsupported ANALYZE option", int(option.Location)+1)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(statement.Rels) == 0 {
		err = snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
			if schema.Descriptor.State() != model.StatePublic {
				return nil
			}
			b.dependency(schema.Descriptor, 0)
			return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(table catalog.PublishedDescriptor) error {
				if len(bound.targets) >= options.Descriptors.MaxItems {
					return bindError("54000", "ANALYZE relation limit exceeded", 0)
				}
				if table.Descriptor.State() == model.StatePublic {
					metadata, ok := table.Descriptor.Table()
					if !ok {
						return bindError("42809", "ANALYZE target is not a table", 0)
					}
					if metadata.Owner != options.Owner {
						return bindError("42501", "must be owner of table to analyze it", 0)
					}
					b.dependency(table.Descriptor, 0)
					bound.targets = append(bound.targets, AnalyzeTarget{Table: table.Descriptor})
				}
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
	} else {
		seen := make(map[model.ObjectID]bool, len(statement.Rels))
		for _, node := range statement.Rels {
			relation := node.GetVacuumRelation()
			if relation == nil || relation.Relation == nil || relation.Oid != 0 {
				return nil, bindError("42601", "invalid ANALYZE relation", 0)
			}
			rangeVar := relation.Relation
			parts := []string{rangeVar.Relname}
			if rangeVar.Schemaname != "" {
				parts = []string{rangeVar.Schemaname, rangeVar.Relname}
			}
			table, schema, err := resolveNamespaceObject(ctx, snapshot, options.Bind.SearchPath, parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(rangeVar.Location)+1)
			if err != nil {
				return nil, err
			}
			if seen[table.ID()] {
				return nil, bindError("42701", "ANALYZE relation specified more than once", int(rangeVar.Location)+1)
			}
			seen[table.ID()] = true
			b.dependency(schema, 0)
			b.dependency(table, 0)
			spec, err := table.SpecWithin(ctx, options.Descriptors)
			if err != nil {
				return nil, err
			}
			if spec.Table.Owner != options.Owner {
				return nil, bindError("42501", "must be owner of table to analyze it", int(rangeVar.Location)+1)
			}
			columns := make([]model.ColumnID, 0, len(relation.VaCols))
			for _, columnNode := range relation.VaCols {
				name := columnNode.GetString_().GetSval()
				column, err := publicIndexColumn(spec.Table.Columns, name)
				if err != nil {
					return nil, err
				}
				if slices.Contains(columns, column.ID) {
					return nil, bindError("42701", "ANALYZE column specified more than once", 0)
				}
				columns = append(columns, column.ID)
			}
			slices.Sort(columns)
			bound.targets = append(bound.targets, AnalyzeTarget{Table: table, Columns: columns})
		}
	}
	indexCount := 0
	for i := range bound.targets {
		target := &bound.targets[i]
		err = snapshot.ScanNamespace(ctx, target.Table.Name().Parent, model.ObjectIndex, func(published catalog.PublishedDescriptor) error {
			index := published.Descriptor
			if index == nil || index.State() != model.StatePublic {
				return nil
			}
			metadata, ok := index.Index()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			if metadata.Table != target.Table.ID() {
				return nil
			}
			if indexCount >= options.Descriptors.MaxItems {
				return bindError("54000", "ANALYZE index limit exceeded", 0)
			}
			indexCount++
			b.dependency(index, 0)
			target.Indexes = append(target.Indexes, index)
			return nil
		})
		if err != nil {
			return nil, err
		}
		slices.SortFunc(target.Indexes, func(left, right *model.Descriptor) int {
			return cmp.Compare(left.ID(), right.ID())
		})
	}
	slices.SortFunc(bound.targets, func(a, b AnalyzeTarget) int { return cmp.Compare(a.Table.ID(), b.Table.ID()) })
	bound.dependencies = b.dependencies
	return bound, ctx.Err()
}

func statisticsBooleanOption(option *zpgparse.DefElem) (bool, error) {
	if option.Arg == nil {
		return true, nil
	}
	return copyBooleanOption(option)
}
