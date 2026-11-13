package engine

// rangeSet is a sorted, non-adjacent interval set over uint64 offsets.
// It is the shared structure for stream-byte tracking and retransmission
// bookkeeping. Ranges are [lo, hi) half-open.
type rangeSet struct {
	r [][2]uint64 // sorted, disjoint, non-touching
}

func (s *rangeSet) empty() bool { return len(s.r) == 0 }
func (s *rangeSet) len() int    { return len(s.r) }
func (s *rangeSet) total() uint64 {
	var n uint64
	for _, r := range s.r {
		n += r[1] - r[0]
	}
	return n
}

// first returns the lowest range.
func (s *rangeSet) first() (lo, hi uint64, ok bool) {
	if len(s.r) == 0 {
		return 0, 0, false
	}
	return s.r[0][0], s.r[0][1], true
}

// contains reports whether off is inside some range.
func (s *rangeSet) contains(off uint64) bool {
	i := s.search(off)
	return i < len(s.r) && s.r[i][0] <= off
}

// search returns the index of the range that could contain off (first range
// with hi > off), or len(r).
func (s *rangeSet) search(off uint64) int {
	lo, hi := 0, len(s.r)
	for lo < hi {
		mid := (lo + hi) / 2
		if s.r[mid][1] > off {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// add merges [lo,hi) into the set, coalescing overlaps and touches.
func (s *rangeSet) add(lo, hi uint64) {
	if lo >= hi {
		return
	}
	// Find insertion window of ranges overlapping or touching [lo,hi].
	i := s.search(lo)
	// Include a preceding range that touches lo.
	if i > 0 && s.r[i-1][1] == lo {
		i--
	}
	j := i
	for j < len(s.r) && s.r[j][0] <= hi {
		j++
	}
	// Merge [lo,hi) with r[i:j].
	nlo, nhi := lo, hi
	if i < j {
		if s.r[i][0] < nlo {
			nlo = s.r[i][0]
		}
		if s.r[j-1][1] > nhi {
			nhi = s.r[j-1][1]
		}
	}
	// Replace s.r[i:j] with [nlo,nhi).
	if i == j {
		if i == len(s.r) {
			s.r = append(s.r, [2]uint64{nlo, nhi})
		} else {
			s.r = append(s.r, [2]uint64{})
			copy(s.r[i+1:], s.r[i:])
			s.r[i] = [2]uint64{nlo, nhi}
		}
	} else {
		s.r[i] = [2]uint64{nlo, nhi}
		if j > i+1 {
			copy(s.r[i+1:], s.r[j:])
			s.r = s.r[:len(s.r)-(j-i-1)]
		}
	}
}

// remove subtracts [lo,hi) from the set.
func (s *rangeSet) remove(lo, hi uint64) {
	if lo >= hi || len(s.r) == 0 {
		return
	}
	i := s.search(lo)
	j := i
	var mid [2][2]uint64
	var midLen int
	for j < len(s.r) && s.r[j][0] < hi {
		a, b := s.r[j][0], s.r[j][1]
		if a < lo {
			mid[midLen] = [2]uint64{a, lo}
			midLen++
		}
		if b > hi {
			mid[midLen] = [2]uint64{hi, b}
			midLen++
		}
		j++
	}
	if j == i {
		return
	}
	delta := midLen - (j - i)
	if delta == 0 {
		for k := 0; k < midLen; k++ {
			s.r[i+k] = mid[k]
		}
	} else if delta < 0 {
		for k := 0; k < midLen; k++ {
			s.r[i+k] = mid[k]
		}
		copy(s.r[i+midLen:], s.r[j:])
		s.r = s.r[:len(s.r)+delta]
	} else {
		s.r = append(s.r, [2]uint64{})
		copy(s.r[i+2:], s.r[i+1:])
		s.r[i] = mid[0]
		s.r[i+1] = mid[1]
	}
}

// maxSeen returns the highest covered offset or false.
func (s *rangeSet) maxSeen() (uint64, bool) {
	if len(s.r) == 0 {
		return 0, false
	}
	return s.r[len(s.r)-1][1], true
}

// clipBelow drops all coverage < lo.
func (s *rangeSet) clipBelow(lo uint64) { s.remove(0, lo) }

// intersectLen returns the total overlap between [lo,hi) and the set.
func (s *rangeSet) intersectLen(lo, hi uint64) uint64 {
	var n uint64
	i := s.search(lo)
	for ; i < len(s.r) && s.r[i][0] < hi; i++ {
		a, b := s.r[i][0], s.r[i][1]
		if a < lo {
			a = lo
		}
		if b > hi {
			b = hi
		}
		n += b - a
	}
	return n
}
