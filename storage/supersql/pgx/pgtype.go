package pgx

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Text represents a nullable PostgreSQL text value.
type Text struct {
	String string
	Valid  bool
}

// Scan implements sql.Scanner.
func (t *Text) Scan(src any) error {
	if src == nil {
		*t = Text{}
		return nil
	}
	switch v := src.(type) {
	case string:
		*t = Text{String: v, Valid: true}
		return nil
	case []byte:
		*t = Text{String: string(v), Valid: true}
		return nil
	default:
		*t = Text{String: fmt.Sprint(v), Valid: true}
		return nil
	}
}

// Value implements driver.Valuer.
func (t Text) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return t.String, nil
}

// Int2 represents a nullable PostgreSQL smallint (int2) value.
type Int2 struct {
	Int16 int16
	Valid bool
}

// Scan implements sql.Scanner.
func (n *Int2) Scan(src any) error {
	if src == nil {
		*n = Int2{}
		return nil
	}
	v, err := toInt64(src)
	if err != nil {
		return err
	}
	if v < math.MinInt16 || v > math.MaxInt16 {
		return fmt.Errorf("pgx: value %d overflows int16", v)
	}
	*n = Int2{Int16: int16(v), Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (n Int2) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return int64(n.Int16), nil
}

// Int4 represents a nullable PostgreSQL integer (int4) value.
type Int4 struct {
	Int32 int32
	Valid bool
}

// Scan implements sql.Scanner.
func (n *Int4) Scan(src any) error {
	if src == nil {
		*n = Int4{}
		return nil
	}
	v, err := toInt64(src)
	if err != nil {
		return err
	}
	if v < math.MinInt32 || v > math.MaxInt32 {
		return fmt.Errorf("pgx: value %d overflows int32", v)
	}
	*n = Int4{Int32: int32(v), Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (n Int4) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return int64(n.Int32), nil
}

// Int8 represents a nullable PostgreSQL bigint (int8) value.
type Int8 struct {
	Int64 int64
	Valid bool
}

// Scan implements sql.Scanner.
func (n *Int8) Scan(src any) error {
	if src == nil {
		*n = Int8{}
		return nil
	}
	v, err := toInt64(src)
	if err != nil {
		return err
	}
	*n = Int8{Int64: v, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (n Int8) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.Int64, nil
}

// Float4 represents a nullable PostgreSQL real (float4) value.
type Float4 struct {
	Float32 float32
	Valid   bool
}

// Scan implements sql.Scanner.
func (f *Float4) Scan(src any) error {
	if src == nil {
		*f = Float4{}
		return nil
	}
	v, err := toFloat64(src)
	if err != nil {
		return err
	}
	*f = Float4{Float32: float32(v), Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (f Float4) Value() (driver.Value, error) {
	if !f.Valid {
		return nil, nil
	}
	return float64(f.Float32), nil
}

// Float8 represents a nullable PostgreSQL double precision (float8) value.
type Float8 struct {
	Float64 float64
	Valid   bool
}

// Scan implements sql.Scanner.
func (f *Float8) Scan(src any) error {
	if src == nil {
		*f = Float8{}
		return nil
	}
	v, err := toFloat64(src)
	if err != nil {
		return err
	}
	*f = Float8{Float64: v, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (f Float8) Value() (driver.Value, error) {
	if !f.Valid {
		return nil, nil
	}
	return f.Float64, nil
}

// Bool represents a nullable PostgreSQL boolean value.
type Bool struct {
	Bool  bool
	Valid bool
}

// Scan implements sql.Scanner.
func (b *Bool) Scan(src any) error {
	if src == nil {
		*b = Bool{}
		return nil
	}
	v, err := toBool(src)
	if err != nil {
		return err
	}
	*b = Bool{Bool: v, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (b Bool) Value() (driver.Value, error) {
	if !b.Valid {
		return nil, nil
	}
	return b.Bool, nil
}

// UUID represents a nullable PostgreSQL uuid value.
type UUID struct {
	Bytes [16]byte
	Valid bool
}

// String formats the UUID in canonical hyphenated lowercase form, or empty when invalid.
func (u UUID) String() string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// Scan implements sql.Scanner.
func (u *UUID) Scan(src any) error {
	if src == nil {
		*u = UUID{}
		return nil
	}
	if existing, ok := src.(UUID); ok {
		*u = existing
		return nil
	}
	var raw [16]byte
	if err := parseUUIDInto(src, &raw); err != nil {
		return err
	}
	*u = UUID{Bytes: raw, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (u UUID) Value() (driver.Value, error) {
	if !u.Valid {
		return nil, nil
	}
	return u.String(), nil
}

// Numeric represents a nullable arbitrary-precision PostgreSQL numeric value
// as Int * 10^Exp (or NaN).
type Numeric struct {
	Int   *big.Int
	Exp   int32
	NaN   bool
	Valid bool
}

// NumericFromString parses a decimal string (or "NaN") into a Numeric.
func NumericFromString(s string) (Numeric, error) {
	var n Numeric
	if err := n.Scan(s); err != nil {
		return Numeric{}, err
	}
	return n, nil
}

// String formats the Numeric as a canonical decimal string, "NaN", or "".
func (n Numeric) String() string {
	if !n.Valid {
		return ""
	}
	if n.NaN {
		return "NaN"
	}
	if n.Int == nil {
		return "0"
	}
	neg := n.Int.Sign() < 0
	abs := new(big.Int).Abs(n.Int)
	digits := abs.String()
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	exp := int(n.Exp)
	switch {
	case exp >= 0:
		b.WriteString(digits)
		for range exp {
			b.WriteByte('0')
		}
	case len(digits) <= -exp:
		b.WriteString("0.")
		for range -exp - len(digits) {
			b.WriteByte('0')
		}
		b.WriteString(digits)
	default:
		split := len(digits) + exp
		b.WriteString(digits[:split])
		b.WriteByte('.')
		b.WriteString(digits[split:])
	}
	return b.String()
}

// Float64 converts the Numeric to a float64.
func (n Numeric) Float64() (float64, error) {
	if !n.Valid {
		return 0, errors.New("pgx: Numeric is NULL")
	}
	if n.NaN {
		return math.NaN(), nil
	}
	return strconv.ParseFloat(n.String(), 64)
}

// Scan implements sql.Scanner.
func (n *Numeric) Scan(src any) error {
	if src == nil {
		*n = Numeric{}
		return nil
	}
	switch v := src.(type) {
	case Numeric:
		cp := Numeric{Exp: v.Exp, NaN: v.NaN, Valid: v.Valid}
		if v.Int != nil {
			cp.Int = new(big.Int).Set(v.Int)
		}
		*n = cp
		return nil
	case *Numeric:
		if v == nil {
			*n = Numeric{}
			return nil
		}
		return n.Scan(*v)
	case int64:
		*n = Numeric{Int: big.NewInt(v), Exp: 0, Valid: true}
		return nil
	case int:
		*n = Numeric{Int: big.NewInt(int64(v)), Exp: 0, Valid: true}
		return nil
	case float64:
		if math.IsNaN(v) {
			*n = Numeric{NaN: true, Valid: true}
			return nil
		}
		return parseNumericString(strconv.FormatFloat(v, 'g', -1, 64), n)
	case []byte:
		return parseNumericString(string(v), n)
	case string:
		return parseNumericString(v, n)
	default:
		return fmt.Errorf("pgx: cannot scan %T into Numeric", src)
	}
}

func parseNumericString(raw string, dst *Numeric) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return errors.New("pgx: empty numeric string")
	}
	if strings.EqualFold(s, "nan") {
		*dst = Numeric{NaN: true, Valid: true}
		return nil
	}
	var expPart int64
	if idx := strings.IndexAny(s, "eE"); idx >= 0 {
		parsedExp, err := strconv.ParseInt(s[idx+1:], 10, 32)
		if err != nil {
			return fmt.Errorf("pgx: invalid numeric exponent in %q: %w", raw, err)
		}
		expPart = parsedExp
		s = s[:idx]
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	var fracDigits int64
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		fracDigits = int64(len(s) - dot - 1)
		s = s[:dot] + s[dot+1:]
	}
	if s == "" {
		return fmt.Errorf("pgx: invalid numeric %q", raw)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return fmt.Errorf("pgx: invalid numeric %q", raw)
		}
	}
	mantissa, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return fmt.Errorf("pgx: invalid numeric %q", raw)
	}
	if neg && mantissa.Sign() != 0 {
		mantissa.Neg(mantissa)
	}
	totalExp := expPart - fracDigits
	if totalExp < math.MinInt32 || totalExp > math.MaxInt32 {
		return fmt.Errorf("pgx: numeric exponent out of range in %q", raw)
	}
	*dst = Numeric{Int: mantissa, Exp: int32(totalExp), Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (n Numeric) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.String(), nil
}

// Timestamp represents a nullable PostgreSQL timestamp without time zone value.
type Timestamp struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (t *Timestamp) Scan(src any) error {
	if src == nil {
		*t = Timestamp{}
		return nil
	}
	parsed, err := parseTimeValue(src)
	if err != nil {
		return err
	}
	*t = Timestamp{Time: parsed, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (t Timestamp) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return t.Time, nil
}

// Timestamptz represents a nullable PostgreSQL timestamp with time zone value.
type Timestamptz struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (t *Timestamptz) Scan(src any) error {
	if src == nil {
		*t = Timestamptz{}
		return nil
	}
	parsed, err := parseTimeValue(src)
	if err != nil {
		return err
	}
	*t = Timestamptz{Time: parsed, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (t Timestamptz) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return t.Time, nil
}

// Date represents a nullable PostgreSQL date value.
type Date struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
func (d *Date) Scan(src any) error {
	if src == nil {
		*d = Date{}
		return nil
	}
	parsed, err := parseTimeValue(src)
	if err != nil {
		return err
	}
	y, m, day := parsed.Date()
	*d = Date{Time: time.Date(y, m, day, 0, 0, 0, 0, time.UTC), Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (d Date) Value() (driver.Value, error) {
	if !d.Valid {
		return nil, nil
	}
	return d.Time.Format("2006-01-02"), nil
}

// JSONB represents a nullable PostgreSQL jsonb (or json) value.
type JSONB struct {
	Bytes []byte
	Valid bool
}

// Scan implements sql.Scanner.
func (j *JSONB) Scan(src any) error {
	if src == nil {
		*j = JSONB{}
		return nil
	}
	switch v := src.(type) {
	case []byte:
		cp := make([]byte, len(v))
		copy(cp, v)
		*j = JSONB{Bytes: cp, Valid: true}
		return nil
	case string:
		*j = JSONB{Bytes: []byte(v), Valid: true}
		return nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		*j = JSONB{Bytes: raw, Valid: true}
		return nil
	}
}

// Value implements driver.Valuer.
func (j JSONB) Value() (driver.Value, error) {
	if !j.Valid {
		return nil, nil
	}
	return string(j.Bytes), nil
}

// MarshalJSON implements json.Marshaler.
func (j JSONB) MarshalJSON() ([]byte, error) {
	if !j.Valid || len(j.Bytes) == 0 {
		return []byte("null"), nil
	}
	return j.Bytes, nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (j *JSONB) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*j = JSONB{}
		return nil
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	*j = JSONB{Bytes: cp, Valid: true}
	return nil
}

func parseTimeValue(src any) (time.Time, error) {
	switch v := src.(type) {
	case time.Time:
		return v, nil
	case []byte:
		return parseTimeString(string(v))
	case string:
		return parseTimeString(v)
	default:
		return time.Time{}, fmt.Errorf("pgx: cannot scan %T as time.Time", src)
	}
}

func parseTimeString(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999-07",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("pgx: cannot parse %q as time.Time", s)
}
