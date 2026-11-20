package txn_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func TestClosedFenceRejectsReplayAndChangedPublicationBarrier(t *testing.T) {
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), 42, 7, clock.Epoch{Assurance: [16]byte{1}, LeapHistory: [32]byte{2}, Config: [32]byte{3}})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.VerifyReadFence(t.Context(), fence); err != nil {
		t.Fatalf("persisted authority lost a barrier: %v", err)
	}
	other, err := txn.NewFenceAuthority([32]byte{2})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.VerifyReadFence(t.Context(), fence); !errors.Is(err, storage.ErrSnapshotNotClosed) {
		t.Fatalf("cross-database replay: %v", err)
	}
	timestamp, position, epoch := fence, fence, fence
	timestamp.ClosedTS++
	position.LogPosition++
	epoch.Epoch.Assurance[0]++
	for _, changed := range []storage.ReadFence{timestamp, position, epoch} {
		if err := authority.VerifyReadFence(t.Context(), changed); !errors.Is(err, storage.ErrSnapshotNotClosed) {
			t.Fatalf("changed publication barrier: %v", err)
		}
	}
}

func TestClosedFenceAuthenticatesEmptyDatabaseWithoutDummyCommit(t *testing.T) {
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), 42, 0, clock.Epoch{Assurance: [16]byte{1}})
	if err != nil {
		t.Fatalf("closed empty-database barrier: %v", err)
	}
	if err := authority.VerifyReadFence(t.Context(), fence); err != nil {
		t.Fatalf("authenticated empty-database barrier: %v", err)
	}
	if err := (storage.SnapshotSpec{Timestamp: 42, Fence: fence}).Validate(); err != nil {
		t.Fatalf("empty-database snapshot: %v", err)
	}
	fence.LogPosition = 1
	if err := authority.VerifyReadFence(t.Context(), fence); !errors.Is(err, storage.ErrSnapshotNotClosed) {
		t.Fatalf("forged first commit was accepted: %v", err)
	}
}
