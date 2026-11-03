package neosepia

import (
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestActiveRangesMatchOverlappingVersionOracle(t *testing.T) {
	ranges := newActiveRanges(64 << 10)
	defer ranges.Close()
	type interval struct {
		end       uint64
		timestamp model.Timestamp
		ordinal   uint64
	}
	var history []interval
	random := rand.New(rand.NewPCG(3, 17))
	var key [8]byte
	for step := uint64(1); step <= 2000; step++ {
		binary.BigEndian.PutUint64(key[:], step)
		ranges.advance(key[:])
		for i := uint64(0); i < 4; i++ {
			entry := interval{end: step + 1 + random.Uint64N(50), timestamp: model.Timestamp(1 + random.Uint64N(30)), ordinal: step*4 + i}
			binary.BigEndian.PutUint64(key[:], entry.end)
			if err := ranges.add(key[:], entry.timestamp, entry.ordinal); err != nil {
				t.Fatal(err)
			}
			history = append(history, entry)
		}
		timestamp, ordinal := model.Timestamp(1+random.Uint64N(30)), random.Uint64N(8192)
		expected := false
		for _, entry := range history {
			if entry.end <= step {
				continue
			}
			if entry.timestamp > timestamp || (entry.timestamp == timestamp && entry.ordinal >= ordinal) {
				expected = true
				break
			}
		}
		if actual := ranges.suppresses(timestamp, ordinal); actual != expected {
			t.Fatalf("position %d: suppression %v, want %v", step, actual, expected)
		}
	}
	binary.BigEndian.PutUint64(key[:], 2051)
	ranges.advance(key[:])
	if ranges.suppresses(1, 1) {
		t.Fatal("expired tombstone still suppresses a row")
	}
}

func TestActiveRangesDoNotRetainScanHistory(t *testing.T) {
	ranges := newActiveRanges(512)
	defer ranges.Close()
	var key [8]byte
	for step := uint64(1); step <= 10000; step++ {
		binary.BigEndian.PutUint64(key[:], step)
		if err := ranges.add(key[:], 10, 1); err != nil {
			t.Fatalf("bounded scan failed at %d: %v", step, err)
		}
		if !ranges.suppresses(10, 1) || ranges.suppresses(10, 2) {
			t.Fatal("intra-transaction range ordering changed")
		}
		ranges.advance(key[:])
		if ranges.suppresses(10, 1) {
			t.Fatal("exclusive range end remained visible")
		}
	}
}

func TestActiveRangeBudgetFailurePreservesOlderBarrier(t *testing.T) {
	ranges := newActiveRanges(512)
	defer ranges.Close()
	if err := ranges.add([]byte("z"), 10, 1); err != nil {
		t.Fatal(err)
	}
	if err := ranges.add(make([]byte, 512), 20, 1); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("unbounded range accepted: %v", err)
	}
	ranges.advance([]byte("m"))
	if !ranges.suppresses(10, 1) || ranges.suppresses(11, 1) {
		t.Fatal("failed range changed the published barrier")
	}
}
