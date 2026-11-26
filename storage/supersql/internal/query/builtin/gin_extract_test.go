package builtin_test

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestGINExtractJSONBArrayAndTSVector(t *testing.T) {
	ctx := t.Context()
	enc, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatalf("NewKeyEncoder: %v", err)
	}

	// 1. JSONB jsonb_ops & jsonb_path_ops
	jsonbLayout := model.TypeLayout{Type: model.Type{ID: 3802, Storage: model.TypeJSONB}}
	doc := model.Value{Type: model.TypeJSONB, StrVal: `{"user":{"name":"Alice","age":30},"tags":["go","sql"]}`}
	toks, err := builtin.ExtractGINDocumentTokens(ctx, jsonbLayout, doc, "jsonb_ops", enc, 1<<20)
	if err != nil {
		t.Fatalf("ExtractGINDocumentTokens jsonb_ops: %v", err)
	}
	asStrings := make([]string, len(toks))
	for i, tk := range toks {
		asStrings[i] = string(tk)
	}
	for _, want := range []string{"k:user", "k:name", "k:tags", "kv:user\x1fname=s:Alice", "kv:tags=s:go", "kv:user\x1fage=n:30"} {
		if !slices.Contains(asStrings, want) {
			t.Fatalf("jsonb_ops tokens %v missing %q", asStrings, want)
		}
	}

	// Query plan for @> containment matches subset of document tokens.
	qPlan, ok, err := builtin.ExtractGINQueryPlan(ctx, 3246, jsonbLayout, model.Value{Type: model.TypeJSONB, StrVal: `{"user":{"name":"Alice"}}`}, "jsonb_ops", enc, 1<<20)
	if err != nil || !ok || !qPlan.RequireAll || len(qPlan.Tokens) == 0 {
		t.Fatalf("ExtractGINQueryPlan(@>): %+v, %v, %v", qPlan, ok, err)
	}
	for _, qt := range qPlan.Tokens {
		if !slices.Contains(asStrings, string(qt)) {
			t.Fatalf("query token %q not present in matching document tokens %v", string(qt), asStrings)
		}
	}

	// Query plan for ? key existence.
	qExists, ok, err := builtin.ExtractGINQueryPlan(ctx, 3247, jsonbLayout, model.StringVal("tags"), "jsonb_ops", enc, 1<<20)
	if err != nil || !ok || len(qExists.Tokens) != 1 || string(qExists.Tokens[0]) != "k:tags" {
		t.Fatalf("ExtractGINQueryPlan(?): %+v, %v, %v", qExists, ok, err)
	}

	// jsonb_path_ops excludes ? operator but supports @>.
	if _, ok, err := builtin.ExtractGINQueryPlan(ctx, 3247, jsonbLayout, model.StringVal("tags"), "jsonb_path_ops", enc, 1<<20); err != nil || ok {
		t.Fatalf("jsonb_path_ops should not support ?, got ok=%v err=%v", ok, err)
	}

	// 2. Array GIN extraction (int4[])
	arrLayout := model.TypeLayout{
		Type:     model.Type{ID: 1007, Storage: model.TypeArray},
		Children: []model.TypeLayout{{Type: model.Type{ID: 23, Storage: model.TypeInt64}}},
	}
	arrDoc := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(10), model.Int64Val(20), model.NullVal(model.TypeInt64)}}
	arrToks, err := builtin.ExtractGINDocumentTokens(ctx, arrLayout, arrDoc, "", enc, 1<<20)
	if err != nil || len(arrToks) != 2 {
		t.Fatalf("array tokens = %d, %v; want 2", len(arrToks), err)
	}
	arrPlan, ok, err := builtin.ExtractGINQueryPlan(ctx, 2750, arrLayout, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(20), model.Int64Val(99)}}, "", enc, 1<<20)
	if err != nil || !ok || arrPlan.RequireAll || len(arrPlan.Tokens) != 2 {
		t.Fatalf("array && plan = %+v, %v, %v", arrPlan, ok, err)
	}

	// 3. tsvector / tsquery GIN extraction
	toVec, err := builtin.Resolve(3610 /* tsvectorin */, builtin.ResolveOptions{DefaultCollation: 950})
	if err != nil {
		t.Fatalf("Resolve tsvectorin: %v", err)
	}
	tsVecVal, err := toVec.Call(ctx, []model.Value{model.StringVal("'fat':2 'cat':3 'rat':4")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatalf("tsvectorin: %v", err)
	}
	toQ, err := builtin.Resolve(3612 /* tsqueryin */, builtin.ResolveOptions{DefaultCollation: 950})
	if err != nil {
		t.Fatalf("Resolve tsqueryin: %v", err)
	}
	tsQVal, err := toQ.Call(ctx, []model.Value{model.StringVal("fat & rat")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20})
	if err != nil {
		t.Fatalf("tsqueryin: %v", err)
	}
	tsLayout := model.TypeLayout{Type: model.Type{ID: 3614, Storage: model.TypeBytes}}
	tsToks, err := builtin.ExtractGINDocumentTokens(ctx, tsLayout, tsVecVal, "tsvector_ops", enc, 1<<20)
	if err != nil || len(tsToks) != 3 {
		t.Fatalf("tsvector tokens = %d, %v; want 3", len(tsToks), err)
	}
	tsPlan, ok, err := builtin.ExtractGINQueryPlan(ctx, 3636, tsLayout, tsQVal, "tsvector_ops", enc, 1<<20)
	if err != nil || !ok || !tsPlan.RequireAll || len(tsPlan.Tokens) != 2 {
		t.Fatalf("tsvector @@ plan = %+v, %v, %v", tsPlan, ok, err)
	}
}

func TestGINExtractJSONBTopLevelArrayAndString(t *testing.T) {
	ctx := t.Context()
	enc, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatalf("NewKeyEncoder: %v", err)
	}
	jsonbLayout := model.TypeLayout{Type: model.Type{ID: 3802, Storage: model.TypeJSONB}}
	tokensOf := func(raw, opClass string) []string {
		t.Helper()
		toks, err := builtin.ExtractGINDocumentTokens(ctx, jsonbLayout, model.Value{Type: model.TypeJSONB, StrVal: raw}, opClass, enc, 1<<20)
		if err != nil {
			t.Fatalf("ExtractGINDocumentTokens(%s, %s): %v", raw, opClass, err)
		}
		out := make([]string, len(toks))
		for i, tk := range toks {
			out[i] = string(tk)
		}
		return out
	}

	// Top-level array of strings emits k:<elem> for direct string elements.
	arrToks := tokensOf(`["foo","bar",42,true]`, "jsonb_ops")
	for _, want := range []string{"k:foo", "k:bar", "v:s:foo", "v:s:bar"} {
		if !slices.Contains(arrToks, want) {
			t.Fatalf("top-level array tokens %v missing %q", arrToks, want)
		}
	}

	// Top-level string emits k:<str> and v:s:<str>.
	strToks := tokensOf(`"foo"`, "jsonb_ops")
	for _, want := range []string{"k:foo", "v:s:foo"} {
		if !slices.Contains(strToks, want) {
			t.Fatalf("top-level string tokens %v missing %q", strToks, want)
		}
	}

	// Nested arrays MUST NOT emit k:<elem> because ? only matches top-level elements.
	nestedToks := tokensOf(`[["foo"]]`, "jsonb_ops")
	if slices.Contains(nestedToks, "k:foo") {
		t.Fatalf("nested array tokens %v must not contain k:foo", nestedToks)
	}

	// jsonb_path_ops must not emit k: tokens.
	pathToks := tokensOf(`["foo","bar"]`, "jsonb_path_ops")
	if slices.Contains(pathToks, "k:foo") {
		t.Fatalf("jsonb_path_ops tokens %v must not contain k:foo", pathToks)
	}
}
