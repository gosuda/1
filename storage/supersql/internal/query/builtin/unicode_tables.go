package builtin

//go:generate go run ./internal/genunicode -input ../../../testdata/postgres184/unicode -output unicode_tables_generated.go

// The tables retain PostgreSQL's inline single-codepoint decompositions.
// Entries are values; no caller receives mutable table storage.
type unicodeNormEntry struct {
	decompositionIndex uint16
	combiningClass     uint8
	decompositionFlags uint8
}

const (
	unicodeDecompSize   = 0x1f
	unicodeDecompCompat = 0x20
	unicodeDecompInline = 0x40
)

func lookupUnicodeNorm(code rune) unicodeNormEntry {
	low, high := 0, len(unicodeNormCodes)
	for low < high {
		mid := low + (high-low)/2
		if unicodeNormCodes[mid] < code {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(unicodeNormCodes) && unicodeNormCodes[low] == code {
		return unicodeNormEntries[low]
	}
	return unicodeNormEntry{}
}

func (entry unicodeNormEntry) decompositionLen(compatibility bool) int {
	if !compatibility && entry.decompositionFlags&unicodeDecompCompat != 0 {
		return 0
	}
	return int(entry.decompositionFlags & unicodeDecompSize)
}

func (entry unicodeNormEntry) decompositionAt(index int) rune {
	if entry.decompositionFlags&unicodeDecompInline != 0 {
		return rune(entry.decompositionIndex)
	}
	return unicodeDecompositionCodes[int(entry.decompositionIndex)+index]
}

func composeUnicodePair(left, right rune) (rune, bool) {
	const (
		sBase  = 0xac00
		lBase  = 0x1100
		vBase  = 0x1161
		tBase  = 0x11a7
		lCount = 19
		vCount = 21
		tCount = 28
		sCount = lCount * vCount * tCount
	)
	if left >= lBase && left < lBase+lCount && right >= vBase && right < vBase+vCount {
		return sBase + ((left-lBase)*vCount+right-vBase)*tCount, true
	}
	// PostgreSQL 18.4 includes TBASE itself, unlike the Unicode TIndex > 0 rule.
	if left >= sBase && left < sBase+sCount && (left-sBase)%tCount == 0 && right >= tBase && right < tBase+tCount {
		return left + right - tBase, true
	}
	if uint32(left) > 0x10ffff || uint32(right) > 0x10ffff {
		return 0, false
	}
	key := uint64(left)<<21 | uint64(right)
	low, high := 0, len(unicodeCompositionKeys)
	for low < high {
		mid := low + (high-low)/2
		if unicodeCompositionKeys[mid] < key {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low < len(unicodeCompositionKeys) && unicodeCompositionKeys[low] == key {
		return unicodeCompositionCodes[low], true
	}
	return 0, false
}

type unicodeQC uint8

const (
	unicodeQCYes unicodeQC = iota
	unicodeQCNo
	unicodeQCMaybe
)

type unicodeQCRange struct {
	first, last rune
	flags       uint8 // NFC in bits 0..1, NFKC in bits 2..3.
}

func unicodeQuickCheck(code rune, compatibility, compose bool) unicodeQC {
	if !compose {
		if code >= 0xac00 && code < 0xac00+11172 || lookupUnicodeNorm(code).decompositionLen(compatibility) != 0 {
			return unicodeQCNo
		}
		return unicodeQCYes
	}
	low, high := 0, len(unicodeQuickCheckRanges)
	for low < high {
		mid := low + (high-low)/2
		if unicodeQuickCheckRanges[mid].last < code {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low == len(unicodeQuickCheckRanges) || code < unicodeQuickCheckRanges[low].first {
		return unicodeQCYes
	}
	flags := unicodeQuickCheckRanges[low].flags
	if compatibility {
		flags >>= 2
	}
	return unicodeQC(flags & 3)
}

func unicodeAssignedCodepoint(code rune) bool {
	low, high := 0, len(unicodeAssignedRanges)
	for low < high {
		mid := low + (high-low)/2
		if unicodeAssignedRanges[mid][1] < code {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low < len(unicodeAssignedRanges) && code >= unicodeAssignedRanges[low][0]
}
