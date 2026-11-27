package bind

import (
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type RoutineCompiler interface {
	Compile(context.Context, *zpgparse.CreateFunctionStmt, model.RoutineMetadata, CatalogSnapshot, Options, bool) (model.RoutineMetadata, error)
}

type CreateRoutineOptions struct {
	Declaration         CreateDeclarationOptions
	Compiler            RoutineCompiler
	CheckFunctionBodies bool
	CurrentSetting      func(string) (string, error)
}

func BindCreateRoutine(ctx context.Context, statement *zpgparse.CreateFunctionStmt, snapshot CatalogSnapshot, options CreateRoutineOptions) (*BoundDeclaration, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE FUNCTION or PROCEDURE", 0)
	}
	// A routine parameter or result may name a pseudo-type (anyelement,
	// anyarray, internal), which is legal in PostgreSQL and resolved per call.
	b, err := declarationBinderDeclaredTypes(ctx, snapshot, options.Declaration, &zpgparse.Node{Node: &zpgparse.Node_CreateFunctionStmt{CreateFunctionStmt: statement}})
	if err != nil {
		return nil, err
	}
	if options.Compiler == nil {
		return nil, bindError("22023", "routine declaration requires a body compiler", 0)
	}
	parts, err := names(statement.Funcname)
	if err != nil || len(parts) == 0 || len(parts) > 2 {
		return nil, bindError("42601", "invalid routine name", 0)
	}
	name := parts[len(parts)-1]
	if err := declarationName(name); err != nil {
		return nil, err
	}
	schema, err := resolveRoutineSchema(ctx, snapshot, options.Declaration.Bind.SearchPath, parts)
	if err != nil {
		return nil, err
	}
	b.dependency(schema, 0)
	metadata, err := bindRoutineMetadata(ctx, b, statement, snapshot, options)
	if err != nil {
		return nil, err
	}
	signature := routineSignature(metadata.Parameters)
	identity := model.NamespaceName{Parent: schema.ID(), Kind: model.ObjectRoutine, Name: name, Signature: signature}
	existing, err := resolveRoutineOverload(ctx, snapshot, schema.ID(), name, metadata.Parameters)
	if err != nil {
		return nil, err
	}
	var id model.ObjectID
	var oid model.OID
	version := uint64(1)
	if existing != nil {
		if !statement.Replace {
			return nil, bindError("42723", "routine already exists", 0)
		}
		spec, err := existing.Spec(ctx)
		if err != nil {
			return nil, err
		}
		if spec.Routine == nil || spec.Routine.Procedure != metadata.Procedure || !sameRoutineResult(spec.Routine.Result, metadata.Result) {
			return nil, bindError("42P13", "cannot change routine kind or return type with CREATE OR REPLACE", 0)
		}
		metadata.Owner = spec.Routine.Owner
		id, oid, version = existing.ID(), existing.OID(), existing.Version()+1
		b.dependency(existing, 0)
	} else {
		id, oid, err = options.Declaration.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, err
		}
	}
	compiled, err := options.Compiler.Compile(ctx, statement, metadata, snapshot, options.Declaration.Bind, options.CheckFunctionBodies)
	if err != nil {
		return nil, err
	}
	for _, dependency := range compiled.Body.Dependencies {
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return nil, err
		}
		if published.Descriptor.Version() != dependency.Version {
			return nil, bindError("0A000", "routine body dependency changed during binding", 0)
		}
		b.dependency(published.Descriptor, dependency.Column)
	}
	dependencies := slices.DeleteFunc(slices.Clone(b.dependencies), func(dependency model.Dependency) bool { return dependency.Object == id })
	description := ""
	if existing != nil {
		description = existing.Description()
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: id, OID: oid, Name: identity, Version: version, State: model.StatePublic, Description: description,
		Dependencies: dependencies, Routine: &compiled,
	}, options.Declaration.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{descriptor}, base: existing, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func resolveRoutineSchema(ctx context.Context, snapshot CatalogSnapshot, path []string, parts []string) (*model.Descriptor, error) {
	if len(parts) == 2 {
		path = parts[:1]
	}
	for _, schemaName := range path {
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if published.Descriptor.State() == model.StatePublic {
			return published.Descriptor, nil
		}
	}
	return nil, bindError("3F000", "no schema has been selected to create in", 0)
}

func bindRoutineMetadata(ctx context.Context, b *binder, statement *zpgparse.CreateFunctionStmt, snapshot CatalogSnapshot, options CreateRoutineOptions) (model.RoutineMetadata, error) {
	metadata := model.RoutineMetadata{Procedure: statement.IsProcedure, Owner: options.Declaration.Owner, Volatility: "volatile", Parallel: "unsafe", Cost: 100, Atomic: statement.SqlBody != nil}
	defaultSeen := false
	for _, node := range statement.Parameters {
		parameter := node.GetFunctionParameter()
		if parameter == nil || parameter.ArgType == nil {
			return metadata, bindError("42601", "invalid routine parameter", 0)
		}
		layout, err := b.typeName(parameter.ArgType)
		if err != nil {
			return metadata, err
		}
		if model.IsPseudoTypeID(layout.Type.ID) && !model.IsRoutineParameterPseudoTypeID(layout.Type.ID) {
			parts, _ := names(parameter.ArgType.Names)
			name := ""
			if len(parts) != 0 {
				name = " " + parts[len(parts)-1]
			}
			return metadata, bindError("42P13", "functions cannot accept type"+name, int(parameter.Location)+1)
		}
		mode := routineParameterMode(parameter.Mode)
		if mode == "" {
			return metadata, bindError("42601", "invalid routine parameter mode", int(parameter.Location)+1)
		}
		bound := model.RoutineParameter{Name: parameter.Name, Layout: layout, Mode: mode}
		if parameter.Defexpr != nil {
			if mode != "in" && mode != "inout" {
				return metadata, bindError("42P13", "only input parameters can have defaults", int(parameter.Location)+1)
			}
			definition, err := BindRoutineDefault(ctx, parameter.Defexpr, layout, snapshot, options.Declaration.Bind)
			if err != nil {
				return metadata, err
			}
			bound.Default = &definition.Definition
			b.warnings = append(b.warnings, definition.Warnings...)
			for _, dependency := range definition.Definition.Dependencies {
				published, err := snapshot.Descriptor(ctx, dependency.Object)
				if err != nil {
					return metadata, err
				}
				b.dependency(published.Descriptor, dependency.Column)
			}
			defaultSeen = true
		} else if defaultSeen && (mode == "in" || mode == "inout") {
			return metadata, bindError("42P13", "input parameters after one with a default must also have defaults", int(parameter.Location)+1)
		}
		metadata.Parameters = append(metadata.Parameters, bound)
	}
	if err := bindRoutineOptions(ctx, b, snapshot, statement.Options, &metadata, options.CurrentSetting); err != nil {
		return metadata, err
	}
	outputs := routineOutputs(metadata.Parameters)
	if len(outputs) == 1 {
		metadata.Result = outputs[0].Layout
	} else if len(outputs) > 1 {
		metadata.Result = model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: -1}, Children: make([]model.TypeLayout, len(outputs)), FieldNames: make([]string, len(outputs))}
		for i, output := range outputs {
			metadata.Result.Children[i], metadata.Result.FieldNames[i] = output.Layout, output.Name
		}
	} else if statement.ReturnType != nil {
		// SETOF is the result declaration's own flag, not part of the named
		// type; the shared parse node must not be mutated for rebinding.
		returnType := &zpgparse.TypeName{Names: statement.ReturnType.Names, TypeOid: statement.ReturnType.TypeOid, PctType: statement.ReturnType.PctType, Typmods: statement.ReturnType.Typmods, Typemod: statement.ReturnType.Typemod, ArrayBounds: statement.ReturnType.ArrayBounds, Location: statement.ReturnType.Location}
		result, err := b.typeName(returnType)
		if err != nil {
			return metadata, err
		}
		metadata.Result = result
		metadata.ReturnsSet = statement.ReturnType.Setof
	} else if statement.IsProcedure {
		metadata.Result = model.TypeLayout{Type: model.Type{ID: 2278, Storage: model.TypeAny, Modifier: -1}}
	} else {
		return metadata, bindError("42P13", "function result type is missing", 0)
	}
	for _, parameter := range metadata.Parameters {
		if parameter.Mode == "table" {
			metadata.ReturnsSet = true
		}
	}
	if metadata.ReturnsSet {
		if metadata.Rows == 0 {
			metadata.Rows = 1000
		}
	} else if metadata.Rows != 0 {
		return metadata, bindError("22023", "ROWS is not applicable when function does not return a set", 0)
	}
	metadata.LanguageOID = map[string]model.OID{"internal": 12, "c": 13, "sql": 14, "plpgsql": 13563}[metadata.Language]
	if metadata.LanguageOID == 0 {
		return metadata, bindError("42704", "routine language does not exist", 0)
	}
	return metadata, nil
}

func routineParameterMode(mode zpgparse.FunctionParameterMode) string {
	switch mode {
	case zpgparse.FunctionParameterMode_FUNC_PARAM_DEFAULT, zpgparse.FunctionParameterMode_FUNC_PARAM_IN:
		return "in"
	case zpgparse.FunctionParameterMode_FUNC_PARAM_OUT:
		return "out"
	case zpgparse.FunctionParameterMode_FUNC_PARAM_INOUT:
		return "inout"
	case zpgparse.FunctionParameterMode_FUNC_PARAM_VARIADIC:
		return "variadic"
	case zpgparse.FunctionParameterMode_FUNC_PARAM_TABLE:
		return "table"
	default:
		return ""
	}
}

func bindRoutineOptions(ctx context.Context, b *binder, snapshot CatalogSnapshot, nodes []*zpgparse.Node, metadata *model.RoutineMetadata, currentSetting func(string) (string, error)) error {
	for _, node := range nodes {
		option := node.GetDefElem()
		if option == nil {
			return bindError("42601", "invalid routine option", 0)
		}
		switch option.Defname {
		case "language":
			metadata.Language = strings.ToLower(option.Arg.GetString_().GetSval())
		case "volatility":
			metadata.Volatility = strings.ToLower(option.Arg.GetString_().GetSval())
		case "strict":
			metadata.Strict = option.Arg.GetBoolean().GetBoolval()
		case "security":
			if option.Arg.GetBoolean().GetBoolval() {
				return unsupported(Authorization, int(option.Location)+1)
			}
		case "as":
			if list := option.Arg.GetList(); list != nil {
				if len(list.Items) > 0 && list.Items[0].GetString_() != nil {
					metadata.Source = list.Items[0].GetString_().GetSval()
				}
				if len(list.Items) > 1 && list.Items[1].GetString_() != nil {
					metadata.Binary = list.Items[1].GetString_().GetSval()
				}
			} else if value := option.Arg.GetString_(); value != nil {
				metadata.Source = value.Sval
			}
		case "leakproof":
			if option.Arg.GetBoolean().GetBoolval() {
				return unsupported(Authorization, 0)
			}
		case "parallel":
			value := strings.ToLower(option.Arg.GetString_().GetSval())
			if value != "unsafe" && value != "restricted" && value != "safe" {
				return bindError("22023", "invalid PARALLEL option", int(option.Location)+1)
			}
			metadata.Parallel = value
		case "cost":
			value, err := routineNumericOption(option)
			if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return bindError("22023", "COST must be greater than zero", int(option.Location)+1)
			}
			metadata.Cost = value
		case "rows":
			value, err := routineNumericOption(option)
			if err != nil || value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || !metadata.ReturnsSet {
				return bindError("22023", "ROWS is only valid for a set-returning routine", int(option.Location)+1)
			}
			metadata.Rows = value
		case "set":
			setting, err := bindRoutineSetting(b, option.Arg.GetVariableSetStmt(), currentSetting)
			if err != nil {
				return err
			}
			replaced := false
			for i := range metadata.Settings {
				if strings.EqualFold(metadata.Settings[i].Name, setting.Name) {
					metadata.Settings[i], replaced = setting, true
					break
				}
			}
			if !replaced {
				metadata.Settings = append(metadata.Settings, setting)
			}
		case "support":
			routine, err := resolveSupportRoutine(ctx, b, snapshot, routineOptionText(option.Arg))
			if err != nil {
				return err
			}
			metadata.Support, metadata.SupportFunction = routine.ID(), model.FunctionID(routine.OID())
		case "transform", "window":
			return unsupported(ExternalCode, int(option.Location)+1)
		default:
			return bindError("42601", "unrecognized routine option", 0)
		}
	}
	if metadata.Language == "" {
		return bindError("42P13", "routine language is missing", 0)
	}
	if metadata.Language == "internal" {
		metadata.ExternalCode, metadata.Source = metadata.Source, ""
	}
	if metadata.Language == "c" {
		metadata.Binary, metadata.ExternalCode, metadata.Source = metadata.Source, metadata.Binary, ""
	}
	return nil
}

func routineOptionText(node *zpgparse.Node) string {
	if node == nil {
		return ""
	}
	if value := node.GetString_(); value != nil {
		return value.Sval
	}
	if list := node.GetList(); list != nil && len(list.Items) == 1 && list.Items[0].GetString_() != nil {
		return list.Items[0].GetString_().GetSval()
	}
	return ""
}

func resolveSupportRoutine(ctx context.Context, b *binder, snapshot CatalogSnapshot, name string) (*model.Descriptor, error) {
	parts := strings.Split(name, ".")
	if name == "" || len(parts) > 2 {
		return nil, bindError("42883", "support function does not exist", 0)
	}
	var selected *model.Descriptor
	for _, schemaName := range b.functionPath(parts) {
		namespace, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectRoutine, func(published catalog.PublishedDescriptor) error {
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.Name().Name != parts[len(parts)-1] {
				return nil
			}
			spec, err := descriptor.Spec(ctx)
			if err != nil {
				return err
			}
			if spec.Routine == nil || spec.Routine.Procedure || spec.Routine.Result.Type.ID != 2281 {
				return nil
			}
			inputs := routineInputs(spec.Routine.Parameters)
			if len(inputs) != 1 || inputs[0].Layout.Type.ID != 2281 {
				return nil
			}
			if selected != nil {
				return bindError("42725", "support function reference is ambiguous", 0)
			}
			selected = descriptor
			return nil
		})
		if err != nil {
			return nil, err
		}
		if selected != nil {
			b.dependency(namespace.Descriptor, 0)
			b.dependency(selected, 0)
			return selected, nil
		}
	}
	return nil, bindError("42883", "support function does not exist", 0)
}

func routineNumericOption(option *zpgparse.DefElem) (float64, error) {
	if option == nil || option.Arg == nil {
		return 0, model.ErrInvalidDescriptor
	}
	constant := option.Arg.GetAConst()
	if constant == nil || constant.Isnull {
		return 0, model.ErrInvalidDescriptor
	}
	var value float64
	switch {
	case constant.GetIval() != nil:
		value = float64(constant.GetIval().Ival)
	case constant.GetFval() != nil:
		parsed, err := strconv.ParseFloat(constant.GetFval().Fval, 64)
		if err != nil {
			return 0, err
		}
		value = parsed
	default:
		return 0, model.ErrInvalidDescriptor
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, model.ErrInvalidDescriptor
	}
	return value, nil
}

func bindRoutineSetting(b *binder, statement *zpgparse.VariableSetStmt, current func(string) (string, error)) (model.RoutineSetting, error) {
	if statement == nil || statement.Name == "" || statement.IsLocal {
		return model.RoutineSetting{}, bindError("42601", "invalid routine SET option", 0)
	}
	if err := ValidateSetting(statement.Name, int(statement.Location)+1); err != nil {
		return model.RoutineSetting{}, err
	}
	setting := model.RoutineSetting{Name: statement.Name}
	switch statement.Kind {
	case zpgparse.VariableSetKind_VAR_SET_DEFAULT, zpgparse.VariableSetKind_VAR_RESET:
		setting.Reset = true
		return setting, nil
	case zpgparse.VariableSetKind_VAR_SET_CURRENT:
		if current == nil {
			return model.RoutineSetting{}, bindError("22023", "routine SET FROM CURRENT requires session settings", int(statement.Location)+1)
		}
		value, err := current(statement.Name)
		setting.Value = value
		return setting, err
	case zpgparse.VariableSetKind_VAR_SET_VALUE:
		command, _, err := b.command(&zpgparse.Node{Node: &zpgparse.Node_VariableSetStmt{VariableSetStmt: statement}})
		if err != nil {
			return model.RoutineSetting{}, err
		}
		bound, ok := command.(SettingCommand)
		if !ok {
			return model.RoutineSetting{}, model.ErrInvalidDescriptor
		}
		setting.Value, err = routineSettingText(bound)
		return setting, err
	default:
		return model.RoutineSetting{}, bindError("42601", "invalid routine SET option", int(statement.Location)+1)
	}
}

func routineSettingText(command SettingCommand) (string, error) {
	if len(command.Values) == 0 {
		return "", bindError("42601", "setting requires a value", 0)
	}
	list := strings.EqualFold(command.Name, "search_path") || strings.EqualFold(command.Name, "DateStyle")
	if !list && len(command.Values) != 1 {
		return "", bindError("42601", "setting requires a single value", 0)
	}
	parts := make([]string, len(command.Values))
	for i, value := range command.Values {
		if value.IsNull {
			return "", bindError("22023", "setting cannot be null", 0)
		}
		switch value.Type {
		case model.TypeString:
			parts[i] = value.StrVal
			if strings.EqualFold(command.Name, "search_path") {
				parts[i] = `"` + strings.ReplaceAll(value.StrVal, `"`, `""`) + `"`
			}
		case model.TypeInt64:
			parts[i] = strconv.FormatInt(value.IntVal, 10)
		case model.TypeBoolean:
			parts[i] = strconv.FormatBool(value.IntVal != 0)
		case model.TypeInterval:
			if !strings.EqualFold(command.Name, "TimeZone") || value.IntervalVal.Months != 0 || value.IntervalVal.Days != 0 {
				return "", bindError("22023", "invalid interval setting value", 0)
			}
			parts[i] = strconv.FormatFloat(float64(value.IntervalVal.Microseconds)/3600000000, 'g', -1, 64)
		default:
			return "", bindError("22023", "invalid setting value type", 0)
		}
	}
	return strings.Join(parts, ", "), nil
}

func routineOutputs(parameters []model.RoutineParameter) []model.RoutineParameter {
	outputs := make([]model.RoutineParameter, 0, len(parameters))
	for _, parameter := range parameters {
		if parameter.Mode == "out" || parameter.Mode == "inout" || parameter.Mode == "table" {
			outputs = append(outputs, parameter)
		}
	}
	return outputs
}
func routineParameterIsInput(parameter model.RoutineParameter) bool {
	return parameter.Mode == "in" || parameter.Mode == "inout" || parameter.Mode == "variadic"
}

func routineInputs(parameters []model.RoutineParameter) []model.RoutineParameter {
	inputs := make([]model.RoutineParameter, 0, len(parameters))
	for _, parameter := range parameters {
		if routineParameterIsInput(parameter) {
			inputs = append(inputs, parameter)
		}
	}
	return inputs
}

func routineSignature(parameters []model.RoutineParameter) string {
	inputs := routineInputs(parameters)
	var signature strings.Builder
	for i, parameter := range inputs {
		if i != 0 {
			signature.WriteByte(',')
		}
		signature.WriteString(strconv.FormatUint(uint64(parameter.Layout.Type.ID), 10))
		signature.WriteByte(':')
		signature.WriteString(strconv.FormatInt(int64(parameter.Layout.Type.Modifier), 10))
	}
	return signature.String()
}

func resolveRoutineOverload(ctx context.Context, snapshot CatalogSnapshot, parent model.ObjectID, name string, parameters []model.RoutineParameter) (*model.Descriptor, error) {
	var found *model.Descriptor
	wanted := routineInputs(parameters)
	err := snapshot.ScanNamespace(ctx, parent, model.ObjectRoutine, func(published catalog.PublishedDescriptor) error {
		descriptor := published.Descriptor
		if descriptor.Name().Name != name {
			return nil
		}
		spec, err := descriptor.Spec(ctx)
		if err != nil || spec.Routine == nil {
			return errors.Join(err, model.ErrInvalidDescriptor)
		}
		existing := routineInputs(spec.Routine.Parameters)
		if len(existing) != len(wanted) {
			return nil
		}
		for i := range existing {
			if !sameRoutineResult(existing[i].Layout, wanted[i].Layout) {
				return nil
			}
		}
		found = descriptor
		return nil
	})
	return found, err
}

func sameRoutineResult(left, right model.TypeLayout) bool {
	return left.Type == right.Type && slices.Equal(left.FieldNames, right.FieldNames) && slices.EqualFunc(left.Children, right.Children, sameRoutineResult)
}
