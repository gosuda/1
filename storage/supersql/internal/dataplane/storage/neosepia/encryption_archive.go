package neosepia

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ErrInvalidEncryptionKey reports a missing, malformed, or mismatched AES-256
// encryption key.
var ErrInvalidEncryptionKey = errors.New("neosepia: invalid or missing encryption key")

const (
	encryptionMagic           = "NSE1"
	encryptionKeySize         = 32
	encryptionNonceSize       = 12
	encryptionTagSize         = 16
	encryptionHeaderSize      = 4 + 4 + encryptionNonceSize
	encryptionChunkHeaderSize = 4
	encryptionChunkOverhead   = encryptionChunkHeaderSize + encryptionTagSize
	encryptionDefaultChunk    = 64 << 10
	encryptionMaxChunk        = 4 << 20
	encryptionFinalBit        = uint32(1 << 31)
	encryptionKeyDomain       = "supersql-enc-key-v1"

	encryptionMetaName    = "encryption.meta"
	encryptionMetaPending = ".encryption.meta.pending"
	encryptionMetaVersion = uint32(1)
	encryptionMetaSize    = 4 + 4 + sha256.Size + sha256.Size
)

// WALArchiveEntry describes one commit WAL segment published by the backend.
type WALArchiveEntry struct {
	LogPosition  uint64
	CommitTS     model.Timestamp
	SourcePath   string
	ArchivedPath string
	Encrypted    bool
}

// WALArchiveMetadata is the lightweight JSON sidecar written alongside each
// archived WAL segment (<020d>.meta). Its fields are checked against the WAL
// before its timestamp can establish a PITR recovery cutoff.
type WALArchiveMetadata struct {
	LogPosition    uint64          `json:"log_position"`
	CommitTS       model.Timestamp `json:"commit_ts"`
	Encrypted      bool            `json:"encrypted"`
	KeyFingerprint string          `json:"key_fingerprint,omitempty"`
	Size           uint64          `json:"size"`
	SHA256         string          `json:"sha256"`
}

// storageCipher holds an immutable AES-256-GCM AEAD and key fingerprint.
// cipher.AEAD is safe for concurrent Seal and Open calls.
type storageCipher struct {
	aead        cipher.AEAD
	fingerprint [sha256.Size]byte
}

// KeyFingerprint computes the canonical key fingerprint
// SHA-256("supersql-enc-key-v1" || key) for a 32-byte AES-256 key.
func KeyFingerprint(key []byte) ([sha256.Size]byte, error) {
	if len(key) != encryptionKeySize {
		return [sha256.Size]byte{}, fmt.Errorf("%w: expected %d-byte AES-256 key, got %d bytes", ErrInvalidEncryptionKey, encryptionKeySize, len(key))
	}
	h := sha256.New()
	_, _ = h.Write([]byte(encryptionKeyDomain))
	_, _ = h.Write(key)
	var fp [sha256.Size]byte
	h.Sum(fp[:0])
	return fp, nil
}

func newStorageCipher(key []byte) (*storageCipher, error) {
	if len(key) == 0 {
		return nil, nil
	}
	fp, err := KeyFingerprint(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEncryptionKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEncryptionKey, err)
	}
	return &storageCipher{aead: aead, fingerprint: fp}, nil
}

func (c *storageCipher) matchesFingerprint(fp [sha256.Size]byte) bool {
	if c == nil {
		return false
	}
	return subtle.ConstantTimeCompare(c.fingerprint[:], fp[:]) == 1
}

func (c *storageCipher) fingerprintHex() string {
	if c == nil {
		return ""
	}
	return hex.EncodeToString(c.fingerprint[:])
}

func deriveChunkNonce(base [encryptionNonceSize]byte, chunkIndex uint64) [encryptionNonceSize]byte {
	nonce := base
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], chunkIndex)
	for i := range 8 {
		nonce[4+i] ^= idx[i]
	}
	return nonce
}

func buildChunkAAD(chunkIndex uint64, plainLen uint32, isFinal bool) [17]byte {
	var aad [17]byte
	copy(aad[0:4], encryptionMagic)
	binary.BigEndian.PutUint64(aad[4:12], chunkIndex)
	binary.BigEndian.PutUint32(aad[12:16], plainLen)
	if isFinal {
		aad[16] = 1
	}
	return aad
}

func encryptedFileSize(plainSize uint64) uint64 {
	if plainSize == 0 {
		return encryptionHeaderSize + encryptionChunkOverhead
	}
	chunks := (plainSize-1)/encryptionDefaultChunk + 1
	return encryptionHeaderSize + plainSize + chunks*encryptionChunkOverhead
}

func parseEncryptedHeader(header []byte) (chunkSize uint64, baseNonce [encryptionNonceSize]byte, err error) {
	if len(header) != encryptionHeaderSize || string(header[:4]) != encryptionMagic {
		return 0, baseNonce, storage.ErrCorruptStorage
	}
	cs := uint64(binary.BigEndian.Uint32(header[4:8]))
	if cs == 0 || cs > encryptionMaxChunk {
		return 0, baseNonce, storage.ErrCorruptStorage
	}
	copy(baseNonce[:], header[8:20])
	return cs, baseNonce, nil
}

// encryptStreamWriter streams plaintext into chunked NSE1 AES-256-GCM frames
// using O(chunkSize) memory.
type encryptStreamWriter struct {
	out           io.Writer
	cipher        *storageCipher
	chunkSize     int
	baseNonce     [encryptionNonceSize]byte
	headerWritten bool
	chunkIndex    uint64
	buf           []byte
	sealed        []byte
	closed        bool
	err           error
}

func newEncryptStreamWriter(out io.Writer, c *storageCipher) (*encryptStreamWriter, error) {
	if c == nil {
		return nil, ErrInvalidEncryptionKey
	}
	var baseNonce [encryptionNonceSize]byte
	if _, err := io.ReadFull(rand.Reader, baseNonce[:]); err != nil {
		return nil, err
	}
	return &encryptStreamWriter{
		out:       out,
		cipher:    c,
		chunkSize: encryptionDefaultChunk,
		baseNonce: baseNonce,
		buf:       make([]byte, 0, encryptionDefaultChunk),
		sealed:    make([]byte, 0, encryptionChunkHeaderSize+encryptionDefaultChunk+encryptionTagSize),
	}, nil
}

func (w *encryptStreamWriter) writeHeader() error {
	if w.headerWritten {
		return nil
	}
	var hdr [encryptionHeaderSize]byte
	copy(hdr[0:4], encryptionMagic)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(w.chunkSize))
	copy(hdr[8:20], w.baseNonce[:])
	n, err := w.out.Write(hdr[:])
	if err != nil {
		return err
	}
	if n != len(hdr) {
		return io.ErrShortWrite
	}
	w.headerWritten = true
	return nil
}

func (w *encryptStreamWriter) flushChunk(isFinal bool) error {
	if err := w.writeHeader(); err != nil {
		return err
	}
	plainLen := uint32(len(w.buf))
	flags := plainLen
	if isFinal {
		flags |= encryptionFinalBit
	}
	w.sealed = w.sealed[:encryptionChunkHeaderSize]
	binary.BigEndian.PutUint32(w.sealed[:encryptionChunkHeaderSize], flags)
	nonce := deriveChunkNonce(w.baseNonce, w.chunkIndex)
	aad := buildChunkAAD(w.chunkIndex, plainLen, isFinal)
	w.sealed = w.cipher.aead.Seal(w.sealed, nonce[:], w.buf, aad[:])
	n, err := w.out.Write(w.sealed)
	if err != nil {
		return err
	}
	if n != len(w.sealed) {
		return io.ErrShortWrite
	}
	w.chunkIndex++
	w.buf = w.buf[:0]
	return nil
}

func (w *encryptStreamWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, storage.ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	total := 0
	for len(p) > 0 {
		// Defer flushing a full buffer until additional bytes arrive so
		// exact chunk-multiple payloads terminate on the last full chunk.
		if len(w.buf) == w.chunkSize {
			if err := w.flushChunk(false); err != nil {
				w.err = err
				return total, err
			}
		}
		space := w.chunkSize - len(w.buf)
		take := min(len(p), space)
		w.buf = append(w.buf, p[:take]...)
		p = p[take:]
		total += take
	}
	return total, nil
}

func (w *encryptStreamWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	w.err = w.flushChunk(true)
	return w.err
}

// decryptStreamToBytes decrypts an NSE1 stream into memory with a strict upper
// bound on plaintext length, verifying every chunk's GCM tag and trailing EOF.
func decryptStreamToBytes(ctx context.Context, r io.Reader, c *storageCipher, maxPlain uint64) ([]byte, error) {
	if c == nil {
		return nil, ErrInvalidEncryptionKey
	}
	var hdr [encryptionHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	chunkSize, baseNonce, err := parseEncryptedHeader(hdr[:])
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	var chunkIdx uint64
	var encBuf []byte
	var plainBuf []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var ch [encryptionChunkHeaderSize]byte
		if _, err := io.ReadFull(r, ch[:]); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		flags := binary.BigEndian.Uint32(ch[:])
		isFinal := (flags & encryptionFinalBit) != 0
		plainLen := uint64(flags &^ encryptionFinalBit)
		if plainLen > chunkSize || (!isFinal && plainLen != chunkSize) {
			return nil, storage.ErrCorruptStorage
		}
		if uint64(out.Len()) > maxPlain || plainLen > maxPlain-uint64(out.Len()) {
			return nil, storage.ErrDecodeBudget
		}
		ctLen := int(plainLen) + encryptionTagSize
		if cap(encBuf) < ctLen {
			encBuf = make([]byte, ctLen)
		} else {
			encBuf = encBuf[:ctLen]
		}
		if _, err := io.ReadFull(r, encBuf); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		nonce := deriveChunkNonce(baseNonce, chunkIdx)
		aad := buildChunkAAD(chunkIdx, uint32(plainLen), isFinal)
		var decErr error
		plainBuf, decErr = c.aead.Open(plainBuf[:0], nonce[:], encBuf, aad[:])
		if decErr != nil {
			return nil, errors.Join(ErrInvalidEncryptionKey, storage.ErrCorruptStorage, decErr)
		}
		if uint64(len(plainBuf)) != plainLen {
			return nil, storage.ErrCorruptStorage
		}
		_, _ = out.Write(plainBuf)
		chunkIdx++
		if isFinal {
			break
		}
	}
	var trailing [1]byte
	n, err := r.Read(trailing[:])
	if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return nil, storage.ErrCorruptStorage
	}
	return out.Bytes(), nil
}

// sstEncWriter streams encrypted SSTable chunks directly to w.file with O(2 * chunkSize)
// memory: chunk 0 is held in memory until Finish writes the 80-byte SST header
// at plaintext offset 0, while chunks >= 1 stream straight to their final NSE1
// offsets on disk.
type sstEncWriter struct {
	file        *os.File
	cipher      *storageCipher
	chunkSize   uint64
	baseNonce   [encryptionNonceSize]byte
	chunk0      []byte
	curChunk    []byte
	curIdx      uint64
	curLen      uint64
	hasCur      bool
	maxPlainEnd uint64
	sealed      []byte
}

func newSSTEncWriter(file *os.File, c *storageCipher) (*sstEncWriter, error) {
	if c == nil {
		return nil, ErrInvalidEncryptionKey
	}
	var baseNonce [encryptionNonceSize]byte
	if _, err := io.ReadFull(rand.Reader, baseNonce[:]); err != nil {
		return nil, err
	}
	return &sstEncWriter{
		file:      file,
		cipher:    c,
		chunkSize: encryptionDefaultChunk,
		baseNonce: baseNonce,
		chunk0:    make([]byte, encryptionDefaultChunk),
		curChunk:  make([]byte, encryptionDefaultChunk),
		sealed:    make([]byte, 0, encryptionChunkHeaderSize+encryptionDefaultChunk+encryptionTagSize),
	}, nil
}

func (ew *sstEncWriter) chunkDiskOffset(chunkIdx uint64) int64 {
	return int64(encryptionHeaderSize + chunkIdx*(ew.chunkSize+encryptionChunkOverhead))
}

func (ew *sstEncWriter) writeEncryptedChunk(chunkIdx uint64, plain []byte, isFinal bool) error {
	plainLen := uint32(len(plain))
	flags := plainLen
	if isFinal {
		flags |= encryptionFinalBit
	}
	ew.sealed = ew.sealed[:encryptionChunkHeaderSize]
	binary.BigEndian.PutUint32(ew.sealed[:encryptionChunkHeaderSize], flags)
	nonce := deriveChunkNonce(ew.baseNonce, chunkIdx)
	aad := buildChunkAAD(chunkIdx, plainLen, isFinal)
	ew.sealed = ew.cipher.aead.Seal(ew.sealed, nonce[:], plain, aad[:])
	n, err := ew.file.WriteAt(ew.sealed, ew.chunkDiskOffset(chunkIdx))
	if err != nil {
		return err
	}
	if n != len(ew.sealed) {
		return io.ErrShortWrite
	}
	return nil
}

func (ew *sstEncWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, storage.ErrInvalidTarget
	}
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	pos := uint64(off)
	if pos+uint64(total) > ew.maxPlainEnd {
		ew.maxPlainEnd = pos + uint64(total)
	}
	if pos < ew.chunkSize {
		take := min(uint64(len(p)), ew.chunkSize-pos)
		copy(ew.chunk0[pos:pos+take], p[:take])
		p = p[take:]
		pos += take
	}
	for len(p) > 0 {
		idx := pos / ew.chunkSize
		inChunk := pos % ew.chunkSize
		if ew.hasCur && idx != ew.curIdx {
			if idx < ew.curIdx || ew.curLen != ew.chunkSize {
				return 0, storage.ErrCorruptStorage
			}
			if err := ew.writeEncryptedChunk(ew.curIdx, ew.curChunk[:ew.chunkSize], false); err != nil {
				return 0, err
			}
			clear(ew.curChunk)
			ew.curIdx = idx
			ew.curLen = 0
		} else if !ew.hasCur {
			ew.hasCur = true
			ew.curIdx = idx
			ew.curLen = 0
		}
		take := min(uint64(len(p)), ew.chunkSize-inChunk)
		copy(ew.curChunk[inChunk:inChunk+take], p[:take])
		if inChunk+take > ew.curLen {
			ew.curLen = inChunk + take
		}
		p = p[take:]
		pos += take
	}
	return total, nil
}

func (ew *sstEncWriter) Finish() error {
	if !ew.hasCur {
		if err := ew.writeEncryptedChunk(0, ew.chunk0[:ew.maxPlainEnd], true); err != nil {
			return err
		}
	} else {
		if err := ew.writeEncryptedChunk(0, ew.chunk0[:ew.chunkSize], false); err != nil {
			return err
		}
		if err := ew.writeEncryptedChunk(ew.curIdx, ew.curChunk[:ew.curLen], true); err != nil {
			return err
		}
	}
	var hdr [encryptionHeaderSize]byte
	copy(hdr[0:4], encryptionMagic)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(ew.chunkSize))
	copy(hdr[8:20], ew.baseNonce[:])
	n, err := ew.file.WriteAt(hdr[:], 0)
	if err != nil {
		return err
	}
	if n != len(hdr) {
		return io.ErrShortWrite
	}
	return nil
}

// sstEncState provides thread-safe, random-access authenticated decryption over
// an NSE1-encrypted SSTable file. All fields are immutable after openSST.
type sstEncState struct {
	cipher     *storageCipher
	chunkSize  uint64
	baseNonce  [encryptionNonceSize]byte
	diskSize   uint64
	plainSize  uint64
	fullChunks uint64
	lastPlain  uint64
}

func newSSTEncState(file *os.File, diskSize int64, c *storageCipher) (*sstEncState, error) {
	if c == nil {
		return nil, ErrInvalidEncryptionKey
	}
	if diskSize < encryptionHeaderSize+encryptionChunkOverhead {
		return nil, storage.ErrCorruptStorage
	}
	var hdr [encryptionHeaderSize]byte
	if _, err := file.ReadAt(hdr[:], 0); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	chunkSize, baseNonce, err := parseEncryptedHeader(hdr[:])
	if err != nil {
		return nil, err
	}
	encChunkSize := chunkSize + encryptionChunkOverhead
	bodySize := uint64(diskSize) - encryptionHeaderSize
	fullChunks := (bodySize - encryptionChunkOverhead) / encChunkSize
	lastDisk := bodySize - fullChunks*encChunkSize
	if lastDisk < encryptionChunkOverhead || lastDisk > encChunkSize {
		return nil, storage.ErrCorruptStorage
	}
	lastPlain := lastDisk - encryptionChunkOverhead
	plainSize := fullChunks*chunkSize + lastPlain
	return &sstEncState{
		cipher:     c,
		chunkSize:  chunkSize,
		baseNonce:  baseNonce,
		diskSize:   uint64(diskSize),
		plainSize:  plainSize,
		fullChunks: fullChunks,
		lastPlain:  lastPlain,
	}, nil
}

func (es *sstEncState) readAt(r io.ReaderAt, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, storage.ErrCorruptStorage
	}
	if len(p) == 0 {
		return 0, nil
	}
	start := uint64(off)
	if start >= es.plainSize {
		return 0, io.EOF
	}
	remaining := uint64(len(p))
	wantEOF := false
	if remaining > es.plainSize-start {
		remaining = es.plainSize - start
		wantEOF = true
	}
	encChunkSize := es.chunkSize + encryptionChunkOverhead
	firstChunk := start / es.chunkSize
	lastChunk := (start + remaining - 1) / es.chunkSize
	copied := 0
	var rawBuf []byte
	var plainBuf []byte
	for chunkIdx := firstChunk; chunkIdx <= lastChunk; chunkIdx++ {
		isFinal := chunkIdx == es.fullChunks
		expectedPlain := es.chunkSize
		if isFinal {
			expectedPlain = es.lastPlain
		}
		chunkDiskLen := encryptionChunkHeaderSize + expectedPlain + encryptionTagSize
		diskOff := int64(encryptionHeaderSize + chunkIdx*encChunkSize)
		if uint64(cap(rawBuf)) < chunkDiskLen {
			rawBuf = make([]byte, chunkDiskLen)
		} else {
			rawBuf = rawBuf[:chunkDiskLen]
		}
		if n, err := r.ReadAt(rawBuf, diskOff); err != nil || uint64(n) != chunkDiskLen {
			return copied, errors.Join(storage.ErrCorruptStorage, err)
		}
		flags := binary.BigEndian.Uint32(rawBuf[:encryptionChunkHeaderSize])
		gotFinal := (flags & encryptionFinalBit) != 0
		gotPlainLen := uint64(flags &^ encryptionFinalBit)
		if gotFinal != isFinal || gotPlainLen != expectedPlain {
			return copied, storage.ErrCorruptStorage
		}
		nonce := deriveChunkNonce(es.baseNonce, chunkIdx)
		aad := buildChunkAAD(chunkIdx, uint32(expectedPlain), isFinal)
		var decErr error
		plainBuf, decErr = es.cipher.aead.Open(plainBuf[:0], nonce[:], rawBuf[encryptionChunkHeaderSize:], aad[:])
		if decErr != nil {
			return copied, errors.Join(ErrInvalidEncryptionKey, storage.ErrCorruptStorage, decErr)
		}
		if uint64(len(plainBuf)) != expectedPlain {
			return copied, storage.ErrCorruptStorage
		}
		sliceStart := uint64(0)
		if chunkIdx == firstChunk {
			sliceStart = start % es.chunkSize
		}
		sliceEnd := expectedPlain
		if sliceEnd-sliceStart > remaining-uint64(copied) {
			sliceEnd = sliceStart + (remaining - uint64(copied))
		}
		n := copy(p[copied:], plainBuf[sliceStart:sliceEnd])
		copied += n
	}
	if wantEOF {
		return copied, io.EOF
	}
	return copied, nil
}

// writeEncryptionMeta atomically persists encryption.meta containing the
// authenticated key fingerprint at the database root.
func writeEncryptionMeta(root *os.Root, c *storageCipher) (err error) {
	if c == nil {
		return nil
	}
	var payload [encryptionMetaSize]byte
	copy(payload[0:4], encryptionMagic)
	binary.BigEndian.PutUint32(payload[4:8], encryptionMetaVersion)
	copy(payload[8:40], c.fingerprint[:])
	sum := sha256.Sum256(payload[:40])
	copy(payload[40:72], sum[:])
	if err := root.Remove(encryptionMetaPending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(encryptionMetaPending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !renamed {
			if rmErr := root.Remove(encryptionMetaPending); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	n, writeErr := file.Write(payload[:])
	if writeErr != nil {
		return writeErr
	}
	if n != len(payload) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	if err := root.Rename(encryptionMetaPending, encryptionMetaName); err != nil {
		return err
	}
	renamed = true
	return syncCommitDirectory(root)
}

func readEncryptionMeta(root *os.Root) (fp [sha256.Size]byte, exists bool, err error) {
	file, err := root.Open(encryptionMetaName)
	if errors.Is(err, os.ErrNotExist) {
		return fp, false, nil
	}
	if err != nil {
		return fp, false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return fp, true, err
	}
	if !info.Mode().IsRegular() || info.Size() != encryptionMetaSize {
		return fp, true, storage.ErrCorruptStorage
	}
	var payload [encryptionMetaSize]byte
	if _, err := io.ReadFull(file, payload[:]); err != nil {
		return fp, true, errors.Join(storage.ErrCorruptStorage, err)
	}
	if string(payload[0:4]) != encryptionMagic || binary.BigEndian.Uint32(payload[4:8]) != encryptionMetaVersion {
		return fp, true, storage.ErrCorruptStorage
	}
	expected := sha256.Sum256(payload[:40])
	if expected != [sha256.Size]byte(payload[40:72]) {
		return fp, true, storage.ErrCorruptStorage
	}
	copy(fp[:], payload[8:40])
	return fp, true, nil
}

// verifyDirectoryEncryption ensures the configured cipher matches the directory's
// persisted encryption.meta state.
func verifyDirectoryEncryption(root *os.Root, c *storageCipher, existingDB bool) (hasMeta bool, err error) {
	fp, exists, err := readEncryptionMeta(root)
	if err != nil {
		return false, err
	}
	if exists {
		if c == nil {
			return true, fmt.Errorf("%w: database is encrypted and requires EncryptionKey", ErrInvalidEncryptionKey)
		}
		if !c.matchesFingerprint(fp) {
			return true, fmt.Errorf("%w: key fingerprint mismatch", ErrInvalidEncryptionKey)
		}
		return true, nil
	}
	if existingDB && c != nil {
		return false, fmt.Errorf("%w: cannot open unencrypted database with EncryptionKey", ErrInvalidEncryptionKey)
	}
	return false, nil
}

// writeCommitWithCipher writes a commit WAL segment, transparently encrypting
// it with NSE1 AES-256-GCM when c != nil. When c == nil it delegates directly
// to the canonical unencrypted writeCommit path.
func writeCommitWithCipher(ctx context.Context, root *os.Root, position uint64, batch storage.MutationBatch, identity [32]byte, maxBytes uint64, fault commitFault, c *storageCipher) (result storage.ApplyResult, err error) {
	if c == nil {
		return writeCommit(ctx, root, position, batch, identity, maxBytes, fault)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if position == 0 || maxBytes == 0 || maxBytes > math.MaxInt64-2*commitHeaderSize {
		return result, storage.ErrInvalidMutation
	}
	pending := pendingCommitName(position)
	committed := commitName(position)
	file, err := root.OpenFile(pending, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return result, err
	}
	publicationAttempted := false
	defer func() {
		cleanupErr := file.Close()
		removeErr := root.Remove(pending)
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
		if cleanupErr != nil && publicationAttempted {
			cleanupErr = errors.Join(storage.ErrCommitIndeterminate, cleanupErr)
		}
		err = errors.Join(err, cleanupErr)
	}()
	encWriter, err := newEncryptStreamWriter(file, c)
	if err != nil {
		return result, err
	}
	if err := writeCommitFrames(ctx, encWriter, position, batch, identity, maxBytes, fault); err != nil {
		return result, err
	}
	if err := encWriter.Close(); err != nil {
		return result, err
	}
	if err := file.Sync(); err != nil {
		return result, err
	}
	if fault != nil {
		if err := fault(commitFileSynced); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	publicationAttempted = true
	if err := root.Link(pending, committed); err != nil {
		return result, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	if fault != nil {
		if err := fault(commitPublished); err != nil {
			return result, errors.Join(storage.ErrCommitIndeterminate, err)
		}
	}
	if err := syncCommitDirectory(root); err != nil {
		return result, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	if fault != nil {
		if err := fault(commitDirectorySynced); err != nil {
			return result, errors.Join(storage.ErrCommitIndeterminate, err)
		}
	}
	return storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}, nil
}

func writeCommitFrames(ctx context.Context, out io.Writer, position uint64, batch storage.MutationBatch, identity [32]byte, maxBytes uint64, fault commitFault) error {
	var header [commitHeaderSize]byte
	copy(header[:], commitMagic)
	binary.LittleEndian.PutUint64(header[8:], commitHeaderSize)
	binary.LittleEndian.PutUint64(header[16:], position)
	copy(header[24:], batch.TxID[:])
	binary.LittleEndian.PutUint64(header[40:], uint64(batch.CommitTS))
	binary.LittleEndian.PutUint64(header[48:], batch.Count())
	binary.LittleEndian.PutUint32(header[56:], batch.ChecksumCRC)
	binary.LittleEndian.PutUint32(header[60:], batch.EncodingVersion)
	copy(header[64:], identity[:])
	headerHash := sha256.Sum256(header[:96])
	copy(header[96:], headerHash[:])
	n, writeErr := out.Write(header[:])
	if writeErr != nil {
		return writeErr
	}
	if n != len(header) {
		return io.ErrShortWrite
	}
	if fault != nil {
		if err := fault(commitBeginWritten); err != nil {
			return err
		}
	}
	writer := commitWriter{writer: out, limit: maxBytes, fault: fault}
	hash := sha256.New()
	if err := storage.EncodeMutationBatch(ctx, io.MultiWriter(&writer, hash), batch); err != nil {
		return err
	}
	var actual [32]byte
	hash.Sum(actual[:0])
	if actual != identity {
		return storage.ErrChecksumMismatch
	}
	if fault != nil {
		if err := fault(commitBodyWritten); err != nil {
			return err
		}
	}
	footer := header
	copy(footer[:], commitEndMagic)
	binary.LittleEndian.PutUint64(footer[8:], writer.count)
	footerHash := sha256.Sum256(footer[:96])
	copy(footer[96:], footerHash[:])
	n, writeErr = out.Write(footer[:])
	if writeErr != nil {
		return writeErr
	}
	if n != len(footer) {
		return io.ErrShortWrite
	}
	if fault != nil {
		if err := fault(commitMarkerWritten); err != nil {
			return err
		}
	}
	return nil
}

// openCommitWithCipher opens and validates a commit WAL segment, transparently
// decrypting NSE1-encrypted segments when c != nil.
func openCommitWithCipher(ctx context.Context, root *os.Root, position uint64, limits storage.BatchDecodeLimits, c *storageCipher) (_ *storedCommit, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := root.Open(commitName(position))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 4 {
		return nil, storage.ErrCorruptStorage
	}
	var prefix [4]byte
	if _, err := file.ReadAt(prefix[:], 0); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	if string(prefix[:]) != encryptionMagic {
		if c != nil {
			return nil, fmt.Errorf("%w: unencrypted WAL segment in encrypted database", ErrInvalidEncryptionKey)
		}
		_ = file.Close()
		return openCommit(ctx, root, position, limits)
	}
	if c == nil {
		return nil, fmt.Errorf("%w: encrypted WAL segment requires EncryptionKey", ErrInvalidEncryptionKey)
	}
	maxPlain := limits.MaxBytes
	if maxPlain > math.MaxUint64-2*commitHeaderSize {
		maxPlain = math.MaxUint64 - 2*commitHeaderSize
	}
	plain, err := decryptStreamToBytes(ctx, file, c, maxPlain+2*commitHeaderSize)
	if err != nil {
		return nil, err
	}
	closeErr := file.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	// Clear defer cleanup since file is already closed; decoded batch reads
	// from the immutable decrypted bytes buffer.
	record, parseErr := parseCommitBytes(ctx, plain, position, limits)
	if parseErr != nil {
		return nil, parseErr
	}
	return record, nil
}

func parseCommitBytes(ctx context.Context, plain []byte, position uint64, limits storage.BatchDecodeLimits) (*storedCommit, error) {
	if len(plain) < 2*commitHeaderSize {
		return nil, storage.ErrCorruptStorage
	}
	header := plain[:commitHeaderSize]
	headerHash := sha256.Sum256(header[:96])
	if string(header[:8]) != commitMagic || string(header[96:128]) != string(headerHash[:]) {
		return nil, storage.ErrCorruptStorage
	}
	if binary.LittleEndian.Uint64(header[8:]) != commitHeaderSize {
		return nil, storage.ErrCorruptStorage
	}
	footer := plain[len(plain)-commitHeaderSize:]
	footerHash := sha256.Sum256(footer[:96])
	if string(footer[:8]) != commitEndMagic || string(footer[96:128]) != string(footerHash[:]) {
		return nil, storage.ErrCorruptStorage
	}
	if !bytes.Equal(footer[16:96], header[16:96]) {
		return nil, storage.ErrCorruptStorage
	}
	length := binary.LittleEndian.Uint64(footer[8:])
	if length != uint64(len(plain)-2*commitHeaderSize) || binary.LittleEndian.Uint64(header[16:]) != position {
		return nil, storage.ErrCorruptStorage
	}
	if length > limits.MaxBytes {
		return nil, storage.ErrDecodeBudget
	}
	identity := [32]byte(header[64:96])
	reader := bytes.NewReader(plain)
	if err := verifyCommitBody(ctx, reader, length, identity); err != nil {
		return nil, err
	}
	source := io.NewSectionReader(reader, commitHeaderSize, int64(length))
	batch, err := storage.OpenEncodedMutationBatch(ctx, source, length, limits)
	if err != nil {
		return nil, err
	}
	if string(batch.TxID[:]) != string(header[24:40]) || uint64(batch.CommitTS) != binary.LittleEndian.Uint64(header[40:]) || batch.Count() != binary.LittleEndian.Uint64(header[48:]) || batch.ChecksumCRC != binary.LittleEndian.Uint32(header[56:]) || batch.EncodingVersion != binary.LittleEndian.Uint32(header[60:]) {
		return nil, storage.ErrCorruptStorage
	}
	return &storedCommit{
		Batch:    batch,
		Result:   storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position},
		Identity: identity,
	}, nil
}

// InspectCommitHeader parses the canonical 128-byte BEGIN header of a WAL
// segment (decrypting with key if NSE1-encrypted) and returns its
// (logPosition, commitTS).
func InspectCommitHeader(ctx context.Context, data []byte, key []byte) (uint64, model.Timestamp, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if len(data) < 4 {
		return 0, 0, storage.ErrCorruptStorage
	}
	plain := data
	if string(data[:4]) == encryptionMagic {
		c, err := newStorageCipher(key)
		if err != nil {
			return 0, 0, err
		}
		if c == nil {
			return 0, 0, ErrInvalidEncryptionKey
		}
		plain, err = decryptStreamToBytes(ctx, bytes.NewReader(data), c, math.MaxInt64)
		if err != nil {
			return 0, 0, err
		}
	}
	if len(plain) < 2*commitHeaderSize {
		return 0, 0, storage.ErrCorruptStorage
	}
	header := plain[:commitHeaderSize]
	headerHash := sha256.Sum256(header[:96])
	if string(header[:8]) != commitMagic || string(header[96:128]) != string(headerHash[:]) {
		return 0, 0, storage.ErrCorruptStorage
	}
	footer := plain[len(plain)-commitHeaderSize:]
	footerHash := sha256.Sum256(footer[:96])
	if string(footer[:8]) != commitEndMagic || string(footer[96:128]) != string(footerHash[:]) {
		return 0, 0, storage.ErrCorruptStorage
	}
	if !bytes.Equal(footer[16:96], header[16:96]) {
		return 0, 0, storage.ErrCorruptStorage
	}
	pos := binary.LittleEndian.Uint64(header[16:24])
	ts := model.Timestamp(binary.LittleEndian.Uint64(header[40:48]))
	return pos, ts, nil
}

// walArchiver handles atomic WAL segment archiving and optional hook callbacks.
type walArchiver struct {
	dir           string
	dbCipher      *storageCipher
	archiveCipher *storageCipher
	hook          func(context.Context, WALArchiveEntry) error
	limits        storage.BatchDecodeLimits
}

func newWALArchiver(dir string, dbCipher, archiveCipher *storageCipher, limits storage.BatchDecodeLimits, hook func(context.Context, WALArchiveEntry) error) (*walArchiver, error) {
	if dir == "" && hook == nil {
		return nil, nil
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	if archiveCipher == nil && dbCipher != nil {
		archiveCipher = dbCipher
	}
	return &walArchiver{
		dir:           dir,
		dbCipher:      dbCipher,
		archiveCipher: archiveCipher,
		hook:          hook,
		limits:        limits,
	}, nil
}

func walMetaName(position uint64) string { return fmt.Sprintf("%020d.meta", position) }

// archiveCommit atomically writes a copy of commits/<pos>.wal (plus <pos>.meta)
// into the archive directory and invokes WALArchiveHook if configured.
// Existing segments are not overwritten. Hooks must be idempotent: an existing
// local archive does not acknowledge an earlier failed hook invocation.
func (a *walArchiver) archiveCommit(ctx context.Context, commitsRoot *os.Root, position uint64, commitTS model.Timestamp) (err error) {
	if a == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	walFile := commitName(position)
	srcPath := filepath.Join(commitsRoot.Name(), walFile)
	var archivedPath string
	encrypted := a.archiveCipher != nil
	defer func() {
		if err == nil && a.hook != nil {
			err = a.hook(ctx, WALArchiveEntry{
				LogPosition:  position,
				CommitTS:     commitTS,
				SourcePath:   srcPath,
				ArchivedPath: archivedPath,
				Encrypted:    encrypted,
			})
		}
	}()
	if a.dir != "" {
		archiveRoot, err := os.OpenRoot(a.dir)
		if err != nil {
			return err
		}
		defer func() { _ = archiveRoot.Close() }()

		metaFile := walMetaName(position)
		archivedPath = filepath.Join(a.dir, walFile)
		if info, walErr := archiveRoot.Lstat(walFile); walErr == nil {
			if !info.Mode().IsRegular() {
				return storage.ErrCorruptStorage
			}
			expected, err := a.archivedCommitMetadata(ctx, commitsRoot, archiveRoot, position, commitTS)
			if err != nil {
				return err
			}
			encrypted = expected.Encrypted
			if err := reconcileArchivedMeta(archiveRoot, metaFile, expected); err != nil {
				return err
			}
			// A previous publication may have failed at the directory sync.
			return syncCommitDirectory(archiveRoot)
		} else if !errors.Is(walErr, os.ErrNotExist) {
			return walErr
		}
		if _, metaErr := archiveRoot.Lstat(metaFile); metaErr == nil {
			return storage.ErrCorruptStorage
		} else if !errors.Is(metaErr, os.ErrNotExist) {
			return metaErr
		}

		src, err := commitsRoot.Open(walFile)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()

		size, checksum, err := a.writeArchivedSegment(ctx, archiveRoot, walFile, src)
		if err != nil {
			return err
		}
		meta := WALArchiveMetadata{
			LogPosition:    position,
			CommitTS:       commitTS,
			Encrypted:      a.archiveCipher != nil,
			KeyFingerprint: a.archiveCipher.fingerprintHex(),
			Size:           size,
			SHA256:         hex.EncodeToString(checksum[:]),
		}
		if err := writeArchivedMeta(archiveRoot, metaFile, meta); err != nil {
			return err
		}
		if err := syncCommitDirectory(archiveRoot); err != nil {
			return err
		}
	}
	return nil
}

// archivedCommitMetadata verifies that an existing archive is the same durable
// decision as its source, then derives the sidecar from the actual stored bytes.
// Existence alone cannot permit retirement of the last valid source WAL.
func (a *walArchiver) archivedCommitMetadata(ctx context.Context, commitsRoot, archiveRoot *os.Root, position uint64, commitTS model.Timestamp) (meta WALArchiveMetadata, err error) {
	source, err := openCommitWithCipher(ctx, commitsRoot, position, a.limits, a.dbCipher)
	if err != nil {
		return meta, err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	archived, err := openCommitWithCipher(ctx, archiveRoot, position, a.limits, a.archiveCipher)
	if err != nil {
		return meta, errors.Join(storage.ErrCorruptStorage, err)
	}
	defer func() { err = errors.Join(err, archived.Close()) }()
	if source.Result.CommitTS != commitTS || source.Result != archived.Result ||
		source.Identity != archived.Identity || source.Batch.TxID != archived.Batch.TxID {
		return meta, storage.ErrCorruptStorage
	}
	file, err := archiveRoot.Open(commitName(position))
	if err != nil {
		return meta, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return meta, err
	}
	var magic [4]byte
	if _, err := file.ReadAt(magic[:], 0); err != nil {
		return meta, errors.Join(storage.ErrCorruptStorage, err)
	}
	digest := sha256.New()
	var buffer [64 << 10]byte
	for offset := int64(0); offset < info.Size(); {
		if err := ctx.Err(); err != nil {
			return meta, err
		}
		part := buffer[:min(int64(len(buffer)), info.Size()-offset)]
		if _, err := file.ReadAt(part, offset); err != nil {
			return meta, errors.Join(storage.ErrCorruptStorage, err)
		}
		_, _ = digest.Write(part)
		offset += int64(len(part))
	}
	meta = WALArchiveMetadata{
		LogPosition: position,
		CommitTS:    commitTS,
		Encrypted:   string(magic[:]) == encryptionMagic,
		Size:        uint64(info.Size()),
		SHA256:      hex.EncodeToString(digest.Sum(nil)),
	}
	if meta.Encrypted {
		meta.KeyFingerprint = a.archiveCipher.fingerprintHex()
	}
	return meta, nil
}

func reconcileArchivedMeta(root *os.Root, name string, expected WALArchiveMetadata) (err error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return writeArchivedMeta(root, name, expected)
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 64<<10 {
		return storage.ErrCorruptStorage
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	raw := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(file, raw); err != nil {
		return errors.Join(storage.ErrCorruptStorage, err)
	}
	var trailing [1]byte
	if n, err := file.Read(trailing[:]); n != 0 || !errors.Is(err, io.EOF) {
		return errors.Join(storage.ErrCorruptStorage, err)
	}
	var actual WALArchiveMetadata
	if err := json.Unmarshal(raw, &actual); err != nil {
		return errors.Join(storage.ErrCorruptStorage, err)
	}
	if actual != expected {
		return storage.ErrCorruptStorage
	}
	return nil
}

func (a *walArchiver) writeArchivedSegment(ctx context.Context, archiveRoot *os.Root, walFile string, src *os.File) (size uint64, sum [sha256.Size]byte, err error) {
	pending := "." + walFile + "." + rand.Text() + ".pending"
	dst, err := archiveRoot.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, sum, err
	}
	defer func() {
		if dst != nil {
			err = errors.Join(err, dst.Close())
		}
		if removeErr := archiveRoot.Remove(pending); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()

	hash := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(dst, hash)}
	sameCipher := (a.dbCipher == nil && a.archiveCipher == nil) ||
		(a.dbCipher != nil && a.archiveCipher != nil && a.dbCipher.matchesFingerprint(a.archiveCipher.fingerprint))

	if sameCipher {
		buf := make([]byte, 64<<10)
		if _, err := io.CopyBuffer(cw, src, buf); err != nil {
			return 0, sum, err
		}
	} else {
		var plain []byte
		if a.dbCipher != nil {
			plain, err = decryptStreamToBytes(ctx, src, a.dbCipher, math.MaxInt64)
			if err != nil {
				return 0, sum, err
			}
		} else {
			plain, err = io.ReadAll(src)
			if err != nil {
				return 0, sum, err
			}
		}
		if a.archiveCipher != nil {
			ew, err := newEncryptStreamWriter(cw, a.archiveCipher)
			if err != nil {
				return 0, sum, err
			}
			if _, err := ew.Write(plain); err != nil {
				return 0, sum, err
			}
			if err := ew.Close(); err != nil {
				return 0, sum, err
			}
		} else {
			if _, err := cw.Write(plain); err != nil {
				return 0, sum, err
			}
		}
	}

	if err := dst.Sync(); err != nil {
		return 0, sum, err
	}
	closeErr := dst.Close()
	dst = nil
	if closeErr != nil {
		return 0, sum, closeErr
	}
	if err := archiveRoot.Link(pending, walFile); err != nil {
		return 0, sum, err
	}
	hash.Sum(sum[:0])
	return cw.n, sum, nil
}

func writeArchivedMeta(archiveRoot *os.Root, metaFile string, meta WALArchiveMetadata) (err error) {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	pending := "." + metaFile + "." + rand.Text() + ".pending"
	f, err := archiveRoot.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		if removeErr := archiveRoot.Remove(pending); !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	closeErr := f.Close()
	f = nil
	if closeErr != nil {
		return closeErr
	}
	if err := archiveRoot.Link(pending, metaFile); err != nil {
		return err
	}
	return nil
}

// archiveBeforeRetire ensures every WAL segment in commits/ up to maxPos is
// archived before WAL retirement can unlink it.
func (a *walArchiver) archiveBeforeRetire(ctx context.Context, commitsRoot *os.Root, maxPos uint64, batchLimits storage.BatchDecodeLimits) error {
	if a == nil || maxPos == 0 {
		return nil
	}
	dir, err := commitsRoot.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			pos, pending, parseErr := commitFilePosition(entry.Name())
			if parseErr != nil || pending || pos > maxPos {
				continue
			}
			record, openErr := openCommitWithCipher(ctx, commitsRoot, pos, batchLimits, a.dbCipher)
			if openErr != nil {
				return openErr
			}
			commitTS := record.Result.CommitTS
			_ = record.Close()
			if err := a.archiveCommit(ctx, commitsRoot, pos, commitTS); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

type countingWriter struct {
	w io.Writer
	n uint64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += uint64(n)
	return n, err
}

// EnableWALArchive enables or updates WAL archiving on a live Backend without
// reopening it. Existing unretired WAL segments currently in commits/ are
// archived immediately. A failed update leaves the previous archiver active.
func (backend *Backend) EnableWALArchive(ctx context.Context, archiveDir string, archiveKey []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var archiveCipher *storageCipher
	if len(archiveKey) > 0 {
		var err error
		archiveCipher, err = newStorageCipher(archiveKey)
		if err != nil {
			return err
		}
	}
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ErrClosed
	}
	archiver, err := newWALArchiver(archiveDir, backend.cipher, archiveCipher, backend.limits.recovery().Batch, nil)
	if err != nil {
		backend.mu.Unlock()
		return err
	}
	commitsRoot := backend.directory.commits
	frontierPos := backend.frontier.LogPosition
	batchLimits := backend.limits.recovery().Batch
	backend.mu.Unlock()

	if archiver != nil && frontierPos > 0 {
		if err := archiver.archiveBeforeRetire(ctx, commitsRoot, frontierPos, batchLimits); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return storage.ErrClosed
	}
	backend.archiver = archiver
	return nil
}

// preparationLimits returns runPreparationLimits wired with the backend's cipher.
func (backend *Backend) preparationLimits() runPreparationLimits {
	p := backend.limits.preparation()
	p.File.cipher = backend.cipher
	return p
}

// recoveryLimits returns recoveryLimits wired with the backend's cipher.
func (backend *Backend) recoveryLimits() recoveryLimits {
	r := backend.limits.recovery()
	r.cipher = backend.cipher
	return r
}

// RecoverToTimestamp opens a NeoSepia database directory and replays WAL
// segments strictly up to commitTS <= targetTS, discarding any commits after
// targetTS. The persisted closed fence in database.state must have
// ClosedTS <= targetTS.
func RecoverToTimestamp(ctx context.Context, config Config, targetTS model.Timestamp) (*Backend, error) {
	if targetTS <= 0 {
		return nil, ErrInvalidConfig
	}
	return openBackend(ctx, config, targetTS)
}

// ParseWALArchiveFilename extracts the log position from an archived WAL
// filename (<020d>.wal).
func ParseWALArchiveFilename(name string) (uint64, bool) {
	if len(name) != 24 || !strings.HasSuffix(name, ".wal") {
		return 0, false
	}
	pos, err := strconv.ParseUint(name[:20], 10, 64)
	if err != nil || pos == 0 {
		return 0, false
	}
	return pos, true
}
