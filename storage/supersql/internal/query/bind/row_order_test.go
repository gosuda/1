package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func TestRowOrderingNormalizesInputsBeforeRuntimeShortCircuit(t *testing.T) {
	for _, tc := range []struct{ sql, state string }{
		{`SELECT ROW(1,1/0)<ROW(2,3)`, "22012"},
		{`SELECT ROW(1,('b'||'ad')::integer)<ROW(1/0,2)`, "22P02"},
	} {
		statement, err := bindSQL(t, tc.sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = normalize.Normalize(t.Context(), statement.Graph(), normalize.Options{Limits: bindingOptions(tc.sql).Limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}})
		requireCode(t, err, tc.state)
	}
}

func TestRowComparisonsRequireBooleanAndFamilyInterpretation(t *testing.T) {
	for _, tc := range []struct{ sql, state string }{
		{`SELECT ROW(1,2)+ROW(3,4)`, "42804"},
		{`SELECT ROW(1)+ROW(2)`, "42804"},
		{`SELECT ROW(ARRAY[1],ARRAY[2]) @> ROW(ARRAY[1],ARRAY[2])`, "0A000"},
		{`SELECT ROW(1,2)<ROW(3)`, "42601"},
		{`SELECT ROW()<ROW()`, "0A000"},
	} {
		_, err := bindSQL(t, tc.sql, nil, nil)
		requireCode(t, err, tc.state)
	}
}
