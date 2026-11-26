package pgwire

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Standard PostgreSQL type OIDs.
const (
	oidBool             uint32 = 16
	oidBytea            uint32 = 17
	oidChar             uint32 = 18
	oidName             uint32 = 19
	oidInt8             uint32 = 20
	oidInt2             uint32 = 21
	oidInt4             uint32 = 23
	oidText             uint32 = 25
	oidOID              uint32 = 26
	oidJSON             uint32 = 114
	oidJSONArray        uint32 = 199
	oidFloat4           uint32 = 700
	oidFloat8           uint32 = 701
	oidUnknown          uint32 = 705
	oidBoolArray        uint32 = 1000
	oidByteaArray       uint32 = 1001
	oidInt2Array        uint32 = 1005
	oidInt4Array        uint32 = 1007
	oidTextArray        uint32 = 1009
	oidBPCharArray      uint32 = 1014
	oidVarcharArray     uint32 = 1015
	oidInt8Array        uint32 = 1016
	oidFloat4Array      uint32 = 1021
	oidFloat8Array      uint32 = 1022
	oidOIDArray         uint32 = 1028
	oidBPChar           uint32 = 1042
	oidVarchar          uint32 = 1043
	oidDate             uint32 = 1082
	oidTime             uint32 = 1083
	oidTimestamp        uint32 = 1114
	oidTimestampArray   uint32 = 1115
	oidDateArray        uint32 = 1182
	oidTimestamptz      uint32 = 1184
	oidTimestamptzArray uint32 = 1185
	oidInterval         uint32 = 1186
	oidNumericArray     uint32 = 1231
	oidTimeTZ           uint32 = 1266
	oidBit              uint32 = 1560
	oidVarbit           uint32 = 1562
	oidNumeric          uint32 = 1700
	oidVoid             uint32 = 2278
	oidUUID             uint32 = 2950
	oidUUIDArray        uint32 = 2951
	oidJSONB            uint32 = 3802
	oidJSONBArray       uint32 = 3807

	formatText   int16 = 0
	formatBinary int16 = 1

	postgresEpochMicros int64 = 946684800000000
)

var postgresEpoch = time.UnixMicro(postgresEpochMicros).UTC()

func typeSize(oid uint32) int16 {
	switch oid {
	case oidBool, oidChar:
		return 1
	case oidInt2:
		return 2
	case oidInt4, oidOID, oidFloat4, oidDate:
		return 4
	case oidInt8, oidFloat8, oidTime, oidTimestamp, oidTimestamptz:
		return 8
	case oidTimeTZ:
		return 12
	case oidInterval, oidUUID:
		return 16
	case oidName:
		return 64
	default:
		return -1
	}
}

func arrayElemOID(arrayOID uint32) (uint32, bool) {
	switch arrayOID {
	case oidBoolArray:
		return oidBool, true
	case oidByteaArray:
		return oidBytea, true
	case oidInt2Array:
		return oidInt2, true
	case oidInt4Array:
		return oidInt4, true
	case oidTextArray:
		return oidText, true
	case oidBPCharArray:
		return oidBPChar, true
	case oidVarcharArray:
		return oidVarchar, true
	case oidInt8Array:
		return oidInt8, true
	case oidFloat4Array:
		return oidFloat4, true
	case oidFloat8Array:
		return oidFloat8, true
	case oidOIDArray:
		return oidOID, true
	case oidNumericArray:
		return oidNumeric, true
	case oidDateArray:
		return oidDate, true
	case oidTimestampArray:
		return oidTimestamp, true
	case oidTimestamptzArray:
		return oidTimestamptz, true
	case oidUUIDArray:
		return oidUUID, true
	case oidJSONArray:
		return oidJSON, true
	case oidJSONBArray:
		return oidJSONB, true
	default:
		return 0, false
	}
}

func elemToArrayOID(elemOID uint32) uint32 {
	switch elemOID {
	case oidBool:
		return oidBoolArray
	case oidBytea:
		return oidByteaArray
	case oidInt2:
		return oidInt2Array
	case oidInt4:
		return oidInt4Array
	case oidText:
		return oidTextArray
	case oidBPChar:
		return oidBPCharArray
	case oidVarchar:
		return oidVarcharArray
	case oidInt8:
		return oidInt8Array
	case oidFloat4:
		return oidFloat4Array
	case oidFloat8:
		return oidFloat8Array
	case oidOID:
		return oidOIDArray
	case oidNumeric:
		return oidNumericArray
	case oidDate:
		return oidDateArray
	case oidTimestamp:
		return oidTimestampArray
	case oidTimestamptz:
		return oidTimestamptzArray
	case oidUUID:
		return oidUUIDArray
	case oidJSON:
		return oidJSONArray
	case oidJSONB:
		return oidJSONBArray
	default:
		return 0
	}
}

func typeNameToOID(name string, isArray bool) uint32 {
	base := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(base, "_") {
		isArray = true
		base = base[1:]
	}
	var oid uint32
	switch base {
	case "bool", "boolean":
		oid = oidBool
	case "bytea":
		oid = oidBytea
	case "char":
		oid = oidChar
	case "name":
		oid = oidName
	case "int8", "bigint":
		oid = oidInt8
	case "int2", "smallint":
		oid = oidInt2
	case "int4", "int", "integer":
		oid = oidInt4
	case "text":
		oid = oidText
	case "oid":
		oid = oidOID
	case "json":
		oid = oidJSON
	case "float4", "real":
		oid = oidFloat4
	case "float8", "double precision":
		oid = oidFloat8
	case "bpchar", "character":
		oid = oidBPChar
	case "varchar", "character varying":
		oid = oidVarchar
	case "date":
		oid = oidDate
	case "time", "time without time zone":
		oid = oidTime
	case "timetz", "time with time zone":
		oid = oidTimeTZ
	case "timestamp", "timestamp without time zone":
		oid = oidTimestamp
	case "timestamptz", "timestamp with time zone":
		oid = oidTimestamptz
	case "interval":
		oid = oidInterval
	case "numeric", "decimal":
		oid = oidNumeric
	case "uuid":
		oid = oidUUID
	case "jsonb":
		oid = oidJSONB
	default:
		return 0
	}
	if isArray {
		return elemToArrayOID(oid)
	}
	return oid
}

// decodeParam converts a wire-encoded parameter value into a driver.Value for
// SuperSQL execution. raw == nil represents SQL NULL.
func decodeParam(oid uint32, format int16, raw []byte) (driver.Value, error) {
	if raw == nil {
		return nil, nil
	}
	if format == formatText {
		return decodeTextParam(oid, raw)
	}
	if format != formatBinary {
		return nil, &wireError{
			Severity: "ERROR",
			Code:     "08P01",
			Message:  fmt.Sprintf("unsupported parameter format code: %d", format),
		}
	}
	return decodeBinaryParam(oid, raw)
}

func decodeTextParam(oid uint32, raw []byte) (driver.Value, error) {
	s := string(raw)
	switch oid {
	case oidBytea:
		b, err := decodeByteaText(s)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: err.Error()}
		}
		return b, nil
	case oidBool:
		b, err := parsePGBool(s)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: err.Error()}
		}
		return b, nil
	case oidInt2:
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 16)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type smallint: %q", s)}
		}
		return int64(v), nil
	case oidInt4:
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type integer: %q", s)}
		}
		return int64(v), nil
	case oidInt8:
		v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type bigint: %q", s)}
		}
		return v, nil
	case oidFloat4:
		v, err := parsePGFloat(s, 32)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type real: %q", s)}
		}
		return v, nil
	case oidFloat8:
		v, err := parsePGFloat(s, 64)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P02", Message: fmt.Sprintf("invalid input syntax for type double precision: %q", s)}
		}
		return v, nil
	default:
		// Leaving text parameters as string allows SuperSQL's binder to perform
		// exact context-sensitive type inference and typinput coercion (covering
		// date, timestamp, timestamptz, uuid, numeric, json, jsonb, arrays, and
		// untyped OID 0 parameters).
		return s, nil
	}
}

func decodeBinaryParam(oid uint32, raw []byte) (driver.Value, error) {
	if elemOID, isArr := arrayElemOID(oid); isArr {
		s, err := decodeBinaryArrayToText(elemOID, raw)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: err.Error()}
		}
		return s, nil
	}
	switch oid {
	case oidBool:
		if len(raw) != 1 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for bool: length %d", len(raw))}
		}
		return raw[0] != 0, nil
	case oidBytea:
		cp := make([]byte, len(raw))
		copy(cp, raw)
		return cp, nil
	case oidInt2:
		if len(raw) != 2 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for int2: length %d", len(raw))}
		}
		return int64(int16(binary.BigEndian.Uint16(raw))), nil
	case oidInt4:
		if len(raw) != 4 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for int4: length %d", len(raw))}
		}
		return int64(int32(binary.BigEndian.Uint32(raw))), nil
	case oidInt8:
		if len(raw) != 8 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for int8: length %d", len(raw))}
		}
		return int64(binary.BigEndian.Uint64(raw)), nil
	case oidOID:
		if len(raw) != 4 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for oid: length %d", len(raw))}
		}
		return int64(binary.BigEndian.Uint32(raw)), nil
	case oidFloat4:
		if len(raw) != 4 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for float4: length %d", len(raw))}
		}
		return float64(math.Float32frombits(binary.BigEndian.Uint32(raw))), nil
	case oidFloat8:
		if len(raw) != 8 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for float8: length %d", len(raw))}
		}
		return math.Float64frombits(binary.BigEndian.Uint64(raw)), nil
	case oidText, oidVarchar, oidBPChar, oidName, oidChar, oidJSON, oidUnknown, 0:
		return string(raw), nil
	case oidJSONB:
		if len(raw) < 1 || raw[0] != 1 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: "unsupported jsonb binary version"}
		}
		return string(raw[1:]), nil
	case oidUUID:
		if len(raw) != 16 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for uuid: length %d", len(raw))}
		}
		return formatUUIDBytes(raw), nil
	case oidDate:
		if len(raw) != 4 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for date: length %d", len(raw))}
		}
		days := int32(binary.BigEndian.Uint32(raw))
		switch days {
		case math.MaxInt32:
			return "infinity", nil
		case math.MinInt32:
			return "-infinity", nil
		default:
			t := postgresEpoch.AddDate(0, 0, int(days))
			return t.Format("2006-01-02"), nil
		}
	case oidTime:
		if len(raw) != 8 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for time: length %d", len(raw))}
		}
		micros := int64(binary.BigEndian.Uint64(raw))
		return formatPGTimeMicros(micros), nil
	case oidTimestamp:
		if len(raw) != 8 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for timestamp: length %d", len(raw))}
		}
		micros := int64(binary.BigEndian.Uint64(raw))
		switch micros {
		case math.MaxInt64:
			return "infinity", nil
		case math.MinInt64:
			return "-infinity", nil
		default:
			t := time.UnixMicro(micros + postgresEpochMicros).UTC()
			return formatPGTimestamp(t, false), nil
		}
	case oidTimestamptz:
		if len(raw) != 8 {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: fmt.Sprintf("incorrect binary data format for timestamptz: length %d", len(raw))}
		}
		micros := int64(binary.BigEndian.Uint64(raw))
		switch micros {
		case math.MaxInt64:
			return "infinity", nil
		case math.MinInt64:
			return "-infinity", nil
		default:
			return time.UnixMicro(micros + postgresEpochMicros).UTC(), nil
		}
	case oidNumeric:
		s, err := decodeBinaryNumeric(raw)
		if err != nil {
			return nil, &wireError{Severity: "ERROR", Code: "22P03", Message: err.Error()}
		}
		return s, nil
	default:
		return string(raw), nil
	}
}

// encodeResultValue encodes one driver.Value into PostgreSQL text (0) or binary (1)
// wire format for the column's OID. nil returns nil (encoded as -1 NULL on wire).
func encodeResultValue(oid uint32, format int16, val driver.Value) ([]byte, error) {
	if val == nil {
		return nil, nil
	}
	if format == formatBinary {
		return encodeBinaryValue(oid, val)
	}
	return encodeTextValue(oid, val)
}

func encodeTextValue(oid uint32, val driver.Value) ([]byte, error) {
	switch oid {
	case oidVoid:
		return []byte{}, nil
	case oidBool:
		b, err := coerceBool(val)
		if err != nil {
			return nil, err
		}
		if b {
			return []byte("t"), nil
		}
		return []byte("f"), nil
	case oidBytea:
		var raw []byte
		switch v := val.(type) {
		case []byte:
			raw = v
		case string:
			raw = []byte(v)
		default:
			return nil, fmt.Errorf("cannot encode %T as bytea", val)
		}
		out := make([]byte, 2+hex.EncodedLen(len(raw)))
		out[0] = '\\'
		out[1] = 'x'
		hex.Encode(out[2:], raw)
		return out, nil
	case oidInt2, oidInt4, oidInt8, oidOID:
		n, err := coerceInt64(val)
		if err != nil {
			return nil, err
		}
		return strconv.AppendInt(nil, n, 10), nil
	case oidFloat4:
		f, err := coerceFloat64(val)
		if err != nil {
			return nil, err
		}
		return []byte(formatPGFloat(f, 32)), nil
	case oidFloat8:
		f, err := coerceFloat64(val)
		if err != nil {
			return nil, err
		}
		return []byte(formatPGFloat(f, 64)), nil
	case oidDate:
		if s, ok := val.(string); ok {
			return []byte(s), nil
		}
		t, err := coerceTime(val)
		if err != nil {
			return nil, err
		}
		return []byte(t.UTC().Format("2006-01-02")), nil
	case oidTimestamp:
		if s, ok := val.(string); ok {
			return []byte(s), nil
		}
		t, err := coerceTime(val)
		if err != nil {
			return nil, err
		}
		return []byte(formatPGTimestamp(t.UTC(), false)), nil
	case oidTimestamptz:
		if s, ok := val.(string); ok {
			return []byte(s), nil
		}
		t, err := coerceTime(val)
		if err != nil {
			return nil, err
		}
		return []byte(formatPGTimestamp(t.UTC(), true)), nil
	default:
		switch v := val.(type) {
		case string:
			return []byte(v), nil
		case []byte:
			cp := make([]byte, len(v))
			copy(cp, v)
			return cp, nil
		case bool:
			if v {
				return []byte("t"), nil
			}
			return []byte("f"), nil
		case int64:
			return strconv.AppendInt(nil, v, 10), nil
		case float64:
			return []byte(formatPGFloat(v, 64)), nil
		case time.Time:
			return []byte(formatPGTimestamp(v.UTC(), true)), nil
		default:
			return []byte(fmt.Sprint(v)), nil
		}
	}
}

func encodeBinaryValue(oid uint32, val driver.Value) ([]byte, error) {
	if elemOID, isArr := arrayElemOID(oid); isArr {
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		return encodeBinaryArrayFromText(elemOID, s)
	}
	switch oid {
	case oidVoid:
		return []byte{}, nil
	case oidBool:
		b, err := coerceBool(val)
		if err != nil {
			return nil, err
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case oidBytea:
		switch v := val.(type) {
		case []byte:
			cp := make([]byte, len(v))
			copy(cp, v)
			return cp, nil
		case string:
			return decodeByteaText(v)
		default:
			return nil, fmt.Errorf("cannot encode %T as binary bytea", val)
		}
	case oidInt2:
		n, err := coerceInt64(val)
		if err != nil {
			return nil, err
		}
		var out [2]byte
		binary.BigEndian.PutUint16(out[:], uint16(int16(n)))
		return out[:], nil
	case oidInt4:
		n, err := coerceInt64(val)
		if err != nil {
			return nil, err
		}
		var out [4]byte
		binary.BigEndian.PutUint32(out[:], uint32(int32(n)))
		return out[:], nil
	case oidInt8:
		n, err := coerceInt64(val)
		if err != nil {
			return nil, err
		}
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], uint64(n))
		return out[:], nil
	case oidOID:
		n, err := coerceInt64(val)
		if err != nil {
			return nil, err
		}
		var out [4]byte
		binary.BigEndian.PutUint32(out[:], uint32(n))
		return out[:], nil
	case oidFloat4:
		f, err := coerceFloat64(val)
		if err != nil {
			return nil, err
		}
		var out [4]byte
		binary.BigEndian.PutUint32(out[:], math.Float32bits(float32(f)))
		return out[:], nil
	case oidFloat8:
		f, err := coerceFloat64(val)
		if err != nil {
			return nil, err
		}
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], math.Float64bits(f))
		return out[:], nil
	case oidText, oidVarchar, oidBPChar, oidName, oidChar, oidJSON, oidUnknown:
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		return []byte(s), nil
	case oidJSONB:
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 1+len(s))
		out[0] = 1 // PostgreSQL JSONB binary format version 1
		copy(out[1:], s)
		return out, nil
	case oidUUID:
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		return parseUUIDBytes(s)
	case oidDate:
		if s, ok := val.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "infinity":
				var out [4]byte
				binary.BigEndian.PutUint32(out[:], uint32(math.MaxInt32))
				return out[:], nil
			case "-infinity":
				var out [4]byte
				binary.BigEndian.PutUint32(out[:], 0x80000000)
				return out[:], nil
			}
		}
		t, err := coerceTime(val)
		if err != nil {
			return nil, err
		}
		days := daysSincePostgresEpoch(t)
		var out [4]byte
		binary.BigEndian.PutUint32(out[:], uint32(days))
		return out[:], nil
	case oidTime:
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		micros, err := parsePGTimeMicros(s)
		if err != nil {
			return nil, err
		}
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], uint64(micros))
		return out[:], nil
	case oidTimestamp, oidTimestamptz:
		if s, ok := val.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "infinity":
				var out [8]byte
				binary.BigEndian.PutUint64(out[:], uint64(math.MaxInt64))
				return out[:], nil
			case "-infinity":
				var out [8]byte
				binary.BigEndian.PutUint64(out[:], 0x8000000000000000)
				return out[:], nil
			}
		}
		t, err := coerceTime(val)
		if err != nil {
			return nil, err
		}
		micros := t.UTC().UnixMicro() - postgresEpochMicros
		var out [8]byte
		binary.BigEndian.PutUint64(out[:], uint64(micros))
		return out[:], nil
	case oidNumeric:
		s, err := coerceString(val)
		if err != nil {
			return nil, err
		}
		return encodeBinaryNumeric(s)
	default:
		// Fallback to text representation for exotic types.
		return encodeTextValue(oid, val)
	}
}

// --- Coercion & formatting helpers ---

func coerceBool(val driver.Value) (bool, error) {
	switch v := val.(type) {
	case bool:
		return v, nil
	case int64:
		return v != 0, nil
	case string:
		return parsePGBool(v)
	case []byte:
		return parsePGBool(string(v))
	default:
		return false, fmt.Errorf("cannot coerce %T to bool", val)
	}
}

func parsePGBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "y", "yes", "on", "1":
		return true, nil
	case "f", "false", "n", "no", "off", "0":
		return false, nil
	default:
		return false, fmt.Errorf("invalid input syntax for type boolean: %q", s)
	}
}

func coerceInt64(val driver.Value) (int64, error) {
	switch v := val.(type) {
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	case []byte:
		return strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
	default:
		return 0, fmt.Errorf("cannot coerce %T to int64", val)
	}
}

func coerceFloat64(val driver.Value) (float64, error) {
	switch v := val.(type) {
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case string:
		return parsePGFloat(v, 64)
	case []byte:
		return parsePGFloat(string(v), 64)
	default:
		return 0, fmt.Errorf("cannot coerce %T to float64", val)
	}
}

func parsePGFloat(s string, bitSize int) (float64, error) {
	trimmed := strings.TrimSpace(s)
	switch strings.ToLower(trimmed) {
	case "nan":
		return math.NaN(), nil
	case "infinity", "+infinity", "inf", "+inf":
		return math.Inf(1), nil
	case "-infinity", "-inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(trimmed, bitSize)
}

func formatPGFloat(f float64, bitSize int) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	default:
		return strconv.FormatFloat(f, 'g', -1, bitSize)
	}
}

func coerceString(val driver.Value) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return formatPGFloat(v, 64), nil
	case bool:
		if v {
			return "t", nil
		}
		return "f", nil
	case time.Time:
		return formatPGTimestamp(v.UTC(), true), nil
	default:
		return fmt.Sprint(v), nil
	}
}

func coerceTime(val driver.Value) (time.Time, error) {
	switch v := val.(type) {
	case time.Time:
		return v, nil
	case string:
		return parsePGTimeString(v)
	case []byte:
		return parsePGTimeString(string(v))
	default:
		return time.Time{}, fmt.Errorf("cannot coerce %T to time.Time", val)
	}
}

func parsePGTimeString(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp %q", s)
}

func formatPGTimestamp(t time.Time, withTZ bool) string {
	var base string
	if t.Nanosecond() == 0 {
		base = t.Format("2006-01-02 15:04:05")
	} else {
		base = t.Format("2006-01-02 15:04:05.999999")
	}
	if withTZ {
		return base + "+00"
	}
	return base
}

func gregorianToJDN(y, m, d int) int32 {
	a := (14 - m) / 12
	y2 := y + 4800 - a
	m2 := m + 12*a - 3
	return int32(d + (153*m2+2)/5 + 365*y2 + y2/4 - y2/100 + y2/400 - 32045)
}

func daysSincePostgresEpoch(t time.Time) int32 {
	y, m, d := t.UTC().Date()
	return gregorianToJDN(y, int(m), d) - gregorianToJDN(2000, 1, 1)
}

func formatPGTimeMicros(micros int64) string {
	if micros < 0 {
		micros = 0
	}
	h := micros / 3_600_000_000
	rem := micros % 3_600_000_000
	m := rem / 60_000_000
	rem %= 60_000_000
	sec := rem / 1_000_000
	us := rem % 1_000_000
	if us == 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	}
	frac := strings.TrimRight(fmt.Sprintf("%06d", us), "0")
	return fmt.Sprintf("%02d:%02d:%02d.%s", h, m, sec, frac)
}

func parsePGTimeMicros(s string) (int64, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	h, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	m, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, err
	}
	var sec, us int64
	if len(parts) == 3 {
		secPart := parts[2]
		if dot := strings.IndexByte(secPart, '.'); dot >= 0 {
			sec, err = strconv.ParseInt(secPart[:dot], 10, 64)
			if err != nil {
				return 0, err
			}
			frac := secPart[dot+1:]
			if len(frac) > 6 {
				frac = frac[:6]
			}
			for len(frac) < 6 {
				frac += "0"
			}
			us, err = strconv.ParseInt(frac, 10, 64)
			if err != nil {
				return 0, err
			}
		} else {
			sec, err = strconv.ParseInt(secPart, 10, 64)
			if err != nil {
				return 0, err
			}
		}
	}
	return h*3_600_000_000 + m*60_000_000 + sec*1_000_000 + us, nil
}

// --- Bytea text codec ---

func decodeByteaText(s string) ([]byte, error) {
	if strings.HasPrefix(s, `\x`) || strings.HasPrefix(s, `\X`) {
		out, err := hex.DecodeString(s[2:])
		if err != nil {
			return nil, fmt.Errorf("invalid hex bytea: %w", err)
		}
		return out, nil
	}
	// Legacy PostgreSQL escape format.
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		if i+1 >= len(s) {
			return nil, fmt.Errorf("invalid escape bytea: trailing backslash")
		}
		if s[i+1] == '\\' {
			out = append(out, '\\')
			i++
			continue
		}
		if i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && s[i+2] >= '0' && s[i+2] <= '7' && s[i+3] >= '0' && s[i+3] <= '7' {
			val := (s[i+1]-'0')*64 + (s[i+2]-'0')*8 + (s[i+3] - '0')
			out = append(out, val)
			i += 3
			continue
		}
		return nil, fmt.Errorf("invalid escape sequence in bytea")
	}
	return out, nil
}

// --- UUID codec ---

func formatUUIDBytes(raw []byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

func parseUUIDBytes(s string) ([]byte, error) {
	cleaned := strings.TrimSpace(s)
	cleaned = strings.TrimPrefix(cleaned, "{")
	cleaned = strings.TrimSuffix(cleaned, "}")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	if len(cleaned) != 32 {
		return nil, fmt.Errorf("invalid uuid %q", s)
	}
	raw, err := hex.DecodeString(cleaned)
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("invalid uuid %q", s)
	}
	return raw, nil
}

// --- PostgreSQL Binary Numeric Codec ---

const (
	numericSignPos  uint16 = 0x0000
	numericSignNeg  uint16 = 0x4000
	numericSignNaN  uint16 = 0xC000
	numericSignPInf uint16 = 0xD000
	numericSignNInf uint16 = 0xF000
)

func decodeBinaryNumeric(raw []byte) (string, error) {
	if len(raw) < 8 {
		return "", fmt.Errorf("numeric binary data too short: %d", len(raw))
	}
	ndigits := int(int16(binary.BigEndian.Uint16(raw[0:2])))
	weight := int(int16(binary.BigEndian.Uint16(raw[2:4])))
	sign := binary.BigEndian.Uint16(raw[4:6])
	dscale := int(binary.BigEndian.Uint16(raw[6:8]))
	if ndigits < 0 || len(raw) != 8+2*ndigits {
		return "", fmt.Errorf("invalid numeric binary length: ndigits=%d len=%d", ndigits, len(raw))
	}
	switch sign {
	case numericSignNaN:
		return "NaN", nil
	case numericSignPInf:
		return "Infinity", nil
	case numericSignNInf:
		return "-Infinity", nil
	case numericSignPos, numericSignNeg:
	default:
		return "", fmt.Errorf("invalid numeric sign: 0x%04x", sign)
	}
	if weight < -2000 || weight > 2000 {
		return "", fmt.Errorf("numeric weight out of range: %d", weight)
	}
	if dscale < 0 || dscale > 1000 {
		return "", fmt.Errorf("numeric display scale out of range: %d", dscale)
	}
	digits := make([]uint16, ndigits)
	for i := range ndigits {
		d := binary.BigEndian.Uint16(raw[8+2*i : 10+2*i])
		if d > 9999 {
			return "", fmt.Errorf("numeric base-10000 digit out of range: %d", d)
		}
		digits[i] = d
	}

	var b strings.Builder
	if sign == numericSignNeg {
		b.WriteByte('-')
	}
	// Integer part: digits at positions i where (weight - i) >= 0.
	if ndigits == 0 || weight < 0 {
		b.WriteByte('0')
	} else {
		for idx := 0; idx <= weight; idx++ {
			var d uint16
			if idx < ndigits {
				d = digits[idx]
			}
			if idx == 0 {
				b.WriteString(strconv.FormatUint(uint64(d), 10))
			} else {
				fmt.Fprintf(&b, "%04d", d)
			}
		}
	}
	// Fractional part: dscale digits after decimal point.
	if dscale > 0 {
		b.WriteByte('.')
		var frac strings.Builder
		fracGroups := (dscale + 3) / 4
		for g := 0; g < fracGroups; g++ {
			// Group g corresponds to 10000^(-(g+1)), i.e. index = weight + 1 + g.
			idx := weight + 1 + g
			var d uint16
			if idx >= 0 && idx < ndigits {
				d = digits[idx]
			}
			fmt.Fprintf(&frac, "%04d", d)
		}
		b.WriteString(frac.String()[:dscale])
	}
	return b.String(), nil
}

func encodeBinaryNumeric(s string) ([]byte, error) {
	trimmed := strings.TrimSpace(s)
	switch strings.ToLower(trimmed) {
	case "nan":
		var out [8]byte
		binary.BigEndian.PutUint16(out[4:6], numericSignNaN)
		return out[:], nil
	case "infinity", "+infinity", "inf", "+inf":
		var out [8]byte
		binary.BigEndian.PutUint16(out[4:6], numericSignPInf)
		return out[:], nil
	case "-infinity", "-inf":
		var out [8]byte
		binary.BigEndian.PutUint16(out[4:6], numericSignNInf)
		return out[:], nil
	}
	sign := numericSignPos
	if strings.HasPrefix(trimmed, "-") {
		sign = numericSignNeg
		trimmed = trimmed[1:]
	} else if strings.HasPrefix(trimmed, "+") {
		trimmed = trimmed[1:]
	}
	// Handle scientific notation if present.
	expShift := 0
	if idx := strings.IndexAny(trimmed, "eE"); idx >= 0 {
		e, err := strconv.Atoi(trimmed[idx+1:])
		if err != nil {
			return nil, fmt.Errorf("invalid numeric %q", s)
		}
		expShift = e
		trimmed = trimmed[:idx]
	}
	var intStr, fracStr string
	if dot := strings.IndexByte(trimmed, '.'); dot >= 0 {
		intStr = trimmed[:dot]
		fracStr = trimmed[dot+1:]
	} else {
		intStr = trimmed
	}
	// Apply scientific exponent shift into intStr / fracStr.
	if expShift != 0 {
		all := intStr + fracStr
		decPos := len(intStr) + expShift
		if decPos <= 0 {
			intStr = "0"
			fracStr = strings.Repeat("0", -decPos) + all
		} else if decPos >= len(all) {
			intStr = all + strings.Repeat("0", decPos-len(all))
			fracStr = ""
		} else {
			intStr = all[:decPos]
			fracStr = all[decPos:]
		}
	}
	dscale := len(fracStr)
	if intStr == "" {
		intStr = "0"
	}
	// Pad intStr on left to a multiple of 4 digits.
	if rem := len(intStr) % 4; rem != 0 {
		intStr = strings.Repeat("0", 4-rem) + intStr
	}
	// Pad fracStr on right to a multiple of 4 digits.
	if rem := len(fracStr) % 4; rem != 0 {
		fracStr = fracStr + strings.Repeat("0", 4-rem)
	}
	intGroups := len(intStr) / 4
	fracGroups := len(fracStr) / 4
	totalGroups := intGroups + fracGroups
	digits := make([]uint16, 0, totalGroups)
	for i := 0; i < len(intStr); i += 4 {
		v, err := strconv.ParseUint(intStr[i:i+4], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric %q", s)
		}
		digits = append(digits, uint16(v))
	}
	for i := 0; i < len(fracStr); i += 4 {
		v, err := strconv.ParseUint(fracStr[i:i+4], 10, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid numeric %q", s)
		}
		digits = append(digits, uint16(v))
	}
	weight := intGroups - 1
	// Strip leading zero groups.
	for len(digits) > 0 && digits[0] == 0 {
		digits = digits[1:]
		weight--
	}
	// Strip trailing zero groups.
	for len(digits) > 0 && digits[len(digits)-1] == 0 {
		digits = digits[:len(digits)-1]
	}
	if len(digits) == 0 {
		weight = 0
		sign = numericSignPos
	}
	out := make([]byte, 8+2*len(digits))
	binary.BigEndian.PutUint16(out[0:2], uint16(len(digits)))
	binary.BigEndian.PutUint16(out[2:4], uint16(int16(weight)))
	binary.BigEndian.PutUint16(out[4:6], sign)
	binary.BigEndian.PutUint16(out[6:8], uint16(dscale))
	for i, d := range digits {
		binary.BigEndian.PutUint16(out[8+2*i:10+2*i], d)
	}
	return out, nil
}

// --- PostgreSQL Binary Array Codec ---

func decodeBinaryArrayToText(expectedElemOID uint32, raw []byte) (string, error) {
	if len(raw) < 12 {
		return "", fmt.Errorf("binary array header too short: %d", len(raw))
	}
	ndim := int(int32(binary.BigEndian.Uint32(raw[0:4])))
	// hasNull := binary.BigEndian.Uint32(raw[4:8])
	elemOID := binary.BigEndian.Uint32(raw[8:12])
	if elemOID == 0 {
		elemOID = expectedElemOID
	}
	if expectedElemOID != 0 && elemOID != expectedElemOID {
		return "", fmt.Errorf("binary array element OID mismatch: got %d, want %d", elemOID, expectedElemOID)
	}
	if _, isNestedArr := arrayElemOID(elemOID); isNestedArr {
		return "", fmt.Errorf("nested binary array element OID is not allowed: %d", elemOID)
	}
	if ndim == 0 {
		return "{}", nil
	}
	if ndim < 0 || ndim > 16 {
		return "", fmt.Errorf("invalid binary array ndim: %d", ndim)
	}
	pos := 12
	if len(raw) < pos+ndim*8 {
		return "", fmt.Errorf("binary array dimensions truncated")
	}
	// Every binary array element consumes at least a 4-byte length prefix.
	maxElems := uint64(len(raw)-(pos+ndim*8)) / 4
	dims := make([]int, ndim)
	var totalElems uint64 = 1
	for i := range ndim {
		dimLen := int64(int32(binary.BigEndian.Uint32(raw[pos : pos+4])))
		pos += 8 // skip dimLen + lbound
		if dimLen <= 0 {
			return "", fmt.Errorf("invalid array dimension length: %d", dimLen)
		}
		if uint64(dimLen) > maxElems || totalElems > maxElems/uint64(dimLen) {
			return "", fmt.Errorf("binary array element count exceeds payload size")
		}
		dims[i] = int(dimLen)
		totalElems *= uint64(dimLen)
	}
	elems := make([]*string, int(totalElems))
	for i := range int(totalElems) {
		if pos+4 > len(raw) {
			return "", fmt.Errorf("binary array element length truncated")
		}
		elemLen := int(int32(binary.BigEndian.Uint32(raw[pos : pos+4])))
		pos += 4
		if elemLen == -1 {
			elems[i] = nil
			continue
		}
		if elemLen < 0 || pos+elemLen > len(raw) {
			return "", fmt.Errorf("binary array element payload truncated")
		}
		elemRaw := raw[pos : pos+elemLen]
		pos += elemLen
		val, err := decodeBinaryParam(elemOID, elemRaw)
		if err != nil {
			return "", err
		}
		textBytes, err := encodeTextValue(elemOID, val)
		if err != nil {
			return "", err
		}
		s := string(textBytes)
		elems[i] = &s
	}
	var b strings.Builder
	idx := 0
	writePGArrayDims(&b, dims, 0, elems, &idx)
	return b.String(), nil
}

func writePGArrayDims(b *strings.Builder, dims []int, level int, elems []*string, idx *int) {
	b.WriteByte('{')
	for i := 0; i < dims[level]; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if level == len(dims)-1 {
			e := elems[*idx]
			*idx++
			if e == nil {
				b.WriteString("NULL")
			} else {
				writePGArrayElement(b, *e)
			}
		} else {
			writePGArrayDims(b, dims, level+1, elems, idx)
		}
	}
	b.WriteByte('}')
}

func writePGArrayElement(b *strings.Builder, s string) {
	needsQuote := s == "" || strings.EqualFold(s, "null")
	if !needsQuote {
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '{', '}', ',', '"', '\\', ' ', '\t', '\n', '\r':
				needsQuote = true
			}
			if needsQuote {
				break
			}
		}
	}
	if !needsQuote {
		b.WriteString(s)
		return
	}
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
}

type pgArrayNode struct {
	isLeaf   bool
	isNull   bool
	val      string
	children []pgArrayNode
}

func encodeBinaryArrayFromText(elemOID uint32, text string) ([]byte, error) {
	s := strings.TrimSpace(text)
	// Strip optional dimension prefix like [1:3]=
	if strings.HasPrefix(s, "[") {
		if eq := strings.IndexByte(s, '='); eq >= 0 {
			s = s[eq+1:]
		}
	}
	if s == "{}" {
		var hdr [12]byte
		binary.BigEndian.PutUint32(hdr[0:4], 0) // ndim = 0
		binary.BigEndian.PutUint32(hdr[4:8], 0) // hasNull = 0
		binary.BigEndian.PutUint32(hdr[8:12], elemOID)
		return hdr[:], nil
	}
	pos := 0
	root, err := parsePGArrayLiteral(s, &pos)
	if err != nil {
		return nil, err
	}
	var dims []int
	cur := root
	for !cur.isLeaf {
		dims = append(dims, len(cur.children))
		if len(cur.children) == 0 {
			break
		}
		cur = cur.children[0]
	}
	var leaves []pgArrayNode
	var collect func(n pgArrayNode)
	collect = func(n pgArrayNode) {
		if n.isLeaf {
			leaves = append(leaves, n)
			return
		}
		for _, ch := range n.children {
			collect(ch)
		}
	}
	collect(root)

	hasNull := uint32(0)
	for _, lf := range leaves {
		if lf.isNull {
			hasNull = 1
			break
		}
	}
	out := make([]byte, 12+8*len(dims))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(dims)))
	binary.BigEndian.PutUint32(out[4:8], hasNull)
	binary.BigEndian.PutUint32(out[8:12], elemOID)
	for i, d := range dims {
		binary.BigEndian.PutUint32(out[12+8*i:16+8*i], uint32(d))
		binary.BigEndian.PutUint32(out[16+8*i:20+8*i], 1) // lbound = 1
	}
	var lenBuf [4]byte
	for _, lf := range leaves {
		if lf.isNull {
			binary.BigEndian.PutUint32(lenBuf[:], 0xFFFFFFFF)
			out = append(out, lenBuf[:]...)
			continue
		}
		// Decode element from its text form into a driver.Value, then encode binary.
		dv, err := decodeTextParam(elemOID, []byte(lf.val))
		if err != nil {
			return nil, err
		}
		enc, err := encodeBinaryValue(elemOID, dv)
		if err != nil {
			return nil, err
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(enc)))
		out = append(out, lenBuf[:]...)
		out = append(out, enc...)
	}
	return out, nil
}

func parsePGArrayLiteral(s string, pos *int) (pgArrayNode, error) {
	if *pos >= len(s) || s[*pos] != '{' {
		return pgArrayNode{}, fmt.Errorf("expected '{' in array literal %q", s)
	}
	*pos++
	node := pgArrayNode{}
	if *pos < len(s) && s[*pos] == '}' {
		*pos++
		return node, nil
	}
	for {
		if *pos >= len(s) {
			return pgArrayNode{}, fmt.Errorf("unterminated array literal %q", s)
		}
		if s[*pos] == '{' {
			child, err := parsePGArrayLiteral(s, pos)
			if err != nil {
				return pgArrayNode{}, err
			}
			node.children = append(node.children, child)
		} else if s[*pos] == '"' {
			*pos++
			var b strings.Builder
			for {
				if *pos >= len(s) {
					return pgArrayNode{}, fmt.Errorf("unterminated quoted element in array literal %q", s)
				}
				ch := s[*pos]
				*pos++
				if ch == '\\' {
					if *pos >= len(s) {
						return pgArrayNode{}, fmt.Errorf("unterminated escape in array literal %q", s)
					}
					b.WriteByte(s[*pos])
					*pos++
					continue
				}
				if ch == '"' {
					break
				}
				b.WriteByte(ch)
			}
			node.children = append(node.children, pgArrayNode{isLeaf: true, val: b.String()})
		} else {
			start := *pos
			for *pos < len(s) && s[*pos] != ',' && s[*pos] != '}' {
				*pos++
			}
			raw := s[start:*pos]
			if strings.EqualFold(raw, "NULL") {
				node.children = append(node.children, pgArrayNode{isLeaf: true, isNull: true})
			} else {
				node.children = append(node.children, pgArrayNode{isLeaf: true, val: raw})
			}
		}
		if *pos >= len(s) {
			return pgArrayNode{}, fmt.Errorf("unterminated array literal %q", s)
		}
		if s[*pos] == ',' {
			*pos++
			continue
		}
		if s[*pos] == '}' {
			*pos++
			return node, nil
		}
		return pgArrayNode{}, fmt.Errorf("unexpected character %q in array literal", s[*pos])
	}
}
