package builtin

import (
	"bytes"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestBinaryCompatibleClassMustPreserveKeySemantics(t *testing.T) {
	encoder, err := NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		typ   model.Type
		class string
	}{
		{"signed_to_unsigned", model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}, "oid_ops"},
		{"text_to_blank_padded", model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}, "bpchar_ops"},
	} {
		t.Run(test.name, func(t *testing.T) {
			class, found := LookupOperatorClassName("btree", test.class)
			if !found {
				t.Fatal("missing pinned class")
			}
			if err := ValidateOperatorClassInput(test.typ, class.ID); err != nil {
				t.Fatalf("binary input compatibility: %v", err)
			}
			err := encoder.ValidateOperatorClass(t.Context(), model.TypeLayout{Type: test.typ}, class.ID)
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "0A000" {
				t.Fatalf("non-equivalent key codec was accepted: %v", err)
			}
		})
	}
}

func TestPatternClassesPreserveBlankComparisonRules(t *testing.T) {
	encoder, err := NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		typ   model.Type
		class model.OID
		equal bool
	}{
		{"bpchar", model.Type{ID: 1042, Modifier: -1, Storage: model.TypeString, Collation: 950}, 4219, true},
		{"varchar", model.Type{ID: 1043, Modifier: -1, Storage: model.TypeString, Collation: 100}, 4218, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := encoder.ValidateOperatorClass(t.Context(), model.TypeLayout{Type: test.typ}, test.class); err != nil {
				t.Fatal(err)
			}
			layout := []storage.KeyColumn{{ColumnID: 1, Type: test.typ}}
			short, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{model.StringVal("x")}, 128)
			if err != nil {
				t.Fatal(err)
			}
			padded, err := encoder.EncodePrefix(t.Context(), layout, []model.Value{model.StringVal("x  ")}, 128)
			if err != nil {
				t.Fatal(err)
			}
			if test.equal {
				if !bytes.Equal(short, padded) {
					t.Fatal("bpchar trailing spaces changed index equality")
				}
			} else if bytes.Compare(short, padded) >= 0 {
				t.Fatal("varchar trailing spaces were ignored or misordered")
			}
		})
	}
}

func TestUUIDOperatorClassRequiresLogicalIdentity(t *testing.T) {
	encoder, err := NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	class, found := LookupDefaultOperatorClass("btree", 2950)
	if !found {
		t.Fatal("missing pinned UUID B-tree class")
	}
	typ := model.Type{ID: 2950, Storage: model.TypeUUID}
	if err := encoder.ValidateOperatorClass(t.Context(), model.TypeLayout{Type: typ}, class.ID); err != nil {
		t.Fatalf("UUID B-tree key compatibility: %v", err)
	}
	for _, test := range []struct {
		name string
		typ  model.Type
	}{
		{"physical_storage", model.Type{ID: 2950, Storage: model.TypeString}},
		{"collation", model.Type{ID: 2950, Storage: model.TypeUUID, Collation: 950}},
		{"different_sql_type", model.Type{ID: 25, Storage: model.TypeUUID}},
		{"unknown_sql_type", model.Type{ID: 99999, Storage: model.TypeUUID}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := encoder.ValidateOperatorClass(t.Context(), model.TypeLayout{Type: test.typ}, class.ID)
			var sql *model.Error
			if !errors.As(err, &sql) || sql.Code != "42804" {
				t.Fatalf("invalid UUID class input was accepted: %v", err)
			}
		})
	}
}
