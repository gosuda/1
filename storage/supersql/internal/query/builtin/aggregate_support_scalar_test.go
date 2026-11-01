package builtin

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPostgreSQLFDWValidatorUsesCatalogContext(t *testing.T) {
	options := func(values ...string) model.Value {
		result := model.Value{Type: model.TypeArray, ArrayVal: make([]model.Value, len(values))}
		for index, value := range values {
			result.ArrayVal[index] = model.StringVal(value)
		}
		return result
	}
	for _, test := range []struct {
		name    string
		values  model.Value
		catalog int64
		valid   bool
	}{
		{name: "server", values: options("host=localhost", "sslmode=require"), catalog: 1417, valid: true},
		{name: "user mapping", values: options("user=alice", "password=secret"), catalog: 1418, valid: true},
		{name: "credentials rejected on server", values: options("password=secret"), catalog: 1417},
		{name: "connection option rejected on user mapping", values: options("host=localhost"), catalog: 1418},
		{name: "unknown catalog", values: options("host=localhost"), catalog: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := validatePostgresFDWOptions(context.Background(), []model.Value{test.values, model.Int64Val(test.catalog)}, CallBudget{})
			if test.valid {
				if err != nil || result.IsNull || result.Type != model.TypeBoolean || result.IntVal != 1 {
					t.Fatalf("validation = %+v, %v; want true", result, err)
				}
				return
			}
			if err == nil {
				t.Fatal("validation succeeded; want invalid option error")
			}
		})
	}
}
