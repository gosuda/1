package bind_test

import "testing"

func TestNamedFunctionArgumentsRejectAmbiguousSyntax(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"duplicate name", `SELECT make_interval(days=>1,days=>2)`, "42601"},
		{"positional after name", `SELECT make_interval(days=>1,2)`, "42601"},
		{"unknown name", `SELECT make_interval(missing=>1)`, "42883"},
		{"quoted name case", `SELECT make_interval("DAYS"=>1)`, "42883"},
		{"literal error precedes notation check", `SELECT make_interval(days=>'bad'::int,2)`, "22P02"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := bindSQL(t, tc.sql, nil, nil); requireCode(t, err, tc.code) })
	}
}
