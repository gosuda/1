package modify_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestExplicitNullDoesNotUseDefaultOrLeaveEarlierRows(t *testing.T) {
	value := model.Int64Val(7)
	s := newStoreWithDefault(t, &value)
	s.seed(t, row(9, 90))
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES (1,DEFAULT),(2,NULL) RETURNING id`)
	batch := columnar.Batch{MaxRows: 1}
	err := portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "23502" {
		t.Fatalf("explicit NULL with a default: %v", err)
	}
	if batch.Rows() != 0 || portal.RowsAffected() != 0 {
		t.Fatal("failed statement published rows")
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 9, 90)
}
