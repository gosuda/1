package maintenance

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivedWALBodyLimitIncludesFraming(t *testing.T) {
	const bodyBytes = 257
	plain := bytes.Repeat([]byte{1}, bodyBytes+2*pitrCommitHeaderSize)
	copy(plain, pitrCommitMagic)
	key := bytes.Repeat([]byte{2}, encryptionKeySize)
	cipher, err := newBackupCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		raw := plain
		if encrypted {
			name = "encrypted"
			raw, err = EncryptBytes(key, plain)
			if err != nil {
				t.Fatal(err)
			}
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, pitrCommitName(1)), raw, 0600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := root.Close(); err != nil {
					t.Error(err)
				}
			})
			limits := Limits{MaxFileBytes: 1 << 20}
			got, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes)
			if err != nil || !bytes.Equal(got, plain) {
				t.Fatalf("exact WAL body limit returned %d bytes, %v", len(got), err)
			}
			if _, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes-1); err == nil {
				t.Fatal("accepted WAL body larger than its configured limit")
			}
		})
	}
}

func TestArchivedWALEncryptedOverLimitReportsLimit(t *testing.T) {
	const bodyBytes = 257
	plain := bytes.Repeat([]byte{1}, bodyBytes+2*pitrCommitHeaderSize)
	copy(plain, pitrCommitMagic)
	key := bytes.Repeat([]byte{2}, encryptionKeySize)
	cipher, err := newBackupCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := EncryptBytes(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, pitrCommitName(1)), enc, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := Limits{MaxFileBytes: 1 << 20}
	if _, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes-1); !errors.Is(err, ErrLimit) {
		t.Fatalf("encrypted over-limit error = %v, want ErrLimit", err)
	}
	bad := bytes.Clone(enc)
	bad[len(bad)-1] ^= 1
	if err := os.WriteFile(filepath.Join(directory, pitrCommitName(1)), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes); !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrLimit) {
		t.Fatalf("tampered archive error = %v, want ErrCorrupt without ErrLimit", err)
	}
}

func TestArchivedWALEncryptedMultiChunkLimitSubtractsEarlierChunks(t *testing.T) {
	const bodyBytes = 2*encryptionDefaultChunk + 100
	plain := bytes.Repeat([]byte{1}, bodyBytes+2*pitrCommitHeaderSize)
	copy(plain, pitrCommitMagic)
	key := bytes.Repeat([]byte{2}, encryptionKeySize)
	cipher, err := newBackupCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := EncryptBytes(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, pitrCommitName(1)), enc, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := Limits{MaxFileBytes: 1 << 20}
	got, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("exact multi-chunk WAL body returned %d bytes, %v", len(got), err)
	}
	if _, err := readArchivedWALSegment(t.Context(), root, pitrCommitName(1), archivedWALMeta{}, false, cipher, limits, bodyBytes-1); !errors.Is(err, ErrLimit) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("multi-chunk over-limit error = %v, want ErrLimit without ErrCorrupt", err)
	}
}
