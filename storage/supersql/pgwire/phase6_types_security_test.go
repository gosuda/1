package pgwire

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
)

// buildBinaryNumericArray builds a PostgreSQL binary numeric[] wire payload
// containing dims and the provided raw binary numeric elements (nil for NULL).
func buildBinaryNumericArray(dims []int, elems [][]byte) []byte {
	hasNull := uint32(0)
	bodyLen := 0
	for _, e := range elems {
		bodyLen += 4
		if e == nil {
			hasNull = 1
		} else {
			bodyLen += len(e)
		}
	}
	out := make([]byte, 12+8*len(dims)+bodyLen)
	binary.BigEndian.PutUint32(out[0:4], uint32(len(dims)))
	binary.BigEndian.PutUint32(out[4:8], hasNull)
	binary.BigEndian.PutUint32(out[8:12], oidNumeric)
	pos := 12
	for _, d := range dims {
		binary.BigEndian.PutUint32(out[pos:pos+4], uint32(d))
		binary.BigEndian.PutUint32(out[pos+4:pos+8], 1) // lbound = 1
		pos += 8
	}
	for _, e := range elems {
		if e == nil {
			binary.BigEndian.PutUint32(out[pos:pos+4], 0xFFFFFFFF)
			pos += 4
			continue
		}
		binary.BigEndian.PutUint32(out[pos:pos+4], uint32(len(e)))
		pos += 4
		copy(out[pos:pos+len(e)], e)
		pos += len(e)
	}
	return out
}

// makeCraftedNumericElem creates a 10-byte binary numeric element:
// ndigits=1, weight=w, sign=0 (positive), dscale=ds, digit[0]=1.
// With w=2000, ds=1000, this 10-byte wire value (14 bytes with length prefix)
// expands to 9002 text bytes (~643x amplification).
func makeCraftedNumericElem(w int16, ds uint16) []byte {
	elem := make([]byte, 10)
	binary.BigEndian.PutUint16(elem[0:2], 1)
	binary.BigEndian.PutUint16(elem[2:4], uint16(w))
	binary.BigEndian.PutUint16(elem[4:6], numericSignPos)
	binary.BigEndian.PutUint16(elem[6:8], ds)
	binary.BigEndian.PutUint16(elem[8:10], 1)
	return elem
}

func TestPhase6TypesNumericArrayAmplificationBudget(t *testing.T) {
	t.Run("single_max_valid_numeric_element_succeeds", func(t *testing.T) {
		elem := makeCraftedNumericElem(2000, 1000)
		raw := buildBinaryNumericArray([]int{1}, [][]byte{elem})
		got, err := decodeBinaryArrayToText(oidNumeric, raw)
		if err != nil {
			t.Fatalf("single valid numeric element failed unexpectedly: %v", err)
		}
		// "{1" + 2000*4 zeros + "." + 1000 zeros + "}" = 2 + 8000 + 1 + 1000 + 1 = 9004 bytes.
		wantLen := 1 + (1 + 2000*4 + 1 + 1000) + 1
		if len(got) != wantLen {
			t.Fatalf("decoded length = %d, want %d", len(got), wantLen)
		}
	})

	t.Run("4000_amplified_elements_rejected_by_budget", func(t *testing.T) {
		// 4000 * 14 bytes = ~56 KiB wire payload that would expand to ~36 MiB (> 16 MiB cap).
		elem := makeCraftedNumericElem(2000, 1000)
		const count = 4000
		elems := make([][]byte, count)
		for i := range elems {
			elems[i] = elem
		}
		raw := buildBinaryNumericArray([]int{count}, elems)
		if len(raw) > 64*1024 {
			t.Fatalf("test wire payload unexpectedly large: %d bytes", len(raw))
		}

		if _, err := decodeBinaryArrayToText(oidNumeric, raw); err == nil {
			t.Fatal("expected decoded-byte budget error for 4000 amplified numeric[] elements, got nil")
		}

		// Also verify decodeBinaryParam maps this cleanly to SQLSTATE 22P03.
		_, err := decodeBinaryParam(oidNumericArray, raw)
		if err == nil {
			t.Fatal("expected error from decodeBinaryParam on amplified numeric[], got nil")
		}
		var we *wireError
		if !errors.As(err, &we) || we.Code != "22P03" {
			t.Fatalf("expected wireError 22P03, got %T (%v)", err, err)
		}
	})

	t.Run("text_quoting_expansion_rejected_by_budget", func(t *testing.T) {
		// 9 MiB of '"' quotes in a single text element expands to ~18 MiB (> 16 MiB cap).
		quotePayload := []byte(strings.Repeat(`"`, 9<<20))
		raw := make([]byte, 12+8+4+len(quotePayload))
		binary.BigEndian.PutUint32(raw[0:4], 1)
		binary.BigEndian.PutUint32(raw[4:8], 0)
		binary.BigEndian.PutUint32(raw[8:12], oidText)
		binary.BigEndian.PutUint32(raw[12:16], 1)
		binary.BigEndian.PutUint32(raw[16:20], 1)
		binary.BigEndian.PutUint32(raw[20:24], uint32(len(quotePayload)))
		copy(raw[24:], quotePayload)

		if _, err := decodeBinaryArrayToText(oidText, raw); err == nil {
			t.Fatal("expected decoded-byte budget error for quoting-amplified text[], got nil")
		}
	})
}

func TestPhase6TypesNormalBinaryNumericAndArrayDecode(t *testing.T) {
	t.Run("2x3_numeric_array_with_nulls_negatives_specials", func(t *testing.T) {
		mustEnc := func(s string) []byte {
			t.Helper()
			b, err := encodeBinaryNumeric(s)
			if err != nil {
				t.Fatalf("encodeBinaryNumeric(%q): %v", s, err)
			}
			return b
		}
		raw := buildBinaryNumericArray(
			[]int{2, 3},
			[][]byte{
				mustEnc("12345.6789"),
				nil,
				mustEnc("-0.0005"),
				mustEnc("0"),
				mustEnc("NaN"),
				mustEnc("Infinity"),
			},
		)
		got, err := decodeBinaryArrayToText(oidNumeric, raw)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText: %v", err)
		}
		want := "{{12345.6789,NULL,-0.0005},{0,NaN,Infinity}}"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}

		// Round-trip text -> binary -> text.
		reencoded, err := encodeBinaryArrayFromText(oidNumeric, got)
		if err != nil {
			t.Fatalf("encodeBinaryArrayFromText: %v", err)
		}
		roundTripped, err := decodeBinaryArrayToText(oidNumeric, reencoded)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText(reencoded): %v", err)
		}
		if roundTripped != want {
			t.Fatalf("round-trip got %q, want %q", roundTripped, want)
		}
	})

	t.Run("numeric_neg_infinity_and_empty_array", func(t *testing.T) {
		ninf, err := encodeBinaryNumeric("-Infinity")
		if err != nil {
			t.Fatalf("encodeBinaryNumeric(-Infinity): %v", err)
		}
		raw := buildBinaryNumericArray([]int{1}, [][]byte{ninf})
		got, err := decodeBinaryArrayToText(oidNumeric, raw)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText: %v", err)
		}
		if got != "{-Infinity}" {
			t.Fatalf("got %q, want {-Infinity}", got)
		}

		emptyRaw, err := encodeBinaryArrayFromText(oidNumeric, "{}")
		if err != nil {
			t.Fatalf("encodeBinaryArrayFromText({}): %v", err)
		}
		emptyGot, err := decodeBinaryArrayToText(oidNumeric, emptyRaw)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText(empty): %v", err)
		}
		if emptyGot != "{}" {
			t.Fatalf("empty got %q, want {}", emptyGot)
		}
	})

	t.Run("text_array_streaming_quoting_roundtrip", func(t *testing.T) {
		in := `{hello,"world,1","a\"b\\c","",NULL,"NULL"}`
		bin, err := encodeBinaryArrayFromText(oidText, in)
		if err != nil {
			t.Fatalf("encodeBinaryArrayFromText: %v", err)
		}
		got, err := decodeBinaryArrayToText(oidText, bin)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText: %v", err)
		}
		if got != in {
			t.Fatalf("got %q, want %q", got, in)
		}
	})
}

func TestPhase6TypesEncodeBinaryNumericBounds(t *testing.T) {
	t.Run("rejects_huge_scientific_exponents", func(t *testing.T) {
		badInputs := []string{
			"1e999999999",
			"1e-999999999",
			"1E147456",
			"1e-147456",
			"1e131072",
			"1e-16384",
			"1e999999999999999999999999",
			strings.Repeat("9", 100000) + "e50000",
			"0." + strings.Repeat("1", 10000) + "e-10000",
			"",
			".",
			"-",
			"1.2.3",
			"12a34",
		}
		for _, in := range badInputs {
			if _, err := encodeBinaryNumeric(in); err == nil {
				t.Errorf("encodeBinaryNumeric(%q) = nil error, want error", in[:min(len(in), 32)])
			}
		}
	})

	t.Run("rejects_more_than_32767_base10000_digits_without_int16_wrap", func(t *testing.T) {
		// 32768 base-10000 digits = 131072 non-zero decimal digits -> would wrap uint16(32768) into int16(-32768).
		overInt := strings.Repeat("9", 32768*4)
		if _, err := encodeBinaryNumeric(overInt); err == nil {
			t.Fatal("expected error for 32768 base-10000 digits, got nil")
		}

		// 32767 integer groups + 1 fractional group = 32768 total digits -> must also be rejected.
		overCombined := strings.Repeat("9", 32767*4) + ".1234"
		if _, err := encodeBinaryNumeric(overCombined); err == nil {
			t.Fatal("expected error for 32768 combined base-10000 digits, got nil")
		}

		// 131073 integer digits ("1" + 131072 zeros) -> weight = 32768 > math.MaxInt16.
		overWeight := "1" + strings.Repeat("0", 131072)
		if _, err := encodeBinaryNumeric(overWeight); err == nil {
			t.Fatal("expected error for weight=32768 exceeding int16, got nil")
		}

		// dscale = 16384 > PG max 16383.
		overDscale := "0." + strings.Repeat("1", 16384)
		if _, err := encodeBinaryNumeric(overDscale); err == nil {
			t.Fatal("expected error for dscale=16384 exceeding 16383, got nil")
		}
	})

	t.Run("valid_boundary_and_scientific_values", func(t *testing.T) {
		// Exact max ndigits = 32767 (all 9s, 131068 decimal digits).
		maxDigitsStr := strings.Repeat("9", 32767*4)
		enc, err := encodeBinaryNumeric(maxDigitsStr)
		if err != nil {
			t.Fatalf("encodeBinaryNumeric(32767 groups): %v", err)
		}
		ndigits := int16(binary.BigEndian.Uint16(enc[0:2]))
		weight := int16(binary.BigEndian.Uint16(enc[2:4]))
		if ndigits != math.MaxInt16 {
			t.Fatalf("ndigits = %d, want %d", ndigits, math.MaxInt16)
		}
		if weight != math.MaxInt16-1 {
			t.Fatalf("weight = %d, want %d", weight, math.MaxInt16-1)
		}

		// Exact max weight = 32767 ("1" + 131071 zeros -> 1000 * 10000^32767, 1 non-zero group).
		maxWeightStr := "1" + strings.Repeat("0", 131071)
		encW, err := encodeBinaryNumeric(maxWeightStr)
		if err != nil {
			t.Fatalf("encodeBinaryNumeric(weight=32767): %v", err)
		}
		if gotW := int16(binary.BigEndian.Uint16(encW[2:4])); gotW != math.MaxInt16 {
			t.Fatalf("weight = %d, want %d", gotW, math.MaxInt16)
		}

		// Exact max dscale = 16383 via scientific notation.
		encS, err := encodeBinaryNumeric("1e-16383")
		if err != nil {
			t.Fatalf("encodeBinaryNumeric(1e-16383): %v", err)
		}
		if gotDS := binary.BigEndian.Uint16(encS[6:8]); gotDS != 16383 {
			t.Fatalf("dscale = %d, want 16383", gotDS)
		}

		// Normal scientific notation round-trip through decodeBinaryNumeric.
		cases := []struct {
			in   string
			want string
		}{
			{"1.25e2", "125"},
			{"125e-2", "1.25"},
			{"-3.5e-3", "-0.0035"},
			{"00042.50", "42.50"},
			{"-0.00", "0.00"},
		}
		for _, tc := range cases {
			raw, err := encodeBinaryNumeric(tc.in)
			if err != nil {
				t.Fatalf("encodeBinaryNumeric(%q): %v", tc.in, err)
			}
			got, err := decodeBinaryNumeric(raw)
			if err != nil {
				t.Fatalf("decodeBinaryNumeric(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("round-trip %q = %q, want %q", tc.in, got, tc.want)
			}
		}
	})
}

func TestPhase6TypesParsePGArrayLiteralDepthAndRectangularity(t *testing.T) {
	t.Run("rejects_10000_nested_braces_without_stack_overflow", func(t *testing.T) {
		deep := strings.Repeat("{", 10000) + "1" + strings.Repeat("}", 10000)
		pos := 0
		if _, err := parsePGArrayLiteral(deep, &pos); err == nil {
			t.Fatal("expected depth limit error for 10,000 nested '{', got nil")
		}
		if _, err := encodeBinaryArrayFromText(oidInt4, deep); err == nil {
			t.Fatal("expected depth limit error from encodeBinaryArrayFromText, got nil")
		}
	})

	t.Run("enforces_pg_maxdim_6", func(t *testing.T) {
		sixDims := "{{{{{{1}}}}}}"
		pos := 0
		if _, err := parsePGArrayLiteral(sixDims, &pos); err != nil {
			t.Fatalf("6D array should succeed, got error: %v", err)
		}
		bin, err := encodeBinaryArrayFromText(oidInt4, sixDims)
		if err != nil {
			t.Fatalf("encodeBinaryArrayFromText(6D): %v", err)
		}
		got, err := decodeBinaryArrayToText(oidInt4, bin)
		if err != nil {
			t.Fatalf("decodeBinaryArrayToText(6D): %v", err)
		}
		if got != sixDims {
			t.Fatalf("6D round-trip = %q, want %q", got, sixDims)
		}

		sevenDims := "{{{{{{{1}}}}}}}"
		pos = 0
		if _, err := parsePGArrayLiteral(sevenDims, &pos); err == nil {
			t.Fatal("expected error for 7D array literal exceeding MAXDIM=6, got nil")
		}
	})

	t.Run("rejects_jagged_and_malformed_arrays", func(t *testing.T) {
		jaggedCases := []struct {
			name string
			lit  string
		}{
			{"short_second_row", "{{1,2},{3}}"},
			{"long_second_row", "{{1},{2,3}}"},
			{"short_third_row", "{{1,2},{3,4},{5}}"},
			{"mixed_subarray_then_scalar", "{{1,2},3}"},
			{"mixed_scalar_then_subarray", "{1,{2,3}}"},
			{"jagged_3d_inner", "{{{1,2},{3,4}},{{5,6},{7}}}"},
			{"jagged_3d_middle", "{{{1,2}},{{3,4},{5,6}}}"},
			{"empty_nested_subarray", "{{}}"},
			{"empty_sibling_subarrays", "{{},{}}"},
			{"mixed_empty_subarray", "{{1},{}}"},
		}
		for _, tc := range jaggedCases {
			t.Run(tc.name, func(t *testing.T) {
				pos := 0
				if _, err := parsePGArrayLiteral(tc.lit, &pos); err == nil {
					t.Errorf("parsePGArrayLiteral(%q) = nil error, want error", tc.lit)
				}
				if _, err := encodeBinaryArrayFromText(oidInt4, tc.lit); err == nil {
					t.Errorf("encodeBinaryArrayFromText(%q) = nil error, want error", tc.lit)
				}
			})
		}

		// Trailing garbage after valid array literal.
		if _, err := encodeBinaryArrayFromText(oidInt4, "{1,2}extra"); err == nil {
			t.Error("expected error for trailing characters after array literal, got nil")
		}
	})

	t.Run("accepts_valid_rectangular_multidim_arrays", func(t *testing.T) {
		validCases := []string{
			"{{1,2},{3,4}}",
			"{{{1,2},{3,4}},{{5,6},{7,NULL}}}",
		}
		for _, lit := range validCases {
			bin, err := encodeBinaryArrayFromText(oidInt4, lit)
			if err != nil {
				t.Fatalf("encodeBinaryArrayFromText(%q): %v", lit, err)
			}
			got, err := decodeBinaryArrayToText(oidInt4, bin)
			if err != nil {
				t.Fatalf("decodeBinaryArrayToText(%q): %v", lit, err)
			}
			if got != lit {
				t.Fatalf("round-trip %q = %q", lit, got)
			}
		}
	})
}
