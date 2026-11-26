package pgwire

import (
	"encoding/binary"
	"testing"
)

func TestDecodeBinaryArrayAndNumericDoSRejection(t *testing.T) {
	t.Run("array_huge_dimLen", func(t *testing.T) {
		raw := make([]byte, 20)
		binary.BigEndian.PutUint32(raw[0:4], 1)            // ndim = 1
		binary.BigEndian.PutUint32(raw[4:8], 0)            // hasNull = 0
		binary.BigEndian.PutUint32(raw[8:12], oidInt4)     // elemOID = int4
		binary.BigEndian.PutUint32(raw[12:16], 0x7fffffff) // dimLen = MaxInt32
		binary.BigEndian.PutUint32(raw[16:20], 1)          // lbound = 1
		if _, err := decodeBinaryArrayToText(oidInt4, raw); err == nil {
			t.Fatal("expected error for oversized dimLen, got nil")
		}
	})

	t.Run("array_overflowing_dims", func(t *testing.T) {
		raw := make([]byte, 28)
		binary.BigEndian.PutUint32(raw[0:4], 2)            // ndim = 2
		binary.BigEndian.PutUint32(raw[4:8], 0)            // hasNull = 0
		binary.BigEndian.PutUint32(raw[8:12], oidInt4)     // elemOID = int4
		binary.BigEndian.PutUint32(raw[12:16], 0x40000001) // dim0
		binary.BigEndian.PutUint32(raw[16:20], 1)
		binary.BigEndian.PutUint32(raw[20:24], 0x40000001) // dim1 (overflows int64/uint64 product)
		binary.BigEndian.PutUint32(raw[24:28], 1)
		if _, err := decodeBinaryArrayToText(oidInt4, raw); err == nil {
			t.Fatal("expected error for overflowing 2D dims, got nil")
		}
	})

	t.Run("array_nested_elem_oid", func(t *testing.T) {
		raw := make([]byte, 24)
		binary.BigEndian.PutUint32(raw[0:4], 1)             // ndim = 1
		binary.BigEndian.PutUint32(raw[4:8], 0)             // hasNull = 0
		binary.BigEndian.PutUint32(raw[8:12], oidInt4Array) // nested array OID
		binary.BigEndian.PutUint32(raw[12:16], 1)
		binary.BigEndian.PutUint32(raw[16:20], 1)
		binary.BigEndian.PutUint32(raw[20:24], 0xFFFFFFFF) // NULL elem
		if _, err := decodeBinaryArrayToText(0, raw); err == nil {
			t.Fatal("expected error for nested array elemOID, got nil")
		}
	})

	t.Run("array_mismatched_elem_oid", func(t *testing.T) {
		raw := make([]byte, 24)
		binary.BigEndian.PutUint32(raw[0:4], 1)
		binary.BigEndian.PutUint32(raw[4:8], 0)
		binary.BigEndian.PutUint32(raw[8:12], oidText) // mismatch with expected oidInt4
		binary.BigEndian.PutUint32(raw[12:16], 1)
		binary.BigEndian.PutUint32(raw[16:20], 1)
		binary.BigEndian.PutUint32(raw[20:24], 0xFFFFFFFF)
		if _, err := decodeBinaryArrayToText(oidInt4, raw); err == nil {
			t.Fatal("expected error for mismatched elemOID, got nil")
		}
	})

	t.Run("numeric_amplification_weight_dscale", func(t *testing.T) {
		// 10-byte crafted numeric: ndigits=1, weight=32767, sign=0, dscale=65535, digit=1
		raw := make([]byte, 10)
		binary.BigEndian.PutUint16(raw[0:2], 1)     // ndigits = 1
		binary.BigEndian.PutUint16(raw[2:4], 32767) // weight = 32767
		binary.BigEndian.PutUint16(raw[4:6], 0)     // sign = positive
		binary.BigEndian.PutUint16(raw[6:8], 65535) // dscale = 65535
		binary.BigEndian.PutUint16(raw[8:10], 1)
		if _, err := decodeBinaryNumeric(raw); err == nil {
			t.Fatal("expected error for extreme numeric weight/dscale, got nil")
		}

		// Valid weight but extreme dscale
		binary.BigEndian.PutUint16(raw[2:4], 0)
		binary.BigEndian.PutUint16(raw[6:8], 65535)
		if _, err := decodeBinaryNumeric(raw); err == nil {
			t.Fatal("expected error for extreme numeric dscale, got nil")
		}

		// Extreme negative weight
		binary.BigEndian.PutUint16(raw[2:4], 0x8000) // -32768
		binary.BigEndian.PutUint16(raw[6:8], 2)
		if _, err := decodeBinaryNumeric(raw); err == nil {
			t.Fatal("expected error for extreme negative numeric weight, got nil")
		}
	})
}
