package neosepia

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestPublishedDecisionRejectsCorruptTailWithoutExposingOtherObjects(t *testing.T) {
	batch := runPreparationFixture(t)
	root := commitTestRoot(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareMutationRuns(t.Context(), root, 1, batch, identity, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	files := prepared.detach()
	decision := runDecision{Position: 1, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}
	set, err := newRunSet(runSetLimits{MaxBytes: 1 << 20, MaxFiles: 16, MaxPins: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	last, err := root.OpenFile(files[len(files)-1].Name, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := last.Close(); err != nil {
			t.Error(err)
		}
	})
	position := int64(sstHeaderSize + paxHeaderSize + 1)
	var original [1]byte
	if _, err := last.ReadAt(original[:], position); err != nil {
		t.Fatal(err)
	}
	corrupted := [1]byte{original[0] ^ 1}
	if _, err := last.WriteAt(corrupted[:], position); err != nil {
		t.Fatal(err)
	}
	if _, err := set.admitDecision(t.Context(), root, decision, files, runLimits(), 1<<20); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("corrupt final file publication: %v", err)
	}
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, layout := range batch.Layouts {
		if got := runPinTargetValues(t, pin, layout.Target); len(got) != 0 {
			t.Fatalf("object %d exposed uncommitted prefix %v", layout.Target.ObjectID, got)
		}
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := last.WriteAt(original[:], position); err != nil {
		t.Fatal(err)
	}
	admission, err := set.admitDecision(t.Context(), root, decision, files, runLimits(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := current.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := runPinTargetValues(t, current, batch.Layouts[0].Target); !slices.Equal(got, []int64{30}) {
		t.Fatalf("base object: got %v, want [30]", got)
	}
	if got := runPinTargetValues(t, current, batch.Layouts[1].Target); !slices.Equal(got, []int64{300, 200}) {
		t.Fatalf("related object: got %v, want [300 200]", got)
	}
}
