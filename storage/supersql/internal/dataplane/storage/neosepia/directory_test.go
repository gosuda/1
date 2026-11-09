package neosepia

import (
	"errors"
	"testing"
)

func TestDirectoryRejectsSecondWriterUntilOwnerCloses(t *testing.T) {
	path := t.TempDir()
	first, err := openDatabaseDirectory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	})
	second, err := openDatabaseDirectory(t.Context(), path)
	if second != nil {
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, ErrDatabaseLocked) {
		t.Fatalf("concurrent writer: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDatabaseDirectory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
}
