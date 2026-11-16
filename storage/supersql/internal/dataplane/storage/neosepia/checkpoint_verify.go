package neosepia

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

// CheckpointVerifier borrows an immutable staged tree and never repairs it.
// A zero Limits selects DefaultLimits; the fence authority is required.
// ExtraFiles names composition-owned root entries the manifest may list beyond
// backend files (for example the fence secret), verified like every other file.
type CheckpointVerifier struct {
	FenceVerifier storage.FenceVerifier
	Limits        Limits
	ExtraFiles    []string
}

type checkpointVerification struct {
	limits       Limits
	manifest     storage.CheckpointManifest
	extras       []string
	used         []bool
	metadata     uint64
	transactions ordered.Tree[recoveryTxnID, transactionRecord]
	layouts      ordered.Tree[checkpointTarget, [32]byte]
}

func (verifier CheckpointVerifier) VerifyCheckpoint(ctx context.Context, root *os.Root, manifest storage.CheckpointManifest) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if verifier.FenceVerifier == nil {
		return ErrInvalidConfig
	}
	limits := verifier.Limits
	if limits == (Limits{}) {
		limits = DefaultLimits()
	}
	if err := limits.validate(); err != nil {
		return err
	}
	if root == nil {
		return storage.ErrClosed
	}
	extras, err := validateCheckpointExtras(verifier.ExtraFiles)
	if err != nil {
		return err
	}
	check := checkpointVerification{limits: limits, manifest: manifest, extras: extras}
	if err := check.validateManifest(ctx); err != nil {
		return err
	}
	if err := check.verifyTree(ctx, root); err != nil {
		return err
	}
	if err := check.consume(databaseStateName, nil); err != nil {
		return err
	}
	for _, name := range check.extras {
		if err := check.consume(name, nil); err != nil {
			return err
		}
	}
	state, err := readDatabaseState(ctx, root, limits.MetadataBytes-check.metadata)
	if err != nil {
		return err
	}
	if state.HistoryFloor != manifest.HistoryFloor || state.Closed != manifest.Fence {
		return storage.ErrCorruptStorage
	}
	latest := state.Scales[len(state.Scales)-1].Scale
	if latest.Name != manifest.Scale.Name || latest.LeapHistoryID != manifest.Scale.LeapHistoryID || !bytes.Equal(latest.CanonicalLeapHistory, manifest.Scale.CanonicalLeapHistory) {
		return storage.ErrCorruptStorage
	}
	for _, version := range state.Scales {
		if version.Position > manifest.Fence.LogPosition || version.Timestamp > manifest.SnapshotTS {
			return storage.ErrCorruptStorage
		}
	}
	stateIndex, _ := check.index(databaseStateName)
	stateBytes := manifest.Files[stateIndex].Size + uint64(len(state.Scales))*uint64(unsafe.Sizeof(scaleVersion{}))
	if err := check.reserveMetadata(stateBytes); err != nil {
		return err
	}
	if err := verifier.FenceVerifier.VerifyReadFence(ctx, state.Closed); err != nil {
		return err
	}
	commits, err := root.OpenRoot("commits")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, commits.Close()) }()
	runs, err := root.OpenRoot("runs")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runs.Close()) }()
	position, err := check.verifyBase(ctx, runs, state)
	if err != nil {
		return err
	}
	if manifest.Fence.LogPosition-position.LogPosition > limits.RecoveryFiles {
		return storage.ErrDecodeBudget
	}
	for position.LogPosition < manifest.Fence.LogPosition {
		position, err = check.verifyDecision(ctx, commits, runs, state, position)
		if err != nil {
			return err
		}
	}
	if position.CommitTS > manifest.SnapshotTS || check.transactions.Len() != manifest.Fence.LogPosition {
		return storage.ErrCorruptStorage
	}
	for _, used := range check.used {
		if !used {
			return storage.ErrCorruptStorage
		}
	}
	return ctx.Err()
}

func (check *checkpointVerification) reserveMetadata(size uint64) error {
	if size > check.limits.MetadataBytes-check.metadata-check.layouts.NodeBytes() {
		return columnar.ErrBatchBudget
	}
	check.metadata += size
	return nil
}

func (check *checkpointVerification) reserveRunMetadata(files []preparedRun) (uint64, error) {
	unit := uint64(unsafe.Sizeof(preparedRun{}))
	if uint64(len(files)) > (check.limits.MetadataBytes-check.metadata-check.layouts.NodeBytes())/unit {
		return 0, columnar.ErrBatchBudget
	}
	used := uint64(len(files)) * unit
	for _, file := range files {
		size := uint64(len(file.Name)) + uint64(cap(file.LowerKey)) + uint64(cap(file.UpperKey))
		if size > check.limits.MetadataBytes-used {
			return 0, columnar.ErrBatchBudget
		}
		used += size
	}
	return used, check.reserveMetadata(used)
}

func (check *checkpointVerification) validateManifest(ctx context.Context) error {
	manifest := check.manifest
	if manifest.Backend != "neosepia" || manifest.FormatVersion != 1 {
		return ErrIncompatibleFormat
	}
	if err := (storage.SnapshotSpec{Timestamp: manifest.SnapshotTS, Fence: manifest.Fence}).Validate(); err != nil {
		return err
	}
	if manifest.SnapshotTS != manifest.Fence.ClosedTS || manifest.HistoryFloor < 0 || manifest.HistoryFloor > manifest.SnapshotTS {
		return storage.ErrCorruptStorage
	}
	epoch := manifest.Fence.Epoch
	if epoch.Assurance == ([16]byte{}) || epoch.Config == ([32]byte{}) || epoch.LeapHistory == ([32]byte{}) {
		return storage.ErrSnapshotNotClosed
	}
	if err := clock.ValidateScale(ctx, manifest.Scale, check.limits.MetadataBytes); err != nil {
		return err
	}
	if len(manifest.Files) == 0 {
		return storage.ErrCorruptStorage
	}
	unit := uint64(unsafe.Sizeof(storage.CheckpointFile{})) + 1
	if uint64(len(manifest.Files)) > check.limits.MaxFiles || uint64(len(manifest.Files)) > check.limits.MetadataBytes/unit {
		return storage.ErrDecodeBudget
	}
	if err := check.reserveMetadata(uint64(len(manifest.Files)) * unit); err != nil {
		return err
	}
	if err := check.reserveMetadata(uint64(len(manifest.Scale.Name)) + uint64(len(manifest.Scale.CanonicalLeapHistory))); err != nil {
		return err
	}
	for i, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i > 0 && manifest.Files[i-1].Name >= file.Name {
			return storage.ErrCorruptStorage
		}
		if file.Size == 0 || file.Size > math.MaxInt64 || !check.checkpointName(file.Name) {
			return storage.ErrCorruptStorage
		}
		if err := check.reserveMetadata(uint64(len(file.Name))); err != nil {
			return err
		}
	}
	check.used = make([]bool, len(manifest.Files))
	return nil
}

func (check *checkpointVerification) checkpointName(name string) bool {
	if checkpointDatabaseName(name) {
		return true
	}
	_, found := slices.BinarySearch(check.extras, name)
	return found
}

func checkpointDatabaseName(name string) bool {
	if name == databaseStateName {
		return true
	}
	if strings.HasPrefix(name, "commits/") {
		_, pending, err := commitFilePosition(strings.TrimPrefix(name, "commits/"))
		return err == nil && !pending
	}
	if !strings.HasPrefix(name, "runs/") {
		return false
	}
	name = strings.TrimPrefix(name, "runs/")
	return validBaseRunName(name) && !strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".sst") || strings.HasSuffix(name, ".runs"))
}

func (check *checkpointVerification) index(name string) (int, bool) {
	return slices.BinarySearchFunc(check.manifest.Files, name, func(file storage.CheckpointFile, name string) int {
		return strings.Compare(file.Name, name)
	})
}

func (check *checkpointVerification) consume(name string, descriptor *preparedRun) error {
	index, found := check.index(name)
	if !found || check.used[index] {
		return fmt.Errorf("%w: unexpected checkpoint reference %q", storage.ErrCorruptStorage, name)
	}
	file := check.manifest.Files[index]
	if descriptor != nil && (file.Size != descriptor.Bytes || file.SHA256 != descriptor.Checksum) {
		return fmt.Errorf("%w: checkpoint descriptor %q", storage.ErrCorruptStorage, name)
	}
	check.used[index] = true
	return nil
}

func (check *checkpointVerification) verifyTree(ctx context.Context, root *os.Root) error {
	size := min(uint64(64<<10), check.limits.MetadataBytes-check.metadata)
	if size == 0 {
		return columnar.ErrBatchBudget
	}
	scratch := make([]byte, int(size))
	var files uint64
	if err := check.verifyDirectory(ctx, root, "", scratch, &files); err != nil {
		return err
	}
	if files != uint64(len(check.manifest.Files)) {
		return storage.ErrCorruptStorage
	}
	return nil
}

func (check *checkpointVerification) verifyDirectory(ctx context.Context, root *os.Root, prefix string, scratch []byte, count *uint64) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	var directories uint8
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(1)
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			name := entry.Name()
			nativeDirectory := name == "commits" || name == "runs"
			if info.IsDir() && prefix == "" && nativeDirectory {
				if name == "commits" {
					directories |= 1
				} else {
					directories |= 2
				}
				err := check.verifySubdirectory(ctx, root, name, scratch, count)
				if err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return storage.ErrCorruptStorage
			}
			index, found := check.index(prefix + name)
			if !found || info.Size() < 0 || uint64(info.Size()) != check.manifest.Files[index].Size {
				return storage.ErrCorruptStorage
			}
			if *count == uint64(len(check.manifest.Files)) {
				return storage.ErrDecodeBudget
			}
			*count++
			if err := verifyCheckpointHash(ctx, root, name, check.manifest.Files[index], scratch); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			if prefix == "" && directories != 3 {
				return storage.ErrCorruptStorage
			}
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (check *checkpointVerification) verifySubdirectory(ctx context.Context, root *os.Root, name string, scratch []byte, count *uint64) (err error) {
	child, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, child.Close()) }()
	return check.verifyDirectory(ctx, child, name+"/", scratch, count)
}

func verifyCheckpointHash(ctx context.Context, root *os.Root, name string, expected storage.CheckpointFile, scratch []byte) (err error) {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	checksum, err := checksumRunFile(ctx, file, int64(expected.Size), scratch)
	if err != nil {
		return err
	}
	if checksum != expected.SHA256 {
		return fmt.Errorf("%w: checkpoint checksum %q", storage.ErrCorruptStorage, expected.Name)
	}
	return nil
}
