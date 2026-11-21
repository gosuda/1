package modify_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func requireCharacterValue(t *testing.T, s *store, id int64, want string) {
	t.Helper()
	row, err := s.tx.Get(t.Context(), s.request(t, id))
	if err != nil {
		t.Fatal(err)
	}
	if len(row.Values) != 2 || row.Values[1].IsNull || row.Values[1].StrVal != want {
		t.Fatalf("row %d=%v, want value %q", id, row.Values, want)
	}
}

func TestParsedCharacterAssignmentFailureRestoresStatement(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 7, Collation: 100, Storage: model.TypeString}}
	s := newStoreWithValueLayout(t, layout, nil)
	s.seed(t, []model.Value{model.Int64Val(9), model.StringVal("old")})
	portal := parsedMutation(t, s, `INSERT INTO items SELECT n, CASE WHEN n=1 THEN 'a' ELSE 'toolong' END FROM generate_series(1,2)t(n) RETURNING value`)
	t.Cleanup(func() {
		if err := portal.Close(); err != nil {
			t.Error(err)
		}
	})
	var batch columnar.Batch
	err := portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22001" {
		t.Fatalf("assignment error=%v, want character length violation", err)
	}
	if batch.Rows() != 0 || portal.RowsAffected() != 0 {
		t.Fatalf("failed statement returned rows=%d affected=%d", batch.Rows(), portal.RowsAffected())
	}
	s.absent(t, 1)
	s.absent(t, 2)
	requireCharacterValue(t, s, 9, "old")
	s.seed(t, []model.Value{model.Int64Val(10), model.StringVal("new")})
	requireCharacterValue(t, s, 10, "new")
	requireClean(t, s)
}

func TestParsedCharacterAssignmentPreservesAllowedTruncation(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 7, Collation: 100, Storage: model.TypeString}}
	s := newStoreWithValueLayout(t, layout, nil)
	portal := parsedMutation(t, s, `INSERT INTO items VALUES (1,'toolong'::varchar(3)),(2,'ab   '),(3,'한글a') RETURNING value`)
	t.Cleanup(func() {
		if err := portal.Close(); err != nil {
			t.Error(err)
		}
	})
	want := [][]model.Value{{model.StringVal("too")}, {model.StringVal("ab ")}, {model.StringVal("한글a")}}
	if got := returned(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("assignment values=%v, want %v", got, want)
	}
	if portal.RowsAffected() != 3 {
		t.Fatalf("affected=%d, want 3", portal.RowsAffected())
	}
	requireCharacterValue(t, s, 1, "too")
	requireCharacterValue(t, s, 2, "ab ")
	requireCharacterValue(t, s, 3, "한글a")
	requireClean(t, s)
}
