package builtin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type InformationSchemaFunctionResolver interface {
	ResolveInformationSchemaFunction(model.FunctionID) (InformationSchemaFunctionBinding, bool)
	InformationSchemaFunctionFingerprint() [sha256.Size]byte
}
type InformationSchemaTypeBinding struct {
	Domain     InformationDomainKind
	Kind       model.TypeKind
	Layout     model.TypeLayout
	BaseLayout model.TypeLayout
	TypeIO     TypeIO
}

type InformationSchemaTypeResolver interface {
	ResolveInformationSchemaType(model.TypeID) (InformationSchemaTypeBinding, bool)
	InformationSchemaTypeFingerprint() [sha256.Size]byte
}

// ResolveOptions are copied into the compiled callable. DefaultCollation is the
// database's concrete collation, not the pg_catalog "default" alias (OID 100).
// ExtraFloatDigits uses PostgreSQL's setting range [-15, 3]; 1 selects shortest
// output. ByteaEscape selects escape output instead of PostgreSQL's default hex.
type ResolveOptions struct {
	Collation                  model.CollationID
	DefaultCollation           model.CollationID
	ExtraFloatDigits           int
	ByteaEscape                bool
	ArrayNullsDisabled         bool
	IntervalStyle              IntervalStyle
	DateStyle                  DateStyle
	DateOrder                  DateOrder
	TimeZone                   TimeZoneContext
	Types                      TypeResolver
	InformationSchemaFunctions InformationSchemaFunctionResolver
	InformationSchemaTypes     InformationSchemaTypeResolver
}

func (options ResolveOptions) SameSemantics(other ResolveOptions) bool {
	leftTypes, rightTypes := options.Types, other.Types
	leftInformationTypes, rightInformationTypes := options.InformationSchemaTypes, other.InformationSchemaTypes
	leftFunctions, rightFunctions := options.InformationSchemaFunctions, other.InformationSchemaFunctions
	options.Types, other.Types = nil, nil
	options.InformationSchemaTypes, other.InformationSchemaTypes = nil, nil
	options.InformationSchemaFunctions, other.InformationSchemaFunctions = nil, nil
	if options != other {
		return false
	}
	if leftTypes == nil || rightTypes == nil {
		if leftTypes != nil || rightTypes != nil {
			return false
		}
	} else if leftTypes.SemanticFingerprint() != rightTypes.SemanticFingerprint() {
		return false
	}
	if leftInformationTypes == nil || rightInformationTypes == nil {
		if leftInformationTypes != nil || rightInformationTypes != nil {
			return false
		}
	} else if leftInformationTypes.InformationSchemaTypeFingerprint() != rightInformationTypes.InformationSchemaTypeFingerprint() {
		return false
	}
	if leftFunctions == nil || rightFunctions == nil {
		return leftFunctions == nil && rightFunctions == nil
	}
	return leftFunctions.InformationSchemaFunctionFingerprint() == rightFunctions.InformationSchemaFunctionFingerprint()
}

type UnknownFunctionError struct{ ID model.FunctionID }

func (e *UnknownFunctionError) Error() string {
	return fmt.Sprintf("unknown builtin function %d", e.ID)
}

// MissingImplementationError is an implementation backlog item, not a product exclusion.
type MissingImplementationError struct{ Signature Signature }

func (e *MissingImplementationError) Error() string {
	return fmt.Sprintf("builtin implementation missing: %s (function %d)", e.Signature.Name, e.Signature.ID)
}

type scalarKernel func(context.Context, []model.Value, CallBudget) (model.Value, error)

// Callable is immutable and safe to share. Arguments are borrowed for the call;
// variable-width results own their storage and never retain an input lane.
type Callable struct {
	signature         Signature
	kernel            scalarKernel
	resultStorage     model.DataType
	indeterminateText bool
	argumentStorage   [8]model.DataType
	argumentTypes     []model.Type
	metadataBytes     uint64
}

func (c Callable) Signature() Signature { return c.signature }

// ArgumentCount is the bound input width, which can exceed the catalog's formal width.
func (c Callable) ArgumentCount() int {
	if c.argumentTypes != nil {
		return len(c.argumentTypes)
	}
	return int(c.signature.ArgumentCount)
}

func (c Callable) argumentType(index int) (model.TypeID, model.DataType) {
	if c.argumentTypes != nil {
		typ := c.argumentTypes[index]
		return typ.ID, typ.Storage
	}
	return c.signature.Arguments[index], c.argumentStorage[index]
}

// OwnedBytes reports retained type and semantic metadata, not live heap size.
func (c Callable) OwnedBytes() uint64 { return c.metadataBytes }

// ErrTypeBindingRequired asks callers to use ResolveTyped, not another executor.
var ErrTypeBindingRequired = &model.Error{Code: "42804", Message: "function requires concrete type binding"}

// Resolve performs executable lookup once, independently of the metadata inventory.
func Resolve(id model.FunctionID, options ResolveOptions) (Callable, error) {
	signature, ok := resolveSignature(id, options)
	if !ok {
		return Callable{}, &UnknownFunctionError{ID: id}
	}
	if options.InformationSchemaFunctions != nil {
		if binding, informationSchema := options.InformationSchemaFunctions.ResolveInformationSchemaFunction(id); informationSchema {
			callable, _ := resolveInformationSchema(binding)
			return callable, nil
		}
	}
	if introspectionScalarFunction(id) {
		return resolveUntypedIntrospectionScalar(signature, options)
	}
	if id == 3198 || id == 3200 || id == 3271 || id == 3273 {
		return Callable{}, ErrTypeBindingRequired
	}
	if _, supported := variadicTextPrefix(id); supported {
		return Callable{}, ErrTypeBindingRequired
	}
	if nativeScalarTyped(id) {
		return Callable{}, ErrTypeBindingRequired
	}
	op := primitiveOperation(id)
	if op == opMissing {
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	switch op {
	case opRangeScalar, opMultirangeScalar, opRangeIO, opRangeHash, opJSONRecord:
		return Callable{}, ErrTypeBindingRequired
	case opArrayIO, opArrayComparison, opArrayToString, opArrayConstruction, opArraySearch, opArrayReplace, opArrayMembership, opArrayFill, opArraySort, opNestedHash, opWidthBucketArray, opTypedQuote, opRecordIO, opRecordComparison, opNullCount, opJSONConversion:
		return Callable{}, ErrTypeBindingRequired
	}
	invalidFloatDigits := options.ExtraFloatDigits < -15 || options.ExtraFloatDigits > 3
	if op == opOutput && isFloat(signature.Arguments[0]) && invalidFloatDigits {
		return Callable{}, &model.Error{Code: "22023", Message: "extra_float_digits must be between -15 and 3"}
	}
	intervalIO := op == opIntervalInput || op == opIntervalOutput
	if intervalIO && !options.IntervalStyle.valid() {
		return Callable{}, sqlError("22023", "invalid interval output style")
	}
	dateStyledOutput := id == 1085 || id == 1151 || id == 1313
	if dateStyledOutput && (!options.DateStyle.valid() || !options.DateOrder.valid()) {
		return Callable{}, sqlError("22023", "invalid date output style or order")
	}
	collation := options.Collation
	if collation == 100 {
		collation = options.DefaultCollation
	}
	var kernel scalarKernel
	var metadataBytes uint64
	var err error
	switch op {
	case opNativeScalar:
		kernel, metadataBytes, err = resolveNativeScalar(signature, options)
	case opTimezone:
		kernel, metadataBytes, err = compileTimezone(signature, options)
	case opTemporalInput:
		kernel, metadataBytes, err = compileTemporalInput(signature, options)
	case opTemporalClock:
		kernel, metadataBytes, err = compileTemporalClock(signature, options)
	case opTemporalInRange:
		kernel, metadataBytes, err = compileTemporalInRange(signature, options)
	case opTimetz:
		kernel, metadataBytes, err = compileTimetz(signature, options)
	default:
		kernel = compilePrimitive(signature, op, options)
	}
	if err != nil {
		return Callable{}, err
	}
	if kernel == nil {
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
	callable := Callable{
		signature: signature, resultStorage: primitiveStorage(signature.Result), kernel: kernel,
		indeterminateText: (signature.Arguments[0] == 25 || signature.Arguments[0] == 1042) && isComparison(op) && collation != 950 && collation != 951,
		metadataBytes:     metadataBytes,
	}
	for i := range signature.ArgumentCount {
		callable.argumentStorage[i] = primitiveStorage(signature.Arguments[i])
	}
	return callable, nil
}

// ResolveSignature rejects stale or altered binding metadata before compilation.
func ResolveSignature(signature Signature, options ResolveOptions) (Callable, error) {
	known, ok := resolveSignature(signature.ID, options)
	if !ok {
		return Callable{}, &UnknownFunctionError{ID: signature.ID}
	}
	if known != signature {
		return Callable{}, &model.Error{Code: "42804", Message: "bound builtin signature differs from catalog"}
	}
	return Resolve(signature.ID, options)
}

func resolveSignature(id model.FunctionID, options ResolveOptions) (Signature, bool) {
	if options.InformationSchemaFunctions != nil {
		if binding, ok := options.InformationSchemaFunctions.ResolveInformationSchemaFunction(id); ok {
			return binding.Signature, true
		}
	}
	return LookupSignature(id)
}

// Call borrows arguments and returns independently owned variable-width storage.
func (c Callable) Call(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if c.kernel == nil {
		return model.Value{}, &model.Error{Code: "XX000", Message: "unresolved builtin callable"}
	}
	if budget.OutputBytes < 0 {
		return model.Value{}, &model.Error{Code: "22023", Message: "negative builtin output byte budget"}
	}
	if len(arguments) != c.ArgumentCount() {
		return model.Value{}, &model.Error{Code: "42883", Message: "builtin argument count does not match bound signature"}
	}
	hasNull := false
	nullCount := c.signature.ID == 438 || c.signature.ID == 440
	for i, argument := range arguments {
		id, storage := c.argumentType(i)
		if argument.Type != storage {
			return model.Value{}, &model.Error{Code: "42804", Message: "builtin argument storage does not match bound signature"}
		}
		if nullCount {
			continue
		}
		if argument.IsNull {
			hasNull = true
			continue
		}
		if isInteger(id) {
			if err := integerRange(argument.IntVal, id); err != nil {
				return model.Value{}, err
			}
		}
		if IsOIDType(id) && (argument.IntVal < 0 || argument.IntVal > math.MaxUint32) {
			return model.Value{}, sqlError("22003", "OID argument is outside uint32 range")
		}
		if id == 1266 {
			if _, _, err := DecodeTimetz(argument.BytesVal); err != nil {
				return model.Value{}, err
			}
		}
		switch id {
		case 600, 601, 602, 603, 604, 628, 718:
			if err := ValidateGeometryValue(ctx, id, argument.BytesVal); err != nil {
				return model.Value{}, err
			}
		}
		if id == 3614 || id == 3615 {
			if err := validateFullTextValue(ctx, id, argument.BytesVal); err != nil {
				return model.Value{}, err
			}
		}
		if id == 18 {
			if _, err := charValueByte(argument.StrVal); err != nil {
				return model.Value{}, err
			}
		}
		if id == 16 && argument.IntVal != 0 && argument.IntVal != 1 {
			return model.Value{}, &model.Error{Code: "42804", Message: "invalid boolean physical value"}
		}
		if id == 700 && !math.IsNaN(argument.FloatVal) && float64(float32(argument.FloatVal)) != argument.FloatVal {
			return model.Value{}, &model.Error{Code: "42804", Message: "float4 argument is not rounded to binary32"}
		}
	}
	if c.signature.Strict && hasNull {
		return model.NullVal(c.resultStorage), nil
	}
	if c.indeterminateText {
		return model.Value{}, sqlError("42P22", "text comparison requires a resolved C or POSIX collation")
	}
	budget.OutputBytes = min(budget.OutputBytes, int64(min(budget.MemoryBytes, uint64(math.MaxInt64))))
	result, err := c.kernel(ctx, arguments, budget)
	if err != nil {
		return model.Value{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return result, nil
}

func primitiveStorage(id model.TypeID) model.DataType {
	switch id {
	case 20, 21, 23, 28, 29, 790:
		return model.TypeInt64
	case 700, 701:
		return model.TypeFloat64
	case 16:
		return model.TypeBoolean
	case 17, 600, 601, 602, 603, 604, 628, 650, 718, 774, 829, 869, 1266, 1560, 1562, 3614, 3615, 4072, 5017:
		return model.TypeBytes
	case 1009, 2277:
		return model.TypeArray
	case 18, 19, 25, 194, 1042, 1043, 2275, 2278, 705:
		return model.TypeString
	case 1700:
		return model.TypeNumeric
	case 2950:
		return model.TypeUUID
	case 1082, 1083, 1114, 1184:
		return model.TypeTimestamp
	case 1186:
		return model.TypeInterval
	case 114, 3802:
		return model.TypeJSONB
	case 1247, 1249, 2249:
		return model.TypeComposite
	default:
		if IsOIDType(id) {
			return model.TypeInt64
		}
		if _, known := LookupRangeFamily(id); known {
			return model.TypeBytes
		}
		if info, known := LookupType(id); known && info.IsArray() {
			return model.TypeArray
		}
		return model.TypeAny
	}
}
func isInteger(id model.TypeID) bool { return id == 20 || id == 21 || id == 23 }
func isFloat(id model.TypeID) bool   { return id == 700 || id == 701 }

func sqlError(code, message string) error { return &model.Error{Code: code, Message: message} }

func outputBudget(size, budget int64) error {
	if size > budget || size > (1<<30)-5 {
		return sqlError("54000", "builtin output byte budget exceeded")
	}
	return nil
}
