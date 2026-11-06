package builtin

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	catalogClassRelation  model.OID = 1259
	catalogClassProcedure model.OID = 1255
	catalogClassType      model.OID = 1247
	catalogClassNamespace model.OID = 2615
	catalogClassCollation model.OID = 3456
)

func introspectionScalarFunction(id model.FunctionID) bool {
	switch id {
	case 44, 45, 637, 638, 1081, 1215, 1216, 1348, 1349, 1387, 1640, 1641, 1642, 1643, 1662, 1665, 1716, 1993,
		2079, 2080, 2081, 2082, 2083, 2093, 2098, 2162, 2165, 2212, 2213, 2214, 2215, 2216, 2217, 2220, 2221, 2232,
		2444, 2445, 2446, 2447, 2448, 2449, 2450, 2451, 2452, 2453, 2454, 2455, 2505, 2506, 2507, 2508, 2509,
		2730, 3159, 3352, 3382, 3403, 3408, 3415, 3424, 3476, 3479, 3492, 3493, 3494, 3808, 3815, 3829,
		3839, 3842, 3843, 3954, 4032, 4084, 4085, 4086, 4087, 4088, 4092, 4093, 4094, 4095, 4098,
		4193, 4194, 4195, 4196, 4197, 6173, 6174, 6197, 6315, 6317:
		return true
	default:
		return false
	}
}

func resolveIntrospectionScalar(signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions) (Callable, error) {
	if len(arguments) != int(signature.ArgumentCount) {
		return Callable{}, sqlError("42883", "introspection builtin argument count does not match signature")
	}
	kernel := compileIntrospectionScalar(signature, options)
	if kernel == nil {
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	if result.Type.ID != signature.Result || result.Type.Storage == model.TypeAny {
		return Callable{}, sqlError("42804", "introspection builtin result layout differs from signature")
	}
	callable := Callable{
		signature: signature, kernel: kernel, resultStorage: result.Type.Storage,
		metadataBytes: uint64(len(arguments)) * uint64(unsafe.Sizeof(model.Type{})),
	}
	callable.argumentTypes = make([]model.Type, len(arguments))
	for i, argument := range arguments {
		if argument.Type.ID != signature.Arguments[i] || argument.Type.Storage == model.TypeAny && signature.Arguments[i] != 2281 {
			return Callable{}, sqlError("42804", "introspection builtin argument layout differs from signature")
		}
		callable.argumentTypes[i] = argument.Type
	}
	return callable, nil
}

func resolveUntypedIntrospectionScalar(signature Signature, options ResolveOptions) (Callable, error) {
	kernel := compileIntrospectionScalar(signature, options)
	if kernel == nil {
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	callable := Callable{signature: signature, kernel: kernel, resultStorage: primitiveStorage(signature.Result)}
	for i := range signature.ArgumentCount {
		callable.argumentStorage[i] = primitiveStorage(signature.Arguments[i])
	}
	return callable, nil
}

func catalogCapability(budget CallBudget) (model.CatalogIntrospection, error) {
	runtime, ok := budget.Relations.(model.CatalogIntrospection)
	if !ok {
		return nil, sqlError("55000", "logical catalog introspection runtime is unavailable")
	}
	return runtime, nil
}
func roleCapability(budget CallBudget) (model.RoleIntrospection, error) {
	runtime, ok := budget.Relations.(model.RoleIntrospection)
	if !ok {
		return nil, sqlError("55000", "role catalog runtime is unavailable")
	}
	return runtime, nil
}

func roleOutputName(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	runtime, err := roleCapability(budget)
	if err != nil {
		return "", false, err
	}
	name, found, err := runtime.RoleName(ctx, oid)
	if err != nil || !found {
		return "", found, err
	}
	return catalogIdentifier(name), true, nil
}

func resolveRoleName(ctx context.Context, text string, budget CallBudget) (model.OID, error) {
	parts, err := ParseQualifiedName(ctx, text, budget.MemoryBytes)
	if err != nil {
		return 0, err
	}
	if len(parts) != 1 {
		return 0, sqlError("42704", "role does not exist")
	}
	runtime, err := roleCapability(budget)
	if err != nil {
		return 0, err
	}
	oid, found, err := runtime.ResolveRole(ctx, parts[0])
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, sqlError("42704", "role does not exist")
	}
	return oid, nil
}

func outputText(ctx context.Context, budget CallBudget, text string) (model.Value, error) {
	return ownedText(ctx, budget.OutputBytes, text)
}

func outputNullableText(ctx context.Context, budget CallBudget, text string, found bool) (model.Value, error) {
	if !found {
		return model.NullVal(model.TypeString), nil
	}
	return outputText(ctx, budget, text)
}

func parseCatalogOID(text string) (model.OID, bool) {
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 32)
	return model.OID(value), err == nil
}

func catalogIdentifier(name string) string {
	if name != "" {
		simple := true
		for index, r := range name {
			if r > unicode.MaxASCII || !(r == '_' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9') {
				simple = false
				break
			}
		}
		if simple {
			return name
		}
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func catalogQualifiedName(namespace, name string) string {
	if namespace == "" || namespace == "pg_catalog" {
		return catalogIdentifier(name)
	}
	return catalogIdentifier(namespace) + "." + catalogIdentifier(name)
}

func builtinTypeAlias(name string) (model.TypeID, bool) {
	switch strings.ToLower(strings.Join(strings.Fields(name), " ")) {
	case "boolean", "bool":
		return 16, true
	case "smallint", "int2":
		return 21, true
	case "integer", "int", "int4":
		return 23, true
	case "bigint", "int8":
		return 20, true
	case "real", "float4":
		return 700, true
	case "double precision", "float8":
		return 701, true
	case "decimal", "numeric":
		return 1700, true
	case "character", "char", "bpchar":
		return 1042, true
	case "character varying", "varchar":
		return 1043, true
	case "bit varying", "varbit":
		return 1562, true
	case "timestamp without time zone", "timestamp":
		return 1114, true
	case "timestamp with time zone", "timestamptz":
		return 1184, true
	case "time without time zone", "time":
		return 1083, true
	case "time with time zone", "timetz":
		return 1266, true
	}
	return 0, false
}

func builtinTypeDisplayName(info TypeInfo) string {
	switch info.ID {
	case 16:
		return "boolean"
	case 20:
		return "bigint"
	case 21:
		return "smallint"
	case 23:
		return "integer"
	case 700:
		return "real"
	case 701:
		return "double precision"
	case 1042:
		return "character"
	case 1043:
		return "character varying"
	case 1114:
		return "timestamp without time zone"
	case 1184:
		return "timestamp with time zone"
	case 1083:
		return "time without time zone"
	case 1266:
		return "time with time zone"
	}
	return info.Name
}

func staticTypeName(id model.TypeID) (string, bool) {
	info, found := LookupType(id)
	if !found {
		return "", false
	}
	if info.IsArray() {
		if element, ok := LookupType(info.Element); ok {
			return builtinTypeDisplayName(element) + "[]", true
		}
	}
	return builtinTypeDisplayName(info), true
}

func parseTypeReference(ctx context.Context, text string, budget CallBudget) (model.TypeID, int32, error) {
	trimmed := strings.TrimSpace(text)
	modifier := int32(-1)
	array := false
	if strings.HasSuffix(trimmed, "[]") {
		array = true
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, "[]"))
	}
	base := trimmed
	if open := strings.LastIndexByte(trimmed, '('); open >= 0 && strings.HasSuffix(trimmed, ")") {
		base = strings.TrimSpace(trimmed[:open])
		parameters := strings.Split(trimmed[open+1:len(trimmed)-1], ",")
		first, err := strconv.ParseInt(strings.TrimSpace(parameters[0]), 10, 31)
		if err != nil || first < 0 {
			return 0, 0, sqlError("22023", "invalid type modifier")
		}
		switch id, known := builtinTypeAlias(base); {
		case known && (id == 1042 || id == 1043):
			modifier = int32(first + 4)
		case known && id == 1700:
			scale := int64(0)
			if len(parameters) > 2 {
				return 0, 0, sqlError("22023", "invalid numeric type modifier")
			}
			if len(parameters) == 2 {
				scale, err = strconv.ParseInt(strings.TrimSpace(parameters[1]), 10, 16)
				if err != nil {
					return 0, 0, sqlError("22023", "invalid numeric type modifier")
				}
			}
			if first > 1000 || scale < -1000 || scale > 1000 {
				return 0, 0, sqlError("22023", "numeric type modifier is out of range")
			}
			modifier = int32((first << 16) | int64(uint16(int16(scale)))) + 4
		default:
			modifier = int32(first)
		}
	}
	var id model.TypeID
	if alias, found := builtinTypeAlias(base); found {
		id = alias
	} else {
		parts, err := ParseQualifiedName(ctx, base, budget.MemoryBytes)
		if err != nil {
			return 0, 0, err
		}
		if len(parts) == 1 || len(parts) == 2 && parts[0] == "pg_catalog" {
			for info := range TypesNamed(parts[len(parts)-1]) {
				id = info.ID
				break
			}
		}
		if id == 0 {
			runtime, err := catalogCapability(budget)
			if err != nil {
				return 0, 0, err
			}
			found, err := runtime.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectType}, parts, "", func(summary model.CatalogObjectSummary) error {
				id = model.TypeID(summary.Descriptor.OID())
				return nil
			})
			if err != nil {
				return 0, 0, err
			}
			if !found {
				return 0, 0, sqlError("42704", "type does not exist")
			}
		}
	}
	if array {
		if info, found := LookupType(id); found && info.Array != 0 {
			id = info.Array
		} else {
			return 0, 0, sqlError("42704", "array type does not exist")
		}
	}
	return id, modifier, nil
}

func formatTypeReference(ctx context.Context, id model.TypeID, modifier int32, budget CallBudget) (string, bool, error) {
	if name, found := staticTypeName(id); found {
		if modifier >= 0 {
			switch id {
			case 1042, 1043:
				if modifier >= 4 {
					name += "(" + strconv.FormatInt(int64(modifier-4), 10) + ")"
				}
			case 1700:
				encoded := modifier - 4
				if encoded >= 0 {
					precision := uint16(uint32(encoded) >> 16)
					scale := numericTypmodScale(modifier)
					name += "(" + strconv.FormatUint(uint64(precision), 10) + "," + strconv.FormatInt(int64(scale), 10) + ")"
				}
			}
		}
		return name, true, nil
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var name string
	found, err := runtime.VisitCatalogObject(ctx, model.OID(id), func(summary model.CatalogObjectSummary) error {
		if summary.Descriptor.Kind() != model.ObjectType {
			return nil
		}
		name = catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
		return nil
	})
	return name, found && name != "", err
}

func resolveRoutineName(ctx context.Context, text string, withArguments bool, budget CallBudget) (model.OID, error) {
	trimmed := strings.TrimSpace(text)
	nameText := trimmed
	var argumentIDs []model.TypeID
	var signatureText string
	if withArguments {
		open := strings.LastIndexByte(trimmed, '(')
		if open < 0 || !strings.HasSuffix(trimmed, ")") {
			return 0, sqlError("22P02", "invalid regprocedure syntax")
		}
		nameText = strings.TrimSpace(trimmed[:open])
		inside := strings.TrimSpace(trimmed[open+1 : len(trimmed)-1])
		if inside != "" {
			for _, token := range strings.Split(inside, ",") {
				id, modifier, err := parseTypeReference(ctx, token, budget)
				if err != nil {
					return 0, err
				}
				argumentIDs = append(argumentIDs, id)
				if signatureText != "" {
					signatureText += ","
				}
				signatureText += strconv.FormatUint(uint64(id), 10) + ":" + strconv.FormatInt(int64(modifier), 10)
			}
		}
	}
	parts, err := ParseQualifiedName(ctx, nameText, budget.MemoryBytes)
	if err != nil {
		return 0, err
	}
	builtinAllowed := len(parts) == 1 || len(parts) == 2 && parts[0] == "pg_catalog"
	if builtinAllowed {
		matches := 0
		var oid model.OID
		for candidate := range SignaturesNamed(parts[len(parts)-1]) {
			if withArguments {
				if int(candidate.ArgumentCount) != len(argumentIDs) {
					continue
				}
				match := true
				for i, id := range argumentIDs {
					if candidate.Arguments[i] != id {
						match = false
					}
				}
				if !match {
					continue
				}
			}
			matches++
			oid = model.OID(candidate.ID)
		}
		if matches == 1 {
			return oid, nil
		}
		if matches > 1 {
			return 0, sqlError("42725", "routine name is ambiguous")
		}
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return 0, err
	}
	var oid model.OID
	found, err := runtime.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectRoutine}, parts, signatureText, func(summary model.CatalogObjectSummary) error {
		oid = summary.Descriptor.OID()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, sqlError("42883", "routine does not exist")
	}
	return oid, nil
}

func routineOutputName(ctx context.Context, oid model.OID, withArguments bool, budget CallBudget) (string, bool, error) {
	if signature, found := LookupSignature(model.FunctionID(oid)); found {
		name := signature.Name
		if withArguments {
			var arguments strings.Builder
			arguments.WriteByte('(')
			for i := range signature.ArgumentCount {
				if i != 0 {
					arguments.WriteByte(',')
				}
				typ, _, err := formatTypeReference(ctx, signature.Arguments[i], -1, budget)
				if err != nil {
					return "", false, err
				}
				arguments.WriteString(typ)
			}
			arguments.WriteByte(')')
			name += arguments.String()
		}
		return name, true, nil
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		metadata, ok := summary.Descriptor.Routine()
		if !ok {
			return nil
		}
		output = catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
		if withArguments {
			output += "(" + formatRoutineTypes(ctx, metadata.Parameters, false, budget) + ")"
		}
		return nil
	})
	return output, found && output != "", err
}

func resolveOperatorName(ctx context.Context, text string, withArguments bool, budget CallBudget) (model.OID, error) {
	trimmed := strings.TrimSpace(text)
	name := trimmed
	var operands [2]model.TypeID
	if withArguments {
		open := strings.LastIndexByte(trimmed, '(')
		if open < 0 || !strings.HasSuffix(trimmed, ")") {
			return 0, sqlError("22P02", "invalid regoperator syntax")
		}
		name = strings.TrimSpace(trimmed[:open])
		parts := strings.Split(trimmed[open+1:len(trimmed)-1], ",")
		if len(parts) != 2 {
			return 0, sqlError("22P02", "regoperator requires two operand types")
		}
		for i, part := range parts {
			if strings.EqualFold(strings.TrimSpace(part), "none") {
				continue
			}
			id, _, err := parseTypeReference(ctx, part, budget)
			if err != nil {
				return 0, err
			}
			operands[i] = id
		}
	}
	name = strings.TrimPrefix(name, "pg_catalog.")
	matches := 0
	var oid model.OID
	for operator := range OperatorsNamed(name) {
		if withArguments && (operator.Left != operands[0] || operator.Right != operands[1]) {
			continue
		}
		matches++
		oid = model.OID(operator.ID)
	}
	if matches == 1 {
		return oid, nil
	}
	if matches > 1 {
		return 0, sqlError("42725", "operator name is ambiguous")
	}
	return 0, sqlError("42704", "operator does not exist")
}

func operatorOutputName(ctx context.Context, oid model.OID, withArguments bool, budget CallBudget) (string, bool, error) {
	operator, found := LookupOperator(model.OperatorID(oid))
	if !found {
		return "", false, nil
	}
	if !withArguments {
		return operator.Name, true, nil
	}
	operand := func(id model.TypeID) (string, error) {
		if id == 0 {
			return "NONE", nil
		}
		name, _, err := formatTypeReference(ctx, id, -1, budget)
		return name, err
	}
	left, err := operand(operator.Left)
	if err != nil {
		return "", false, err
	}
	right, err := operand(operator.Right)
	if err != nil {
		return "", false, err
	}
	return operator.Name + "(" + left + "," + right + ")", true, nil
}

func resolveNamespaceName(ctx context.Context, text string, budget CallBudget) (model.OID, error) {
	if oid, numeric := parseCatalogOID(text); numeric {
		return oid, nil
	}
	parts, err := ParseQualifiedName(ctx, text, budget.MemoryBytes)
	if err != nil {
		return 0, err
	}
	if len(parts) != 1 {
		return 0, sqlError("22P02", "invalid schema name")
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return 0, err
	}
	var oid model.OID
	found, err := runtime.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectSchema}, parts, "", func(summary model.CatalogObjectSummary) error {
		oid = summary.Descriptor.OID()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, sqlError("3F000", "schema does not exist")
	}
	return oid, nil
}

func namespaceOutputName(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var name string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		if summary.Descriptor.Kind() == model.ObjectSchema {
			name = catalogIdentifier(summary.Descriptor.Name().Name)
		}
		return nil
	})
	return name, found && name != "", err
}

func resolveCollationName(ctx context.Context, text string, budget CallBudget) (model.OID, error) {
	if oid, numeric := parseCatalogOID(text); numeric {
		return oid, nil
	}
	parts, err := ParseQualifiedName(ctx, text, budget.MemoryBytes)
	if err != nil {
		return 0, err
	}
	if len(parts) > 2 || len(parts) == 2 && parts[0] != "pg_catalog" {
		return 0, sqlError("42704", "collation does not exist")
	}
	if info, found := LookupCollationName(parts[len(parts)-1]); found {
		return model.OID(info.ID), nil
	}
	return 0, sqlError("42704", "collation does not exist")
}

func collationOutputName(oid model.OID) (string, bool) {
	info, found := LookupCollation(model.CollationID(oid))
	if !found {
		return "", false
	}
	return catalogIdentifier(info.Name), true
}

func regOutput(ctx context.Context, oid model.OID, kind string, budget CallBudget) (model.Value, error) {
	var text string
	var found bool
	var err error
	switch kind {
	case "proc":
		text, found, err = routineOutputName(ctx, oid, false, budget)
	case "procedure":
		text, found, err = routineOutputName(ctx, oid, true, budget)
	case "oper":
		text, found, err = operatorOutputName(ctx, oid, false, budget)
	case "operator":
		text, found, err = operatorOutputName(ctx, oid, true, budget)
	case "type":
		text, found, err = formatTypeReference(ctx, model.TypeID(oid), -1, budget)
	case "namespace":
		text, found, err = namespaceOutputName(ctx, oid, budget)
	case "collation":
		text, found = collationOutputName(oid)
	case "role":
		text, found, err = roleOutputName(ctx, oid, budget)
	}
	if err != nil {
		return model.Value{}, err
	}
	if !found {
		text = strconv.FormatUint(uint64(oid), 10)
	}
	return outputText(ctx, budget, text)
}

func regInput(ctx context.Context, text, kind string, missingOK bool, budget CallBudget) (model.Value, error) {
	if oid, numeric := parseCatalogOID(text); numeric {
		return model.Int64Val(int64(oid)), nil
	}
	var oid model.OID
	var err error
	switch kind {
	case "proc":
		oid, err = resolveRoutineName(ctx, text, false, budget)
	case "procedure":
		oid, err = resolveRoutineName(ctx, text, true, budget)
	case "oper":
		oid, err = resolveOperatorName(ctx, text, false, budget)
	case "operator":
		oid, err = resolveOperatorName(ctx, text, true, budget)
	case "type":
		var id model.TypeID
		id, _, err = parseTypeReference(ctx, text, budget)
		oid = model.OID(id)
	case "namespace":
		oid, err = resolveNamespaceName(ctx, text, budget)
	case "collation":
		oid, err = resolveCollationName(ctx, text, budget)
	case "role":
		oid, err = resolveRoleName(ctx, text, budget)
	}
	if err != nil && missingOK {
		var sql *model.Error
		if errors.As(err, &sql) && (sql.Code == "42704" || sql.Code == "42883" || sql.Code == "3F000") {
			return model.NullVal(model.TypeInt64), nil
		}
	}
	if err != nil {
		return model.Value{}, err
	}
	return model.Int64Val(int64(oid)), nil
}

func regReceive(value model.Value) (model.Value, error) {
	if len(value.BytesVal) != 4 {
		return model.Value{}, sqlError("22P03", "invalid reg* binary value")
	}
	return model.Int64Val(int64(binary.BigEndian.Uint32(value.BytesVal))), nil
}

func regSend(ctx context.Context, value model.Value, budget CallBudget) (model.Value, error) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], uint32(value.IntVal))
	return ownedBytes(ctx, budget.OutputBytes, data[:])
}

func descriptorDescription(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var description string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		description = strings.Clone(summary.Descriptor.Description())
		return nil
	})
	if err != nil || found && description != "" {
		return description, found && description != "", err
	}
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView, model.ObjectType}, func(summary model.CatalogObjectSummary) error {
		if table, ok := summary.Descriptor.Table(); ok {
			for _, constraint := range table.Constraints {
				if constraint.OID == oid {
					description = strings.Clone(constraint.Description)
				}
			}
			for _, trigger := range table.Triggers {
				if trigger.OID == oid {
					description = strings.Clone(trigger.Description)
				}
			}
			for _, statistic := range table.ExtendedStatistics {
				if statistic.OID == oid {
					description = strings.Clone(statistic.Description)
				}
			}
		}
		if typ, ok := summary.Descriptor.DeclaredType(); ok {
			for _, constraint := range typ.Constraints {
				if constraint.OID == oid {
					description = strings.Clone(constraint.Description)
				}
			}
		}
		return nil
	})
	return description, description != "", err
}

func columnDescription(ctx context.Context, oid model.OID, attribute int64, budget CallBudget) (string, bool, error) {
	if attribute <= 0 {
		return "", false, nil
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var description string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		var columns []model.ColumnDescriptor
		if table, ok := summary.Descriptor.Table(); ok {
			columns = table.Columns
		} else if view, ok := summary.Descriptor.View(); ok {
			columns = view.Columns
		}
		visible := int64(0)
		for _, column := range columns {
			if column.State != model.StatePublic || column.SystemIdentity {
				continue
			}
			visible++
			if visible == attribute {
				description = strings.Clone(column.Description)
				break
			}
		}
		return nil
	})
	return description, found && description != "", err
}

func catalogObjectVisible(ctx context.Context, oid model.OID, expected []model.ObjectKind, budget CallBudget) (bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return false, err
	}
	var kind model.ObjectKind
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		kind = summary.Descriptor.Kind()
		return nil
	})
	if err != nil || !found {
		return false, err
	}
	allowed := false
	for _, candidate := range expected {
		allowed = allowed || candidate == kind
	}
	if !allowed {
		return false, nil
	}
	return runtime.CatalogObjectVisible(ctx, kind, oid)
}

func columnName(columns []model.ColumnDescriptor, id model.ColumnID) string {
	for _, column := range columns {
		if column.ID == id {
			return catalogIdentifier(column.Name)
		}
	}
	return ""
}

func keyElementSQL(element model.KeyElement, columns []model.ColumnDescriptor) string {
	text := columnName(columns, element.Column)
	if element.Expression != nil {
		text = element.Expression.SQL
	}
	if element.Descending {
		text += " DESC"
	}
	if element.NullsFirst {
		text += " NULLS FIRST"
	} else if element.Descending {
		text += " NULLS LAST"
	}
	return text
}

func formatKeyElements(elements []model.KeyElement, columns []model.ColumnDescriptor) string {
	parts := make([]string, len(elements))
	for i, element := range elements {
		parts[i] = keyElementSQL(element, columns)
	}
	return strings.Join(parts, ", ")
}

func partitionRoot(ctx context.Context, oid model.OID, budget CallBudget) (model.OID, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return 0, false, err
	}
	current := oid
	seen := make(map[model.OID]struct{})
	for {
		if _, duplicate := seen[current]; duplicate {
			return 0, false, sqlError("XX000", "partition ancestry contains a cycle")
		}
		seen[current] = struct{}{}
		var parentID model.ObjectID
		found, err := runtime.VisitCatalogObject(ctx, current, func(summary model.CatalogObjectSummary) error {
			table, ok := summary.Descriptor.Table()
			if ok && table.Partition != nil && len(table.Parents) != 0 {
				parentID = table.Parents[0]
			}
			return nil
		})
		if err != nil || !found {
			return 0, false, err
		}
		if parentID == 0 {
			return current, true, nil
		}
		var parentOID model.OID
		err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable}, func(summary model.CatalogObjectSummary) error {
			if summary.Descriptor.ID() == parentID {
				parentOID = summary.Descriptor.OID()
			}
			return nil
		})
		if err != nil || parentOID == 0 {
			return 0, false, err
		}
		current = parentOID
	}
}

func findTableNested(ctx context.Context, oid model.OID, budget CallBudget, visit func(model.CatalogObjectSummary, model.TableMetadata) (bool, string, error)) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found := false
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}, func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if !ok {
			return nil
		}
		matched, text, visitErr := visit(summary, table)
		if matched {
			found, output = true, strings.Clone(text)
		}
		return visitErr
	})
	return output, found, err
}

func constraintDefinition(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	return findTableNested(ctx, oid, budget, func(_ model.CatalogObjectSummary, table model.TableMetadata) (bool, string, error) {
		for _, constraint := range table.Constraints {
			if constraint.OID != oid {
				continue
			}
			columns := make([]string, len(constraint.Columns))
			for i, id := range constraint.Columns {
				columns[i] = columnName(table.Columns, id)
			}
			switch constraint.Kind {
			case model.ConstraintCheck:
				if constraint.Expression != nil {
					return true, "CHECK (" + constraint.Expression.SQL + ")", nil
				}
			case model.ConstraintUnique:
				return true, "UNIQUE (" + strings.Join(columns, ", ") + ")", nil
			case model.ConstraintPrimaryKey:
				return true, "PRIMARY KEY (" + strings.Join(columns, ", ") + ")", nil
			case model.ConstraintNotNull:
				return true, "NOT NULL " + strings.Join(columns, ", "), nil
			case model.ConstraintForeignKey:
				return true, "FOREIGN KEY (" + strings.Join(columns, ", ") + ")", nil
			}
			return true, "", nil
		}
		return false, "", nil
	})
}

type catalogViewDefinitionRuntime interface {
	CatalogViewDefinitionByOID(context.Context, model.OID) (string, bool, error)
	CatalogViewDefinitionByName(context.Context, []string) (string, bool, error)
}

func viewDefinitionByOID(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	if logical, ok := budget.Relations.(catalogViewDefinitionRuntime); ok {
		sql, found, err := logical.CatalogViewDefinitionByOID(ctx, oid)
		if err != nil || found {
			return sql, found, err
		}
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var sql string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		view, ok := summary.Descriptor.View()
		if ok {
			sql = strings.Clone(view.Definition)
		}
		return nil
	})
	return sql, found && sql != "", err
}

func viewDefinitionByName(ctx context.Context, name string, budget CallBudget) (string, bool, error) {
	parts, err := ParseQualifiedName(ctx, name, budget.MemoryBytes)
	if err != nil {
		return "", false, err
	}
	if logical, ok := budget.Relations.(catalogViewDefinitionRuntime); ok {
		sql, found, err := logical.CatalogViewDefinitionByName(ctx, parts)
		if err != nil || found {
			return sql, found, err
		}
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var sql string
	found, err := runtime.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectView, model.ObjectMaterializedView}, parts, "", func(summary model.CatalogObjectSummary) error {
		view, ok := summary.Descriptor.View()
		if ok {
			sql = strings.Clone(view.Definition)
		}
		return nil
	})
	return sql, found && sql != "", err
}

func indexDefinition(ctx context.Context, oid model.OID, column int64, budget CallBudget) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		index, ok := summary.Descriptor.Index()
		if !ok {
			return nil
		}
		return runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable}, func(tableSummary model.CatalogObjectSummary) error {
			if tableSummary.Descriptor.ID() != index.Table {
				return nil
			}
			table, ok := tableSummary.Descriptor.Table()
			if !ok {
				return nil
			}
			if column > 0 {
				position := int(column - 1)
				if position < len(index.Keys) {
					output = keyElementSQL(index.Keys[position], table.Columns)
				} else if position-len(index.Keys) < len(index.Include) {
					output = columnName(table.Columns, index.Include[position-len(index.Keys)])
				}
				return nil
			}
			unique := ""
			if index.Unique {
				unique = "UNIQUE "
			}
			output = "CREATE " + unique + "INDEX " + catalogIdentifier(summary.Descriptor.Name().Name) + " ON " + catalogQualifiedName(tableSummary.NamespaceName, tableSummary.Descriptor.Name().Name) + " USING btree (" + formatKeyElements(index.Keys, table.Columns) + ")"
			if len(index.Include) != 0 {
				include := make([]string, len(index.Include))
				for i, id := range index.Include {
					include[i] = columnName(table.Columns, id)
				}
				output += " INCLUDE (" + strings.Join(include, ", ") + ")"
			}
			if index.Predicate != nil {
				output += " WHERE " + index.Predicate.SQL
			}
			return nil
		})
	})
	return output, found && output != "", err
}

func triggerDefinition(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	return findTableNested(ctx, oid, budget, func(summary model.CatalogObjectSummary, table model.TableMetadata) (bool, string, error) {
		for _, trigger := range table.Triggers {
			if trigger.OID != oid {
				continue
			}
			timing := strings.ToUpper(trigger.Timing)
			events := strings.Join(trigger.Events, " OR ")
			text := "CREATE TRIGGER " + catalogIdentifier(trigger.Name) + " " + timing + " " + events + " ON " + catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
			if trigger.ForEachRow {
				text += " FOR EACH ROW"
			} else {
				text += " FOR EACH STATEMENT"
			}
			if trigger.When != nil {
				text += " WHEN (" + trigger.When.SQL + ")"
			}
			routineName, err := triggerRoutineName(ctx, trigger, budget)
			if err != nil {
				return true, "", err
			}
			text += " EXECUTE FUNCTION " + routineName + "(" + strings.Join(trigger.Arguments, ", ") + ")"
			return true, text, nil
		}
		return false, "", nil
	})
}

func triggerRoutineName(ctx context.Context, trigger model.TriggerDescriptor, budget CallBudget) (string, error) {
	if signature, found := LookupSignature(trigger.Function); found {
		return catalogQualifiedName("pg_catalog", signature.Name), nil
	}
	if trigger.Routine == 0 {
		return "", sqlError("XX000", "trigger routine identity is missing")
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", err
	}
	var name string
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectRoutine}, func(summary model.CatalogObjectSummary) error {
		if summary.Descriptor.ID() == trigger.Routine {
			name = catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", sqlError("XX000", "trigger routine does not exist")
	}
	return name, nil
}

func formatRoutineTypes(ctx context.Context, parameters []model.RoutineParameter, names bool, budget CallBudget) string {
	parts := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		if parameter.Mode != "in" && parameter.Mode != "inout" && parameter.Mode != "variadic" {
			continue
		}
		typ, found, err := formatTypeReference(ctx, parameter.Layout.Type.ID, parameter.Layout.Type.Modifier, budget)
		if err != nil || !found {
			typ = strconv.FormatUint(uint64(parameter.Layout.Type.ID), 10)
		}
		if names && parameter.Name != "" {
			typ = catalogIdentifier(parameter.Name) + " " + typ
		}
		parts = append(parts, typ)
	}
	return strings.Join(parts, ", ")
}

func routineText(ctx context.Context, oid model.OID, mode string, argument int64, budget CallBudget) (string, bool, error) {
	if signature, found := LookupSignature(model.FunctionID(oid)); found {
		switch mode {
		case "arguments", "identity":
			parts := make([]string, signature.ArgumentCount)
			for i := range signature.ArgumentCount {
				parts[i], _, _ = formatTypeReference(ctx, signature.Arguments[i], -1, budget)
			}
			return strings.Join(parts, ", "), true, nil
		case "result":
			text, found, err := formatTypeReference(ctx, signature.Result, -1, budget)
			return text, found, err
		}
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		metadata, ok := summary.Descriptor.Routine()
		if !ok {
			return nil
		}
		switch mode {
		case "arguments":
			parts := make([]string, 0, len(metadata.Parameters))
			for _, parameter := range metadata.Parameters {
				typ, _, typeErr := formatTypeReference(ctx, parameter.Layout.Type.ID, parameter.Layout.Type.Modifier, budget)
				if typeErr != nil {
					return typeErr
				}
				piece := ""
				if parameter.Mode != "" && parameter.Mode != "in" {
					piece += strings.ToUpper(parameter.Mode) + " "
				}
				if parameter.Name != "" {
					piece += catalogIdentifier(parameter.Name) + " "
				}
				piece += typ
				if parameter.Default != nil {
					piece += " DEFAULT " + parameter.Default.SQL
				}
				parts = append(parts, piece)
			}
			output = strings.Join(parts, ", ")
		case "identity":
			output = formatRoutineTypes(ctx, metadata.Parameters, true, budget)
		case "result":
			output, _, err = formatTypeReference(ctx, metadata.Result.Type.ID, metadata.Result.Type.Modifier, budget)
		case "default":
			inputs := make([]model.RoutineParameter, 0, len(metadata.Parameters))
			for _, parameter := range metadata.Parameters {
				if parameter.Mode == "in" || parameter.Mode == "inout" || parameter.Mode == "variadic" {
					inputs = append(inputs, parameter)
				}
			}
			if argument >= 0 && argument < int64(len(inputs)) && inputs[argument].Default != nil {
				output = strings.Clone(inputs[argument].Default.SQL)
			}
		case "body":
			output = strings.Clone(metadata.Body.SQL)
			if output == "" {
				output = strings.Clone(metadata.Source)
			}
		case "definition":
			kind := "FUNCTION"
			if metadata.Procedure {
				kind = "PROCEDURE"
			}
			name := catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
			output = "CREATE OR REPLACE " + kind + " " + name + "(" + formatRoutineTypes(ctx, metadata.Parameters, true, budget) + ")"
			if !metadata.Procedure {
				result, _, typeErr := formatTypeReference(ctx, metadata.Result.Type.ID, metadata.Result.Type.Modifier, budget)
				if typeErr != nil {
					return typeErr
				}
				if metadata.ReturnsSet {
					result = "SETOF " + result
				}
				output += " RETURNS " + result
			}
			output += " LANGUAGE " + catalogIdentifier(metadata.Language)
			if metadata.Volatility != "" {
				output += " " + strings.ToUpper(metadata.Volatility)
			}
			if metadata.Strict {
				output += " STRICT"
			}
			if metadata.SecurityDefiner {
				output += " SECURITY DEFINER"
			}
			body := metadata.Body.SQL
			if body == "" {
				body = metadata.Source
			}
			output += " AS '" + strings.ReplaceAll(body, "'", "''") + "'"
		}
		return err
	})
	return output, found && output != "", err
}

func partKeyDefinition(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if !ok || len(table.PartitionKeys) == 0 {
			return nil
		}
		strategy := "RANGE"
		switch table.PartitionStrategy {
		case 'l':
			strategy = "LIST"
		case 'h':
			strategy = "HASH"
		}
		output = strategy + " (" + formatKeyElements(table.PartitionKeys, table.Columns) + ")"
		return nil
	})
	return output, found && output != "", err
}

func partitionConstraintDefinition(ctx context.Context, oid model.OID, budget CallBudget) (string, bool, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return "", false, err
	}
	var output string
	found, err := runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if ok && table.Partition != nil {
			output = strings.Clone(table.Partition.SQL)
		}
		return nil
	})
	return output, found && output != "", err
}

func statisticText(ctx context.Context, oid model.OID, mode string, budget CallBudget) (model.Value, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	var text string
	var expressions []model.Value
	found := false
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectTable}, func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if !ok {
			return nil
		}
		for _, statistic := range table.ExtendedStatistics {
			if statistic.OID != oid {
				continue
			}
			found = true
			switch mode {
			case "columns":
				columns := make([]string, len(statistic.Keys))
				for i, id := range statistic.Keys {
					columns[i] = columnName(table.Columns, id)
				}
				text = strings.Join(columns, ", ")
			case "expressions":
				expressions = make([]model.Value, len(statistic.Expressions))
				for i, expression := range statistic.Expressions {
					expressions[i] = model.StringVal(strings.Clone(expression.SQL))
				}
			case "definition":
				items := make([]string, 0, len(statistic.Keys)+len(statistic.Expressions))
				for _, id := range statistic.Keys {
					items = append(items, columnName(table.Columns, id))
				}
				for _, expression := range statistic.Expressions {
					items = append(items, "("+expression.SQL+")")
				}
				text = "CREATE STATISTICS " + catalogIdentifier(statistic.Name) + " ON " + strings.Join(items, ", ") + " FROM " + catalogQualifiedName(summary.NamespaceName, summary.Descriptor.Name().Name)
			}
		}
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if !found {
		if mode == "expressions" {
			return model.NullVal(model.TypeArray), nil
		}
		return model.NullVal(model.TypeString), nil
	}
	if mode == "expressions" {
		var bytes int64
		for _, value := range expressions {
			bytes += int64(len(value.StrVal))
		}
		if err := outputBudget(bytes, budget.OutputBytes); err != nil {
			return model.Value{}, err
		}
		return introspectionArray(expressions), nil
	}
	return outputText(ctx, budget, text)
}

func compileIntrospectionScalar(signature Signature, options ResolveOptions) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		id := signature.ID
		switch id {
		case 44, 2212, 2214, 2216, 2220, 4084, 4098, 4193:
			kind := map[model.FunctionID]string{44: "proc", 2212: "procedure", 2214: "oper", 2216: "operator", 2220: "type", 4084: "namespace", 4098: "role", 4193: "collation"}[id]
			return regInput(ctx, arguments[0].StrVal, kind, false, budget)
		case 3494, 3479, 3492, 3476, 3493, 4086, 4093, 4195:
			kind := map[model.FunctionID]string{3494: "proc", 3479: "procedure", 3492: "oper", 3476: "operator", 3493: "type", 4086: "namespace", 4093: "role", 4195: "collation"}[id]
			return regInput(ctx, arguments[0].StrVal, kind, true, budget)
		case 45, 2213, 2215, 2217, 2221, 4085, 4092, 4194:
			kind := map[model.FunctionID]string{45: "proc", 2213: "procedure", 2215: "oper", 2217: "operator", 2221: "type", 4085: "namespace", 4092: "role", 4194: "collation"}[id]
			return regOutput(ctx, model.OID(arguments[0].IntVal), kind, budget)
		case 2444, 2446, 2448, 2450, 2452, 2454, 4087, 4094, 4196:
			return regReceive(arguments[0])
		case 2445, 2447, 2449, 2451, 2453, 2455, 4088, 4095, 4197:
			return regSend(ctx, arguments[0], budget)
		case 1081:
			if arguments[0].IsNull {
				return model.NullVal(model.TypeString), nil
			}
			if arguments[0].IntVal == 0 {
				return outputText(ctx, budget, "-")
			}
			modifier := int32(arguments[1].IntVal)
			if arguments[1].IsNull {
				modifier = -1
			}
			name, found, err := formatTypeReference(ctx, model.TypeID(arguments[0].IntVal), modifier, budget)
			if err == nil && !found {
				name, found = "???", true
			}
			returnValue, outputErr := outputNullableText(ctx, budget, name, found)
			return returnValue, errors.Join(err, outputErr)
		case 6317:
			_, modifier, err := parseTypeReference(ctx, arguments[0].StrVal, budget)
			if err != nil {
				var sql *model.Error
				if errors.As(err, &sql) && sql.Code == "42704" {
					return model.NullVal(model.TypeInt64), nil
				}
				return model.Value{}, err
			}
			return model.Int64Val(int64(modifier)), nil
		case 6315:
			id := model.TypeID(arguments[0].IntVal)
			for depth := 0; depth < 64; depth++ {
				if options.Types != nil {
					if resolved, found := options.Types.ResolveType(id); found && resolved.Kind() == model.TypeKindDomain {
						id = resolved.BaseLayout().Type.ID
						continue
					}
				}
				if options.InformationSchemaTypes != nil {
					if binding, found := options.InformationSchemaTypes.ResolveInformationSchemaType(id); found && binding.Kind == model.TypeKindDomain {
						id = binding.BaseLayout.Type.ID
						continue
					}
				}
				break
			}
			return model.Int64Val(int64(id)), nil
		case 1349:
			parts := make([]string, len(arguments[0].ArrayVal))
			for i, value := range arguments[0].ArrayVal {
				name, found, err := formatTypeReference(ctx, model.TypeID(value.IntVal), -1, budget)
				if err != nil {
					return model.Value{}, err
				}
				if !found {
					name = "???"
				}
				parts[i] = name
			}
			return outputText(ctx, budget, strings.Join(parts, ", "))
		case 1215, 1348:
			description, found, err := descriptorDescription(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, description, found)
		case 1216:
			description, found, err := columnDescription(ctx, model.OID(arguments[0].IntVal), arguments[1].IntVal, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, description, found)
		case 1993:
			return model.NullVal(model.TypeString), nil
		case 1716, 2509:
			return outputText(ctx, budget, arguments[0].StrVal)
		case 1640, 2505:
			text, found, err := viewDefinitionByName(ctx, arguments[0].StrVal, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 1641, 2506, 3159:
			text, found, err := viewDefinitionByOID(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 1387, 2508:
			text, found, err := constraintDefinition(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 1643, 2507:
			column := int64(0)
			if id == 2507 {
				column = arguments[1].IntVal
			}
			text, found, err := indexDefinition(ctx, model.OID(arguments[0].IntVal), column, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 1662, 2730:
			text, found, err := triggerDefinition(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 2098:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "definition", 0, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 2162:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "arguments", 0, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 2232:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "identity", 0, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 2165:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "result", 0, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 3808:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "default", arguments[1].IntVal, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 6197:
			text, found, err := routineText(ctx, model.OID(arguments[0].IntVal), "body", 0, budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 3352:
			text, found, err := partKeyDefinition(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 3408:
			text, found, err := partitionConstraintDefinition(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			return outputNullableText(ctx, budget, text, found)
		case 3415:
			return statisticText(ctx, model.OID(arguments[0].IntVal), "definition", budget)
		case 6173:
			return statisticText(ctx, model.OID(arguments[0].IntVal), "expressions", budget)
		case 6174:
			return statisticText(ctx, model.OID(arguments[0].IntVal), "columns", budget)
		case 3424:
			oid, found, err := partitionRoot(ctx, model.OID(arguments[0].IntVal), budget)
			if err != nil {
				return model.Value{}, err
			}
			if !found {
				return model.NullVal(model.TypeInt64), nil
			}
			return model.Int64Val(int64(oid)), nil
		case 2079:
			visible, err := catalogObjectVisible(ctx, model.OID(arguments[0].IntVal), []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation}, budget)
			return model.BoolVal(visible), err
		case 2080:
			if _, found := LookupType(model.TypeID(arguments[0].IntVal)); found {
				return model.BoolVal(true), nil
			}
			visible, err := catalogObjectVisible(ctx, model.OID(arguments[0].IntVal), []model.ObjectKind{model.ObjectType}, budget)
			return model.BoolVal(visible), err
		case 2081:
			if _, found := LookupSignature(model.FunctionID(arguments[0].IntVal)); found {
				return model.BoolVal(true), nil
			}
			visible, err := catalogObjectVisible(ctx, model.OID(arguments[0].IntVal), []model.ObjectKind{model.ObjectRoutine}, budget)
			return model.BoolVal(visible), err
		case 2082:
			_, found := LookupOperator(model.OperatorID(arguments[0].IntVal))
			return model.BoolVal(found), nil
		case 2083:
			_, found := LookupOperatorClass(model.OID(arguments[0].IntVal))
			return model.BoolVal(found), nil
		case 2093:
			return model.BoolVal(false), nil
		case 3403:
			value, err := statisticText(ctx, model.OID(arguments[0].IntVal), "columns", budget)
			return model.BoolVal(err == nil && !value.IsNull), err
		case 3815:
			_, found := LookupCollation(model.CollationID(arguments[0].IntVal))
			return model.BoolVal(found), nil
		case 3829:
			found := false
			for class := range OperatorClasses() {
				found = found || class.Family == model.OID(arguments[0].IntVal)
			}
			return model.BoolVal(found), nil
		case 637:
			return indexProperty(ctx, model.OID(arguments[0].IntVal), 0, arguments[1].StrVal, budget)
		case 638:
			return indexProperty(ctx, model.OID(arguments[0].IntVal), arguments[1].IntVal, arguments[2].StrVal, budget)
		case 3842:
			return relationUpdatable(ctx, model.OID(arguments[0].IntVal), arguments[1].IntVal != 0, budget)
		case 3843:
			return columnUpdatable(ctx, model.OID(arguments[0].IntVal), arguments[1].IntVal, arguments[2].IntVal != 0, budget)
		case 1642:
			runtime, err := roleCapability(budget)
			if err != nil {
				return model.Value{}, err
			}
			name, found, err := runtime.RoleName(ctx, model.OID(arguments[0].IntVal))
			if err != nil {
				return model.Value{}, err
			}
			if !found {
				return model.Value{}, sqlError("42704", "role does not exist")
			}
			return outputText(ctx, budget, name)
		case 1665:
			return serialSequenceName(ctx, arguments[0].StrVal, arguments[1].StrVal, budget)
		case 4032:
			if budget.Sequences == nil {
				return model.Value{}, sqlError("55000", "sequence runtime is unavailable")
			}
			value, err := budget.Sequences.CurrentSequence(ctx, model.OID(arguments[0].IntVal))
			return model.Int64Val(value), err
		case 3382, 3839, 3954:
			return catalogIdentityFunction(ctx, id, arguments, budget)
		default:
			return model.Value{}, &MissingImplementationError{Signature: signature}
		}
	}
}

func indexProperty(ctx context.Context, oid model.OID, column int64, property string, budget CallBudget) (model.Value, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	result := model.NullVal(model.TypeBoolean)
	_, err = runtime.VisitCatalogObject(ctx, oid, func(summary model.CatalogObjectSummary) error {
		index, ok := summary.Descriptor.Index()
		if !ok {
			return nil
		}
		property = strings.ToLower(property)
		if column == 0 {
			switch property {
			case "clusterable", "bitmap_scan":
				result = model.BoolVal(false)
			case "index_scan", "backward_scan":
				result = model.BoolVal(true)
			}
			return nil
		}
		position := int(column - 1)
		if position < 0 || position >= len(index.Keys)+len(index.Include) {
			return nil
		}
		key := position < len(index.Keys)
		var descending, nullsFirst bool
		if key {
			descending, nullsFirst = index.Keys[position].Descending, index.Keys[position].NullsFirst
		}
		switch property {
		case "asc":
			result = model.BoolVal(key && !descending)
		case "desc":
			result = model.BoolVal(key && descending)
		case "nulls_first":
			result = model.BoolVal(key && nullsFirst)
		case "nulls_last":
			result = model.BoolVal(key && !nullsFirst)
		case "orderable", "search_nulls":
			result = model.BoolVal(key)
		case "distance_orderable", "search_array":
			result = model.BoolVal(false)
		case "returnable":
			result = model.BoolVal(true)
		}
		return nil
	})
	return result, err
}

func relationUpdatable(ctx context.Context, oid model.OID, includeTriggers bool, budget CallBudget) (model.Value, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	mask, err := runtime.RelationUpdatability(ctx, oid, includeTriggers)
	return model.Int64Val(int64(mask)), err
}

func columnUpdatable(ctx context.Context, oid model.OID, attribute int64, includeTriggers bool, budget CallBudget) (model.Value, error) {
	if attribute <= 0 {
		return model.BoolVal(false), nil
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	updatable, err := runtime.ColumnUpdatable(ctx, oid, attribute, includeTriggers)
	return model.BoolVal(updatable), err
}

func serialSequenceName(ctx context.Context, tableName, columnText string, budget CallBudget) (model.Value, error) {
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	parts, err := ParseQualifiedName(ctx, tableName, budget.MemoryBytes)
	if err != nil {
		return model.Value{}, err
	}
	columnParts, err := ParseQualifiedName(ctx, columnText, budget.MemoryBytes)
	if err != nil || len(columnParts) != 1 {
		return model.Value{}, errors.Join(err, sqlError("42703", "invalid column name"))
	}
	var tableID model.ObjectID
	var columnID model.ColumnID
	var identity model.ObjectID
	found, err := runtime.ResolveCatalogObject(ctx, []model.ObjectKind{model.ObjectTable}, parts, "", func(summary model.CatalogObjectSummary) error {
		table, ok := summary.Descriptor.Table()
		if !ok {
			return nil
		}
		tableID = summary.Descriptor.ID()
		for _, column := range table.Columns {
			if column.Name == columnParts[0] {
				columnID, identity = column.ID, column.IdentitySequence
			}
		}
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if !found {
		return model.Value{}, sqlError("42P01", "relation does not exist")
	}
	if columnID == 0 {
		return model.NullVal(model.TypeString), nil
	}
	var name string
	err = runtime.VisitCatalogObjects(ctx, []model.ObjectKind{model.ObjectSequence}, func(summary model.CatalogObjectSummary) error {
		descriptor := summary.Descriptor
		// Identity columns record their sequence on the column itself; serial
		// columns carry only the sequence's owner link back to the column.
		if descriptor.ID() != identity {
			metadata, ok := descriptor.Sequence()
			if !ok || metadata.OwnerTable != tableID || metadata.OwnerColumn != columnID {
				return nil
			}
		}
		name = catalogQualifiedName(summary.NamespaceName, descriptor.Name().Name)
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	return outputNullableText(ctx, budget, name, name != "")
}

func catalogIdentityFunction(ctx context.Context, id model.FunctionID, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if id == 3954 {
		return catalogAddressLookup(ctx, arguments, budget)
	}
	classID := model.OID(arguments[0].IntVal)
	objectID := model.OID(arguments[1].IntVal)
	subID := arguments[2].IntVal
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	var typ, schema, name, identity string
	found, err := runtime.VisitCatalogObject(ctx, objectID, func(summary model.CatalogObjectSummary) error {
		kind := summary.Descriptor.Kind()
		expected := catalogClassForKind(kind)
		if expected != classID || subID < 0 {
			return nil
		}
		typ = catalogKindName(kind)
		schema = strings.Clone(summary.NamespaceName)
		name = strings.Clone(summary.Descriptor.Name().Name)
		identity = catalogQualifiedName(schema, name)
		if subID > 0 {
			table, ok := summary.Descriptor.Table()
			if !ok {
				return nil
			}
			visible := int64(0)
			for _, column := range table.Columns {
				if column.State != model.StatePublic || column.SystemIdentity {
					continue
				}
				visible++
				if visible == subID {
					name = column.Name
					identity += "." + catalogIdentifier(column.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if !found || typ == "" {
		return model.Value{}, sqlError("42704", "catalog object does not exist")
	}
	if id == 3382 {
		return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal(typ), introspectionArray([]model.Value{model.StringVal(schema), model.StringVal(name)}), introspectionArray(nil)}}, nil
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal(typ), optionalText(schema), model.StringVal(name), model.StringVal(identity)}}, nil
}

func catalogAddressLookup(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	objectType := strings.ToLower(strings.TrimSpace(arguments[0].StrVal))
	names := make([]string, len(arguments[1].ArrayVal))
	for i, value := range arguments[1].ArrayVal {
		if value.IsNull || value.Type != model.TypeString {
			return model.Value{}, sqlError("22023", "catalog object name array contains null or non-text")
		}
		names[i] = value.StrVal
	}
	args := make([]string, len(arguments[2].ArrayVal))
	for i, value := range arguments[2].ArrayVal {
		if value.IsNull || value.Type != model.TypeString {
			return model.Value{}, sqlError("22023", "catalog object argument array contains null or non-text")
		}
		args[i] = value.StrVal
	}
	if objectType == "collation" {
		if len(names) == 0 || len(names) > 2 || len(args) != 0 {
			return model.Value{}, sqlError("22023", "invalid collation address")
		}
		oid, err := resolveCollationName(ctx, strings.Join(names, "."), budget)
		if err != nil {
			return model.Value{}, err
		}
		return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(int64(catalogClassCollation)), model.Int64Val(int64(oid)), model.Int64Val(0)}}, nil
	}
	runtime, err := catalogCapability(budget)
	if err != nil {
		return model.Value{}, err
	}
	classID := catalogClassRelation
	kinds := []model.ObjectKind{}
	signature := ""
	columnName := ""
	switch objectType {
	case "schema":
		classID, kinds = catalogClassNamespace, []model.ObjectKind{model.ObjectSchema}
	case "table":
		kinds = []model.ObjectKind{model.ObjectTable}
	case "index":
		kinds = []model.ObjectKind{model.ObjectIndex}
	case "sequence":
		kinds = []model.ObjectKind{model.ObjectSequence}
	case "view":
		kinds = []model.ObjectKind{model.ObjectView}
	case "materialized view":
		kinds = []model.ObjectKind{model.ObjectMaterializedView}
	case "table column":
		kinds = []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}
		if len(names) < 2 {
			return model.Value{}, sqlError("22023", "table column address lacks a column name")
		}
		columnName = names[len(names)-1]
		names = names[:len(names)-1]
	case "function", "procedure", "routine":
		classID, kinds = catalogClassProcedure, []model.ObjectKind{model.ObjectRoutine}
		for i, argument := range args {
			typeID, modifier, typeErr := parseTypeReference(ctx, argument, budget)
			if typeErr != nil {
				return model.Value{}, typeErr
			}
			if i != 0 {
				signature += ","
			}
			signature += strconv.FormatUint(uint64(typeID), 10) + ":" + strconv.FormatInt(int64(modifier), 10)
		}
	case "type", "domain":
		classID, kinds = catalogClassType, []model.ObjectKind{model.ObjectType}
	default:
		return model.Value{}, sqlError("42704", "unsupported catalog object type")
	}
	if len(names) == 0 || len(names) > 2 {
		return model.Value{}, sqlError("22023", "invalid catalog object address")
	}
	var objectID model.OID
	subID := int64(0)
	found, err := runtime.ResolveCatalogObject(ctx, kinds, names, signature, func(summary model.CatalogObjectSummary) error {
		objectID = summary.Descriptor.OID()
		if columnName == "" {
			return nil
		}
		var columns []model.ColumnDescriptor
		if table, ok := summary.Descriptor.Table(); ok {
			columns = table.Columns
		} else if view, ok := summary.Descriptor.View(); ok {
			columns = view.Columns
		}
		visible := int64(0)
		for _, column := range columns {
			if column.State != model.StatePublic || column.SystemIdentity {
				continue
			}
			visible++
			if column.Name == columnName {
				subID = visible
			}
		}
		return nil
	})
	if err != nil {
		return model.Value{}, err
	}
	if !found || objectID == 0 || columnName != "" && subID == 0 {
		return model.Value{}, sqlError("42704", "catalog object does not exist")
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(int64(classID)), model.Int64Val(int64(objectID)), model.Int64Val(subID)}}, nil
}

func catalogClassForKind(kind model.ObjectKind) model.OID {
	switch kind {
	case model.ObjectSchema:
		return catalogClassNamespace
	case model.ObjectRoutine:
		return catalogClassProcedure
	case model.ObjectType:
		return catalogClassType
	default:
		return catalogClassRelation
	}
}

func catalogKindName(kind model.ObjectKind) string {
	switch kind {
	case model.ObjectSchema:
		return "schema"
	case model.ObjectTable:
		return "table"
	case model.ObjectIndex:
		return "index"
	case model.ObjectSequence:
		return "sequence"
	case model.ObjectView:
		return "view"
	case model.ObjectMaterializedView:
		return "materialized view"
	case model.ObjectRoutine:
		return "function"
	case model.ObjectType:
		return "type"
	default:
		return ""
	}
}
