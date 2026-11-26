package neosepia

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestNativeV2ReopensAndUpgradesThroughLocalizedCompaction(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{fixture.batch(t, 1, 10, 100, 100), fixture.batch(t, 2, 20, 90, 110)} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil { t.Fatal(err) }
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{}); err != nil { t.Fatal(err) }
	generation := fixture.backend.state.BaseGeneration
	base, err := loadRunBase(t.Context(), fixture.backend.directory.runs, generation, fixture.backend.limits.MetadataBytes, fixture.backend.limits.IdentityBytes, fixture.backend.limits.MaxFiles)
	if err != nil { t.Fatal(err) }
	if err := fixture.backend.Close(); err != nil { t.Fatal(err) }
	for i := range base.Files {
		path := filepath.Join(fixture.config.Directory, "runs", base.Files[i].Name)
		data, err := os.ReadFile(path)
		if err != nil { t.Fatal(err) }
		previous := nativeV2SST(t, data)
		if err := os.WriteFile(path, previous, 0600); err != nil { t.Fatal(err) }
		base.Files[i].Bytes, base.Files[i].Checksum = uint64(len(previous)), sha256.Sum256(previous)
	}
	limits := DefaultLimits()
	encoded, err := encodeRunBase(t.Context(), base, limits.MetadataBytes, limits.IdentityBytes, limits.MaxFiles)
	if err != nil { t.Fatal(err) }
	previousBase := append([]byte(nil), encoded[:baseManifestHeader]...)
	for i := range base.Files {
		offset := baseManifestHeader+i*baseManifestEntryBytes
		previousBase = append(previousBase, encoded[offset:offset+128]...)
	}
	previousBase = append(previousBase, encoded[baseManifestHeader+len(base.Files)*baseManifestEntryBytes:len(encoded)-sha256.Size]...)
	copy(previousBase[:8], "NSBASE02")
	clear(previousBase[144:baseManifestHeader])
	binary.LittleEndian.PutUint64(previousBase[8:16], uint64(len(previousBase)+sha256.Size))
	digest := sha256.Sum256(previousBase)
	previousBase = append(previousBase, digest[:]...)
	if err := os.WriteFile(filepath.Join(fixture.config.Directory, "runs", baseManifestName(generation)), previousBase, 0600); err != nil { t.Fatal(err) }
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil { t.Fatal(err) }
	fixture.backend = reopened
	defer func() { if err := reopened.Close(); err != nil { t.Error(err) } }()
	for _, timestamp := range []model.Timestamp{10, 20} {
		reader := fixture.snapshot(t, timestamp)
		want := int64(100)
		if timestamp == 20 { want = 90 }
		if got := backendBalance(t, reader, 0); got != want { t.Fatalf("v2 snapshot %d: got %d, want %d", timestamp, got, want) }
		if err := reader.Close(); err != nil { t.Fatal(err) }
	}
	task, err := reopened.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: encTestTarget()})
	if err != nil { t.Fatal(err) }
	if err := task.Wait(t.Context()); err != nil { t.Fatal(err) }
	reader := fixture.snapshot(t, 10)
	if got := backendBalance(t, reader, 0); got != 100 { t.Fatalf("localized v2 upgrade lost history: %d", got) }
}

func nativeV2SST(t *testing.T, data []byte) []byte {
	t.Helper()
	if string(data[:8]) != sstMagic { t.Fatal("fixture is not the current SST format") }
	indexOffset := int(binary.LittleEndian.Uint64(data[40:48]))
	count := int(binary.LittleEndian.Uint32(data[36:40]))
	index := data[indexOffset:]
	out := append([]byte(nil), data[:64]...)
	out = append(out, data[sstHeaderSize:indexOffset]...)
	previousOffset := len(out)
	out = append(out, index[:sstIndexHeaderSize]...)
	var encoded [sstEntrySize]byte
	for i := range count {
		entry := decodeSSTEntry(index[sstIndexHeaderSize+i*sstEntrySize:sstIndexHeaderSize+(i+1)*sstEntrySize])
		entry.offset -= sstHeaderSize-64
		encodeSSTEntry(encoded[:], entry)
		out = append(out, encoded[:56]...)
	}
	out = append(out, index[sstIndexHeaderSize+count*sstEntrySize:len(index)-sstIndexHashSize]...)
	digest := sha256.Sum256(out[previousOffset:])
	out = append(out, digest[:]...)
	copy(out[:8], "NSSST002")
	clear(out[60:64])
	binary.LittleEndian.PutUint64(out[40:48], uint64(previousOffset))
	binary.LittleEndian.PutUint64(out[48:56], uint64(len(out)-previousOffset))
	binary.LittleEndian.PutUint32(out[56:60], paxChecksum(out[:64]))
	return out
}
