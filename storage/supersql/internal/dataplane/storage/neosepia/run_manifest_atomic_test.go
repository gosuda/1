package neosepia

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func syntheticPreparedRuns(root *os.Root, position uint64, counts ...uint64) *preparedRuns {
	files := make([]preparedRun, len(counts))
	for i, rows := range counts {
		target := encTestTarget()
		target.ObjectID += model.ObjectID(i)
		files[i] = preparedRun{
			Name:     preparedRunName(position, i),
			Target:   target,
			Rows:     rows,
			Bytes:    uint64(1024 + i*64),
			Checksum: sha256.Sum256([]byte(preparedRunName(position, i))),
		}
	}
	return &preparedRuns{
		root:          root,
		files:         files,
		metadataBytes: 128,
	}
}

func syntheticRunDecision(position uint64, commitTS model.Timestamp, rows uint64) runDecision {
	return runDecision{
		Position:      position,
		TxID:          model.TxnID{byte(position), 0xAB, 0xCD},
		CommitTS:      commitTS,
		Identity:      sha256.Sum256([]byte(runManifestName(position))),
		MutationCount: rows,
	}
}

func assertFileDoesNotExist(t *testing.T, root *os.Root, name string) {
	t.Helper()
	if _, err := root.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to not exist, got err=%v", name, err)
	}
}

func readRootFile(t *testing.T, root *os.Root, name string) []byte {
	t.Helper()
	file, err := root.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", name, err)
		}
	}()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func writeRootFile(t *testing.T, root *os.Root, name string, content []byte) {
	t.Helper()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		t.Fatalf("write %s: %v", name, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func TestPersistManifestAtomicPublishesAndRemovesPending(t *testing.T) {
	limits := preparationLimits()

	t.Run("single", func(t *testing.T) {
		root := commitTestRoot(t)
		const pos uint64 = 7
		prepared := syntheticPreparedRuns(root, pos, 3, 5)
		initialMeta := prepared.metadataBytes
		decision := syntheticRunDecision(pos, 70, 8)

		if err := prepared.persistManifest(t.Context(), decision, limits.MetadataBytes); err != nil {
			t.Fatalf("persistManifest: %v", err)
		}
		if prepared.manifest != runManifestName(pos) {
			t.Fatalf("prepared.manifest = %q, want %q", prepared.manifest, runManifestName(pos))
		}
		if prepared.metadataBytes != initialMeta+25 {
			t.Fatalf("prepared.metadataBytes = %d, want %d", prepared.metadataBytes, initialMeta+25)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(pos))

		loaded, err := loadRunManifest(t.Context(), root, decision, limits)
		if err != nil {
			t.Fatalf("loadRunManifest: %v", err)
		}
		if len(loaded) != len(prepared.files) {
			t.Fatalf("loaded %d runs, want %d", len(loaded), len(prepared.files))
		}
		for i := range prepared.files {
			if loaded[i].Name != prepared.files[i].Name ||
				loaded[i].Target != prepared.files[i].Target ||
				loaded[i].Rows != prepared.files[i].Rows ||
				loaded[i].Bytes != prepared.files[i].Bytes ||
				loaded[i].Checksum != prepared.files[i].Checksum {
				t.Fatalf("run[%d] mismatch: got %+v, want %+v", i, loaded[i], prepared.files[i])
			}
		}
		span, err := inspectRunManifest(t.Context(), root, pos)
		if err != nil {
			t.Fatalf("inspectRunManifest: %v", err)
		}
		if span != (runManifestSpan{firstPosition: pos, lastPosition: pos, coalesced: false}) {
			t.Fatalf("unexpected span: %+v", span)
		}
	})

	t.Run("coalesced", func(t *testing.T) {
		root := commitTestRoot(t)
		decisions := []runDecision{
			syntheticRunDecision(10, 100, 4),
			syntheticRunDecision(11, 110, 6),
			syntheticRunDecision(12, 120, 5),
		}
		coalesced, err := makeCoalescedDecision(decisions)
		if err != nil {
			t.Fatalf("makeCoalescedDecision: %v", err)
		}
		prepared := syntheticPreparedRuns(root, coalesced.LastPosition, 7, 8)
		initialMeta := prepared.metadataBytes

		if err := prepared.persistCoalescedManifest(t.Context(), coalesced, limits.MetadataBytes); err != nil {
			t.Fatalf("persistCoalescedManifest: %v", err)
		}
		if prepared.manifest != runManifestName(coalesced.LastPosition) {
			t.Fatalf("prepared.manifest = %q, want %q", prepared.manifest, runManifestName(coalesced.LastPosition))
		}
		if prepared.metadataBytes != initialMeta+25 {
			t.Fatalf("prepared.metadataBytes = %d, want %d", prepared.metadataBytes, initialMeta+25)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(coalesced.LastPosition))

		loaded, err := loadCoalescedRunManifest(t.Context(), root, coalesced, limits)
		if err != nil {
			t.Fatalf("loadCoalescedRunManifest: %v", err)
		}
		if len(loaded) != len(prepared.files) {
			t.Fatalf("loaded %d runs, want %d", len(loaded), len(prepared.files))
		}
		for i := range prepared.files {
			if loaded[i].Name != prepared.files[i].Name ||
				loaded[i].Target != prepared.files[i].Target ||
				loaded[i].Rows != prepared.files[i].Rows ||
				loaded[i].Bytes != prepared.files[i].Bytes ||
				loaded[i].Checksum != prepared.files[i].Checksum {
				t.Fatalf("run[%d] mismatch: got %+v, want %+v", i, loaded[i], prepared.files[i])
			}
		}
		span, err := inspectRunManifest(t.Context(), root, coalesced.LastPosition)
		if err != nil {
			t.Fatalf("inspectRunManifest: %v", err)
		}
		if span != (runManifestSpan{firstPosition: 10, lastPosition: 12, coalesced: true}) {
			t.Fatalf("unexpected span: %+v", span)
		}
	})
}

func TestPersistManifestDoesNotOverwriteExisting(t *testing.T) {
	limits := preparationLimits()
	sentinel := []byte("existing-manifest-sentinel-do-not-overwrite")

	t.Run("single", func(t *testing.T) {
		root := commitTestRoot(t)
		const pos uint64 = 5
		writeRootFile(t, root, runManifestName(pos), sentinel)

		prepared := syntheticPreparedRuns(root, pos, 4)
		initialMeta := prepared.metadataBytes
		decision := syntheticRunDecision(pos, 50, 4)

		err := prepared.persistManifest(t.Context(), decision, limits.MetadataBytes)
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("persistManifest over existing: got err=%v, want os.ErrExist", err)
		}
		if prepared.manifest != "" {
			t.Fatalf("prepared.manifest mutated on failed link: %q", prepared.manifest)
		}
		if prepared.metadataBytes != initialMeta {
			t.Fatalf("prepared.metadataBytes mutated on failed link: got %d, want %d", prepared.metadataBytes, initialMeta)
		}
		if got := readRootFile(t, root, runManifestName(pos)); !bytes.Equal(got, sentinel) {
			t.Fatalf("existing manifest was modified: got %q, want %q", got, sentinel)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(pos))
	})

	t.Run("coalesced", func(t *testing.T) {
		root := commitTestRoot(t)
		decisions := []runDecision{
			syntheticRunDecision(8, 80, 3),
			syntheticRunDecision(9, 90, 5),
		}
		coalesced, err := makeCoalescedDecision(decisions)
		if err != nil {
			t.Fatalf("makeCoalescedDecision: %v", err)
		}
		writeRootFile(t, root, runManifestName(coalesced.LastPosition), sentinel)

		prepared := syntheticPreparedRuns(root, coalesced.LastPosition, 8)
		initialMeta := prepared.metadataBytes

		err = prepared.persistCoalescedManifest(t.Context(), coalesced, limits.MetadataBytes)
		if !errors.Is(err, os.ErrExist) {
			t.Fatalf("persistCoalescedManifest over existing: got err=%v, want os.ErrExist", err)
		}
		if prepared.manifest != "" {
			t.Fatalf("prepared.manifest mutated on failed link: %q", prepared.manifest)
		}
		if prepared.metadataBytes != initialMeta {
			t.Fatalf("prepared.metadataBytes mutated on failed link: got %d, want %d", prepared.metadataBytes, initialMeta)
		}
		if got := readRootFile(t, root, runManifestName(coalesced.LastPosition)); !bytes.Equal(got, sentinel) {
			t.Fatalf("existing manifest was modified: got %q, want %q", got, sentinel)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(coalesced.LastPosition))
	})
}

func TestPersistManifestRecoversFromStalePendingFile(t *testing.T) {
	limits := preparationLimits()
	stale := []byte("truncated-crash-leftover-pending-manifest")

	t.Run("single", func(t *testing.T) {
		root := commitTestRoot(t)
		const pos uint64 = 15
		writeRootFile(t, root, runManifestPendingName(pos), stale)

		prepared := syntheticPreparedRuns(root, pos, 6)
		decision := syntheticRunDecision(pos, 150, 6)

		if err := prepared.persistManifest(t.Context(), decision, limits.MetadataBytes); err != nil {
			t.Fatalf("persistManifest with stale pending file: %v", err)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(pos))

		loaded, err := loadRunManifest(t.Context(), root, decision, limits)
		if err != nil {
			t.Fatalf("loadRunManifest: %v", err)
		}
		if len(loaded) != 1 || loaded[0].Rows != 6 {
			t.Fatalf("unexpected loaded runs: %+v", loaded)
		}
	})

	t.Run("coalesced", func(t *testing.T) {
		root := commitTestRoot(t)
		decisions := []runDecision{
			syntheticRunDecision(20, 200, 3),
			syntheticRunDecision(21, 210, 4),
		}
		coalesced, err := makeCoalescedDecision(decisions)
		if err != nil {
			t.Fatalf("makeCoalescedDecision: %v", err)
		}
		writeRootFile(t, root, runManifestPendingName(coalesced.LastPosition), stale)

		prepared := syntheticPreparedRuns(root, coalesced.LastPosition, 7)

		if err := prepared.persistCoalescedManifest(t.Context(), coalesced, limits.MetadataBytes); err != nil {
			t.Fatalf("persistCoalescedManifest with stale pending file: %v", err)
		}
		assertFileDoesNotExist(t, root, runManifestPendingName(coalesced.LastPosition))

		loaded, err := loadCoalescedRunManifest(t.Context(), root, coalesced, limits)
		if err != nil {
			t.Fatalf("loadCoalescedRunManifest: %v", err)
		}
		if len(loaded) != 1 || loaded[0].Rows != 7 {
			t.Fatalf("unexpected loaded runs: %+v", loaded)
		}
	})
}

func TestRecoveryIgnoresAndCleansUpPendingRunManifest(t *testing.T) {
	// Verify pending run manifest filename classification directly.
	validPending := runManifestPendingName(42)
	if !isPendingRunManifestName(validPending) || !obsoleteRunArtifactName(validPending) {
		t.Fatalf("expected %q to be classified as pending obsolete run artifact", validPending)
	}
	for _, invalid := range []string{
		runManifestName(42),
		".0000000000000000004.runs.pending",   // 19 digits
		".000000000000000000042.runs.pending", // 21 digits
		"000000000000000000042.runs.pending",  // missing leading dot
		".0000000000000000004a.runs.pending",  // non-decimal
		".00000000000000000042.pending",       // wrong suffix
		baseManifestPending,
	} {
		if isPendingRunManifestName(invalid) {
			t.Fatalf("unexpectedly matched %q as pending run manifest", invalid)
		}
	}

	fixture := openBackendFixture(t)
	batch := fixture.batch(t, 1, 10, 111, 222)
	result, err := fixture.backend.ApplyMutations(t.Context(), batch)
	if err != nil {
		t.Fatalf("ApplyMutations: %v", err)
	}
	if result.LogPosition != 1 {
		t.Fatalf("LogPosition = %d, want 1", result.LogPosition)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate crashed manifest writes left behind in runs/ for both a committed
	// position and an uncommitted position, with corrupt/truncated contents.
	runsDir := filepath.Join(fixture.config.Directory, "runs")
	pendingCommitted := filepath.Join(runsDir, runManifestPendingName(1))
	pendingUncommitted := filepath.Join(runsDir, runManifestPendingName(2))
	if err := os.WriteFile(pendingCommitted, []byte("corrupt-partial-manifest"), 0600); err != nil {
		t.Fatalf("write pendingCommitted: %v", err)
	}
	if err := os.WriteFile(pendingUncommitted, []byte("uncommitted-crash-leftover"), 0600); err != nil {
		t.Fatalf("write pendingUncommitted: %v", err)
	}

	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatalf("Open with pending run manifests: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = reopened

	for _, path := range []string{pendingCommitted, pendingUncommitted} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected recovery to clean up %s, got err=%v", path, err)
		}
	}

	reader := fixture.snapshot(t, 10)
	if got := backendBalance(t, reader, 0); got != 111 {
		t.Fatalf("object 0 = %d, want 111", got)
	}
	if got := backendBalance(t, reader, 1); got != 222 {
		t.Fatalf("object 1 = %d, want 222", got)
	}
}
