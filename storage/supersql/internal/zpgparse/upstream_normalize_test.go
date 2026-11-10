package zpgparse_test

import (
	"reflect"
	"testing"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse"
	"github.com/gosuda/supersql-alpha/internal/zpgparse/parser"
)

var normalizeTests = []struct {
	input    string
	expected string
}{
	{
		"SELECT 1",
		"SELECT $1",
	},
}

func TestNormalize(t *testing.T) {
	for _, test := range normalizeTests {
		actual, err := pg_query.Normalize(test.input)

		if err != nil {
			t.Errorf("Normalize(%s)\nerror %s\n\n", test.input, err)
		} else if !reflect.DeepEqual(actual, test.expected) {
			t.Errorf("Normalize(%s)\nexpected %s\nactual %s\n\n", test.input, test.expected, actual)
		}
	}
}

var normalizeErrorTests = []struct {
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

func TestNormalizeError(t *testing.T) {
	for _, test := range normalizeErrorTests {
		_, actualErr := pg_query.Normalize(test.input)

		if actualErr == nil {
			t.Errorf("Normalize(%s)\nexpected error but none returned\n\n", test.input)
		} else {
			requireParserError(t, actualErr.(*parser.Error), test.expectedErr.(*parser.Error))
		}
	}
}

var normalizeUtilityTests = []struct {
	input    string
	expected string
}{
	{
		"SELECT 1",
		"SELECT 1",
	},
	{
		"CREATE ROLE postgres PASSWORD 'xyz'",
		"CREATE ROLE postgres PASSWORD $1",
	},
}

func TestNormalizeUtility(t *testing.T) {
	for _, test := range normalizeUtilityTests {
		actual, err := pg_query.NormalizeUtility(test.input)

		if err != nil {
			t.Errorf("Normalize(%s)\nerror %s\n\n", test.input, err)
		} else if !reflect.DeepEqual(actual, test.expected) {
			t.Errorf("Normalize(%s)\nexpected %s\nactual %s\n\n", test.input, test.expected, actual)
		}
	}
}
