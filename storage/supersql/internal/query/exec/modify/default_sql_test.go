package modify_test

import (
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func defaultSQLRows(t *testing.T, cursor storage.BatchCursor) [][]model.Value {
	t.Helper()
	var result [][]model.Value
	batch := columnar.Batch{MaxRows: 1}
	defer batch.Reset()
	for {
		err := cursor.Next(t.Context(), &batch)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range batch.Rows() {
			values := make([]model.Value, len(batch.Columns))
			for column := range batch.Columns {
				value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(row), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				values[column] = value
			}
			result = append(result, values)
		}
		batch.Reset()
	}
}

func TestParsedInsertUsesDefaultForEachRequestedRow(t *testing.T) {
	value := model.Int64Val(7)
	s := newStoreWithDefault(t, &value)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES (1,DEFAULT),(2,20),(3,DEFAULT) RETURNING id,value`)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(7)}, {model.Int64Val(2), model.Int64Val(20)}, {model.Int64Val(3), model.Int64Val(7)}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("DEFAULT rows %v, want %v", got, want)
	}
	if portal.RowsAffected() != 3 {
		t.Fatalf("affected rows %d", portal.RowsAffected())
	}
	s.require(t, 1, 7)
	s.require(t, 2, 20)
	s.require(t, 3, 7)
}

func TestParsedUpdateAssignsColumnDefault(t *testing.T) {
	value := model.Int64Val(7)
	s := newStoreWithDefault(t, &value)
	s.seed(t, row(1, 50))
	portal := parsedMutation(t, s, `UPDATE items SET value=DEFAULT WHERE id=1 RETURNING old.value,new.value`)
	want := [][]model.Value{{model.Int64Val(50), model.Int64Val(7)}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("default assignment %v, want %v", got, want)
	}
	s.require(t, 1, 7)
}

func TestParsedInsertValuesCoerceToTargetBeforeCombiningRows(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	s := newStoreWithValueLayout(t, text, nil)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES (1,1),(2,'word') RETURNING value`)
	want := [][]model.Value{{model.StringVal("1")}, {model.StringVal("word")}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("destination-typed values %v, want %v", got, want)
	}
}

func TestParsedInsertValuesAllowIndependentExplicitCollations(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	s := newStoreWithValueLayout(t, text, nil)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES (1,'a' COLLATE "C"),(2,'b' COLLATE "POSIX") RETURNING value`)
	want := [][]model.Value{{model.StringVal("a")}, {model.StringVal("b")}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("independent input collations %v, want %v", got, want)
	}
}
