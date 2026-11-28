package builtin_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func uuidScalarDatum(text string) model.Value {
	return model.Value{Type: model.TypeUUID, StrVal: text}
}

func TestUUIDTextIOCanonicalizesPostgresForms(t *testing.T) {
	const canonical = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	for _, operation := range []struct {
		name   string
		input  model.TypeID
		output model.DataType
	}{
		{"uuid_in", 2275, model.TypeUUID},
		{"uuid_out", 2950, model.TypeString},
	} {
		t.Run(operation.name, func(t *testing.T) {
			function := callable(t, operation.name, operation.input)
			for _, tc := range []struct {
				name string
				text string
			}{
				{"canonical", canonical},
				{"uppercase", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11"},
				{"compact", "a0eebc999c0b4ef8bb6d6bb9bd380a11"},
				{"braces", "{a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}"},
				{"every_four_digits", "{A0EE-BC99-9C0B-4EF8-BB6D-6BB9-BD38-0A11}"},
				{"mixed_optional_hyphens", "a0ee-bc999c0b-4ef8bb6d6bb9-bd380a11"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					input := uuidScalarDatum(tc.text)
					if operation.input == 2275 {
						input = model.StringVal(tc.text)
					}
					value, err := function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
					if err != nil || value.IsNull || value.Type != operation.output || value.StrVal != canonical {
						t.Fatalf("%s(%q) = %v, %v; want canonical %q of type %v", operation.name, tc.text, value, err, canonical, operation.output)
					}
				})
			}
		})
	}
}

func TestUUIDTextIOOwnsCanonicalResultAtExactBudget(t *testing.T) {
	const canonical = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	const next = "00000000-0000-0000-0000-000000000000"
	for _, name := range []string{"uuid_in", "uuid_out"} {
		t.Run(name, func(t *testing.T) {
			inputType := model.TypeID(2950)
			input := uuidScalarDatum(strings.Clone(canonical))
			if name == "uuid_in" {
				inputType = 2275
				input.Type = model.TypeString
			}
			borrowed := unsafe.StringData(input.StrVal)
			function := callable(t, name, inputType)
			args := []model.Value{input}
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 35, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
			first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
			if err != nil || first.IsNull || first.StrVal != canonical {
				t.Fatalf("exact-budget UUID result = %v, %v", first, err)
			}
			if unsafe.StringData(first.StrVal) == borrowed {
				t.Fatal("canonical UUID result retains borrowed text storage")
			}
			args[0] = model.Value{Type: input.Type, StrVal: next}
			second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
			if err != nil || second.IsNull || second.StrVal != next {
				t.Fatalf("later UUID result = %v, %v", second, err)
			}
			if first.StrVal != canonical {
				t.Fatalf("later call changed retained UUID result to %q", first.StrVal)
			}
		})
	}
}

func TestUUIDInputRejectsMalformedText(t *testing.T) {
	function := callable(t, "uuid_in", 2275)
	for _, tc := range []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"too_few_digits", "0000000000000000000000000000000"},
		{"too_many_digits", "000000000000000000000000000000000"},
		{"non_hex", "00000000-0000-0000-0000-00000000000g"},
		{"hyphen_after_two_digits", "00-000000000000000000000000000000"},
		{"hyphen_inside_byte", "0-0000000000000000000000000000000"},
		{"double_hyphen", "00000000--0000-0000-0000-000000000000"},
		{"trailing_hyphen", "00000000-0000-0000-0000-000000000000-"},
		{"missing_closing_brace", "{00000000000000000000000000000000"},
		{"missing_opening_brace", "00000000000000000000000000000000}"},
		{"nested_braces", "{{00000000000000000000000000000000}}"},
		{"suffix_after_braces", "{00000000000000000000000000000000}x"},
		{"leading_space", " 00000000000000000000000000000000"},
		{"trailing_newline", "00000000000000000000000000000000\n"},
		{"internal_space", "0000 0000000000000000000000000000"},
		{"embedded_nul", "00000000-0000-0000-0000-00000000000\x00"},
		{"nul_suffix", "00000000000000000000000000000000\x00"},
		{"non_ascii", "00000000-0000-0000-0000-00000000000é"},
		{"oversized", strings.Repeat("0", 1<<16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.text)}, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22P02")
		})
	}
}

func TestUUIDKernelsValidateEntireTypedValues(t *testing.T) {
	const malformed = "00000000-0000-0000-0000-00000000000g"
	const valid = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	for _, name := range []string{"uuid_out", "uuid_send", "uuid_extract_version", "uuid_lt", "uuid_le", "uuid_eq", "uuid_ge", "uuid_gt", "uuid_ne", "uuid_cmp"} {
		t.Run(name, func(t *testing.T) {
			types := []model.TypeID{2950}
			args := []model.Value{uuidScalarDatum(valid)}
			if name != "uuid_out" && name != "uuid_send" && name != "uuid_extract_version" {
				types = append(types, 2950)
				args = append(args, uuidScalarDatum(valid))
			}
			function := callable(t, name, types...)
			for i := range args {
				invalid := append([]model.Value(nil), args...)
				invalid[i] = uuidScalarDatum(malformed)
				_, err := function.Call(t.Context(), invalid, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22P02")
			}
		})
	}
}

func TestUUIDComparisonsUseUnsignedBytesAcrossSpellings(t *testing.T) {
	const lower = "7f000000-0000-4000-8000-ffffffffffff"
	const greater = "80000000-0000-4000-8000-000000000000"
	const equal = "{7F00-0000-0000-4000-8000-FFFF-FFFF-FFFF}"
	for _, tc := range []struct {
		name string
		want [3]int64
	}{
		{"uuid_lt", [3]int64{1, 0, 0}},
		{"uuid_le", [3]int64{1, 0, 1}},
		{"uuid_eq", [3]int64{0, 0, 1}},
		{"uuid_ge", [3]int64{0, 1, 1}},
		{"uuid_gt", [3]int64{0, 1, 0}},
		{"uuid_ne", [3]int64{1, 1, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := callable(t, tc.name, 2950, 2950)
			for i, operands := range [][2]string{{lower, greater}, {greater, lower}, {lower, equal}} {
				value, err := function.Call(t.Context(), []model.Value{uuidScalarDatum(operands[0]), uuidScalarDatum(operands[1])}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || value.IsNull || value.Type != model.TypeBoolean || value.IntVal != tc.want[i] {
					t.Errorf("%s(%q, %q) = %v, %v; want %d", tc.name, operands[0], operands[1], value, err, tc.want[i])
				}
			}
		})
	}
}

func TestUUIDCmpUsesByteOrdering(t *testing.T) {
	function := callable(t, "uuid_cmp", 2950, 2950)
	for _, tc := range []struct {
		name        string
		left, right string
		want        int64
	}{
		{"unsigned_negative", "00000000-0000-0000-0000-000000000000", "ff000000-0000-0000-0000-000000000000", -1},
		{"unsigned_positive", "ff000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000", 1},
		{"last_byte", "00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000012", -1},
		{"first_difference_wins", "11000000-0000-0000-0000-ffffffffffff", "33000000-0000-0000-0000-000000000000", -1},
		{"equal_alternate_spelling", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "{A0EEBC999C0B4EF8BB6D6BB9BD380A11}", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{uuidScalarDatum(tc.left), uuidScalarDatum(tc.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeInt64 || int64(cmp.Compare(value.IntVal, 0)) != tc.want {
				t.Fatalf("uuid_cmp = %v, %v; want sign %d", value, err, tc.want)
			}
		})
	}
}

func TestUUIDComparisonsDoNotAllocatePayloads(t *testing.T) {
	ctx := t.Context()
	args := []model.Value{
		uuidScalarDatum("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"),
		uuidScalarDatum("{A0EE-BC99-9C0B-4EF8-BB6D-6BB9-BD38-0A12}"),
	}
	for _, name := range []string{"uuid_lt", "uuid_le", "uuid_eq", "uuid_ge", "uuid_gt", "uuid_ne", "uuid_cmp"} {
		t.Run(name, func(t *testing.T) {
			function := callable(t, name, 2950, 2950)
			allocations := testing.AllocsPerRun(100, func() {
				_, err := function.Call(ctx, args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
			})
			if allocations != 0 {
				t.Fatalf("UUID comparison allocations = %g, want no per-call heap payload", allocations)
			}
		})
	}
}

func TestUUIDSendOwnsRawBytesAtExactBudget(t *testing.T) {
	function := callable(t, "uuid_send", 2950)
	args := []model.Value{uuidScalarDatum("{0011-2233-4455-6677-8899-AABB-CCDD-EEFF}")}
	want := []byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}
	_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 15, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
	if err != nil || first.IsNull || first.Type != model.TypeBytes || !bytes.Equal(first.BytesVal, want) {
		t.Fatalf("uuid_send = %v, %v; want %x", first, err, want)
	}
	if cap(first.BytesVal) != 16 {
		t.Fatalf("uuid_send retained capacity = %d, want charged 16 bytes", cap(first.BytesVal))
	}
	second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
	if err != nil || !bytes.Equal(second.BytesVal, want) {
		t.Fatalf("second uuid_send = %v, %v", second, err)
	}
	first.BytesVal[0] = 0xff
	if !bytes.Equal(second.BytesVal, want) {
		t.Fatalf("uuid_send outputs share mutable storage: %x", second.BytesVal)
	}
}

func TestUUIDVersionRequiresRFCVariant(t *testing.T) {
	function := callable(t, "uuid_extract_version", 2950)
	for _, tc := range []struct {
		name string
		text string
		want int64
		null bool
	}{
		{"version_one", "00000000-0000-1fff-8000-000000000000", 1, false},
		{"version_four", "00000000-0000-4fff-9000-000000000000", 4, false},
		{"version_seven", "00000000-0000-7fff-a000-000000000000", 7, false},
		{"future_version", "{000000000000FFFFB000000000000000}", 15, false},
		{"version_zero", "00000000-0000-0fff-8000-000000000000", 0, false},
		{"nil_uuid", "00000000-0000-0000-0000-000000000000", 0, true},
		{"ncs_variant", "00000000-0000-4fff-7000-000000000000", 0, true},
		{"microsoft_variant", "00000000-0000-4fff-c000-000000000000", 0, true},
		{"reserved_variant", "00000000-0000-4fff-e000-000000000000", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{uuidScalarDatum(tc.text)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			wrongVersion := !tc.null && value.IntVal != tc.want
			if err != nil || value.Type != model.TypeInt64 || value.IsNull != tc.null || wrongVersion {
				t.Fatalf("uuid_extract_version(%q) = %v, %v; want version %d, NULL %v", tc.text, value, err, tc.want, tc.null)
			}
		})
	}
}

func TestUUIDStrictNullPrecedesParsingAndBudget(t *testing.T) {
	for _, id := range []model.FunctionID{2952, 2953, 2954, 2955, 2956, 2957, 2958, 2959, 2960, 2962, 6343} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(function.Signature().Name, func(t *testing.T) {
			argument := uuidScalarDatum("malformed")
			resultType := model.TypeBoolean
			switch id {
			case 2952:
				argument.Type = model.TypeString
				resultType = model.TypeUUID
			case 2953:
				resultType = model.TypeString
			case 2960, 6343:
				resultType = model.TypeInt64
			case 2962:
				resultType = model.TypeBytes
			}
			args := []model.Value{argument}
			if function.Signature().ArgumentCount == 2 {
				args = append(args, argument)
			}
			for i := range args {
				nullArgs := append([]model.Value(nil), args...)
				nullArgs[i].IsNull = true
				value, err := function.Call(t.Context(), nullArgs, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != resultType {
					t.Errorf("NULL argument %d = %v, %v; want NULL type %v", i, value, err, resultType)
				}
			}
		})
	}
}

func TestUUIDCancellation(t *testing.T) {
	for _, id := range []model.FunctionID{2952, 2953, 2954, 2955, 2956, 2957, 2958, 2959, 2960, 2962, 6343} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(function.Signature().Name, func(t *testing.T) {
			argument := uuidScalarDatum("00000000-0000-4000-8000-000000000000")
			if id == 2952 {
				argument.Type = model.TypeString
			}
			args := []model.Value{argument}
			if function.Signature().ArgumentCount == 2 {
				args = append(args, argument)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, args, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled UUID call = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 3}
			_, err = function.Call(interrupted, args, builtin.CallBudget{OutputBytes: 36, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted UUID parsing = %v", err)
			}
		})
	}
}
