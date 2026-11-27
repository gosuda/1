package maintenance

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// ErrInvalidEncryptionKey reports a missing, malformed, or mismatched AES-256
// backup encryption key.
var ErrInvalidEncryptionKey = errors.New("invalid or missing backup encryption key")

const (
	encryptionMagic           = "NSE1"
	encryptionKeySize         = 32
	encryptionSaltSize        = 32
	encryptionNonceSize       = 12
	encryptionTagSize         = 16
	encryptionHeaderSizeV1    = 4 + 4 + encryptionNonceSize
	encryptionHeaderSizeV2    = 4 + 4 + encryptionSaltSize
	encryptionHeaderSize      = encryptionHeaderSizeV2
	encryptionHeaderV2Bit     = uint32(1 << 31)
	encryptionChunkHeaderSize = 4
	encryptionChunkOverhead   = encryptionChunkHeaderSize + encryptionTagSize
	encryptionDefaultChunk    = 64 << 10
	encryptionMaxChunk        = 4 << 20
	encryptionFinalBit        = uint32(1 << 31)
	encryptionKeyDomain       = "supersql-enc-key-v1"
	encryptionDEKInfo         = "supersql-nse2-dek-v1"
)

type backupCipher struct {
	masterKey   [encryptionKeySize]byte
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

func newBackupCipher(key []byte) (*backupCipher, error) {
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
	var masterKey [encryptionKeySize]byte
	copy(masterKey[:], key)
	return &backupCipher{masterKey: masterKey, aead: aead, fingerprint: fp}, nil
}

func (c *backupCipher) deriveFileAEAD(salt [encryptionSaltSize]byte) (cipher.AEAD, error) {
	if c == nil {
		return nil, ErrInvalidEncryptionKey
	}
	dek, err := hkdf.Key(sha256.New, c.masterKey[:], salt[:], encryptionDEKInfo, encryptionKeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEncryptionKey, err)
	}
	defer clear(dek)
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEncryptionKey, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidEncryptionKey, err)
	}
	return aead, nil
}

func (c *backupCipher) fingerprintHex() string {
	if c == nil {
		return ""
	}
	return hex.EncodeToString(c.fingerprint[:])
}

func (c *backupCipher) matchesFingerprintHex(fpHex string) bool {
	if c == nil {
		return false
	}
	decoded, err := hex.DecodeString(fpHex)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(c.fingerprint[:], decoded) == 1
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

func deriveChunkNonceV2(chunkIndex uint64) [encryptionNonceSize]byte {
	var nonce [encryptionNonceSize]byte
	binary.BigEndian.PutUint64(nonce[4:12], chunkIndex)
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

func buildHeaderV2(chunkSize uint32, salt [encryptionSaltSize]byte) [encryptionHeaderSizeV2]byte {
	var hdr [encryptionHeaderSizeV2]byte
	copy(hdr[0:4], encryptionMagic)
	binary.BigEndian.PutUint32(hdr[4:8], encryptionHeaderV2Bit|chunkSize)
	copy(hdr[8:encryptionHeaderSizeV2], salt[:])
	return hdr
}

func buildChunkAADV2(header [encryptionHeaderSizeV2]byte, chunkIndex uint64, plainLen uint32, isFinal bool) [encryptionHeaderSizeV2 + 13]byte {
	var aad [encryptionHeaderSizeV2 + 13]byte
	copy(aad[0:encryptionHeaderSizeV2], header[:])
	binary.BigEndian.PutUint64(aad[encryptionHeaderSizeV2:encryptionHeaderSizeV2+8], chunkIndex)
	binary.BigEndian.PutUint32(aad[encryptionHeaderSizeV2+8:encryptionHeaderSizeV2+12], plainLen)
	if isFinal {
		aad[encryptionHeaderSizeV2+12] = 1
	}
	return aad
}

// EncryptBytes encrypts plaintext into the chunked NSE1 v2 AES-256-GCM format.
func EncryptBytes(key, plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	_, _, _, err := EncryptStream(context.Background(), &buf, bytes.NewReader(plaintext), key)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecryptBytes decrypts and authenticates an NSE1 (v1 or v2) AES-256-GCM payload.
func DecryptBytes(key, ciphertext []byte) ([]byte, error) {
	var buf bytes.Buffer
	_, _, err := DecryptStream(context.Background(), &buf, bytes.NewReader(ciphertext), key)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncryptStream encrypts src to dst in chunked NSE1 v2 AES-256-GCM framing
// with a per-file HKDF-SHA256 DEK and deterministic chunk counter nonces using
// bounded O(64 KiB) memory, returning the plaintext byte count, plaintext
// SHA-256 digest, and written ciphertext byte count.
func EncryptStream(ctx context.Context, dst io.Writer, src io.Reader, key []byte) (plainSize uint64, plainSHA [sha256.Size]byte, encSize uint64, err error) {
	c, err := newBackupCipher(key)
	if err != nil {
		return 0, plainSHA, 0, err
	}
	if c == nil {
		return 0, plainSHA, 0, ErrInvalidEncryptionKey
	}
	return encryptStreamWithCipher(ctx, dst, src, c, nil)
}

// DecryptStream decrypts an NSE1 (v1 or v2) AES-256-GCM stream from src into
// dst using bounded O(64 KiB) memory, verifying every chunk tag, final-chunk
// bit, and trailing EOF.
func DecryptStream(ctx context.Context, dst io.Writer, src io.Reader, key []byte) (plainSize uint64, plainSHA [sha256.Size]byte, err error) {
	c, err := newBackupCipher(key)
	if err != nil {
		return 0, plainSHA, err
	}
	if c == nil {
		return 0, plainSHA, ErrInvalidEncryptionKey
	}
	return decryptStreamWithCipher(ctx, dst, src, c, nil)
}

func encryptStreamWithCipher(ctx context.Context, dst io.Writer, src io.Reader, c *backupCipher, maxPlain *uint64) (plainSize uint64, plainSHA [sha256.Size]byte, encSize uint64, err error) {
	if c == nil {
		return 0, plainSHA, 0, ErrInvalidEncryptionKey
	}
	var salt [encryptionSaltSize]byte
	if _, err := io.ReadFull(rand.Reader, salt[:]); err != nil {
		return 0, plainSHA, 0, err
	}
	fileAEAD, err := c.deriveFileAEAD(salt)
	if err != nil {
		return 0, plainSHA, 0, err
	}
	hdr := buildHeaderV2(encryptionDefaultChunk, salt)
	n, err := dst.Write(hdr[:])
	if n != len(hdr) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	if err != nil {
		return 0, plainSHA, 0, err
	}
	encSize = uint64(len(hdr))

	digest := sha256.New()
	cur := make([]byte, encryptionDefaultChunk)
	next := make([]byte, encryptionDefaultChunk)
	sealed := make([]byte, 0, encryptionChunkHeaderSize+encryptionDefaultChunk+encryptionTagSize)

	readFullChunk := func(buf []byte) (int, bool, error) {
		total := 0
		for total < len(buf) {
			if err := ctx.Err(); err != nil {
				return total, false, err
			}
			rn, rerr := src.Read(buf[total:])
			total += rn
			if rerr == io.EOF {
				return total, true, nil
			}
			if rerr != nil {
				return total, false, rerr
			}
			if rn == 0 {
				return total, false, io.ErrNoProgress
			}
		}
		return total, false, nil
	}

	writeChunk := func(chunkIdx uint64, plain []byte, isFinal bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		plainLen := uint32(len(plain))
		flags := plainLen
		if isFinal {
			flags |= encryptionFinalBit
		}
		sealed = sealed[:encryptionChunkHeaderSize]
		binary.BigEndian.PutUint32(sealed[:encryptionChunkHeaderSize], flags)
		nonce := deriveChunkNonceV2(chunkIdx)
		aad := buildChunkAADV2(hdr, chunkIdx, plainLen, isFinal)
		sealed = fileAEAD.Seal(sealed, nonce[:], plain, aad[:])
		wn, werr := dst.Write(sealed)
		if wn != len(sealed) {
			werr = errors.Join(werr, io.ErrShortWrite)
		}
		if werr != nil {
			return werr
		}
		encSize += uint64(wn)
		return nil
	}

	curLen, hitEOF, err := readFullChunk(cur)
	if err != nil {
		return 0, plainSHA, encSize, err
	}
	var chunkIdx uint64
	for {
		if maxPlain != nil && uint64(curLen) > *maxPlain-plainSize {
			return 0, plainSHA, encSize, ErrCorrupt
		}
		_, _ = digest.Write(cur[:curLen])
		plainSize += uint64(curLen)

		if hitEOF || curLen < encryptionDefaultChunk {
			if err := writeChunk(chunkIdx, cur[:curLen], true); err != nil {
				return 0, plainSHA, encSize, err
			}
			break
		}
		nextLen, nextEOF, err := readFullChunk(next)
		if err != nil {
			return 0, plainSHA, encSize, err
		}
		if nextLen == 0 && nextEOF {
			if err := writeChunk(chunkIdx, cur[:curLen], true); err != nil {
				return 0, plainSHA, encSize, err
			}
			break
		}
		if err := writeChunk(chunkIdx, cur[:curLen], false); err != nil {
			return 0, plainSHA, encSize, err
		}
		chunkIdx++
		cur, next = next, cur
		curLen, hitEOF = nextLen, nextEOF
	}
	digest.Sum(plainSHA[:0])
	return plainSize, plainSHA, encSize, ctx.Err()
}

func decryptStreamWithCipher(ctx context.Context, dst io.Writer, src io.Reader, c *backupCipher, maxPlain *uint64) (plainSize uint64, plainSHA [sha256.Size]byte, err error) {
	if c == nil {
		return 0, plainSHA, ErrInvalidEncryptionKey
	}
	var prefix [8]byte
	if _, err := io.ReadFull(src, prefix[:]); err != nil {
		return 0, plainSHA, errors.Join(ErrCorrupt, err)
	}
	if string(prefix[0:4]) != encryptionMagic {
		return 0, plainSHA, ErrCorrupt
	}
	hdrFlags := binary.BigEndian.Uint32(prefix[4:8])
	isV2 := (hdrFlags & encryptionHeaderV2Bit) != 0
	chunkSize := uint64(hdrFlags &^ encryptionHeaderV2Bit)
	if chunkSize == 0 || chunkSize > encryptionMaxChunk {
		return 0, plainSHA, ErrCorrupt
	}

	var fileAEAD cipher.AEAD
	var baseNonce [encryptionNonceSize]byte
	var hdrV2 [encryptionHeaderSizeV2]byte
	if isV2 {
		var salt [encryptionSaltSize]byte
		if _, err := io.ReadFull(src, salt[:]); err != nil {
			return 0, plainSHA, errors.Join(ErrCorrupt, err)
		}
		copy(hdrV2[0:8], prefix[:])
		copy(hdrV2[8:encryptionHeaderSizeV2], salt[:])
		fileAEAD, err = c.deriveFileAEAD(salt)
		if err != nil {
			return 0, plainSHA, err
		}
	} else {
		if _, err := io.ReadFull(src, baseNonce[:]); err != nil {
			return 0, plainSHA, errors.Join(ErrCorrupt, err)
		}
		fileAEAD = c.aead
	}

	digest := sha256.New()
	var encBuf []byte
	var plainBuf []byte
	var chunkIdx uint64
	for {
		if err := ctx.Err(); err != nil {
			return 0, plainSHA, err
		}
		var ch [encryptionChunkHeaderSize]byte
		if _, err := io.ReadFull(src, ch[:]); err != nil {
			return 0, plainSHA, errors.Join(ErrCorrupt, err)
		}
		flags := binary.BigEndian.Uint32(ch[:])
		isFinal := (flags & encryptionFinalBit) != 0
		plainLen := uint64(flags &^ encryptionFinalBit)
		if plainLen > chunkSize || (!isFinal && plainLen != chunkSize) {
			return 0, plainSHA, ErrCorrupt
		}
		if maxPlain != nil && (plainSize > *maxPlain || plainLen > *maxPlain-plainSize) {
			return 0, plainSHA, ErrCorrupt
		}
		ctLen := int(plainLen) + encryptionTagSize
		if cap(encBuf) < ctLen {
			encBuf = make([]byte, ctLen)
		} else {
			encBuf = encBuf[:ctLen]
		}
		if _, err := io.ReadFull(src, encBuf); err != nil {
			return 0, plainSHA, errors.Join(ErrCorrupt, err)
		}
		var decErr error
		if isV2 {
			nonce := deriveChunkNonceV2(chunkIdx)
			aad := buildChunkAADV2(hdrV2, chunkIdx, uint32(plainLen), isFinal)
			plainBuf, decErr = fileAEAD.Open(plainBuf[:0], nonce[:], encBuf, aad[:])
		} else {
			nonce := deriveChunkNonce(baseNonce, chunkIdx)
			aad := buildChunkAAD(chunkIdx, uint32(plainLen), isFinal)
			plainBuf, decErr = fileAEAD.Open(plainBuf[:0], nonce[:], encBuf, aad[:])
		}
		if decErr != nil {
			return 0, plainSHA, errors.Join(ErrCorrupt, ErrInvalidEncryptionKey, decErr)
		}
		if uint64(len(plainBuf)) != plainLen {
			return 0, plainSHA, ErrCorrupt
		}
		if len(plainBuf) > 0 {
			wn, werr := dst.Write(plainBuf)
			if wn != len(plainBuf) {
				werr = errors.Join(werr, io.ErrShortWrite)
			}
			if werr != nil {
				return 0, plainSHA, werr
			}
			_, _ = digest.Write(plainBuf)
			plainSize += uint64(wn)
		}
		chunkIdx++
		if isFinal {
			break
		}
	}
	if err := requireEOF(ctx, src); err != nil {
		return 0, plainSHA, err
	}
	digest.Sum(plainSHA[:0])
	return plainSize, plainSHA, ctx.Err()
}

func copyFileEncrypted(ctx context.Context, root *os.Root, expected storage.CheckpointFile, source io.ReadCloser, c *backupCipher) (encSize uint64, err error) {
	defer func() { err = errors.Join(err, source.Close()) }()
	destination, err := root.OpenFile(expected.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	limit := expected.Size
	plainSize, plainSHA, encSize, err := encryptStreamWithCipher(ctx, destination, source, c, &limit)
	if err != nil {
		return 0, fmt.Errorf("encrypt %q: %w", expected.Name, err)
	}
	if plainSize != expected.Size || plainSHA != expected.SHA256 {
		return 0, fmt.Errorf("encrypt %q: %w", expected.Name, ErrCorrupt)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := destination.Sync(); err != nil {
		return 0, err
	}
	return encSize, nil
}

func copyFileDecrypted(ctx context.Context, root *os.Root, expected storage.CheckpointFile, source io.ReadCloser, c *backupCipher) (err error) {
	defer func() { err = errors.Join(err, source.Close()) }()
	destination, err := root.OpenFile(expected.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, destination.Close()) }()
	limit := expected.Size
	plainSize, plainSHA, err := decryptStreamWithCipher(ctx, destination, source, c, &limit)
	if err != nil {
		return fmt.Errorf("decrypt %q: %w", expected.Name, err)
	}
	if plainSize != expected.Size || plainSHA != expected.SHA256 {
		return fmt.Errorf("decrypt %q: %w", expected.Name, ErrCorrupt)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return destination.Sync()
}

func openBackupFileWithSize(root *os.Root, fileName string, expectedDiskSize uint64) (file *os.File, err error) {
	var child *os.Root
	defer func() {
		if child != nil {
			err = errors.Join(err, child.Close())
		}
		if err != nil && file != nil {
			err = errors.Join(err, file.Close())
			file = nil
		}
	}()
	directory, name, nested := strings.Cut(fileName, "/")
	if nested {
		child, err = openChildRoot(root, directory)
		if err != nil {
			return nil, err
		}
		root = child
	} else {
		name = fileName
	}
	file, err = openRegular(root, name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return file, err
	}
	if info.Size() < 0 || uint64(info.Size()) != expectedDiskSize {
		return file, ErrCorrupt
	}
	return file, nil
}
