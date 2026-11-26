package txn

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type normalizerConst model.Value

func (value normalizerConst) Evaluate(ctx context.Context, _ []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	result, _, err := model.CopyValue(ctx, model.Value(value), 1<<20)
	return result, err
}

type normalizerExpression func(context.Context, []model.Value) (model.Value, error)

func (expression normalizerExpression) Evaluate(ctx context.Context, values []model.Value, _ model.ExpressionRuntime) (model.Value, error) {
	return expression(ctx, values)
}

func normalizerTarget() storage.ObjectRef {
	return storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1}
}

func normalizerColumn(id model.ColumnID, kind model.DataType, nullable bool) ColumnRule {
	return ColumnRule{Layout: storage.ColumnLayout{ID: id, Type: model.Type{ID: model.TypeID(kind), Storage: kind}, Nullable: nullable}}
}

func requireNormalizer(t *testing.T, columns []ColumnRule, budget uint64) *RowNormalizer {
	t.Helper()
	normalizer, err := NewRowNormalizer(t.Context(), normalizerTarget(), columns, budget)
	if err != nil {
		t.Fatal(err)
	}
	return normalizer
}

func requireNormalizerState(t *testing.T, err error, code string) {
	t.Helper()
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func TestRowNormalizerCheckResults(t *testing.T) {
	for _, test := range []struct {
		name  string
		value model.Value
		code  string
	}{
		{"true", model.BoolVal(true), ""},
		{"null", model.NullVal(model.TypeBoolean), ""},
		{"false", model.BoolVal(false), "23514"},
		{"wrong_type", model.Int64Val(1), "42804"},
		{"invalid_boolean", model.Value{Type: model.TypeBoolean, IntVal: 2}, "42804"},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalizer, err := NewRowNormalizer(t.Context(), normalizerTarget(), []ColumnRule{normalizerColumn(1, model.TypeInt64, false)}, 8192, CheckRule{Name: "positive", Expression: normalizerConst(test.value)})
			if err != nil {
				t.Fatal(err)
			}
			row, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{model.Int64Val(7)}), nil)
			if test.code != "" {
				requireNormalizerState(t, err, test.code)
				return
			}
			if err != nil || row.Values[0].IntVal != 7 {
				t.Fatalf("accepted row = %+v, %v", row, err)
			}
		})
	}
}

func TestRowNormalizerCheckSeesCompletedRowBeforeDuplicate(t *testing.T) {
	columns := []ColumnRule{normalizerColumn(1, model.TypeInt64, false), normalizerColumn(2, model.TypeInt64, false)}
	columns[0].Default = normalizerConst(model.Int64Val(7))
	columns[1].Generated = normalizerExpression(func(_ context.Context, row []model.Value) (model.Value, error) {
		return model.Int64Val(row[0].IntVal * 2), nil
	})
	calls := 0
	checks := []CheckRule{
		{Name: "z_later", Expression: normalizerExpression(func(context.Context, []model.Value) (model.Value, error) {
			t.Fatal("later CHECK ran after a violation")
			return model.Value{}, nil
		})},
		{Name: "a_completed", Expression: normalizerExpression(func(_ context.Context, row []model.Value) (model.Value, error) {
			calls++
			return model.BoolVal(row[0].IntVal != 7 || row[1].IntVal != 14), nil
		})},
	}
	normalizer, err := NewRowNormalizer(t.Context(), normalizerTarget(), columns, 8192, checks...)
	if err != nil {
		t.Fatal(err)
	}
	checks[1].Expression = normalizerConst(model.BoolVal(true))
	previous := &model.Row{Key: []byte("row"), Values: []model.Value{model.Int64Val(2), model.Int64Val(4)}}
	_, err = normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), nil, nil), previous)
	requireNormalizerState(t, err, "23514")
	if calls != 1 || previous.Values[0].IntVal != 2 || previous.Values[1].IntVal != 4 {
		t.Fatalf("CHECK calls=%d, previous=%+v", calls, previous)
	}
	_, err = normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{model.NullVal(model.TypeInt64)}), previous)
	requireNormalizerState(t, err, "23502")
	if calls != 1 {
		t.Fatal("NOT NULL failure evaluated CHECK")
	}
}

func TestRowNormalizerRejectsInvalidCheckRules(t *testing.T) {
	valid := normalizerConst(model.BoolVal(true))
	for _, checks := range [][]CheckRule{
		{{Expression: valid}},
		{{Name: "missing"}},
		{{Name: "typed_nil", Expression: normalizerExpression(nil)}},
		{{Name: "duplicate", Expression: valid}, {Name: "duplicate", Expression: valid}},
	} {
		if _, err := NewRowNormalizer(t.Context(), normalizerTarget(), nil, 8192, checks...); !errors.Is(err, storage.ErrInvalidLayout) {
			t.Fatalf("invalid CHECK rules accepted: %v", err)
		}
	}
}

func TestRowNormalizerOmissionSemantics(t *testing.T) {
	columns := []ColumnRule{normalizerColumn(2, model.TypeInt64, false), normalizerColumn(5, model.TypeInt64, true), normalizerColumn(9, model.TypeInt64, true)}
	columns[0].Default = normalizerConst(model.Int64Val(10))
	columns[1].Default = normalizerConst(model.Int64Val(20))
	previous := &model.Row{Key: []byte("row"), Values: []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3)}}
	for _, test := range []struct {
		name       string
		op         storage.MutationOp
		previous   *model.Row
		wantSecond int64
		wantThird  model.Value
	}{
		{"insert_defaults", storage.MutationInsert, nil, 20, model.NullVal(model.TypeInt64)},
		{"update_preserves", storage.MutationUpdate, previous, 2, model.Int64Val(3)},
		{"upsert_existing_preserves", storage.MutationUpsert, previous, 2, model.Int64Val(3)},
		{"upsert_absent_defaults", storage.MutationUpsert, nil, 20, model.NullVal(model.TypeInt64)},
		{"replace_existing_resets", storage.MutationReplace, previous, 20, model.NullVal(model.TypeInt64)},
		{"replace_absent_defaults", storage.MutationReplace, nil, 20, model.NullVal(model.TypeInt64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalizer := requireNormalizer(t, columns, 1<<20)
			mutation := storage.Mutation{Op: test.op, Target: normalizerTarget(), Key: []byte("row"), Ordinal: 17, ColumnIDs: []model.ColumnID{2}, Values: []model.Value{model.Int64Val(8)}}
			result, err := normalizer.normalizeOwned(t.Context(), mutation, test.previous)
			if err != nil {
				t.Fatal(err)
			}
			if result.Op != storage.MutationUpsert || result.Target != mutation.Target || result.Ordinal != 17 || !bytes.Equal(result.Key, mutation.Key) {
				t.Fatalf("normalized identity = %+v", result)
			}
			if !reflect.DeepEqual(result.ColumnIDs, []model.ColumnID{2, 5, 9}) {
				t.Fatalf("columns = %v", result.ColumnIDs)
			}
			if result.Values[0].IntVal != 8 || result.Values[1].IntVal != test.wantSecond || result.Values[2].IsNull != test.wantThird.IsNull || result.Values[2].IntVal != test.wantThird.IntVal || result.Values[2].Type != model.TypeInt64 {
				t.Fatalf("completed values = %+v", result.Values)
			}
		})
	}
}

func TestRowNormalizerExplicitNullDoesNotEvaluateDefault(t *testing.T) {
	column := normalizerColumn(1, model.TypeInt64, true)
	column.Default = normalizerExpression(func(context.Context, []model.Value) (model.Value, error) {
		t.Fatal("explicit NULL evaluated the default")
		return model.Value{}, nil
	})
	normalizer := requireNormalizer(t, []ColumnRule{column}, 4096)
	result, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{model.NullVal(model.TypeInt64)}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Values[0].IsNull || result.Values[0].Type != model.TypeInt64 {
		t.Fatalf("explicit NULL = %+v", result.Values[0])
	}
}

func TestRowNormalizerGeneratedUsesCompletedOrdinaryColumns(t *testing.T) {
	columns := []ColumnRule{normalizerColumn(1, model.TypeInt64, false), normalizerColumn(3, model.TypeInt64, false), normalizerColumn(8, model.TypeInt64, false)}
	columns[0].Default = normalizerConst(model.Int64Val(7))
	columns[1].Generated = normalizerExpression(func(_ context.Context, values []model.Value) (model.Value, error) {
		if len(values) != 3 {
			t.Fatalf("storage column count = %d", len(values))
		}
		return model.Int64Val(values[0].IntVal*10 + values[2].IntVal), nil
	})
	normalizer := requireNormalizer(t, columns, 8192)
	result, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{8}, []model.Value{model.Int64Val(4)}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Values[1].IntVal != 74 {
		t.Fatalf("generated value = %d, want 74", result.Values[1].IntVal)
	}
	previous := &model.Row{Key: result.Key, Values: result.Values}
	updated, err := normalizer.normalizeOwned(t.Context(), storage.NewUpdateMutation(normalizerTarget(), []byte("row"), []model.ColumnID{8}, []model.Value{model.Int64Val(9)}), previous)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Values[1].IntVal != 79 {
		t.Fatalf("recomputed generated value = %d, want 79", updated.Values[1].IntVal)
	}
}

func TestRowNormalizerRejectsInvalidExplicitValuesBeforeDefaults(t *testing.T) {
	columns := []ColumnRule{normalizerColumn(1, model.TypeInt64, false), normalizerColumn(2, model.TypeInt64, false), normalizerColumn(3, model.TypeInt64, false)}
	columns[0].Default = normalizerExpression(func(context.Context, []model.Value) (model.Value, error) {
		t.Fatal("invalid explicit values evaluated an omitted default")
		return model.Value{}, nil
	})
	columns[2].Generated = normalizerConst(model.Int64Val(9))
	for _, test := range []struct {
		name  string
		id    model.ColumnID
		value model.Value
		code  string
	}{
		{"unknown", 4, model.Int64Val(1), "42703"},
		{"wrong_type", 2, model.StringVal("wrong"), "42804"},
		{"not_null", 2, model.NullVal(model.TypeInt64), "23502"},
		{"generated_override", 3, model.Int64Val(3), "428C9"},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalizer := requireNormalizer(t, columns, 8192)
			_, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{test.id}, []model.Value{test.value}), nil)
			requireNormalizerState(t, err, test.code)
		})
	}
}

func TestRowNormalizerExistenceAndRangeSemantics(t *testing.T) {
	normalizer := requireNormalizer(t, []ColumnRule{normalizerColumn(1, model.TypeInt64, true)}, 4096)
	previous := &model.Row{Key: []byte("row"), Values: []model.Value{model.Int64Val(1)}}
	_, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), nil, nil), previous)
	requireNormalizerState(t, err, "23505")
	_, err = normalizer.normalizeOwned(t.Context(), storage.NewUpdateMutation(normalizerTarget(), []byte("row"), nil, nil), nil)
	if !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("missing update = %v", err)
	}
	deleted, err := normalizer.normalizeOwned(t.Context(), storage.NewDeleteMutation(normalizerTarget(), []byte("row")), nil)
	if err != nil || deleted.Op != storage.MutationDelete {
		t.Fatalf("missing delete = %+v, %v", deleted, err)
	}
	rangeMutation := storage.NewDeleteRangeMutation(normalizerTarget(), []byte("a"), []byte("z"))
	rangeMutation.Ordinal = 4
	logical, err := normalizer.normalizeOwned(t.Context(), rangeMutation, nil)
	if err != nil || logical.Op != storage.MutationDeleteRange || logical.Ordinal != 4 || !bytes.Equal(logical.Key, []byte("a")) || !bytes.Equal(logical.EndKey, []byte("z")) {
		t.Fatalf("logical range = %+v, %v", logical, err)
	}
	_, err = normalizer.normalizeOwned(t.Context(), storage.NewDeleteRangeMutation(normalizerTarget(), []byte("z"), []byte("z")), nil)
	if !errors.Is(err, storage.ErrRangeInvalid) {
		t.Fatalf("empty range = %v", err)
	}
}

func TestRowNormalizerPreservesDeclaredNestedTypes(t *testing.T) {
	column := normalizerColumn(1, model.TypeArray, true)
	column.Layout.Children = []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}}
	normalizer := requireNormalizer(t, []ColumnRule{column}, 8192)
	for _, value := range []model.Value{model.NullVal(model.TypeArray), {Type: model.TypeArray}, {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeString)}}} {
		result, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{value}), nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Values[0].Type != model.TypeArray || normalizer.Layout().Columns[0].Children[0].Type.ID != 25 {
			t.Fatal("lost empty or NULL array's declared element type")
		}
	}
	wrong := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeInt64)}}
	_, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{wrong}), nil)
	requireNormalizerState(t, err, "42804")
}

func TestRowNormalizerFailureLeavesOwnedInputsUnchanged(t *testing.T) {
	failure := errors.New("generated evaluation rejected")
	columns := []ColumnRule{normalizerColumn(1, model.TypeBytes, false), normalizerColumn(2, model.TypeBytes, false), normalizerColumn(3, model.TypeInt64, false)}
	columns[2].Generated = normalizerExpression(func(context.Context, []model.Value) (model.Value, error) { return model.Value{}, failure })
	normalizer := requireNormalizer(t, columns, 8192)
	mutation := storage.NewUpdateMutation(normalizerTarget(), []byte("row"), []model.ColumnID{2, 1}, []model.Value{model.BytesVal([]byte("new2")), model.BytesVal([]byte("new1"))})
	previous := &model.Row{Key: []byte("row"), Values: []model.Value{model.BytesVal([]byte("old1")), model.BytesVal([]byte("old2")), model.Int64Val(9)}}
	_, err := normalizer.normalizeOwned(t.Context(), mutation, previous)
	if !errors.Is(err, failure) {
		t.Fatalf("evaluation cause = %v", err)
	}
	changedMutation := mutation.Op != storage.MutationUpdate || mutation.ColumnIDs[0] != 2
	changedPayload := string(mutation.Values[0].BytesVal) != "new2" || string(mutation.Values[1].BytesVal) != "new1"
	changedPrevious := string(previous.Values[0].BytesVal) != "old1" || previous.Values[2].IntVal != 9
	if changedMutation || changedPayload || changedPrevious {
		t.Fatal("failed normalization modified caller inputs")
	}
}

func TestRowNormalizerOwnsBoundedDescriptorCopies(t *testing.T) {
	column := normalizerColumn(1, model.TypeArray, true)
	children := make([]model.TypeLayout, 1, 1024)
	children[0] = model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes}}
	column.Layout.Children = children
	column.Layout.HasMissingValue = true
	column.Layout.MissingValue = model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.BytesVal([]byte("missing"))}}
	rules := []ColumnRule{column}
	normalizer := requireNormalizer(t, rules, 4096)
	children[0].Type.ID = 99
	column.Layout.MissingValue.ArrayVal[0].BytesVal[0] = 'X'
	rules[0].Layout.ID = 100
	layout := normalizer.Layout()
	if layout.Columns[0].ID != 1 || layout.Columns[0].Children[0].Type.ID != 17 || string(layout.Columns[0].MissingValue.ArrayVal[0].BytesVal) != "missing" || cap(layout.Columns[0].Children) != 1 {
		t.Fatalf("descriptor ownership lost: %+v", layout.Columns[0])
	}
	layout.Columns[0].Children[0].Type.ID = 123
	layout.Columns[0].MissingValue.ArrayVal[0].BytesVal[0] = 'Y'
	fresh := normalizer.Layout()
	if fresh.Columns[0].Children[0].Type.ID != 17 || string(fresh.Columns[0].MissingValue.ArrayVal[0].BytesVal) != "missing" {
		t.Fatal("Layout exposes mutable normalizer metadata")
	}
	result, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), nil, nil), nil)
	if err != nil || !result.Values[0].IsNull {
		t.Fatalf("read missing value became an insert default: %+v, %v", result, err)
	}
}

func TestRowNormalizerRejectsMetadataAndRowMemoryExcess(t *testing.T) {
	column := normalizerColumn(1, model.TypeBytes, true)
	if _, err := NewRowNormalizer(t.Context(), normalizerTarget(), []ColumnRule{column}, 1); !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("metadata budget = %v", err)
	}
	largeMissing := column
	largeMissing.Layout.HasMissingValue = true
	largeMissing.Layout.MissingValue = model.BytesVal(make([]byte, 8192))
	if _, err := NewRowNormalizer(t.Context(), normalizerTarget(), []ColumnRule{largeMissing}, 4096); !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("missing value budget = %v", err)
	}
	normalizer := requireNormalizer(t, []ColumnRule{column}, 4096)
	mutation := storage.Mutation{Op: storage.MutationInsert, Target: normalizerTarget(), Key: []byte("row"), ColumnIDs: []model.ColumnID{1}, Values: []model.Value{model.BytesVal(make([]byte, 1, 8192))}}
	if _, err := normalizer.normalizeOwned(t.Context(), mutation, nil); !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("retained payload budget = %v", err)
	}
	column.Default = normalizerExpression(func(context.Context, []model.Value) (model.Value, error) {
		return model.BytesVal(make([]byte, 1, 8192)), nil
	})
	normalizer = requireNormalizer(t, []ColumnRule{column}, 4096)
	if _, err := normalizer.normalizeOwned(t.Context(), storage.NewInsertMutation(normalizerTarget(), []byte("row"), nil, nil), nil); !errors.Is(err, ordered.ErrBudget) {
		t.Fatalf("evaluated payload budget = %v", err)
	}
}

func TestRowNormalizerRejectsWrongDescriptorsAndPreviousRows(t *testing.T) {
	column := normalizerColumn(1, model.TypeInt64, true)
	for _, columns := range [][]ColumnRule{{column, column}, {normalizerColumn(2, model.TypeInt64, true), column}, {{Layout: column.Layout, Default: normalizerConst(model.Int64Val(1)), Generated: normalizerConst(model.Int64Val(2))}}} {
		if _, err := NewRowNormalizer(t.Context(), normalizerTarget(), columns, 8192); !errors.Is(err, storage.ErrInvalidLayout) {
			t.Fatalf("invalid columns = %v", err)
		}
	}
	invalidTarget := normalizerTarget()
	invalidTarget.Generation = 0
	if _, err := NewRowNormalizer(t.Context(), invalidTarget, []ColumnRule{column}, 8192); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("invalid target = %v", err)
	}
	normalizer := requireNormalizer(t, []ColumnRule{column}, 8192)
	mutation := storage.NewUpdateMutation(normalizerTarget(), []byte("row"), nil, nil)
	for _, previous := range []*model.Row{{Key: []byte("other"), Values: []model.Value{model.Int64Val(1)}}, {Key: []byte("row")}, {Key: []byte("row"), Values: []model.Value{model.StringVal("wrong")}}, {Key: []byte("row"), Deleted: true, Values: []model.Value{model.Int64Val(1)}}} {
		if _, err := normalizer.normalizeOwned(t.Context(), mutation, previous); !errors.Is(err, storage.ErrInvalidLayout) {
			t.Fatalf("invalid previous row = %v", err)
		}
	}
	mutation.Target.DescriptorVersion++
	if _, err := normalizer.normalizeOwned(t.Context(), mutation, nil); !errors.Is(err, storage.ErrInvalidTarget) {
		t.Fatalf("descriptor mismatch = %v", err)
	}
}

func TestRowNormalizerWriteOnlyDefaultFillsExistingMissingValue(t *testing.T) {
	column := normalizerColumn(1, model.TypeInt64, true)
	column.Default = normalizerConst(model.Int64Val(7))
	column.FillMissingOnUpdate = true
	normalizer := requireNormalizer(t, []ColumnRule{column}, 8192)
	previous := &model.Row{Key: []byte("row"), Values: []model.Value{model.NullVal(model.TypeInt64)}}
	result, err := normalizer.normalizeOwned(t.Context(), storage.NewUpdateMutation(normalizerTarget(), []byte("row"), nil, nil), previous)
	if err != nil {
		t.Fatal(err)
	}
	if result.Values[0].IsNull || result.Values[0].Type != model.TypeInt64 || result.Values[0].IntVal != 7 {
		t.Fatalf("write-only default = %v, want 7", result.Values[0])
	}
	explicit := storage.NewUpdateMutation(normalizerTarget(), []byte("row"), []model.ColumnID{1}, []model.Value{model.NullVal(model.TypeInt64)})
	result, err = normalizer.normalizeOwned(t.Context(), explicit, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Values[0].IsNull {
		t.Fatalf("explicit NULL was replaced: %v", result.Values[0])
	}
}
