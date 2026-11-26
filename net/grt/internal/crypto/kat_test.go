package crypto

import (
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"os"
	"testing"
)

// Regression vectors in testdata/kat.json were generated once from this
// implementation and pinned. They are not independently derived; their
// purpose is to catch silent drift and to let independent GRT/1.1
// implementations cross-check their key schedule, ticket, token, and
// packet-protection outputs (spec section 27.1, Appendix C).
const katPath = "testdata/kat.json"

type hexBytes []byte

func (h hexBytes) MarshalJSON() ([]byte, error) { return json.Marshal(hex.EncodeToString(h)) }

func (h *hexBytes) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := hex.DecodeString(s)
	*h = v
	return err
}

func (h hexBytes) array32(t *testing.T, field string) [32]byte {
	t.Helper()
	if len(h) != 32 {
		t.Fatalf("%s: %d bytes, want 32", field, len(h))
	}
	return [32]byte(h)
}

type katFile struct {
	Comment    string         `json:"comment"`
	KDFLabel   []katKDF       `json:"kdf_label"`
	EpochKeys  []katEpochKeys `json:"epoch_keys"`
	TicketPSK  []katTicketPSK `json:"ticket_psk"`
	TicketSeal []katTicket    `json:"ticket_seal"`
	TokenRetry []katToken     `json:"token_retry"`
	AEADPacket []katAEAD      `json:"aead_packet"`
	PQXDHKDF   []katPQXDH     `json:"pqxdh_kdf"`
}

// katKDF: L(secret, label, context, length) of spec section 4.3.
type katKDF struct {
	Name    string   `json:"name"`
	Secret  hexBytes `json:"secret"`
	Label   string   `json:"label"`
	Context hexBytes `json:"context"`
	Length  int      `json:"length"`
	Output  hexBytes `json:"output"`
}

func (v *katKDF) compute(*testing.T) hexBytes { return L(v.Secret, v.Label, v.Context, v.Length) }

// katEpochKeys: DeriveEpochKeysFrom for one cipher and direction.
type katEpochKeys struct {
	Name      string   `json:"name"`
	Root      hexBytes `json:"root"`
	T1        hexBytes `json:"t1"`
	Cipher    uint16   `json:"cipher"`
	Epoch     uint32   `json:"epoch"`
	Direction int      `json:"direction"`
	Key       hexBytes `json:"key"`
	IV        hexBytes `json:"iv"`
}

func (v *katEpochKeys) compute(t *testing.T) (key, iv hexBytes) {
	key, iv = make(hexBytes, 32), make(hexBytes, NonceLen(v.Cipher))
	DeriveEpochKeysFrom(v.Root.array32(t, "root"), v.T1.array32(t, "t1"), v.Cipher, v.Epoch, v.Direction, key, iv)
	return key, iv
}

// katTicketPSK: L(resumption_master, "ticket psk", ticket_id, 32).
type katTicketPSK struct {
	Name             string   `json:"name"`
	ResumptionMaster hexBytes `json:"resumption_master"`
	TicketID         hexBytes `json:"ticket_id"`
	PSK              hexBytes `json:"psk"`
}

func (v *katTicketPSK) compute(t *testing.T) hexBytes {
	if len(v.TicketID) != 16 {
		t.Fatalf("ticket_id: %d bytes, want 16", len(v.TicketID))
	}
	psk := TicketPSK(v.ResumptionMaster.array32(t, "resumption_master"), [16]byte(v.TicketID))
	return psk[:]
}

// katTicket: TicketMaster.Seal; the envelope's first four bytes are the
// derived key_id.
type katTicket struct {
	Name       string   `json:"name"`
	Master     hexBytes `json:"master"`
	Generation uint32   `json:"generation"`
	NowUnixS   int64    `json:"now_unix_s"`
	Prefix     hexBytes `json:"nonce_prefix"`
	Counter    uint64   `json:"counter"`
	Plaintext  hexBytes `json:"plaintext"`
	Ticket     hexBytes `json:"ticket"`
}

func (v *katTicket) compute(t *testing.T) hexBytes {
	if len(v.Prefix) != 16 || len(v.Plaintext) != TicketPlainBytes {
		t.Fatalf("nonce_prefix/plaintext: %d/%d bytes, want 16/%d", len(v.Prefix), len(v.Plaintext), TicketPlainBytes)
	}
	m := TicketMaster{Key: v.Master.array32(t, "master"), Generation: v.Generation}
	plain := [TicketPlainBytes]byte(v.Plaintext)
	tkt, err := m.Seal(v.NowUnixS, [16]byte(v.Prefix), v.Counter, &plain)
	if err != nil {
		t.Fatal(err)
	}
	return tkt[:]
}

// katToken: a kind-0 RETRY token (spec section 6.2) under a fixed key.
type katToken struct {
	Name       string   `json:"name"`
	Key        hexBytes `json:"key"`
	KeyID      uint32   `json:"key_id"`
	NowUnixS   uint64   `json:"now_unix_s"`
	Domain     hexBytes `json:"listener_domain"`
	Source     string   `json:"source"`
	LocalDest  string   `json:"local_dest"`
	OrigDCID   uint64   `json:"orig_dcid"`
	RetryDCID  uint64   `json:"retry_dcid"`
	FlightHash hexBytes `json:"flight_hash"`
	Token      hexBytes `json:"token"`
}

func (v *katToken) compute(t *testing.T) hexBytes {
	s := NewTokenService(func() uint64 { return v.NowUnixS }, v.Domain)
	s.cur, s.curID = v.Key.array32(t, "key"), v.KeyID
	src, err := netip.ParseAddrPort(v.Source)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := netip.ParseAddrPort(v.LocalDest)
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := s.IssueRetry(v.OrigDCID, v.RetryDCID, v.FlightHash.array32(t, "flight_hash"), src, dst)
	if !ok {
		t.Fatal("IssueRetry refused")
	}
	return tok[:]
}

// katAEAD: EpochAEAD.Seal of one DATA packet (spec section 5.1).
type katAEAD struct {
	Name       string   `json:"name"`
	Cipher     uint16   `json:"cipher"`
	Key        hexBytes `json:"key"`
	IV         hexBytes `json:"iv"`
	PN         uint64   `json:"pn"`
	AAD        hexBytes `json:"aad"`
	Plaintext  hexBytes `json:"plaintext"`
	Ciphertext hexBytes `json:"ciphertext"`
}

func (v *katAEAD) compute(t *testing.T) hexBytes {
	e, err := NewEpochAEAD(v.Cipher, v.Key, v.IV)
	if err != nil {
		t.Fatal(err)
	}
	return e.Seal(v.PN, v.AAD, v.Plaintext, nil)
}

// katPQXDH: the PQXDH-shaped KDF of spec section 4.3.
type katPQXDH struct {
	Name string   `json:"name"`
	DH1  hexBytes `json:"dh1"`
	DH2  hexBytes `json:"dh2"`
	DH3  hexBytes `json:"dh3"`
	SS   hexBytes `json:"ss"`
	SK   hexBytes `json:"sk"`
}

func (v *katPQXDH) compute(t *testing.T) hexBytes {
	sk := PQXDHKDF(v.DH1.array32(t, "dh1"), v.DH2.array32(t, "dh2"), v.DH3.array32(t, "dh3"), v.SS.array32(t, "ss"))
	return sk[:]
}

func loadKAT(t *testing.T) *katFile {
	t.Helper()
	b, err := os.ReadFile(katPath)
	if err != nil {
		t.Fatalf("read %s: %v", katPath, err)
	}
	var f katFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse %s: %v", katPath, err)
	}
	return &f
}

func checkKAT(t *testing.T, field string, got, want hexBytes) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: vector has no expected value", field)
	}
	if string(got) != string(want) {
		t.Errorf("%s = %x, want %x", field, got, want)
	}
}

func TestKATKDFLabel(t *testing.T) {
	for _, v := range loadKAT(t).KDFLabel {
		t.Run(v.Name, func(t *testing.T) { checkKAT(t, "output", v.compute(t), v.Output) })
	}
}

func TestKATEpochKeys(t *testing.T) {
	for _, v := range loadKAT(t).EpochKeys {
		t.Run(v.Name, func(t *testing.T) {
			key, iv := v.compute(t)
			checkKAT(t, "key", key, v.Key)
			checkKAT(t, "iv", iv, v.IV)
		})
	}
}

func TestKATTicketPSK(t *testing.T) {
	for _, v := range loadKAT(t).TicketPSK {
		t.Run(v.Name, func(t *testing.T) { checkKAT(t, "psk", v.compute(t), v.PSK) })
	}
}

func TestKATTicketSeal(t *testing.T) {
	for _, v := range loadKAT(t).TicketSeal {
		t.Run(v.Name, func(t *testing.T) {
			checkKAT(t, "ticket", v.compute(t), v.Ticket)
			m := TicketMaster{Key: v.Master.array32(t, "master"), Generation: v.Generation}
			got, err := m.Open(v.NowUnixS, v.Ticket)
			if err != nil {
				t.Fatalf("Open pinned ticket: %v", err)
			}
			plain := EncodeTicketPlain(&got)
			checkKAT(t, "opened plaintext", plain[:], v.Plaintext)
		})
	}
}

func TestKATTokenRetry(t *testing.T) {
	for _, v := range loadKAT(t).TokenRetry {
		t.Run(v.Name, func(t *testing.T) { checkKAT(t, "token", v.compute(t), v.Token) })
	}
}

func TestKATAEADPacket(t *testing.T) {
	for _, v := range loadKAT(t).AEADPacket {
		t.Run(v.Name, func(t *testing.T) { checkKAT(t, "ciphertext", v.compute(t), v.Ciphertext) })
	}
}

func TestKATPQXDHKDF(t *testing.T) {
	for _, v := range loadKAT(t).PQXDHKDF {
		t.Run(v.Name, func(t *testing.T) { checkKAT(t, "sk", v.compute(t), v.SK) })
	}
}
