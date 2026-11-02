package neosepia

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func commitFixture(t *testing.T) (storage.MutationBatch, [32]byte) {
	t.Helper()
	b := storage.MutationBatch{TxID: model.TxnID{7}, CommitTS: 23, EncodingVersion: storage.MutationEncodingVersion}
	for i := range 2 {
		target := storage.ObjectRef{ObjectID: model.ObjectID(i + 1), Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		b.Layouts = append(b.Layouts, storage.ObjectLayout{Target: target, Columns: []storage.ColumnLayout{{ID: 70000, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
		m := storage.NewUpsertMutation(target, []byte("key"), []model.ColumnID{70000}, []model.Value{model.Int64Val(int64(i + 10))})
		m.Ordinal = uint64(i + 1)
		b.Mutations = append(b.Mutations, m)
	}
	var err error
	b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := b.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return b, identity
}

func commitLimits() storage.BatchDecodeLimits {
	return storage.BatchDecodeLimits{MaxBytes: 1 << 20, MaxAllocation: 1 << 20, MaxMutationBytes: 1 << 16}
}

func commitTestRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	return root
}

func checkStoredCommit(t *testing.T, root *os.Root, identity [32]byte) {
	t.Helper()
	record, err := openCommit(t.Context(), root, 1, commitLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := record.Close(); err != nil {
			t.Error(err)
		}
	})
	if record.Identity != identity || record.Result.LogPosition != 1 || record.Result.CommitTS != 23 {
		t.Fatal("recovered another commit decision")
	}
	cursor, err := record.Batch.OpenMutations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := range 2 {
		m, err := cursor.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if m.Target.ObjectID != model.ObjectID(i+1) || m.Values[0].IntVal != int64(i+10) {
			t.Fatal("cross-object transaction was not recovered atomically")
		}
	}
	if _, err := cursor.Next(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra mutation: %v", err)
	}
}

func TestCommitPublicationBoundaryDeterminesResolution(t *testing.T) {
	failure := errors.New("injected publication failure")
	for _, test := range []struct {
		name      string
		point     commitPoint
		published bool
	}{{"before publication", commitFileSynced, false}, {"after publication", commitPublished, true}, {"after directory sync", commitDirectorySynced, true}} {
		t.Run(test.name, func(t *testing.T) {
			root := commitTestRoot(t)
			batch, identity := commitFixture(t)
			_, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, func(point commitPoint) error {
				if point == test.point {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) {
				t.Fatalf("fault lost: %v", err)
			}
			if errors.Is(err, storage.ErrCommitIndeterminate) != test.published {
				t.Fatalf("incorrect publication certainty: %v", err)
			}
			if test.published {
				checkStoredCommit(t, root, identity)
				return
			}
			if _, err := openCommit(t.Context(), root, 1, commitLimits()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unpublished record recovered: %v", err)
			}
		})
	}
}

func TestCommitCancellationAfterPublicationRemainsCommitted(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := writeCommit(ctx, root, 1, batch, identity, 1<<20, func(point commitPoint) error {
		if point == commitPublished {
			cancel()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AppliedCount != 2 {
		t.Fatal("cancelled a published cross-object decision")
	}
	checkStoredCommit(t, root, identity)
}

func TestCommitBudgetFailureCannotPublishOrPoisonRetry(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1, nil); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("byte limit: %v", err)
	}
	if _, err := openCommit(t.Context(), root, 1, commitLimits()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial payload published: %v", err)
	}
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	checkStoredCommit(t, root, identity)
}

func TestPublishedCommitCannotBeOverwritten(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	batch.TxID[0]++
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	other, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeCommit(t.Context(), root, 1, batch, other, 1<<20, nil); err == nil {
		t.Fatal("overwrote a committed log position")
	}
	checkStoredCommit(t, root, identity)
}

func TestCommitPayloadCorruptionDoesNotBecomeRollback(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(commitName(1), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var value [1]byte
	_, readErr := file.ReadAt(value[:], commitHeaderSize+20)
	value[0] ^= 1
	_, writeErr := file.WriteAt(value[:], commitHeaderSize+20)
	if err := errors.Join(readErr, writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := openCommit(t.Context(), root, 1, commitLimits()); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("corrupted committed data: %v", err)
	}
}

func TestCommitSurvivesProcessExitAfterPublication(t *testing.T) {
	directory := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommitCrashHelper$")
	command.Env = append(os.Environ(), "NEOSEPIA_COMMIT_CHILD="+directory)
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("child did not reach publication: %v", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	_, identity := commitFixture(t)
	checkStoredCommit(t, root, identity)
}

func TestCommitCrashHelper(t *testing.T) {
	directory := os.Getenv("NEOSEPIA_COMMIT_CHILD")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	batch, identity := commitFixture(t)
	_, err = writeCommit(t.Context(), root, 1, batch, identity, 1<<20, func(point commitPoint) error {
		if point == commitPublished {
			os.Exit(23)
		}
		return nil
	})
	t.Fatalf("publication hook did not exit: %v", err)
}

func TestCommitRejectsCompleteBodyWithoutCommitMarker(t *testing.T) {
	root := commitTestRoot(t)
	batch, identity := commitFixture(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(commitName(1), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := errors.Join(file.Truncate(info.Size()-commitHeaderSize), file.Close()); err != nil {
		t.Fatal(err)
	}
	record, err := openCommit(t.Context(), root, 1, commitLimits())
	if record != nil {
		if closeErr := record.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("complete row frames became a commit without a marker: %v", err)
	}
}

func TestCommitRejectsMarkerFromAnotherTransaction(t *testing.T) {
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
	other, err := root.ReadFile(commitName(2))
	if err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(commitName(1), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	_, writeErr := file.WriteAt(other[len(other)-commitHeaderSize:], info.Size()-commitHeaderSize)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	record, err := openCommit(t.Context(), root, 1, commitLimits())
	if record != nil {
		if closeErr := record.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("foreign commit marker was accepted: %v", err)
	}
}
