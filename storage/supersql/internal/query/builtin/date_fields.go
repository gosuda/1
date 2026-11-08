package builtin

import (
	"cmp"
	"context"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	dateFieldDOW intervalInputUnit = intervalFieldReserved + 1 + iota
	dateFieldISODOW
	dateFieldDOY
	dateFieldISOYear
	dateFieldJulian
)

func dateFieldUnit(text string) intervalInputUnit {
	lower := intervalInputKeyword(text)
	length := min(len(text), len(lower))
	for i := range length {
		if lower[i] == 0 {
			length = i
			break
		}
	}
	switch string(lower[:length]) {
	case "dow":
		return dateFieldDOW
	case "isodow":
		return dateFieldISODOW
	case "doy":
		return dateFieldDOY
	case "isoyear":
		return dateFieldISOYear
	case "j", "jd", "julian":
		return dateFieldJulian
	}
	return intervalFieldUnit(text, true)
}

func compileDateFields(signature Signature) scalarKernel {
	if signature.ID == 1384 {
		partSignature, known := LookupSignature(2021)
		if !known {
			return nil
		}
		part := compileTimestampFields(partSignature)
		return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
			if err := ctx.Err(); err != nil {
				return model.Value{}, err
			}
			value, err := timestampFromDate(args[1].IntVal)
			if err != nil {
				return model.Value{}, err
			}
			arguments := [2]model.Value{args[0], timestampValue(value)}
			return part(ctx, arguments[:], budget)
		}
	}
	if signature.ID != 6199 {
		return nil
	}
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		days := args[1].IntVal
		if !validDateDatum(days) {
			return model.Value{}, sqlError("22008", "date out of range")
		}
		unit := dateFieldUnit(args[0].StrVal)
		switch unit {
		case intervalInputUnknown, intervalInputIgnore, intervalInputAgo:
			return model.Value{}, temporalFieldError(args[0].StrVal, "date", false)
		case intervalFieldReserved, intervalInputHour, intervalInputMinute, intervalInputSecond, intervalInputMillisecond, intervalInputMicrosecond, intervalInputUnsupported:
			return model.Value{}, temporalFieldError(args[0].StrVal, "date", true)
		}
		if !finiteDate(days) {
			switch unit {
			case intervalInputDay, intervalInputMonth, intervalFieldQuarter, intervalInputWeek, dateFieldDOW, dateFieldISODOW, dateFieldDOY:
				return model.NullVal(model.TypeNumeric), nil
			}
			text := "Infinity"
			if days < 0 {
				text = "-Infinity"
			}
			if err := outputBudget(int64(len(text)), budget.OutputBytes); err != nil {
				return model.Value{}, err
			}
			return model.Value{Type: model.TypeNumeric, StrVal: text}, nil
		}
		return intervalNumericField(ctx, dateIntegralField(unit, days), 0, 0, budget.OutputBytes)
	}
}

// featureDatetimeFields and featureEncodingConversion name the embedded_pg18
// profile exclusions the builtin runtime kernels raise for temporal field units
// and character-set conversion. The bind package owns the canonical FeatureID
// values (bind.DatetimeFields, bind.EncodingConversion) but cannot be imported
// here: bind imports builtin. gates_test.go's manifest-driven exclusion table
// fails if these ids drift from testdata/postgres/embedded_features.json.
const (
	featureDatetimeFields     = "datetime_fields"
	featureEncodingConversion = "encoding_conversion"
)

func temporalFieldError(text, typeName string, supported bool) error {
	end := min(len(text), nameBytes)
	if end < len(text) {
		for end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	var lower [nameBytes]byte
	for i := range end {
		c := text[i]
		if c == 0 {
			end = i
			break
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	unit := "unit \"" + string(lower[:end]) + "\" "
	if supported {
		return &model.Error{Code: "0A000", Message: unit + "not supported for type " + typeName, FeatureID: featureDatetimeFields}
	}
	return sqlError("22023", unit+"not recognized for type "+typeName)
}

func dateIntegralField(unit intervalInputUnit, days int64) int64 {
	year, month, day := dateCalendar(days)
	julian := int32(days + dateEpochJulian)
	switch unit {
	case intervalInputDay:
		return day
	case intervalInputMonth:
		return month
	case intervalFieldQuarter:
		return (month-1)/3 + 1
	case intervalInputYear:
		if year <= 0 {
			return year - 1
		}
		return year
	case intervalInputDecade:
		if year < 0 {
			return -((9 - year) / 10)
		}
		return year / 10
	case intervalInputCentury:
		if year <= 0 {
			return -((100 - year) / 100)
		}
		return (year + 99) / 100
	case intervalInputMillennium:
		if year <= 0 {
			return -((1000 - year) / 1000)
		}
		return (year + 999) / 1000
	case dateFieldJulian:
		return int64(julian)
	case dateFieldDOW:
		return int64(dateWeekday(julian))
	case dateFieldISODOW:
		return int64(cmp.Or(dateWeekday(julian), 7))
	case dateFieldDOY:
		current := int32(dateFieldJulianDay(year, month, day))
		first := int32(dateFieldJulianDay(year, 1, 1))
		return int64(current - first + 1)
	case intervalInputWeek, dateFieldISOYear:
		return dateISOField(unit, year, month, day)
	default:
		return (days + 10957) * 86400
	}
}

func dateWeekday(julian int32) int32 {
	weekday := (julian + 1) % 7
	if weekday < 0 {
		weekday += 7
	}
	return weekday
}

func dateISOStart(year int64) int32 {
	fourth := int32(dateFieldJulianDay(year, 1, 4))
	return fourth - dateWeekday(fourth-1)
}

func dateISOField(unit intervalInputUnit, year, month, day int64) int64 {
	julian := int32(dateFieldJulianDay(year, month, day))
	start := dateISOStart(year)
	if julian < start {
		start = dateISOStart(year - 1)
		if unit == dateFieldISOYear {
			year--
		}
	}
	week := (julian-start)/7 + 1
	if week >= 52 {
		next := dateISOStart(year + 1)
		if julian >= next {
			if unit == intervalInputWeek {
				week = (julian-next)/7 + 1
			} else {
				year++
			}
		}
	}
	if unit == intervalInputWeek {
		return int64(week)
	}
	if year <= 0 {
		year--
	}
	return year
}

func dateFieldJulianDay(year, month, day int64) int64 {
	if month > 2 {
		month++
		year += 4800
	} else {
		month += 13
		year += 4799
	}
	century := year / 100
	return year*365 - 32167 + year/4 - century + century/4 + 7834*month/256 + day
}

func dateISOYearStart(year int64) int64 {
	fourth := dateFieldJulianDay(year, 1, 4)
	weekday := fourth % 7
	if weekday < 0 {
		weekday += 7
	}
	return fourth - weekday
}
