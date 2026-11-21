package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	pitrCommitHeaderSize = 128
	pitrCommitMagic      = "NSTXBEG2"
	pitrCommitEndMagic   = "NSTXCMT2"
	pitrEncMetaName      = "encryption.meta"
	pitrEncMetaSize      = 72
)

// PITRResult summarizes one completed Point-in-Time Recovery restore.
type PITRResult struct {
	BaseManifest      storage.CheckpointManifest
	BaseLogPosition   uint64
	BaseSnapshotTS    model.Timestamp
	TargetTS          model.Timestamp
	RecoveredLogPos   uint64
	RecoveredCommitTS model.Timestamp
	ReplayedSegments  uint64
}

type archivedWALMeta struct {
	LogPosition    uint64          `json:"log_position"`
	CommitTS       model.Timestamp `json:"commit_ts"`
	Encrypted      bool            `json:"encrypted"`
	KeyFingerprint string          `json:"key_fingerprint,omitempty"`
	Size           uint64          `json:"size"`
	SHA256         string          `json:"sha256"`
}

// RestorePITR restores baseBackupDir into options.Destination and stages every
// contiguous archived WAL segment from walArchiveDir with
// logPosition > baseLogPosition and commitTS <= targetTS into commits/,
// stopping cleanly at the first segment with commitTS > targetTS.
func RestorePITR(ctx context.Context, baseBackupDir string, walArchiveDir string, targetTS model.Timestamp, options RestoreOptions) (PITRResult, error) {
	if baseBackupDir == "" {
		return PITRResult{}, ErrInvalid
	}
	return RestorePITRFromChain(ctx, []string{baseBackupDir}, walArchiveDir, targetTS, options)
}

// RestorePITRFromChain restores a full or incremental backup chain into
// options.Destination and replays contiguous archived WAL segments from
// walArchiveDir strictly up to commitTS <= targetTS.
func RestorePITRFromChain(ctx context.Context, chainDirs []string, walArchiveDir string, targetTS model.Timestamp, options RestoreOptions) (result PITRResult, err error) {
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if nilDependency(options.Verifier) || len(chainDirs) == 0 || walArchiveDir == "" || targetTS <= 0 {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	backupCipher, err := newBackupCipher(options.EncryptionKey)
	if err != nil {
		return result, err
	}
	archiveKey := options.WALArchiveKey
	if len(archiveKey) == 0 {
		archiveKey = options.EncryptionKey
	}
	archiveCipher, err := newBackupCipher(archiveKey)
	if err != nil {
		return result, err
	}
	scratch := make([]byte, int(options.Limits.CopyBufferBytes))
	hops, err := openValidatedChain(ctx, chainDirs, options.Limits, backupCipher, scratch)
	if err != nil {
		return result, err
	}
	defer func() {
		for i := range hops {
			if hops[i].root != nil {
				err = errors.Join(err, hops[i].root.Close())
			}
		}
		if err != nil {
			result = PITRResult{}
		}
	}()
	baseManifest := hops[len(hops)-1].env.Manifest
	if targetTS < baseManifest.SnapshotTS {
		return result, fmt.Errorf("PITR target timestamp %d precedes base backup snapshot %d: %w", targetTS, baseManifest.SnapshotTS, ErrInvalid)
	}

	archiveRoot, err := openBackupRoot(walArchiveDir)
	if err != nil {
		return result, err
	}
	defer func() {
		if archiveRoot != nil {
			err = errors.Join(err, archiveRoot.Close())
		}
	}()

	stage, err := newStagedTree(ctx, options.Destination)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.close()) }()

	manifest, err := stageChainIntoRoot(ctx, stage.root, hops, backupCipher, scratch)
	if err != nil {
		return result, err
	}
	for i := range hops {
		closeErr := hops[i].root.Close()
		hops[i].root = nil
		if closeErr != nil {
			return result, closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := options.Verifier.VerifyCheckpoint(ctx, stage.root, manifest); err != nil {
		return result, fmt.Errorf("verify base checkpoint before PITR: %w", err)
	}

	// If the restored database itself uses live at-rest encryption, verify the
	// key against encryption.meta so archived WAL segments staged into commits/
	// are written in the database's live encryption format.
	liveKey := options.LiveEncryptionKey
	if len(liveKey) == 0 {
		liveKey = options.EncryptionKey
	}
	liveCipher, err := newBackupCipher(liveKey)
	if err != nil {
		return result, err
	}
	dbLiveCipher, err := inspectRestoredLiveEncryption(stage.root, liveCipher)
	if err != nil {
		return result, err
	}

	basePos := manifest.Fence.LogPosition
	positions, err := discoverArchivedWALPositions(ctx, archiveRoot, basePos)
	if err != nil {
		return result, err
	}
	slices.Sort(positions)

	commitsRoot, err := openChildRoot(stage.root, "commits")
	if err != nil {
		return result, err
	}
	defer func() {
		if commitsRoot != nil {
			err = errors.Join(err, commitsRoot.Close())
		}
	}()

	recoveredPos := basePos
	recoveredTS := manifest.SnapshotTS
	var replayed uint64
	expectedPos := basePos + 1

	for _, pos := range positions {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		meta, hasMeta, err := readWALMetaSidecar(archiveRoot, pos)
		if err != nil {
			return result, err
		}
		if hasMeta {
			if meta.LogPosition != pos {
				return result, fmt.Errorf("archived WAL %d metadata position mismatch: %w", pos, ErrCorrupt)
			}
			if meta.Encrypted {
				if archiveCipher == nil || !archiveCipher.matchesFingerprintHex(meta.KeyFingerprint) {
					return result, ErrInvalidEncryptionKey
				}
			}
			if meta.CommitTS > targetTS {
				// Cleanly stop before the first commit past targetTS.
				break
			}
		}
		if pos != expectedPos {
			return result, fmt.Errorf("missing archived WAL segment at position %d (found %d): %w", expectedPos, pos, ErrCorrupt)
		}

		walName := pitrCommitName(pos)
		plainWAL, err := readArchivedWALSegment(ctx, archiveRoot, walName, meta, hasMeta, archiveCipher, options.Limits)
		if err != nil {
			return result, err
		}
		walPos, walCommitTS, err := validatePlainWALSegment(plainWAL)
		if err != nil {
			return result, fmt.Errorf("invalid archived WAL %q: %w", walName, err)
		}
		if walPos != pos {
			return result, fmt.Errorf("archived WAL %q position %d != %d: %w", walName, walPos, pos, ErrCorrupt)
		}
		if hasMeta && walCommitTS != meta.CommitTS {
			return result, fmt.Errorf("archived WAL %q commitTS mismatch: %w", walName, ErrCorrupt)
		}
		if walCommitTS <= recoveredTS {
			return result, fmt.Errorf("archived WAL %q non-monotonic commitTS %d <= %d: %w", walName, walCommitTS, recoveredTS, ErrCorrupt)
		}
		if walCommitTS > targetTS {
			break
		}

		if err := writeStagedWALSegment(ctx, commitsRoot, walName, plainWAL, dbLiveCipher); err != nil {
			return result, err
		}
		recoveredPos = pos
		recoveredTS = walCommitTS
		replayed++
		expectedPos++
	}

	closeCommitsErr := commitsRoot.Close()
	commitsRoot = nil
	if closeCommitsErr != nil {
		return result, closeCommitsErr
	}
	closeArchiveErr := archiveRoot.Close()
	archiveRoot = nil
	if closeArchiveErr != nil {
		return result, closeArchiveErr
	}
	if err := stage.publish(ctx); err != nil {
		return result, err
	}
	return PITRResult{
		BaseManifest:      manifest,
		BaseLogPosition:   basePos,
		BaseSnapshotTS:    manifest.SnapshotTS,
		TargetTS:          targetTS,
		RecoveredLogPos:   recoveredPos,
		RecoveredCommitTS: recoveredTS,
		ReplayedSegments:  replayed,
	}, nil
}

func pitrCommitName(pos uint64) string  { return fmt.Sprintf("%020d.wal", pos) }
func pitrWALMetaName(pos uint64) string { return fmt.Sprintf("%020d.meta", pos) }

func discoverArchivedWALPositions(ctx context.Context, archiveRoot *os.Root, after uint64) (positions []uint64, err error) {
	dir, err := archiveRoot.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, readErr := dir.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			if len(name) != 24 || !strings.HasSuffix(name, ".wal") || strings.HasPrefix(name, ".") {
				continue
			}
			pos, parseErr := strconv.ParseUint(name[:20], 10, 64)
			if parseErr != nil || pos == 0 {
				continue
			}
			if pos > after {
				positions = append(positions, pos)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return positions, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func readWALMetaSidecar(archiveRoot *os.Root, pos uint64) (meta archivedWALMeta, exists bool, err error) {
	f, err := openRegular(archiveRoot, pitrWALMetaName(pos))
	if errors.Is(err, os.ErrNotExist) {
		return meta, false, nil
	}
	if err != nil {
		return meta, false, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return meta, true, err
	}
	if info.Size() <= 0 || info.Size() > 64<<10 {
		return meta, true, ErrCorrupt
	}
	raw := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(f, raw); err != nil {
		return meta, true, errors.Join(ErrCorrupt, err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, true, errors.Join(ErrCorrupt, err)
	}
	return meta, true, nil
}

func readArchivedWALSegment(ctx context.Context, archiveRoot *os.Root, walName string, meta archivedWALMeta, hasMeta bool, archiveCipher *backupCipher, limits Limits) (_ []byte, err error) {
	f, err := openRegular(archiveRoot, walName)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 4 || uint64(info.Size()) > limits.MaxFileBytes {
		return nil, ErrCorrupt
	}
	if hasMeta && uint64(info.Size()) != meta.Size {
		return nil, ErrCorrupt
	}
	raw := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if hasMeta && meta.SHA256 != "" {
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != meta.SHA256 {
			return nil, ErrCorrupt
		}
	}
	if string(raw[:4]) == encryptionMagic {
		if archiveCipher == nil {
			return nil, ErrInvalidEncryptionKey
		}
		var plain bytes.Buffer
		limit := limits.MaxFileBytes
		if _, _, err := decryptStreamWithCipher(ctx, &plain, bytes.NewReader(raw), archiveCipher, &limit); err != nil {
			return nil, err
		}
		return plain.Bytes(), nil
	}
	if hasMeta && meta.Encrypted {
		return nil, ErrCorrupt
	}
	return raw, nil
}

func validatePlainWALSegment(plain []byte) (uint64, model.Timestamp, error) {
	if len(plain) < 2*pitrCommitHeaderSize {
		return 0, 0, ErrCorrupt
	}
	header := plain[:pitrCommitHeaderSize]
	headerHash := sha256.Sum256(header[:96])
	if string(header[:8]) != pitrCommitMagic || !bytes.Equal(header[96:128], headerHash[:]) {
		return 0, 0, ErrCorrupt
	}
	if binary.LittleEndian.Uint64(header[8:16]) != pitrCommitHeaderSize {
		return 0, 0, ErrCorrupt
	}
	footer := plain[len(plain)-pitrCommitHeaderSize:]
	footerHash := sha256.Sum256(footer[:96])
	if string(footer[:8]) != pitrCommitEndMagic || !bytes.Equal(footer[96:128], footerHash[:]) {
		return 0, 0, ErrCorrupt
	}
	if !bytes.Equal(footer[16:96], header[16:96]) {
		return 0, 0, ErrCorrupt
	}
	bodyLen := binary.LittleEndian.Uint64(footer[8:16])
	if bodyLen != uint64(len(plain)-2*pitrCommitHeaderSize) {
		return 0, 0, ErrCorrupt
	}
	bodySum := sha256.Sum256(plain[pitrCommitHeaderSize : len(plain)-pitrCommitHeaderSize])
	if !bytes.Equal(bodySum[:], header[64:96]) {
		return 0, 0, ErrCorrupt
	}
	pos := binary.LittleEndian.Uint64(header[16:24])
	commitTS := model.Timestamp(binary.LittleEndian.Uint64(header[40:48]))
	if pos == 0 || commitTS <= 0 {
		return 0, 0, ErrCorrupt
	}
	return pos, commitTS, nil
}

func writeStagedWALSegment(ctx context.Context, commitsRoot *os.Root, walName string, plainWAL []byte, dbLiveCipher *backupCipher) (err error) {
	dst, err := commitsRoot.OpenFile(walName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dst.Close()) }()
	if dbLiveCipher != nil {
		if _, _, _, err := encryptStreamWithCipher(ctx, dst, bytes.NewReader(plainWAL), dbLiveCipher, nil); err != nil {
			return err
		}
	} else {
		n, writeErr := dst.Write(plainWAL)
		if n != len(plainWAL) {
			writeErr = errors.Join(writeErr, io.ErrShortWrite)
		}
		if writeErr != nil {
			return writeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return dst.Sync()
}

func inspectRestoredLiveEncryption(stageRoot *os.Root, c *backupCipher) (_ *backupCipher, err error) {
	f, err := openRegular(stageRoot, pitrEncMetaName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != pitrEncMetaSize {
		return nil, ErrCorrupt
	}
	var payload [pitrEncMetaSize]byte
	if _, err := io.ReadFull(f, payload[:]); err != nil {
		return nil, errors.Join(ErrCorrupt, err)
	}
	if string(payload[0:4]) != encryptionMagic {
		return nil, ErrCorrupt
	}
	expected := sha256.Sum256(payload[:40])
	if !bytes.Equal(expected[:], payload[40:72]) {
		return nil, ErrCorrupt
	}
	if c == nil || subtle.ConstantTimeCompare(c.fingerprint[:], payload[8:40]) != 1 {
		return nil, ErrInvalidEncryptionKey
	}
	return c, nil
}

// CleanPath helper retained for callers building archive paths cleanly.
var _ = filepath.Clean
