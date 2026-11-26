package neosepia

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func databaseStateFixture() databaseState {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	return databaseState{Generation: 1, Closed: storage.ReadFence{ID: [16]byte{1}, ClosedTS: 10, Epoch: clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}}, Scales: []scaleVersion{{Scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}}}}
}

func TestDatabaseStatePublicationRequiresResolutionAfterRename(t *testing.T) {
	for _, test := range []struct {
		name    string
		point   statePoint
		unknown bool
		closed  model.Timestamp
	}{
		{name: "before_rename", point: stateAfterFileSync, closed: 10},
		{name: "after_rename", point: stateAfterRename, unknown: true, closed: 20},
		{name: "after_directory_sync", point: stateAfterDirectorySync, unknown: true, closed: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, err := os.OpenRoot(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			old := databaseStateFixture()
			if _, err := writeDatabaseState(t.Context(), root, old, 4096, nil); err != nil {
				t.Fatal(err)
			}
			next := old
			next.Generation++
			next.Closed.ClosedTS = 20
			next.Closed.ID[0] = 2
			injected := errors.New("metadata I/O failure")
			unknown, err := writeDatabaseState(t.Context(), root, next, 4096, func(point statePoint) error {
				if point == test.point {
					return injected
				}
				return nil
			})
			if !errors.Is(err, injected) || unknown != test.unknown {
				t.Fatalf("publication outcome: unknown=%v error=%v", unknown, err)
			}
			if err := syncCommitDirectory(root); err != nil {
				t.Fatal(err)
			}
			recovered, err := readDatabaseState(t.Context(), root, 4096)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Closed.ClosedTS != test.closed {
				t.Fatalf("reconciled closed timestamp: got %d, want %d", recovered.Closed.ClosedTS, test.closed)
			}
		})
	}
}

func TestDatabaseStateRejectsCorruptedClosedTimestamp(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := writeDatabaseState(t.Context(), root, databaseStateFixture(), 4096, nil); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile(databaseStateName, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{99}, 32); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readDatabaseState(t.Context(), root, 4096); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("corrupt closed timestamp: %v", err)
	}
}
