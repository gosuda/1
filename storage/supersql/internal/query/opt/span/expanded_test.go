package span_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
)

func TestUUIDSpanExtractionAndKeyOrder(t *testing.T) {
	uuidType := model.Type{ID: 2950, Storage: model.TypeUUID}
	low := "00000000-0000-0000-0000-000000000001"
	mid := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	midUpper := "{A0EE-BC99-9C0B-4EF8-BB6D-6BB9-BD38-0A11}"
	high := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	for _, descending := range []bool{false, true} {
		f := setup(t, uuidType)
		f.keys[0].Column.Descending = descending
		// Equality with braced/uppercase form matches canonical form.
		eq := f.call(2956, 0, f.slots[0], f.constant(0, model.Value{Type: model.TypeUUID, StrVal: midUpper}))
		eqRes := extract(t, f.graph(), eq, f.keys, nil, limits())
		if eqRes.Empty || eqRes.Residual != 0 || len(eqRes.Spans) != 1 {
			t.Fatalf("descending=%t uuid eq got %+v", descending, eqRes)
		}
		if !matches(t, f.keys, eqRes, model.Value{Type: model.TypeUUID, StrVal: mid}) ||
			matches(t, f.keys, eqRes, model.Value{Type: model.TypeUUID, StrVal: low}) {
			t.Fatalf("descending=%t uuid eq mismatch", descending)
		}

		// Range: low < id <= mid
		f2 := setup(t, uuidType)
		f2.keys[0].Column.Descending = descending
		gt := f2.call(2958, 0, f2.slots[0], f2.constant(0, model.Value{Type: model.TypeUUID, StrVal: low}))
		le := f2.call(2955, 0, f2.slots[0], f2.constant(0, model.Value{Type: model.TypeUUID, StrVal: mid}))
		pred := f2.booleanExpr(ir.And, gt, le)
		res := extract(t, f2.graph(), pred, f2.keys, nil, limits())
		if res.Empty || res.Residual != 0 || len(res.Spans) != 1 {
			t.Fatalf("descending=%t uuid range got %+v", descending, res)
		}
		if matches(t, f2.keys, res, model.Value{Type: model.TypeUUID, StrVal: low}) ||
			!matches(t, f2.keys, res, model.Value{Type: model.TypeUUID, StrVal: mid}) ||
			matches(t, f2.keys, res, model.Value{Type: model.TypeUUID, StrVal: high}) ||
			matches(t, f2.keys, res, model.NullVal(model.TypeUUID)) {
			t.Fatalf("descending=%t uuid range boundary mismatch", descending)
		}
	}

	// Malformed UUID constant must not produce invalid bounds.
	f := setup(t, uuidType)
	bad := f.call(2956, 0, f.slots[0], f.constant(0, model.Value{Type: model.TypeUUID, StrVal: "not-a-uuid"}))
	res := extract(t, f.graph(), bad, f.keys, nil, limits())
	if res.Empty || res.Residual != bad || !res.Spans[0].Lower.Unbounded {
		t.Fatalf("malformed uuid restricted bounds: %+v", res)
	}
}

func TestVarcharAndBPCharSpanExtraction(t *testing.T) {
	varcharType := model.Type{ID: 1043, Modifier: 24, Storage: model.TypeString, Collation: 950}
	textType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}

	// VARCHAR column relabeled to TEXT for texteq / text_ge / text_lt.
	f := setup(t, varcharType)
	textLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: textType})
	if err != nil {
		t.Fatal(err)
	}
	relabel := f.add(ir.Scalar{Kind: ir.Relabel, Layout: textLayout, Nullable: true, CastContext: ir.ImplicitCast}, []ir.ScalarID{f.slots[0]}, model.Value{})
	low := f.add(ir.Scalar{Kind: ir.Const, Layout: textLayout}, nil, model.StringVal("alpha"))
	high := f.add(ir.Scalar{Kind: ir.Const, Layout: textLayout}, nil, model.StringVal("gamma"))
	pred := f.booleanExpr(ir.And, f.call(743, 0, relabel, low), f.call(740, 0, relabel, high))
	res := extract(t, f.graph(), pred, f.keys, nil, limits())
	if res.Empty || res.Residual != 0 || len(res.Spans) != 1 {
		t.Fatalf("varchar range got %+v", res)
	}
	if matches(t, f.keys, res, model.StringVal("alph")) ||
		!matches(t, f.keys, res, model.StringVal("alpha")) ||
		!matches(t, f.keys, res, model.StringVal("beta")) ||
		matches(t, f.keys, res, model.StringVal("gamma")) {
		t.Fatalf("varchar range membership mismatch")
	}

	// BPCHAR trailing space semantics: 'abc  ' == 'abc'.
	bpcharType := model.Type{ID: 1042, Modifier: 9, Storage: model.TypeString, Collation: 950}
	fb := setup(t, bpcharType)
	p1 := fb.call(1048, 0, fb.slots[0], fb.constant(0, model.StringVal("abc  ")))
	p2 := fb.call(1048, 0, fb.slots[0], fb.constant(0, model.StringVal("abc")))
	eqBP := fb.booleanExpr(ir.And, p1, p2)
	resBP := extract(t, fb.graph(), eqBP, fb.keys, nil, limits())
	if resBP.Empty || resBP.Residual != 0 || len(resBP.Spans) != 1 {
		t.Fatalf("bpchar trailing-space equality intersection failed: %+v", resBP)
	}
	if !matches(t, fb.keys, resBP, model.StringVal("abc")) ||
		!matches(t, fb.keys, resBP, model.StringVal("abc     ")) ||
		matches(t, fb.keys, resBP, model.StringVal("abcd")) {
		t.Fatalf("bpchar membership mismatch")
	}

	// BPCHAR relabeled to TEXT must NOT be unwrapped (ordering differs on trailing spaces).
	fb2 := setup(t, bpcharType)
	tl2, err := fb2.b.AddLayout(t.Context(), model.TypeLayout{Type: textType})
	if err != nil {
		t.Fatal(err)
	}
	unsafeRelabel := fb2.add(ir.Scalar{Kind: ir.Relabel, Layout: tl2, Nullable: true, CastContext: ir.ExplicitCast}, []ir.ScalarID{fb2.slots[0]}, model.Value{})
	c := fb2.add(ir.Scalar{Kind: ir.Const, Layout: tl2}, nil, model.StringVal("a "))
	unsafePred := fb2.call(742, 0, unsafeRelabel, c)
	unsafeRes := extract(t, fb2.graph(), unsafePred, fb2.keys, nil, limits())
	if unsafeRes.Residual != unsafePred || !unsafeRes.Spans[0].Lower.Unbounded {
		t.Fatalf("unsafe bpchar->text relabel was unwrapped: %+v", unsafeRes)
	}
}

func TestNumericSpanExtractionAndKeyOrder(t *testing.T) {
	numType := model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}
	numVal := func(s string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: s} }

	for _, descending := range []bool{false, true} {
		// Equal with distinct decimal forms: 1.50 AND 1.5e0 -> non-empty exact point.
		f := setup(t, numType)
		f.keys[0].Column.Descending = descending
		eq1 := f.call(1718, 0, f.slots[0], f.constant(0, numVal("1.50")))
		eq2 := f.call(1718, 0, f.slots[0], f.constant(0, numVal("1.5e0")))
		pred := f.booleanExpr(ir.And, eq1, eq2)
		res := extract(t, f.graph(), pred, f.keys, nil, limits())
		if res.Empty || res.Residual != 0 || len(res.Spans) != 1 {
			t.Fatalf("descending=%t numeric eq got %+v", descending, res)
		}
		if !matches(t, f.keys, res, numVal("1.5")) ||
			!matches(t, f.keys, res, numVal("1.5000")) ||
			matches(t, f.keys, res, numVal("1.51")) {
			t.Fatalf("descending=%t numeric eq mismatch", descending)
		}

		// Range across negative, zero, positive, and NaN/Infinity ordering.
		f2 := setup(t, numType)
		f2.keys[0].Column.Descending = descending
		ge := f2.call(1721, 0, f2.slots[0], f2.constant(0, numVal("-10.5")))
		lt := f2.call(1722, 0, f2.slots[0], f2.constant(0, numVal("1e2")))
		rpred := f2.booleanExpr(ir.And, ge, lt)
		rres := extract(t, f2.graph(), rpred, f2.keys, nil, limits())
		if rres.Empty || rres.Residual != 0 || len(rres.Spans) != 1 {
			t.Fatalf("descending=%t numeric range got %+v", descending, rres)
		}
		for _, tc := range []struct {
			val  string
			want bool
		}{
			{"-Infinity", false},
			{"-11", false},
			{"-10.50", true},
			{"-2", true},
			{"-0.0", true},
			{"0", true},
			{"99.999", true},
			{"100", false},
			{"Infinity", false},
			{"NaN", false},
		} {
			if got := matches(t, f2.keys, rres, numVal(tc.val)); got != tc.want {
				t.Fatalf("descending=%t numeric %s: got %t, want %t", descending, tc.val, got, tc.want)
			}
		}

		// NaN sorts above +Infinity in PostgreSQL B-tree order.
		f3 := setup(t, numType)
		f3.keys[0].Column.Descending = descending
		gtInf := f3.call(1720, 0, f3.slots[0], f3.constant(0, numVal("Infinity")))
		infRes := extract(t, f3.graph(), gtInf, f3.keys, nil, limits())
		if infRes.Residual != 0 || !matches(t, f3.keys, infRes, numVal("NaN")) || matches(t, f3.keys, infRes, numVal("Infinity")) {
			t.Fatalf("descending=%t numeric > Infinity mismatch: %+v", descending, infRes)
		}
	}
}

func TestDomainAndEnumSpanExtraction(t *testing.T) {
	// User-defined domain over int4 (OID 90001 -> relabeled to 23).
	domainInt := model.Type{ID: 90001, Modifier: -1, Storage: model.TypeInt64}
	int4Type := model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}
	f := setup(t, domainInt)
	int4Layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: int4Type})
	if err != nil {
		t.Fatal(err)
	}
	relabel := f.add(ir.Scalar{Kind: ir.Relabel, Layout: int4Layout, Nullable: true, CastContext: ir.ImplicitCast}, []ir.ScalarID{f.slots[0]}, model.Value{})
	val := f.add(ir.Scalar{Kind: ir.Const, Layout: int4Layout}, nil, model.Int64Val(42))
	pred := f.call(65, 0, relabel, val) // int4eq
	res := extract(t, f.graph(), pred, f.keys, nil, limits())
	if res.Empty || res.Residual != 0 || len(res.Spans) != 1 || res.Spans[0].Lower.Values[0].IntVal != 42 {
		t.Fatalf("domain int4 eq got %+v", res)
	}

	// User-defined enum (OID 90025): single equality is exact; contradictory equality is empty;
	// OR of two distinct enum values widens conservatively so catalog enum order is never violated.
	enumType := model.Type{ID: 90025, Modifier: -1, Storage: model.TypeInt64}
	fe := setup(t, enumType)
	eqRed := fe.call(3508, 0, fe.slots[0], fe.constant(0, model.Int64Val(92001)))
	eqBlue := fe.call(3508, 0, fe.slots[0], fe.constant(0, model.Int64Val(92002)))
	both := fe.booleanExpr(ir.And, eqRed, eqBlue)
	either := fe.booleanExpr(ir.Or, eqRed, eqBlue)
	g := fe.graph()

	redRes := extract(t, g, eqRed, fe.keys, nil, limits())
	if redRes.Empty || redRes.Residual != 0 || len(redRes.Spans) != 1 || redRes.Spans[0].Lower.Values[0].IntVal != 92001 {
		t.Fatalf("enum single eq got %+v", redRes)
	}
	bothRes := extract(t, g, both, fe.keys, nil, limits())
	if !bothRes.Empty || len(bothRes.Spans) != 0 {
		t.Fatalf("contradictory enum eq must prove empty: %+v", bothRes)
	}
	eitherRes := extract(t, g, either, fe.keys, nil, limits())
	if eitherRes.Empty || eitherRes.Residual != either || len(eitherRes.Spans) != 1 || !eitherRes.Spans[0].Lower.Unbounded {
		t.Fatalf("multi-value enum OR must widen conservatively: %+v", eitherRes)
	}
}

func TestLikePrefixSpanExtraction(t *testing.T) {
	textType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}

	for _, descending := range []bool{false, true} {
		// Pure prefix LIKE 'abc%' on text -> exact [abc, abd)
		f := setup(t, textType)
		f.keys[0].Column.Descending = descending
		pred := f.call(850, 0, f.slots[0], f.constant(0, model.StringVal("abc%")))
		res := extract(t, f.graph(), pred, f.keys, nil, limits())
		if res.Empty || res.Residual != 0 || len(res.Spans) != 1 {
			t.Fatalf("descending=%t LIKE 'abc%%' got %+v", descending, res)
		}
		for _, tc := range []struct {
			s    string
			want bool
		}{
			{"ab", false},
			{"abc", true},
			{"abc\nxyz", true},
			{"abcz", true},
			{"abd", false},
			{"abC", false},
		} {
			if got := matches(t, f.keys, res, model.StringVal(tc.s)); got != tc.want {
				t.Fatalf("descending=%t LIKE 'abc%%' on %q: got %t, want %t", descending, tc.s, got, tc.want)
			}
		}
		if matches(t, f.keys, res, model.NullVal(model.TypeString)) {
			t.Fatalf("descending=%t LIKE selected NULL", descending)
		}
	}

	// Non-pure prefix LIKE 'abc%def' and LIKE 'abc_' extract [abc, abd) and keep residual.
	for _, pat := range []string{"abc%def", "abc_"} {
		f := setup(t, textType)
		pred := f.call(850, 0, f.slots[0], f.constant(0, model.StringVal(pat)))
		res := extract(t, f.graph(), pred, f.keys, nil, limits())
		if res.Empty || res.Residual != pred || len(res.Spans) != 1 {
			t.Fatalf("LIKE %q expected narrowed span + residual, got %+v", pat, res)
		}
		if !matches(t, f.keys, res, model.StringVal("abcx")) || matches(t, f.keys, res, model.StringVal("abd")) {
			t.Fatalf("LIKE %q span bounds wrong: %+v", pat, res.Spans)
		}
	}

	// Escaped wildcard LIKE 'a\_b%' -> prefix "a_b", exact [a_b, a_c).
	fEsc := setup(t, textType)
	escPred := fEsc.call(850, 0, fEsc.slots[0], fEsc.constant(0, model.StringVal(`a\_b%`)))
	escRes := extract(t, fEsc.graph(), escPred, fEsc.keys, nil, limits())
	if escRes.Residual != 0 || !matches(t, fEsc.keys, escRes, model.StringVal("a_b1")) || matches(t, fEsc.keys, escRes, model.StringVal("axb1")) {
		t.Fatalf("escaped LIKE got %+v", escRes)
	}

	// UTF-8 boundary prefixes (0x7f, multi-byte, U+10FFFF) must produce valid UTF-8 KeyEncoder bounds.
	for _, pat := range []string{"a\x7f%", "café%", "\U0010ffff%"} {
		fU := setup(t, textType)
		pU := fU.call(850, 0, fU.slots[0], fU.constant(0, model.StringVal(pat)))
		rU := extract(t, fU.graph(), pU, fU.keys, nil, limits())
		if rU.Empty || len(rU.Spans) != 1 {
			t.Fatalf("UTF-8 LIKE %q failed: %+v", pat, rU)
		}
		sample := pat[:len(pat)-1] + "z"
		if !matches(t, fU.keys, rU, model.StringVal(sample)) {
			t.Fatalf("UTF-8 LIKE %q excluded matching %q", pat, sample)
		}
	}

	// Leading wildcard and trailing unescaped escape must remain unrestricted + residual.
	for _, pat := range []string{"%abc", "_abc", `abc\`, `abc%\`} {
		fBad := setup(t, textType)
		pBad := fBad.call(850, 0, fBad.slots[0], fBad.constant(0, model.StringVal(pat)))
		rBad := extract(t, fBad.graph(), pBad, fBad.keys, nil, limits())
		if rBad.Empty || rBad.Residual != pBad || !rBad.Spans[0].Lower.Unbounded {
			t.Fatalf("LIKE %q unexpectedly narrowed: %+v", pat, rBad)
		}
	}

	// Unresolved collation 100 must NOT extract LIKE spans.
	fCol := setup(t, model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 100})
	pCol := fCol.call(850, 0, fCol.slots[0], fCol.constant(0, model.StringVal("abc%")))
	rCol := extract(t, fCol.graph(), pCol, fCol.keys, nil, limits())
	if rCol.Empty || rCol.Residual != pCol || !rCol.Spans[0].Lower.Unbounded {
		t.Fatalf("unresolved collation extracted LIKE span: %+v", rCol)
	}

	// BPCHAR LIKE 'abc%' extracts span AND keeps residual.
	bpType := model.Type{ID: 1042, Modifier: 9, Storage: model.TypeString, Collation: 950}
	fBP := setup(t, bpType)
	tl, err := fBP.b.AddLayout(t.Context(), model.TypeLayout{Type: textType})
	if err != nil {
		t.Fatal(err)
	}
	bpPat := fBP.add(ir.Scalar{Kind: ir.Const, Layout: tl}, nil, model.StringVal("abc%"))
	pBP := fBP.call(1631, 0, fBP.slots[0], bpPat)
	rBP := extract(t, fBP.graph(), pBP, fBP.keys, nil, limits())
	if rBP.Empty || rBP.Residual != pBP || len(rBP.Spans) != 1 {
		t.Fatalf("bpcharlike expected narrowed span + residual: %+v", rBP)
	}
	if !matches(t, fBP.keys, rBP, model.StringVal("abc  ")) || matches(t, fBP.keys, rBP, model.StringVal("abd  ")) {
		t.Fatalf("bpcharlike span bounds wrong: %+v", rBP.Spans)
	}
}

func TestArrayQuantifiedSpanExtraction(t *testing.T) {
	// Helper to build ArrayQuantified node: left <op> ANY/ALL (right)
	addQuantified := func(f *fixture, kind ir.SubqueryKind, op model.OperatorID, left, right ir.ScalarID, elemLayout ir.LayoutID) ir.ScalarID {
		sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Operator: op, Result: f.boolean, Strict: true, Nullable: true},
			Parameters: []ir.LayoutID{f.nodes[left].Layout, elemLayout},
		})
		if err != nil {
			t.Fatal(err)
		}
		return f.add(ir.Scalar{
			Kind:         ir.ArrayQuantified,
			SubqueryKind: kind,
			Operator:     op,
			Signature:    sig,
			Strict:       true,
			Nullable:     true,
			Layout:       f.boolean,
		}, []ir.ScalarID{left, right}, model.Value{})
	}

	// 1. = ANY(ARRAY[10, 2, 10, NULL]) -> exact disjoint sorted spans [2,2], [10,10].
	f := setup(t, integer)
	arrLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 1016, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: integer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	arrExpr := f.add(ir.Scalar{Kind: ir.Array, Layout: arrLayout}, []ir.ScalarID{
		f.constant(0, model.Int64Val(10)),
		f.constant(0, model.Int64Val(2)),
		f.constant(0, model.Int64Val(10)),
		f.constant(0, model.NullVal(model.TypeInt64)),
	}, model.Value{})
	pred := addQuantified(f, ir.AnySubquery, 410, f.slots[0], arrExpr, f.layouts[0])
	res := extract(t, f.graph(), pred, f.keys, nil, limits())
	if res.Empty || res.Residual != 0 || res.Widened || len(res.Spans) != 2 {
		t.Fatalf("= ANY(ARRAY[...]) got %+v", res)
	}
	for _, v := range []int64{1, 2, 5, 10, 11} {
		if got := matches(t, f.keys, res, model.Int64Val(v)); got != (v == 2 || v == 10) {
			t.Fatalf("value %d: got %t", v, got)
		}
	}
	if matches(t, f.keys, res, model.NullVal(model.TypeInt64)) {
		t.Fatal("= ANY selected NULL")
	}

	// 2. = ANY($1) with bound int8[] parameter, plus MaxSpans widening and ExtractStatic.
	f2 := setup(t, integer)
	arrLayout2, err := f2.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 1016, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: integer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	param := f2.add(ir.Scalar{Kind: ir.Param, Layout: arrLayout2, Nullable: true, Parameter: 1}, nil, model.Value{})
	pred2 := addQuantified(f2, ir.AnySubquery, 410, f2.slots[0], param, f2.layouts[0])
	g2 := f2.graph()
	paramLayout := model.TypeLayout{Type: model.Type{ID: 1016, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: integer}}}
	paramVal := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}},
		ArrayVal:   []model.Value{model.Int64Val(30), model.Int64Val(10), model.Int64Val(20)},
	}
	res2 := extract(t, g2, pred2, f2.keys, []span.Parameter{{Number: 1, Layout: paramLayout, Value: paramVal}}, limits())
	if res2.Empty || res2.Residual != 0 || len(res2.Spans) != 3 {
		t.Fatalf("= ANY($1) got %+v", res2)
	}
	if !matches(t, f2.keys, res2, model.Int64Val(10)) || !matches(t, f2.keys, res2, model.Int64Val(20)) || !matches(t, f2.keys, res2, model.Int64Val(30)) || matches(t, f2.keys, res2, model.Int64Val(15)) {
		t.Fatalf("= ANY($1) membership mismatch")
	}

	// Widening when distinct array elements exceed MaxSpans.
	tight := limits()
	tight.MaxSpans = 2
	wide := extract(t, g2, pred2, f2.keys, []span.Parameter{{Number: 1, Layout: paramLayout, Value: paramVal}}, tight)
	if !wide.Widened || wide.Residual != pred2 || !wide.Spans[0].Lower.Unbounded {
		t.Fatalf("= ANY($1) MaxSpans widening failed: %+v", wide)
	}

	// Empty array and NULL array prove Empty.
	emptyArr := model.Value{Type: model.TypeArray}
	if r := extract(t, g2, pred2, f2.keys, []span.Parameter{{Number: 1, Layout: paramLayout, Value: emptyArr}}, limits()); !r.Empty || r.Residual != 0 {
		t.Fatalf("= ANY('{}') must prove empty: %+v", r)
	}
	if r := extract(t, g2, pred2, f2.keys, []span.Parameter{{Number: 1, Layout: paramLayout, Value: model.NullVal(model.TypeArray)}}, limits()); !r.Empty || r.Residual != 0 {
		t.Fatalf("= ANY(NULL) must prove empty: %+v", r)
	}

	// Static extraction leaves unbound $1 as residual.
	staticRes, err := span.ExtractStatic(t.Context(), g2, pred2, f2.keys, limits())
	if err != nil {
		t.Fatal(err)
	}
	if staticRes.Empty || staticRes.Residual != pred2 || !staticRes.Spans[0].Lower.Unbounded {
		t.Fatalf("ExtractStatic on = ANY($1) narrowed unexpectedly: %+v", staticRes)
	}

	// 3. = ANY($1) with UUID[] parameter.
	uuidType := model.Type{ID: 2950, Storage: model.TypeUUID}
	fu := setup(t, uuidType)
	uuidArrLayout, err := fu.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 2951, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: uuidType}},
	})
	if err != nil {
		t.Fatal(err)
	}
	uParam := fu.add(ir.Scalar{Kind: ir.Param, Layout: uuidArrLayout, Nullable: true, Parameter: 1}, nil, model.Value{})
	uPred := addQuantified(fu, ir.AnySubquery, 2972, fu.slots[0], uParam, fu.layouts[0]) // uuid = uuid (2972)
	u1 := "00000000-0000-0000-0000-000000000001"
	u2 := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	uLayout := model.TypeLayout{Type: model.Type{ID: 2951, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: uuidType}}}
	uVal := model.Value{
		Type:       model.TypeArray,
		Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		ArrayVal:   []model.Value{{Type: model.TypeUUID, StrVal: u2}, {Type: model.TypeUUID, StrVal: u1}},
	}
	uRes := extract(t, fu.graph(), uPred, fu.keys, []span.Parameter{{Number: 1, Layout: uLayout, Value: uVal}}, limits())
	if uRes.Empty || uRes.Residual != 0 || len(uRes.Spans) != 2 {
		t.Fatalf("uuid = ANY($1) got %+v", uRes)
	}
	if !matches(t, fu.keys, uRes, model.Value{Type: model.TypeUUID, StrVal: u1}) || !matches(t, fu.keys, uRes, model.Value{Type: model.TypeUUID, StrVal: u2}) {
		t.Fatalf("uuid = ANY($1) membership mismatch")
	}

	// 4. = ALL(ARRAY[...]) is NOT extracted as OR spans.
	fa := setup(t, integer)
	al, err := fa.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 1016, Modifier: -1, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: integer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ae := fa.add(ir.Scalar{Kind: ir.Array, Layout: al}, []ir.ScalarID{fa.constant(0, model.Int64Val(1)), fa.constant(0, model.Int64Val(2))}, model.Value{})
	allPred := addQuantified(fa, ir.AllSubquery, 410, fa.slots[0], ae, fa.layouts[0])
	allRes := extract(t, fa.graph(), allPred, fa.keys, nil, limits())
	if allRes.Empty || allRes.Residual != allPred || !allRes.Spans[0].Lower.Unbounded {
		t.Fatalf("= ALL should remain residual: %+v", allRes)
	}
}
