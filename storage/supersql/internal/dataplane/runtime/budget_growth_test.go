package runtime

import (
	"errors"
	"math"
	"testing"
)

func TestReservationGrowthUsesSharedHeadroomWithoutLosingClaim(t *testing.T) {
	root, err := NewBudget(100, 8)
	if err != nil {
		t.Fatal(err)
	}
	first, second := root.child(80, 4), root.child(80, 4)
	claim, err := first.TryReserve(20)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Release()
	blocker, err := second.TryReserve(60)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	growth, err := claim.GrowUpTo(math.MaxInt64)
	if err != nil || growth != 20 || claim.Bytes() != 40 {
		t.Fatalf("shared growth=%d bytes=%d error=%v", growth, claim.Bytes(), err)
	}
	if _, err := second.TryReserve(1); !errors.Is(err, ErrBudget) {
		t.Fatalf("growth bypassed shared cap: %v", err)
	}
	growth, err = claim.GrowUpTo(1)
	if err != nil || growth != 0 || claim.Bytes() != 40 {
		t.Fatalf("full growth=%d bytes=%d error=%v", growth, claim.Bytes(), err)
	}
	blocker.Release()
	growth, err = claim.GrowUpTo(100)
	if err != nil || growth != 40 || claim.Bytes() != 80 {
		t.Fatalf("child growth=%d bytes=%d error=%v", growth, claim.Bytes(), err)
	}
	if _, err := claim.GrowUpTo(-1); !errors.Is(err, ErrInvalidConfig) || claim.Bytes() != 80 {
		t.Fatalf("negative growth changed claim: %v", err)
	}
	claim.Release()
	if _, err := claim.GrowUpTo(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("released claim grew: %v", err)
	}
	if usage := root.Usage(); usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("growth leaked ownership: %+v", usage)
	}
}
