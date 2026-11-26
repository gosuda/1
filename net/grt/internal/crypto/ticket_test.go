package crypto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

const ticketTestDay = 20_000 // an arbitrary period index (2024-10-04)

func ticketTestMaster() TicketMaster {
	return TicketMaster{Key: [32]byte{0x5a, 1, 2, 3}, Generation: 7}
}

func ticketTestPlain() [TicketPlainBytes]byte {
	tp := TicketPlain{PSK: [32]byte{0xaa}, TicketID: [16]byte{0xbb},
		IssuedUnixS: ticketTestDay * TicketKeyPeriodS, MaxEarlyPlain: 4096}
	tp.ExpiresUnixS = tp.IssuedUnixS + TicketMaxLifetimeS
	return EncodeTicketPlain(&tp)
}

func sealTestTicket(t *testing.T, m *TicketMaster, now int64, counter uint64) [TicketWireBytes]byte {
	t.Helper()
	plain := ticketTestPlain()
	tkt, err := m.Seal(now, [16]byte{9}, counter, &plain)
	if err != nil {
		t.Fatalf("Seal(now=%d, counter=%d): %v", now, counter, err)
	}
	return tkt
}

func TestTicketMasterOpensCurrentAndPreviousPeriod(t *testing.T) {
	m := ticketTestMaster()
	issued := int64(ticketTestDay*TicketKeyPeriodS + TicketKeyPeriodS - 1) // last second of the period
	tkt := sealTestTicket(t, &m, issued, 1)
	cases := []struct {
		name string
		now  int64
		ok   bool
	}{
		{"same period", issued, true},
		{"next period", issued + 1, true},
		{"end of next period", issued + TicketKeyPeriodS, true},
		{"two periods later", issued + TicketKeyPeriodS + 1, false},
		{"period before issue", issued - TicketKeyPeriodS, false},
	}
	want := ticketTestPlain()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.Open(tc.now, tkt[:])
			if (err == nil) != tc.ok {
				t.Fatalf("Open(now=%d) err = %v, want ok=%v", tc.now, err, tc.ok)
			}
			if tc.ok {
				if enc := EncodeTicketPlain(&got); enc != want {
					t.Fatal("opened plaintext differs from sealed plaintext")
				}
			}
		})
	}
}

func TestTicketMasterKeyIDEncodesPeriodAndBlock(t *testing.T) {
	m := ticketTestMaster()
	now := int64(ticketTestDay * TicketKeyPeriodS)
	cases := []struct {
		name    string
		counter uint64
		wantSub uint32
	}{
		{"first block", 0, 0},
		{"last counter of first block", TicketSealsPerKey - 1, 0},
		{"first counter of second block", TicketSealsPerKey, 1},
		{"sub index wraps modulo 4096", 4096 * TicketSealsPerKey, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt := sealTestTicket(t, &m, now, tc.counter)
			id, _ := TicketKeyID(tkt[:])
			want := uint32(ticketTestDay)<<ticketSubBits | tc.wantSub
			if id != want {
				t.Fatalf("key_id = %#x, want %#x", id, want)
			}
			if got := binary.BigEndian.Uint64(tkt[20:28]); got != tc.counter {
				t.Fatalf("nonce counter = %d, want %d", got, tc.counter)
			}
			if _, err := m.Open(now, tkt[:]); err != nil {
				t.Fatalf("Open: %v", err)
			}
		})
	}
}

func TestTicketMasterBlocksUseDistinctKeys(t *testing.T) {
	m := ticketTestMaster()
	day := uint64(ticketTestDay)
	keys := map[[32]byte]string{}
	for name, k := range map[string][32]byte{
		"block 0":          m.periodKey(day, 0),
		"block 1":          m.periodKey(day, 1),
		"next period":      m.periodKey(day+1, 0),
		"other generation": (&TicketMaster{Key: m.Key, Generation: 8}).periodKey(day, 0),
	} {
		if prev, dup := keys[k]; dup {
			t.Fatalf("%s and %s derive the same envelope key", name, prev)
		}
		keys[k] = name
	}
}

func TestTicketMasterRejects(t *testing.T) {
	m := ticketTestMaster()
	now := int64(ticketTestDay * TicketKeyPeriodS)
	tkt := sealTestTicket(t, &m, now, 1)
	otherGen := TicketMaster{Key: m.Key, Generation: m.Generation + 1}
	if _, err := otherGen.Open(now, tkt[:]); err == nil {
		t.Fatal("ticket opened under a different master generation")
	}
	var zero TicketMaster
	if _, err := zero.Open(now, tkt[:]); err == nil {
		t.Fatal("all-zero master opened a ticket")
	}
	plain := ticketTestPlain()
	if _, err := zero.Seal(now, [16]byte{}, 1, &plain); !errors.Is(err, errTicketMasterUnset) {
		t.Fatalf("all-zero master Seal err = %v, want errTicketMasterUnset", err)
	}
	if _, err := m.Seal(-1, [16]byte{}, 1, &plain); !errors.Is(err, errTicketClock) {
		t.Fatalf("negative clock Seal err = %v, want errTicketClock", err)
	}
	if _, err := m.Open(-1, tkt[:]); err == nil {
		t.Fatal("ticket opened at a negative clock")
	}
	if _, err := m.Open(now, tkt[:TicketWireBytes-1]); err == nil {
		t.Fatal("truncated ticket opened")
	}
	tampered := tkt
	tampered[3] ^= 1 // key_id: same period, different block
	if _, err := m.Open(now, tampered[:]); err == nil {
		t.Fatal("ticket with altered key_id opened")
	}
}

func TestTicketMasterPeriodZeroHasNoPrevious(t *testing.T) {
	m := ticketTestMaster()
	// key_id period field 2^20-1 is (0-1) mod 2^20; period 0 must not
	// treat it as its predecessor.
	var tkt [TicketWireBytes]byte
	binary.BigEndian.PutUint32(tkt[0:], ticketPeriodMask<<ticketSubBits)
	if _, err := m.Open(0, tkt[:]); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("Open at period 0 err = %v, want ErrTicketInvalid", err)
	}
}

func TestTicketMasterZeroize(t *testing.T) {
	m := ticketTestMaster()
	m.Zeroize()
	if m != (TicketMaster{}) {
		t.Fatal("Zeroize left key material")
	}
}

func TestTicketEncodingsRoundTrip(t *testing.T) {
	meta := TicketMetadata{TicketID: [16]byte{1}, ExpiresUnixS: 0x0102030405060708,
		ReplayDomain: [16]byte{2}, ServiceHash: [32]byte{3}, MaxEarlyPlain: 0x0a0b0c0d,
		CachedParams: [128]byte{4, 127: 5}}
	mb := EncodeTicketMeta(&meta)
	if got := DecodeTicketMeta(mb[:]); got != meta {
		t.Fatalf("metadata round trip = %+v, want %+v", got, meta)
	}
	tp := TicketPlain{PSK: [32]byte{1}, TicketID: [16]byte{2}, IssuedUnixS: 3,
		ExpiresUnixS: 4, ReplayDomain: [16]byte{5}, ServiceHash: [32]byte{6},
		MetadataHash: [32]byte{7}, PolicyHash: [32]byte{8}, MaxEarlyPlain: 9}
	pb := EncodeTicketPlain(&tp)
	if got := DecodeTicketPlain(pb[:]); got != tp {
		t.Fatalf("plaintext round trip = %+v, want %+v", got, tp)
	}
}

func TestEarlyDataSealOpenAndBinder(t *testing.T) {
	ch0 := []byte("client hello through ticket metadata")
	ek := DeriveEarly([32]byte{0x11}, ch0)
	pt := []byte("early stream bytes")
	ct, err := EarlySeal(&ek, pt, ch0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := EarlyOpen(&ek, ct, ch0)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("EarlyOpen = %q, %v; want %q", got, err, pt)
	}
	if _, err := EarlyOpen(&ek, ct, []byte("different CH0")); err == nil {
		t.Fatal("early ciphertext opened under a different CH0 AAD")
	}
	binder := EarlyBinder(&ek, []byte("ch without binder"))
	if !EarlyBinderOK(&ek, []byte("ch without binder"), binder) {
		t.Fatal("valid binder rejected")
	}
	if EarlyBinderOK(&ek, []byte("other transcript"), binder) {
		t.Fatal("binder accepted for a different transcript")
	}
	if other := DeriveEarly([32]byte{0x11}, []byte("other CH0")); other.Key == ek.Key {
		t.Fatal("early key does not depend on CH0")
	}
	ek.Zeroize()
	if ek != (EarlyKeys{}) {
		t.Fatal("EarlyKeys.Zeroize left key material")
	}
}
