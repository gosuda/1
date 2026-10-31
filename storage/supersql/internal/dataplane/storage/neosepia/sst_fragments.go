package neosepia

import "bytes"

func (s *sstFile) containsKey(key []byte) bool {
	return (len(s.descriptor.LowerKey) == 0 || bytes.Compare(key, s.descriptor.LowerKey) >= 0) && (len(s.descriptor.UpperKey) == 0 || bytes.Compare(key, s.descriptor.UpperKey) < 0)
}

func (s *sstFile) fragmentBlocks(first, end int) (int, int) {
	return s.rangeBlocks(first, end, s.descriptor.LowerKey, s.descriptor.UpperKey)
}

func (s *sstFile) rangeBlocks(first, end int, lower, upper []byte) (int, int) {
	if len(lower) != 0 {
		low, high := first, end
		for low < high {
			middle := low+(high-low)/2
			if bytes.Compare(s.key(s.entry(middle).maximum), lower) < 0 { low = middle+1 } else { high = middle }
		}
		first = low
	}
	if len(upper) != 0 {
		low, high := first, end
		for low < high {
			middle := low+(high-low)/2
			if bytes.Compare(s.key(s.entry(middle).minimum), upper) < 0 { low = middle+1 } else { high = middle }
		}
		end = low
	}
	return first, end
}

func (s *sstFile) lowerBound() []byte {
	lower := s.key(s.entry(0).minimum)
	if bytes.Compare(s.descriptor.LowerKey, lower) > 0 { lower = s.descriptor.LowerKey }
	return lower
}

func (s *sstFile) upperBound() []byte {
	maximum := s.key(s.entry(s.count-1).maximum)
	if len(s.descriptor.UpperKey) != 0 && bytes.Compare(s.descriptor.UpperKey, maximum) <= 0 {
		return bytes.Clone(s.descriptor.UpperKey)
	}
	upper := make([]byte, len(maximum)+1)
	copy(upper, maximum)
	return upper
}

func samePreparedRun(a, b preparedRun) bool {
	return a.Name == b.Name && a.Target == b.Target && a.Rows == b.Rows && a.Bytes == b.Bytes && a.Checksum == b.Checksum && bytes.Equal(a.LowerKey, b.LowerKey) && bytes.Equal(a.UpperKey, b.UpperKey)
}

func (s *sstFile) endsBefore(key []byte) bool {
	return (len(s.descriptor.UpperKey) != 0 && bytes.Compare(s.descriptor.UpperKey, key) <= 0) || bytes.Compare(s.key(s.entry(s.count-1).maximum), key) < 0
}

func (s *sstFile) overlaps(lower, upper []byte) bool {
	return bytes.Compare(s.lowerBound(), upper) < 0 && !s.endsBefore(lower)
}

func validFragmentBounds(descriptor preparedRun, maxKeyBytes uint64) bool {
	return (len(descriptor.LowerKey) <= 1 || uint64(len(descriptor.LowerKey)-1) <= maxKeyBytes) &&
		(len(descriptor.UpperKey) <= 1 || uint64(len(descriptor.UpperKey)-1) <= maxKeyBytes) &&
		(len(descriptor.LowerKey) == 0 || len(descriptor.UpperKey) == 0 || bytes.Compare(descriptor.LowerKey, descriptor.UpperKey) < 0)
}
