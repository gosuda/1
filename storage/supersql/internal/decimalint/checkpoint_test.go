package decimalint_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/decimalint"
)

func TestCheckpointRetainsSelectedResultsAfterEarlierValuesClose(t *testing.T) {
	workspace, err := decimalint.NewWorkspace(t.Context(), nil, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	first, err := workspace.FromInt64(t.Context(), 12)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := workspace.Checkpoint()
	discarded, err := workspace.FromInt64(t.Context(), 99)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := workspace.FromInt64(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := discarded.Close(); err != nil {
		t.Fatal(err)
	}
	if err := workspace.ReleaseAfter(checkpoint, retained); err != nil {
		t.Fatal(err)
	}
	later := workspace.Checkpoint()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	transient, err := workspace.FromInt64(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspace.ReleaseAfter(later); err != nil {
		t.Fatal(err)
	}
	if value, ok := retained.Int64(); !ok || value != 100 {
		t.Fatalf("retained result = %d %v", value, ok)
	}
	if _, err := workspace.Clone(t.Context(), transient); !errors.Is(err, decimalint.ErrClosed) {
		t.Fatalf("released temporary accepted: %v", err)
	}
}

func TestStreamedDigitsPreserveGroupsAndRejectInvalidDigit(t *testing.T) {
	workspace, err := decimalint.NewWorkspace(t.Context(), nil, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	input := []uint8{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1}
	index := 0
	value, err := workspace.ParseDigits(t.Context(), len(input), true, func() (uint8, error) { digit := input[index]; index++; return digit, nil })
	if err != nil {
		t.Fatal(err)
	}
	text, err := value.AppendDecimal(t.Context(), make([]byte, 0, 32))
	if err != nil || string(text) != "-12345678901" {
		t.Fatalf("streamed value = %q, %v", text, err)
	}
	before := workspace.UsedBytes()
	_, err = workspace.ParseBaseDigits(t.Context(), 1, 2, false, func() (uint8, error) { return 2, nil })
	if !errors.Is(err, decimalint.ErrInvalid) || workspace.UsedBytes() != before {
		t.Fatalf("invalid streamed digit retained memory: %v", err)
	}
}
