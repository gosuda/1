package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type checkpointBuilder struct {
	handle *checkpointHandle
	limits Limits
	extras []string
	used   uint64
}

// validateCheckpointExtras admits only plain regular-file names at the database
// root: no separators, no hidden or reserved entries. Output is sorted so
// manifest order is deterministic.
func validateCheckpointExtras(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	extras := slices.Clone(names)
	slices.Sort(extras)
	for i, name := range extras {
		if i > 0 && extras[i-1] == name {
			return nil, ErrInvalidConfig
		}
		switch {
		case name == "" || name == "." || name == "..", strings.Contains(name, "/"), strings.HasPrefix(name, "."):
			return nil, ErrInvalidConfig
		case name == databaseStateName || name == "commits" || name == "runs" || name == "backup.manifest":
			return nil, ErrInvalidConfig
		case len(name) > 255:
			return nil, ErrInvalidConfig
		}
	}
	return extras, nil
}

func (builder *checkpointBuilder) reserve(bytes uint64) error {
	if builder.used > builder.limits.MetadataBytes || bytes > builder.limits.MetadataBytes-builder.used {
		return columnar.ErrBatchBudget
	}
	builder.used += bytes
	return nil
}

func (builder *checkpointBuilder) add(file storage.CheckpointFile) (int, error) {
	if err := builder.reserve(uint64(len(file.Name))); err != nil {
		return 0, err
	}
	files := builder.handle.manifest.Files
	if len(files) == cap(files) {
		if cap(files) > math.MaxInt/2 {
			return 0, columnar.ErrBatchBudget
		}
		capacity := max(8, cap(files)*2)
		unit := uint64(unsafe.Sizeof(storage.CheckpointFile{}))
		if uint64(capacity) > (builder.limits.MetadataBytes-builder.used)/unit {
			return 0, columnar.ErrBatchBudget
		}
		grown := make([]storage.CheckpointFile, len(files), capacity)
		copy(grown, files)
		builder.used += uint64(capacity-cap(files)) * unit
		files = grown
	}
	index := len(files)
	builder.handle.manifest.Files = append(files, file)
	return index, nil
}

func (builder *checkpointBuilder) link(name string) error {
	index, err := builder.add(storage.CheckpointFile{Name: name})
	if err != nil {
		return err
	}
	handle := builder.handle
	if err := handle.parent.Link(name, handle.directory+"/"+name); err != nil {
		return err
	}
	info, err := handle.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return storage.ErrCorruptStorage
	}
	handle.manifest.Files[index].Size = uint64(info.Size())
	return nil
}

func (builder *checkpointBuilder) writeState(ctx context.Context, state databaseState) error {
	data, err := encodeDatabaseState(ctx, state, builder.limits.MetadataBytes-builder.used)
	if err != nil {
		return err
	}
	if err := builder.reserve(uint64(len(data))); err != nil {
		return err
	}
	defer func() { builder.used -= uint64(len(data)) }()
	checksum, err := checksumStateBytes(ctx, data)
	if err != nil {
		return err
	}
	if _, err := builder.add(storage.CheckpointFile{Name: databaseStateName, Size: uint64(len(data)), SHA256: checksum}); err != nil {
		return err
	}
	return writeCheckpointBytes(ctx, builder.handle.root, databaseStateName, data)
}

func writeCheckpointBytes(ctx context.Context, root *os.Root, name string, data []byte) (err error) {
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + min(len(data)-offset, 64<<10)
		count, err := file.Write(data[offset:end])
		if err != nil {
			return err
		}
		if count != end-offset {
			return io.ErrShortWrite
		}
		offset = end
	}
	return file.Sync()
}

func (builder *checkpointBuilder) index(name string) (int, error) {
	index, found := slices.BinarySearchFunc(builder.handle.manifest.Files, name, func(file storage.CheckpointFile, name string) int { return strings.Compare(file.Name, name) })
	if !found {
		return 0, storage.ErrCorruptStorage
	}
	return index, nil
}

func (builder *checkpointBuilder) expect(name string, size uint64, checksum [32]byte, seen []bool) error {
	index, err := builder.index(name)
	if err != nil {
		return err
	}
	file := &builder.handle.manifest.Files[index]
	if seen[index] || file.Size != size {
		return storage.ErrCorruptStorage
	}
	file.SHA256 = checksum
	seen[index] = true
	return nil
}

func (builder *checkpointBuilder) hashEntry(ctx context.Context, root *os.Root, name, manifestName string, known *storage.CheckpointFile, scratch []byte, seen []bool) (err error) {
	index, err := builder.index(manifestName)
	if err != nil {
		return err
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	expected := builder.handle.manifest.Files[index]
	if known != nil && (known.Name != manifestName || known.Size != expected.Size) {
		return storage.ErrCorruptStorage
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) != expected.Size {
		return storage.ErrCorruptStorage
	}
	checksum, err := checksumRunFile(ctx, file, info.Size(), scratch)
	if err != nil {
		return err
	}
	if known != nil && checksum != known.SHA256 {
		return storage.ErrCorruptStorage
	}
	return builder.expect(manifestName, expected.Size, checksum, seen)
}

func (builder *checkpointBuilder) finish(ctx context.Context) (err error) {
	handle := builder.handle
	if err := builder.reserve(uint64(len(handle.manifest.Files))); err != nil {
		return err
	}
	seen := make([]bool, len(handle.manifest.Files))
	metadata, err := builder.index(databaseStateName)
	if err != nil {
		return err
	}
	seen[metadata] = true
	scratchBytes := min(uint64(64<<10), builder.limits.MetadataBytes-builder.used)
	if scratchBytes == 0 {
		return columnar.ErrBatchBudget
	}
	scratch := make([]byte, int(scratchBytes))
	commits, err := handle.root.OpenRoot("commits")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, commits.Close()) }()
	runs, err := handle.root.OpenRoot("runs")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runs.Close()) }()
	for generation := handle.pin.generation; generation != nil; generation = generation.parent {
		if generation.baseGeneration != 0 {
			if err := builder.finishBase(ctx, runs, generation, scratch, seen); err != nil {
				return err
			}
			continue
		}
		if err := builder.finishCommit(ctx, commits, runs, generation.position, scratch, seen); err != nil {
			return err
		}
	}
	for _, name := range builder.extras {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := builder.hashEntry(ctx, handle.root, name, name, nil, scratch, seen); err != nil {
			return err
		}
	}
	for _, complete := range seen {
		if !complete {
			return storage.ErrCorruptStorage
		}
	}
	if err := syncCommitDirectory(commits); err != nil {
		return err
	}
	if err := syncCommitDirectory(runs); err != nil {
		return err
	}
	return syncCommitDirectory(handle.root)
}

func (builder *checkpointBuilder) finishCommit(ctx context.Context, commits, runs *os.Root, position uint64, scratch []byte, seen []bool) (err error) {
	record, err := openCommit(ctx, commits, position, builder.limits.recovery().Batch)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, record.Close()) }()
	decision := runDecision{Position: position, TxID: record.Batch.TxID, CommitTS: record.Result.CommitTS, Identity: record.Identity, MutationCount: record.Result.AppliedCount}
	files, err := loadRunManifest(ctx, runs, decision, builder.limits.preparation())
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		known := storage.CheckpointFile{Name: "runs/" + file.Name, Size: file.Bytes, SHA256: file.Checksum}
		if err := builder.hashEntry(ctx, runs, file.Name, known.Name, &known, scratch, seen); err != nil {
			return err
		}
	}
	if err := builder.hashEntry(ctx, commits, commitName(position), "commits/"+commitName(position), nil, scratch, seen); err != nil {
		return err
	}
	return builder.hashEntry(ctx, runs, runManifestName(position), "runs/"+runManifestName(position), nil, scratch, seen)
}

func (builder *checkpointBuilder) finishBase(ctx context.Context, runs *os.Root, generation *runGeneration, scratch []byte, seen []bool) error {
	base, err := loadRunBase(ctx, runs, generation.baseGeneration, builder.limits.MetadataBytes, builder.limits.IdentityBytes, builder.limits.MaxFiles)
	if err != nil {
		return err
	}
	if base.Position != generation.position || base.CommitTS != generation.timestamp || base.Floor != builder.handle.manifest.HistoryFloor {
		return storage.ErrCorruptStorage
	}
	for _, file := range base.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		known := storage.CheckpointFile{Name: "runs/" + file.Name, Size: file.Bytes, SHA256: file.Checksum}
		if err := builder.hashEntry(ctx, runs, file.Name, known.Name, &known, scratch, seen); err != nil {
			return err
		}
	}
	name := baseManifestName(base.Generation)
	return builder.hashEntry(ctx, runs, name, "runs/"+name, nil, scratch, seen)
}
