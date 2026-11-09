package zpgparse_test

import (
	"testing"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// rowtypePlPgSQLTests pins the datum list for %ROWTYPE declarations. Upstream
// keeps the declaration a scalar variable carrying the "%rowtype" type name --
// the relation is resolved when the routine runs, never while parsing -- and
// the INTO/FOR machinery builds an unnamed row that references the variable.
// RECORD declarations keep their record datum and record-field datums, and a
// field of a %rowtype variable is only materialized as a datum when it is an
// assignment target. Every expectation except the field target is byte-identical
// to upstream libpg_query output.
var rowtypePlPgSQLTests = []struct {
	input        string
	expectedJSON string
}{
	{
		`do $$ declare x foo%rowtype; begin end $$;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"x","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"foo%rowtype"}}}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_return":{}}]}}}}
]`,
	},
	{
		`create function f() returns text as $$ declare r t%rowtype; begin select into r * from t; return r.b; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"r","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"t%rowtype"}}}},{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"r","varno":1}]}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_execsql":{"lineno":1,"sqlstmt":{"PLpgSQL_expr":{"query":"select        * from t","parseMode":0}},"into":true,"target":{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"r","varno":1}]}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"r.b","parseMode":2}}}}]}}}}
]`,
	},
	{
		`create function f() returns int as $$ declare r t%rowtype; begin for r in select * from t loop return 1; end loop; return 2; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"r","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"t%rowtype"}}}},{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"r","varno":1}]}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_fors":{"lineno":1,"var":{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"r","varno":1}]}},"body":[{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"1","parseMode":2}}}}],"query":{"PLpgSQL_expr":{"query":"select * from t","parseMode":0}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"2","parseMode":2}}}}]}}}}
]`,
	},
	{
		`create function f() returns int as $$ declare a int; r t%rowtype; begin select into a, r from t; return 1; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"a","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"int4"}}}},{"PLpgSQL_var":{"refname":"r","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"t%rowtype"}}}},{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"a","varno":1},{"name":"r","varno":2}]}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_execsql":{"lineno":1,"sqlstmt":{"PLpgSQL_expr":{"query":"select           from t","parseMode":0}},"into":true,"target":{"PLpgSQL_row":{"refname":"(unnamed row)","lineno":1,"fields":[{"name":"a","varno":1},{"name":"r","varno":2}]}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"1","parseMode":2}}}}]}}}}
]`,
	},
	{
		`create function f() returns text as $$ declare r record; begin select into r * from t; return r.b; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_rec":{"refname":"r","dno":1,"lineno":1}},{"PLpgSQL_recfield":{"fieldname":"b","recparentno":1}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_execsql":{"lineno":1,"sqlstmt":{"PLpgSQL_expr":{"query":"select        * from t","parseMode":0}},"into":true,"target":{"PLpgSQL_rec":{"refname":"r","dno":1,"lineno":1}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"r.b","parseMode":2}}}}]}}}}
]`,
	},
	{
		`create function f() returns int as $$ declare r t%rowtype; begin r := ROW(1); return 1; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"r","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"t%rowtype"}}}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_assign":{"lineno":1,"varno":1,"expr":{"PLpgSQL_expr":{"query":"r := ROW(1)","parseMode":3}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"1","parseMode":2}}}}]}}}}
]`,
	},
	{
		`create function f() returns int as $$ declare r t%rowtype; begin r.a := 1; return r.a; end $$ language plpgsql;`,
		`[
{"PLpgSQL_function":{"datums":[{"PLpgSQL_var":{"refname":"found","datatype":{"PLpgSQL_type":{"typname":"bool"}}}},{"PLpgSQL_var":{"refname":"r","lineno":1,"datatype":{"PLpgSQL_type":{"typname":"t%rowtype"}}}},{"PLpgSQL_recfield":{"fieldname":"a","recparentno":1}}],"action":{"PLpgSQL_stmt_block":{"lineno":1,"body":[{"PLpgSQL_stmt_assign":{"lineno":1,"varno":2,"expr":{"PLpgSQL_expr":{"query":"r.a := 1","parseMode":4}}}},{"PLpgSQL_stmt_return":{"lineno":1,"expr":{"PLpgSQL_expr":{"query":"r.a","parseMode":2}}}}]}}}}
]`,
	},
}

func TestParsePlPgSQLRowtype(t *testing.T) {
	for _, test := range rowtypePlPgSQLTests {
		actualJSON, err := pg_query.ParsePlPgSqlToJSON(test.input)
		if err != nil {
			t.Errorf("ParsePlPgSqlToJSON(%s)\nerror %s\n\n", test.input, err)
		} else if actualJSON != test.expectedJSON {
			t.Errorf("ParsePlPgSqlToJSON(%s)\nexpected %s\nactual %s\n\n", test.input, test.expectedJSON, actualJSON)
		}
	}
}
