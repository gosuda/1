package maintenance

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

const envelopeMagic = "SSQBKP01"
const envelopeFixedBytes uint64 = 240
const fileFixedBytes uint64 = 48

func admit(used *uint64, size, limit uint64) error {
	if *used > limit || size > limit-*used {
		return ErrLimit
	}
	*used += size
	return nil
}

func validateManifest(ctx context.Context, manifest storage.CheckpointManifest, limits Limits) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if manifest.FormatVersion != 1 || manifest.Backend != "neosepia" || manifest.HistoryFloor < 0 || manifest.HistoryFloor > manifest.SnapshotTS {
		return 0, ErrInvalid
	}
	if err := validateSnapshot(storage.SnapshotSpec{Timestamp: manifest.SnapshotTS, Fence: manifest.Fence}); err != nil {
		return 0, err
	}
	metadata := uint64(unsafe.Sizeof(storage.CheckpointManifest{}))
	wire := envelopeFixedBytes
	var names, total uint64
	if err := admit(&metadata, uint64(len(manifest.Scale.CanonicalLeapHistory)), limits.MaxMetadataBytes); err != nil {
		return 0, err
	}
	if err := admit(&wire, uint64(len(manifest.Scale.CanonicalLeapHistory)), limits.MaxManifestBytes); err != nil {
		return 0, err
	}
	for _, name := range [...]string{manifest.Backend, manifest.Scale.Name} {
		if err := admitName(name, &metadata, &wire, &names, limits); err != nil {
			return 0, err
		}
	}
	count := uint64(len(manifest.Files))
	entryBytes := uint64(unsafe.Sizeof(storage.CheckpointFile{}))
	if count == 0 {
		return 0, ErrInvalid
	}
	if count > limits.MaxFiles || count > (limits.MaxMetadataBytes-metadata)/entryBytes || count > (limits.MaxManifestBytes-wire)/fileFixedBytes {
		return 0, ErrLimit
	}
	metadata += count * entryBytes
	wire += count * fileFixedBytes
	var previous string
	var hasState bool
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := admitName(file.Name, &metadata, &wire, &names, limits); err != nil {
			return 0, err
		}
		if !validDatabasePath(file.Name) || previous >= file.Name {
			return 0, fmt.Errorf("file name %q: %w", file.Name, ErrInvalid)
		}
		if file.Size > limits.MaxFileBytes {
			return 0, ErrLimit
		}
		if err := admit(&total, file.Size, limits.MaxTotalBytes); err != nil {
			return 0, err
		}
		hasState = hasState || file.Name == "database.state"
		previous = file.Name
	}
	if !hasState {
		return 0, ErrInvalid
	}
	if err := clock.ValidateScale(ctx, manifest.Scale, limits.MaxMetadataBytes); err != nil {
		return 0, err
	}
	return wire, nil
}

func admitName(name string, metadata, wire, names *uint64, limits Limits) error {
	size := uint64(len(name))
	if size == 0 || size > limits.MaxNameBytes {
		return ErrLimit
	}
	if err := admit(names, size, limits.MaxNamesBytes); err != nil {
		return err
	}
	if err := admit(metadata, size, limits.MaxMetadataBytes); err != nil {
		return err
	}
	return admit(wire, size, limits.MaxManifestBytes)
}

func validateSnapshot(snapshot storage.SnapshotSpec) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if snapshot.Overlay != nil || snapshot.Timestamp != snapshot.Fence.ClosedTS {
		return ErrInvalid
	}
	epoch := snapshot.Fence.Epoch
	if epoch.Assurance == ([16]byte{}) || epoch.LeapHistory == ([32]byte{}) || epoch.Config == ([32]byte{}) {
		return ErrInvalid
	}
	return nil
}

// validDatabasePath enforces traversal-safe manifest names. Directory children
// stay confined to commits/ and runs/; plain root names admit database.state
// and composition-declared extras, whose format admission is the backend
// verifier's responsibility, not the copy layer's.
func validDatabasePath(name string) bool {
	if !fs.ValidPath(name) || !utf8.ValidString(name) || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, char := range name {
		if char < ' ' || char == 127 {
			return false
		}
	}
	if name == "database.state" {
		return true
	}
	if name == BackupManifestName || name == ".lock" || strings.HasPrefix(name, ".") {
		return false
	}
	directory, base, ok := strings.Cut(name, "/")
	if !ok {
		return true
	}
	if (directory != "commits" && directory != "runs") || strings.Contains(base, "/") {
		return false
	}
	return base != ".lock" && base != BackupManifestName && !strings.HasPrefix(base, ".")
}
