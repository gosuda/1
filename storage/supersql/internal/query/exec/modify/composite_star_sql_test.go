package modify_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestParsedUpdateCompositeStarReturnsOldAndNewFields(t *testing.T) {
	s := newStore(t)
	s.seed(t, []model.Value{model.Int64Val(1), model.Int64Val(10)})
	portal := parsedMutation(t, s, `UPDATE items SET value=value+3 RETURNING (ROW(old.value,new.value)).*`)
	t.Cleanup(func() {
		if err := portal.Close(); err != nil {
			t.Error(err)
		}
	})
	want := [][]model.Value{{model.Int64Val(10), model.Int64Val(13)}}
	if got := returned(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("returned=%v, want %v", got, want)
	}
	if portal.RowsAffected() != 1 {
		t.Fatalf("affected=%d", portal.RowsAffected())
	}
	requireClean(t, s)
}
