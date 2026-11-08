package engine

import "testing"

func TestRangeSetBasic(t *testing.T) {
	var s rangeSet
	s.add(10, 20)
	s.add(30, 40)
	s.add(20, 30) // coalesce into [10, 40)
	if s.len() != 1 {
		t.Fatalf("expected 1 range, got %d", s.len())
	}
	lo, hi, ok := s.first()
	if !ok || lo != 10 || hi != 40 {
		t.Fatalf("expected [10, 40), got [%d, %d)", lo, hi)
	}

	// Split in middle: remove [20, 30) -> [10, 20), [30, 40)
	s.remove(20, 30)
	if s.len() != 2 {
		t.Fatalf("expected 2 ranges, got %d", s.len())
	}
	if s.r[0] != [2]uint64{10, 20} || s.r[1] != [2]uint64{30, 40} {
		t.Fatalf("unexpected ranges: %v", s.r)
	}

	// Remove prefix [5, 15) -> [15, 20), [30, 40)
	s.remove(5, 15)
	if s.r[0] != [2]uint64{15, 20} || s.r[1] != [2]uint64{30, 40} {
		t.Fatalf("unexpected ranges: %v", s.r)
	}

	// Remove spanning across first and into second [18, 35) -> [15, 18), [35, 40)
	s.remove(18, 35)
	if s.len() != 2 || s.r[0] != [2]uint64{15, 18} || s.r[1] != [2]uint64{35, 40} {
		t.Fatalf("unexpected ranges: %v", s.r)
	}

	// Remove entire set
	s.remove(0, 100)
	if !s.empty() {
		t.Fatalf("expected empty, got %v", s.r)
	}
}

func BenchmarkRangeSetStreaming(b *testing.B) {
	var s rangeSet
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate adding a chunk and removing segments (streaming hotpath)
		s.add(0, 65536)
		for off := uint64(0); off < 65536; off += 1200 {
			end := off + 1200
			if end > 65536 {
				end = 65536
			}
			s.remove(off, end)
		}
	}
}
