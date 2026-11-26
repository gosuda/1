package neosepia

import (
	"errors"
	"os"
	"slices"
	"testing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestRecoveryReplaysCompleteDecisionsBeforeDiscardingPendingFiles(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	batch.TxID[0]++
	batch.CommitTS++
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity, err = batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeCommit(t.Context(), root, 2, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pendingCommitName(3), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	var positions []uint64
	recovered, err := recoverCommits(t.Context(), root, recoveredPosition{}, recoveryLimits{Batch: commitLimits(), MaxFiles: 10}, func(record *storedCommit) error { positions = append(positions, record.Result.LogPosition); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(positions, []uint64{1, 2}) || recovered.LogPosition != 2 || recovered.CommitTS != 24 {
		t.Fatal("recovery changed commit order or high watermarks")
	}
	if _, err := root.Stat(pendingCommitName(3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending record was not discarded: %v", err)
	}
}

func TestRecoveryCannotTreatCorruptPublishedRecordAsPending(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(pendingCommitName(2), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(commitName(1), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(file.Truncate(commitHeaderSize), file.Close()); err != nil {
		t.Fatal(err)
	}
	_, err = recoverCommits(t.Context(), root, recoveredPosition{}, recoveryLimits{Batch: commitLimits(), MaxFiles: 10}, func(*storedCommit) error { t.Fatal("replayed a corrupt decision"); return nil })
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("corrupt published decision: %v", err)
	}
	if _, err := root.Stat(pendingCommitName(2)); err != nil {
		t.Fatalf("cleanup ran before recovery validated: %v", err)
	}
}

func TestRecoveryRejectsMissingUncheckpointedDecision(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 2, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	_, err := recoverCommits(t.Context(), root, recoveredPosition{}, recoveryLimits{Batch: commitLimits(), MaxFiles: 10}, func(*storedCommit) error { t.Fatal("replayed past a gap"); return nil })
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("missing log position: %v", err)
	}
}
