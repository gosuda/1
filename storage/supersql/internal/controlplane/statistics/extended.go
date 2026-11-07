package statistics

import (
	"bytes"
	"cmp"
	"context"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	ndistinctWireMagic   uint64 = 0x4e44560000000001
	dependenciesWireMagic uint64 = 0x4644500000000001
)

// ValueExpression is a previously bound immutable expression. ANALYZE evaluates
// it with an empty ExpressionRuntime: statistics expressions may not have effects.
type ValueExpression interface {
	Evaluate(context.Context, []model.Value, model.ExpressionRuntime) (model.Value, error)
}

// ExtendedExpression supplies the executable form and resolved result layout of
// one descriptor expression. The operator identities are the equality and
// ordering operators used by the result type's default btree operator class.
type ExtendedExpression struct {
	Expression       ValueExpression
	Layout           model.TypeLayout
	EqualityOperator model.OperatorID
	OrderingOperator model.OperatorID
}

// ExtendedDefinition is the executable projection of one catalog descriptor.
// Definitions and Keys are strictly ordered by OID and ColumnID respectively.
type ExtendedDefinition struct {
	OID         model.OID
	Inherit     bool
	Keys        []model.ColumnID
	Expressions []ExtendedExpression
	Kinds       []byte
	Target      int
	TargetSet   bool
}

// NDistinctSummary records the estimated number of distinct tuples for a set of
// zero-based descriptor dimensions. Dimensions are strictly increasing.
type NDistinctSummary struct {
	Dimensions []uint32
	Value      float64
}

// FunctionalDependency records determinant dimensions => Dependent. Degree is
// the sampled fraction consistent with the dependency, in [0,1].
type FunctionalDependency struct {
	Determinants []uint32
	Dependent    uint32
	Degree       float64
}

func validDimensions(dimensions []uint32, maximum int) bool {
	if len(dimensions) == 0 || len(dimensions) > maximum {
		return false
	}
	for i, dimension := range dimensions {
		if int(dimension) >= maximum || i != 0 && dimensions[i-1] >= dimension {
			return false
		}
	}
	return true
}

func validateNDistinct(summary NDistinctSummary, limits Limits) error {
	if !validDimensions(summary.Dimensions, limits.MaxColumns) || !finiteNonnegative(summary.Value) {
		return ErrInvalid
	}
	return nil
}

func writeNDistinct(writer *wireWriter, summary NDistinctSummary) {
	writer.number(ndistinctWireMagic)
	writer.number(uint64(len(summary.Dimensions)))
	for _, dimension := range summary.Dimensions {
		writer.number(uint64(dimension))
	}
	writer.number(math.Float64bits(summary.Value))
}

func EncodeNDistinct(ctx context.Context, summary NDistinctSummary, limits Limits) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := validateNDistinct(summary, limits); err != nil {
		return nil, err
	}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes}
	writeNDistinct(&writer, summary)
	if writer.err != nil {
		return nil, writer.err
	}
	writer.data = make([]byte, int(writer.used))
	writer.used = 0
	writeNDistinct(&writer, summary)
	return writer.data, writer.err
}

func DecodeNDistinct(ctx context.Context, data []byte, limits Limits) (NDistinctSummary, error) {
	if err := limits.Validate(); err != nil {
		return NDistinctSummary{}, err
	}
	if uint64(len(data)) > limits.MaxBytes {
		return NDistinctSummary{}, ErrLimit
	}
	reader := wireReader{data: data, budget: meter{ctx: ctx, limit: limits.MaxBytes, maxValues: limits.MaxValues, maxDepth: limits.MaxDepth}}
	if reader.number() != ndistinctWireMagic {
		return NDistinctSummary{}, ErrInvalid
	}
	count := reader.bounded(uint64(limits.MaxColumns))
	if count == 0 || reader.err != nil || reader.budget.add(count, 4) != nil {
		return NDistinctSummary{}, ErrInvalid
	}
	summary := NDistinctSummary{Dimensions: make([]uint32, int(count))}
	for i := range summary.Dimensions {
		summary.Dimensions[i] = uint32(reader.bounded(math.MaxUint32))
	}
	summary.Value = math.Float64frombits(reader.number())
	if reader.err != nil {
		return NDistinctSummary{}, reader.err
	}
	if reader.offset != len(data) || validateNDistinct(summary, limits) != nil {
		return NDistinctSummary{}, ErrInvalid
	}
	check := canonicalWriter{remaining: data}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes, destination: &check}
	writeNDistinct(&writer, summary)
	if writer.err != nil {
		return NDistinctSummary{}, writer.err
	}
	if len(check.remaining) != 0 {
		return NDistinctSummary{}, ErrInvalid
	}
	return summary, ctx.Err()
}

func validateDependencies(dependencies []FunctionalDependency, dimensions int, limits Limits) error {
	if dimensions < 2 || dimensions > limits.MaxColumns || len(dependencies) > limits.MaxValues {
		return ErrInvalid
	}
	for i, dependency := range dependencies {
		if !validDimensions(dependency.Determinants, dimensions) || int(dependency.Dependent) >= dimensions || slices.Contains(dependency.Determinants, dependency.Dependent) || !fraction(dependency.Degree) {
			return ErrInvalid
		}
		if i > 0 && compareFunctionalDependency(dependencies[i-1], dependency) >= 0 {
			return ErrInvalid
		}
	}
	return nil
}

func compareFunctionalDependency(left, right FunctionalDependency) int {
	if order := slices.Compare(left.Determinants, right.Determinants); order != 0 {
		return order
	}
	return cmp.Compare(left.Dependent, right.Dependent)
}

func writeDependencies(writer *wireWriter, dimensions int, dependencies []FunctionalDependency) {
	writer.number(dependenciesWireMagic)
	writer.number(uint64(dimensions))
	writer.number(uint64(len(dependencies)))
	for _, dependency := range dependencies {
		writer.number(uint64(len(dependency.Determinants)))
		for _, determinant := range dependency.Determinants {
			writer.number(uint64(determinant))
		}
		writer.number(uint64(dependency.Dependent))
		writer.number(math.Float64bits(dependency.Degree))
	}
}

func EncodeDependencies(ctx context.Context, dimensions int, dependencies []FunctionalDependency, limits Limits) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := validateDependencies(dependencies, dimensions, limits); err != nil {
		return nil, err
	}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes}
	writeDependencies(&writer, dimensions, dependencies)
	if writer.err != nil {
		return nil, writer.err
	}
	writer.data = make([]byte, int(writer.used))
	writer.used = 0
	writeDependencies(&writer, dimensions, dependencies)
	return writer.data, writer.err
}

func DecodeDependencies(ctx context.Context, data []byte, limits Limits) (int, []FunctionalDependency, error) {
	if err := limits.Validate(); err != nil {
		return 0, nil, err
	}
	if uint64(len(data)) > limits.MaxBytes {
		return 0, nil, ErrLimit
	}
	reader := wireReader{data: data, budget: meter{ctx: ctx, limit: limits.MaxBytes, maxValues: limits.MaxValues, maxDepth: limits.MaxDepth}}
	if reader.number() != dependenciesWireMagic {
		return 0, nil, ErrInvalid
	}
	dimensions := int(reader.bounded(uint64(limits.MaxColumns)))
	count := reader.count(uint64(unsafe.Sizeof(FunctionalDependency{})))
	dependencies := make([]FunctionalDependency, count)
	for i := range dependencies {
		determinants := reader.bounded(uint64(limits.MaxColumns))
		if determinants == 0 || reader.err != nil || reader.budget.add(determinants, 4) != nil {
			return 0, nil, ErrInvalid
		}
		dependencies[i].Determinants = make([]uint32, int(determinants))
		for j := range dependencies[i].Determinants {
			dependencies[i].Determinants[j] = uint32(reader.bounded(math.MaxUint32))
		}
		dependencies[i].Dependent = uint32(reader.bounded(math.MaxUint32))
		dependencies[i].Degree = math.Float64frombits(reader.number())
	}
	if reader.err != nil {
		return 0, nil, reader.err
	}
	if reader.offset != len(data) || validateDependencies(dependencies, dimensions, limits) != nil {
		return 0, nil, ErrInvalid
	}
	check := canonicalWriter{remaining: data}
	writer := wireWriter{ctx: ctx, limit: limits.MaxBytes, destination: &check}
	writeDependencies(&writer, dimensions, dependencies)
	if writer.err != nil {
		return 0, nil, writer.err
	}
	if len(check.remaining) != 0 {
		return 0, nil, ErrInvalid
	}
	return dimensions, dependencies, ctx.Err()
}

type tupleEntry struct {
	row int
	key []byte
}

type dependencyEntry struct {
	determinant []byte
	dependent   []byte
}

func validateExtendedDefinitions(o AnalyzeOptions) error {
	for i, definition := range o.Extended {
		if definition.OID == 0 || i > 0 && o.Extended[i-1].OID >= definition.OID || len(definition.Keys)+len(definition.Expressions) < 2 || len(definition.Kinds) == 0 || definition.Target < 0 || definition.Target > 10000 || !definition.TargetSet && definition.Target != 0 {
			return ErrInvalid
		}
		last := model.ColumnID(0)
		for _, key := range definition.Keys {
			if key <= last {
				return ErrInvalid
			}
			last = key
			if _, found := slices.BinarySearchFunc(o.Layout, key, func(layout storage.ColumnLayout, id model.ColumnID) int { return cmp.Compare(layout.ID, id) }); !found {
				return ErrInvalid
			}
		}
		var kinds [256]bool
		for _, kind := range definition.Kinds {
			if kinds[kind] || kind != 'd' && kind != 'f' && kind != 'm' && kind != 'e' {
				return ErrInvalid
			}
			kinds[kind] = true
		}
		if kinds['e'] != (len(definition.Expressions) != 0) || len(definition.Keys)+len(definition.Expressions) > o.Limits.MaxColumns {
			return ErrInvalid
		}
		for _, expression := range definition.Expressions {
			if expression.Expression == nil || expression.Layout.Validate() != nil {
				return ErrInvalid
			}
		}
	}
	return nil
}

func summarizeExtended(ctx context.Context, encoder storage.KeyEncoder, reservoir *reservoir, order []int, live uint64, o AnalyzeOptions, working, output *meter) ([]ExtendedStatistic, error) {
	if len(o.Extended) == 0 {
		return nil, nil
	}
	if err := output.add(uint64(len(o.Extended)), uint64(unsafe.Sizeof(ExtendedStatistic{}))); err != nil {
		return nil, err
	}
	result := make([]ExtendedStatistic, len(o.Extended))
	for i, definition := range o.Extended {
		statistic, err := summarizeExtendedDefinition(ctx, encoder, reservoir, order, live, definition, o, working, output)
		if err != nil {
			return nil, err
		}
		result[i] = statistic
	}
	return result, ctx.Err()
}

func summarizeExtendedDefinition(ctx context.Context, encoder storage.KeyEncoder, reservoir *reservoir, order []int, live uint64, definition ExtendedDefinition, o AnalyzeOptions, working, output *meter) (ExtendedStatistic, error) {
	dimensions := len(definition.Keys) + len(definition.Expressions)
	if uint64(max(1, reservoir.rows))*uint64(dimensions) > uint64(math.MaxInt) {
		return ExtendedStatistic{}, ErrLimit
	}
	fixed := uint64(max(1, reservoir.rows)*dimensions) * uint64(unsafe.Sizeof(model.Value{}))
	if err := working.add(fixed, 1); err != nil {
		return ExtendedStatistic{}, err
	}
	defer func() { working.used -= fixed }()
	values := make([]model.Value, reservoir.rows*dimensions)
	layouts := make([]model.TypeLayout, dimensions)
	operators := make([][2]model.OperatorID, len(definition.Expressions))
	for dimension, key := range definition.Keys {
		column, found := slices.BinarySearchFunc(o.Layout, key, func(layout storage.ColumnLayout, id model.ColumnID) int { return cmp.Compare(layout.ID, id) })
		if !found {
			return ExtendedStatistic{}, ErrInvalid
		}
		layouts[dimension] = model.TypeLayout{Type: o.Layout[column].Type, Children: o.Layout[column].Children}
		for row := range reservoir.rows {
			values[row*dimensions+dimension] = reservoir.values[row*reservoir.columns+column]
		}
	}
	for index, expression := range definition.Expressions {
		dimension := len(definition.Keys) + index
		layouts[dimension] = expression.Layout
		operators[index] = [2]model.OperatorID{expression.EqualityOperator, expression.OrderingOperator}
		for row := range reservoir.rows {
			if err := ctx.Err(); err != nil {
				return ExtendedStatistic{}, err
			}
			value, err := expression.Expression.Evaluate(ctx, reservoir.values[row*reservoir.columns:(row+1)*reservoir.columns], model.ExpressionRuntime{})
			if err != nil {
				return ExtendedStatistic{}, err
			}
			if err := storage.ValidateStoredValue(ctx, value, expression.Layout); err != nil {
				return ExtendedStatistic{}, err
			}
			owned, bytes, err := model.CopyValue(ctx, value, working.limit-working.used)
			if err != nil {
				return ExtendedStatistic{}, err
			}
			if err := working.add(bytes, 1); err != nil {
				return ExtendedStatistic{}, err
			}
			defer func(n uint64) { working.used -= n }(bytes)
			values[row*dimensions+dimension] = owned
		}
	}
	entries, keyBytes, err := extendedTupleEntries(ctx, encoder, values, layouts, reservoir.rows, o, working)
	if err != nil {
		return ExtendedStatistic{}, err
	}
	defer func() { working.used -= keyBytes }()
	groups, singletons := tupleGroups(ctx, entries)
	if err := ctx.Err(); err != nil {
		return ExtendedStatistic{}, err
	}
	distinct := estimateDistinct(len(groups), singletons, float64(reservoir.rows), float64(live))
	statistic := ExtendedStatistic{OID: definition.OID, Inherit: definition.Inherit}
	if slices.Contains(definition.Kinds, byte('d')) {
		dimensionIDs := make([]uint32, dimensions)
		for i := range dimensionIDs {
			dimensionIDs[i] = uint32(i)
		}
		statistic.NDistinct, err = EncodeNDistinct(ctx, NDistinctSummary{Dimensions: dimensionIDs, Value: distinct}, o.Limits)
		if err != nil {
			return ExtendedStatistic{}, err
		}
		if err := output.add(uint64(cap(statistic.NDistinct)), 1); err != nil {
			return ExtendedStatistic{}, err
		}
	}
	if slices.Contains(definition.Kinds, byte('f')) {
		dependencies, err := buildFunctionalDependencies(ctx, encoder, values, layouts, reservoir.rows, o, working)
		if err != nil {
			return ExtendedStatistic{}, err
		}
		statistic.Dependencies, err = EncodeDependencies(ctx, dimensions, dependencies, o.Limits)
		if err != nil {
			return ExtendedStatistic{}, err
		}
		if err := output.add(uint64(cap(statistic.Dependencies)), 1); err != nil {
			return ExtendedStatistic{}, err
		}
	}
	target := min(o.MostCommon, o.Limits.MaxValues)
	if definition.TargetSet {
		target = min(definition.Target, o.Limits.MaxValues)
	}
	if slices.Contains(definition.Kinds, byte('m')) {
		list, err := buildMCVList(ctx, encoder, values, layouts, entries, groups, reservoir.rows, target, o.MaxKeyBytes)
		if err != nil {
			return ExtendedStatistic{}, err
		}
		statistic.MCV, err = EncodeMCVList(ctx, list, Limits{MaxBytes: o.Limits.MaxBytes - output.used, MaxColumns: o.Limits.MaxColumns, MaxValues: o.Limits.MaxValues, MaxDepth: o.Limits.MaxDepth})
		if err != nil {
			return ExtendedStatistic{}, err
		}
		if err := output.add(uint64(cap(statistic.MCV)), 1); err != nil {
			return ExtendedStatistic{}, err
		}
	}
	if len(definition.Expressions) != 0 {
		histogramBins := o.HistogramBins
		if definition.TargetSet {
			histogramBins = min(definition.Target, o.Limits.MaxValues)
		}
		expressions, err := expressionStatisticRecords(ctx, encoder, values, layouts, operators, len(definition.Keys), reservoir.rows, order, target, histogramBins, o, working, output)
		if err != nil {
			return ExtendedStatistic{}, err
		}
		statistic.Expressions = expressions
	}
	return statistic, ctx.Err()
}


func extendedTupleEntries(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, layouts []model.TypeLayout, rows int, o AnalyzeOptions, working *meter) ([]tupleEntry, uint64, error) {
	keyLayout := make([]storage.KeyColumn, len(layouts))
	for i, layout := range layouts {
		keyLayout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children}
	}
	entries := make([]tupleEntry, rows)
	var keyBytes uint64
	for row := range rows {
		key, err := encoder.EncodePrefix(ctx, keyLayout, values[row*len(layouts):(row+1)*len(layouts)], min(o.MaxKeyBytes, working.limit-working.used))
		if err != nil {
			return nil, keyBytes, err
		}
		if err := working.add(uint64(cap(key)), 1); err != nil {
			return nil, keyBytes, err
		}
		keyBytes += uint64(cap(key))
		entries[row] = tupleEntry{row: row, key: key}
	}
	slices.SortFunc(entries, func(left, right tupleEntry) int {
		if order := bytes.Compare(left.key, right.key); order != 0 {
			return order
		}
		return cmp.Compare(left.row, right.row)
	})
	return entries, keyBytes, ctx.Err()
}

func tupleGroups(ctx context.Context, entries []tupleEntry) ([]sampleGroup, int) {
	groups := make([]sampleGroup, 0, len(entries))
	singletons := 0
	for start := 0; start < len(entries); {
		if ctx.Err() != nil {
			return nil, 0
		}
		end := start + 1
		for end < len(entries) && bytes.Equal(entries[start].key, entries[end].key) {
			end++
		}
		groups = append(groups, sampleGroup{start: start, count: end - start})
		if end-start == 1 {
			singletons++
		}
		start = end
	}
	return groups, singletons
}

func estimateDistinct(groups, singletons int, sampleRows, liveRows float64) float64 {
	if sampleRows == 0 || liveRows == 0 {
		return 0
	}
	return min(liveRows, max(float64(groups), float64(groups)/(1-float64(singletons)/sampleRows+float64(singletons)/liveRows)))
}

func buildFunctionalDependencies(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, layouts []model.TypeLayout, rows int, o AnalyzeOptions, working *meter) ([]FunctionalDependency, error) {
	dependencies := make([]FunctionalDependency, len(layouts))
	for dependent := range layouts {
		determinants := make([]uint32, 0, len(layouts)-1)
		determinantLayout := make([]storage.KeyColumn, 0, len(layouts)-1)
		for dimension, layout := range layouts {
			if dimension == dependent {
				continue
			}
			determinants = append(determinants, uint32(dimension))
			determinantLayout = append(determinantLayout, storage.KeyColumn{ColumnID: model.ColumnID(dimension + 1), Type: layout.Type, Children: layout.Children})
		}
		dependentLayout := []storage.KeyColumn{{ColumnID: model.ColumnID(dependent + 1), Type: layouts[dependent].Type, Children: layouts[dependent].Children}}
		entries := make([]dependencyEntry, rows)
		var keyBytes uint64
		for row := range rows {
			tuple := values[row*len(layouts):(row+1)*len(layouts)]
			determinantValues := make([]model.Value, 0, len(determinants))
			for _, dimension := range determinants {
				determinantValues = append(determinantValues, tuple[dimension])
			}
			determinantKey, err := encoder.EncodePrefix(ctx, determinantLayout, determinantValues, min(o.MaxKeyBytes, working.limit-working.used))
			if err != nil {
				return nil, err
			}
			if err := working.add(uint64(cap(determinantKey)), 1); err != nil {
				return nil, err
			}
			keyBytes += uint64(cap(determinantKey))
			dependentKey, err := encoder.EncodePrefix(ctx, dependentLayout, tuple[dependent:dependent+1], min(o.MaxKeyBytes, working.limit-working.used))
			if err != nil {
				return nil, err
			}
			if err := working.add(uint64(cap(dependentKey)), 1); err != nil {
				return nil, err
			}
			keyBytes += uint64(cap(dependentKey))
			entries[row] = dependencyEntry{determinant: determinantKey, dependent: dependentKey}
		}
		slices.SortFunc(entries, func(left, right dependencyEntry) int {
			if order := bytes.Compare(left.determinant, right.determinant); order != 0 {
				return order
			}
			return bytes.Compare(left.dependent, right.dependent)
		})
		consistent := 0
		for start := 0; start < len(entries); {
			end := start + 1
			maximum, run := 1, 1
			for end < len(entries) && bytes.Equal(entries[start].determinant, entries[end].determinant) {
				if bytes.Equal(entries[end-1].dependent, entries[end].dependent) {
					run++
				} else {
					maximum = max(maximum, run)
					run = 1
				}
				end++
			}
			consistent += max(maximum, run)
			start = end
		}
		working.used -= keyBytes
		degree := 1.0
		if rows != 0 {
			degree = float64(consistent) / float64(rows)
		}
		dependencies[dependent] = FunctionalDependency{Determinants: determinants, Dependent: uint32(dependent), Degree: degree}
	}
	slices.SortFunc(dependencies, compareFunctionalDependency)
	return dependencies, ctx.Err()
}

func buildMCVList(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, layouts []model.TypeLayout, entries []tupleEntry, groups []sampleGroup, rows, target int, maxKeyBytes uint64) (*MCVList, error) {
	ordered := slices.Clone(groups)
	slices.SortFunc(ordered, func(left, right sampleGroup) int {
		if order := cmp.Compare(right.count, left.count); order != 0 {
			return order
		}
		return bytes.Compare(entries[left.start].key, entries[right.start].key)
	})
	ordered = ordered[:min(target, len(ordered))]
	list := &MCVList{Types: slices.Clone(layouts), Items: make([]MCVItem, len(ordered))}
	for index, group := range ordered {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row := entries[group.start].row
		item := &list.Items[index]
		item.Values = make([]model.Value, len(layouts))
		item.Nulls = make([]bool, len(layouts))
		item.Frequency = float64(group.count) / float64(max(1, rows))
		item.BaseFrequency = 1
		for dimension, layout := range layouts {
			value := values[row*len(layouts)+dimension]
			item.Nulls[dimension] = value.IsNull
			keyLayout := []storage.KeyColumn{{ColumnID: model.ColumnID(dimension + 1), Type: layout.Type, Children: layout.Children}}
			valueKey, err := encoder.EncodePrefix(ctx, keyLayout, []model.Value{value}, maxKeyBytes)
			if err != nil {
				return nil, err
			}
			marginal := 0
			for candidate := range rows {
				other := values[candidate*len(layouts)+dimension]
				otherKey, err := encoder.EncodePrefix(ctx, keyLayout, []model.Value{other}, maxKeyBytes)
				if err != nil {
					return nil, err
				}
				if bytes.Equal(valueKey, otherKey) {
					marginal++
				}
			}
			item.BaseFrequency *= float64(marginal) / float64(max(1, rows))
			owned, _, err := model.CopyValue(ctx, value, maxKeyBytes)
			if err != nil {
				return nil, err
			}
			item.Values[dimension] = owned
		}
	}
	return list, ctx.Err()
}


func expressionStatisticRecords(ctx context.Context, encoder storage.KeyEncoder, values []model.Value, layouts []model.TypeLayout, operators [][2]model.OperatorID, first, rows int, order []int, mostCommon, histogramBins int, o AnalyzeOptions, working, output *meter) (model.Value, error) {
	records := make([]model.Value, len(operators))
	expressionReservoir := reservoir{values: values, rows: rows, columns: len(layouts), ordinals: make([]uint64, rows)}
	for row := range rows {
		expressionReservoir.ordinals[row] = uint64(row + 1)
	}
	scratch := make([]sampleEntry, rows)
	groups := make([]sampleGroup, rows)
	for index, operator := range operators {
		column := Column{ID: model.ColumnID(index + 1), Layout: layouts[first+index]}
		if err := summarize(ctx, encoder, &expressionReservoir, first+index, order, scratch, groups, &column, uint64(rows), mostCommon, histogramBins, o, working, output); err != nil {
			return model.Value{}, err
		}
		records[index] = expressionStatisticRecord(column, index, operator)
	}
	return arrayValue(records), ctx.Err()
}

func expressionStatisticRecord(column Column, ordinal int, operators [2]model.OperatorID) model.Value {
	fields := make([]model.Value, 31)
	fields[0] = model.Int64Val(0)
	fields[1] = model.Int64Val(int64(-ordinal - 1))
	fields[2] = model.BoolVal(false)
	fields[3] = model.Float64Val(column.NullFraction)
	fields[4] = model.Int64Val(int64(math.Round(column.AverageBytes)))
	fields[5] = model.Float64Val(column.Distinct)
	for i := 6; i < len(fields); i++ {
		if i < 21 {
			fields[i] = model.Int64Val(0)
		} else {
			fields[i] = model.NullVal(model.TypeArray)
		}
	}
	next := 0
	if len(column.MostCommon) != 0 && operators[0] != 0 {
		fields[6+next] = model.Int64Val(1)
		fields[11+next] = model.Int64Val(int64(operators[0]))
		fields[16+next] = model.Int64Val(int64(column.Layout.Type.Collation))
		frequencies := make([]model.Value, len(column.MostCommon))
		common := make([]model.Value, len(column.MostCommon))
		for i, frequency := range column.MostCommon {
			frequencies[i] = model.Float64Val(frequency.Fraction)
			common[i] = frequency.Value
		}
		fields[21+next] = arrayValue(frequencies)
		fields[26+next] = arrayValue(common)
		next++
	}
	if len(column.Histogram) != 0 && operators[1] != 0 && next < 5 {
		fields[6+next] = model.Int64Val(2)
		fields[11+next] = model.Int64Val(int64(operators[1]))
		fields[16+next] = model.Int64Val(int64(column.Layout.Type.Collation))
		fields[26+next] = arrayValue(column.Histogram)
		next++
	}
	if column.CorrelationKnown && operators[1] != 0 && next < 5 {
		fields[6+next] = model.Int64Val(3)
		fields[11+next] = model.Int64Val(int64(operators[1]))
		fields[16+next] = model.Int64Val(int64(column.Layout.Type.Collation))
		fields[21+next] = arrayValue([]model.Value{model.Float64Val(column.Correlation)})
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: fields}
}

func arrayValue(values []model.Value) model.Value {
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		value.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	return value
}

func validateExpressionStatisticRecords(value model.Value, limits Limits) error {
	if value.Type != model.TypeArray || value.IsNull || len(value.ArrayVal) == 0 || len(value.ArrayVal) > limits.MaxColumns {
		return ErrInvalid
	}
	if len(value.Dimensions) != 1 || value.Dimensions[0].LowerBound != 1 || int(value.Dimensions[0].Length) != len(value.ArrayVal) {
		return ErrInvalid
	}
	for ordinal, record := range value.ArrayVal {
		if record.Type != model.TypeComposite || record.IsNull || len(record.ArrayVal) != 31 || len(record.Dimensions) != 0 {
			return ErrInvalid
		}
		fields := record.ArrayVal
		if fields[0].Type != model.TypeInt64 || fields[0].IntVal != 0 ||
			fields[1].Type != model.TypeInt64 || fields[1].IntVal != int64(-ordinal-1) ||
			fields[2].Type != model.TypeBoolean || fields[2].IsNull ||
			fields[3].Type != model.TypeFloat64 || !fraction(fields[3].FloatVal) ||
			fields[4].Type != model.TypeInt64 || fields[4].IntVal < 0 ||
			fields[5].Type != model.TypeFloat64 || !finiteNonnegative(fields[5].FloatVal) {
			return ErrInvalid
		}
		for i := 6; i < 21; i++ {
			if fields[i].Type != model.TypeInt64 || fields[i].IsNull {
				return ErrInvalid
			}
		}
		for i := 21; i < 31; i++ {
			if fields[i].Type != model.TypeArray || !fields[i].IsNull && len(fields[i].ArrayVal) != 0 && (len(fields[i].Dimensions) != 1 || fields[i].Dimensions[0].LowerBound != 1 || int(fields[i].Dimensions[0].Length) != len(fields[i].ArrayVal)) {
				return ErrInvalid
			}
		}
	}
	return nil
}

