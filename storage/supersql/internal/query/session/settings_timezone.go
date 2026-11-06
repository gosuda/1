package session

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// numericTimeZone follows variable.c: hours are converted to whole seconds by
// truncation, with SQL's east-positive sign. pgtz.c renders the equivalent POSIX
// specification; tzcode accepts hours up to 167, not merely civil UTC +/-14.
func numericTimeZone(raw string) (Zone, bool, error) {
	value := strings.TrimLeft(raw, " \t\n\r\v\f")
	if strings.ContainsRune(value, '_') {
		return Zone{}, false, nil
	}
	hours, err := strconv.ParseFloat(value, 64)
	if err != nil {
		// C strtod accepts hex numbers without Go's mandatory p exponent.
		hex := strings.TrimPrefix(strings.TrimPrefix(value, "+"), "-")
		if strings.HasPrefix(strings.ToLower(hex), "0x") && !strings.ContainsAny(hex, "pP") {
			hours, err = strconv.ParseFloat(value+"p0", 64)
		}
	}
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			return Zone{}, true, errors.Join(badSetting("TimeZone", raw), err)
		}
		return Zone{}, false, err
	}
	seconds := math.Trunc(hours * 3600)
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= -604800 || seconds >= 604800 {
		return Zone{}, true, badSetting("TimeZone", raw)
	}
	offset := int32(seconds)
	absolute := offset
	if absolute < 0 {
		absolute = -absolute
	}
	text := fmt.Sprintf("%02d", absolute/3600)
	if absolute%3600 != 0 {
		text += fmt.Sprintf(":%02d", absolute%3600/60)
		if absolute%60 != 0 {
			text += fmt.Sprintf(":%02d", absolute%60)
		}
	}
	name := "<+" + text + ">-" + text
	if offset < 0 {
		name = "<-" + text + ">+" + text
	}
	return Zone{Name: name, Fixed: true, OffsetSeconds: offset}, true, nil
}
