package builtin

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// jsonDatetimeText converts ISO SQL output to the SQL/JSON datetime spelling.
// Ordinary SQL and JSONB timetz output keep their existing offset spelling;
// JSONPath calls this helper for timetz as well as the timestamp types.
func jsonDatetimeText(typeID model.TypeID, text string) (string, error) {
	if typeID != 1114 && typeID != 1184 && typeID != 1266 {
		return text, nil
	}
	var scratch [timestamptzOutputMaxBytes + 3]byte
	if len(text) > len(scratch)-3 {
		return "", sqlError("22008", "timestamp out of range")
	}
	length := copy(scratch[:], text)
	if typeID == 1114 || typeID == 1184 {
		for i := range length {
			if scratch[i] == ' ' && i+1 < length && scratch[i+1] >= '0' && scratch[i+1] <= '9' {
				scratch[i] = 'T'
				break
			}
		}
	}
	if typeID == 1184 || typeID == 1266 {
		end := length
		if strings.HasSuffix(text, " BC") {
			end -= 3
		}
		if end >= 3 && (scratch[end-3] == '+' || scratch[end-3] == '-') {
			copy(scratch[end+3:], scratch[end:length])
			copy(scratch[end:end+3], ":00")
			length += 3
		}
	}
	return string(scratch[:length]), nil
}

// A JSONPath timestamp keeps the explicit offset supplied by its string.
// The datum remains UTC for comparisons; only rendering uses this fixed offset.
func (execution *jsonPathExecution) datetimeTimeZone(text string) (int32, bool, error) {
	fields, err := parseTemporalInputMode(execution.ctx, text, execution.options, false)
	if err != nil {
		return 0, false, err
	}
	if !fields.HasZone {
		return 0, false, nil
	}
	zone, _, err := resolveTimeZoneContext(execution.options.TimeZone)
	if err != nil {
		return 0, false, err
	}
	days := dateFieldJulianDay(fields.Year, int64(fields.Month), int64(fields.Day)) - dateEpochJulian
	offset, err := temporalInputOffset(execution.ctx, fields, zone, days)
	if err != nil {
		return 0, false, err
	}
	return int32(offset), true, nil
}
