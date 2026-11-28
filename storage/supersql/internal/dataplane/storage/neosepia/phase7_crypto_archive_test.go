package neosepia_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/maintenance"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

const (
	testNSEMagic        = "NSE1"
	testNSEHeaderV2Bit  = uint32(1 << 31)
	testNSEFinalBit     = uint32(1 << 31)
	testNSEHeaderSizeV2 = 4 + 4 + 32
	testNSEChunkSize    = 64 << 10
	testNSETagSize      = 16
	testNSEDEKInfo      = "supersql-nse2-dek-v1"
)

type phase7Fixture struct {
	dir       string
	scale     clock.ScaleDescriptor
	encoder   storage.KeyEncoder
	authority *txn.FenceAuthority
	epoch     clock.Epoch
	layouts   []storage.ObjectLayout
	intType   model.Type
	strType   model.Type
}

func newPhase7Fixture(t *testing.T) phase7Fixture {
	t.Helper()
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(history),
		CanonicalLeapHistory: history,
	}
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := txn.NewFenceAuthority([32]byte{29})
	if err != nil {
		t.Fatal(err)
	}
	intType := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	strType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}
	layouts := []storage.ObjectLayout{{
		Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Columns: []storage.ColumnLayout{
			{ID: 1, Type: intType},
			{ID: 2, Type: intType},
			{ID: 3, Type: strType},
		},
	}}
	return phase7Fixture{
		dir:       filepath.Join(t.TempDir(), "db"),
		scale:     scale,
		encoder:   encoder,
		authority: authority,
		epoch:     clock.Epoch{Assurance: [16]byte{5}, Config: [32]byte{6}, LeapHistory: scale.LeapHistoryID},
		layouts:   layouts,
		intType:   intType,
		strType:   strType,
	}
}

func (f phase7Fixture) encodeKey(t *testing.T, id int64) []byte {
	t.Helper()
	k, err := f.encoder.EncodePrefix(t.Context(), []storage.KeyColumn{{ColumnID: 1, Type: f.intType}}, []model.Value{model.Int64Val(id)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

type phase7Row struct {
	id  int64
	val int64
	txt string
}

func (f phase7Fixture) makeBatch(t *testing.T, txByte byte, commitTS model.Timestamp, rows []phase7Row) storage.MutationBatch {
	t.Helper()
	batch := storage.MutationBatch{
		TxID:            model.TxnID{txByte},
		CommitTS:        commitTS,
		Epoch:           f.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         f.layouts,
	}
	for i, r := range rows {
		batch.Mutations = append(batch.Mutations, storage.Mutation{
			Op:        storage.MutationUpsert,
			Target:    f.layouts[0].Target,
			Key:       f.encodeKey(t, r.id),
			Ordinal:   uint64(i + 1),
			ColumnIDs: []model.ColumnID{1, 2, 3},
			Values:    []model.Value{model.Int64Val(r.id), model.Int64Val(r.val), model.StringVal(r.txt)},
		})
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func (f phase7Fixture) readRow(t *testing.T, db *neosepia.Backend, readTS model.Timestamp, minPos uint64, id int64) (int64, string, bool) {
	t.Helper()
	fence, err := f.authority.Issue(t.Context(), readTS, minPos, f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := db.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: readTS, Fence: fence})
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()
	row, err := snap.Get(t.Context(), storage.ReadRequest{
		Target:    f.layouts[0].Target,
		Key:       f.encodeKey(t, id),
		ColumnIDs: []model.ColumnID{2, 3},
		Layout:    f.layouts[0].Columns,
	})
	if errors.Is(err, storage.ErrRowNotFound) {
		return 0, "", false
	}
	if err != nil {
		t.Fatalf("Get(%d): %v", id, err)
	}
	return row.Values[0].IntVal, row.Values[1].StrVal, true
}

func phase7RandomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatal(err)
	}
	return key
}

// encryptLegacyNSE1V1 produces a legacy NSE1 v1 stream (static master key +
// 12-byte random baseNonce XORed with chunkIndex, 17-byte v1 AAD) so tests can
// verify backwards-compatible decryption in both neosepia and maintenance.
func encryptLegacyNSE1V1(t *testing.T, masterKey, plain []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	var baseNonce [12]byte
	if _, err := io.ReadFull(rand.Reader, baseNonce[:]); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var hdr [20]byte
	copy(hdr[0:4], testNSEMagic)
	binary.BigEndian.PutUint32(hdr[4:8], testNSEChunkSize)
	copy(hdr[8:20], baseNonce[:])
	out.Write(hdr[:])

	var chunkIdx uint64
	rem := plain
	for {
		take := min(len(rem), testNSEChunkSize)
		chunk := rem[:take]
		rem = rem[take:]
		isFinal := len(rem) == 0

		flags := uint32(len(chunk))
		if isFinal {
			flags |= testNSEFinalBit
		}
		var ch [4]byte
		binary.BigEndian.PutUint32(ch[:], flags)
		out.Write(ch[:])

		nonce := baseNonce
		var idxBytes [8]byte
		binary.BigEndian.PutUint64(idxBytes[:], chunkIdx)
		for i := range 8 {
			nonce[4+i] ^= idxBytes[i]
		}
		var aad [17]byte
		copy(aad[0:4], testNSEMagic)
		binary.BigEndian.PutUint64(aad[4:12], chunkIdx)
		binary.BigEndian.PutUint32(aad[12:16], uint32(len(chunk)))
		if isFinal {
			aad[16] = 1
		}
		ct := aead.Seal(nil, nonce[:], chunk, aad[:])
		out.Write(ct)

		chunkIdx++
		if isFinal {
			break
		}
	}
	return out.Bytes()
}

// independentVerifyNSE1V2 independently parses and decrypts an NSE1 v2 stream
// using stdlib crypto/hkdf + deterministic counter nonces + full-header AAD,
// returning the 32-byte salt, derived 32-byte DEK, chunk count, and plaintext.
func independentVerifyNSE1V2(t *testing.T, masterKey, raw []byte) (salt [32]byte, dek []byte, chunks int, plain []byte) {
	t.Helper()
	if len(raw) < testNSEHeaderSizeV2+4+testNSETagSize {
		t.Fatalf("v2 ciphertext too short: %d bytes", len(raw))
	}
	if string(raw[0:4]) != testNSEMagic {
		t.Fatalf("magic = %q, want %q", raw[0:4], testNSEMagic)
	}
	hdrFlags := binary.BigEndian.Uint32(raw[4:8])
	if hdrFlags&testNSEHeaderV2Bit == 0 {
		t.Fatalf("v2 bit not set in header flags: 0x%08x", hdrFlags)
	}
	chunkSize := int(hdrFlags &^ testNSEHeaderV2Bit)
	if chunkSize != testNSEChunkSize {
		t.Fatalf("chunkSize = %d, want %d", chunkSize, testNSEChunkSize)
	}
	copy(salt[:], raw[8:testNSEHeaderSizeV2])
	var err error
	dek, err = hkdf.Key(sha256.New, masterKey, salt[:], testNSEDEKInfo, 32)
	if err != nil {
		t.Fatalf("hkdf.Key: %v", err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}

	var out bytes.Buffer
	pos := testNSEHeaderSizeV2
	var chunkIdx uint64
	for {
		if pos+4 > len(raw) {
			t.Fatalf("truncated chunk header at chunk %d", chunkIdx)
		}
		flags := binary.BigEndian.Uint32(raw[pos : pos+4])
		pos += 4
		isFinal := flags&testNSEFinalBit != 0
		plainLen := int(flags &^ testNSEFinalBit)
		ctLen := plainLen + testNSETagSize
		if pos+ctLen > len(raw) {
			t.Fatalf("truncated chunk body at chunk %d", chunkIdx)
		}
		var nonce [12]byte
		binary.BigEndian.PutUint64(nonce[4:12], chunkIdx)

		var aad [testNSEHeaderSizeV2 + 13]byte
		copy(aad[0:testNSEHeaderSizeV2], raw[0:testNSEHeaderSizeV2])
		binary.BigEndian.PutUint64(aad[testNSEHeaderSizeV2:testNSEHeaderSizeV2+8], chunkIdx)
		binary.BigEndian.PutUint32(aad[testNSEHeaderSizeV2+8:testNSEHeaderSizeV2+12], uint32(plainLen))
		if isFinal {
			aad[testNSEHeaderSizeV2+12] = 1
		}
		pt, err := aead.Open(nil, nonce[:], raw[pos:pos+ctLen], aad[:])
		if err != nil {
			t.Fatalf("independent v2 decrypt chunk %d failed: %v", chunkIdx, err)
		}
		out.Write(pt)
		pos += ctLen
		chunkIdx++
		if isFinal {
			break
		}
	}
	if pos != len(raw) {
		t.Fatalf("trailing bytes after final chunk: %d", len(raw)-pos)
	}
	return salt, dek, int(chunkIdx), out.Bytes()
}

func writeTestEncryptionMeta(t *testing.T, dir string, key []byte) {
	t.Helper()
	fp, err := neosepia.KeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	var payload [4 + 4 + 32 + 32]byte
	copy(payload[0:4], testNSEMagic)
	binary.BigEndian.PutUint32(payload[4:8], 1)
	copy(payload[8:40], fp[:])
	sum := sha256.Sum256(payload[:40])
	copy(payload[40:72], sum[:])
	if err := os.WriteFile(filepath.Join(dir, "encryption.meta"), payload[:], 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPhase7CryptoHKDFPerFileDEKAndLegacyV1Compat(t *testing.T) {
	f := newPhase7Fixture(t)
	key := phase7RandomKey(t)

	cfg := neosepia.Config{
		Directory:     f.dir,
		Scale:         f.scale,
		KeyEncoder:    f.encoder,
		FenceVerifier: f.authority,
		EncryptionKey: key,
	}
	db, err := neosepia.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Open encrypted: %v", err)
	}
	largeA := strings.Repeat("HKDF-PER-FILE-DEK-PAYLOAD-A-", 6000) // ~168 KiB (>2 chunks)
	largeB := strings.Repeat("HKDF-PER-FILE-DEK-PAYLOAD-B-", 6000)
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []phase7Row{
		{id: 1, val: 11, txt: largeA},
	})); err != nil {
		t.Fatalf("ApplyMutations 1: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 2, 200, []phase7Row{
		{id: 2, val: 22, txt: largeB},
	})); err != nil {
		t.Fatalf("ApplyMutations 2: %v", err)
	}
	// Trigger compaction/flush to create encrypted SST and base manifest while
	// retiring older WALs, then write two more WALs (including a multi-chunk WAL).
	if err := db.Collect(t.Context(), storage.GCRequest{Boundary: 150}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	largeC := strings.Repeat("HKDF-PER-FILE-DEK-WAL-C-", 7000) // ~168 KiB (>2 chunks)
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 3, 300, []phase7Row{
		{id: 3, val: 33, txt: largeC},
	})); err != nil {
		t.Fatalf("ApplyMutations 3: %v", err)
	}
	if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 4, 400, []phase7Row{
		{id: 4, val: 44, txt: "post-compact-wal-row"},
	})); err != nil {
		t.Fatalf("ApplyMutations 4: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify WAL files and SST files on disk use v2 HKDF per-file DEK with
	// distinct salts, distinct DEKs, and deterministic chunk counter nonces.
	walEntries, err := os.ReadDir(filepath.Join(f.dir, "commits"))
	if err != nil {
		t.Fatal(err)
	}
	seenSalts := make(map[[32]byte]string)
	seenDEKs := make(map[[32]byte]string)
	var sampleWAL []byte
	walCount := 0
	for _, e := range walEntries {
		if !strings.HasSuffix(e.Name(), ".wal") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, "commits", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		salt, dek, chunks, _ := independentVerifyNSE1V2(t, key, raw)
		var dekArr [32]byte
		copy(dekArr[:], dek)
		if prev, dup := seenSalts[salt]; dup {
			t.Fatalf("duplicate salt between %s and %s", prev, e.Name())
		}
		if prev, dup := seenDEKs[dekArr]; dup {
			t.Fatalf("duplicate DEK between %s and %s", prev, e.Name())
		}
		seenSalts[salt] = e.Name()
		seenDEKs[dekArr] = e.Name()
		if chunks >= 3 && sampleWAL == nil {
			sampleWAL = raw
		}
		walCount++
	}
	if walCount < 2 || sampleWAL == nil {
		t.Fatalf("expected >=2 WAL files including multi-chunk WAL; got count=%d sample=%v", walCount, sampleWAL != nil)
	}

	// Verify SST files and base-*.runs manifests also use v2 HKDF per-file DEKs with distinct salts.
	runEntries, err := os.ReadDir(filepath.Join(f.dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	sstCount := 0
	for _, e := range runEntries {
		name := e.Name()
		isBase := strings.HasPrefix(name, "base-") && strings.HasSuffix(name, ".runs")
		if !strings.HasSuffix(name, ".sst") && !isBase {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(f.dir, "runs", name))
		if err != nil {
			t.Fatal(err)
		}
		salt, dek, _, _ := independentVerifyNSE1V2(t, key, raw)
		var dekArr [32]byte
		copy(dekArr[:], dek)
		if prev, dup := seenSalts[salt]; dup {
			t.Fatalf("duplicate salt between %s and %s", prev, name)
		}
		if prev, dup := seenDEKs[dekArr]; dup {
			t.Fatalf("duplicate DEK between %s and %s", prev, name)
		}
		seenSalts[salt] = name
		seenDEKs[dekArr] = name
		if strings.HasSuffix(name, ".sst") {
			sstCount++
		}
	}
	if sstCount == 0 {
		t.Fatal("expected at least one encrypted SST run on disk")
	}

	// Tamper checks on multi-chunk v2 WAL: salt, header flags, chunk body, trailing byte.
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "salt byte flipped", mutate: func(b []byte) []byte { c := bytes.Clone(b); c[12] ^= 0x80; return c }},
		{name: "header flags flipped", mutate: func(b []byte) []byte { c := bytes.Clone(b); c[7] ^= 0x01; return c }},
		{name: "chunk 0 ciphertext flipped", mutate: func(b []byte) []byte { c := bytes.Clone(b); c[testNSEHeaderSizeV2+10] ^= 0x01; return c }},
		{name: "final chunk tag flipped", mutate: func(b []byte) []byte { c := bytes.Clone(b); c[len(c)-1] ^= 0x01; return c }},
		{name: "truncated", mutate: func(b []byte) []byte { return bytes.Clone(b[:len(b)-1]) }},
		{name: "trailing byte", mutate: func(b []byte) []byte { return append(bytes.Clone(b), 0x42) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := neosepia.InspectCommitHeader(t.Context(), tc.mutate(sampleWAL), key); err == nil {
				t.Fatalf("tampered WAL (%s) unexpectedly accepted", tc.name)
			}
		})
	}

	// Reopen and verify all rows decrypt cleanly from both SST and WAL.
	reopened, err := neosepia.Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("reopen encrypted: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if v, s, ok := f.readRow(t, reopened, 400, 4, 1); !ok || v != 11 || s != largeA {
		t.Fatalf("row 1 mismatch: ok=%v v=%d len=%d", ok, v, len(s))
	}
	if v, s, ok := f.readRow(t, reopened, 400, 4, 2); !ok || v != 22 || s != largeB {
		t.Fatalf("row 2 mismatch: ok=%v v=%d len=%d", ok, v, len(s))
	}
	if v, s, ok := f.readRow(t, reopened, 400, 4, 3); !ok || v != 33 || s != largeC {
		t.Fatalf("row 3 mismatch: ok=%v v=%d len=%d", ok, v, len(s))
	}
	if v, s, ok := f.readRow(t, reopened, 400, 4, 4); !ok || v != 44 || s != "post-compact-wal-row" {
		t.Fatalf("row 4 mismatch: ok=%v v=%d s=%q", ok, v, s)
	}

	// Legacy NSE1 v1 backwards compatibility: take a plaintext DB with both SST
	// and WAL files, wrap them in legacy NSE1 v1 framing, write encryption.meta,
	// and verify neosepia.InspectCommitHeader and neosepia.Open read everything!
	plainFix := newPhase7Fixture(t)
	plainDB, err := neosepia.Open(t.Context(), neosepia.Config{
		Directory:     plainFix.dir,
		Scale:         plainFix.scale,
		KeyEncoder:    plainFix.encoder,
		FenceVerifier: plainFix.authority,
	})
	if err != nil {
		t.Fatalf("Open plaintext: %v", err)
	}
	legacyText := strings.Repeat("LEGACY-V1-MULTI-CHUNK-DATA-", 5000) // ~135 KiB
	if _, err := plainDB.ApplyMutations(t.Context(), plainFix.makeBatch(t, 1, 100, []phase7Row{
		{id: 10, val: 101, txt: legacyText},
	})); err != nil {
		t.Fatal(err)
	}
	if err := plainDB.Collect(t.Context(), storage.GCRequest{Boundary: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := plainDB.ApplyMutations(t.Context(), plainFix.makeBatch(t, 2, 200, []phase7Row{
		{id: 20, val: 202, txt: "legacy-v1-wal-row"},
	})); err != nil {
		t.Fatal(err)
	}
	if err := plainDB.Close(); err != nil {
		t.Fatal(err)
	}

	// Convert the compacted base SST, base-*.runs manifest, and commit 2 WAL in
	// plainFix.dir to legacy NSE1 v1 framing. Update base-*.runs file descriptors
	// to match the v1-encrypted SST size/SHA-256, and remove commit 2's optional
	// pre-materialized delta run so commit 2 replays directly from its legacy v1 WAL.
	legacyKey := phase7RandomKey(t)
	writeTestEncryptionMeta(t, plainFix.dir, legacyKey)

	runsDir := filepath.Join(plainFix.dir, "runs")
	runFiles, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatal(err)
	}
	type fileMeta struct {
		size uint64
		sum  [32]byte
	}
	v1SSTMeta := make(map[string]fileMeta)
	for _, e := range runFiles {
		name := e.Name()
		full := filepath.Join(runsDir, name)
		switch {
		case strings.HasPrefix(name, "00000000000000000002"):
			if err := os.Remove(full); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(name, ".sst"):
			rawPlain, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			v1Bytes := encryptLegacyNSE1V1(t, legacyKey, rawPlain)
			if err := os.WriteFile(full, v1Bytes, 0600); err != nil {
				t.Fatal(err)
			}
			v1SSTMeta[name] = fileMeta{size: uint64(len(v1Bytes)), sum: sha256.Sum256(v1Bytes)}
		}
	}
	if len(v1SSTMeta) == 0 {
		t.Fatal("expected compacted SST file for legacy v1 test")
	}
	for _, e := range runFiles {
		name := e.Name()
		if !strings.HasPrefix(name, "base-") || !strings.HasSuffix(name, ".runs") {
			continue
		}
		full := filepath.Join(runsDir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		// Patch each SST file descriptor's (Bytes, Checksum) in the NSBASE04 manifest
		// (header=176, entry=144, Bytes at [88:96], Checksum at [96:128]) and update
		// the trailing SHA-256 before wrapping in legacy NSE1 v1.
		const (
			baseHdrBytes   = 176
			baseEntryBytes = 144
		)
		fileCount := int(binary.LittleEndian.Uint64(data[48:56]))
		if fileCount == 0 {
			t.Fatalf("base manifest %s has fileCount == 0", name)
		}
		for i := range fileCount {
			entry := data[baseHdrBytes+i*baseEntryBytes : baseHdrBytes+(i+1)*baseEntryBytes]
			nameEnd := bytes.IndexByte(entry[:48], 0)
			if nameEnd < 0 {
				nameEnd = 48
			}
			sstName := string(entry[:nameEnd])
			meta, ok := v1SSTMeta[sstName]
			if !ok {
				t.Fatalf("missing v1 SST metadata for %s", sstName)
			}
			binary.LittleEndian.PutUint64(entry[88:96], meta.size)
			copy(entry[96:128], meta.sum[:])
		}
		end := len(data) - sha256.Size
		newSum := sha256.Sum256(data[:end])
		copy(data[end:], newSum[:])
		v1Base := encryptLegacyNSE1V1(t, legacyKey, data)
		if err := os.WriteFile(full, v1Base, 0600); err != nil {
			t.Fatal(err)
		}
	}

	commitsDir := filepath.Join(plainFix.dir, "commits")
	commitFiles, err := os.ReadDir(commitsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range commitFiles {
		name := e.Name()
		if !strings.HasSuffix(name, ".wal") {
			continue
		}
		full := filepath.Join(commitsDir, name)
		rawPlain, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		v1Bytes := encryptLegacyNSE1V1(t, legacyKey, rawPlain)
		if err := os.WriteFile(full, v1Bytes, 0600); err != nil {
			t.Fatal(err)
		}
		pos, ts, err := neosepia.InspectCommitHeader(t.Context(), v1Bytes, legacyKey)
		if err != nil {
			t.Fatalf("InspectCommitHeader legacy v1 %s: %v", name, err)
		}
		if pos != 2 || ts != 200 {
			t.Fatalf("InspectCommitHeader legacy v1 = (%d, %d), want (2, 200)", pos, ts)
		}
	}

	legacyOpened, err := neosepia.Open(t.Context(), neosepia.Config{
		Directory:     plainFix.dir,
		Scale:         plainFix.scale,
		KeyEncoder:    plainFix.encoder,
		FenceVerifier: plainFix.authority,
		EncryptionKey: legacyKey,
	})
	if err != nil {
		t.Fatalf("Open legacy NSE1 v1 database: %v", err)
	}
	defer func() { _ = legacyOpened.Close() }()
	if v, s, ok := plainFix.readRow(t, legacyOpened, 200, 2, 10); !ok || v != 101 || s != legacyText {
		t.Fatalf("legacy v1 SST row mismatch: ok=%v v=%d len=%d", ok, v, len(s))
	}
	if v, s, ok := plainFix.readRow(t, legacyOpened, 200, 2, 20); !ok || v != 202 || s != "legacy-v1-wal-row" {
		t.Fatalf("legacy v1 WAL row mismatch: ok=%v v=%d s=%q", ok, v, s)
	}
}

func TestPhase7CryptoMaintenanceHKDFAndLegacyV1Compat(t *testing.T) {
	key := phase7RandomKey(t)
	wrongKey := phase7RandomKey(t)

	sizes := []int{0, 1, 17, 65535, 65536, 65537, 131072, 200000}
	for _, sz := range sizes {
		plain := make([]byte, sz)
		if _, err := io.ReadFull(rand.Reader, plain); err != nil {
			t.Fatal(err)
		}
		ct1, err := maintenance.EncryptBytes(key, plain)
		if err != nil {
			t.Fatalf("EncryptBytes(%d) #1: %v", sz, err)
		}
		ct2, err := maintenance.EncryptBytes(key, plain)
		if err != nil {
			t.Fatalf("EncryptBytes(%d) #2: %v", sz, err)
		}

		// Independent v2 verification proves HKDF-SHA256 DEK + deterministic counter nonce + v2 AAD.
		salt1, dek1, _, pt1 := independentVerifyNSE1V2(t, key, ct1)
		salt2, dek2, _, pt2 := independentVerifyNSE1V2(t, key, ct2)
		if salt1 == salt2 {
			t.Fatalf("size %d: identical salts across two encryptions", sz)
		}
		if bytes.Equal(dek1, dek2) {
			t.Fatalf("size %d: identical derived DEKs across two encryptions", sz)
		}
		if !bytes.Equal(pt1, plain) || !bytes.Equal(pt2, plain) {
			t.Fatalf("size %d: independent decrypt mismatch", sz)
		}

		// Round-trip through maintenance.DecryptBytes and DecryptStream.
		got, err := maintenance.DecryptBytes(key, ct1)
		if err != nil {
			t.Fatalf("DecryptBytes(%d): %v", sz, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("DecryptBytes(%d) mismatch", sz)
		}
		if _, err := maintenance.DecryptBytes(wrongKey, ct1); err == nil {
			t.Fatalf("size %d: wrong key accepted", sz)
		}

		// Tampering with salt byte must fail authentication.
		tamperedSalt := bytes.Clone(ct1)
		tamperedSalt[10] ^= 0x01
		if _, err := maintenance.DecryptBytes(key, tamperedSalt); err == nil {
			t.Fatalf("size %d: tampered salt accepted", sz)
		}

		// Legacy NSE1 v1 backwards compatibility in maintenance.DecryptBytes / DecryptStream.
		legacyV1 := encryptLegacyNSE1V1(t, key, plain)
		gotLegacy, err := maintenance.DecryptBytes(key, legacyV1)
		if err != nil {
			t.Fatalf("DecryptBytes legacy v1 (%d): %v", sz, err)
		}
		if !bytes.Equal(gotLegacy, plain) {
			t.Fatalf("DecryptBytes legacy v1 (%d) mismatch", sz)
		}
		var streamOut bytes.Buffer
		gotLen, gotSHA, err := maintenance.DecryptStream(t.Context(), &streamOut, bytes.NewReader(legacyV1), key)
		if err != nil {
			t.Fatalf("DecryptStream legacy v1 (%d): %v", sz, err)
		}
		wantSHA := sha256.Sum256(plain)
		if gotLen != uint64(sz) || gotSHA != wantSHA || !bytes.Equal(streamOut.Bytes(), plain) {
			t.Fatalf("DecryptStream legacy v1 (%d) mismatch", sz)
		}
	}
}

func TestPhase7CryptoStreamingCrossEncryptedWALArchive(t *testing.T) {
	t.Run("unencrypted DB to encrypted archive", func(t *testing.T) {
		f := newPhase7Fixture(t)
		archiveDir := filepath.Join(t.TempDir(), "archive-enc")
		archiveKey := phase7RandomKey(t)

		db, err := neosepia.Open(t.Context(), neosepia.Config{
			Directory:     f.dir,
			Scale:         f.scale,
			KeyEncoder:    f.encoder,
			FenceVerifier: f.authority,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()

		payload := strings.Repeat("STREAMING-UNENC-TO-ENC-WAL-", 7000) // ~189 KiB (3 chunks)
		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []phase7Row{
			{id: 1, val: 1, txt: payload},
		})); err != nil {
			t.Fatal(err)
		}
		if err := db.EnableWALArchive(t.Context(), archiveDir, archiveKey); err != nil {
			t.Fatalf("EnableWALArchive: %v", err)
		}
		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 2, 200, []phase7Row{
			{id: 2, val: 2, txt: payload},
		})); err != nil {
			t.Fatal(err)
		}

		var prevSalt [32]byte
		for pos := uint64(1); pos <= 2; pos++ {
			name := fmt.Sprintf("%020d.wal", pos)
			livePlain, err := os.ReadFile(filepath.Join(f.dir, "commits", name))
			if err != nil {
				t.Fatal(err)
			}
			archivedRaw, err := os.ReadFile(filepath.Join(archiveDir, name))
			if err != nil {
				t.Fatal(err)
			}
			salt, _, chunks, _ := independentVerifyNSE1V2(t, archiveKey, archivedRaw)
			if chunks < 3 {
				t.Fatalf("pos %d chunks = %d, want >= 3", pos, chunks)
			}
			if pos == 2 && salt == prevSalt {
				t.Fatal("archived WALs reused salt across segments")
			}
			prevSalt = salt

			// Cross-package decryption via maintenance.DecryptBytes must match live plaintext.
			decrypted, err := maintenance.DecryptBytes(archiveKey, archivedRaw)
			if err != nil {
				t.Fatalf("maintenance.DecryptBytes pos %d: %v", pos, err)
			}
			if !bytes.Equal(decrypted, livePlain) {
				t.Fatalf("archived WAL pos %d decrypted bytes != live WAL", pos)
			}
			gotPos, gotTS, err := neosepia.InspectCommitHeader(t.Context(), archivedRaw, archiveKey)
			if err != nil || gotPos != pos || gotTS != model.Timestamp(pos*100) {
				t.Fatalf("InspectCommitHeader pos %d = (%d, %d, %v)", pos, gotPos, gotTS, err)
			}
		}
	})

	t.Run("encrypted DB to different-key encrypted archive", func(t *testing.T) {
		f := newPhase7Fixture(t)
		archiveDir := filepath.Join(t.TempDir(), "archive-reenc")
		dbKey := phase7RandomKey(t)
		archiveKey := phase7RandomKey(t)

		db, err := neosepia.Open(t.Context(), neosepia.Config{
			Directory:     f.dir,
			Scale:         f.scale,
			KeyEncoder:    f.encoder,
			FenceVerifier: f.authority,
			EncryptionKey: dbKey,
			WALArchiveDir: archiveDir,
			WALArchiveKey: archiveKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()

		payload := strings.Repeat("STREAMING-REENCRYPT-KEYA-TO-KEYB-", 6000) // ~198 KiB (>3 chunks)
		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []phase7Row{
			{id: 1, val: 42, txt: payload},
		})); err != nil {
			t.Fatal(err)
		}

		name := fmt.Sprintf("%020d.wal", 1)
		liveRaw, err := os.ReadFile(filepath.Join(f.dir, "commits", name))
		if err != nil {
			t.Fatal(err)
		}
		archivedRaw, err := os.ReadFile(filepath.Join(archiveDir, name))
		if err != nil {
			t.Fatal(err)
		}

		liveSalt, _, _, livePlain := independentVerifyNSE1V2(t, dbKey, liveRaw)
		archSalt, _, chunks, archPlain := independentVerifyNSE1V2(t, archiveKey, archivedRaw)
		if chunks < 3 {
			t.Fatalf("re-encrypted archive chunks = %d, want >= 3", chunks)
		}
		if liveSalt == archSalt {
			t.Fatal("re-encrypted archive reused live WAL salt")
		}
		if !bytes.Equal(livePlain, archPlain) {
			t.Fatal("re-encrypted archive plaintext != live WAL plaintext")
		}
		if _, err := maintenance.DecryptBytes(dbKey, archivedRaw); err == nil {
			t.Fatal("archived WAL unexpectedly decrypted with dbKey")
		}
		if _, err := maintenance.DecryptBytes(archiveKey, liveRaw); err == nil {
			t.Fatal("live WAL unexpectedly decrypted with archiveKey")
		}
	})

	t.Run("oversized WAL segment rejected during cross-encrypted archiving", func(t *testing.T) {
		for _, encryptedSource := range []bool{false, true} {
			name := "plaintext-source"
			if encryptedSource {
				name = "encrypted-source"
			}
			t.Run(name, func(t *testing.T) {
				dbKey := phase7RandomKey(t)
				archiveKey := phase7RandomKey(t)
				var srcKey []byte
				if encryptedSource {
					srcKey = dbKey
				}

				// Create an oversized valid WAL using a DB with default large limits.
				bigFix := newPhase7Fixture(t)
				bigDB, err := neosepia.Open(t.Context(), neosepia.Config{
					Directory:     bigFix.dir,
					Scale:         bigFix.scale,
					KeyEncoder:    bigFix.encoder,
					FenceVerifier: bigFix.authority,
					EncryptionKey: srcKey,
				})
				if err != nil {
					t.Fatal(err)
				}
				bigPayload := strings.Repeat("OVERSIZED-WAL-SEGMENT-", 2000) // ~44 KiB
				if _, err := bigDB.ApplyMutations(t.Context(), bigFix.makeBatch(t, 1, 100, []phase7Row{
					{id: 1, val: 1, txt: bigPayload},
				})); err != nil {
					t.Fatal(err)
				}
				if err := bigDB.Close(); err != nil {
					t.Fatal(err)
				}
				oversizedWAL, err := os.ReadFile(filepath.Join(bigFix.dir, "commits", fmt.Sprintf("%020d.wal", 1)))
				if err != nil {
					t.Fatal(err)
				}

				// Open target DB with tight WALBytes limit (8 KiB) and commit a small batch.
				smallFix := newPhase7Fixture(t)
				tightLimits := neosepia.DefaultLimits()
				tightLimits.WALBytes = 8 << 10
				tightLimits.WALAllocation = 8 << 10
				tightLimits.MutationBytes = 8 << 10
				smallDB, err := neosepia.Open(t.Context(), neosepia.Config{
					Directory:     smallFix.dir,
					Scale:         smallFix.scale,
					KeyEncoder:    smallFix.encoder,
					FenceVerifier: smallFix.authority,
					EncryptionKey: srcKey,
					Limits:        tightLimits,
				})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = smallDB.Close() }()
				if _, err := smallDB.ApplyMutations(t.Context(), smallFix.makeBatch(t, 1, 100, []phase7Row{
					{id: 1, val: 1, txt: "small"},
				})); err != nil {
					t.Fatal(err)
				}

				// Swap in the oversized WAL segment before enabling cross-encrypted archive.
				walPath := filepath.Join(smallFix.dir, "commits", fmt.Sprintf("%020d.wal", 1))
				if err := os.WriteFile(walPath, oversizedWAL, 0600); err != nil {
					t.Fatal(err)
				}

				archiveDir := filepath.Join(t.TempDir(), "archive-reject")
				err = smallDB.EnableWALArchive(t.Context(), archiveDir, archiveKey)
				if !errors.Is(err, storage.ErrDecodeBudget) {
					t.Fatalf("EnableWALArchive oversized WAL error = %v, want ErrDecodeBudget", err)
				}
				if _, statErr := os.Stat(filepath.Join(archiveDir, fmt.Sprintf("%020d.wal", 1))); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("oversized WAL was published to archive: statErr=%v", statErr)
				}
			})
		}
	})
}

func TestPhase7CryptoEnableWALArchivePreservesHook(t *testing.T) {
	t.Run("preserves hook when updating archive dir and key", func(t *testing.T) {
		f := newPhase7Fixture(t)
		dir1 := filepath.Join(t.TempDir(), "archive-1")
		dir2 := filepath.Join(t.TempDir(), "archive-2")
		archiveKey := phase7RandomKey(t)

		var mu sync.Mutex
		var entries []neosepia.WALArchiveEntry
		db, err := neosepia.Open(t.Context(), neosepia.Config{
			Directory:     f.dir,
			Scale:         f.scale,
			KeyEncoder:    f.encoder,
			FenceVerifier: f.authority,
			WALArchiveDir: dir1,
			WALArchiveHook: func(_ context.Context, e neosepia.WALArchiveEntry) error {
				mu.Lock()
				entries = append(entries, e)
				mu.Unlock()
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()

		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []phase7Row{{id: 1, val: 10, txt: "one"}})); err != nil {
			t.Fatal(err)
		}

		// Update archive destination and enable encryption; existing hook must survive!
		if err := db.EnableWALArchive(t.Context(), dir2, archiveKey); err != nil {
			t.Fatalf("EnableWALArchive: %v", err)
		}

		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 2, 200, []phase7Row{{id: 2, val: 20, txt: "two"}})); err != nil {
			t.Fatal(err)
		}

		mu.Lock()
		got := append([]neosepia.WALArchiveEntry(nil), entries...)
		mu.Unlock()

		// Expected calls: pos 1 in dir1 (plaintext), pos 1 backfilled to dir2 (encrypted), pos 2 in dir2 (encrypted).
		var sawPos2InDir2 bool
		for _, e := range got {
			if e.LogPosition == 2 && filepath.Dir(e.ArchivedPath) == dir2 && e.Encrypted {
				sawPos2InDir2 = true
			}
		}
		if !sawPos2InDir2 {
			t.Fatalf("existing WALArchiveHook was not preserved after EnableWALArchive; entries=%+v", got)
		}
	})

	t.Run("preserves hook configured without initial archive dir", func(t *testing.T) {
		f := newPhase7Fixture(t)
		dir := filepath.Join(t.TempDir(), "archive-new")
		archiveKey := phase7RandomKey(t)

		var mu sync.Mutex
		var entries []neosepia.WALArchiveEntry
		db, err := neosepia.Open(t.Context(), neosepia.Config{
			Directory:     f.dir,
			Scale:         f.scale,
			KeyEncoder:    f.encoder,
			FenceVerifier: f.authority,
			WALArchiveHook: func(_ context.Context, e neosepia.WALArchiveEntry) error {
				mu.Lock()
				entries = append(entries, e)
				mu.Unlock()
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()

		if err := db.EnableWALArchive(t.Context(), dir, archiveKey); err != nil {
			t.Fatalf("EnableWALArchive: %v", err)
		}
		if _, err := db.ApplyMutations(t.Context(), f.makeBatch(t, 1, 100, []phase7Row{{id: 1, val: 10, txt: "hook-only"}})); err != nil {
			t.Fatal(err)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(entries) != 1 || entries[0].LogPosition != 1 || filepath.Dir(entries[0].ArchivedPath) != dir || !entries[0].Encrypted {
			t.Fatalf("hook entries after EnableWALArchive = %+v", entries)
		}
	})
}
