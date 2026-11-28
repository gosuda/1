package builtin_test

import (
	"bytes"
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type testCollationResolver struct {
	specs map[model.CollationID]builtin.ICUCollationSpec
}

func (r testCollationResolver) ResolveCollation(id model.CollationID) (builtin.ICUCollationSpec, bool) {
	s, ok := r.specs[id]
	return s, ok
}

func (r testCollationResolver) CollationFingerprint() [sha256.Size]byte {
	return sha256.Sum256([]byte("test-collation-resolver"))
}

func TestICUCollationComparisonsAndKeys(t *testing.T) {
	ctx := t.Context()

	// 1. en-US-x-icu orders 'a' < 'B' < 'b' (unlike C byte order where 'B' < 'a').
	enCmp, err := builtin.CompareCollationText(ctx, builtin.CollationEnUSICU, nil, "a", "B")
	if err != nil || enCmp >= 0 {
		t.Fatalf("en-US-x-icu Compare('a', 'B') = %d, %v; want < 0", enCmp, err)
	}
	cCmp, err := builtin.CompareCollationText(ctx, 950, nil, "a", "B")
	if err != nil || cCmp <= 0 {
		t.Fatalf("C Compare('a', 'B') = %d, %v; want > 0", cCmp, err)
	}

	// 2. ko-KR-x-icu orders Hangul syllables in standard Korean alphabetical order.
	koOrder := []string{"가", "나", "다", "라", "마", "바", "사", "아", "자", "하"}
	for i := 0; i < len(koOrder)-1; i++ {
		cmp, err := builtin.CompareCollationText(ctx, builtin.CollationKoKRICU, nil, koOrder[i], koOrder[i+1])
		if err != nil || cmp >= 0 {
			t.Fatalf("ko-KR-x-icu Compare(%q, %q) = %d, %v; want < 0", koOrder[i], koOrder[i+1], cmp, err)
		}
	}

	// 3. Nondeterministic case-insensitive collation treats 'Hello' == 'hello'.
	ciEq, err := builtin.Resolve(67 /* texteq */, builtin.ResolveOptions{Collation: builtin.CollationUndCI, DefaultCollation: 950})
	if err != nil {
		t.Fatalf("Resolve(texteq, und-u-ks-level2): %v", err)
	}
	val, err := ciEq.Call(ctx, []model.Value{model.StringVal("Hello"), model.StringVal("hello")}, builtin.CallBudget{MemoryBytes: 1 << 20})
	if err != nil || val.IntVal == 0 {
		t.Fatalf("CI texteq('Hello', 'hello') = %v, %v; want true", val, err)
	}

	// 4. Numeric ICU collation orders 'file2' < 'file10'.
	numCmp, err := builtin.CompareCollationText(ctx, builtin.CollationEnUSNumeric, nil, "file2", "file10")
	if err != nil || numCmp >= 0 {
		t.Fatalf("en-US-u-kn-true Compare('file2', 'file10') = %d, %v; want < 0", numCmp, err)
	}

	// 5. KeyEncoder UCA sort keys match collation ordering AND round-trip for deterministic collations.
	enc, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatalf("NewKeyEncoder: %v", err)
	}
	enLayout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: builtin.CollationEnUSICU}}}
	ka, err := enc.EncodePrefix(ctx, enLayout, []model.Value{model.StringVal("apple")}, 1<<16)
	if err != nil {
		t.Fatalf("EncodePrefix(apple): %v", err)
	}
	kb, err := enc.EncodePrefix(ctx, enLayout, []model.Value{model.StringVal("Banana")}, 1<<16)
	if err != nil {
		t.Fatalf("EncodePrefix(Banana): %v", err)
	}
	if bytes.Compare(ka, kb) >= 0 {
		t.Fatalf("expected UCA key('apple') < key('Banana')")
	}
	decoded, err := enc.DecodePrefix(ctx, enLayout, ka, 1<<16)
	if err != nil || len(decoded) != 1 || decoded[0].StrVal != "apple" {
		t.Fatalf("DecodePrefix = %+v, %v; want 'apple'", decoded, err)
	}

	// 6. Nondeterministic CI keys produce identical bytes for 'Hello' and 'hello'.
	ciLayout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: builtin.CollationUndCI}}}
	k1, err := enc.EncodePrefix(ctx, ciLayout, []model.Value{model.StringVal("Hello")}, 1<<16)
	if err != nil {
		t.Fatalf("EncodePrefix CI Hello: %v", err)
	}
	k2, err := enc.EncodePrefix(ctx, ciLayout, []model.Value{model.StringVal("hello")}, 1<<16)
	if err != nil {
		t.Fatalf("EncodePrefix CI hello: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("expected identical CI keys for 'Hello' and 'hello', got %x vs %x", k1, k2)
	}

	// 7. User-defined CollationResolver + concurrent race safety.
	customID := model.CollationID(20500)
	resolver := testCollationResolver{
		specs: map[model.CollationID]builtin.ICUCollationSpec{
			customID: {ID: customID, Name: "my_ci", Locale: "en-US-u-ks-level2", Deterministic: false},
		},
	}
	customEnc := enc.WithResolveOptions(builtin.ResolveOptions{Collations: resolver})
	customLayout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 25, Storage: model.TypeString, Collation: customID}}}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range 50 {
				cmp, err := builtin.CompareCollationText(ctx, customID, resolver, "SuperSQL", "supersql")
				if err != nil || cmp != 0 {
					t.Errorf("goroutine %d: custom CI compare = %d, %v", id, cmp, err)
					return
				}
				if _, err := customEnc.EncodePrefix(ctx, customLayout, []model.Value{model.StringVal("SuperSQL")}, 1<<16); err != nil {
					t.Errorf("goroutine %d: custom encode: %v", id, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
