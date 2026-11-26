package builtin

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type InformationSchemaFunction uint8

const (
	InformationCharMaxLength InformationSchemaFunction = iota + 1
	InformationCharOctetLength
	InformationDatetimePrecision
	InformationIndexPosition
	InformationIntervalType
	InformationNumericPrecision
	InformationNumericPrecisionRadix
	InformationNumericScale
	InformationTrueTypeID
	InformationTrueTypeModifier
	InformationExpandArray
)
const InformationSchemaFunctionCount = int(InformationExpandArray)


type InformationSchemaFunctionInfo struct {
	Operation InformationSchemaFunction
	Signature Signature
}

type InformationSchemaFunctionBinding struct {
	Operation InformationSchemaFunction
	Signature Signature
}

var informationSchemaFunctions = [...]InformationSchemaFunctionInfo{
	{Operation: InformationCharMaxLength, Signature: Signature{Name: "_pg_char_max_length", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationCharOctetLength, Signature: Signature{Name: "_pg_char_octet_length", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationDatetimePrecision, Signature: Signature{Name: "_pg_datetime_precision", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationExpandArray, Signature: Signature{Name: "_pg_expandarray", Arguments: [8]model.TypeID{2277}, ArgumentCount: 1, Result: 2249, Strict: true, ReturnsSet: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Rows: 100, Support: 3996, Parallel: 's', Source: "SELECT * FROM pg_catalog.unnest($1) WITH ORDINALITY"}},
	{Operation: InformationIndexPosition, Signature: Signature{Name: "_pg_index_position", Arguments: [8]model.TypeID{26, 21}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 's', Kind: 'f', Language: 14, Cost: 100, Parallel: 'u'}},
	{Operation: InformationIntervalType, Signature: Signature{Name: "_pg_interval_type", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 25, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationNumericPrecision, Signature: Signature{Name: "_pg_numeric_precision", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationNumericPrecisionRadix, Signature: Signature{Name: "_pg_numeric_precision_radix", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationNumericScale, Signature: Signature{Name: "_pg_numeric_scale", Arguments: [8]model.TypeID{26, 23}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationTrueTypeID, Signature: Signature{Name: "_pg_truetypid", Arguments: [8]model.TypeID{1249, 1247}, ArgumentCount: 2, Result: 26, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
	{Operation: InformationTrueTypeModifier, Signature: Signature{Name: "_pg_truetypmod", Arguments: [8]model.TypeID{1249, 1247}, ArgumentCount: 2, Result: 23, Strict: true, Volatility: 'i', Kind: 'f', Language: 14, Cost: 100, Parallel: 's'}},
}

func InformationSchemaFunctions() []InformationSchemaFunctionInfo {
	return slices.Clone(informationSchemaFunctions[:])
}

func InformationSchemaFunctionByOperation(operation InformationSchemaFunction) (InformationSchemaFunctionInfo, bool) {
	for _, function := range informationSchemaFunctions {
		if function.Operation == operation {
			return function, true
		}
	}
	return InformationSchemaFunctionInfo{}, false
}

func InformationSchemaInputArguments(operation InformationSchemaFunction) ([2]string, uint8, bool) {
	switch operation {
	case InformationCharMaxLength, InformationCharOctetLength, InformationDatetimePrecision, InformationNumericPrecision, InformationNumericPrecisionRadix, InformationNumericScale:
		return [2]string{"typid", "typmod"}, 2, true
	case InformationIntervalType:
		return [2]string{"typid", "mod"}, 2, true
	default:
		return [2]string{}, 0, false
	}
}

func InformationSchemaOutputArguments(operation InformationSchemaFunction) ([3]model.TypeID, [3]byte, [3]string, uint8, bool) {
	if operation != InformationExpandArray {
		return [3]model.TypeID{}, [3]byte{}, [3]string{}, 0, false
	}
	return [3]model.TypeID{2277, 2283, 23}, [3]byte{'i', 'o', 'o'}, [3]string{"", "x", "n"}, 3, true
}

func resolveInformationSchema(binding InformationSchemaFunctionBinding) (Callable, bool) {
	if binding.Operation == 0 {
		return Callable{}, false
	}
	signature := binding.Signature
	if signature.ReturnsSet {
		return Callable{}, true
	}
	kernel := informationSchemaKernel(binding.Operation)
	callable := Callable{signature: signature, kernel: kernel, resultStorage: primitiveStorage(signature.Result)}
	for i := range signature.ArgumentCount {
		callable.argumentStorage[i] = primitiveStorage(signature.Arguments[i])
	}
	return callable, true
}

func informationSchemaKernel(operation InformationSchemaFunction) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		typeID, modifier := model.TypeID(arguments[0].IntVal), int32(arguments[1].IntVal)
		switch operation {
		case InformationCharMaxLength:
			return informationCharacterLength(typeID, modifier, false), nil
		case InformationCharOctetLength:
			return informationCharacterLength(typeID, modifier, true), nil
		case InformationDatetimePrecision:
			return informationDatetime(typeID, modifier), nil
		case InformationIntervalType:
			return informationInterval(typeID, modifier), nil
		case InformationNumericPrecision:
			return informationNumericPrecision(typeID, modifier), nil
		case InformationNumericPrecisionRadix:
			return informationNumericRadix(typeID), nil
		case InformationNumericScale:
			return informationNumericScale(typeID, modifier), nil
		case InformationTrueTypeID, InformationTrueTypeModifier:
			return informationTrueType(operation, arguments)
		case InformationIndexPosition:
			runtime, ok := budget.Relations.(interface {
				InformationSchemaIndexPosition(context.Context, model.OID, int16) (int16, bool, error)
			})
			if !ok {
				return model.Value{}, sqlError("55000", "information schema index catalog is not configured")
			}
			position, found, err := runtime.InformationSchemaIndexPosition(ctx, model.OID(arguments[0].IntVal), int16(arguments[1].IntVal))
			if err != nil {
				return model.Value{}, err
			}
			if !found {
				return model.NullVal(model.TypeInt64), nil
			}
			return model.Int64Val(int64(position)), nil
		}
		return model.Value{}, sqlError("XX000", "unknown information schema helper")
	}
}

func informationCharacterLength(id model.TypeID, modifier int32, octets bool) model.Value {
	if octets {
		switch id {
		case 25, 1042, 1043:
			if modifier == -1 {
				return model.Int64Val(1 << 30)
			}
			if id == 25 {
				return model.NullVal(model.TypeInt64)
			}
			return model.Int64Val(int64(modifier-4) * 4)
		default:
			return model.NullVal(model.TypeInt64)
		}
	}
	switch id {
	case 1042, 1043:
		if modifier == -1 {
			return model.NullVal(model.TypeInt64)
		}
		return model.Int64Val(int64(modifier - 4))
	case 1560, 1562:
		if modifier == -1 {
			return model.NullVal(model.TypeInt64)
		}
		return model.Int64Val(int64(modifier))
	default:
		return model.NullVal(model.TypeInt64)
	}
}

func informationDatetime(id model.TypeID, modifier int32) model.Value {
	if id == 1082 {
		return model.Int64Val(0)
	}
	if id != 1083 && id != 1114 && id != 1184 && id != 1186 && id != 1266 {
		return model.NullVal(model.TypeInt64)
	}
	if modifier < 0 || id == 1186 && modifier&0xffff == 0xffff {
		return model.Int64Val(6)
	}
	if id == 1186 {
		return model.Int64Val(int64(modifier & 0xffff))
	}
	return model.Int64Val(int64(modifier))
}

func informationInterval(id model.TypeID, modifier int32) model.Value {
	if id != 1186 || modifier < 0 {
		return model.NullVal(model.TypeString)
	}
	var name string
	switch (modifier >> 16) & 0x7fff {
	case 1:
		name = "YEAR"
	case 2:
		name = "MONTH"
	case 3:
		name = "YEAR TO MONTH"
	case 4:
		name = "DAY"
	case 8:
		name = "HOUR"
	case 12:
		name = "DAY TO HOUR"
	case 16:
		name = "MINUTE"
	case 24:
		name = "HOUR TO MINUTE"
	case 28:
		name = "DAY TO MINUTE"
	case 32:
		name = "SECOND"
	case 48:
		name = "MINUTE TO SECOND"
	case 56:
		name = "HOUR TO SECOND"
	case 60:
		name = "DAY TO SECOND"
	default:
		return model.NullVal(model.TypeString)
	}
	return model.StringVal(name)
}

func informationNumericPrecision(id model.TypeID, modifier int32) model.Value {
	switch id { case 21: return model.Int64Val(16); case 23: return model.Int64Val(32); case 20: return model.Int64Val(64); case 700: return model.Int64Val(24); case 701: return model.Int64Val(53) }
	if id != 1700 || modifier < 4 { return model.NullVal(model.TypeInt64) }
	return model.Int64Val(int64(uint32(modifier-4) >> 16))
}

func informationNumericRadix(id model.TypeID) model.Value {
	switch id { case 21, 23, 20, 700, 701: return model.Int64Val(2); case 1700: return model.Int64Val(10); default: return model.NullVal(model.TypeInt64) }
}

func informationNumericScale(id model.TypeID, modifier int32) model.Value {
	switch id {
	case 21, 23, 20:
		return model.Int64Val(0)
	}
	if id != 1700 || modifier == -1 {
		return model.NullVal(model.TypeInt64)
	}
	return model.Int64Val(int64(numericTypmodScale(modifier)))
}

// numericTypmodScale decodes the scale a numeric typmod carries in its low 11
// bits, sign-extended the way PostgreSQL's numeric_typmod_scale does. Decoding
// the low 16 bits as int16 instead reports numeric(6,2) as numeric(6,-32766).
func numericTypmodScale(modifier int32) int32 {
	return int32(((uint32(modifier-4) & 0x7ff) ^ 1024) - 1024)
}

func informationTrueType(operation InformationSchemaFunction, arguments []model.Value) (model.Value, error) {
	attribute, typ := arguments[0].ArrayVal, arguments[1].ArrayVal
	if len(attribute) <= 5 || len(typ) <= 26 { return model.Value{}, sqlError("42804", "information schema helper requires pg_attribute and pg_type rows") }
	domain := !typ[6].IsNull && typ[6].StrVal == "d"
	if operation == InformationTrueTypeID {
		if domain { return model.Int64Val(typ[25].IntVal), nil }
		return model.Int64Val(attribute[2].IntVal), nil
	}
	if domain { return model.Int64Val(typ[26].IntVal), nil }
	return model.Int64Val(attribute[5].IntVal), nil
}

