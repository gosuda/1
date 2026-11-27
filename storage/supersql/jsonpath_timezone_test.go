package supersql

import (
	"database/sql"
	"testing"
)

// These outcomes were frozen against PostgreSQL 18.4 in UTC and New York.
// They pin session-zone conversion and its non-suppressible timezone guard.
func TestJSONPathTimestampTimezoneConversions(t *testing.T) {
	ctx := t.Context()
	db := sql.OpenDB(mustConnector(t, ctx))
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cases := []struct {
		name, zone, query, state string
		rows                     [][]string
	}{
		{"UTC/array-tz-silent-false", "UTC", "SELECT jsonb_path_query_array_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false)::text", "", [][]string{{"[\"2023-08-15T12:34:56+00:00\"]"}}},
		{"UTC/rows-tz-silent-false", "UTC", "SELECT value::text FROM jsonb_path_query_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false) AS q(value)", "", [][]string{{"\"2023-08-15T12:34:56+00:00\""}}},
		{"UTC/comparison-tz-silent-false", "UTC", "SELECT jsonb_path_match_tz('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, false)::text", "", [][]string{{"false"}}},
		{"UTC/array-plain-silent-false", "UTC", "SELECT jsonb_path_query_array('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false)::text", "0A000", nil},
		{"UTC/rows-plain-silent-false", "UTC", "SELECT value::text FROM jsonb_path_query('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false) AS q(value)", "0A000", nil},
		{"UTC/comparison-plain-silent-false", "UTC", "SELECT jsonb_path_match('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, false)::text", "0A000", nil},
		{"UTC/array-tz-silent-true", "UTC", "SELECT jsonb_path_query_array_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true)::text", "", [][]string{{"[\"2023-08-15T12:34:56+00:00\"]"}}},
		{"UTC/rows-tz-silent-true", "UTC", "SELECT value::text FROM jsonb_path_query_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true) AS q(value)", "", [][]string{{"\"2023-08-15T12:34:56+00:00\""}}},
		{"UTC/comparison-tz-silent-true", "UTC", "SELECT jsonb_path_match_tz('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, true)::text", "", [][]string{{"false"}}},
		{"UTC/array-plain-silent-true", "UTC", "SELECT jsonb_path_query_array('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true)::text", "0A000", nil},
		{"UTC/rows-plain-silent-true", "UTC", "SELECT value::text FROM jsonb_path_query('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true) AS q(value)", "0A000", nil},
		{"UTC/comparison-plain-silent-true", "UTC", "SELECT jsonb_path_match('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, true)::text", "0A000", nil},
		{"America/New_York/array-tz-silent-false", "America/New_York", "SELECT jsonb_path_query_array_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false)::text", "", [][]string{{"[\"2023-08-15T12:34:56-04:00\"]"}}},
		{"America/New_York/rows-tz-silent-false", "America/New_York", "SELECT value::text FROM jsonb_path_query_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false) AS q(value)", "", [][]string{{"\"2023-08-15T12:34:56-04:00\""}}},
		{"America/New_York/comparison-tz-silent-false", "America/New_York", "SELECT jsonb_path_match_tz('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, false)::text", "", [][]string{{"true"}}},
		{"America/New_York/array-plain-silent-false", "America/New_York", "SELECT jsonb_path_query_array('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false)::text", "0A000", nil},
		{"America/New_York/rows-plain-silent-false", "America/New_York", "SELECT value::text FROM jsonb_path_query('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, false) AS q(value)", "0A000", nil},
		{"America/New_York/comparison-plain-silent-false", "America/New_York", "SELECT jsonb_path_match('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, false)::text", "0A000", nil},
		{"America/New_York/array-tz-silent-true", "America/New_York", "SELECT jsonb_path_query_array_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true)::text", "", [][]string{{"[\"2023-08-15T12:34:56-04:00\"]"}}},
		{"America/New_York/rows-tz-silent-true", "America/New_York", "SELECT value::text FROM jsonb_path_query_tz('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true) AS q(value)", "", [][]string{{"\"2023-08-15T12:34:56-04:00\""}}},
		{"America/New_York/comparison-tz-silent-true", "America/New_York", "SELECT jsonb_path_match_tz('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, true)::text", "", [][]string{{"true"}}},
		{"America/New_York/array-plain-silent-true", "America/New_York", "SELECT jsonb_path_query_array('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true)::text", "0A000", nil},
		{"America/New_York/rows-plain-silent-true", "America/New_York", "SELECT value::text FROM jsonb_path_query('\"2023-08-15 12:34:56\"'::jsonb, '$.timestamp_tz().string()', '{}'::jsonb, true) AS q(value)", "0A000", nil},
		{"America/New_York/comparison-plain-silent-true", "America/New_York", "SELECT jsonb_path_match('{\"local\":\"2023-08-15 12:34:56\",\"zoned\":\"2023-08-15 16:34:56+00\"}'::jsonb, '$.local.timestamp() == $.zoned.timestamp_tz()', '{}'::jsonb, true)::text", "0A000", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := conn.ExecContext(ctx, "SET TimeZone = '"+tc.zone+"'"); err != nil {
				t.Fatal(err)
			}
			got := runDiffStep(ctx, conn, diffStep{sql: tc.query, query: true, ordered: true})
			want := diffOutcome{state: tc.state, columns: 1, rows: tc.rows}
			if reason := compareDiffOutcome(want, got, true); reason != "" {
				t.Fatalf("%s: %s; want=%s got=%s", tc.query, reason, want.summary(), got.summary())
			}
		})
	}
}
