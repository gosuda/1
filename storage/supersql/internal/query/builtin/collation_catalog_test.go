package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestCollationMetadataPreservesPinnedIdentities(t *testing.T) {
	for _, want := range []struct {
		name     string
		id       model.CollationID
		provider byte
		encoding int32
	}{
		{"default", 100, 'd', -1},
		{"C", 950, 'c', -1},
		{"POSIX", 951, 'c', -1},
		{"pg_c_utf8", 811, 'b', 6},
		{"ucs_basic", 962, 'b', 6},
		{"unicode", 963, 'i', -1},
		{"pg_unicode_fast", 6411, 'b', 6},
	} {
		t.Run(want.name, func(t *testing.T) {
			byName, ok := builtin.LookupCollationName(want.name)
			if !ok {
				t.Fatalf("pinned collation %q missing", want.name)
			}
			if byName.ID != want.id {
				t.Errorf("collation %q ID = %d, want %d", want.name, byName.ID, want.id)
			}
			byID, ok := builtin.LookupCollation(want.id)
			if !ok {
				t.Fatalf("pinned collation OID %d missing", want.id)
			}
			if byID.Name != want.name {
				t.Errorf("collation %d name = %q, want %q", want.id, byID.Name, want.name)
			}
			for _, info := range []builtin.CollationInfo{byName, byID} {
				if info.Provider != want.provider || info.Encoding != want.encoding || !info.Deterministic {
					t.Errorf("collation %q metadata = %+v, want provider %q, encoding %d, deterministic", want.name, info, want.provider, want.encoding)
				}
			}
		})
	}
}

func TestCollationNameLookupDoesNotResolveSQLNames(t *testing.T) {
	for _, name := range []string{"", "c", "posix", "DEFAULT", "\"C\"", "pg_catalog.C", "public.C", "public.unicode", "en-US-x-icu", "missing_collation"} {
		t.Run(name, func(t *testing.T) {
			if info, ok := builtin.LookupCollationName(name); ok {
				t.Fatalf("noncatalog name %q resolved to %+v", name, info)
			}
		})
	}
}

func TestCollationLookupRejectsUnknownIdentities(t *testing.T) {
	for _, id := range []model.CollationID{0, 949, 952, 6412} {
		if info, ok := builtin.LookupCollation(id); ok {
			t.Errorf("unknown collation OID %d resolved to %+v", id, info)
		}
	}
}
