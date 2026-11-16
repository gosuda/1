package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestInputCollationDerivation(t *testing.T) {
	text := func(collation model.CollationID) model.Type {
		return model.Type{ID: 25, Storage: model.TypeString, Collation: collation}
	}
	for _, tc := range []struct {
		name      string
		bound     model.CollationID
		arguments []model.Type
		want      model.CollationID
	}{
		{"explicit overrides conflict", 951, []model.Type{text(950), text(951)}, 951},
		{"default yields", 0, []model.Type{text(100), text(950)}, 950},
		{"default yields reversed", 0, []model.Type{text(951), text(100)}, 951},
		{"conflicting implicit", 0, []model.Type{text(950), text(951)}, 0},
		{"indeterminate propagates", 0, []model.Type{text(0), text(950)}, 0},
		{"default identity retained", 0, []model.Type{text(100)}, 100},
		{"cstring is not collatable", 0, []model.Type{{ID: 2275, Storage: model.TypeString}, text(950)}, 950},
		{"array metadata", 0, []model.Type{{ID: 1009, Storage: model.TypeArray, Collation: 951}}, 951},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := builtin.InputCollation(tc.bound, tc.arguments); got != tc.want {
				t.Fatalf("collation=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestTypeCollatableUsesCatalogIdentity(t *testing.T) {
	for _, tc := range []struct {
		typ  model.Type
		want bool
	}{
		{model.Type{ID: 25, Storage: model.TypeString}, true},
		{model.Type{ID: 1009, Storage: model.TypeArray}, true},
		{model.Type{ID: 1016, Storage: model.TypeArray}, false},
		{model.Type{ID: 2275, Storage: model.TypeString}, false},
		{model.Type{ID: 999999, Storage: model.TypeString, Collation: 950}, false},
	} {
		if got := builtin.TypeCollatable(tc.typ); got != tc.want {
			t.Errorf("type %d collatable=%v, want %v", tc.typ.ID, got, tc.want)
		}
	}
}
