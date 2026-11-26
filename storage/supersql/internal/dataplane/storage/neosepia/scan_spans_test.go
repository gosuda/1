package neosepia

import "testing"

func TestPrefixBoundsIncludeOrExcludeEveryTupleExtension(t *testing.T) {
	prefix := []byte{0xff, 0xff}
	inclusive := encodedSpan{lower: keyCut{prefix: prefix}, upper: keyCut{prefix: prefix, after: true}}
	exclusiveLower := encodedSpan{lower: keyCut{prefix: prefix, after: true}, upper: keyCut{infinity: 1}}
	exclusiveUpper := encodedSpan{lower: keyCut{infinity: -1}, upper: keyCut{prefix: prefix}}
	for _, key := range [][]byte{{0xff, 0xff}, {0xff, 0xff, 0}, {0xff, 0xff, 0xff}} {
		if !inclusive.contains(key) {
			t.Fatalf("inclusive prefix lost extension %x", key)
		}
		if exclusiveLower.contains(key) || exclusiveUpper.contains(key) {
			t.Fatalf("exclusive prefix admitted extension %x", key)
		}
	}
	if inclusive.contains([]byte{0xff, 0xfe}) || !exclusiveUpper.contains([]byte{0xff, 0xfe}) {
		t.Fatal("prefix bound changed its preceding tuple")
	}
}

func TestCutOrderingDistinguishesNestedPrefixesFromAdjacentRanges(t *testing.T) {
	parent := keyCut{prefix: []byte("a"), after: true}
	child := keyCut{prefix: []byte("ab")}
	if compareCuts(parent, child) <= 0 {
		t.Fatal("nested prefix appeared disjoint from its parent interval")
	}
	boundary := keyCut{prefix: []byte("b")}
	if compareCuts(boundary, boundary) != 0 {
		t.Fatal("adjacent half-open ranges overlap")
	}
	if compareCuts(keyCut{prefix: []byte("a")}, child) >= 0 || compareCuts(child, parent) >= 0 {
		t.Fatal("child prefix is not inside its parent cuts")
	}
}
