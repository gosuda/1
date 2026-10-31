package builtin

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var nativeTestFormatLocale = model.FormatLocale{
	NumericName:             "C",
	MonetaryName:            "C",
	TimeName:                "C",
	DecimalPoint:            ".",
	ThousandsSeparator:      ",",
	PositiveSign:            "+",
	NegativeSign:            "-",
	CurrencySymbol:          "$",
	ISOCurrencySymbol:       "USD",
	MonthNames:              [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	AbbreviatedMonthNames:   [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	DayNames:                [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	AbbreviatedDayNames:     [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
}

func TestNativeNumericPicturesMatchPostgreSQLExamples(t *testing.T) {
	for _, test := range []struct {
		input   string
		picture string
		want    string
	}{
		{input: "-0.1", picture: "99.99", want: "  -.10"},
		{input: "-0.1", picture: "FM9.99", want: "-.1"},
		{input: "-0.1", picture: "FM90.99", want: "-0.1"},
		{input: "0.1", picture: "0.9", want: " 0.1"},
		{input: "12", picture: "9990999.9", want: "    0012.0"},
		{input: "12", picture: "FM9990999.9", want: "0012."},
		{input: "485", picture: "9 9 9", want: " 4 8 5"},
		{input: "1485", picture: "9,999", want: " 1,485"},
		{input: "148.5", picture: "FM999.990", want: "148.500"},
		{input: "-485", picture: "999S", want: "485-"},
		{input: "-12", picture: "S9999", want: "  -12"},
		{input: "-485", picture: "9SG99", want: "4-85"},
		{input: "-485", picture: "999PR", want: "<485>"},
		{input: "482", picture: "999th", want: " 482nd"},
		{input: "12.45", picture: "99V9", want: " 125"},
		{input: "0.0004859", picture: "9.99EEEE", want: " 4.86e-04"},
		{input: "123456789012345678901234567890", picture: "9.99EEEE", want: " 1.23e+29"},
		{input: "9.99", picture: "9.9EEEE", want: " 1.0e+01"},
	} {
		t.Run(test.input+"/"+test.picture, func(t *testing.T) {
			got, err := nativeFormatNumberPicture(test.input, test.picture, nativeTestFormatLocale)
			if err != nil || got != test.want {
				t.Fatalf("format = %q, %v; want %q", got, err, test.want)
			}
		})
	}
	parsed, err := nativeParseNumberPicture(".1", ".9", nativeTestFormatLocale)
	if err != nil || parsed != "0.1" {
		t.Fatalf("parsed leading decimal = %q, %v; want 0.1", parsed, err)
	}
}

func TestNativeDatePictureInputSeparatorAndISOConventions(t *testing.T) {
	for _, test := range []struct {
		input, picture string
		year           int64
		month, day     int
	}{
		{input: " 2000 - JUN", picture: "YYYY-MON", year: 2000, month: 6, day: 1},
		{input: "2000JUN", picture: "YYYY///MON", year: 2000, month: 6, day: 1},
		{input: "2000/JUN", picture: "FXYYYY MON", year: 2000, month: 6, day: 1},
		{input: "2006-42-4", picture: "IYYY-IW-ID", year: 2006, month: 10, day: 19},
		{input: "70", picture: "YY", year: 1970, month: 1, day: 1},
		{input: "520", picture: "YYY", year: 1520, month: 1, day: 1},
		{input: "519", picture: "YYY", year: 2519, month: 1, day: 1},
		{input: "2000", picture: "YYYYSP", year: 2000, month: 1, day: 1},
		{input: "5", picture: "Y", year: 2005, month: 1, day: 1},
	} {
		t.Run(test.input+"/"+test.picture, func(t *testing.T) {
			got, err := nativeParseDatePicture(test.input, test.picture, nativeTestFormatLocale)
			if err != nil || got.year != test.year || got.month != test.month || got.day != test.day {
				t.Fatalf("parse = %+v, %v; want %04d-%02d-%02d", got, err, test.year, test.month, test.day)
			}
		})
	}
	fraction, err := nativeParseDatePicture("12.3", "SS.MS", nativeTestFormatLocale)
	if err != nil || fraction.second != 12 || fraction.micros != 300000 {
		t.Fatalf("fraction parse = %+v, %v; want 12.300000 seconds", fraction, err)
	}
	implicitYear, err := nativeParseDatePicture("6", "MM", nativeTestFormatLocale)
	if err != nil || implicitYear.year != 0 || implicitYear.month != 6 || implicitYear.day != 1 {
		t.Fatalf("implicit year parse = %+v, %v; want 0001 BC-06-01", implicitYear, err)
	}
	_, err = nativeParseDatePicture("2006-42-4", "IYYY-IW-D", nativeTestFormatLocale)
	requireReplacementSQLState(t, err, "22007")
	for _, test := range []struct {
		input   string
		picture string
	}{
		{input: "2006-42-4", picture: "IYYY-IW-ID"},
		{input: "4-42-2006", picture: "ID-IW-IYYY"},
	} {
		isoDate, parseErr := nativeParseDatePicture(test.input, test.picture, nativeTestFormatLocale)
		if parseErr != nil || isoDate.year != 2006 || isoDate.month != 10 || isoDate.day != 19 {
			t.Fatalf("ISO parse %q/%q = %+v, %v; want 2006-10-19", test.input, test.picture, isoDate, parseErr)
		}
	}
}

func TestNativeNumericPicturesUseResolvedLocale(t *testing.T) {
	locale := nativeTestFormatLocale
	locale.DecimalPoint = ","
	locale.ThousandsSeparator = "."
	locale.PositiveSign = "p"
	locale.NegativeSign = "m"
	locale.CurrencySymbol = "€"
	locale.ISOCurrencySymbol = "EUR"
	got, err := nativeFormatNumberPicture("-1234.5", "L9G999D9S", locale)
	if err != nil || got != "€1.234,5m" {
		t.Fatalf("localized format = %q, %v; want %q", got, err, "€1.234,5m")
	}
	got, err = nativeFormatNumberPicture("1.2", "C9D9S", locale)
	if err != nil || got != "EUR1,2p" {
		t.Fatalf("localized ISO currency format = %q, %v; want %q", got, err, "EUR1,2p")
	}
	parsed, err := nativeParseNumberPicture("$12.454,8m", "L99G999D9S", locale)
	if err != nil || parsed != "-12454.8" {
		t.Fatalf("localized parse = %q, %v; want -12454.8", parsed, err)
	}
}

func TestNativeDatePicturesUseResolvedTimeLocale(t *testing.T) {
	locale := nativeTestFormatLocale
	locale.MonthNames[5] = "Juni"
	locale.AbbreviatedMonthNames[5] = "Jun."
	parsed, err := nativeParseDatePicture("2000-Jun.", "YYYY-TMMON", locale)
	if err != nil || parsed.year != 2000 || parsed.month != 6 || parsed.day != 1 {
		t.Fatalf("localized parse = %+v, %v; want 2000-06-01", parsed, err)
	}
	value := (dateFieldJulianDay(2000, 6, 1) - dateEpochJulian) * intervalMicrosPerDay
	got, err := nativeFormatDatePicture(value, "TMMONTH", namedTimeZone{}, false, locale)
	if err != nil || got != "JUNI" {
		t.Fatalf("localized format = %q; want JUNI", got)
	}
}
