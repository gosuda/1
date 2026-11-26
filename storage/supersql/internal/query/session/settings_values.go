package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

type settingID uint8

const (
	searchPath settingID = iota
	timeZone
	dateStyle
	intervalStyle
	extraFloatDigits
	byteaOutput
	standardConformingStrings
	escapeStringWarning
	backslashQuote
	clientEncoding
	clientMinMessages
	applicationName
	checkFunctionBodies
	lcMonetary
	lcNumeric
	lcTime
	defaultTextSearchConfig
	defaultIsolation
	transactionIsolation
	defaultReadOnly
	transactionReadOnly
	defaultDeferrable
	transactionDeferrable
	statementTimeout
	lockTimeout
	idleInTransactionTimeout
	transactionTimeout
	workMem
	maintenanceWorkMem
	tempFileLimit
	serverEncoding
	serverVersion
	serverVersionNumber
	integerDatetimes
	maxIdentifierLength
	settingCount
)

type settingKind uint8

const (
	stringSetting settingKind = iota
	booleanSetting
	integerSetting
	memorySetting
	durationSetting
	readOnlySetting
)

type settingDefinition struct {
	name    string
	initial string
	kind    settingKind
	minimum int64
	maximum int64
}

var settingDefinitions = [settingCount]settingDefinition{
	searchPath:                {"search_path", `"$user", public`, stringSetting, 0, 0},
	timeZone:                  {"TimeZone", "GMT", stringSetting, 0, 0},
	dateStyle:                 {"DateStyle", "ISO, MDY", stringSetting, 0, 0},
	intervalStyle:             {"IntervalStyle", "postgres", stringSetting, 0, 0},
	extraFloatDigits:          {"extra_float_digits", "1", integerSetting, -15, 3},
	byteaOutput:               {"bytea_output", "hex", stringSetting, 0, 0},
	standardConformingStrings: {"standard_conforming_strings", "on", booleanSetting, 0, 0},
	escapeStringWarning:       {"escape_string_warning", "on", booleanSetting, 0, 0},
	backslashQuote:            {"backslash_quote", "safe_encoding", stringSetting, 0, 0},
	clientEncoding:            {"client_encoding", "UTF8", stringSetting, 0, 0},
	clientMinMessages:         {"client_min_messages", "notice", stringSetting, 0, 0},
	applicationName:           {"application_name", "", stringSetting, 0, 0},
	checkFunctionBodies:       {"check_function_bodies", "on", booleanSetting, 0, 0},
	lcMonetary:                {"lc_monetary", "C", stringSetting, 0, 0},
	lcNumeric:                 {"lc_numeric", "C", stringSetting, 0, 0},
	lcTime:                    {"lc_time", "C", stringSetting, 0, 0},
	defaultTextSearchConfig:   {"default_text_search_config", "pg_catalog.simple", stringSetting, 0, 0},
	defaultIsolation:          {"default_transaction_isolation", "serializable", stringSetting, 0, 0},
	transactionIsolation:      {"transaction_isolation", "serializable", stringSetting, 0, 0},
	defaultReadOnly:           {"default_transaction_read_only", "off", booleanSetting, 0, 0},
	transactionReadOnly:       {"transaction_read_only", "off", booleanSetting, 0, 0},
	defaultDeferrable:         {"default_transaction_deferrable", "off", booleanSetting, 0, 0},
	transactionDeferrable:     {"transaction_deferrable", "off", booleanSetting, 0, 0},
	statementTimeout:          {"statement_timeout", "0", durationSetting, 0, math.MaxInt32},
	lockTimeout:               {"lock_timeout", "0", durationSetting, 0, math.MaxInt32},
	idleInTransactionTimeout:  {"idle_in_transaction_session_timeout", "0", durationSetting, 0, math.MaxInt32},
	transactionTimeout:        {"transaction_timeout", "0", durationSetting, 0, math.MaxInt32},
	workMem:                   {"work_mem", "65536", memorySetting, 64, math.MaxInt32},
	maintenanceWorkMem:        {"maintenance_work_mem", "65536", memorySetting, 64, math.MaxInt32},
	tempFileLimit:             {"temp_file_limit", "-1", memorySetting, -1, math.MaxInt32},
	serverEncoding:            {"server_encoding", "UTF8", readOnlySetting, 0, 0},
	// These identify the dialect target, not a running PostgreSQL server.
	serverVersion:       {"server_version", "18.4 (SuperSQL embedded_pg18 dialect)", readOnlySetting, 0, 0},
	serverVersionNumber: {"server_version_num", "180004", readOnlySetting, 0, 0},
	integerDatetimes:    {"integer_datetimes", "on", readOnlySetting, 0, 0},
	maxIdentifierLength: {"max_identifier_length", "63", readOnlySetting, 0, 0},
}

func (s *Settings) parse(ctx context.Context, id settingID, raw string) (settingValue, error) {
	if err := ctx.Err(); err != nil {
		return settingValue{}, err
	}
	def := settingDefinitions[id]
	if !s.validString(raw) {
		return settingValue{}, badSetting(def.name, "invalid UTF8, zero byte, or host value-size limit")
	}
	var result settingValue
	switch def.kind {
	case booleanSetting:
		flag, ok := parseBoolean(raw)
		if !ok {
			return result, badSetting(def.name, raw)
		}
		return booleanValue(flag), nil
	case integerSetting, memorySetting, durationSetting:
		number, ok := parseSettingNumber(raw, def.kind)
		if !ok || number < def.minimum || number > def.maximum {
			return result, badSetting(def.name, raw)
		}
		return settingValue{number: number, text: showSettingNumber(number, def.kind)}, nil
	case readOnlySetting:
		return settingValue{text: raw}, nil
	}
	value := strings.ToLower(raw)
	switch id {
	case searchPath:
		if _, err := identifierList(raw); err != nil {
			return result, badSetting(def.name, raw)
		}
		result.text = strings.Clone(raw)
	case timeZone:
		zone, numeric, err := numericTimeZone(raw)
		if !numeric {
			named, lookupErr := s.resolver.ResolveTimeZone(ctx, raw)
			if lookupErr != nil {
				return result, errors.Join(lookupErr, err)
			}
			zone = named
		} else if err != nil {
			return result, err
		}
		if zone.Name == "" || !s.validString(zone.Name) {
			return result, settingError("XX000", "timezone resolver returned invalid identity")
		}
		zone.Name = strings.Clone(zone.Name)
		result = settingValue{text: zone.Name, zone: zone}
	case dateStyle:
		current := cmp.Or(s.current[dateStyle].text, "ISO, MDY")
		reset := cmp.Or(s.reset[dateStyle].text, "ISO, MDY")
		canonical, ok := parseDateStyle(raw, current, reset)
		if !ok {
			return result, badSetting(def.name, raw)
		}
		result.text = canonical
	case intervalStyle:
		if !oneOf(value, "postgres", "postgres_verbose", "sql_standard", "iso_8601") {
			return result, badSetting(def.name, raw)
		}
		result.text = value
	case byteaOutput:
		if !oneOf(value, "hex", "escape") {
			return result, badSetting(def.name, raw)
		}
		result.text = value
	case backslashQuote:
		switch value {
		case "safe_encoding":
			result.text = value
		case "on", "true", "yes", "1":
			result.text = "on"
		case "off", "false", "no", "0":
			result.text = "off"
		default:
			return result, badSetting(def.name, raw)
		}
	case clientEncoding:
		// PostgreSQL encoding-name lookup ignores non-alphanumeric characters.
		var normalized strings.Builder
		for _, ch := range value {
			letter, digit := ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9'
			if letter || digit {
				normalized.WriteRune(ch)
			}
		}
		encoding := normalized.String()
		if encoding != "utf8" && encoding != "unicode" {
			if !excludedEncoding(encoding) {
				return result, badSetting(def.name, raw)
			}
			return result, &model.Error{Code: "0A000", FeatureID: string(bind.LocaleProviders), Message: "only UTF8 client encoding is supported"}
		}
		result.text = "UTF8"
	case clientMinMessages:
		if !oneOf(value, "debug5", "debug4", "debug3", "debug2", "debug1", "debug", "log", "info", "notice", "warning", "error") {
			return result, badSetting(def.name, raw)
		}
		if value == "debug" {
			value = "debug2"
		}
		result.text = value
	case applicationName:
		// GUC_IS_NAME clips at a UTF8 boundary before check_application_name.
		clipped := clipIdentifier(raw)
		var clean strings.Builder
		for i := range len(clipped) {
			ch := clipped[i]
			if ch < 32 || ch > 126 {
				fmt.Fprintf(&clean, `\x%02x`, ch)
			} else {
				clean.WriteByte(ch)
			}
		}
		result.text = clean.String()
	case lcMonetary, lcNumeric, lcTime:
		numeric, monetary, clock := cmp.Or(s.current[lcNumeric].text, "C"), cmp.Or(s.current[lcMonetary].text, "C"), cmp.Or(s.current[lcTime].text, "C")
		switch id {
		case lcNumeric:
			numeric = raw
		case lcMonetary:
			monetary = raw
		default:
			clock = raw
		}
		locale, err := s.resolver.ResolveFormatLocale(ctx, numeric, monetary, clock)
		if err != nil {
			return result, err
		}
		if locale.NumericName == "" || locale.MonetaryName == "" || locale.TimeName == "" ||
			!s.validString(locale.NumericName) || !s.validString(locale.MonetaryName) || !s.validString(locale.TimeName) {
			return result, settingError("XX000", "format locale resolver returned an invalid identity")
		}
		if (id != lcNumeric && locale.NumericName != numeric) ||
			(id != lcMonetary && locale.MonetaryName != monetary) ||
			(id != lcTime && locale.TimeName != clock) {
			return result, settingError("XX000", "format locale resolver changed an unrelated identity")
		}
		switch id {
		case lcNumeric:
			result.text = locale.NumericName
		case lcMonetary:
			result.text = locale.MonetaryName
		default:
			result.text = locale.TimeName
		}
		result.text = strings.Clone(result.text)
	case defaultTextSearchConfig:
		canonical, err := s.resolver.ResolveTextSearchConfig(ctx, raw, s.SearchPath())
		if err != nil {
			var sqlErr *model.Error
			if errors.As(err, &sqlErr) && oneOf(sqlErr.Code, "42704", "42602") {
				return result, &model.Error{Code: "22023", Message: "invalid text search configuration", Detail: raw, Cause: err}
			}
			return result, err
		}
		if canonical == "" || !s.validString(canonical) {
			return result, settingError("XX000", "text-search resolver returned invalid identity")
		}
		result.text = strings.Clone(canonical)
	case defaultIsolation, transactionIsolation:
		if err := bind.ValidateIsolation(raw, 0); err != nil {
			return result, err
		}
		result.text = "serializable"
	default:
		return result, settingError("XX000", "missing setting value parser")
	}
	if !s.validString(result.text) {
		return settingValue{}, settingError("54000", "setting host value-size limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return settingValue{}, err
	}
	return result, nil
}

func booleanValue(flag bool) settingValue {
	text := "off"
	if flag {
		text = "on"
	}
	return settingValue{text: text, flag: flag}
}

func parseBoolean(value string) (bool, bool) {
	value = strings.ToLower(value)
	if value == "1" {
		return true, true
	}
	if value == "0" {
		return false, true
	}
	if value == "" || value == "o" {
		return false, false
	}
	for _, word := range []string{"true", "yes", "on"} {
		if strings.HasPrefix(word, value) {
			return true, true
		}
	}
	for _, word := range []string{"false", "no", "off"} {
		if strings.HasPrefix(word, value) {
			return false, true
		}
	}
	return false, false
}

func oneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func parseDateStyle(raw, current, reset string) (string, bool) {
	parts, err := identifierList(raw)
	if err != nil {
		return "", false
	}
	pair := strings.Split(current, ", ")
	style, order := pair[0], pair[1]
	haveStyle, haveOrder := false, false
	for _, part := range parts {
		word := strings.ToUpper(part)
		newStyle, newOrder := "", ""
		switch {
		case word == "ISO":
			newStyle = "ISO"
		case word == "SQL":
			newStyle = "SQL"
		case strings.HasPrefix(word, "POSTGRES"):
			newStyle = "Postgres"
		case word == "GERMAN":
			newStyle = "German"
			if !haveOrder {
				order = "DMY"
			}
		case word == "YMD":
			newOrder = "YMD"
		case word == "DMY" || strings.HasPrefix(word, "EURO"):
			newOrder = "DMY"
		case word == "MDY" || word == "US" || strings.HasPrefix(word, "NONEURO"):
			newOrder = "MDY"
		case word == "DEFAULT":
			defaults := strings.Split(reset, ", ")
			if !haveStyle {
				style = defaults[0]
			}
			if !haveOrder {
				order = defaults[1]
			}
		default:
			return "", false
		}
		if newStyle != "" {
			if haveStyle && style != newStyle {
				return "", false
			}
			style, haveStyle = newStyle, true
		}
		if newOrder != "" {
			if haveOrder && order != newOrder {
				return "", false
			}
			order, haveOrder = newOrder, true
		}
	}
	return style + ", " + order, true
}

func clipIdentifier(value string) string {
	if len(value) <= 63 {
		return value
	}
	end := 63
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func asciiSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\v' || ch == '\f'
}

// identifierList matches SplitIdentifierString, not SQL lexical identifiers:
// unquoted components extend to whitespace/separator and are ASCII-folded.
func identifierList(raw string) ([]string, error) {
	var parts []string
	pos := 0
	for pos < len(raw) && asciiSpace(raw[pos]) {
		pos++
	}
	if pos == len(raw) {
		return parts, nil
	}
	for {
		var value string
		if raw[pos] == '"' {
			pos++
			var text strings.Builder
			closed := false
			for pos < len(raw) {
				ch := raw[pos]
				pos++
				if ch != '"' {
					text.WriteByte(ch)
					continue
				}
				if pos < len(raw) && raw[pos] == '"' {
					text.WriteByte('"')
					pos++
					continue
				}
				closed = true
				break
			}
			if !closed {
				return nil, settingError("22023", "unterminated identifier")
			}
			value = text.String()
		} else {
			start := pos
			for pos < len(raw) && raw[pos] != ',' && !asciiSpace(raw[pos]) {
				pos++
			}
			value = raw[start:pos]
			if value == "" {
				return nil, settingError("22023", "empty identifier")
			}
			var folded strings.Builder
			for i := range len(value) {
				ch := value[i]
				if ch >= 'A' && ch <= 'Z' {
					ch += 'a' - 'A'
				}
				folded.WriteByte(ch)
			}
			value = folded.String()
		}
		parts = append(parts, clipIdentifier(value))
		for pos < len(raw) && asciiSpace(raw[pos]) {
			pos++
		}
		if pos == len(raw) {
			return parts, nil
		}
		if raw[pos] != ',' {
			return nil, settingError("22023", "invalid identifier separator")
		}
		pos++
		for pos < len(raw) && asciiSpace(raw[pos]) {
			pos++
		}
		if pos == len(raw) {
			return nil, settingError("22023", "trailing identifier separator")
		}
	}
}

type settingUnit struct {
	name       string
	multiplier float64
}

var memoryUnits = [...]settingUnit{{"TB", 1073741824}, {"GB", 1048576}, {"MB", 1024}, {"kB", 1}, {"B", 1.0 / 1024}}
var durationUnits = [...]settingUnit{{"d", 86400000}, {"h", 3600000}, {"min", 60000}, {"s", 1000}, {"ms", 1}, {"us", .001}}

func unitsFor(kind settingKind) []settingUnit {
	if kind == memorySetting {
		return memoryUnits[:]
	}
	if kind == durationSetting {
		return durationUnits[:]
	}
	return nil
}

func parseSettingNumber(raw string, kind settingKind) (int64, bool) {
	value := strings.TrimLeft(raw, " \t\n\r\v\f")
	if value == "" {
		return 0, false
	}
	// strtol(base=0) consumes octal/hex before the optional strtod retry.
	pos := 0
	if value[pos] == '+' || value[pos] == '-' {
		pos++
	}
	start := pos
	base := 10
	if pos < len(value) && value[pos] == '0' {
		base = 8
		if pos+2 < len(value) {
			hexPrefix := value[pos+1] == 'x' || value[pos+1] == 'X'
			if hexPrefix && hexDigit(value[pos+2]) {
				base = 16
				pos += 2
				start = pos
			}
		}
	}
	for pos < len(value) && digitForBase(value[pos], base) {
		pos++
	}
	if pos == start && (pos == len(value) || value[pos] != '.') {
		return 0, false
	}
	number, intErr := strconv.ParseInt(value[:pos], 0, 64)
	if base == 10 {
		number, intErr = strconv.ParseInt(value[:pos], 10, 64)
	}
	f := float64(number)
	fractionalSuffix := false
	if pos < len(value) {
		switch value[pos] {
		case '.', 'e', 'E':
			fractionalSuffix = true
		}
	}
	if intErr != nil || fractionalSuffix {
		end := pos
		for end < len(value) && (value[end] >= '0' && value[end] <= '9' || strings.ContainsRune(".eE+-", rune(value[end]))) {
			end++
		}
		var err error
		f, err = strconv.ParseFloat(value[:end], 64)
		if err != nil {
			return 0, false
		}
		pos = end
	}
	unit := strings.Trim(value[pos:], " \t\n\r\v\f")
	if unit != "" {
		found := false
		units := unitsFor(kind)
		for i, entry := range units {
			if unit != entry.name {
				continue
			}
			f *= entry.multiplier
			if i+1 < len(units) {
				smaller := units[i+1].multiplier
				f = math.RoundToEven(f/smaller) * smaller
			}
			found = true
			break
		}
		if !found {
			return 0, false
		}
	}
	f = math.RoundToEven(f)
	if math.IsNaN(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, false
	}
	return int64(f), true
}

func hexDigit(ch byte) bool {
	return ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F'
}
func digitForBase(ch byte, base int) bool {
	if base == 16 {
		return hexDigit(ch)
	}
	return ch >= '0' && ch < '0'+byte(base)
}

func showSettingNumber(value int64, kind settingKind) string {
	if value <= 0 {
		return strconv.FormatInt(value, 10)
	}
	for _, entry := range unitsFor(kind) {
		if entry.multiplier < 1 {
			continue
		}
		unit := int64(entry.multiplier)
		if value%unit == 0 {
			return strconv.FormatInt(value/unit, 10) + entry.name
		}
	}
	return strconv.FormatInt(value, 10)
}

func excludedEncoding(value string) bool {
	switch value {
	case "abc", "alt", "big5", "euccn", "eucjis2004", "eucjp", "euckr", "euctw", "gb18030", "gbk",
		"iso88591", "iso885910", "iso885913", "iso885914", "iso885915", "iso885916", "iso88592", "iso88593", "iso88594", "iso88595", "iso88596", "iso88597", "iso88598", "iso88599",
		"johab", "koi8", "koi8r", "koi8u", "latin1", "latin10", "latin2", "latin3", "latin4", "latin5", "latin6", "latin7", "latin8", "latin9",
		"mskanji", "muleinternal", "shiftjis", "shiftjis2004", "sjis", "sqlascii", "tcvn", "tcvn5712", "uhc", "vscii", "win",
		"win1250", "win1251", "win1252", "win1253", "win1254", "win1255", "win1256", "win1257", "win1258", "win866", "win874", "win932", "win936", "win949", "win950",
		"windows1250", "windows1251", "windows1252", "windows1253", "windows1254", "windows1255", "windows1256", "windows1257", "windows1258", "windows866", "windows874", "windows932", "windows936", "windows949", "windows950":
		return true
	}
	return false
}
