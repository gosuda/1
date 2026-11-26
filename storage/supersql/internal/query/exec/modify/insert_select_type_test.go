package modify_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestParsedInsertSelectInfersUnknownLiteralFromDestination(t *testing.T) {
	s := newStore(t)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) SELECT 1,'42' RETURNING id,value`)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(42)}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("INSERT SELECT rows %v, want %v", got, want)
	}
	s.require(t, 1, 42)
}

func TestParsedInsertSelectNullReachesColumnConstraint(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(9, 90))
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) SELECT 1,NULL RETURNING id`)
	batch := columnar.Batch{MaxRows: 1}
	err := portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "23502" {
		t.Fatalf("untyped SELECT NULL: %v", err)
	}
	s.absent(t, 1)
	s.require(t, 9, 90)
}
