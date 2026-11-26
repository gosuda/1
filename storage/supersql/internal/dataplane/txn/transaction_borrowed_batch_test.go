package txn

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedWoundPreservesOutstandingNestedBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		target := storage.ObjectRef{ObjectID: 2, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		integer := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
		policy := contractPolicy(t, target,
			ColumnRule{Layout: storage.ColumnLayout{ID: 1, Type: integer.Type}, KeyPosition: 1},
			ColumnRule{Layout: storage.ColumnLayout{ID: 2, Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}},
		)
		options := contractOptions(t, policy)
		older := contractBegin(t, fixture.manager, options)
		younger := contractBegin(t, fixture.manager, options)
		array := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}
		contractApply(t, younger, storage.NewInsertMutation(target, contractKey(1), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(1), array}))
		view := contractView(t, younger, target, contractSpan(1, 2))
		plan, err := view.PlanScan(t.Context(), storage.PushdownRequest{Target: target, Layout: policy.Layout().Columns, KeyLayout: contractKeyLayout(), ColumnIDs: []model.ColumnID{2}, Spans: []storage.KeySpan{contractSpan(1, 2)}})
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := view.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		})
		batch := columnar.Batch{MaxRows: 1}
		if err := cursor.Next(t.Context(), &batch); err != nil {
			t.Fatal(err)
		}
		finished := make(chan error, 1)
		go func() {
			_, err := older.Get(t.Context(), storage.ReadRequest{Target: target, Key: contractKey(1), ColumnIDs: []model.ColumnID{1, 2}, Layout: policy.Layout().Columns})
			finished <- err
		}()
		if err := <-finished; !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("wounded insertion remained visible: %v", err)
		}
		if younger.State() != Aborted {
			t.Fatalf("holder did not finish abort before lock transfer: %v", younger.State())
		}
		if err := batch.Validate(1); err != nil {
			t.Fatalf("asynchronous wound invalidated published child headers: %v", err)
		}
		value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(0), 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(value.ArrayVal) != 2 || value.ArrayVal[0].IntVal != 7 || !value.ArrayVal[1].IsNull {
			t.Fatalf("wound changed outstanding nested batch: %+v", value)
		}
	})
}
