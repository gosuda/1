package neosepia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	runManifestMagic       = "NSRUNS01"
	runManifestHeaderBytes = 160
	runManifestEntryBytes  = 80
)

type runDecision struct {
	Position      uint64
	TxID          model.TxnID
	CommitTS      model.Timestamp
	Identity      [32]byte
	MutationCount uint64
}

func (decision runDecision) validate() error {
	if decision.Position == 0 || decision.CommitTS <= 0 || decision.TxID == (model.TxnID{}) || decision.MutationCount == 0 {
		return storage.ErrInvalidMutation
	}
	return nil
}

func runManifestName(position uint64) string { return fmt.Sprintf("%020d.runs", position) }
func preparedRunName(position uint64, index int) string {
	return fmt.Sprintf("%020d-%08x.sst", position, index)
}

// The manifest is a pre-durability artifact, not the transaction decision. Only
// a matching validated WAL commit authorizes recovery of its file set.
func (p *preparedRuns) persistManifest(ctx context.Context, decision runDecision, maximum uint64) (err error) {
	if err := decision.validate(); err != nil {
		return err
	}
	if len(p.files) == 0 {
		return storage.ErrBatchEmpty
	}
	const nameBytes = 25
	if nameBytes > maximum-p.metadataBytes {
		return columnar.ErrBatchBudget
	}
	name := runManifestName(decision.Position)
	file, err := p.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	p.manifest = name
	p.metadataBytes += nameBytes
	defer func() { err = errors.Join(err, file.Close()) }()
	var header [runManifestHeaderBytes]byte
	if _, err := file.Write(header[:]); err != nil {
		return err
	}
	digest := sha256.New()
	var entry [runManifestEntryBytes]byte
	var rows uint64
	for index, run := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if run.Name != preparedRunName(decision.Position, index) || run.Rows == 0 || run.Rows > decision.MutationCount-rows || run.Bytes == 0 {
			return storage.ErrCorruptStorage
		}
		rows += run.Rows
		encodeRunEntry(entry[:], run)
		if _, err := file.Write(entry[:]); err != nil {
			return err
		}
		if _, err := digest.Write(entry[:]); err != nil {
			return err
		}
	}
	if rows != decision.MutationCount {
		return storage.ErrCorruptStorage
	}
	copy(header[:8], runManifestMagic)
	binary.LittleEndian.PutUint64(header[8:16], uint64(runManifestHeaderBytes)+uint64(len(p.files))*runManifestEntryBytes)
	binary.LittleEndian.PutUint64(header[16:24], decision.Position)
	binary.LittleEndian.PutUint64(header[24:32], uint64(decision.CommitTS))
	copy(header[32:48], decision.TxID[:])
	copy(header[48:80], decision.Identity[:])
	binary.LittleEndian.PutUint64(header[80:88], uint64(len(p.files)))
	digest.Sum(header[88:88:120])
	binary.LittleEndian.PutUint64(header[120:128], decision.MutationCount)
	checksum := sha256.Sum256(header[:128])
	copy(header[128:], checksum[:])
	if _, err := file.WriteAt(header[:], 0); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return file.Sync()
}

func encodeRunEntry(entry []byte, run preparedRun) {
	clear(entry)
	binary.LittleEndian.PutUint64(entry[0:8], uint64(run.Target.ObjectID))
	binary.LittleEndian.PutUint64(entry[8:16], run.Target.Generation)
	binary.LittleEndian.PutUint64(entry[16:24], run.Target.DescriptorVersion)
	binary.LittleEndian.PutUint32(entry[24:28], run.Target.EncodingVersion)
	binary.LittleEndian.PutUint64(entry[32:40], run.Rows)
	binary.LittleEndian.PutUint64(entry[40:48], run.Bytes)
	copy(entry[48:80], run.Checksum[:])
}

func loadRunManifest(ctx context.Context, root *os.Root, decision runDecision, limits runPreparationLimits) (_ []preparedRun, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := decision.validate(); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, storage.ErrClosed
	}
	file, err := root.Open(runManifestName(decision.Position))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var header [runManifestHeaderBytes]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	checksum := sha256.Sum256(header[:128])
	if string(header[:8]) != runManifestMagic || !bytes.Equal(header[128:], checksum[:]) || binary.LittleEndian.Uint64(header[120:128]) != decision.MutationCount {
		return nil, storage.ErrCorruptStorage
	}
	position := binary.LittleEndian.Uint64(header[16:24])
	timestamp := binary.LittleEndian.Uint64(header[24:32])
	if position != decision.Position || timestamp != uint64(decision.CommitTS) || !bytes.Equal(header[32:48], decision.TxID[:]) || !bytes.Equal(header[48:80], decision.Identity[:]) {
		return nil, storage.ErrCorruptStorage
	}
	count := binary.LittleEndian.Uint64(header[80:88])
	if count == 0 || count > limits.MaxRuns || count > min(uint64(math.MaxInt), uint64(math.MaxUint32)) || count > (math.MaxUint64-runManifestHeaderBytes)/runManifestEntryBytes {
		return nil, storage.ErrDecodeBudget
	}
	size := uint64(runManifestHeaderBytes) + count*runManifestEntryBytes
	if binary.LittleEndian.Uint64(header[8:16]) != size {
		return nil, storage.ErrCorruptStorage
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 0 || uint64(info.Size()) != size {
		return nil, storage.ErrCorruptStorage
	}
	fixed := uint64(runManifestHeaderBytes + runManifestEntryBytes)
	if fixed > limits.MetadataBytes || count > (limits.MetadataBytes-fixed)/(uint64(unsafe.Sizeof(preparedRun{}))+33) {
		return nil, columnar.ErrBatchBudget
	}
	files := make([]preparedRun, int(count))
	digest := sha256.New()
	var entry [runManifestEntryBytes]byte
	var rows uint64
	for index := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(file, entry[:]); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		if _, err := digest.Write(entry[:]); err != nil {
			return nil, err
		}
		if binary.LittleEndian.Uint32(entry[28:32]) != 0 {
			return nil, storage.ErrCorruptStorage
		}
		run := preparedRun{Name: preparedRunName(position, index), Target: storage.ObjectRef{ObjectID: model.ObjectID(binary.LittleEndian.Uint64(entry[0:8])), Generation: binary.LittleEndian.Uint64(entry[8:16]), DescriptorVersion: binary.LittleEndian.Uint64(entry[16:24]), EncodingVersion: binary.LittleEndian.Uint32(entry[24:28])}, Rows: binary.LittleEndian.Uint64(entry[32:40]), Bytes: binary.LittleEndian.Uint64(entry[40:48])}
		copy(run.Checksum[:], entry[48:80])
		if err := run.Target.Validate(); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		if run.Rows == 0 || run.Rows > decision.MutationCount-rows || run.Bytes == 0 || run.Bytes > limits.File.MaxBytes {
			return nil, storage.ErrCorruptStorage
		}
		rows += run.Rows
		files[index] = run
	}
	var bodyChecksum [32]byte
	digest.Sum(bodyChecksum[:0])
	if rows != decision.MutationCount || !bytes.Equal(bodyChecksum[:], header[88:120]) {
		return nil, storage.ErrCorruptStorage
	}
	return files, nil
}
