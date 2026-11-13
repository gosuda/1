package modify_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestParsedInsertSelectUsesDestinationParameterType(t *testing.T) {
	s := newStore(t)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) SELECT 1,$1 RETURNING value`, func(environment *exec.Environment) {
		environment.Parameters = []expr.ParameterValue{{Number: 1, Layout: integer, Value: model.Int64Val(73)}}
	})
	want := [][]model.Value{{model.Int64Val(73)}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("typed INSERT SELECT rows %v, want %v", got, want)
	}
	s.require(t, 1, 73)
}
