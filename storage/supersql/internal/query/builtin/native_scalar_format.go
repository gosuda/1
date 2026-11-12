package builtin

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func nativeNumericPictureUsesLocale(picture string) bool {
	quoted := false
	for offset := 0; offset < len(picture); {
		if picture[offset] == '"' {
			quoted = !quoted
			offset++
			continue
		}
		if quoted {
			if picture[offset] == '\\' && offset+1 < len(picture) {
				offset++
			}
			offset++
			continue
		}
		if offset+2 <= len(picture) {
			switch strings.ToUpper(picture[offset : offset+2]) {
			case "FM", "MI", "PL", "PR", "RN", "SG", "SP", "TH":
				offset += 2
				continue
			}
		}
		switch picture[offset] {
		case 'D', 'd', 'G', 'g', 'L', 'l', 'C', 'c', 'S', 's':
			return true
		}
		offset++
	}
	return false
}

func nativeDatePictureUsesLocale(picture string) bool {
	quoted := false
	for offset := 0; offset < len(picture); offset++ {
		if picture[offset] == '"' {
			quoted = !quoted
			continue
		}
		if quoted && picture[offset] == '\\' && offset+1 < len(picture) {
			offset++
			continue
		}
		if !quoted && offset+2 <= len(picture) && strings.EqualFold(picture[offset:offset+2], "TM") {
			return true
		}
	}
	return false
}

func nativeFormatLocale(ctx context.Context, picture string, numeric, date bool, budget CallBudget) (model.FormatLocale, error) {
	needNumeric, needDate := numeric && nativeNumericPictureUsesLocale(picture), date && nativeDatePictureUsesLocale(picture)
	if !needNumeric && !needDate {
		return model.FormatLocale{}, nil
	}
	if budget.Settings == nil {
		return model.FormatLocale{}, sqlError("55000", "settings runtime is unavailable")
	}
	locale, err := budget.Settings.FormatLocale(ctx)
	if err != nil {
		return model.FormatLocale{}, err
	}
	validText := func(value string) bool { return utf8.ValidString(value) && !strings.ContainsRune(value, 0) }
	if (needNumeric && (locale.NumericName == "" || locale.MonetaryName == "" || !validText(locale.NumericName) || !validText(locale.MonetaryName))) || (needDate && (locale.TimeName == "" || !validText(locale.TimeName))) {
		return model.FormatLocale{}, sqlError("XX000", "format locale resolver returned an invalid identity")
	}
	for _, value := range []string{locale.DecimalPoint, locale.ThousandsSeparator, locale.PositiveSign, locale.NegativeSign, locale.CurrencySymbol, locale.ISOCurrencySymbol} {
		if needNumeric && !validText(value) {
			return model.FormatLocale{}, sqlError("XX000", "format locale resolver returned invalid numeric data")
		}
	}
	if needNumeric && (locale.DecimalPoint == "" || locale.ThousandsSeparator == "" || locale.PositiveSign == "" || locale.NegativeSign == "" || locale.CurrencySymbol == "" || locale.ISOCurrencySymbol == "" || locale.DecimalPoint == locale.ThousandsSeparator || locale.PositiveSign == locale.NegativeSign) {
		return model.FormatLocale{}, sqlError("XX000", "format locale resolver returned ambiguous numeric data")
	}
	if needDate {
		for _, names := range [][]string{locale.MonthNames[:], locale.AbbreviatedMonthNames[:], locale.DayNames[:], locale.AbbreviatedDayNames[:]} {
			for _, name := range names {
				if name == "" || !validText(name) {
					return model.FormatLocale{}, sqlError("XX000", "format locale resolver returned invalid time data")
				}
			}
		}
	}
	return locale, nil
}

func compileNativeFormat(signature Signature, zone namedTimeZone) scalarKernel {
	return func(ctx context.Context, arguments []model.Value, budget CallBudget) (result model.Value, err error) {
		format := arguments[1].StrVal
		numeric := signature.ID >= 1772 && signature.ID <= 1777
		date := signature.ID == 1770 || signature.ID == 1778 || signature.ID == 1780 || signature.ID == 2049
		locale, err := nativeFormatLocale(ctx, format, numeric, date, budget)
		if err != nil { return model.Value{}, err }
		switch signature.ID {
		case 1777:
			text, err := nativeParseNumberPicture(arguments[0].StrVal, format, locale)
			if err != nil { return model.Value{}, err }
			work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
			defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
			parsed, err := parseNumeric(ctx, work, text); if err != nil { return model.Value{}, err }
			return numericValue(ctx, parsed, budget.OutputLimit(work))
		case 1778:
			return nativePictureTimestamp(ctx, arguments[0].StrVal, format, zone, false, locale)
		case 1780:
			return nativePictureTimestamp(ctx, arguments[0].StrVal, format, zone, true, locale)
		case 1768:
			return ownedText(ctx, budget.OutputBytes, nativeFormatIntervalPicture(arguments[0].IntervalVal, format))
		case 1772, 1773, 1774, 1775, 1776:
			var number string
			switch signature.ID {
			case 1772:
				number = arguments[0].StrVal
			case 1773, 1774:
				number = strconv.FormatInt(arguments[0].IntVal, 10)
			case 1775:
				number = strconv.FormatFloat(arguments[0].FloatVal, 'g', -1, 32)
			default:
				number = strconv.FormatFloat(arguments[0].FloatVal, 'g', -1, 64)
			}
			text, err := nativeFormatNumberPicture(number, format, locale)
			if err != nil { return model.Value{}, err }
			return ownedText(ctx, budget.OutputBytes, text)
		default:
			text, err := nativeFormatDatePicture(arguments[0].IntVal, format, zone, signature.ID == 1770, locale)
			if err != nil { return model.Value{}, err }
			return ownedText(ctx, budget.OutputBytes, text)
		}
	}
}

