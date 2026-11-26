package zpgparse_test

import (
	"reflect"
	"testing"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse"
	"github.com/gosuda/supersql-alpha/internal/zpgparse/parser"
)

var isUtilityStmtTests = []struct {
	input    string
	expected []bool
}{
	// DML statements (not utility)
	{
		"SELECT 1",
		[]bool{false},
	},
	{
		"INSERT INTO t (a) VALUES (1)",
		[]bool{false},
	},
	{
		"UPDATE t SET a = 1",
		[]bool{false},
	},
	{
		"DELETE FROM t",
		[]bool{false},
	},
	// Utility statements
	{
		"SHOW fsync",
		[]bool{true},
	},
	{
		"SET fsync = off",
		[]bool{true},
	},
	{
		"CREATE TABLE t (a int)",
		[]bool{true},
	},
	{
		"DROP TABLE t",
		[]bool{true},
	},
	// Multi-statement input
	{
		"SELECT 1; SELECT 2",
		[]bool{false, false},
	},
	{
		"SELECT 1; SHOW fsync",
		[]bool{false, true},
	},
	{
		"SHOW fsync; SELECT 1",
		[]bool{true, false},
	},
	{
		"SET a = 1; SET b = 2",
		[]bool{true, true},
	},
}

func TestIsUtilityStmt(t *testing.T) {
	for _, test := range isUtilityStmtTests {
		actual, err := pg_query.IsUtilityStmt(test.input)

		if err != nil {
			t.Errorf("IsUtilityStmt(%s)\nerror %s\n\n", test.input, err)
		} else if !reflect.DeepEqual(actual, test.expected) {
			t.Errorf("IsUtilityStmt(%s)\nexpected %v\nactual %v\n\n", test.input, test.expected, actual)
		}
	}
}

var isUtilityStmtErrorTests = []struct {
	input       string
	expectedErr error
}{
	{
		"SELECT $",
		&parser.Error{
			Code:      "42601",
			Message:   "syntax error at or near \"$\"",
			Cursorpos: 8,
			Filename:  "scan.l",
			Funcname:  "scanner_yyerror",
			Location:  7,
		},
	},
}

func TestIsUtilityStmtError(t *testing.T) {
	for _, test := range isUtilityStmtErrorTests {
		_, actualErr := pg_query.IsUtilityStmt(test.input)

		if actualErr == nil {
			t.Errorf("IsUtilityStmt(%s)\nexpected error but none returned\n\n", test.input)
		} else {
			requireParserError(t, actualErr.(*parser.Error), test.expectedErr.(*parser.Error))
		}
	}
}
