package txn

import "testing"

func TestTableLockCompatibilityMatrix(t *testing.T) {
	all := lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
	matrix := [][]bool{
		{false, false, false, false, false, false, false, true},
		{false, false, false, false, false, false, true, true},
		{false, false, false, false, true, true, true, true},
		{false, false, false, true, true, true, true, true},
		{false, false, true, true, false, true, true, true},
		{false, false, true, true, true, true, true, true},
		{false, true, true, true, true, true, true, true},
		{true, true, true, true, true, true, true, true},
	}
	for held := AccessShareTableLock; held <= AccessExclusiveTableLock; held++ {
		for requested := AccessShareTableLock; requested <= AccessExclusiveTableLock; requested++ {
			got := lockRequestsConflict(all, tableLockMode(held), all, tableLockMode(requested))
			if got != matrix[held-1][requested-1] {
				t.Fatalf("held=%d requested=%d conflict=%v", held, requested, got)
			}
		}
	}
}
