package normalize_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func arrayIndexOperand(t *testing.T, graph *ir.Graph) (ir.ScalarID, ir.Scalar) {
	t.Helper()
	root := expression(t, graph)
	scalar, ok := graph.Scalar(root)
	if !ok {
		t.Fatalf("missing root scalar %d", root)
	}
	if scalar.Kind != ir.ArrayIndex || scalar.OperandCount() < 2 {
		t.Fatalf("root scalar = %+v, want ArrayIndex with index operand", scalar)
	}
	indexID, ok := graph.Operand(root, 1)
	if !ok {
		t.Fatal("missing array index operand")
	}
	indexScalar, ok := graph.Scalar(indexID)
	if !ok {
		t.Fatalf("missing index scalar %d", indexID)
	}
	return indexID, indexScalar
}

func TestFilterConstantRejectsCrossTypeAndNonIdentityTypes(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{
			name: "timestamp eq date",
			sql:  `SELECT (ARRAY[10,20,30])[EXTRACT(DAY FROM ts)::int] FROM (VALUES (TIMESTAMP '2000-01-02 00:00:00'), (TIMESTAMP '2000-01-03 00:00:00')) t(ts) WHERE ts = DATE '2000-01-02'`,
		},
		{
			name: "date eq timestamp reversed",
			sql:  `SELECT (ARRAY[10,20,30])[EXTRACT(DAY FROM ts)::int] FROM (VALUES (TIMESTAMP '2000-01-02 00:00:00'), (TIMESTAMP '2000-01-03 00:00:00')) t(ts) WHERE DATE '2000-01-02' = ts`,
		},
		{
			name: "numeric scale preservation",
			sql:  `SELECT (ARRAY[10,20,30])[scale(n)] FROM (VALUES (1.00::numeric), (2.00::numeric)) t(n) WHERE n = 1.0::numeric`,
		},
		{
			name: "int4 eq int8 no overflow fold",
			sql:  `SELECT (ARRAY[10,20,30])[i + 1] FROM (VALUES (1::int4), (2::int4)) t(i) WHERE i = 10000000000::bigint`,
		},
		{
			name: "int2 eq int4 no overflow fold",
			sql:  `SELECT (ARRAY[10,20,30])[(s + 1::int2)::int4] FROM (VALUES (1::int2), (2::int2)) t(s) WHERE s = 100000::int4`,
		},
		{
			name: "bpchar trailing spaces",
			sql:  `SELECT (ARRAY[10,20,30])[octet_length(c::text)] FROM (VALUES ('a'::char(3)), ('b'::char(3))) t(c) WHERE c = 'a'::char(3)`,
		},
		{
			name: "float8 signed zero",
			sql:  `SELECT (ARRAY[10,20,30])[CASE WHEN (1.0::float8 / f) > 0 THEN 1 ELSE 2 END] FROM (VALUES (-0.0::float8), (1.0::float8)) t(f) WHERE f = 0.0::float8`,
		},
		{
			name: "timestamptz excluded",
			sql:  `SELECT (ARRAY[10,20,30])[EXTRACT(DAY FROM tz)::int] FROM (VALUES (TIMESTAMPTZ '2000-01-02 00:00:00+00'), (TIMESTAMPTZ '2000-01-03 00:00:00+00')) t(tz) WHERE tz = TIMESTAMPTZ '2000-01-02 00:00:00+00'`,
		},
		{
			name: "jsonb numeric scale excluded",
			sql:  `SELECT (ARRAY[10,20,30])[length(j::text)] FROM (VALUES ('1.00'::jsonb), ('2.00'::jsonb)) t(j) WHERE j = '1.0'::jsonb`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := normalize.Normalize(t.Context(), bound(t, tc.sql), options())
			if err != nil {
				t.Fatalf("Normalize(%s) error = %v, want nil", tc.sql, err)
			}
			_, indexScalar := arrayIndexOperand(t, graph)
			if indexScalar.Kind == ir.Const {
				t.Fatalf("array index unexpectedly folded to constant %+v for %s", indexScalar, tc.sql)
			}
		})
	}
}

func TestFilterConstantDoesNotOverwriteNumericSingleRowValuesScale(t *testing.T) {
	sql := `SELECT (ARRAY[10,20,30])[scale(n)] FROM (VALUES (1.00::numeric)) t(n) WHERE n = 1.0::numeric`
	graph, err := normalize.Normalize(t.Context(), bound(t, sql), options())
	if err != nil {
		t.Fatalf("Normalize error = %v", err)
	}
	indexID, indexScalar := arrayIndexOperand(t, graph)
	if indexScalar.Kind == ir.Const {
		val, err := graph.CloneConstant(t.Context(), indexID, 1<<20)
		if err != nil {
			t.Fatalf("CloneConstant: %v", err)
		}
		if val.IntVal != 2 {
			t.Fatalf("folded scale(1.00) = %d, want 2 (must not substitute 1.0)", val.IntVal)
		}
	}
}

func TestFilterConstantAllowsExactSameTypeIdentitySubstitution(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int64
	}{
		{
			name: "int4 eq constant",
			sql:  `SELECT (ARRAY[10,20,30])[i] FROM (VALUES (1::int4), (2::int4)) t(i) WHERE i = 2`,
			want: 2,
		},
		{
			name: "constant eq int4 reversed",
			sql:  `SELECT (ARRAY[10,20,30])[i] FROM (VALUES (1::int4), (2::int4)) t(i) WHERE 2 = i`,
			want: 2,
		},
		{
			name: "int4 eq inside AND",
			sql:  `SELECT (ARRAY[10,20,30])[i + 1] FROM (VALUES (1::int4), (2::int4)) t(i) WHERE i > 0 AND i = 2`,
			want: 3,
		},
		{
			name: "deterministic text eq",
			sql:  `SELECT (ARRAY[10,20,30])[length(s)] FROM (VALUES ('a'::text), ('ab'::text)) t(s) WHERE s = 'ab'::text`,
			want: 2,
		},
		{
			name: "same type date eq",
			sql:  `SELECT (ARRAY[10,20,30])[EXTRACT(DAY FROM d)::int] FROM (VALUES (DATE '2000-01-01'), (DATE '2000-01-02')) t(d) WHERE d = DATE '2000-01-02'`,
			want: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph, err := normalize.Normalize(t.Context(), bound(t, tc.sql), options())
			if err != nil {
				t.Fatalf("Normalize(%s) error = %v", tc.sql, err)
			}
			indexID, indexScalar := arrayIndexOperand(t, graph)
			if indexScalar.Kind != ir.Const {
				t.Fatalf("array index = %+v, want folded Const(%d)", indexScalar, tc.want)
			}
			val, err := graph.CloneConstant(t.Context(), indexID, 1<<20)
			if err != nil {
				t.Fatalf("CloneConstant: %v", err)
			}
			if val.IsNull || val.IntVal != tc.want {
				t.Fatalf("folded index = %+v, want %d", val, tc.want)
			}
		})
	}
}

func addLayout(t *testing.T, f *fixture, layout model.TypeLayout) ir.LayoutID {
	t.Helper()
	id, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFilterConstantRejectsNonDeterministicCollationAndTypeModMismatch(t *testing.T) {
	t.Run("non-deterministic text collation", func(t *testing.T) {
		f := newFixture(t)
		ciText := addLayout(t, f, model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 16308 /* und-u-ks-level2 */, Storage: model.TypeString}})
		intArr := addLayout(t, f, model.TypeLayout{
			Type:     model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray},
			Children: []model.TypeLayout{{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}},
		})
		slot := f.slot(ciText, false)
		row1 := f.constant(ciText, model.StringVal("A"))
		row2 := f.constant(ciText, model.StringVal("b"))
		scan, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 2}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{row1, row2}})
		if err != nil {
			t.Fatal(err)
		}
		slotRef, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: ciText, Slot: slot}})
		if err != nil {
			t.Fatal(err)
		}
		constVal := f.constant(ciText, model.StringVal("a"))
		eqSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Operator: 98, Result: f.boolean, InputCollation: 16308, Strict: true, Effects: ir.Effects{Volatility: ir.Immutable}},
			Parameters: []ir.LayoutID{ciText, ciText},
		})
		if err != nil {
			t.Fatal(err)
		}
		pred, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Operator: 98, Signature: eqSig, Strict: true},
			Operands: []ir.ScalarID{slotRef, constVal},
		})
		if err != nil {
			t.Fatal(err)
		}
		filter, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{slot}})
		if err != nil {
			t.Fatal(err)
		}
		lenSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Function: 1317, Result: f.integer, Strict: true, Effects: ir.Effects{Volatility: ir.Immutable}},
			Parameters: []ir.LayoutID{ciText},
		})
		if err != nil {
			t.Fatal(err)
		}
		lenCall, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.integer, Function: 1317, Signature: lenSig, Strict: true},
			Operands: []ir.ScalarID{slotRef},
		})
		if err != nil {
			t.Fatal(err)
		}
		arrConst := f.constant(intArr, model.Value{
			Type:       model.TypeArray,
			ArrayVal:   []model.Value{model.Int64Val(10), model.Int64Val(20)},
			Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		})
		idxExpr, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.ArrayIndex, Scope: f.scope, Layout: f.integer, Nullable: true},
			Operands: []ir.ScalarID{arrConst, lenCall},
		})
		if err != nil {
			t.Fatal(err)
		}
		outSlot := f.slot(f.integer, true)
		proj, err := f.b.AddRel(t.Context(), ir.RelSpec{
			Rel:      ir.Rel{Kind: ir.Project, Scope: f.scope},
			Inputs:   []ir.RelID{filter},
			Outputs:  []model.SlotID{outSlot},
			Bindings: []ir.Binding{{Slot: outSlot, Expr: idxExpr}},
		})
		if err != nil {
			t.Fatal(err)
		}
		graph, err := f.b.Build(t.Context(), proj)
		if err != nil {
			t.Fatal(err)
		}
		norm, err := normalize.Normalize(t.Context(), graph, options())
		if err != nil {
			t.Fatalf("Normalize error = %v", err)
		}
		_, indexScalar := arrayIndexOperand(t, norm)
		if indexScalar.Kind == ir.Const {
			t.Fatalf("non-deterministic collation unexpectedly substituted constant: %+v", indexScalar)
		}
	})

	t.Run("typmod mismatch on timestamp", func(t *testing.T) {
		f := newFixture(t)
		ts0 := addLayout(t, f, model.TypeLayout{Type: model.Type{ID: 1114, Modifier: 0, Storage: model.TypeTimestamp}})
		tsAny := addLayout(t, f, model.TypeLayout{Type: model.Type{ID: 1114, Modifier: -1, Storage: model.TypeTimestamp}})
		intArr := addLayout(t, f, model.TypeLayout{
			Type:     model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray},
			Children: []model.TypeLayout{{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}},
		})
		slot := f.slot(ts0, false)
		row1 := f.constant(ts0, model.Value{Type: model.TypeTimestamp, IntVal: 0})
		row2 := f.constant(ts0, model.Value{Type: model.TypeTimestamp, IntVal: 1_000_000})
		scan, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 2}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{row1, row2}})
		if err != nil {
			t.Fatal(err)
		}
		slotRef, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: ts0, Slot: slot}})
		if err != nil {
			t.Fatal(err)
		}
		constVal := f.constant(tsAny, model.Value{Type: model.TypeTimestamp, IntVal: 0})
		eqSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Operator: 2060, Result: f.boolean, Strict: true, Effects: ir.Effects{Volatility: ir.Immutable}},
			Parameters: []ir.LayoutID{ts0, tsAny},
		})
		if err != nil {
			t.Fatal(err)
		}
		pred, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Operator: 2060, Signature: eqSig, Strict: true},
			Operands: []ir.ScalarID{slotRef, constVal},
		})
		if err != nil {
			t.Fatal(err)
		}
		filter, err := f.b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{slot}})
		if err != nil {
			t.Fatal(err)
		}
		nullTest, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.NullTest, Scope: f.scope, Layout: f.boolean},
			Operands: []ir.ScalarID{slotRef},
		})
		if err != nil {
			t.Fatal(err)
		}
		one := f.constant(f.integer, model.Int64Val(1))
		two := f.constant(f.integer, model.Int64Val(2))
		caseIdx, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.Case, Scope: f.scope, Layout: f.integer},
			Operands: []ir.ScalarID{nullTest, one, two},
		})
		if err != nil {
			t.Fatal(err)
		}
		arrConst := f.constant(intArr, model.Value{
			Type:       model.TypeArray,
			ArrayVal:   []model.Value{model.Int64Val(10), model.Int64Val(20)},
			Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}},
		})
		idxExpr, err := f.b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.ArrayIndex, Scope: f.scope, Layout: f.integer, Nullable: true},
			Operands: []ir.ScalarID{arrConst, caseIdx},
		})
		if err != nil {
			t.Fatal(err)
		}
		outSlot := f.slot(f.integer, true)
		proj, err := f.b.AddRel(t.Context(), ir.RelSpec{
			Rel:      ir.Rel{Kind: ir.Project, Scope: f.scope},
			Inputs:   []ir.RelID{filter},
			Outputs:  []model.SlotID{outSlot},
			Bindings: []ir.Binding{{Slot: outSlot, Expr: idxExpr}},
		})
		if err != nil {
			t.Fatal(err)
		}
		graph, err := f.b.Build(t.Context(), proj)
		if err != nil {
			t.Fatal(err)
		}
		norm, err := normalize.Normalize(t.Context(), graph, options())
		if err != nil {
			t.Fatalf("Normalize error = %v", err)
		}
		_, indexScalar := arrayIndexOperand(t, norm)
		if indexScalar.Kind == ir.Const {
			t.Fatalf("typmod mismatch unexpectedly substituted constant: %+v", indexScalar)
		}
	})
}
