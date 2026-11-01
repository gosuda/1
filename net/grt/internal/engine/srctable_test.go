package engine

import (
	"net/netip"
	"testing"
)

// A table filled by spoofed sources must admit new sources once the
// spoofed buckets have refilled, instead of refusing them forever.
func TestSrcTableAgesOutIdleSources(t *testing.T) {
	tab := newSrcTable(retrySrcPktRate, retrySrcPktBurst)
	const t0 = uint64(1_000_000)
	base := netip.MustParseAddr("10.0.0.0").As4()
	for i := range retrySrcMax {
		a := base
		a[1], a[2], a[3] = byte(i>>16), byte(i>>8), byte(i)
		b := tab.bucket(netip.AddrFrom4(a), t0)
		if b == nil {
			t.Fatalf("source %d refused before the table was full", i)
		}
		b.take(t0, retrySrcPktRate, retrySrcPktBurst, 1)
	}
	fresh := netip.MustParseAddr("192.0.2.1")
	if tab.bucket(fresh, t0+1) != nil {
		t.Fatal("saturated table admitted a new source while every bucket is throttling")
	}
	later := t0 + tab.idleUS + srcSweepFullIntervalUS
	if tab.bucket(fresh, later) == nil {
		t.Fatal("new source still refused after spoofed buckets refilled")
	}
	if len(tab.m) != 1 {
		t.Fatalf("sweep kept %d idle buckets, want only the new source", len(tab.m)-1)
	}
}

// A bucket that is still throttling survives the sweep.
func TestSrcTableKeepsThrottledSources(t *testing.T) {
	tab := newSrcTable(retrySrcPktRate, retrySrcPktBurst)
	src := netip.MustParseAddr("198.51.100.7")
	b := tab.bucket(src, 1)
	for b.take(1, retrySrcPktRate, retrySrcPktBurst, 1) {
	}
	tab.sweep(2)
	if tab.m[src] != b {
		t.Fatal("sweep dropped a bucket that is still throttling its source")
	}
}
