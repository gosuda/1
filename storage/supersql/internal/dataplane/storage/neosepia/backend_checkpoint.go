package neosepia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

const checkpointMagic = "NSCP0001"
const checkpointPrefix = ".checkpoint-"

func (backend *Backend) Checkpoint(ctx context.Context, request storage.CheckpointRequest) (_ storage.CheckpointHandle, err error) {
	if request.Snapshot.Overlay != nil {
		return nil, storage.ErrInvalidMutation
	}
	backend.commitMu.Lock()
	backend.mu.Lock()
	builder, err := backend.captureCheckpoint(ctx, request.Snapshot)
	backend.mu.Unlock()
	backend.commitMu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, builder.handle.Close())
		}
	}()
	if err := builder.finish(ctx); err != nil {
		return nil, err
	}
	return builder.handle, nil
}

// Capture only links immutable files while holding the publication mutex;
// checksum validation runs afterward against the independent linked namespace.
func (backend *Backend) captureCheckpoint(ctx context.Context, spec storage.SnapshotSpec) (_ *checkpointBuilder, err error) {
	if err := backend.prepareRead(ctx, spec); err != nil {
		return nil, err
	}
	pin, err := backend.runs.pinCheckpoint(ctx, spec)
	if err != nil {
		return nil, err
	}
	handle := &checkpointHandle{pin: pin, maxStreams: backend.limits.MaxPins}
	defer func() {
		if err != nil {
			err = errors.Join(err, handle.Close())
		}
	}()
	builder := &checkpointBuilder{handle: handle, limits: backend.limits, extras: backend.extras, used: uint64(unsafe.Sizeof(checkpointHandle{})) + 1024}
	if err := builder.reserve(0); err != nil {
		return nil, err
	}
	handle.parent, err = backend.directory.root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	var identity [16]byte
	rand.Read(identity[:])
	name := checkpointPrefix + hex.EncodeToString(identity[:])
	if err := handle.parent.Mkdir(name, 0700); err != nil {
		return nil, err
	}
	handle.directory = name
	handle.root, err = handle.parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	handle.lock, err = handle.root.OpenFile(".lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockDatabaseFile(handle.lock); err != nil {
		return nil, err
	}
	count, err := handle.lock.WriteString(checkpointMagic)
	if err != nil {
		return nil, err
	}
	if count != len(checkpointMagic) {
		return nil, io.ErrShortWrite
	}
	if err := handle.lock.Sync(); err != nil {
		return nil, err
	}
	if err := syncCommitDirectory(handle.root); err != nil {
		return nil, err
	}
	if err := syncCommitDirectory(handle.parent); err != nil {
		return nil, err
	}
	if err := handle.root.Mkdir("commits", 0700); err != nil {
		return nil, err
	}
	if err := handle.root.Mkdir("runs", 0700); err != nil {
		return nil, err
	}
	currentScale := backend.state.Scales[len(backend.state.Scales)-1].Scale
	if err := builder.reserve(uint64(len(currentScale.CanonicalLeapHistory))); err != nil {
		return nil, err
	}
	handle.manifest = storage.CheckpointManifest{FormatVersion: 1, Backend: "neosepia", SnapshotTS: spec.Timestamp, HistoryFloor: backend.state.HistoryFloor, Fence: spec.Fence, Scale: copyClockScale(currentScale)}
	state := backend.state
	state.Closed = spec.Fence
	state.BaseGeneration = 0
	var basePosition uint64
	for generation := pin.generation; generation != nil; generation = generation.parent {
		if generation.baseGeneration != 0 {
			state.BaseGeneration = generation.baseGeneration
			basePosition = generation.position
			break
		}
	}
	scaleBytes := uint64(len(state.Scales)) * uint64(unsafe.Sizeof(scaleVersion{}))
	if err := builder.reserve(scaleBytes); err != nil {
		return nil, err
	}
	state.Scales = make([]scaleVersion, len(backend.state.Scales))
	for i, version := range backend.state.Scales {
		version.Position = min(version.Position, spec.Fence.LogPosition)
		version.Timestamp = min(version.Timestamp, spec.Fence.ClosedTS)
		state.Scales[i] = version
	}
	if err := builder.writeState(ctx, state); err != nil {
		return nil, err
	}
	builder.used -= scaleBytes
	for position := basePosition; position < spec.Fence.LogPosition; {
		position++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := builder.link("commits/" + commitName(position)); err != nil {
			return nil, err
		}
	}
	for generation := pin.generation; generation != nil; generation = generation.parent {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if generation.baseGeneration != 0 {
			if err := builder.link("runs/" + baseManifestName(generation.baseGeneration)); err != nil {
				return nil, err
			}
		} else {
			if err := builder.link("runs/" + runManifestName(generation.position)); err != nil {
				return nil, err
			}
		}
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := builder.link("runs/" + file.descriptor.Name); err != nil {
				return nil, err
			}
		}
	}
	for _, name := range builder.extras {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := builder.link(name); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(handle.manifest.Files, func(a, b storage.CheckpointFile) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(handle.manifest.Files); i++ {
		if handle.manifest.Files[i-1].Name == handle.manifest.Files[i].Name {
			return nil, storage.ErrCorruptStorage
		}
	}
	return builder, nil
}
