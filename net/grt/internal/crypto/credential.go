package crypto

import (
	"crypto/subtle"
	"encoding/binary"

	"gosuda.org/grt/internal/wire"
)

// Credential is one parsed 196-byte compact credential record
// (spec section 9.2).
type Credential struct {
	Usage       uint16
	Issuer      [32]byte
	Subject     [32]byte
	ServiceHash [32]byte
	NotBefore   uint64
	NotAfter    uint64
	Serial      [16]byte
	Signature   [64]byte
}

// ParseCredential decodes the fixed record layout and checks version,
// usage-bit legality, and time-range ordering.
func ParseCredential(b []byte) (Credential, bool) {
	var c Credential
	if len(b) != wire.CredentialBytes {
		return c, false
	}
	if binary.BigEndian.Uint16(b[0:]) != 1 {
		return c, false // record version 1 only
	}
	c.Usage = binary.BigEndian.Uint16(b[2:])
	if c.Usage&^3 != 0 {
		return c, false // only bits 0 (leaf) and 1 (issue) exist
	}
	copy(c.Issuer[:], b[4:36])
	copy(c.Subject[:], b[36:68])
	copy(c.ServiceHash[:], b[68:100])
	c.NotBefore = binary.BigEndian.Uint64(b[100:])
	c.NotAfter = binary.BigEndian.Uint64(b[108:])
	copy(c.Serial[:], b[116:132])
	copy(c.Signature[:], b[132:196])
	if c.NotBefore > c.NotAfter {
		return c, false
	}
	return c, true
}

// VerifyCredentialChain validates a leaf-first credential chain
// (spec section 9.1/9.2): leaf subject = service key IK_S; each issuer is
// the next record's subject; the terminal issuer equals a trust anchor;
// usage bits, service hash, and validity windows all hold at nowUnixS.
func VerifyCredentialChain(creds [][wire.CredentialBytes]byte, serviceKey [32]byte, anchors [][32]byte, serviceHash [32]byte, nowUnixS int64) bool {
	n := len(creds)
	if n < 1 || n > wire.MaxCredentials {
		return false
	}
	parsed := make([]Credential, n)
	for i := range creds {
		c, ok := ParseCredential(creds[i][:])
		if !ok {
			return false
		}
		parsed[i] = c
		// Every record binds the requested service hash.
		if subtle.ConstantTimeCompare(c.ServiceHash[:], serviceHash[:]) != 1 {
			return false
		}
		// Validity window (an unsafe/absent clock must not pass).
		if nowUnixS < 0 || uint64(nowUnixS) < c.NotBefore || uint64(nowUnixS) > c.NotAfter {
			return false
		}
		// Usage: leaf requires bit0; issuers require bit1.
		if i == 0 {
			if c.Usage&1 == 0 {
				return false
			}
		} else if c.Usage&2 == 0 {
			return false
		}
		// Signatures verify under the record's issuer key.
		if !XEdDSAPublicIsValid(c.Issuer[:]) {
			return false
		}
		if !XEdDSAVerify(c.Issuer[:], CredentialMessage(creds[i][:wire.CredentialPrefix]), c.Signature[:]) {
			return false
		}
	}
	// Leaf subject is the presented service key.
	if subtle.ConstantTimeCompare(parsed[0].Subject[:], serviceKey[:]) != 1 {
		return false
	}
	// Chaining: issuer[i] == subject[i+1]; reject duplicate subjects
	// (which also excludes cycles within the presented list).
	for i := 0; i+1 < n; i++ {
		if subtle.ConstantTimeCompare(parsed[i].Issuer[:], parsed[i+1].Subject[:]) != 1 {
			return false
		}
		for j := i + 1; j < n; j++ {
			if subtle.ConstantTimeCompare(parsed[i].Subject[:], parsed[j].Subject[:]) == 1 {
				return false
			}
		}
	}
	// Terminal issuer must be a provisioned anchor.
	root := parsed[n-1].Issuer
	for _, a := range anchors {
		if subtle.ConstantTimeCompare(root[:], a[:]) == 1 {
			return true
		}
	}
	return false
}
