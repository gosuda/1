package modify_test

import (
	"cmp"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestParsedInsertReturningAliasesExposeNullOldImage(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(9, 90))
	portal := parsedMutation(t, s, `INSERT INTO public.items AS i (id,value) VALUES (1,10),(2,20)
		RETURNING WITH (OLD AS o, NEW AS n) o.id,o.value,n.*,i.value,*`)
	want := [][]model.Value{
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.Int64Val(1), model.Int64Val(10), model.Int64Val(10), model.Int64Val(1), model.Int64Val(10)},
		{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.Int64Val(2), model.Int64Val(20), model.Int64Val(20), model.Int64Val(2), model.Int64Val(20)},
	}
	got := defaultSQLRows(t, portal)
	slices.SortFunc(got, func(a, b []model.Value) int { return cmp.Compare(a[2].IntVal, b[2].IntVal) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("INSERT RETURNING = %v, want %v", got, want)
	}
	s.require(t, 1, 10)
	s.require(t, 2, 20)
	s.require(t, 9, 90)
}

func TestParsedUpdateReturningAliasesKeepBeforeAndAfterImages(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(9, 90))
	portal := parsedMutation(t, s, `UPDATE public.items AS i SET id=id+10,value=value+1 WHERE id<9
		RETURNING WITH (OLD AS o, NEW AS n) o.*,n.id,n.value,i.*,value`)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10), model.Int64Val(11), model.Int64Val(11), model.Int64Val(11), model.Int64Val(11), model.Int64Val(11)},
		{model.Int64Val(2), model.Int64Val(20), model.Int64Val(12), model.Int64Val(21), model.Int64Val(12), model.Int64Val(21), model.Int64Val(21)},
	}
	got := defaultSQLRows(t, portal)
	slices.SortFunc(got, func(a, b []model.Value) int { return cmp.Compare(a[0].IntVal, b[0].IntVal) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UPDATE RETURNING = %v, want %v", got, want)
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 11, 11)
	s.require(t, 12, 21)
	s.require(t, 9, 90)
}

func TestParsedDeleteReturningAliasesExposeNullNewImage(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(9, 90))
	portal := parsedMutation(t, s, `DELETE FROM public.items AS i WHERE id<9
		RETURNING WITH (OLD AS o, NEW AS n) o.*,n.*,i.id,*`)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.Int64Val(1), model.Int64Val(1), model.Int64Val(10)},
		{model.Int64Val(2), model.Int64Val(20), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.Int64Val(2), model.Int64Val(2), model.Int64Val(20)},
	}
	got := defaultSQLRows(t, portal)
	slices.SortFunc(got, func(a, b []model.Value) int { return cmp.Compare(a[0].IntVal, b[0].IntVal) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DELETE RETURNING = %v, want %v", got, want)
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 9, 90)
}

func TestParsedReturningResolvesSchemaQualifiedTarget(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		sql      string
		want     []model.Value
		retained [][2]int64
		absent   []int64
	}{
		{
			name:     "insert",
			sql:      `INSERT INTO items (id,value) VALUES (2,20) RETURNING public.items.value,public.items.*`,
			want:     []model.Value{model.Int64Val(20), model.Int64Val(2), model.Int64Val(20)},
			retained: [][2]int64{{1, 10}, {2, 20}},
		},
		{
			name:     "update",
			sql:      `UPDATE items SET value=20 WHERE id=1 RETURNING public.items.value,public.items.*`,
			want:     []model.Value{model.Int64Val(20), model.Int64Val(1), model.Int64Val(20)},
			retained: [][2]int64{{1, 20}},
		},
		{
			name:   "delete",
			sql:    `DELETE FROM items WHERE id=1 RETURNING public.items.value,public.items.*`,
			want:   []model.Value{model.Int64Val(10), model.Int64Val(1), model.Int64Val(10)},
			absent: []int64{1},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(9, 90))
			portal := parsedMutation(t, s, scenario.sql)
			want := [][]model.Value{scenario.want}
			if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("schema-qualified RETURNING = %v, want %v", got, want)
			}
			for _, row := range scenario.retained {
				s.require(t, row[0], row[1])
			}
			for _, id := range scenario.absent {
				s.absent(t, id)
			}
			s.require(t, 9, 90)
		})
	}
}

func TestParsedReturningExplicitAliasOverridesOppositeDefaultName(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
		want []model.Value
	}{
		{
			name: "old named new",
			sql: `UPDATE items SET id=2,value=30 WHERE id=1
				RETURNING WITH (OLD AS new) new.*,new.value,*`,
			want: []model.Value{model.Int64Val(1), model.Int64Val(10), model.Int64Val(10), model.Int64Val(2), model.Int64Val(30)},
		},
		{
			name: "new named old",
			sql: `UPDATE items SET id=2,value=30 WHERE id=1
				RETURNING WITH (NEW AS old) old.*,old.value,*`,
			want: []model.Value{model.Int64Val(2), model.Int64Val(30), model.Int64Val(30), model.Int64Val(2), model.Int64Val(30)},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(9, 90))
			portal := parsedMutation(t, s, scenario.sql)
			want := [][]model.Value{scenario.want}
			if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("opposite-name RETURNING = %v, want %v", got, want)
			}
			s.absent(t, 1)
			s.require(t, 2, 30)
			s.require(t, 9, 90)
		})
	}
}

func TestParsedReturningAliasFailureRestoresStatementWrites(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(9, 90))
	portal := parsedMutation(t, s, `UPDATE items AS i SET id=id+10,value=value+1 WHERE id<9
		RETURNING WITH (OLD AS o, NEW AS n) o.id,n.value/(2-o.id)`)
	batch := columnar.Batch{MaxRows: 1}
	defer batch.Reset()
	err := portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
		t.Fatalf("RETURNING error = %v, want SQLSTATE 22012", err)
	}
	if batch.Rows() != 0 || portal.RowsAffected() != 0 {
		t.Fatalf("failed RETURNING published %d rows, affected=%d", batch.Rows(), portal.RowsAffected())
	}
	s.require(t, 1, 10)
	s.require(t, 2, 20)
	s.require(t, 9, 90)
	s.absent(t, 11)
	s.absent(t, 12)
}
