package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestJSONBPrettyRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"mixed", `{"zz":0,"a":[{},[],{"x":[1,true,null]}],"zz":2}`, "{\n    \"a\": [\n        {\n        },\n        [\n        ],\n        {\n            \"x\": [\n                1,\n                true,\n                null\n            ]\n        }\n    ],\n    \"zz\": 2\n}"},
		{"empty object", `{}`, "{\n}"},
		{"empty array", `[]`, "[\n]"},
		{"object containers", `{"a":{},"b":[]}`, "{\n    \"a\": {\n    },\n    \"b\": [\n    ]\n}"},
		{"decoded duplicate", `{"é":0,"aa":1,"a":false,"\u0061":true}`, "{\n    \"a\": true,\n    \"aa\": 1,\n    \"é\": 0\n}"},
		{"numeric scale", `[1.2300e2,-0.00,9007199254740993]`, "[\n    123.00,\n    0.00,\n    9007199254740993\n]"},
		{"unicode controls", `"\u00e9\ud83d\ude00\/\b\f\n\r\t\u001f\"\\"`, `"é😀/\b\f\n\r\t\u001f\"\\"`},
		{"root number", `1.20e-2`, `0.0120`},
		{"root null", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := compileJSONBPretty(Signature{ID: 3306})(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: tc.input}}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("pretty = %q (%v), %v; want %q", value.StrVal, value.Type, err, tc.want)
			}
		})
	}
}

func requirePrettySQLState(t *testing.T, err error, want string) {
	t.Helper()
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != want {
		t.Fatalf("error = %v; want SQLSTATE %s", err, want)
	}
}

func TestJSONBPrettyRejectsInvalidRawValues(t *testing.T) {
	for _, tc := range []struct{ input, state string }{
		{`{"a":1e131072,"a":0}`, "22003"},
		{`{"a":"\u0000","a":0}`, "22P05"},
		{`["\ud800"]`, "22P02"},
		{"[\"\xff\"]", "22021"},
		{`[1,]`, "22P02"},
		{"null\x00", "22P02"},
	} {
		_, err := jsonbPretty(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: tc.input}}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requirePrettySQLState(t, err, tc.state)
	}
}

func TestJSONBPrettyExactPeakBudget(t *testing.T) {
	for _, tc := range []struct {
		input, want           string
		nodes, depth, decoded int64
	}{
		{`{"a":0,"\u0061":[]}`, "{\n    \"a\": [\n    ]\n}", 3, 2, 2},
		{`["\u0001",1e3]`, "[\n    \"\\u0001\",\n    1000\n]", 3, 1, 1},
		{`true`, "true", 1, 0, 0},
	} {
		budget := tc.nodes*int64(unsafe.Sizeof(jsonbNode{})) + tc.depth*int64(unsafe.Sizeof(jsonbFrame{})) + tc.decoded + int64(len(tc.want))
		args := []model.Value{{Type: model.TypeJSONB, StrVal: tc.input}}
		value, err := jsonbPretty(t.Context(), args, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("exact peak %d = %q, %v; want %q", budget, value.StrVal, err, tc.want)
		}
		_, err = jsonbPretty(t.Context(), args, CallBudget{OutputBytes: budget - 1, MemoryBytes: 64 << 20})
		requirePrettySQLState(t, err, "54000")
	}
}

func TestJSONBPrettyOwnsResult(t *testing.T) {
	for _, input := range []string{"true", `"plain"`, `1e2`, `{"a":[],"discard":"` + strings.Repeat("x", 1<<16) + `","discard":null}`} {
		value, err := jsonbPretty(t.Context(), []model.Value{{Type: model.TypeJSONB, StrVal: input}}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		start := uintptr(unsafe.Pointer(unsafe.StringData(input)))
		result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
		if result >= start && result < start+uintptr(len(input)) {
			t.Fatal("pretty result retains borrowed input")
		}
	}
}

type prettyCancelContext struct {
	context.Context
	remaining int
}

func (ctx *prettyCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining <= 0 {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestJSONBPrettyCancellation(t *testing.T) {
	ctx := &prettyCancelContext{Context: t.Context(), remaining: 4}
	_, err := jsonbPretty(ctx, []model.Value{{Type: model.TypeJSONB, StrVal: `["` + strings.Repeat("x", 1<<20) + `"]`}}, CallBudget{OutputBytes: 1 << 24, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parse cancellation = %v", err)
	}
	document, err := parseJSONBDocument(t.Context(), strings.Repeat("[", 100)+"0"+strings.Repeat("]", 100), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, emit := range []bool{false, true} {
		var output strings.Builder
		var target *strings.Builder
		if emit {
			target = &output
		}
		ctx := &prettyCancelContext{Context: t.Context(), remaining: 150}
		_, err := walkJSONBPretty(ctx, document, target)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("traversal cancellation (emit %t) = %v", emit, err)
		}
	}
	var output strings.Builder
	ctx = &prettyCancelContext{Context: t.Context(), remaining: 3}
	if err := appendJSONBIndent(ctx, &output, 6400); !errors.Is(err, context.Canceled) {
		t.Fatalf("indentation cancellation = %v", err)
	}
}
