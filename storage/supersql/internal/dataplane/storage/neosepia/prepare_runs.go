package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type preparedRun struct {
	Name        string
	Target      storage.ObjectRef
	Rows, Bytes uint64
	Checksum    [32]byte
	LowerKey, UpperKey []byte
}

type runPreparationLimits struct {
	MaxBuilders   int
	MaxRuns       uint64
	MetadataBytes uint64
	Chunk         mutationBlockLimits
	File          sstLimits
}

type targetBuilder struct {
	block       *mutationBlockBuilder
	lastOrdinal uint64
}

// The directory is borrowed. Close removes only files this preparation created;
// detach transfers those files and their metadata to the publication owner.
type preparedRuns struct {
	root          *os.Root
	files         []preparedRun
	manifest      string
	metadataBytes uint64
	detached      bool
}

// Each builder has its own Chunk.BuildBytes bound. Encoding and sorting are
// serial and reuse their separate quotas; metadata includes the builder pool,
// retained filenames and one checksum buffer. No transaction-sized row map exists.
// The caller supplies the identity of the validated immutable normalized batch.
func prepareMutationRuns(ctx context.Context, root *os.Root, position uint64, batch storage.MutationBatch, identity [32]byte, limits runPreparationLimits) (_ *preparedRuns, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root == nil || position == 0 {
		return nil, storage.ErrInvalidTarget
	}
	if limits.MaxBuilders <= 0 || limits.MaxRuns == 0 || limits.MaxRuns > min(uint64(math.MaxUint32), uint64(math.MaxInt)) || limits.Chunk.Block != limits.File.Block {
		return nil, storage.ErrInvalidLayout
	}
	fixed := uint64(unsafe.Sizeof(preparedRuns{})) + (32 << 10) + runManifestHeaderBytes + runManifestEntryBytes
	if fixed > limits.MetadataBytes || uint64(limits.MaxBuilders) > (limits.MetadataBytes-fixed)/uint64(unsafe.Sizeof(targetBuilder{})) {
		return nil, columnar.ErrBatchBudget
	}
	fixed += uint64(limits.MaxBuilders) * uint64(unsafe.Sizeof(targetBuilder{}))
	prepared := &preparedRuns{root: root, metadataBytes: fixed}
	builders := make([]targetBuilder, limits.MaxBuilders)
	defer func() {
		for i := range builders {
			if builders[i].block != nil {
				err = errors.Join(err, builders[i].block.Close())
			}
		}
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()
	cursor, err := batch.OpenMutations(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	scratch := make([]byte, 32<<10)
	for {
		mutation, nextErr := cursor.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, nextErr
		}
		slot := 0
		found := false
		for i := range builders {
			if builders[i].block != nil && builders[i].block.layout.Target == mutation.Target {
				slot = i
				found = true
				break
			}
			if builders[i].block == nil || builders[i].lastOrdinal < builders[slot].lastOrdinal {
				slot = i
			}
		}
		if !found {
			if builders[slot].block != nil {
				if err := prepared.flush(ctx, position, builders[slot].block, limits, scratch); err != nil {
					return nil, err
				}
				if err := builders[slot].block.Close(); err != nil {
					return nil, err
				}
				builders[slot].block = nil
			}
			layout, ok := batch.Layout(mutation.Target)
			if !ok {
				return nil, storage.ErrInvalidLayout
			}
			builders[slot].block, err = newMutationBlockBuilder(ctx, layout, batch.CommitTS, limits.Chunk)
			if err != nil {
				return nil, err
			}
		}
		builder := builders[slot].block
		appendErr := builder.Append(ctx, mutation)
		if errors.Is(appendErr, columnar.ErrBatchBudget) && builder.Rows() != 0 {
			if err := prepared.flush(ctx, position, builder, limits, scratch); err != nil {
				return nil, err
			}
			appendErr = builder.Append(ctx, mutation)
		}
		if appendErr != nil {
			return nil, appendErr
		}
		builders[slot].lastOrdinal = mutation.Ordinal
	}
	for i := range builders {
		if builders[i].block != nil {
			if err := prepared.flush(ctx, position, builders[i].block, limits, scratch); err != nil {
				return nil, err
			}
		}
	}
	if len(prepared.files) == 0 {
		return nil, storage.ErrBatchEmpty
	}
	decision := runDecision{Position: position, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}
	if err := prepared.persistManifest(ctx, decision, limits.MetadataBytes); err != nil {
		return nil, err
	}
	if err := syncCommitDirectory(root); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (p *preparedRuns) flush(ctx context.Context, position uint64, builder *mutationBlockBuilder, limits runPreparationLimits, scratch []byte) error {
	count := builder.Rows()
	if count == 0 {
		return nil
	}
	if uint64(len(p.files)) == limits.MaxRuns {
		return columnar.ErrBatchBudget
	}
	if len(p.files) == cap(p.files) {
		capacity := min(max(4, len(p.files)+1, min(math.MaxInt/2, cap(p.files))*2), int(limits.MaxRuns))
		bytes := uint64(capacity) * uint64(unsafe.Sizeof(preparedRun{}))
		if bytes > limits.MetadataBytes-p.metadataBytes {
			return columnar.ErrBatchBudget
		}
		next := make([]preparedRun, len(p.files), capacity)
		copy(next, p.files)
		p.metadataBytes += uint64(capacity-cap(p.files)) * uint64(unsafe.Sizeof(preparedRun{}))
		p.files = next
	}
	const nameBytes = 33
	if nameBytes > limits.MetadataBytes-p.metadataBytes {
		return columnar.ErrBatchBudget
	}
	name := preparedRunName(position, len(p.files))
	data, err := builder.Flush(ctx)
	if err != nil {
		return err
	}
	file, err := p.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	p.files = append(p.files, preparedRun{Name: name, Target: builder.layout.Target, Rows: uint64(count)})
	p.metadataBytes += nameBytes
	descriptor, err := writePreparedRun(ctx, file, builder.layout.Target, data, limits.File, scratch)
	if err != nil {
		return err
	}
	descriptor.Name = name
	descriptor.Rows = uint64(count)
	p.files[len(p.files)-1] = descriptor
	return nil
}

func writePreparedRun(ctx context.Context, file *os.File, target storage.ObjectRef, data []byte, limits sstLimits, scratch []byte) (_ preparedRun, err error) {
	defer func() { err = errors.Join(err, file.Close()) }()
	writer, err := newSSTWriter(file, target, limits)
	if err != nil {
		return preparedRun{}, err
	}
	if err := writer.Append(ctx, data); err != nil {
		return preparedRun{}, err
	}
	if err := writer.Finish(ctx); err != nil {
		return preparedRun{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return preparedRun{}, err
	}
	checksum, err := checksumRunFile(ctx, file, info.Size(), scratch)
	if err != nil {
		return preparedRun{}, err
	}
	return preparedRun{Target: target, Bytes: uint64(info.Size()), Checksum: checksum}, nil
}

func (p *preparedRuns) detach() []preparedRun {
	files := p.files
	p.files = nil
	p.detached = true
	return files
}

func (p *preparedRuns) Close() error {
	if p.detached {
		return nil
	}
	p.detached = true
	var err error
	for _, file := range p.files {
		err = errors.Join(err, p.root.Remove(file.Name))
	}
	if p.manifest != "" {
		err = errors.Join(err, p.root.Remove(p.manifest))
		p.manifest = ""
	}
	p.files = nil
	return err
}
