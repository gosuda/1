package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"os"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type envelopeWriter struct {
	ctx       context.Context
	out       io.Writer
	digest    hash.Hash
	scratch   []byte
	remaining uint64
	err       error
}

func (writer *envelopeWriter) bytes(data []byte) {
	if writer.err != nil {
		return
	}
	if uint64(len(data)) > writer.remaining {
		writer.err = ErrLimit
		return
	}
	for len(data) != 0 {
		if writer.err = writer.ctx.Err(); writer.err != nil {
			return
		}
		part := data[:min(len(data), len(writer.scratch))]
		n, err := writer.out.Write(part)
		if n != len(part) {
			err = errors.Join(err, io.ErrShortWrite)
		}
		if err != nil {
			writer.err = err
			return
		}
		_, _ = writer.digest.Write(part)
		writer.remaining -= uint64(len(part))
		data = data[len(part):]
	}
}

func (writer *envelopeWriter) number(value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	writer.bytes(data[:])
}

func (writer *envelopeWriter) text(value string) {
	writer.number(uint64(len(value)))
	for len(value) != 0 && writer.err == nil {
		n := copy(writer.scratch, value)
		writer.bytes(writer.scratch[:n])
		value = value[n:]
	}
}

func writeEnvelope(ctx context.Context, out io.Writer, manifest storage.CheckpointManifest, size uint64, scratch []byte) error {
	writer := envelopeWriter{ctx: ctx, out: out, digest: sha256.New(), scratch: scratch, remaining: size - sha256.Size}
	writer.bytes([]byte(envelopeMagic))
	writer.number(uint64(manifest.FormatVersion))
	writer.text(manifest.Backend)
	writer.number(uint64(manifest.SnapshotTS))
	writer.number(uint64(manifest.HistoryFloor))
	writer.bytes(manifest.Fence.ID[:])
	writer.number(uint64(manifest.Fence.ClosedTS))
	writer.number(manifest.Fence.LogPosition)
	writer.bytes(manifest.Fence.Epoch.Assurance[:])
	writer.bytes(manifest.Fence.Epoch.LeapHistory[:])
	writer.bytes(manifest.Fence.Epoch.Config[:])
	writer.text(manifest.Scale.Name)
	writer.bytes(manifest.Scale.LeapHistoryID[:])
	writer.number(uint64(len(manifest.Scale.CanonicalLeapHistory)))
	writer.bytes(manifest.Scale.CanonicalLeapHistory)
	writer.number(uint64(len(manifest.Files)))
	for _, file := range manifest.Files {
		writer.text(file.Name)
		writer.number(file.Size)
		writer.bytes(file.SHA256[:])
		if writer.err != nil {
			return writer.err
		}
	}
	if writer.err != nil {
		return writer.err
	}
	if writer.remaining != 0 {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var sum [sha256.Size]byte
	writer.digest.Sum(sum[:0])
	n, err := out.Write(sum[:])
	if n != len(sum) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}

type envelopeReader struct {
	ctx                        context.Context
	in                         io.Reader
	digest                     hash.Hash
	scratch                    []byte
	limits                     Limits
	remaining, metadata, names uint64
	err                        error
}

func (reader *envelopeReader) bytes(data []byte) {
	if reader.err != nil {
		return
	}
	if uint64(len(data)) > reader.remaining {
		reader.err = ErrCorrupt
		return
	}
	for len(data) != 0 {
		if reader.err = reader.ctx.Err(); reader.err != nil {
			return
		}
		part := data[:min(len(data), len(reader.scratch))]
		if _, reader.err = io.ReadFull(reader.in, part); reader.err != nil {
			reader.err = errors.Join(ErrCorrupt, reader.err)
			return
		}
		_, _ = reader.digest.Write(part)
		reader.remaining -= uint64(len(part))
		data = data[len(part):]
	}
}

func (reader *envelopeReader) number() uint64 {
	var data [8]byte
	reader.bytes(data[:])
	return binary.BigEndian.Uint64(data[:])
}

func (reader *envelopeReader) text() string {
	size := reader.number()
	if reader.err != nil {
		return ""
	}
	if size > reader.remaining {
		reader.err = ErrCorrupt
		return ""
	}
	if size == 0 || size > reader.limits.MaxNameBytes {
		reader.err = ErrLimit
		return ""
	}
	if reader.err = admit(&reader.names, size, reader.limits.MaxNamesBytes); reader.err != nil {
		return ""
	}
	if reader.err = admit(&reader.metadata, size, reader.limits.MaxMetadataBytes); reader.err != nil {
		return ""
	}
	var value strings.Builder
	value.Grow(int(size))
	for size != 0 && reader.err == nil {
		part := reader.scratch[:int(min(size, uint64(len(reader.scratch))))]
		reader.bytes(part)
		if reader.err != nil {
			return ""
		}
		_, _ = value.Write(part)
		size -= uint64(len(part))
	}
	return value.String()
}

func readEnvelope(ctx context.Context, file *os.File, limits Limits, scratch []byte) (storage.CheckpointManifest, error) {
	info, err := file.Stat()
	if err != nil {
		return storage.CheckpointManifest{}, err
	}
	if info.Size() < int64(envelopeFixedBytes) {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	if uint64(info.Size()) > limits.MaxManifestBytes {
		return storage.CheckpointManifest{}, ErrLimit
	}
	reader := envelopeReader{ctx: ctx, in: file, digest: sha256.New(), scratch: scratch, limits: limits, remaining: uint64(info.Size()) - sha256.Size}
	if err := admit(&reader.metadata, uint64(unsafe.Sizeof(storage.CheckpointManifest{})), limits.MaxMetadataBytes); err != nil {
		return storage.CheckpointManifest{}, err
	}
	var magic [len(envelopeMagic)]byte
	reader.bytes(magic[:])
	version := reader.number()
	if reader.err != nil {
		return storage.CheckpointManifest{}, reader.err
	}
	if string(magic[:]) != envelopeMagic || version != 1 {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	manifest := storage.CheckpointManifest{FormatVersion: uint32(version)}
	manifest.Backend = reader.text()
	manifest.SnapshotTS = model.Timestamp(reader.number())
	manifest.HistoryFloor = model.Timestamp(reader.number())
	reader.bytes(manifest.Fence.ID[:])
	manifest.Fence.ClosedTS = model.Timestamp(reader.number())
	manifest.Fence.LogPosition = reader.number()
	reader.bytes(manifest.Fence.Epoch.Assurance[:])
	reader.bytes(manifest.Fence.Epoch.LeapHistory[:])
	reader.bytes(manifest.Fence.Epoch.Config[:])
	manifest.Scale.Name = reader.text()
	reader.bytes(manifest.Scale.LeapHistoryID[:])
	size := reader.number()
	if reader.err != nil {
		return storage.CheckpointManifest{}, reader.err
	}
	if size > reader.remaining {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	if err := admit(&reader.metadata, size, limits.MaxMetadataBytes); err != nil {
		return storage.CheckpointManifest{}, err
	}
	manifest.Scale.CanonicalLeapHistory = make([]byte, int(size))
	reader.bytes(manifest.Scale.CanonicalLeapHistory)
	count := reader.number()
	if reader.err != nil {
		return storage.CheckpointManifest{}, reader.err
	}
	if count > reader.remaining/fileFixedBytes {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	entryBytes := uint64(unsafe.Sizeof(storage.CheckpointFile{}))
	if count > limits.MaxFiles || count > (limits.MaxMetadataBytes-reader.metadata)/entryBytes {
		return storage.CheckpointManifest{}, ErrLimit
	}
	reader.metadata += count * entryBytes
	manifest.Files = make([]storage.CheckpointFile, int(count))
	for i := range manifest.Files {
		entry := &manifest.Files[i]
		entry.Name = reader.text()
		entry.Size = reader.number()
		reader.bytes(entry.SHA256[:])
		if reader.err != nil {
			return storage.CheckpointManifest{}, reader.err
		}
	}
	if reader.remaining != 0 {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	var expected, actual [sha256.Size]byte
	if _, err := io.ReadFull(file, expected[:]); err != nil {
		return storage.CheckpointManifest{}, errors.Join(ErrCorrupt, err)
	}
	reader.digest.Sum(actual[:0])
	if expected != actual {
		return storage.CheckpointManifest{}, ErrCorrupt
	}
	if err := requireEOF(ctx, file); err != nil {
		return storage.CheckpointManifest{}, err
	}
	if _, err := validateManifest(ctx, manifest, limits); err != nil {
		return storage.CheckpointManifest{}, err
	}
	return manifest, nil
}
