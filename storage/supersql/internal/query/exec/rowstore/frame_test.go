package rowstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestFrameBytesMatchMemoryAndFile(t *testing.T) {
	fs := &faultFS{}
	q := testQuery(t, fs, runtime.QueryConfig{})
	limits := testLimits()
	layouts := []model.TypeLayout{testLayout(model.TypeString)}
	memory := newSpool(t, q, layouts, limits)
	limits.MemoryBytes = 0
	spill := newSpool(t, q, layouts, limits)
	row := []model.Value{model.StringVal("canonical\x00payload")}
	for _, spool := range []*Spool{memory, spill} {
		if _, err := spool.AppendRow(t.Context(), row); err != nil {
			t.Fatal(err)
		}
		if err := spool.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(fs.file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, memory.memory) {
		t.Fatal("memory and disk frames differ")
	}
	if string(data[:4]) != "RSW1" || binary.LittleEndian.Uint32(data[4:8]) != 1 || binary.LittleEndian.Uint64(data[8:16]) != 0 {
		t.Fatalf("frame header = %x", data[:24])
	}
	if uint64(len(data)-32) != binary.LittleEndian.Uint64(data[16:24]) {
		t.Fatal("incorrect payload extent")
	}
	if crc32.ChecksumIEEE(data[32:]) != binary.LittleEndian.Uint32(data[24:28]) || crc32.ChecksumIEEE(data[:28]) != binary.LittleEndian.Uint32(data[28:32]) {
		t.Fatal("frame checksums do not cover canonical bytes")
	}
	decoded, err := storage.DecodeValue(t.Context(), data[32:], 8192)
	if err != nil || decoded.Type != model.TypeComposite || len(decoded.ArrayVal) != 1 || decoded.ArrayVal[0].StrVal != row[0].StrVal {
		t.Fatalf("canonical payload = %v, %v", decoded, err)
	}
}

func TestReadersRejectInvalidOffsets(t *testing.T) {
	for _, spilled := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "file"}[spilled], func(t *testing.T) {
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
			limits := testLimits()
			if spilled {
				limits.MemoryBytes = 0
			}
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(8)}); err != nil {
				t.Fatal(err)
			}
			if err := s.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			r := openReader(t, s)
			for _, position := range []Position{1, 32, Position(s.Bytes()), Position(math.MaxUint64)} {
				if _, err := r.Read(t.Context(), position); !errors.Is(err, ErrPosition) {
					t.Fatalf("offset %d = %v", position, err)
				}
			}
			_, row, err := r.NextRow(t.Context())
			if err != nil || row[0].IntVal != 8 {
				t.Fatalf("random failures moved cursor: %v, %v", row, err)
			}
		})
	}
}

func TestReadersRejectCorruptOrTruncatedFiles(t *testing.T) {
	for _, damage := range []string{"header", "payload", "version", "columns", "length", "short-header", "short-payload", "typed-payload"} {
		t.Run(damage, func(t *testing.T) {
			fs := &faultFS{}
			q := testQuery(t, fs, runtime.QueryConfig{})
			limits := testLimits()
			limits.MemoryBytes = 0
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
			if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(8)}); err != nil {
				t.Fatal(err)
			}
			if err := s.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			r := openReader(t, s)
			data, err := os.ReadFile(fs.file.Name())
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "header":
				data[28] ^= 1
			case "payload":
				data[len(data)-1] ^= 1
			case "version":
				data[3] = '2'
			case "columns":
				binary.LittleEndian.PutUint32(data[4:8], 2)
			case "length":
				binary.LittleEndian.PutUint64(data[16:24], math.MaxUint64)
			case "short-header":
				data = data[:10]
			case "short-payload":
				data = data[:len(data)-1]
			case "typed-payload":
				// Composite envelope is 15 bytes. Replace int64 by timestamp,
				// preserving a valid canonical payload but violating the schema.
				data[32+15] = byte(model.TypeTimestamp)
				binary.LittleEndian.PutUint32(data[24:28], crc32.ChecksumIEEE(data[32:]))
			}
			if damage == "columns" || damage == "length" || damage == "typed-payload" {
				binary.LittleEndian.PutUint32(data[28:32], crc32.ChecksumIEEE(data[:28]))
			}
			if err := fs.file.File.Truncate(int64(len(data))); err != nil {
				t.Fatal(err)
			}
			if _, err := fs.file.File.WriteAt(data, 0); err != nil {
				t.Fatal(err)
			}
			_, _, err = r.NextRow(t.Context())
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s record = %v", damage, err)
			}
			if (damage == "short-header" || damage == "short-payload") && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("short I/O cause lost: %v", err)
			}
		})
	}
}

func TestMemoryPressureMigratesInsteadOfExceedingWorkBudget(t *testing.T) {
	limits := testLimits()
	work := int64(limits.MetadataBytes + limits.MaxRecordBytes + limits.DecodeBytes + codecScratchBytes + limits.MemoryBytes - 1)
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: work})
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(11)}); err != nil {
		t.Fatal(err)
	}
	if !s.Spilled() || q.Usage().Work.Peak > work || q.Usage().Spill.Live != 57 {
		t.Fatalf("pressure accounting = %+v", q.Usage())
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	_, row, err := r.NextRow(t.Context())
	if err != nil || row[0].IntVal != 11 {
		t.Fatalf("pressure row = %v, %v", row, err)
	}
}

func TestReaderDecodeAndMetadataCaps(t *testing.T) {
	t.Run("decoded-row", func(t *testing.T) {
		q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
		limits := testLimits()
		limits.DecodeBytes = uint64(unsafe.Sizeof(model.Value{})) - 1
		s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeString)}, limits)
		if _, err := s.AppendRow(t.Context(), []model.Value{model.StringVal("")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		r := openReader(t, s)
		if _, _, err := r.NextRow(t.Context()); !errors.Is(err, storage.ErrDecodeBudget) {
			t.Fatalf("decoded row cap = %v", err)
		}
	})
	t.Run("reader-metadata", func(t *testing.T) {
		q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
		limits := testLimits()
		limits.MetadataBytes = uint64(unsafe.Sizeof(Spool{})) + 256 + uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(model.Value{})) + readerMetadata() - 1
		s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
		if err := s.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		before := q.Usage().Work
		if _, err := s.Reader(t.Context()); !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("metadata cap = %v", err)
		}
		if after := q.Usage().Work; after.Live != before.Live || after.Reservations != before.Reservations {
			t.Fatalf("failed admission retained a claim: %+v -> %+v", before, after)
		}
	})
}

func TestInvalidSchemaDoesNotRetainClaims(t *testing.T) {
	q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
	array := testLayout(model.TypeArray)
	if _, err := New(t.Context(), q, []model.TypeLayout{array}, testLimits()); !errors.Is(err, model.ErrInvalidTypeLayout) {
		t.Fatalf("invalid layout = %v", err)
	}
	if q.Usage().Work.Live != 0 || q.Usage().Work.Reservations != 0 {
		t.Fatalf("constructor leaked: %+v", q.Usage())
	}
	array.Children = []model.TypeLayout{testLayout(model.TypeInt64)}
	s := newSpool(t, q, []model.TypeLayout{array}, testLimits())
	row := []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.StringVal("wrong")}}}
	if _, err := s.AppendRow(t.Context(), row); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("nested type mismatch = %v", err)
	}
	if s.Rows() != 0 || s.Bytes() != 0 {
		t.Fatal("invalid nested value admitted")
	}
}

func TestSpillAppendRollbackPreservesSealedPrefix(t *testing.T) {
	fs := &faultFS{}
	q := testQuery(t, fs, runtime.QueryConfig{})
	limits := testLimits()
	limits.MemoryBytes = 0
	s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeInt64)}, limits)
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(6)}); err != nil {
		t.Fatal(err)
	}
	fs.writeErr = syscall.ENOSPC
	if _, err := s.AppendRow(t.Context(), []model.Value{model.Int64Val(7)}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("append failure = %v", err)
	}
	if s.Bytes() != 57 || s.Rows() != 1 || q.Usage().Spill.Live != 57 {
		t.Fatalf("rollback extent = %d/%d; %+v", s.Bytes(), s.Rows(), q.Usage())
	}
	if err := s.Seal(t.Context()); err != nil {
		t.Fatal(err)
	}
	r := openReader(t, s)
	_, row, err := r.NextRow(t.Context())
	if err != nil || row[0].IntVal != 6 {
		t.Fatalf("sealed prefix = %v, %v", row, err)
	}
	if _, _, err := r.NextRow(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("failed record escaped prefix = %v", err)
	}
}
