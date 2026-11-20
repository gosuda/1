package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestResolvedFunctionExclusionsPreserveOverloads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		argument model.TypeID
		feature  bind.FeatureID
	}{
		{"max", 23, ""},
		{"max", 27, bind.PhysicalStorage},
		{"jsonb_contains", 3802, ""},
		{"xpath", 25, bind.XML},
		{"pg_read_file", 25, bind.ServerIO},
		{"pg_notify", 25, bind.Notifications},
		{"pg_advisory_lock", 20, ""},
		{"pg_cancel_backend", 23, bind.ServerObservability},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+string(tc.feature), func(t *testing.T) {
			found := false
			for signature := range builtin.SignaturesNamed(tc.name) {
				if signature.Arguments[0] != tc.argument {
					continue
				}
				found = true
				err := bind.ValidateBuiltinFunction(signature.ID, 7)
				if tc.feature == "" {
					if err != nil {
						t.Fatalf("required overload %v rejected: %v", signature.Arguments, err)
					}
					continue
				}
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "0A000" || sqlErr.FeatureID != string(tc.feature) || sqlErr.Position != 7 {
					t.Fatalf("overload %v: got %v; want feature %s at 7", signature.Arguments, err, tc.feature)
				}
			}
			if !found {
				t.Fatal("reference overload absent from catalog")
			}
		})
	}
}

func TestSettingExclusionsDoNotInventUnknownParameters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, state string }{
		{"TimeZone", ""}, {"default_transaction_isolation", ""}, {"work_mem", ""},
		{"enable_seqscan", "0A000"}, {"shared_buffers", "0A000"}, {"application_not_a_setting", "42704"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := bind.ValidateSetting(tc.name, 1)
			if tc.state == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != tc.state {
				t.Fatalf("got %v; want %s", err, tc.state)
			}
		})
	}
}

func TestIsolationProfileRejectsWeakerNotInvalidAsUnsupported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ level, state string }{
		{"serializable", ""}, {"SERIALIZABLE", ""}, {"read committed", "0A000"},
		{"repeatable read", "0A000"}, {"read uncommitted", "0A000"}, {"snapshot", "22023"},
	} {
		t.Run(tc.level, func(t *testing.T) {
			err := bind.ValidateIsolation(tc.level, 1)
			if tc.state == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != tc.state {
				t.Fatalf("got %v; want %s", err, tc.state)
			}
		})
	}
}
