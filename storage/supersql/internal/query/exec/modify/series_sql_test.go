package modify_test

import (
	"cmp"
	"reflect"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestParsedInsertSelectGenerateSeries(t *testing.T) {
	s := newStore(t)
	portal := parsedMutation(t, s, `INSERT INTO items(id,value)
		SELECT n,n*10 FROM generate_series(1,3) AS g(n) RETURNING id,value`)
	got := defaultSQLRows(t, portal)
	slices.SortFunc(got, func(a, b []model.Value) int { return cmp.Compare(a[0].IntVal, b[0].IntVal) })
	want := [][]model.Value{row(1, 10), row(2, 20), row(3, 30)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("series insert rows=%v, want %v", got, want)
	}
	for id := int64(1); id <= 3; id++ {
		s.require(t, id, id*10)
	}
}
