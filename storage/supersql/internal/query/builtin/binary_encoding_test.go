package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func binaryEncodingCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireBinaryEncode(t *testing.T, input []byte, format, want string) {
	t.Helper()
	function := binaryEncodingCallable(t, 1946)
	input = bytes.Clone(input)
	original := bytes.Clone(input)
	args := []model.Value{model.BytesVal(input), model.StringVal(format)}
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != want {
		t.Fatalf("encode(%x, %q) = %v, %v; want %q", input, format, value, err, want)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("encoding changed borrowed input")
	}
	if len(want) > 0 {
		_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
	for i := range input {
		input[i] ^= 0xff
	}
	if value.StrVal != want {
		t.Fatalf("input mutation changed encoded result to %q; want %q", value.StrVal, want)
	}
}

func requireBinaryDecode(t *testing.T, input, format string, want []byte) {
	t.Helper()
	function := binaryEncodingCallable(t, 1947)
	args := []model.Value{model.StringVal(input), model.StringVal(format)}
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, want) {
		t.Fatalf("decode(%q, %q) = %v, %v; want %x", input, format, value, err, want)
	}
	if len(want) == 0 {
		return
	}
	result := uintptr(unsafe.Pointer(unsafe.SliceData(value.BytesVal)))
	for _, argument := range args {
		start := uintptr(unsafe.Pointer(unsafe.StringData(argument.StrVal)))
		if result >= start && result < start+uintptr(len(argument.StrVal)) {
			t.Fatal("decoded result retains borrowed string storage")
		}
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	for i := range value.BytesVal {
		value.BytesVal[i] ^= 0xff
	}
	again, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || !bytes.Equal(again.BytesVal, want) {
		t.Fatalf("result mutation changed subsequent decode to %x, %v; want %x", again.BytesVal, err, want)
	}
}

func TestBinaryEncodingHexPairs(t *testing.T) {
	requireBinaryEncode(t, []byte{0, 0x0f, 0x10, 0x7f, 0x80, 0xff}, "HeX", "000f107f80ff")
	requireBinaryDecode(t, "\t00 0F\n10\r7f\t80 ff\n", "hEX", []byte{0, 0x0f, 0x10, 0x7f, 0x80, 0xff})
}

func TestBinaryEncodingBase64LineBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length int
		want   string
	}{
		{"two padding characters complete line", 55, strings.Repeat("YWFh", 18) + "YQ=="},
		{"one padding character completes line", 56, strings.Repeat("YWFh", 18) + "YWE="},
		{"un-padded line ends with newline", 57, strings.Repeat("YWFh", 19) + "\n"},
		{"padded next line", 58, strings.Repeat("YWFh", 19) + "\nYQ=="},
		{"two complete lines", 114, strings.Repeat(strings.Repeat("YWFh", 19)+"\n", 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireBinaryEncode(t, bytes.Repeat([]byte{'a'}, tc.length), "BaSe64", tc.want)
		})
	}
}

func TestBinaryEncodingBase64DecodePaddingAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        []byte
	}{
		{"whitespace within quartet and padding", " Z \tm\r9\nv Z g =\t=\n", []byte("foof")},
		{"one byte nonzero unused bits", "Zh==", []byte("f")},
		{"two bytes nonzero unused bits", "Zm9=", []byte("fo")},
		{"data after first padding", "Zg=A", []byte("f")},
		{"one byte padding persists", "Zg==Zm9v", []byte("ff")},
		{"two byte padding persists", "Zm8=YmFy", []byte("foba")},
		{"extra padding forms one byte group", "Zg======", []byte{'f', 0}},
		{"extra padding forms two byte group", "Zm8=====", []byte{'f', 'o', 0, 0}},
		{"high octets", "//8=", []byte{0xff, 0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireBinaryDecode(t, tc.input, "BASE64", tc.want)
		})
	}
}

func TestBinaryEncodingEscapePreservesASCIIControls(t *testing.T) {
	input := []byte{0, 1, 8, 9, 10, 13, 31, ' ', '\\', 126, 127, 128, 255}
	want := `\000` + "\x01\b\t\n\r\x1f " + `\\` + "~\x7f" + `\200\377`
	requireBinaryEncode(t, input, "EsCaPe", want)
	requireBinaryDecode(t, want, "eSCAPE", input)
	requireBinaryDecode(t, `\1234\0123\\é`, "escape", []byte("S4\n3\\é"))
	requireBinaryDecode(t, "unchanged é", "escape", []byte("unchanged é"))
	requireBinaryEncode(t, []byte("unchanged"), "escape", "unchanged")
}

func TestBinaryEncodingEmptyOutputNeedsNoBudget(t *testing.T) {
	for _, format := range []string{"hex", "base64", "escape"} {
		t.Run(format, func(t *testing.T) {
			requireBinaryEncode(t, nil, format, "")
			requireBinaryDecode(t, "", format, nil)
			if format != "escape" {
				requireBinaryDecode(t, strings.Repeat(" \t\n\r", 4096), format, nil)
			}
		})
	}
}

func TestBinaryEncodingDecodeRejectsMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name, format, input, state string
	}{
		{"hex odd digits", "hex", "a", "22023"},
		{"hex whitespace splits pair", "hex", "a b", "22023"},
		{"hex incomplete pair before whitespace", "hex", "00a ", "22023"},
		{"hex rejects vertical tab", "hex", "00\v11", "22023"},
		{"hex rejects form feed", "hex", "00\f11", "22023"},
		{"hex rejects Unicode whitespace", "hex", "00\u00a011", "22023"},
		{"hex rejects prefix", "hex", `\x00`, "22023"},
		{"hex rejects nonhex character", "hex", "0g", "22023"},
		{"base64 missing both padding", "base64", "Zg", "22023"},
		{"base64 missing one padding", "base64", "Zm8", "22023"},
		{"base64 partial padding", "base64", "Zg=", "22023"},
		{"base64 padding at group start", "base64", "=AAA", "22023"},
		{"base64 padding after one digit", "base64", "A===", "22023"},
		{"base64 padding after unpadded group", "base64", "AAAA====", "22023"},
		{"base64 partial group after padding", "base64", "Zg==A", "22023"},
		{"base64 invalid symbol after padding", "base64", "Zg==?===", "22023"},
		{"base64 rejects vertical tab", "base64", "Zm\v9v", "22023"},
		{"base64 rejects form feed", "base64", "Zm\f9v", "22023"},
		{"base64 rejects Unicode whitespace", "base64", "Zm\u00a09v", "22023"},
		{"base64 rejects URL alphabet", "base64", "__8=", "22023"},
		{"escape trailing backslash", "escape", `\`, "22P02"},
		{"escape one octal digit", "escape", `\0`, "22P02"},
		{"escape two octal digits", "escape", `\00`, "22P02"},
		{"escape octal overflow", "escape", `\400`, "22P02"},
		{"escape invalid octal digit", "escape", `\080`, "22P02"},
		{"escape rejects C newline", "escape", `\n`, "22P02"},
		{"escape rejects hex notation", "escape", `\x41`, "22P02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := binaryEncodingCallable(t, 1947)
			_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), model.StringVal(tc.format)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.state)
		})
	}
}

func TestBinaryEncodingRejectsUnknownFormats(t *testing.T) {
	for _, id := range []model.FunctionID{1946, 1947} {
		function := binaryEncodingCallable(t, id)
		for _, format := range []string{"", " hex", "hex ", "base64url", "eſcape"} {
			t.Run(fmt.Sprintf("%d/%q", id, format), func(t *testing.T) {
				input := model.BytesVal(nil)
				if id == 1947 {
					input = model.StringVal("")
				}
				_, err := function.Call(t.Context(), []model.Value{input, model.StringVal(format)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22023")
			})
		}
	}
}

func TestBinaryEncodingRejectsInvalidText(t *testing.T) {
	for _, id := range []model.FunctionID{1946, 1947} {
		function := binaryEncodingCallable(t, id)
		signature := function.Signature()
		for _, invalid := range []string{"\xff", "\x00"} {
			for index, typ := range signature.Arguments[:2] {
				if typ != 25 {
					continue
				}
				args := []model.Value{model.BytesVal([]byte{0xff, 0}), model.StringVal("escape")}
				if id == 1947 {
					args[0] = model.StringVal("")
				}
				args[index] = model.StringVal(invalid)
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			}
		}
	}
}

func TestBinaryEncodingStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{1946, 1947} {
		function := binaryEncodingCallable(t, id)
		for nullIndex := range 2 {
			args := []model.Value{model.BytesVal([]byte{0xff, 0}), model.StringVal("\xff")}
			wantType := model.TypeString
			if id == 1947 {
				args[0] = model.StringVal("\xff")
				wantType = model.TypeBytes
			}
			args[nullIndex] = model.NullVal(args[nullIndex].Type)
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != wantType {
				t.Fatalf("function %d NULL operand %d = %v, %v", id, nullIndex, value, err)
			}
		}
	}
}

func TestBinaryEncodingCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		input       model.Value
		format      string
		checkpoints int
	}{
		{"hex output", 1946, model.BytesVal(bytes.Repeat([]byte{'a'}, 32768)), "hex", 4},
		{"base64 output", 1946, model.BytesVal(bytes.Repeat([]byte{'a'}, 32768)), "base64", 4},
		{"escape length scan", 1946, model.BytesVal(bytes.Repeat([]byte{0xff}, 32768)), "escape", 4},
		{"escape output", 1946, model.BytesVal(bytes.Repeat([]byte{0xff}, 32768)), "escape", 12},
		{"format validation", 1946, model.BytesVal(nil), strings.Repeat("x", 32768), 3},
		{"text validation", 1947, model.StringVal(strings.Repeat("41", 16384)), "hex", 4},
		{"hex length scan", 1947, model.StringVal(strings.Repeat("41", 16384)), "hex", 12},
		{"hex output", 1947, model.StringVal(strings.Repeat("41", 16384)), "hex", 20},
		{"base64 length scan", 1947, model.StringVal(strings.Repeat("YWFh", 8192)), "base64", 12},
		{"base64 output", 1947, model.StringVal(strings.Repeat("YWFh", 8192)), "base64", 20},
		{"escape length scan", 1947, model.StringVal(strings.Repeat(`\141`, 8192)), "escape", 12},
		{"escape output", 1947, model.StringVal(strings.Repeat(`\141`, 8192)), "escape", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := binaryEncodingCallable(t, tc.id)
			args := []model.Value{tc.input, model.StringVal(tc.format)}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.checkpoints}
			_, err = function.Call(interrupted, args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted call = %v", err)
			}
		})
	}
}
