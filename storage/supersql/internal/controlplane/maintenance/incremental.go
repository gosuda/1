package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	backupKindFull        = "full"
	backupKindIncremental = "incremental"
	jsonEnvelopeVersion   = 2
)

// IncrementalManifest summarizes an atomic incremental backup published by
// CreateIncrementalBackup.
type IncrementalManifest struct {
	BackupID       string
	ParentBackupID string
	BaseSnapshotTS model.Timestamp
	SnapshotTS     model.Timestamp
	Encrypted      bool
	KeyFingerprint string
	Manifest       storage.CheckpointManifest
	ChangedFiles   []storage.CheckpointFile
	InheritedFiles []storage.CheckpointFile
}

type backupEnvelopeSpec struct {
	Kind           string
	ParentBackupID string
	BaseSnapshotTS model.Timestamp
	Encrypted      bool
	KeyFingerprint string
	Manifest       storage.CheckpointManifest
	ChangedFiles   []storage.CheckpointFile
	InheritedFiles []storage.CheckpointFile
	EncryptedSizes map[string]uint64
}

type loadedEnvelope struct {
	BackupID       string
	ParentBackupID string
	Kind           string
	BaseSnapshotTS model.Timestamp
	Encrypted      bool
	KeyFingerprint string
	Manifest       storage.CheckpointManifest
	ChangedFiles   []storage.CheckpointFile
	InheritedFiles []storage.CheckpointFile
	EncryptedSizes map[string]uint64
}

type jsonCheckpointFile struct {
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"`
}

type jsonCheckpointManifest struct {
	FormatVersion        uint32               `json:"format_version"`
	Backend              string               `json:"backend"`
	SnapshotTS           model.Timestamp      `json:"snapshot_ts"`
	HistoryFloor         model.Timestamp      `json:"history_floor"`
	FenceID              string               `json:"fence_id"`
	FenceClosedTS        model.Timestamp      `json:"fence_closed_ts"`
	FenceLogPosition     uint64               `json:"fence_log_position"`
	FenceAssurance       string               `json:"fence_assurance"`
	FenceLeapHistory     string               `json:"fence_leap_history"`
	FenceConfig          string               `json:"fence_config"`
	ScaleName            string               `json:"scale_name"`
	ScaleLeapHistoryID   string               `json:"scale_leap_history_id"`
	ScaleCanonicalBinary string               `json:"scale_canonical_leap_history"`
	Files                []jsonCheckpointFile `json:"files"`
}

type jsonEnvelopeBody struct {
	FormatVersion  uint32                 `json:"format_version"`
	Kind           string                 `json:"kind"`
	ParentBackupID string                 `json:"parent_backup_id,omitempty"`
	BaseSnapshotTS model.Timestamp        `json:"base_snapshot_ts,omitempty"`
	SnapshotTS     model.Timestamp        `json:"snapshot_ts"`
	Encrypted      bool                   `json:"encrypted"`
	KeyFingerprint string                 `json:"key_fingerprint,omitempty"`
	Manifest       jsonCheckpointManifest `json:"manifest"`
	ChangedFiles   []jsonCheckpointFile   `json:"changed_files,omitempty"`
	InheritedFiles []jsonCheckpointFile   `json:"inherited_files,omitempty"`
	EncryptedSizes map[string]uint64      `json:"encrypted_sizes,omitempty"`
}

type jsonEnvelopeFile struct {
	FormatVersion  uint32                 `json:"format_version"`
	Kind           string                 `json:"kind"`
	BackupID       string                 `json:"backup_id"`
	ParentBackupID string                 `json:"parent_backup_id,omitempty"`
	BaseSnapshotTS model.Timestamp        `json:"base_snapshot_ts,omitempty"`
	SnapshotTS     model.Timestamp        `json:"snapshot_ts"`
	Encrypted      bool                   `json:"encrypted"`
	KeyFingerprint string                 `json:"key_fingerprint,omitempty"`
	Manifest       jsonCheckpointManifest `json:"manifest"`
	ChangedFiles   []jsonCheckpointFile   `json:"changed_files,omitempty"`
	InheritedFiles []jsonCheckpointFile   `json:"inherited_files,omitempty"`
	EncryptedSizes map[string]uint64      `json:"encrypted_sizes,omitempty"`
	EnvelopeSHA256 string                 `json:"envelope_sha256"`
}

func toJSONFiles(files []storage.CheckpointFile) []jsonCheckpointFile {
	out := make([]jsonCheckpointFile, len(files))
	for i, f := range files {
		out[i] = jsonCheckpointFile{
			Name:   f.Name,
			Size:   f.Size,
			SHA256: hex.EncodeToString(f.SHA256[:]),
		}
	}
	return out
}

func fromJSONFiles(files []jsonCheckpointFile) ([]storage.CheckpointFile, error) {
	out := make([]storage.CheckpointFile, len(files))
	for i, f := range files {
		sum, err := decodeHex32(f.SHA256)
		if err != nil {
			return nil, ErrCorrupt
		}
		out[i] = storage.CheckpointFile{Name: f.Name, Size: f.Size, SHA256: sum}
	}
	return out, nil
}

func decodeHex16(s string) ([16]byte, error) {
	var out [16]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 16 {
		return out, ErrCorrupt
	}
	copy(out[:], raw)
	return out, nil
}

func decodeHex32(s string) ([32]byte, error) {
	var out [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return out, ErrCorrupt
	}
	copy(out[:], raw)
	return out, nil
}

func toJSONManifest(m storage.CheckpointManifest) jsonCheckpointManifest {
	return jsonCheckpointManifest{
		FormatVersion:        m.FormatVersion,
		Backend:              m.Backend,
		SnapshotTS:           m.SnapshotTS,
		HistoryFloor:         m.HistoryFloor,
		FenceID:              hex.EncodeToString(m.Fence.ID[:]),
		FenceClosedTS:        m.Fence.ClosedTS,
		FenceLogPosition:     m.Fence.LogPosition,
		FenceAssurance:       hex.EncodeToString(m.Fence.Epoch.Assurance[:]),
		FenceLeapHistory:     hex.EncodeToString(m.Fence.Epoch.LeapHistory[:]),
		FenceConfig:          hex.EncodeToString(m.Fence.Epoch.Config[:]),
		ScaleName:            m.Scale.Name,
		ScaleLeapHistoryID:   hex.EncodeToString(m.Scale.LeapHistoryID[:]),
		ScaleCanonicalBinary: hex.EncodeToString(m.Scale.CanonicalLeapHistory),
		Files:                toJSONFiles(m.Files),
	}
}

func fromJSONManifest(jm jsonCheckpointManifest) (storage.CheckpointManifest, error) {
	fenceID, err := decodeHex16(jm.FenceID)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	assurance, err := decodeHex16(jm.FenceAssurance)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	leapHist, err := decodeHex32(jm.FenceLeapHistory)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	cfg, err := decodeHex32(jm.FenceConfig)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	scaleID, err := decodeHex32(jm.ScaleLeapHistoryID)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	canonical, err := hex.DecodeString(jm.ScaleCanonicalBinary)
	if err != nil {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	files, err := fromJSONFiles(jm.Files)
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	return storage.CheckpointManifest{
		FormatVersion: jm.FormatVersion,
		Backend:       jm.Backend,
		SnapshotTS:    jm.SnapshotTS,
		HistoryFloor:  jm.HistoryFloor,
		Fence: storage.ReadFence{
			ID:          fenceID,
			ClosedTS:    jm.FenceClosedTS,
			LogPosition: jm.FenceLogPosition,
			Epoch: clock.Epoch{
				Assurance:   assurance,
				LeapHistory: leapHist,
				Config:      cfg,
			},
		},
		Scale: clock.ScaleDescriptor{
			Name:                 jm.ScaleName,
			LeapHistoryID:        scaleID,
			CanonicalLeapHistory: canonical,
		},
		Files: files,
	}, nil
}

func computeJSONEnvelopeDigest(body jsonEnvelopeBody) (string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func writeJSONEnvelope(ctx context.Context, root *os.Root, spec backupEnvelopeSpec, limits Limits) (backupID string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	body := jsonEnvelopeBody{
		FormatVersion:  jsonEnvelopeVersion,
		Kind:           spec.Kind,
		ParentBackupID: spec.ParentBackupID,
		BaseSnapshotTS: spec.BaseSnapshotTS,
		SnapshotTS:     spec.Manifest.SnapshotTS,
		Encrypted:      spec.Encrypted,
		KeyFingerprint: spec.KeyFingerprint,
		Manifest:       toJSONManifest(spec.Manifest),
		ChangedFiles:   toJSONFiles(spec.ChangedFiles),
		InheritedFiles: toJSONFiles(spec.InheritedFiles),
		EncryptedSizes: spec.EncryptedSizes,
	}
	digest, err := computeJSONEnvelopeDigest(body)
	if err != nil {
		return "", err
	}
	env := jsonEnvelopeFile{
		FormatVersion:  body.FormatVersion,
		Kind:           body.Kind,
		BackupID:       digest,
		ParentBackupID: body.ParentBackupID,
		BaseSnapshotTS: body.BaseSnapshotTS,
		SnapshotTS:     body.SnapshotTS,
		Encrypted:      body.Encrypted,
		KeyFingerprint: body.KeyFingerprint,
		Manifest:       body.Manifest,
		ChangedFiles:   body.ChangedFiles,
		InheritedFiles: body.InheritedFiles,
		EncryptedSizes: body.EncryptedSizes,
		EnvelopeSHA256: digest,
	}
	encoded, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	if uint64(len(encoded)) > limits.MaxManifestBytes {
		return "", ErrLimit
	}
	file, err := root.OpenFile(BackupManifestName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	n, writeErr := file.Write(encoded)
	if n != len(encoded) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	if writeErr != nil {
		return "", writeErr
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	return digest, nil
}

func loadAnyEnvelope(ctx context.Context, root *os.Root, limits Limits, scratch []byte) (loaded loadedEnvelope, err error) {
	file, err := openRegular(root, BackupManifestName)
	if err != nil {
		return loaded, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return loaded, err
	}
	if info.Size() <= 0 {
		return loaded, ErrCorrupt
	}
	if uint64(info.Size()) > limits.MaxManifestBytes {
		return loaded, ErrLimit
	}
	var first [1]byte
	if _, err := file.ReadAt(first[:], 0); err != nil {
		return loaded, errors.Join(ErrCorrupt, err)
	}
	if first[0] != '{' {
		manifest, err := readEnvelope(ctx, file, limits, scratch)
		if err != nil {
			return loaded, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return loaded, err
		}
		h := sha256.New()
		if _, err := io.CopyBuffer(h, file, scratch); err != nil {
			return loaded, err
		}
		var sum [sha256.Size]byte
		h.Sum(sum[:0])
		return loadedEnvelope{
			BackupID:     hex.EncodeToString(sum[:]),
			Kind:         backupKindFull,
			Encrypted:    false,
			Manifest:     manifest,
			ChangedFiles: manifest.Files,
		}, nil
	}
	raw := make([]byte, int(info.Size()))
	if _, err := io.ReadFull(file, raw); err != nil {
		return loaded, errors.Join(ErrCorrupt, err)
	}
	if err := requireEOF(ctx, file); err != nil {
		return loaded, err
	}
	var env jsonEnvelopeFile
	if err := json.Unmarshal(raw, &env); err != nil {
		return loaded, errors.Join(ErrCorrupt, err)
	}
	if env.FormatVersion != jsonEnvelopeVersion {
		return loaded, ErrCorrupt
	}
	if env.Kind != backupKindFull && env.Kind != backupKindIncremental {
		return loaded, ErrCorrupt
	}
	body := jsonEnvelopeBody{
		FormatVersion:  env.FormatVersion,
		Kind:           env.Kind,
		ParentBackupID: env.ParentBackupID,
		BaseSnapshotTS: env.BaseSnapshotTS,
		SnapshotTS:     env.SnapshotTS,
		Encrypted:      env.Encrypted,
		KeyFingerprint: env.KeyFingerprint,
		Manifest:       env.Manifest,
		ChangedFiles:   env.ChangedFiles,
		InheritedFiles: env.InheritedFiles,
		EncryptedSizes: env.EncryptedSizes,
	}
	expectedDigest, err := computeJSONEnvelopeDigest(body)
	if err != nil || env.EnvelopeSHA256 != expectedDigest || env.BackupID != expectedDigest {
		return loaded, ErrCorrupt
	}
	manifest, err := fromJSONManifest(env.Manifest)
	if err != nil {
		return loaded, err
	}
	if _, err := validateManifest(ctx, manifest, limits); err != nil {
		return loaded, err
	}
	if env.SnapshotTS != manifest.SnapshotTS {
		return loaded, ErrCorrupt
	}
	changed, err := fromJSONFiles(env.ChangedFiles)
	if err != nil {
		return loaded, err
	}
	inherited, err := fromJSONFiles(env.InheritedFiles)
	if err != nil {
		return loaded, err
	}
	if env.Kind == backupKindFull {
		if env.ParentBackupID != "" || len(inherited) != 0 {
			return loaded, ErrCorrupt
		}
		if len(changed) == 0 {
			changed = manifest.Files
		}
	} else {
		if env.ParentBackupID == "" {
			return loaded, ErrCorrupt
		}
	}
	if err := validateFilePartition(manifest.Files, changed, inherited); err != nil {
		return loaded, err
	}
	if env.Encrypted {
		if len(env.KeyFingerprint) != sha256.Size*2 {
			return loaded, ErrCorrupt
		}
		if len(env.EncryptedSizes) != len(changed) {
			return loaded, ErrCorrupt
		}
		for _, f := range changed {
			if _, ok := env.EncryptedSizes[f.Name]; !ok {
				return loaded, ErrCorrupt
			}
		}
	}
	return loadedEnvelope{
		BackupID:       env.BackupID,
		ParentBackupID: env.ParentBackupID,
		Kind:           env.Kind,
		BaseSnapshotTS: env.BaseSnapshotTS,
		Encrypted:      env.Encrypted,
		KeyFingerprint: env.KeyFingerprint,
		Manifest:       manifest,
		ChangedFiles:   changed,
		InheritedFiles: inherited,
		EncryptedSizes: env.EncryptedSizes,
	}, nil
}

// validateFilePartition checks that changed and inherited are strictly sorted,
// disjoint, and their union equals all.
func validateFilePartition(all, changed, inherited []storage.CheckpointFile) error {
	if len(changed)+len(inherited) != len(all) {
		return ErrCorrupt
	}
	ci, ii := 0, 0
	for _, f := range all {
		switch {
		case ci < len(changed) && changed[ci] == f:
			ci++
		case ii < len(inherited) && inherited[ii] == f:
			ii++
		default:
			return ErrCorrupt
		}
	}
	if ci != len(changed) || ii != len(inherited) {
		return ErrCorrupt
	}
	return nil
}

// inspectBackupSubset requires root to contain exactly backup.manifest, commits/,
// runs/, and the sorted subset of physically stored files (ChangedFiles for an
// incremental backup or Files for a full backup).
func inspectBackupSubset(ctx context.Context, root *os.Root, stored []storage.CheckpointFile) (err error) {
	var rootFiles int
	for _, file := range stored {
		if !strings.Contains(file.Name, "/") {
			rootFiles++
		}
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	var files, directories int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			name := entry.Name()
			switch {
			case name == BackupManifestName:
				if !entry.Type().IsRegular() {
					return ErrCorrupt
				}
			case entry.IsDir():
				if name != "commits" && name != "runs" {
					return fmt.Errorf("unexpected backup entry %q: %w", name, ErrCorrupt)
				}
				directories++
			case entry.Type().IsRegular():
				if _, found := slices.BinarySearchFunc(stored, name, func(file storage.CheckpointFile, name string) int {
					return strings.Compare(file.Name, name)
				}); !found || strings.Contains(name, "/") {
					return fmt.Errorf("unlisted backup entry %q: %w", name, ErrCorrupt)
				}
				files++
			default:
				return fmt.Errorf("unexpected backup entry %q: %w", name, ErrCorrupt)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if files != rootFiles || directories != 2 {
		return ErrCorrupt
	}
	for _, sub := range [...]string{"commits", "runs"} {
		expectedSub := 0
		prefix := sub + "/"
		for _, f := range stored {
			if strings.HasPrefix(f.Name, prefix) {
				expectedSub++
			}
		}
		if err := inspectBackupSubdirExact(ctx, root, sub, stored, expectedSub); err != nil {
			return err
		}
	}
	return nil
}

func inspectBackupSubdirExact(ctx context.Context, root *os.Root, name string, stored []storage.CheckpointFile, expected int) (err error) {
	child, err := openChildRoot(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, child.Close()) }()
	directory, err := child.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			count++
			if count > expected {
				return ErrCorrupt
			}
			if !entry.Type().IsRegular() {
				return ErrCorrupt
			}
			path := name + "/" + entry.Name()
			if _, found := slices.BinarySearchFunc(stored, path, func(file storage.CheckpointFile, name string) int {
				return strings.Compare(file.Name, name)
			}); !found {
				return fmt.Errorf("unlisted backup file %q: %w", path, ErrCorrupt)
			}
		}
		if readErr == io.EOF {
			if count != expected {
				return ErrCorrupt
			}
			return nil
		}
	}
}

// CreateIncrementalBackup captures a consistent checkpoint at
// request.Snapshot.Fence.ClosedTS and copies only files that are new or
// modified relative to baseBackupDir (which may be a full backup or a previous
// incremental backup). Identical files (matching Name, Size, and SHA256) are
// recorded as InheritedFiles without duplicating their data on disk.
func CreateIncrementalBackup(ctx context.Context, source Checkpointer, request storage.CheckpointRequest, baseBackupDir string, options BackupOptions) (result IncrementalManifest, err error) {
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if nilDependency(source) || baseBackupDir == "" {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := validateSnapshot(request.Snapshot); err != nil {
		return result, err
	}
	cipher, err := newBackupCipher(options.EncryptionKey)
	if err != nil {
		return result, err
	}
	baseRoot, err := openBackupRoot(baseBackupDir)
	if err != nil {
		return result, err
	}
	defer func() {
		if baseRoot != nil {
			err = errors.Join(err, baseRoot.Close())
		}
		if err != nil {
			result = IncrementalManifest{}
		}
	}()
	scratch := make([]byte, int(options.Limits.CopyBufferBytes))
	baseEnv, err := loadAnyEnvelope(ctx, baseRoot, options.Limits, scratch)
	if err != nil {
		return result, err
	}
	if err := inspectBackupSubset(ctx, baseRoot, baseEnv.ChangedFiles); err != nil {
		return result, err
	}
	closeBaseErr := baseRoot.Close()
	baseRoot = nil
	if closeBaseErr != nil {
		return result, closeBaseErr
	}
	if request.Snapshot.Timestamp < baseEnv.Manifest.SnapshotTS {
		return result, fmt.Errorf("incremental snapshot %d precedes base snapshot %d: %w", request.Snapshot.Timestamp, baseEnv.Manifest.SnapshotTS, ErrInvalid)
	}

	baseFiles := make(map[string]storage.CheckpointFile, len(baseEnv.Manifest.Files))
	for _, f := range baseEnv.Manifest.Files {
		baseFiles[f.Name] = f
	}

	stage, err := newStagedTree(ctx, options.Destination)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.close()) }()

	handle, err := source.Checkpoint(ctx, request)
	defer func() {
		if !nilDependency(handle) {
			err = errors.Join(err, handle.Close())
		}
	}()
	if err != nil {
		return result, err
	}
	if nilDependency(handle) {
		return result, ErrInvalid
	}
	manifest, err := handle.Manifest(ctx, storage.CheckpointManifestLimits{MaxBytes: options.Limits.MaxMetadataBytes, MaxFiles: options.Limits.MaxFiles})
	if err != nil {
		return result, err
	}
	if _, err := validateManifest(ctx, manifest, options.Limits); err != nil {
		return result, err
	}
	if manifest.SnapshotTS != request.Snapshot.Timestamp || manifest.Fence != request.Snapshot.Fence {
		return result, ErrInvalid
	}

	var changed []storage.CheckpointFile
	var inherited []storage.CheckpointFile
	var encryptedSizes map[string]uint64
	if cipher != nil {
		encryptedSizes = make(map[string]uint64)
	}
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if prev, exists := baseFiles[file.Name]; exists && prev.Size == file.Size && prev.SHA256 == file.SHA256 {
			inherited = append(inherited, file)
			continue
		}
		changed = append(changed, file)
		stream, err := handle.OpenFile(ctx, file.Name)
		if err != nil {
			if !nilDependency(stream) {
				err = errors.Join(err, stream.Close())
			}
			return result, fmt.Errorf("open checkpoint file %q: %w", file.Name, err)
		}
		if nilDependency(stream) {
			return result, ErrInvalid
		}
		if cipher != nil {
			encSize, copyErr := copyFileEncrypted(ctx, stage.root, file, stream, cipher)
			if copyErr != nil {
				return result, copyErr
			}
			encryptedSizes[file.Name] = encSize
		} else {
			if err := copyFile(ctx, stage.root, file, stream, scratch); err != nil {
				return result, err
			}
		}
	}
	closeErr := handle.Close()
	handle = nil
	if closeErr != nil {
		return result, closeErr
	}

	backupID, err := writeJSONEnvelope(ctx, stage.root, backupEnvelopeSpec{
		Kind:           backupKindIncremental,
		ParentBackupID: baseEnv.BackupID,
		BaseSnapshotTS: baseEnv.Manifest.SnapshotTS,
		Encrypted:      cipher != nil,
		KeyFingerprint: cipher.fingerprintHex(),
		Manifest:       manifest,
		ChangedFiles:   changed,
		InheritedFiles: inherited,
		EncryptedSizes: encryptedSizes,
	}, options.Limits)
	if err != nil {
		return result, err
	}
	if err := stage.publish(ctx); err != nil {
		return result, err
	}
	return IncrementalManifest{
		BackupID:       backupID,
		ParentBackupID: baseEnv.BackupID,
		BaseSnapshotTS: baseEnv.Manifest.SnapshotTS,
		SnapshotTS:     manifest.SnapshotTS,
		Encrypted:      cipher != nil,
		KeyFingerprint: cipher.fingerprintHex(),
		Manifest:       manifest,
		ChangedFiles:   changed,
		InheritedFiles: inherited,
	}, nil
}

type openedChainHop struct {
	dir  string
	root *os.Root
	env  loadedEnvelope
}

func openValidatedChain(ctx context.Context, chainDirs []string, limits Limits, cipher *backupCipher, scratch []byte) (hops []openedChainHop, err error) {
	if len(chainDirs) == 0 {
		return nil, ErrInvalid
	}
	defer func() {
		if err != nil {
			for i := range hops {
				if hops[i].root != nil {
					err = errors.Join(err, hops[i].root.Close())
				}
			}
			hops = nil
		}
	}()
	hops = make([]openedChainHop, 0, len(chainDirs))
	for i, dir := range chainDirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root, openErr := openBackupRoot(dir)
		if openErr != nil {
			return nil, openErr
		}
		hop := openedChainHop{dir: dir, root: root}
		hops = append(hops, hop)
		env, envErr := loadAnyEnvelope(ctx, root, limits, scratch)
		if envErr != nil {
			return nil, envErr
		}
		if env.Encrypted {
			if cipher == nil || !cipher.matchesFingerprintHex(env.KeyFingerprint) {
				return nil, ErrInvalidEncryptionKey
			}
		}
		if i == 0 {
			if env.Kind != backupKindFull {
				return nil, fmt.Errorf("chain base %q is %q, expected full backup: %w", dir, env.Kind, ErrInvalid)
			}
		} else {
			prev := hops[i-1].env
			if env.Kind != backupKindIncremental {
				return nil, fmt.Errorf("chain hop %d (%q) is %q, expected incremental backup: %w", i, dir, env.Kind, ErrInvalid)
			}
			if env.ParentBackupID != prev.BackupID {
				return nil, fmt.Errorf("chain hop %d parent mismatch: got %q, want %q: %w", i, env.ParentBackupID, prev.BackupID, ErrCorrupt)
			}
			if env.BaseSnapshotTS != prev.Manifest.SnapshotTS || env.Manifest.SnapshotTS < env.BaseSnapshotTS {
				return nil, fmt.Errorf("chain hop %d snapshot lineage mismatch: %w", i, ErrCorrupt)
			}
			for _, inh := range env.InheritedFiles {
				idx, found := slices.BinarySearchFunc(prev.Manifest.Files, inh.Name, func(f storage.CheckpointFile, name string) int {
					return strings.Compare(f.Name, name)
				})
				if !found || prev.Manifest.Files[idx] != inh {
					return nil, fmt.Errorf("chain hop %d inherited file %q mismatch: %w", i, inh.Name, ErrCorrupt)
				}
			}
		}
		if err := inspectBackupSubset(ctx, root, env.ChangedFiles); err != nil {
			return nil, err
		}
		hops[i].env = env
	}
	return hops, nil
}

func stageChainIntoRoot(ctx context.Context, stageRoot *os.Root, hops []openedChainHop, cipher *backupCipher, scratch []byte) (storage.CheckpointManifest, error) {
	finalManifest := hops[len(hops)-1].env.Manifest
	// Map each file in the final manifest to the highest hop that physically stores it.
	owners := make(map[string]int, len(finalManifest.Files))
	for i, hop := range hops {
		for _, f := range hop.env.ChangedFiles {
			owners[f.Name] = i
		}
	}
	for _, file := range finalManifest.Files {
		if err := ctx.Err(); err != nil {
			return storage.CheckpointManifest{}, err
		}
		hopIdx, ok := owners[file.Name]
		if !ok {
			return storage.CheckpointManifest{}, fmt.Errorf("missing file %q in backup chain: %w", file.Name, ErrCorrupt)
		}
		hop := hops[hopIdx]
		idx, found := slices.BinarySearchFunc(hop.env.ChangedFiles, file.Name, func(f storage.CheckpointFile, name string) int {
			return strings.Compare(f.Name, name)
		})
		if !found || hop.env.ChangedFiles[idx] != file {
			return storage.CheckpointManifest{}, fmt.Errorf("chain owner checksum mismatch for %q: %w", file.Name, ErrCorrupt)
		}
		if hop.env.Encrypted {
			encSize, hasSize := hop.env.EncryptedSizes[file.Name]
			if !hasSize {
				return storage.CheckpointManifest{}, ErrCorrupt
			}
			stream, err := openBackupFileWithSize(hop.root, file.Name, encSize)
			if err != nil {
				return storage.CheckpointManifest{}, fmt.Errorf("open backup file %q: %w", file.Name, err)
			}
			if err := copyFileDecrypted(ctx, stageRoot, file, stream, cipher); err != nil {
				return storage.CheckpointManifest{}, err
			}
		} else {
			stream, err := openBackupFile(hop.root, file)
			if err != nil {
				return storage.CheckpointManifest{}, fmt.Errorf("open backup file %q: %w", file.Name, err)
			}
			if err := copyFile(ctx, stageRoot, file, stream, scratch); err != nil {
				return storage.CheckpointManifest{}, err
			}
		}
	}
	return finalManifest, nil
}

// RestoreIncremental validates the complete backup chain [baseDir, inc1Dir, ...],
// verifies parent lineage, checksums, and decryption keys, reconstructs the
// database directory at options.Destination, runs options.Verifier, and
// publishes the destination atomically.
func RestoreIncremental(ctx context.Context, chainDirs []string, options RestoreOptions) (result storage.CheckpointManifest, err error) {
	if err := options.Limits.Validate(); err != nil {
		return result, err
	}
	if nilDependency(options.Verifier) || len(chainDirs) == 0 {
		return result, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	cipher, err := newBackupCipher(options.EncryptionKey)
	if err != nil {
		return result, err
	}
	scratch := make([]byte, int(options.Limits.CopyBufferBytes))
	hops, err := openValidatedChain(ctx, chainDirs, options.Limits, cipher, scratch)
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
			result = storage.CheckpointManifest{}
		}
	}()
	stage, err := newStagedTree(ctx, options.Destination)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, stage.close()) }()

	manifest, err := stageChainIntoRoot(ctx, stage.root, hops, cipher, scratch)
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
		return result, fmt.Errorf("verify restored checkpoint: %w", err)
	}
	if err := stage.publish(ctx); err != nil {
		return result, err
	}
	return manifest, nil
}

// ReadBackupFile reads, decrypts (if encrypted), and SHA-256-verifies a single
// manifest-listed file from a full backup directory (for example fence.secret).
func ReadBackupFile(ctx context.Context, backupDir, fileName string, limits Limits, encryptionKey []byte) ([]byte, error) {
	return ReadChainFile(ctx, []string{backupDir}, fileName, limits, encryptionKey)
}

// ReadChainFile reads, decrypts (if encrypted), and SHA-256-verifies a single
// manifest-listed file from the latest hop that stores it in a backup chain.
func ReadChainFile(ctx context.Context, chainDirs []string, fileName string, limits Limits, encryptionKey []byte) (_ []byte, err error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDatabasePath(fileName) {
		return nil, ErrInvalid
	}
	cipher, err := newBackupCipher(encryptionKey)
	if err != nil {
		return nil, err
	}
	scratch := make([]byte, int(limits.CopyBufferBytes))
	hops, err := openValidatedChain(ctx, chainDirs, limits, cipher, scratch)
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range hops {
			if hops[i].root != nil {
				err = errors.Join(err, hops[i].root.Close())
			}
		}
	}()
	finalFiles := hops[len(hops)-1].env.Manifest.Files
	idx, found := slices.BinarySearchFunc(finalFiles, fileName, func(f storage.CheckpointFile, name string) int {
		return strings.Compare(f.Name, name)
	})
	if !found {
		return nil, fmt.Errorf("%q not in backup manifest: %w", fileName, ErrCorrupt)
	}
	target := finalFiles[idx]
	ownerIdx := -1
	for i := len(hops) - 1; i >= 0; i-- {
		if _, ok := slices.BinarySearchFunc(hops[i].env.ChangedFiles, fileName, func(f storage.CheckpointFile, name string) int {
			return strings.Compare(f.Name, name)
		}); ok {
			ownerIdx = i
			break
		}
	}
	if ownerIdx < 0 {
		return nil, ErrCorrupt
	}
	hop := hops[ownerIdx]
	var out bytes.Buffer
	if hop.env.Encrypted {
		encSize, ok := hop.env.EncryptedSizes[fileName]
		if !ok {
			return nil, ErrCorrupt
		}
		f, err := openBackupFileWithSize(hop.root, fileName, encSize)
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		limit := target.Size
		plainSize, plainSHA, decErr := decryptStreamWithCipher(ctx, &out, f, cipher, &limit)
		if decErr != nil {
			return nil, decErr
		}
		if plainSize != target.Size || plainSHA != target.SHA256 {
			return nil, ErrCorrupt
		}
		return out.Bytes(), nil
	}
	f, err := openBackupFile(hop.root, target)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := copyVerified(ctx, &out, f, target, scratch); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
