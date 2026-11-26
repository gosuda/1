package catalogrel

import (
	"context"
	"fmt"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func emitStatistics(ctx context.Context, snapshot Snapshot, view ReadView, relation Relation, options SourceOptions, emit rowEmitter) error {
	if relation.ID == IDPgStatisticExt {
		return emitExtendedStatisticDefinitions(ctx, snapshot, relation, options, emit)
	}
	if relation.ID == IDPgStatisticExtData {
		return emitExtendedStatisticData(ctx, snapshot, view, relation, options, emit)
	}
	if options.Statistics == nil {
		return &model.Error{Code: "55000", Message: "statistics catalog is not configured"}
	}
	reader, err := options.Statistics.OpenCatalogStatistics(ctx, view)
	if err != nil { return err }
	if reader == nil { return &model.Error{Code: "55000", Message: "statistics catalog returned no snapshot"} }
	if reader.Timestamp() != view.Timestamp() {
		return &model.Error{Code: "55000", Message: "statistics snapshot timestamp mismatch"}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		ref := storage.ObjectRef{ObjectID: object.spec.ID, Generation: object.spec.Physical.Generation, DescriptorVersion: object.spec.Version, EncodingVersion: object.spec.Physical.EncodingVersion}
		table, ok, err := reader.Lookup(ctx, ref)
		if err != nil { return err }
		if !ok { return nil }
		for _, column := range table.Columns {
			if columnName(descriptorColumns(object.spec), column.ID) == "" {
				return &model.Error{Code: "XX000", Message: "statistics column is unavailable"}
			}
			if relation.ID == IDPgStats {
				if err := emitStatsViewRow(relation, emit, object, column); err != nil { return err }
				continue
			}
			if err := emitStatisticRow(relation, emit, object, column); err != nil { return err }
		}
		return nil
	})
}
func emitExtendedStatisticDefinitions(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		for _, statistic := range object.spec.Table.ExtendedStatistics {
			namespace, err := snapshot.Descriptor(ctx, statistic.Namespace)
			if err != nil {
				return err
			}
			if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic || namespace.Descriptor.Kind() != model.ObjectSchema || namespace.Descriptor.OID() == 0 {
				return &model.Error{Code: "XX000", Message: "extended statistic namespace descriptor is unavailable"}
			}
			keys := make([]int64, len(statistic.Keys))
			for i, key := range statistic.Keys {
				keys[i] = int64(key)
			}
			kinds := make([]string, len(statistic.Kinds))
			for i, kind := range statistic.Kinds {
				kinds[i] = string([]byte{kind})
			}
			fields := []fieldValue{
				fv("oid", oidValue(statistic.OID)), fv("stxrelid", oidValue(object.spec.OID)), fv("stxname", stringValue(statistic.Name)),
				fv("stxnamespace", oidValue(namespace.Descriptor.OID())), fv("stxowner", oidValue(statistic.Owner)), fv("stxkeys", intArray(keys)), fv("stxkind", stringArray(kinds)),
			}
			if statistic.TargetSet {
				fields = append(fields, fv("stxstattarget", intValue(int64(statistic.Target))))
			}
			if len(statistic.Expressions) != 0 {
				expressions := make([]string, len(statistic.Expressions))
				for i, expression := range statistic.Expressions {
					expressions[i] = expression.SQL
				}
				fields = append(fields, fv("stxexprs", stringValue("("+strings.Join(expressions, ", ")+")")))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitExtendedStatisticData(ctx context.Context, snapshot Snapshot, view ReadView, relation Relation, options SourceOptions, emit rowEmitter) error {
	if options.Statistics == nil {
		return &model.Error{Code: "55000", Message: "statistics catalog is not configured"}
	}
	reader, err := options.Statistics.OpenCatalogStatistics(ctx, view)
	if err != nil {
		return err
	}
	if reader == nil || reader.Timestamp() != view.Timestamp() {
		return &model.Error{Code: "55000", Message: "statistics snapshot timestamp mismatch"}
	}
	return scanObjects(ctx, snapshot, options, []model.ObjectKind{model.ObjectTable}, func(object objectDescriptor) error {
		ref := storage.ObjectRef{ObjectID: object.spec.ID, Generation: object.spec.Physical.Generation, DescriptorVersion: object.spec.Version, EncodingVersion: object.spec.Physical.EncodingVersion}
		table, ok, err := reader.Lookup(ctx, ref)
		if err != nil || !ok {
			return err
		}
		definitions := make(map[model.OID]struct{}, len(object.spec.Table.ExtendedStatistics))
		for _, statistic := range object.spec.Table.ExtendedStatistics {
			definitions[statistic.OID] = struct{}{}
		}
		for _, statistic := range table.Extended {
			if _, ok := definitions[statistic.OID]; !ok {
				return &model.Error{Code: "55000", Message: "extended statistics data has no descriptor definition"}
			}
			fields := []fieldValue{fv("stxoid", oidValue(statistic.OID)), fv("stxdinherit", boolValue(statistic.Inherit))}
			if len(statistic.NDistinct) != 0 {
				fields = append(fields, fv("stxdndistinct", model.BytesVal(statistic.NDistinct)))
			}
			if len(statistic.Dependencies) != 0 {
				fields = append(fields, fv("stxddependencies", model.BytesVal(statistic.Dependencies)))
			}
			if len(statistic.MCV) != 0 {
				fields = append(fields, fv("stxdmcv", model.BytesVal(statistic.MCV)))
			}
			if statistic.Expressions.Type != model.TypeAny {
				fields = append(fields, fv("stxdexpr", statistic.Expressions))
			}
			if err := emitCatalogRow(relation, emit, fields...); err != nil {
				return err
			}
		}
		return nil
	})
}

func emitStatisticRow(relation Relation, emit rowEmitter, object objectDescriptor, column statistics.Column) error {
	fields := []fieldValue{
		fv("starelid", oidValue(object.spec.OID)), fv("staattnum", intValue(int64(column.ID))), fv("stainherit", boolValue(false)),
		fv("stanullfrac", floatValue(column.NullFraction)), fv("stawidth", intValue(int64(column.AverageBytes))), fv("stadistinct", floatValue(column.Distinct)),
	}
	kinds := [5]int16{}
	operators := [5]model.OperatorID{}
	collations := [5]model.CollationID{}
	numbers := [5][]float64{}
	values := [5][]model.Value{}
	next := 0
	if equality := comparisonOperator("=", column.Layout.Type.ID); len(column.MostCommon) != 0 && equality != 0 {
		kinds[next] = 1
		operators[next] = equality
		collations[next] = column.Layout.Type.Collation
		for _, frequency := range column.MostCommon {
			values[next] = append(values[next], frequency.Value)
			numbers[next] = append(numbers[next], frequency.Fraction)
		}
		next++
	}
	if less := comparisonOperator("<", column.Layout.Type.ID); len(column.Histogram) != 0 && less != 0 && next < len(kinds) {
		kinds[next] = 2
		operators[next] = less
		collations[next] = column.Layout.Type.Collation
		values[next] = column.Histogram
		next++
	}
	if less := comparisonOperator("<", column.Layout.Type.ID); column.CorrelationKnown && less != 0 && next < len(kinds) {
		kinds[next] = 3
		operators[next] = less
		collations[next] = column.Layout.Type.Collation
		numbers[next] = []float64{column.Correlation}
	}
	for i := range 5 {
		n := i + 1
		fields = append(fields,
			fv(fmt.Sprintf("stakind%d", n), intValue(int64(kinds[i]))), fv(fmt.Sprintf("staop%d", n), intValue(int64(operators[i]))), fv(fmt.Sprintf("stacoll%d", n), intValue(int64(collations[i]))))
		if len(numbers[i]) != 0 { fields = append(fields, fv(fmt.Sprintf("stanumbers%d", n), floatArray(numbers[i]))) }
		if len(values[i]) != 0 { fields = append(fields, fv(fmt.Sprintf("stavalues%d", n), statisticValues(values[i]))) }
	}
	return emitCatalogRow(relation, emit, fields...)
}

func emitStatsViewRow(relation Relation, emit rowEmitter, object objectDescriptor, column statistics.Column) error {
	name := columnName(descriptorColumns(object.spec), column.ID)
	if name == "" {
		return &model.Error{Code: "XX000", Message: "statistics column is unavailable"}
	}
	fields := []fieldValue{
		fv("schemaname", stringValue(object.schema.name)), fv("tablename", stringValue(object.spec.Name.Name)), fv("attname", stringValue(name)),
		fv("inherited", boolValue(false)), fv("null_frac", floatValue(column.NullFraction)), fv("avg_width", intValue(int64(column.AverageBytes))), fv("n_distinct", floatValue(column.Distinct)),
	}
	if len(column.MostCommon) != 0 && comparisonOperator("=", column.Layout.Type.ID) != 0 {
		values := make([]model.Value, len(column.MostCommon))
		frequencies := make([]float64, len(column.MostCommon))
		for i, frequency := range column.MostCommon {
			values[i] = frequency.Value
			frequencies[i] = frequency.Fraction
		}
		fields = append(fields, fv("most_common_vals", statisticValues(values)), fv("most_common_freqs", floatArray(frequencies)))
	}
	if len(column.Histogram) != 0 && comparisonOperator("<", column.Layout.Type.ID) != 0 {
		fields = append(fields, fv("histogram_bounds", statisticValues(column.Histogram)))
	}
	if column.CorrelationKnown && comparisonOperator("<", column.Layout.Type.ID) != 0 {
		fields = append(fields, fv("correlation", floatValue(column.Correlation)))
	}
	return emitCatalogRow(relation, emit, fields...)
}

func comparisonOperator(name string, typeID model.TypeID) model.OperatorID {
	for operator := range builtin.OperatorsNamed(name) {
		if operator.Left == typeID && operator.Right == typeID { return operator.ID }
	}
	return 0
}

func floatArray(values []float64) model.Value {
	items := make([]model.Value, len(values)); for i, value := range values { items[i] = floatValue(value) }
	return model.Value{Type: model.TypeArray, ArrayVal: items, Dimensions: []model.ArrayDimension{{Length: int32(len(items)), LowerBound: 1}}}
}

func statisticValues(values []model.Value) model.Value {
	items := make([]model.Value, len(values))
	for i, value := range values { items[i] = stringValue(statisticValueText(value)) }
	return model.Value{Type: model.TypeArray, ArrayVal: items, Dimensions: []model.ArrayDimension{{Length: int32(len(items)), LowerBound: 1}}}
}

func statisticValueText(value model.Value) string {
	if value.IsNull { return "" }
	switch value.Type {
	case model.TypeString: return value.StrVal
	case model.TypeInt64: return fmt.Sprintf("%d", value.IntVal)
	case model.TypeFloat64: return fmt.Sprintf("%g", value.FloatVal)
	case model.TypeBoolean:
		if value.IntVal != 0 { return "t" }; return "f"
	case model.TypeBytes: return fmt.Sprintf("\\x%x", value.BytesVal)
	default: return fmt.Sprint(value)
	}
}

func emitDependencies(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	kinds := []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectRoutine, model.ObjectCast, model.ObjectOperator, model.ObjectType, model.ObjectCompositeRelation, model.ObjectTextSearchDictionary, model.ObjectTextSearchConfiguration, model.ObjectTextSearchTemplate}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		for _, dependency := range object.spec.Dependencies {
			target, err := snapshot.Descriptor(ctx, dependency.Object)
			if err != nil {
				return err
			}
			if target.Descriptor == nil || target.Descriptor.State() != model.StatePublic || target.Descriptor.OID() == 0 || object.spec.OID == 0 {
				return &model.Error{Code: "XX000", Message: "catalog dependency target is unavailable"}
			}
			if target.Descriptor.Kind() == model.ObjectSchema && target.Descriptor.OID() == 11 {
				continue
			}
			if err := emitDependencyRow(relation, emit, kindCatalogOID(object.spec.Name.Kind), object.spec.OID, 0, kindCatalogOID(target.Descriptor.Kind()), target.Descriptor.OID(), int32(dependency.Column), descriptorDependencyType(object.spec, dependency)); err != nil {
				return err
			}
		}
		if object.spec.Table == nil {
			return nil
		}
		for _, statistic := range object.spec.Table.ExtendedStatistics {
			if statistic.OID == 0 {
				return &model.Error{Code: "XX000", Message: "extended statistic has no SQL catalog identity"}
			}
			namespace, err := snapshot.Descriptor(ctx, statistic.Namespace)
			if err != nil {
				return err
			}
			if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic || namespace.Descriptor.Kind() != model.ObjectSchema || namespace.Descriptor.OID() == 0 {
				return &model.Error{Code: "XX000", Message: "extended statistic namespace descriptor is unavailable"}
			}
			if err := emitDependencyRow(relation, emit, 3381, statistic.OID, 0, 2615, namespace.Descriptor.OID(), 0, 'n'); err != nil {
				return err
			}
			if len(statistic.Keys) == 0 {
				if err := emitDependencyRow(relation, emit, 3381, statistic.OID, 0, 1259, object.spec.OID, 0, 'a'); err != nil {
					return err
				}
			} else {
				for _, column := range statistic.Keys {
					if err := emitDependencyRow(relation, emit, 3381, statistic.OID, 0, 1259, object.spec.OID, int32(column), 'a'); err != nil {
						return err
					}
				}
			}
			for _, expression := range statistic.Expressions {
				for _, dependency := range expression.Dependencies {
					target, err := snapshot.Descriptor(ctx, dependency.Object)
					if err != nil {
						return err
					}
					if target.Descriptor == nil || target.Descriptor.State() != model.StatePublic || target.Descriptor.OID() == 0 {
						return &model.Error{Code: "XX000", Message: "extended statistic expression dependency is unavailable"}
					}
					dependencyType := byte('n')
					if dependency.Object == object.spec.ID {
						dependencyType = 'a'
					}
					if err := emitDependencyRow(relation, emit, 3381, statistic.OID, 0, kindCatalogOID(target.Descriptor.Kind()), target.Descriptor.OID(), int32(dependency.Column), dependencyType); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func emitDependencyRow(relation Relation, emit rowEmitter, class, object model.OID, subobject int32, referenceClass, referenceObject model.OID, referenceSubobject int32, dependencyType byte) error {
	return emitCatalogRow(relation, emit,
		fv("classid", oidValue(class)), fv("objid", oidValue(object)), fv("objsubid", intValue(int64(subobject))),
		fv("refclassid", oidValue(referenceClass)), fv("refobjid", oidValue(referenceObject)), fv("refobjsubid", intValue(int64(referenceSubobject))), fv("deptype", charValue(dependencyType)))
}

func descriptorDependencyType(spec model.DescriptorSpec, dependency model.Dependency) byte {
	switch {
	case spec.Index != nil && dependency.Object == spec.Index.Table:
		return 'a'
	case spec.Sequence != nil && spec.Sequence.OwnerTable != 0 && dependency.Object == spec.Sequence.OwnerTable:
		return 'a'
	case spec.Type != nil && spec.Type.DeclaredKind() == model.TypeKindComposite && dependency.Object == spec.Type.Relation:
		return 'i'
	case spec.Type != nil && spec.Type.DeclaredKind() == model.TypeKindArray && dependency.Object == spec.Type.Base:
		return 'i'
	default:
		return 'n'
	}
}

func emitDescriptions(ctx context.Context, snapshot Snapshot, relation Relation, options SourceOptions, emit rowEmitter) error {
	for _, description := range []struct {
		class, object model.OID
		text          string
	}{
		{3601, 3722, "default word parser"},
		{3764, 3727, "simple dictionary: just lower case and check for stopword"},
		{3764, 3730, "synonym dictionary: replace word by its synonym"},
		{3764, 3733, "ispell dictionary"},
		{3764, 3742, "thesaurus dictionary: phrase by phrase substitution"},
		{3600, 3765, "simple dictionary: just lower case and check for stopword"},
		{3602, 3748, "simple configuration"},
	} {
		if err := emitDescriptionRow(relation, emit, description.class, description.object, 0, description.text); err != nil {
			return err
		}
	}
	if err := scanSchemas(ctx, snapshot, options, func(schema schemaDescriptor) error {
		if schema.description == "" {
			return nil
		}
		return emitDescriptionRow(relation, emit, 2615, schema.oid, 0, schema.description)
	}); err != nil {
		return err
	}
	kinds := []model.ObjectKind{
		model.ObjectTable,
		model.ObjectIndex,
		model.ObjectSequence,
		model.ObjectView,
		model.ObjectMaterializedView,
		model.ObjectRoutine,
		model.ObjectCast,
		model.ObjectOperator,
		model.ObjectType,
		model.ObjectCompositeRelation,
		model.ObjectTextSearchDictionary,
		model.ObjectTextSearchConfiguration,
		model.ObjectTextSearchTemplate,
	}
	return scanObjects(ctx, snapshot, options, kinds, func(object objectDescriptor) error {
		if object.spec.Description != "" {
			if err := emitDescriptionRow(relation, emit, kindCatalogOID(object.spec.Name.Kind), object.spec.OID, 0, object.spec.Description); err != nil {
				return err
			}
		}
		for _, column := range descriptorColumns(object.spec) {
			if column.State != model.StatePublic || column.Description == "" {
				continue
			}
			if err := emitDescriptionRow(relation, emit, 1259, object.spec.OID, int32(column.ID), column.Description); err != nil {
				return err
			}
		}
		var constraints []model.ConstraintDescriptor
		if object.spec.Table != nil {
			constraints = object.spec.Table.Constraints
		} else if object.spec.Type != nil {
			constraints = object.spec.Type.Constraints
		}
		for _, constraint := range constraints {
			if constraint.Mutation || constraint.Description == "" {
				continue
			}
			if constraint.OID == 0 {
				return &model.Error{Code: "55000", Message: "described constraint has no SQL catalog identity"}
			}
			if err := emitDescriptionRow(relation, emit, 2606, constraint.OID, 0, constraint.Description); err != nil {
				return err
			}
		}
		if object.spec.Table != nil {
			for _, statistic := range object.spec.Table.ExtendedStatistics {
				if statistic.Description != "" {
					if err := emitDescriptionRow(relation, emit, 3381, statistic.OID, 0, statistic.Description); err != nil {
						return err
					}
				}
			}
			for _, trigger := range object.spec.Table.Triggers {
				if trigger.Description != "" {
					if err := emitDescriptionRow(relation, emit, 2620, trigger.OID, 0, trigger.Description); err != nil {
						return err
					}
				}
			}
		}
		if object.spec.View != nil {
			for _, trigger := range object.spec.View.Triggers {
				if trigger.Description != "" {
					if err := emitDescriptionRow(relation, emit, 2620, trigger.OID, 0, trigger.Description); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

func emitDescriptionRow(relation Relation, emit rowEmitter, class, object model.OID, subobject int32, description string) error {
	return emitCatalogRow(relation, emit,
		fv("objoid", oidValue(object)),
		fv("classoid", oidValue(class)),
		fv("objsubid", intValue(int64(subobject))),
		fv("description", stringValue(description)),
	)
}


func kindCatalogOID(kind model.ObjectKind) model.OID {
	switch kind {
	case model.ObjectCast:
		return 2605
	case model.ObjectOperator:
		return 2617
	case model.ObjectRoutine:
		return 1255
	case model.ObjectType:
		return 1247
	case model.ObjectSchema:
		return 2615
	case model.ObjectTextSearchTemplate:
		return 3764
	case model.ObjectTextSearchDictionary:
		return 3600
	case model.ObjectTextSearchConfiguration:
		return 3602
	default:
		return 1259
	}
}
