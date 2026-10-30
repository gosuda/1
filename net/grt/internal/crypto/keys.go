package crypto

import (
	"crypto/sha256"
	"encoding/binary"
)

// Key-schedule domain labels (spec sections 8.3-8.4, 19.3). The L()
// construction prepends "GRT11 "; these are the literal label fields.
const (
	lblServerFlightKey  = "server flight key"
	lblServerFlightIV   = "server flight iv"
	lblServerFinished   = "server finished"
	lblRoot             = "root"
	lblC2SKey           = "c2s key"
	lblC2SIV            = "c2s iv"
	lblS2CKey           = "s2c key"
	lblS2CIV            = "s2c iv"
	lblRefreshSecret    = "refresh secret"
	lblClientFinished   = "client finished"
	lblServerDone       = "server done"
	lblResumptionMaster = "resumption master"
	lblExporterMaster   = "exporter master"
	lblRefreshedRoot    = "refreshed root"
	lblRefreshConfirm   = "refresh confirmation"
	lblEarlyKey         = "early key"
	lblEarlyIV          = "early iv"
	lblBinder           = "binder"
	lblTicketPSK        = "ticket psk"
	lblAppExporter      = "application exporter"
)

// Direction indices.
const (
	DirC2S = 0
	DirS2C = 1
)

// HandshakeResult carries the derived handshake secrets. Private fields hold
// material that is erased from the owning engine after epoch-0 install.
type HandshakeResult struct {
	SK   [32]byte // PQXDH output
	T0   [32]byte
	HS   [32]byte
	T1   [32]byte
	Root [32]byte // ROOT0

	HsKey [32]byte // server flight body key (single use)
	HsIV  [24]byte // left-padded; first NLen(cs) used
	SfKey [32]byte // server finished key

	FinishKey        [32]byte
	DoneKey          [32]byte
	ResumptionMaster [32]byte
	ExporterMaster   [32]byte
	RefreshSecret0   [32]byte
}

// Zeroize erases every derived secret in place; callers defer it on the
// transient bundle after committing the fields they retain.
func (r *HandshakeResult) Zeroize() { *r = HandshakeResult{} }

// Kctx builds KCTX(e) = T1 || u16(cs) || u32(e).
func Kctx(cs uint16, epoch uint32, t1 []byte) []byte {
	out := make([]byte, 38)
	copy(out[0:], t1)
	binary.BigEndian.PutUint16(out[32:], cs)
	binary.BigEndian.PutUint32(out[34:], epoch)
	return out
}

// DeriveEpochKeysFrom derives directional key+iv from the epoch root.
// root is the 32-byte epoch root (ROOT0 or ROOTnew); t1 is the handshake T1.
func DeriveEpochKeysFrom(root, t1 [32]byte, cs uint16, epoch uint32, dir int, keyOut, ivOut []byte) {
	ctx := Kctx(cs, epoch, t1[:])
	var kLabel, ivLabel string
	if dir == DirC2S {
		kLabel, ivLabel = lblC2SKey, lblC2SIV
	} else {
		kLabel, ivLabel = lblS2CKey, lblS2CIV
	}
	LInto(keyOut, root[:], kLabel, ctx)
	LInto(ivOut, root[:], ivLabel, ctx)
}

// DeriveRefreshSecret derives refresh_secret[e] from the epoch root.
func DeriveRefreshSecret(root, t1 [32]byte, cs uint16, epoch uint32) [32]byte {
	return L32(root[:], lblRefreshSecret, Kctx(cs, epoch, t1[:]))
}

// DeriveServerFlight computes T0, HS, and the flight keys.
// clientMsg and sp are the exact wire encodings; psk is the ticket PSK or
// nil. cipher is the selected cipher.
func DeriveServerFlight(clientMsg, sp []byte, sk, psk [32]byte, hasPSK bool, cipher uint16) HandshakeResult {
	var r HandshakeResult
	r.SK = sk
	h := sha256.New()
	h.Write([]byte("GRT11 handshake"))
	h.Write(clientMsg)
	h.Write(sp)
	copy(r.T0[:], h.Sum(nil))
	var ikm [64]byte
	copy(ikm[0:], sk[:])
	if hasPSK {
		copy(ikm[32:], psk[:])
	}
	r.HS = Extract(r.T0[:], ikm[:])
	// HCTX = T0 || u16(selected_cipher)
	var hctx [34]byte
	copy(hctx[0:], r.T0[:])
	binary.BigEndian.PutUint16(hctx[32:], cipher)
	LInto(r.HsKey[:], r.HS[:], lblServerFlightKey, hctx[:])
	nl := NonceLen(cipher)
	LInto(r.HsIV[:nl], r.HS[:], lblServerFlightIV, hctx[:])
	r.SfKey = L32(r.HS[:], lblServerFinished, r.T0[:])
	return r
}

// ServerFinished computes HMAC(sf_key, T0 || H(SB)).
func ServerFinishedMAC(sfKey, t0 [32]byte, sb []byte) [32]byte {
	sbh := sha256.Sum256(sb)
	return HMAC256a(sfKey[:], t0[:], sbh[:])
}

// DeriveEstablished computes T1, ROOT0, and the epoch-0 secret set.
// clientMsg is the complete client message; serverMsg is SP||ciphertext.
func DeriveEstablished(hs *HandshakeResult, clientMsg, serverMsg []byte, cipher uint16) {
	h := sha256.New()
	h.Write([]byte("GRT11 established"))
	h.Write(clientMsg)
	h.Write(serverMsg)
	copy(hs.T1[:], h.Sum(nil))
	hs.Root = L32(hs.HS[:], lblRoot, hs.T1[:])
	hs.FinishKey = L32(hs.Root[:], lblClientFinished, hs.T1[:])
	hs.DoneKey = L32(hs.Root[:], lblServerDone, hs.T1[:])
	hs.ResumptionMaster = L32(hs.Root[:], lblResumptionMaster, hs.T1[:])
	hs.ExporterMaster = L32(hs.Root[:], lblExporterMaster, hs.T1[:])
	hs.RefreshSecret0 = DeriveRefreshSecret(hs.Root, hs.T1, cipher, 0)
}

// ClientFinishMAC computes HMAC(finish_key, T1 || client_reset_token).
func ClientFinishMAC(finishKey, t1 [32]byte, resetToken [16]byte) [32]byte {
	return HMAC256a(finishKey[:], t1[:], resetToken[:])
}

// ServerDoneMAC computes HMAC(done_key, T1 || client_finish_mac).
func ServerDoneMAC(doneKey, t1, finishMAC [32]byte) [32]byte {
	return HMAC256a(doneKey[:], t1[:], finishMAC[:])
}

// RefreshSecrets is the output of the section 19.3 derivation.
type RefreshSecrets struct {
	RT         [32]byte
	RootNew    [32]byte
	AnswerMAC  [32]byte
	CommitMAC  [32]byte
	ConfirmMAC [32]byte
}

// Zeroize erases the derived refresh secrets in place.
func (r *RefreshSecrets) Zeroize() { *r = RefreshSecrets{} }

// DeriveRefresh computes the refresh root and MACs:
//
//	RT = H("GRT11 refresh" || T1 || OFFER || AP)
//	RPRK = Extract(refresh_secret[current], D_R || SS_R)
//	ROOTnew = L(RPRK, "refreshed root", RT, 32)
//	rk = L(ROOTnew, "refresh confirmation", RT, 32)
//	answer_mac = HMAC(rk, "answer" || RT)   etc.
func DeriveRefresh(t1 [32]byte, offer, ap []byte, refreshSecret, dR, ssR [32]byte) RefreshSecrets {
	var r RefreshSecrets
	h := sha256.New()
	h.Write([]byte("GRT11 refresh"))
	h.Write(t1[:])
	h.Write(offer)
	h.Write(ap)
	copy(r.RT[:], h.Sum(nil))
	var ikm [64]byte
	copy(ikm[0:], dR[:])
	copy(ikm[32:], ssR[:])
	rprk := Extract(refreshSecret[:], ikm[:])
	r.RootNew = L32(rprk[:], lblRefreshedRoot, r.RT[:])
	rk := L32(r.RootNew[:], lblRefreshConfirm, r.RT[:])
	r.AnswerMAC = HMAC256a(rk[:], []byte("answer"), r.RT[:])
	r.CommitMAC = HMAC256a(rk[:], []byte("commit"), r.RT[:])
	r.ConfirmMAC = HMAC256a(rk[:], []byte("confirm"), r.RT[:])
	return r
}

// ServiceHash computes H("GRT11 service" || u16(len) || authority || app_id).
func ServiceHash(authority string, appID [16]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte("GRT11 service"))
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(len(authority)))
	h.Write(tmp[:])
	h.Write([]byte(authority))
	h.Write(appID[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// CredentialMessage builds the XEdDSA signing input
// "GRT11 credential" || prefix132.
func CredentialMessage(prefix132 []byte) []byte {
	out := make([]byte, 0, 16+len(prefix132))
	out = append(out, "GRT11 credential"...)
	out = append(out, prefix132...)
	return out
}

// Exporter derives an application exporter:
// L(exporter_master, "application exporter", H(u16(len(label))||label ||
// u16(len(ctx))||ctx), 32).
func Exporter(master [32]byte, label, context string) [32]byte {
	h := sha256.New()
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(len(label)))
	h.Write(tmp[:])
	h.Write([]byte(label))
	binary.BigEndian.PutUint16(tmp[:], uint16(len(context)))
	h.Write(tmp[:])
	h.Write([]byte(context))
	var inner [32]byte
	copy(inner[:], h.Sum(nil))
	return L32(master[:], lblAppExporter, inner[:])
}
