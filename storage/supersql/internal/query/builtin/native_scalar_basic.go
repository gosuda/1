package builtin

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type nativeSchemaRuntime interface {
	CurrentSchemas(context.Context, bool) ([]string, error)
	TemporarySchema(context.Context) (model.OID, bool, error)
	IsOtherTemporarySchema(context.Context, model.OID) (bool, error)
}

type nativeSequenceCatalogRuntime interface {
	SequenceParameters(context.Context, model.OID) (model.Value, error)
	SequenceData(context.Context, model.OID) (model.Value, error)
}

type nativeObjectDescriptionRuntime interface {
	DescribeObject(context.Context, model.OID, model.OID, int32, uint64) (string, error)
}

func compileNativeBasic(signature Signature) scalarKernel {
	switch signature.ID {
	case 89:
		return nativeConstantText("PostgreSQL 18.4 compatible SuperSQL embedded_pg18")
	case 710, 745, 746:
		return nativeConstantText(model.Principal{}.Name())
	case 861:
		return nativeCurrentDatabase
	case 810:
		return nativeClientEncoding
	case 1039:
		return nativeDatabaseEncoding
	case 1264, 1597, 2319:
		return compileNativeEncoding(signature.ID)
	case 209, 211, 223, 224, 458, 459, 768, 769, 770, 771, 1063, 1064, 1236, 1237, 1965, 1966, 6393, 6394:
		return compileNativeExtremum(signature.ID)
	case 655, 656, 657, 658, 659, 716, 717, 1638, 1639, 2160, 2161, 2163, 2164, 2166, 2174, 2175, 2177, 2178, 2180:
		return compileNativeComparison(signature.ID)
	case 766, 1219, 3546:
		return compileNativeCounter(signature.ID)
	case 636:
		return nativeIndexAMProperty
	case 676:
		return nativeIndexAMProgressPhase
	case 1318, 1367, 1372, 1375:
		return compileNativeBPCharLength(signature.ID)
	case 1402, 1403, 2854, 2855:
		return compileNativeSchema(signature.ID)
	case 1598, 1599, 6212, 6339, 6340:
		return compileNativeRandom(signature.ID)
	case 6341:
		return nativeRandomNumeric
	case 1713, 1714, 1717, 1813:
		return compileNativeConversion(signature.ID)
	case 1810, 1811:
		return compileNativeBitLength(signature.ID)
	case 1845, 1846, 1847:
		return nativeToASCII
	case 2288, 3166, 3334:
		return compileNativeSize(signature.ID)
	case 2417, 2501:
		return nativeRawSend
	case 2626, 3935, 3936:
		return compileNativeSleep(signature.ID)
	case 2903, 2904, 2905, 2906, 2907, 2908, 2909, 2910, 2911, 2912, 2913, 2914, 2915, 2916:
		return compileNativeTypmod(signature.ID)
	case 2922:
		return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) { return ownedText(ctx, budget.OutputBytes, arguments[0].StrVal) }
	case 3078, 6427:
		return compileNativeSequenceCatalog(signature.ID)
	case 3298, 3299:
		return func(context.Context, []model.Value, CallBudget) (model.Value, error) { return model.BoolVal(false), nil }
	case 3432, 6428, 6429, 6430, 6342:
		return compileNativeUUID(signature.ID)
	case 3537:
		return nativeDescribeObject
	case 3696:
		return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) { if err := ctx.Err(); err != nil { return model.Value{}, err }; return model.BoolVal(strings.HasPrefix(arguments[0].StrVal, arguments[1].StrVal)), nil }
	case 6099:
		return nativeConstantText(unicodeDataVersion)
	case 6210, 6211:
		return compileNativeTypeInput(signature.ID)
	case 6219:
		return nativeUnaryFloat(math.Erf)
	case 6220:
		return nativeUnaryFloat(math.Erfc)
	case 6240:
		return nativeSettingsFlags
	case 6364:
		return nativeCRC32(crc32.IEEETable)
	case 6365:
		return nativeCRC32(crc32.MakeTable(crc32.Castagnoli))
	case 6383:
		return nativeUnaryFloat(math.Gamma)
	case 6384:
		return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) { value, _ := math.Lgamma(arguments[0].FloatVal); return model.Float64Val(value), nil }
	case 6412:
		return nativeCaseFold
	default:
		return nil
	}
}

func nativeConstantText(text string) scalarKernel {
	return func(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) { return ownedText(ctx, budget.OutputBytes, text) }
}

func nativeCurrentDatabase(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) {
	if budget.Native.Database == nil {
		return model.Value{}, sqlError("55000", "database identity runtime is unavailable")
	}
	identity, err := budget.Native.Database.DatabaseIdentity(ctx)
	if err != nil {
		return model.Value{}, err
	}
	return ownedText(ctx, budget.OutputBytes, identity.Name)
}

func nativeClientEncoding(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) {
	if budget.Settings == nil {
		return model.Value{}, sqlError("55000", "settings runtime is unavailable")
	}
	return budget.Settings.CurrentSetting(ctx, "client_encoding", false)
}

func nativeDatabaseEncoding(ctx context.Context, _ []model.Value, budget CallBudget) (model.Value, error) {
	if budget.Native.Database == nil {
		return model.Value{}, sqlError("55000", "database identity runtime is unavailable")
	}
	identity, err := budget.Native.Database.DatabaseIdentity(ctx)
	if err != nil {
		return model.Value{}, err
	}
	name := nativeEncodingName(identity.Encoding)
	if name == "" {
		return model.Value{}, sqlError("XX000", "database encoding is invalid")
	}
	return ownedText(ctx, budget.OutputBytes, name)
}

var nativeEncodingNames = [...]string{
	"SQL_ASCII", "EUC_JP", "EUC_CN", "EUC_KR", "EUC_TW", "EUC_JIS_2004", "UTF8", "MULE_INTERNAL",
	"LATIN1", "LATIN2", "LATIN3", "LATIN4", "LATIN5", "LATIN6", "LATIN7", "LATIN8", "LATIN9", "LATIN10",
	"WIN1256", "WIN1258", "WIN866", "WIN874", "KOI8R", "WIN1251", "WIN1252", "ISO_8859_5", "ISO_8859_6", "ISO_8859_7", "ISO_8859_8",
	"WIN1250", "WIN1253", "WIN1254", "WIN1255", "WIN1257", "KOI8U", "SJIS", "BIG5", "GBK", "UHC", "GB18030", "JOHAB", "SHIFT_JIS_2004",
}

func nativeEncodingName(id int32) string {
	if id < 0 || int(id) >= len(nativeEncodingNames) {
		return ""
	}
	return nativeEncodingNames[id]
}

func nativeEncodingID(name string) int32 {
	canonical := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if canonical == "UNICODE" || canonical == "UTF_8" {
		canonical = "UTF8"
	}
	for id, candidate := range nativeEncodingNames {
		if canonical == candidate {
			return int32(id)
		}
	}
	return -1
}

func compileNativeEncoding(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		switch id {
		case 1264:
			return model.Int64Val(int64(nativeEncodingID(arguments[0].StrVal))), nil
		case 1597:
			return ownedText(ctx, budget.OutputBytes, nativeEncodingName(int32(arguments[0].IntVal)))
		default:
			encoding := int32(arguments[0].IntVal)
			if encoding < 0 || int(encoding) >= len(nativeEncodingNames) {
				return model.Value{}, sqlError("22023", "invalid encoding number")
			}
			if encoding == 4 || encoding == 7 || encoding == 38 || encoding == 41 {
				return model.Int64Val(4), nil
			}
			if encoding >= 1 && encoding <= 6 || encoding == 39 {
				return model.Int64Val(3), nil
			}
			if encoding == 35 || encoding == 36 || encoding == 37 || encoding == 40 {
				return model.Int64Val(2), nil
			}
			return model.Int64Val(1), nil
		}
	}
}

func compileNativeExtremum(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		order := 0
		switch id {
		case 209, 211, 223, 224:
			order = compareFloat(arguments[0].FloatVal, arguments[1].FloatVal)
		case 458, 459, 1063, 1064:
			left, right := arguments[0].StrVal, arguments[1].StrVal
			if id == 1063 || id == 1064 {
				left, right = strings.TrimRight(left, " "), strings.TrimRight(right, " ")
			}
			var err error
			order, err = compareText(ctx, left, right)
			if err != nil { return model.Value{}, err }
		case 6393, 6394:
			var err error
			order, err = compareBytes(ctx, arguments[0].BytesVal, arguments[1].BytesVal)
			if err != nil { return model.Value{}, err }
		default:
			order = intCompare(arguments[0].IntVal, arguments[1].IntVal)
		}
		larger := id == 209 || id == 223 || id == 458 || id == 768 || id == 770 || id == 1063 || id == 1236 || id == 1965 || id == 6375 || id == 6393
		selected := arguments[0]
		if larger && order < 0 || !larger && order > 0 {
			selected = arguments[1]
		}
		limit := uint64(max(budget.OutputBytes, 0))
		owned, _, err := model.CopyValue(ctx, selected, limit)
		if err != nil {
			return model.Value{}, &model.Error{Code: "54000", Message: "scalar result exceeds output budget", Cause: err}
		}
		return owned, nil
	}
}

func compileNativeComparison(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		order := 0
		switch id {
		case 655, 656, 657, 658, 659:
			var err error
			order, err = compareText(ctx, arguments[0].StrVal, arguments[1].StrVal)
			if err != nil { return model.Value{}, err }
		case 2160, 2161, 2163, 2164, 2166:
			var err error
			order, err = compareBytes(ctx, []byte(arguments[0].StrVal), []byte(arguments[1].StrVal))
			if err != nil { return model.Value{}, err }
		case 2174, 2175, 2177, 2178, 2180:
			var err error
			order, err = compareBytes(ctx, []byte(strings.TrimRight(arguments[0].StrVal, " ")), []byte(strings.TrimRight(arguments[1].StrVal, " ")))
			if err != nil { return model.Value{}, err }
		default:
			order = intCompare(arguments[0].IntVal, arguments[1].IntVal)
		}
		switch id {
		case 655, 716, 2160, 2174:
			return model.BoolVal(order < 0), nil
		case 656, 717, 2161, 2175:
			return model.BoolVal(order <= 0), nil
		case 657, 1638, 2164, 2178:
			return model.BoolVal(order > 0), nil
		case 658, 1639, 2163, 2177:
			return model.BoolVal(order >= 0), nil
		case 659:
			return model.BoolVal(order != 0), nil
		default:
			return model.Int64Val(int64(order)), nil
		}
	}
}

func intCompare(left, right int64) int {
	if left < right { return -1 }
	if left > right { return 1 }
	return 0
}

func compileNativeCounter(id model.FunctionID) scalarKernel {
	return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		value := arguments[0].IntVal
		decrement := id == 3546
		if decrement {
			if value == math.MinInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }
			value--
		} else {
			if id == 766 && value == math.MaxInt32 { return model.Value{}, sqlError("22003", "integer out of range") }
			if value == math.MaxInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }
			value++
		}
		return model.Int64Val(value), nil
	}
}

func nativeIndexAMProperty(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
	if arguments[0].IntVal != 403 {
		return model.NullVal(model.TypeBoolean), nil
	}
	switch strings.ToLower(arguments[1].StrVal) {
	case "can_order", "can_unique", "can_multi_col", "can_exclude", "can_include":
		return model.BoolVal(true), nil
	case "clusterable", "index_scan", "bitmap_scan", "backward_scan":
		return model.BoolVal(true), nil
	case "can_parallel", "use_maintenance_work_mem", "summarizing":
		return model.BoolVal(false), nil
	default:
		return model.NullVal(model.TypeBoolean), nil
	}
}

func nativeIndexAMProgressPhase(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if arguments[0].IntVal != 403 {
		return model.NullVal(model.TypeString), nil
	}
	phases := [...]string{"initializing", "waiting for writers before build", "building index", "waiting for writers before validation", "index validation: scanning index", "index validation: sorting tuples", "index validation: scanning table", "waiting for old snapshots", "waiting for readers before marking dead", "waiting for readers before dropping"}
	phase := arguments[1].IntVal
	if phase < 0 || phase >= int64(len(phases)) {
		return model.NullVal(model.TypeString), nil
	}
	return ownedText(ctx, budget.OutputBytes, phases[phase])
}

func compileNativeBPCharLength(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		text := strings.TrimRight(arguments[0].StrVal, " ")
		if id == 1375 {
			return model.Int64Val(int64(len(text))), nil
		}
		if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
			return model.Value{}, sqlError("22021", "invalid UTF8 text datum")
		}

		if err := ctx.Err(); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(utf8.RuneCountInString(text))), nil
	}
}
func nativeRandomNumeric(ctx context.Context, arguments []model.Value, budget CallBudget) (result model.Value, err error) {
	if budget.Native.Random == nil { return model.Value{}, sqlError("55000", "session random runtime is unavailable") }
	work, err := budget.NewWorkspace(ctx)
	if err != nil { return model.Value{}, numericAllocationError(err) }
	defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	lower, err := parseNumeric(ctx, work, arguments[0].StrVal); if err != nil { return model.Value{}, err }
	upper, err := parseNumeric(ctx, work, arguments[1].StrVal); if err != nil { return model.Value{}, err }
	order, err := numericCompare(ctx, lower, upper); if err != nil { return model.Value{}, err }
	if order > 0 { return model.Value{}, sqlError("22023", "lower bound must be less than or equal to upper bound") }
	bits, err := budget.Native.Random.RandomUint64(ctx); if err != nil { return model.Value{}, err }
	fractionText := strconv.FormatFloat(float64(bits>>11)*(1.0/(1<<53)), 'f', 17, 64)
	fraction, err := parseNumeric(ctx, work, fractionText); if err != nil { return model.Value{}, err }
	span, err := numericArithmetic(ctx, work, upper, lower, opMi); if err != nil { return model.Value{}, err }
	span, err = numericArithmetic(ctx, work, span, fraction, opMul); if err != nil { return model.Value{}, err }
	value, err := numericArithmetic(ctx, work, lower, span, opPl); if err != nil { return model.Value{}, err }
	return numericValue(ctx, value, budget.OutputLimit(work))
}

func compileNativeSchema(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		runtime, ok := budget.Relations.(nativeSchemaRuntime)
		if !ok { return model.Value{}, sqlError("55000", "schema runtime is unavailable") }
		switch id {
		case 1402:
			names, err := runtime.CurrentSchemas(ctx, false)
			if err != nil { return model.Value{}, err }
			if len(names) == 0 { return model.NullVal(model.TypeString), nil }
			return ownedText(ctx, budget.OutputBytes, names[0])
		case 1403:
			names, err := runtime.CurrentSchemas(ctx, arguments[0].IntVal != 0)
			if err != nil { return model.Value{}, err }
			values := make([]model.Value, len(names))
			for i, name := range names { values[i] = model.StringVal(name) }
			value := model.Value{Type: model.TypeArray, ArrayVal: values}
			if len(values) != 0 { value.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}} }
			owned, _, err := model.CopyValue(ctx, value, uint64(max(budget.OutputBytes, 0)))
			if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "schema array exceeds output budget", Cause: err} }
			return owned, nil
		case 2854:
			oid, _, err := runtime.TemporarySchema(ctx)
			return model.Int64Val(int64(oid)), err
		default:
			other, err := runtime.IsOtherTemporarySchema(ctx, model.OID(arguments[0].IntVal))
			return model.BoolVal(other), err
		}
	}
}

func compileNativeRandom(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if budget.Native.Random == nil { return model.Value{}, sqlError("55000", "session random runtime is unavailable") }
		if id == 1599 {
			seed := arguments[0].FloatVal
			if math.IsNaN(seed) || seed < -1 || seed > 1 { return model.Value{}, sqlError("22023", "setseed parameter must be between -1 and 1") }
			if err := budget.Native.Random.SeedRandom(ctx, seed); err != nil { return model.Value{}, err }
			return model.StringVal(""), nil
		}
		next := func() (uint64, error) { return budget.Native.Random.RandomUint64(ctx) }
		if id == 1598 {
			bits, err := next(); if err != nil { return model.Value{}, err }
			return model.Float64Val(float64(bits>>11) * (1.0 / (1 << 53))), nil
		}
		if id == 6212 {
			first, err := next(); if err != nil { return model.Value{}, err }
			second, err := next(); if err != nil { return model.Value{}, err }
			u1 := (float64(first>>11) + 1) / ((1 << 53) + 1)
			u2 := float64(second>>11) * (1.0 / (1 << 53))
			return model.Float64Val(arguments[0].FloatVal + arguments[1].FloatVal*math.Sqrt(-2*math.Log(u1))*math.Cos(2*math.Pi*u2)), nil
		}
		value, err := randomInt64Range(next, arguments[0].IntVal, arguments[1].IntVal)
		if err != nil { return model.Value{}, err }
		return model.Int64Val(value), nil
	}
}

func randomInt64Range(next func() (uint64, error), lower, upper int64) (int64, error) {
	if lower > upper { return 0, sqlError("22023", "lower bound must be less than or equal to upper bound") }
	width := uint64(upper) - uint64(lower) + 1
	if width == 0 {
		bits, err := next(); return int64(bits), err
	}
	threshold := -width % width
	for {
		bits, err := next(); if err != nil { return 0, err }
		if bits >= threshold { return int64(uint64(lower) + bits%width), nil }
	}
}

func compileNativeBitLength(id model.FunctionID) scalarKernel {
	return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		length := len(arguments[0].BytesVal)
		if id == 1811 { length = len(arguments[0].StrVal) }
		if length > math.MaxInt32/8 { return model.Value{}, sqlError("22003", "integer out of range") }
		return model.Int64Val(int64(length * 8)), nil
	}
}

func nativeRawSend(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if arguments[0].Type == model.TypeBytes { return ownedBytes(ctx, budget.OutputBytes, arguments[0].BytesVal) }
	return ownedBytes(ctx, budget.OutputBytes, []byte(arguments[0].StrVal))
}

func nativeToASCII(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if len(arguments) > 1 {
		encoding := int32(arguments[1].IntVal)
		if arguments[1].Type == model.TypeString { encoding = nativeEncodingID(arguments[1].StrVal) }
		if encoding != 8 && encoding != 9 && encoding != 10 && encoding != 11 && encoding != 12 { return model.Value{}, sqlError("0A000", "encoding conversion from this source is not supported by to_ascii") }
	}
	var output strings.Builder
	for _, r := range arguments[0].StrVal {
		if r < utf8.RuneSelf { output.WriteRune(r); continue }
		switch r {
		case 'À','Á','Â','Ã','Ä','Å','à','á','â','ã','ä','å': output.WriteByte('a')
		case 'Ç','ç': output.WriteByte('c')
		case 'È','É','Ê','Ë','è','é','ê','ë': output.WriteByte('e')
		case 'Ì','Í','Î','Ï','ì','í','î','ï': output.WriteByte('i')
		case 'Ñ','ñ': output.WriteByte('n')
		case 'Ò','Ó','Ô','Õ','Ö','Ø','ò','ó','ô','õ','ö','ø': output.WriteByte('o')
		case 'Ù','Ú','Û','Ü','ù','ú','û','ü': output.WriteByte('u')
		case 'Ý','ý','ÿ': output.WriteByte('y')
		case 'Æ','æ': output.WriteString("ae")
		case 'Œ','œ': output.WriteString("oe")
		case 'ß': output.WriteString("ss")
		default: output.WriteRune(r)
		}
	}
	return ownedText(ctx, budget.OutputBytes, output.String())
}

func compileNativeSize(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if id == 3334 {
			value, err := parsePrettyBytes(arguments[0].StrVal)
			if err != nil { return model.Value{}, err }
			return model.Int64Val(value), nil
		}
		value := float64(arguments[0].IntVal)
		if id == 3166 {
			work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
			defer work.Close()
			parsed, err := parseNumeric(ctx, work, arguments[0].StrVal); if err != nil { return model.Value{}, err }
			text, err := numericText(ctx, parsed, 256); if err != nil { return model.Value{}, err }
			value, err = strconv.ParseFloat(text, 64); if err != nil { return model.Value{}, sqlError("22003", "numeric value is out of range") }
		}
		return ownedText(ctx, budget.OutputBytes, formatPrettyBytes(value))
	}
}

func formatPrettyBytes(value float64) string {
	units := [...]string{"bytes", "kB", "MB", "GB", "TB", "PB"}
	absolute := math.Abs(value)
	unit := 0
	for unit+1 < len(units) && absolute >= 10*1024 { value /= 1024; absolute /= 1024; unit++ }
	if unit == 0 { return strconv.FormatInt(int64(value), 10) + " bytes" }
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64) + " " + units[unit]
}

func parsePrettyBytes(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	position := 0
	for position < len(trimmed) && (trimmed[position] == '+' || trimmed[position] == '-' || trimmed[position] == '.' || trimmed[position] >= '0' && trimmed[position] <= '9') { position++ }
	if position == 0 { return 0, sqlError("22023", "invalid size") }
	value, err := strconv.ParseFloat(trimmed[:position], 64); if err != nil { return 0, sqlError("22023", "invalid size") }
	unit := strings.ToLower(strings.TrimSpace(trimmed[position:]))
	factor := float64(1)
	switch unit {
	case "", "b", "byte", "bytes":
	case "kb", "kib": factor = 1 << 10
	case "mb", "mib": factor = 1 << 20
	case "gb", "gib": factor = 1 << 30
	case "tb", "tib": factor = 1 << 40
	case "pb", "pib": factor = 1 << 50
	default: return 0, sqlError("22023", "invalid size unit")
	}
	value *= factor
	if math.IsNaN(value) || math.IsInf(value, 0) || value < math.MinInt64 || value > math.MaxInt64 { return 0, sqlError("22003", "size is out of range") }
	return int64(math.Round(value)), nil
}

func compileNativeSleep(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		seconds := arguments[0].FloatVal
		if id == 3935 {
			interval := arguments[0].IntervalVal
			seconds = float64(interval.Microseconds)/1e6 + float64(interval.Days)*86400 + float64(interval.Months)*30*86400
		} else if id == 3936 {
			if budget.Temporal == nil { return model.Value{}, sqlError("55000", "temporal runtime is unavailable") }
			now, err := budget.Temporal.ClockTimestamp(ctx); if err != nil { return model.Value{}, err }
			seconds = float64(arguments[0].IntVal-int64(now)) / 1e6
		}
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) { return model.Value{}, sqlError("22023", "sleep duration must be finite") }
		if seconds <= 0 { return model.StringVal(""), nil }
		if seconds > float64(math.MaxInt64)/float64(time.Second) { return model.Value{}, sqlError("22023", "sleep duration is too large") }
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select { case <-ctx.Done(): return model.Value{}, ctx.Err(); case <-timer.C: return model.StringVal(""), nil }
	}
}

func nativeCRC32(table *crc32.Table) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil { return model.Value{}, err }
		return model.Int64Val(int64(crc32.Checksum(arguments[0].BytesVal, table))), nil
	}
}

func nativeUnaryFloat(operation func(float64) float64) scalarKernel {
	return func(_ context.Context, arguments []model.Value, _ CallBudget) (model.Value, error) { return model.Float64Val(operation(arguments[0].FloatVal)), nil }
}

func nativeCaseFold(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if !utf8.ValidString(arguments[0].StrVal) || strings.IndexByte(arguments[0].StrVal, 0) >= 0 { return model.Value{}, sqlError("22021", "invalid UTF8 text datum") }
	var output strings.Builder
	for _, r := range arguments[0].StrVal {
		if err := ctx.Err(); err != nil { return model.Value{}, err }
		switch r { case 'ß', 'ẞ': output.WriteString("ss"); case 'ς': output.WriteRune('σ'); default: output.WriteRune(unicode.ToLower(r)) }
	}
	return ownedText(ctx, budget.OutputBytes, output.String())
}

func compileNativeSequenceCatalog(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		runtime, ok := budget.Native.Database.(nativeSequenceCatalogRuntime)
		if !ok { return model.Value{}, sqlError("55000", "sequence catalog runtime is unavailable") }
		if id == 3078 { return runtime.SequenceParameters(ctx, model.OID(arguments[0].IntVal)) }
		return runtime.SequenceData(ctx, model.OID(arguments[0].IntVal))
	}
}

func nativeDescribeObject(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	runtime, ok := budget.Relations.(nativeObjectDescriptionRuntime)
	if !ok { return model.Value{}, sqlError("55000", "object description runtime is unavailable") }
	text, err := runtime.DescribeObject(ctx, model.OID(arguments[0].IntVal), model.OID(arguments[1].IntVal), int32(arguments[2].IntVal), uint64(max(budget.OutputBytes, 0)))
	if err != nil { return model.Value{}, err }
	return ownedText(ctx, budget.OutputBytes, text)
}

func compileNativeTypeInput(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if budget.Native.TypeInput == nil { return model.Value{}, sqlError("55000", "type input runtime is unavailable") }
		failure, err := budget.Native.TypeInput.ValidateTypeInput(ctx, arguments[0].StrVal, arguments[1].StrVal, budget.OutputBytes, budget.MemoryBytes)
		if err != nil { return model.Value{}, err }
		if id == 6210 { return model.BoolVal(failure == nil), nil }
		if failure == nil { return model.NullVal(model.TypeComposite), nil }
		detail, hint := model.NullVal(model.TypeString), model.NullVal(model.TypeString)
		if failure.Detail != "" { detail = model.StringVal(failure.Detail) }
		if failure.Hint != "" { hint = model.StringVal(failure.Hint) }
		fields := []model.Value{model.StringVal(failure.Message), detail, hint, model.StringVal(failure.Code)}
		value := model.Value{Type: model.TypeComposite, ArrayVal: fields}
		owned, _, copyErr := model.CopyValue(ctx, value, uint64(max(budget.OutputBytes, 0)))
		if copyErr != nil { return model.Value{}, &model.Error{Code: "54000", Message: "input error record exceeds output budget", Cause: copyErr} }
		return owned, nil
	}
}

func nativeSettingsFlags(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
	if budget.Settings == nil { return model.Value{}, sqlError("55000", "settings runtime is unavailable") }
	if _, err := budget.Settings.CurrentSetting(ctx, arguments[0].StrVal, false); err != nil { return model.Value{}, err }
	return model.Value{Type: model.TypeArray}, nil
}

func compileNativeTypmod(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if id%2 == 0 {
			modifier := arguments[0].IntVal
			text := ""
			if modifier >= 0 {
				value := modifier
				if id == 2914 || id == 2916 { value -= 4 }
				text = "(" + strconv.FormatInt(value, 10) + ")"
			}
			return ownedText(ctx, budget.OutputBytes, text)
		}
		array := arguments[0]
		if array.Type != model.TypeArray || len(array.Dimensions) != 1 || len(array.ArrayVal) == 0 || len(array.ArrayVal) > 2 { return model.Value{}, sqlError("22023", "invalid type modifier") }
		last := array.ArrayVal[len(array.ArrayVal)-1]
		value, err := strconv.ParseInt(last.StrVal, 10, 32)
		if err != nil { return model.Value{}, sqlError("22023", "invalid type modifier") }
		if id == 2913 || id == 2915 {
			if len(array.ArrayVal) != 1 || value < 1 { return model.Value{}, sqlError("22023", "length for type must be at least 1") }
			return model.Int64Val(value + 4), nil
		}
		if value < 0 || value > 6 { return model.Value{}, sqlError("22023", "precision must be between 0 and 6") }
		return model.Int64Val(value), nil
	}
}

func compileNativeConversion(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		source, target := int32(6), int32(6)
		input := arguments[0].BytesVal
		if id == 1717 { input = []byte(arguments[0].StrVal); target = nativeEncodingID(arguments[1].StrVal) }
		if id == 1713 || id == 1714 { source = nativeEncodingID(arguments[1].StrVal) }
		if id == 1813 { source, target = nativeEncodingID(arguments[1].StrVal), nativeEncodingID(arguments[2].StrVal) }
		text, err := decodeNativeEncoding(input, source)
		if err != nil { return model.Value{}, err }
		if id == 1713 { return model.Int64Val(int64(utf8.RuneCountInString(text))), nil }
		if id == 1714 { return ownedText(ctx, budget.OutputBytes, text) }
		output, err := encodeNativeEncoding(text, target)
		if err != nil { return model.Value{}, err }
		return ownedBytes(ctx, budget.OutputBytes, output)
	}
}

// unsupportedEncodingConversion reports the encoding_conversion profile
// exclusion for a source or target encoding the builtin conversion kernels do
// not implement.
func unsupportedEncodingConversion() error {
	return &model.Error{Code: "0A000", Message: "encoding conversion is unavailable", FeatureID: featureEncodingConversion}
}

func decodeNativeEncoding(input []byte, encoding int32) (string, error) {
	switch encoding {
	case 0:
		return string(input), nil
	case 6:
		if !utf8.Valid(input) { return "", sqlError("22021", "invalid UTF8 byte sequence") }
		return string(input), nil
	case 8:
		runes := make([]rune, len(input)); for i, value := range input { runes[i] = rune(value) }; return string(runes), nil
	case 24:
		runes := make([]rune, len(input)); for i, value := range input { if value >= 0x80 && value <= 0x9f { runes[i] = win1252Runes[value-0x80] } else { runes[i] = rune(value) } }; return string(runes), nil
	default:
		return "", unsupportedEncodingConversion()
	}
}

func encodeNativeEncoding(text string, encoding int32) ([]byte, error) {
	if !utf8.ValidString(text) { return nil, sqlError("22021", "invalid UTF8 text datum") }
	switch encoding {
	case 0, 6:
		return []byte(text), nil
	case 8:
		output := make([]byte, 0, len(text)); for _, r := range text { if r > 255 { return nil, sqlError("22P05", "character has no equivalent in LATIN1") }; output = append(output, byte(r)) }; return output, nil
	case 24:
		output := make([]byte, 0, len(text)); for _, r := range text { if r <= 255 && (r < 0x80 || r >= 0xa0) { output = append(output, byte(r)); continue }; found := false; for i, candidate := range win1252Runes { if r == candidate { output = append(output, byte(i+0x80)); found = true; break } }; if !found { return nil, sqlError("22P05", "character has no equivalent in WIN1252") } }; return output, nil
	default:
		return nil, unsupportedEncodingConversion()
	}
}

var win1252Runes = [...]rune{'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f, 0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ'}

func compileNativeUUID(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (model.Value, error) {
		if id == 6342 {
			data, err := parseUUIDText(ctx, arguments[0].StrVal); if err != nil { return model.Value{}, err }
			if data[6]>>4 != 7 { return model.NullVal(model.TypeTimestamp), nil }
			milliseconds := int64(binary.BigEndian.Uint64(data[:8]) >> 16)
			return timestampValue((milliseconds-946684800000)*1000), nil
		}
		var data [16]byte
		if id == 3432 || id == 6428 {
			if _, err := cryptorand.Read(data[:]); err != nil { return model.Value{}, err }
			data[6] = data[6]&0x0f | 0x40
		} else {
			if budget.Temporal == nil { return model.Value{}, sqlError("55000", "temporal runtime is unavailable") }
			now, err := budget.Temporal.ClockTimestamp(ctx); if err != nil { return model.Value{}, err }
			value := int64(now)
			if id == 6430 { shifted, err := timestampAddInterval(value, arguments[0].IntervalVal); if err != nil { return model.Value{}, err }; value = shifted.IntVal }
			milliseconds := uint64(value/1000 + 946684800000)
			binary.BigEndian.PutUint64(data[:8], milliseconds<<16)
			if _, err := cryptorand.Read(data[6:]); err != nil { return model.Value{}, err }
			data[6] = data[6]&0x0f | 0x70
		}
		data[8] = data[8]&0x3f | 0x80
		return ownedText(ctx, budget.OutputBytes, formatUUID(data))
	}
}

