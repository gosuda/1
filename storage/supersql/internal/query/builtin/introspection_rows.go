package builtin

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type introspectionRowsPlan struct {
	id       model.FunctionID
	options  ResolveOptions
	metadata uint64
}

func introspectionRowFunction(id model.FunctionID) bool {
	switch id {
	case 1686, 2084, 2289, 2510, 2511, 2599, 2856, 3423, 3425, 3427, 6159, 6401:
		return true
	default:
		return false
	}
}

func ResolveIntrospectionRows(ctx context.Context, id model.FunctionID, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (RowSourcePlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, known := LookupSignature(id)
	if !known {
		return nil, &UnknownFunctionError{ID: id}
	}
	if !introspectionRowFunction(id) {
		return nil, &MissingImplementationError{Signature: signature}
	}
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "introspection row source argument count does not match signature")
	}
	if err := validateIntrospectionArguments(id, arguments); err != nil {
		return nil, err
	}
	if err := validateIntrospectionResult(id, result); err != nil {
		return nil, err
	}
	metadata := uint64(unsafe.Sizeof(introspectionRowsPlan{})) + uint64(len(options.TimeZone.Name))
	if metadata > maximum {
		return nil, sqlError("54000", "builtin type metadata budget exceeded")
	}
	options.TimeZone.Name = strings.Clone(options.TimeZone.Name)
	return &introspectionRowsPlan{id: id, options: options, metadata: metadata}, nil
}

func validateIntrospectionArguments(id model.FunctionID, arguments []model.TypeLayout) error {
	column := func(index int, typeID model.TypeID, storage model.DataType) bool {
		return index < len(arguments) && arguments[index].Type.ID == typeID && arguments[index].Type.Storage == storage
	}
	valid := len(arguments) == 0
	switch id {
	case 2289:
		valid = column(0, 1009, model.TypeArray) && len(arguments[0].Children) == 1 && arguments[0].Children[0].Type.ID == 25 && arguments[0].Children[0].Type.Storage == model.TypeString
	case 3423, 3425:
		valid = column(0, 2205, model.TypeInt64)
	case 3427:
		valid = column(0, 5017, model.TypeBytes)
	}
	if !valid {
		return sqlError("42804", "introspection row source requires its declared argument types")
	}
	return nil
}

func validateIntrospectionResult(id model.FunctionID, result model.TypeLayout) error {
	column := func(index int, typeID model.TypeID, storage model.DataType) bool {
		return index < len(result.Children) && result.Children[index].Type.ID == typeID && result.Children[index].Type.Storage == storage
	}
	arrayColumn := func(index int, typeID, elementID model.TypeID, elementStorage model.DataType) bool {
		return column(index, typeID, model.TypeArray) && len(result.Children[index].Children) == 1 && result.Children[index].Children[0].Type.ID == elementID && result.Children[index].Children[0].Type.Storage == elementStorage
	}
	valid := false
	switch id {
	case 1686:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 5 && column(0, 25, model.TypeString) && column(1, 18, model.TypeString) && column(2, 16, model.TypeBoolean) && column(3, 25, model.TypeString) && column(4, 25, model.TypeString)
	case 2084:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 17
		for i := 0; valid && i < 11; i++ {
			valid = column(i, 25, model.TypeString)
		}
		valid = valid && arrayColumn(11, 1009, 25, model.TypeString) && column(12, 25, model.TypeString) && column(13, 25, model.TypeString) && column(14, 25, model.TypeString) && column(15, 23, model.TypeInt64) && column(16, 16, model.TypeBoolean)
	case 2289:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 2 && column(0, 25, model.TypeString) && column(1, 25, model.TypeString)
	case 2510:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 8 && column(0, 25, model.TypeString) && column(1, 25, model.TypeString) && column(2, 1184, model.TypeTimestamp) && arrayColumn(3, 2211, 2206, model.TypeInt64) && arrayColumn(4, 2211, 2206, model.TypeInt64) && column(5, 16, model.TypeBoolean) && column(6, 20, model.TypeInt64) && column(7, 20, model.TypeInt64)
	case 2511:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 6 && column(0, 25, model.TypeString) && column(1, 25, model.TypeString) && column(2, 16, model.TypeBoolean) && column(3, 16, model.TypeBoolean) && column(4, 16, model.TypeBoolean) && column(5, 1184, model.TypeTimestamp)
	case 2599, 6401:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 3 && column(0, 25, model.TypeString) && column(1, 1186, model.TypeInterval) && column(2, 16, model.TypeBoolean)
	case 2856:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 4 && column(0, 25, model.TypeString) && column(1, 25, model.TypeString) && column(2, 1186, model.TypeInterval) && column(3, 16, model.TypeBoolean)
	case 3423:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 4 && column(0, 2205, model.TypeInt64) && column(1, 2205, model.TypeInt64) && column(2, 16, model.TypeBoolean) && column(3, 23, model.TypeInt64)
	case 3425:
		valid = result.Type.ID == 2205 && result.Type.Storage == model.TypeInt64 && len(result.Children) == 0
	case 3427:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 5 && column(0, 23, model.TypeInt64) && arrayColumn(1, 1009, 25, model.TypeString) && arrayColumn(2, 1000, 16, model.TypeBoolean) && column(3, 701, model.TypeFloat64) && column(4, 701, model.TypeFloat64)
	case 6159:
		valid = result.Type.Storage == model.TypeComposite && len(result.Children) == 6 && column(0, 2205, model.TypeInt64) && arrayColumn(1, 1009, 25, model.TypeString) && column(2, 2205, model.TypeInt64) && arrayColumn(3, 1009, 25, model.TypeString) && column(4, 16, model.TypeBoolean) && column(5, 16, model.TypeBoolean)
	}
	if !valid {
		return sqlError("42804", "introspection row source result record has the wrong layout")
	}
	return nil
}

func (plan *introspectionRowsPlan) OwnedBytes() uint64 { return plan.metadata }

type introspectionRowsIterator struct {
	rows     [][]model.Value
	position int
	claim    columnar.Allocation
	closed   bool
}

func (plan *introspectionRowsPlan) Open(ctx context.Context, arguments []model.Value, options RowSourceOptions) (_ RowSourceIterator, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	signature, _ := LookupSignature(plan.id)
	if len(arguments) != int(signature.ArgumentCount) {
		return nil, sqlError("42883", "introspection row source argument count does not match signature")
	}
	var inputBytes uint64
	for i, argument := range arguments {
		if argument.Type != primitiveStorage(signature.Arguments[i]) {
			return nil, sqlError("42804", "introspection row source argument storage does not match signature")
		}
		size, valueErr := model.ValueBytes(ctx, argument, options.MaxInputBytes-inputBytes)
		if valueErr != nil || size > options.MaxInputBytes-inputBytes {
			return nil, sqlError("54000", "introspection row source input exceeded byte budget")
		}
		inputBytes += size
		if signature.Strict && argument.IsNull {
			return &introspectionRowsIterator{}, nil
		}
	}
	rows, err := plan.prepare(ctx, arguments, options)
	if err != nil {
		return nil, err
	}
	iterator := &introspectionRowsIterator{rows: rows}
	retained := uint64(unsafe.Sizeof(*iterator)) + uint64(len(rows))*uint64(unsafe.Sizeof([]model.Value{}))
	if retained > options.MaxMemoryBytes {
		return nil, sqlError("54000", "introspection row source exceeded memory budget")
	}
	for _, row := range rows {
		rowBytes, valueErr := model.ValueBytes(ctx, model.Value{Type: model.TypeArray, ArrayVal: row}, options.MaxRowBytes)
		if valueErr != nil || rowBytes > options.MaxRowBytes {
			return nil, sqlError("54000", "introspection row exceeded byte budget")
		}
		if retained > options.MaxMemoryBytes || rowBytes > options.MaxMemoryBytes-retained {
			return nil, sqlError("54000", "introspection row source exceeded memory budget")
		}
		retained += rowBytes
	}
	if retained > options.MaxMemoryBytes || inputBytes > options.MaxMemoryBytes-retained {
		return nil, sqlError("54000", "introspection row source exceeded memory budget")
	}
	retained += inputBytes
	if options.Allocator != nil {
		iterator.claim, err = options.Allocator.Reserve(retained)
		if err != nil {
			return nil, err
		}
	}
	return iterator, nil
}

func (plan *introspectionRowsPlan) prepare(ctx context.Context, arguments []model.Value, options RowSourceOptions) ([][]model.Value, error) {
	switch plan.id {
	case 1686:
		return prepareKeywordRows(ctx)
	case 2084:
		return prepareSettingRows(ctx, options)
	case 2289:
		return prepareOptionRows(ctx, arguments[0])
	case 2510:
		return preparePreparedStatementRows(ctx, options)
	case 2511:
		return prepareCursorRows(ctx, options)
	case 2599:
		return prepareDefaultTimezoneAbbreviations(ctx, options)
	case 6401:
		return plan.prepareSessionTimezoneAbbreviations(ctx, options)
	case 2856:
		return prepareTimezoneNameRows(ctx, options)
	case 3423:
		return preparePartitionTreeRows(ctx, model.OID(arguments[0].IntVal), options)
	case 3425:
		return preparePartitionAncestorRows(ctx, model.OID(arguments[0].IntVal), options)
	case 3427:
		return plan.prepareMCVRows(ctx, arguments[0].BytesVal, options)
	case 6159:
		return prepareCatalogForeignKeyRows(ctx, options)
	default:
		return nil, &UnknownFunctionError{ID: plan.id}
	}
}

func prepareKeywordRows(ctx context.Context) ([][]model.Value, error) {
	keywords := zpgparse.Keywords()
	rows := make([][]model.Value, len(keywords))
	for i, keyword := range keywords {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		code, description := "", ""
		switch keyword.Kind {
		case zpgparse.KeywordKind_UNRESERVED_KEYWORD:
			code, description = "U", "unreserved"
		case zpgparse.KeywordKind_COL_NAME_KEYWORD:
			code, description = "C", "unreserved (cannot be function or type name)"
		case zpgparse.KeywordKind_TYPE_FUNC_NAME_KEYWORD:
			code, description = "T", "reserved (can be function or type name)"
		case zpgparse.KeywordKind_RESERVED_KEYWORD:
			code, description = "R", "reserved"
		}
		bareDescription := "requires AS"
		if keyword.BareLabel {
			bareDescription = "can be bare label"
		}
		rows[i] = []model.Value{model.StringVal(keyword.Word), model.StringVal(code), model.BoolVal(keyword.BareLabel), model.StringVal(description), model.StringVal(bareDescription)}
	}
	return rows, nil
}

func introspectionArray(values []model.Value) model.Value {
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		value.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	return value
}

func optionalText(value string) model.Value {
	if value == "" {
		return model.NullVal(model.TypeString)
	}
	return model.StringVal(strings.Clone(value))
}

func settingCategory(name string) string {
	switch strings.ToLower(name) {
	case "work_mem", "maintenance_work_mem", "temp_file_limit":
		return "Resource Usage / Memory"
	case "server_encoding", "server_version", "server_version_num", "integer_datetimes", "max_identifier_length":
		return "Preset Options"
	case "datestyle", "intervalstyle", "timezone", "extra_float_digits":
		return "Client Connection Defaults / Locale and Formatting"
	default:
		return "Client Connection Defaults / Statement Behavior"
	}
}

func prepareSettingRows(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	runtime, ok := options.Settings.(model.SettingsIntrospection)
	if !ok {
		return nil, sqlError("55000", "session settings introspection runtime is unavailable")
	}
	var rows [][]model.Value
	err := runtime.VisitSettings(ctx, func(setting model.SettingSummary) error {
		category := setting.Category
		if category == "" {
			category = settingCategory(setting.Name)
		}
		contextName := setting.Context
		if contextName == "" {
			if setting.ReadOnly {
				contextName = "internal"
			} else {
				contextName = "user"
			}
		}
		enumValues := model.NullVal(model.TypeArray)
		if setting.EnumValues != nil {
			values := make([]model.Value, len(setting.EnumValues))
			for i, value := range setting.EnumValues {
				values[i] = model.StringVal(strings.Clone(value))
			}
			enumValues = introspectionArray(values)
		}
		sourceLine := model.NullVal(model.TypeInt64)
		if setting.HasSourceLine {
			sourceLine = model.Int64Val(int64(setting.SourceLine))
		}
		rows = append(rows, []model.Value{
			model.StringVal(strings.ToLower(setting.Name)), model.StringVal(strings.Clone(setting.Setting)), optionalText(setting.Unit), model.StringVal(strings.Clone(category)),
			model.StringVal(strings.Clone(setting.ShortDescription)), optionalText(setting.ExtraDescription), model.StringVal(strings.Clone(contextName)), model.StringVal(strings.Clone(setting.ValueType)),
			model.StringVal(strings.Clone(setting.Source)), optionalText(setting.Minimum), optionalText(setting.Maximum), enumValues, model.StringVal(strings.Clone(setting.BootValue)),
			model.StringVal(strings.Clone(setting.ResetValue)), optionalText(setting.SourceFile), sourceLine, model.BoolVal(setting.PendingRestart),
		})
		return nil
	})
	return rows, err
}

func prepareOptionRows(ctx context.Context, input model.Value) ([][]model.Value, error) {
	rows := make([][]model.Value, 0, len(input.ArrayVal))
	for i, value := range input.ArrayVal {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if value.IsNull || value.Type != model.TypeString {
			return nil, sqlError("22023", "option array contains a non-text value")
		}
		name, option, found := strings.Cut(value.StrVal, "=")
		if !found || name == "" {
			return nil, sqlError("22023", "invalid option syntax")
		}
		rows = append(rows, []model.Value{model.StringVal(name), model.StringVal(option)})
	}
	return rows, nil
}

func typeIDArray(types []model.TypeID) model.Value {
	values := make([]model.Value, len(types))
	for i, typeID := range types {
		values[i] = model.Int64Val(int64(typeID))
	}
	return introspectionArray(values)
}

func sqlTimestampFromTime(value time.Time) (model.Value, error) {
	seconds := value.Unix()
	if seconds < math.MinInt64/intervalMicrosPerSecond+timezoneUnixEpoch || seconds > math.MaxInt64/intervalMicrosPerSecond+timezoneUnixEpoch {
		return model.Value{}, sqlError("22008", "session object timestamp is out of range")
	}
	micros := (seconds-timezoneUnixEpoch)*intervalMicrosPerSecond + int64(value.Nanosecond()/1000)
	if !validTimestamp(micros) {
		return model.Value{}, sqlError("22008", "session object timestamp is out of range")
	}
	return timestampValue(micros), nil
}

func preparePreparedStatementRows(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	if options.SessionIntrospection == nil {
		return nil, sqlError("55000", "session prepared-statement introspection runtime is unavailable")
	}
	var rows [][]model.Value
	err := options.SessionIntrospection.VisitPreparedStatements(ctx, func(statement model.PreparedStatementSummary) error {
		preparedAt, err := sqlTimestampFromTime(statement.PreparedAt)
		if err != nil {
			return err
		}
		if statement.GenericPlans > math.MaxInt64 || statement.CustomPlans > math.MaxInt64 {
			return sqlError("22003", "prepared statement plan count is out of range")
		}
		rows = append(rows, []model.Value{
			model.StringVal(strings.Clone(statement.Name)), model.StringVal(strings.Clone(statement.Statement)), preparedAt,
			typeIDArray(statement.ParameterTypes), typeIDArray(statement.ResultTypes), model.BoolVal(statement.FromSQL),
			model.Int64Val(int64(statement.GenericPlans)), model.Int64Val(int64(statement.CustomPlans)),
		})
		return nil
	})
	return rows, err
}

func prepareCursorRows(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	if options.SessionIntrospection == nil {
		return nil, sqlError("55000", "session cursor introspection runtime is unavailable")
	}
	var rows [][]model.Value
	err := options.SessionIntrospection.VisitCursors(ctx, func(cursor model.CursorSummary) error {
		createdAt, err := sqlTimestampFromTime(cursor.CreatedAt)
		if err != nil {
			return err
		}
		rows = append(rows, []model.Value{
			model.StringVal(strings.Clone(cursor.Name)), model.StringVal(strings.Clone(cursor.Statement)), model.BoolVal(cursor.Holdable),
			model.BoolVal(cursor.Binary), model.BoolVal(cursor.Scrollable), createdAt,
		})
		return nil
	})
	return rows, err
}

func introspectionInstant(ctx context.Context, options RowSourceOptions) (int64, error) {
	if options.Temporal == nil {
		return 0, sqlError("55000", "SQL temporal runtime is unavailable")
	}
	stamp, err := options.Temporal.TransactionTimestamp(ctx)
	if err != nil {
		return 0, err
	}
	return timezoneFloor(int64(stamp), intervalMicrosPerSecond) + timezoneUnixEpoch, nil
}

func timezoneRow(name string, typ timezoneType) []model.Value {
	return []model.Value{model.StringVal(name), {Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: int64(typ.offset) * intervalMicrosPerSecond}}, model.BoolVal(typ.dst)}
}

func timezoneAbbreviationType(ctx context.Context, zone namedTimeZone, name string, seconds int64) (timezoneType, bool, error) {
	if zone.record == nil {
		typ := zone.at(seconds)
		return typ, timezoneEqual(name, typ.name), nil
	}
	record := zone.record
	cutoff := record.cutoff(seconds)
	for i := cutoff; i > 0; i-- {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return timezoneType{}, false, err
			}
		}
		typ := record.typeAt(timezoneZoneBytes[record.indices+i-1])
		if timezoneEqual(name, typ.name) {
			return typ, true, nil
		}
	}
	typ := record.typeAt(record.defaultType)
	if timezoneEqual(name, typ.name) {
		return typ, true, nil
	}
	for i := cutoff; i < record.count; i++ {
		typ = record.typeAt(timezoneZoneBytes[record.indices+i])
		if timezoneEqual(name, typ.name) {
			return typ, true, nil
		}
	}
	if record.hasFuture {
		rule := record.future
		if timezoneEqual(name, rule.StandardName) {
			return timezoneType{offset: rule.StandardOffset, name: rule.StandardName}, true, nil
		}
		if rule.Daylight && timezoneEqual(name, rule.DaylightName) {
			return timezoneType{offset: rule.DaylightOffset, dst: true, name: rule.DaylightName}, true, nil
		}
	}
	return timezoneType{}, false, nil
}

func prepareDefaultTimezoneAbbreviations(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	seconds, err := introspectionInstant(ctx, options)
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, 0, len(timezoneDefaultAbbreviations))
	for i, abbreviation := range timezoneDefaultAbbreviations {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		typ := timezoneType{offset: abbreviation.offset, dst: abbreviation.dst, name: abbreviation.name}
		if abbreviation.zone >= 0 {
			var found bool
			typ, found, err = timezoneAbbreviationType(ctx, namedTimeZone{record: &timezoneZones[abbreviation.zone]}, abbreviation.name, seconds)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, sqlError("XX000", "generated timezone abbreviation target is inconsistent")
			}
		}
		rows = append(rows, timezoneRow(strings.ToUpper(abbreviation.name), typ))
	}
	return rows, nil
}

func alphabeticTimezoneAbbreviation(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func (plan *introspectionRowsPlan) prepareSessionTimezoneAbbreviations(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	seconds, err := introspectionInstant(ctx, options)
	if err != nil {
		return nil, err
	}
	zone, _, err := resolveTimeZoneContext(plan.options.TimeZone)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{})
	if zone.record != nil {
		for i := uint16(0); i < zone.record.typeCount; i++ {
			name := strings.ToUpper(zone.record.typeAt(uint8(i)).name)
			if alphabeticTimezoneAbbreviation(name) {
				names[name] = struct{}{}
			}
		}
		if zone.record.hasFuture {
			for _, name := range [...]string{zone.record.future.StandardName, zone.record.future.DaylightName} {
				name = strings.ToUpper(name)
				if alphabeticTimezoneAbbreviation(name) {
					names[name] = struct{}{}
				}
			}
		}
	} else {
		name := strings.ToUpper(zone.at(seconds).name)
		if alphabeticTimezoneAbbreviation(name) {
			names[name] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	rows := make([][]model.Value, 0, len(ordered))
	for _, name := range ordered {
		typ, found, err := timezoneAbbreviationType(ctx, zone, name, seconds)
		if err != nil {
			return nil, err
		}
		if found {
			rows = append(rows, timezoneRow(name, typ))
		}
	}
	return rows, nil
}

func prepareTimezoneNameRows(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	seconds, err := introspectionInstant(ctx, options)
	if err != nil {
		return nil, err
	}
	rows := make([][]model.Value, len(timezoneZones))
	for i := range timezoneZones {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		typ := (namedTimeZone{record: &timezoneZones[i]}).at(seconds)
		rows[i] = []model.Value{model.StringVal(timezoneZones[i].name), model.StringVal(typ.name), {Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: int64(typ.offset) * intervalMicrosPerSecond}}, model.BoolVal(typ.dst)}
	}
	return rows, nil
}

type partitionSummary struct {
	id          model.ObjectID
	oid         model.OID
	parent      model.ObjectID
	partitioned bool
}

func catalogIntrospection(options RowSourceOptions) (model.CatalogIntrospection, error) {
	runtime, ok := options.Relations.(model.CatalogIntrospection)
	if !ok {
		return nil, sqlError("55000", "logical catalog introspection runtime is unavailable")
	}
	return runtime, nil
}

func partitionCatalog(ctx context.Context, options RowSourceOptions) (map[model.ObjectID]partitionSummary, error) {
	runtime, err := catalogIntrospection(options)
	if err != nil {
		return nil, err
	}
	objects := make(map[model.ObjectID]partitionSummary)
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable}, func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if ok {
			object := partitionSummary{id: summary.Descriptor.ID(), oid: summary.Descriptor.OID(), partitioned: table.Partition != nil}
			if len(table.Parents) != 0 {
				object.parent = table.Parents[0]
			}
			objects[object.id] = object
		}
		return nil
	})
	return objects, err
}

func partitionByOID(objects map[model.ObjectID]partitionSummary, oid model.OID) (partitionSummary, bool) {
	for _, object := range objects {
		if object.oid == oid {
			return object, true
		}
	}
	return partitionSummary{}, false
}

func partitionParent(objects map[model.ObjectID]partitionSummary, object partitionSummary) (partitionSummary, bool) {
	if !object.partitioned || object.parent == 0 {
		return partitionSummary{}, false
	}
	parent, ok := objects[object.parent]
	return parent, ok
}

func partitionChildren(objects map[model.ObjectID]partitionSummary, parent model.ObjectID) []partitionSummary {
	var children []partitionSummary
	for _, object := range objects {
		if !object.partitioned || object.parent != parent {
			continue
		}
		children = append(children, object)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].oid < children[j].oid })
	return children
}

func partitionEligible(objects map[model.ObjectID]partitionSummary, object partitionSummary) bool {
	return object.partitioned || len(partitionChildren(objects, object.id)) != 0
}

func preparePartitionTreeRows(ctx context.Context, oid model.OID, options RowSourceOptions) ([][]model.Value, error) {
	objects, err := partitionCatalog(ctx, options)
	if err != nil {
		return nil, err
	}
	root, found := partitionByOID(objects, oid)
	if !found || !partitionEligible(objects, root) {
		return nil, nil
	}
	type member struct {
		object partitionSummary
		parent model.OID
		level  int64
	}
	queue := []member{{object: root}}
	var rows [][]model.Value
	for len(queue) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := queue[0]
		queue = queue[1:]
		children := partitionChildren(objects, current.object.id)
		parent := model.NullVal(model.TypeInt64)
		if current.parent != 0 {
			parent = model.Int64Val(int64(current.parent))
		}
		rows = append(rows, []model.Value{model.Int64Val(int64(current.object.oid)), parent, model.BoolVal(len(children) == 0), model.Int64Val(current.level)})
		for _, child := range children {
			queue = append(queue, member{object: child, parent: current.object.oid, level: current.level + 1})
		}
	}
	return rows, nil
}

func preparePartitionAncestorRows(ctx context.Context, oid model.OID, options RowSourceOptions) ([][]model.Value, error) {
	objects, err := partitionCatalog(ctx, options)
	if err != nil {
		return nil, err
	}
	object, found := partitionByOID(objects, oid)
	if !found || !partitionEligible(objects, object) {
		return nil, nil
	}
	var rows [][]model.Value
	seen := make(map[model.ObjectID]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, duplicate := seen[object.id]; duplicate {
			return nil, sqlError("XX000", "partition ancestry contains a cycle")
		}
		seen[object.id] = struct{}{}
		rows = append(rows, []model.Value{model.Int64Val(int64(object.oid))})
		parent, ok := partitionParent(objects, object)
		if !ok {
			break
		}
		object = parent
	}
	return rows, nil
}

func (plan *introspectionRowsPlan) prepareMCVRows(ctx context.Context, data []byte, options RowSourceOptions) ([][]model.Value, error) {
	limits := statistics.Limits{MaxBytes: options.MaxMemoryBytes, MaxColumns: 1024, MaxValues: int(min(options.MaxMemoryBytes/uint64(unsafe.Sizeof(model.Value{})), uint64(math.MaxInt))), MaxDepth: 64}
	list, err := statistics.DecodeMCVList(ctx, data, limits)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, statistics.ErrLimit) {
			return nil, sqlError("54000", "pg_mcv_list exceeded the invocation memory budget")
		}
		return nil, sqlError("XX000", "invalid pg_mcv_list value")
	}
	outputs := make([]Callable, len(list.Types))
	cstring := model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
	for i, layout := range list.Types {
		io, known := lookupResolvedTypeIO(plan.options, layout)
		if !known || io.Output == 0 {
			return nil, sqlError("42804", "MCV dimension type has no output function")
		}
		outputs[i], err = ResolveTyped(ctx, io.Output, []model.TypeLayout{layout}, cstring, plan.options, options.MaxMemoryBytes)
		if err != nil {
			return nil, err
		}
	}
	rows := make([][]model.Value, len(list.Items))
	for i, item := range list.Items {
		values := make([]model.Value, len(item.Values))
		nulls := make([]model.Value, len(item.Nulls))
		for dimension, value := range item.Values {
			nulls[dimension] = model.BoolVal(item.Nulls[dimension])
			if item.Nulls[dimension] {
				values[dimension] = model.NullVal(model.TypeString)
				continue
			}
			result, callErr := outputs[dimension].Call(ctx, []model.Value{value}, CallBudget{OutputBytes: int64(min(options.MaxRowBytes, uint64(math.MaxInt64))), MemoryBytes: options.MaxMemoryBytes, Temporal: options.Temporal, Relations: options.Relations, Settings: options.Settings, TextSearch: options.TextSearch})
			if callErr != nil {
				return nil, callErr
			}
			values[dimension] = result
		}
		rows[i] = []model.Value{model.Int64Val(int64(i)), introspectionArray(values), introspectionArray(nulls), model.Float64Val(item.Frequency), model.Float64Val(item.BaseFrequency)}
	}
	return rows, nil
}

func prepareCatalogForeignKeyRows(ctx context.Context, options RowSourceOptions) ([][]model.Value, error) {
	if options.CatalogMetadata == nil {
		return nil, sqlError("55000", "logical catalog foreign-key metadata is unavailable")
	}
	var rows [][]model.Value
	err := options.CatalogMetadata.VisitCatalogForeignKeys(ctx, func(key model.CatalogForeignKeySummary) error {
		foreign := make([]model.Value, len(key.ForeignColumns))
		primary := make([]model.Value, len(key.PrimaryColumns))
		for i, name := range key.ForeignColumns {
			foreign[i] = model.StringVal(strings.Clone(name))
		}
		for i, name := range key.PrimaryColumns {
			primary[i] = model.StringVal(strings.Clone(name))
		}
		rows = append(rows, []model.Value{model.Int64Val(int64(key.ForeignTable)), introspectionArray(foreign), model.Int64Val(int64(key.PrimaryTable)), introspectionArray(primary), model.BoolVal(key.ForeignIsArray), model.BoolVal(key.ForeignOptional)})
		return nil
	})
	return rows, err
}

func (iterator *introspectionRowsIterator) Next(ctx context.Context) ([]model.Value, bool, error) {
	if iterator == nil || iterator.closed || iterator.position == len(iterator.rows) {
		if iterator != nil {
			_ = iterator.Close()
		}
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		_ = iterator.Close()
		return nil, false, err
	}
	row := iterator.rows[iterator.position]
	iterator.position++
	return row, true, nil
}

func (iterator *introspectionRowsIterator) Close() error {
	if iterator == nil || iterator.closed {
		return nil
	}
	iterator.closed = true
	iterator.rows = nil
	if iterator.claim != nil {
		iterator.claim.Release()
		iterator.claim = nil
	}
	return nil
}
