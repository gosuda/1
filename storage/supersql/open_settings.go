package supersql

import (
	"context"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

// newStackSettings resolves session GUC values that name a session-visible
// object or locale. The native build pins its time zone database, its single
// C locale, its built-in text search configurations, and any host-registered
// custom text search analyzers, so resolution does not require a catalog read
// view.
type newStackSettings struct {
	database *Catalog
}

// ResolveTimeZone validates a name against the pinned tzcode database. Fixed
// offsets never reach this method: Settings parses numeric zones itself. The
// returned identity is the caller's canonical spelling, so the zone data stays
// owned by builtin.
func (newStackSettings) ResolveTimeZone(ctx context.Context, name string) (session.Zone, error) {
	if err := ctx.Err(); err != nil {
		return session.Zone{}, err
	}
	value := strings.TrimSpace(name)
	if value == "" {
		return session.Zone{}, &model.Error{Code: "22023", Message: "invalid value for parameter \"TimeZone\"", Detail: "empty time zone name"}
	}
	// Resolving timestamptz_out under this zone is the retained validation path:
	// it rejects unknown names and leap-second zones exactly as evaluation would.
	if _, err := builtin.ResolveTyped(ctx, timeZoneValidationFunction,
		[]model.TypeLayout{timestamptzLayout}, textLayout,
		builtin.ResolveOptions{TimeZone: builtin.TimeZoneContext{Name: value}}, 1<<20); err != nil {
		return session.Zone{}, &model.Error{Code: "22023", Message: "invalid value for parameter \"TimeZone\"", Detail: value, Cause: err}
	}
	return session.Zone{Name: value}, nil
}

// ResolveTextSearchConfig accepts any host-registered custom text search
// analyzer or built-in PostgreSQL text search configuration (simple and all
// embedded Snowball languages).
func (s newStackSettings) ResolveTextSearchConfig(ctx context.Context, name string, _ []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parts, err := builtin.ParseQualifiedName(ctx, name, 4096)
	if err != nil {
		return "", &model.Error{Code: "42602", Message: "invalid text search configuration name", Detail: name, Cause: err}
	}
	if len(parts) == 1 && s.database != nil && s.database.textSearchRegistry != nil {
		if cfg, ok := s.database.textSearchRegistry.LookupByName(parts[0]); ok {
			return cfg.Name, nil
		}
	}
	if canonical, ok := builtin.HasBuiltinTextSearchConfiguration(name); ok {
		return canonical, nil
	}
	return "", &model.Error{Code: "42704", Message: "text search configuration does not exist", Detail: name}
}

// ResolveFormatLocale accepts only the C locale, which is the build's sole
// locale data set. Its numeric, monetary and time formatting picture is
// materialized here from the same English data the native formatters use.
func (newStackSettings) ResolveFormatLocale(ctx context.Context, numeric, monetary, clock string) (model.FormatLocale, error) {
	if err := ctx.Err(); err != nil {
		return model.FormatLocale{}, err
	}
	for _, name := range [...]string{numeric, monetary, clock} {
		if !strings.EqualFold(strings.TrimSpace(name), "C") {
			return model.FormatLocale{}, &model.Error{Code: "22023", Message: "invalid locale name for the embedded C locale", Detail: name}
		}
	}
	return cFormatLocale, nil
}

var (
	_ session.SettingResolver = newStackSettings{}
)

// timeZoneValidationFunction is timestamptz_out, the stable-immutable routine
// whose compile step retains and validates the session time zone.
const timeZoneValidationFunction model.FunctionID = 1151

var (
	timestamptzLayout = model.TypeLayout{Type: model.Type{ID: 1184, Modifier: -1, Storage: model.TypeTimestamp}}
	textLayout        = model.TypeLayout{Type: model.Type{ID: 2275, Modifier: -1, Storage: model.TypeString}}
)

// cFormatLocale mirrors the C locale's decimal and date picture.
var cFormatLocale = model.FormatLocale{
	NumericName: "C", MonetaryName: "C", TimeName: "C",
	DecimalPoint: ".", ThousandsSeparator: ",", PositiveSign: "+", NegativeSign: "-",
	CurrencySymbol: " ", ISOCurrencySymbol: " ",
	MonthNames:            [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
	AbbreviatedMonthNames: [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	DayNames:              [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	AbbreviatedDayNames:   [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
}
