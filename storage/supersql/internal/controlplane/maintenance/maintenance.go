// Package maintenance copies immutable native checkpoints. The composition root
// opens a restored database afterward with an explicit Clock and readiness policy.
package maintenance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var (
	ErrInvalid             = errors.New("invalid checkpoint backup")
	ErrLimit               = errors.New("checkpoint backup resource limit exceeded")
	ErrCorrupt             = errors.New("corrupt checkpoint backup")
	ErrDestinationOccupied = errors.New("checkpoint destination is not an empty directory")
)

const BackupManifestName = "backup.manifest"

// PublicationError means rename committed the destination, but final durability
// or cleanup could not be confirmed. The tree is preserved; do not blindly retry.
type PublicationError struct {
	Destination string
	Err         error
}

func (err *PublicationError) Error() string {
	return fmt.Sprintf("checkpoint destination %q published; durability or cleanup indeterminate: %v", err.Destination, err.Err)
}

func (err *PublicationError) Unwrap() error { return err.Err }

// Limits admit both wire data and owned decoded metadata before allocation.
// MaxNamesBytes includes backend, scale, and file names. Every field is required;
// zero never means unlimited. File sizes must fit the filesystem's signed offsets.
type Limits struct {
	MaxManifestBytes uint64
	MaxMetadataBytes uint64
	MaxFiles         uint64
	MaxNameBytes     uint64
	MaxNamesBytes    uint64
	MaxFileBytes     uint64
	MaxTotalBytes    uint64
	CopyBufferBytes  uint64
}

func (limits Limits) Validate() error {
	for _, value := range [...]uint64{limits.MaxManifestBytes, limits.MaxMetadataBytes, limits.MaxFiles, limits.MaxNameBytes, limits.MaxNamesBytes, limits.CopyBufferBytes} {
		if value == 0 || value > uint64(math.MaxInt) {
			return ErrLimit
		}
	}
	if limits.MaxFileBytes == 0 || limits.MaxFileBytes > math.MaxInt64 || limits.MaxTotalBytes == 0 {
		return ErrLimit
	}
	return nil
}

// Checkpointer is borrowed; CreateBackup owns and closes only its returned handle.
type Checkpointer interface {
	Checkpoint(context.Context, storage.CheckpointRequest) (storage.CheckpointHandle, error)
}

// CheckpointVerifier borrows an immutable staged database tree for read-only
// bounded validation. It must neither close the root nor repair the database.
type CheckpointVerifier interface {
	VerifyCheckpoint(context.Context, *os.Root, storage.CheckpointManifest) error
}

type BackupOptions struct {
	Destination string
	Limits      Limits
}

type RestoreOptions struct {
	Destination string
	Limits      Limits
	Verifier    CheckpointVerifier
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
