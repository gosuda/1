package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestSQLTimestamptzFieldsSeparateLocalCalendarAndEpoch(t *testing.T) {
	options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "Asia/Kathmandu"}}
	prepared := prepareSemanticSQL(t, `SELECT extract(hour FROM t),extract(second FROM t),extract(timezone FROM t),extract(timezone_hour FROM t),extract(timezone_minute FROM t),extract(epoch FROM t),date_part('timezone_minute',t) FROM (VALUES(to_timestamp(946684800.123456))) v(t)`, nil, options)
	env := statefulEnvironment(t)
	env.Builtin = options
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	numeric := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }
	assertAnyValueRows(t, portal, []model.TypeID{1700, 1700, 1700, 1700, 1700, 1700, 701}, [][]model.Value{{numeric("5"), numeric("0.123456"), numeric("20700"), numeric("5"), numeric("45"), numeric("946684800.123456"), model.Float64Val(45)}})
	aggregateProjectionReleased(t, env.Query)
}
