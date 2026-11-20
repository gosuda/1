package session

import (
	"strings"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// BuiltinOptions captures callable settings for both preparation and execution.
// Collations come from the immutable catalog snapshot, not session text settings.
func (s *Settings) BuiltinOptions(collation, defaultCollation model.CollationID) builtin.ResolveOptions {
	style := builtin.IntervalPostgres
	switch s.current[intervalStyle].text {
	case "postgres_verbose":
		style = builtin.IntervalPostgresVerbose
	case "sql_standard":
		style = builtin.IntervalSQLStandard
	case "iso_8601":
		style = builtin.IntervalISO8601
	}
	dateFormat, dateOrder := builtin.DateISO, builtin.DateMDY
	format, order, _ := strings.Cut(s.current[dateStyle].text, ", ")
	switch format {
	case "SQL":
		dateFormat = builtin.DateSQL
	case "Postgres":
		dateFormat = builtin.DatePostgres
	case "German":
		dateFormat = builtin.DateGerman
	}
	switch order {
	case "DMY":
		dateOrder = builtin.DateDMY
	case "YMD":
		dateOrder = builtin.DateYMD
	}
	zone := s.TimeZone()
	return builtin.ResolveOptions{
		Collation: collation, DefaultCollation: defaultCollation,
		ExtraFloatDigits: int(s.current[extraFloatDigits].number),
		ByteaEscape:      s.current[byteaOutput].text == "escape", IntervalStyle: style,
		DateStyle: dateFormat, DateOrder: dateOrder,
		TimeZone: builtin.TimeZoneContext{Name: zone.Name, Fixed: zone.Fixed, OffsetSeconds: zone.OffsetSeconds},
	}
}
