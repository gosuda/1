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
	runManifestMagicV2     = "NSRUNS02"
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

type coalescedDecision struct {
	FirstPosition uint64
	LastPosition  uint64
	FirstCommitTS model.Timestamp
	LastCommitTS  model.Timestamp
	Identity      [32]byte
	MutationCount uint64
}

func (decision runDecision) validate() error {
	if decision.Position == 0 || decision.CommitTS <= 0 || decision.TxID == (model.TxnID{}) || decision.MutationCount == 0 {
		return storage.ErrInvalidMutation
	}
	return nil
}

func makeCoalescedDecision(decisions []runDecision) (coalescedDecision, error) {
	if len(decisions) == 0 {
		return coalescedDecision{}, storage.ErrBatchEmpty
	}
	digest := sha256.New()
	var buf [72]byte
	var total uint64
	for i, d := range decisions {
		if err := d.validate(); err != nil {
			return coalescedDecision{}, err
		}
		if i > 0 {
			if d.Position != decisions[i-1].Position+1 || d.CommitTS <= decisions[i-1].CommitTS {
				return coalescedDecision{}, storage.ErrCorruptStorage
			}
		}
		if d.MutationCount > math.MaxUint64-total {
			return coalescedDecision{}, storage.ErrCorruptStorage
		}
		total += d.MutationCount
		binary.LittleEndian.PutUint64(buf[0:8], d.Position)
		binary.LittleEndian.PutUint64(buf[8:16], uint64(d.CommitTS))
		copy(buf[16:32], d.TxID[:])
		copy(buf[32:64], d.Identity[:])
		binary.LittleEndian.PutUint64(buf[64:72], d.MutationCount)
		digest.Write(buf[:])
	}
	var agg [32]byte
	digest.Sum(agg[:0])
	first := decisions[0]
	last := decisions[len(decisions)-1]
	return coalescedDecision{
		FirstPosition: first.Position,
		LastPosition:  last.Position,
		FirstCommitTS: first.CommitTS,
		LastCommitTS:  last.CommitTS,
		Identity:      agg,
		MutationCount: total,
	}, nil
}

func runManifestName(position uint64) string { return fmt.Sprintf("%020d.runs", position) }
func runManifestPendingName(position uint64) string {
	return fmt.Sprintf(".%020d.runs.pending", position)
}
func preparedRunName(position uint64, index int) string {
	return fmt.Sprintf("%020d-%08x.sst", position, index)
}

// The manifest is a pre-durability artifact, not the transaction decision. Only
// a matching validated WAL commit authorizes recovery of its file set.
func (p *preparedRuns) persistManifest(ctx context.Context, decision runDecision, maximum uint64) error {
	if err := decision.validate(); err != nil {
		return err
	}
	return p.writeManifestAtomic(ctx, decision.Position, maximum, decision.MutationCount, func(header *[runManifestHeaderBytes]byte, bodyDigest [32]byte) {
		copy(header[:8], runManifestMagic)
		binary.LittleEndian.PutUint64(header[8:16], uint64(runManifestHeaderBytes)+uint64(len(p.files))*runManifestEntryBytes)
		binary.LittleEndian.PutUint64(header[16:24], decision.Position)
		binary.LittleEndian.PutUint64(header[24:32], uint64(decision.CommitTS))
		copy(header[32:48], decision.TxID[:])
		copy(header[48:80], decision.Identity[:])
		binary.LittleEndian.PutUint64(header[80:88], uint64(len(p.files)))
		copy(header[88:120], bodyDigest[:])
		binary.LittleEndian.PutUint64(header[120:128], decision.MutationCount)
	})
}

// persistCoalescedManifest writes an NSRUNS02 manifest covering contiguous
// committed positions [decision.FirstPosition .. decision.LastPosition].
func (p *preparedRuns) persistCoalescedManifest(ctx context.Context, decision coalescedDecision, maximum uint64) error {
	if decision.FirstPosition == 0 || decision.LastPosition < decision.FirstPosition || decision.FirstCommitTS <= 0 || decision.LastCommitTS < decision.FirstCommitTS || decision.MutationCount == 0 {
		return storage.ErrInvalidMutation
	}
	return p.writeManifestAtomic(ctx, decision.LastPosition, maximum, decision.MutationCount, func(header *[runManifestHeaderBytes]byte, bodyDigest [32]byte) {
		copy(header[:8], runManifestMagicV2)
		binary.LittleEndian.PutUint64(header[8:16], uint64(runManifestHeaderBytes)+uint64(len(p.files))*runManifestEntryBytes)
		binary.LittleEndian.PutUint64(header[16:24], decision.LastPosition)
		binary.LittleEndian.PutUint64(header[24:32], uint64(decision.LastCommitTS))
		binary.LittleEndian.PutUint64(header[32:40], decision.FirstPosition)
		binary.LittleEndian.PutUint64(header[40:48], uint64(decision.FirstCommitTS))
		copy(header[48:80], decision.Identity[:])
		binary.LittleEndian.PutUint64(header[80:88], uint64(len(p.files)))
		copy(header[88:120], bodyDigest[:])
		binary.LittleEndian.PutUint64(header[120:128], decision.MutationCount)
	})
}

func (p *preparedRuns) writeManifestAtomic(
	ctx context.Context,
	position uint64,
	maximum uint64,
	expectedRows uint64,
	buildHeader func(header *[runManifestHeaderBytes]byte, bodyDigest [32]byte),
) (err error) {
	if len(p.files) == 0 {
		return storage.ErrBatchEmpty
	}
	const nameBytes = 25
	if p.metadataBytes > maximum || nameBytes > maximum-p.metadataBytes {
		return columnar.ErrBatchBudget
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pendingName := runManifestPendingName(position)
	finalName := runManifestName(position)
	if err := p.root.Remove(pendingName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := p.root.OpenFile(pendingName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	pendingRemoved := false
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !pendingRemoved {
			if removeErr := p.root.Remove(pendingName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
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
		if run.Name != preparedRunName(position, index) || run.Rows == 0 || run.Rows > expectedRows-rows || run.Bytes == 0 {
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
	if rows != expectedRows {
		return storage.ErrCorruptStorage
	}
	var bodyDigest [32]byte
	digest.Sum(bodyDigest[:0])
	buildHeader(&header, bodyDigest)
	checksum := sha256.Sum256(header[:128])
	copy(header[128:], checksum[:])
	if _, err := file.WriteAt(header[:], 0); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.root.Link(pendingName, finalName); err != nil {
		return err
	}
	p.manifest = finalName
	p.metadataBytes += nameBytes
	removeErr := p.root.Remove(pendingName)
	pendingRemoved = true
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	return syncCommitDirectory(p.root)
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

type runManifestSpan struct {
	firstPosition uint64
	lastPosition  uint64
	coalesced     bool
}

func inspectRunManifest(ctx context.Context, root *os.Root, position uint64) (_ runManifestSpan, err error) {
	if err := ctx.Err(); err != nil {
		return runManifestSpan{}, err
	}
	file, err := root.Open(runManifestName(position))
	if err != nil {
		return runManifestSpan{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var header [runManifestHeaderBytes]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return runManifestSpan{}, errors.Join(storage.ErrCorruptStorage, err)
	}
	checksum := sha256.Sum256(header[:128])
	if !bytes.Equal(header[128:], checksum[:]) {
		return runManifestSpan{}, storage.ErrCorruptStorage
	}
	lastPos := binary.LittleEndian.Uint64(header[16:24])
	if lastPos != position {
		return runManifestSpan{}, storage.ErrCorruptStorage
	}
	switch string(header[:8]) {
	case runManifestMagic:
		return runManifestSpan{firstPosition: position, lastPosition: position, coalesced: false}, nil
	case runManifestMagicV2:
		firstPos := binary.LittleEndian.Uint64(header[32:40])
		if firstPos == 0 || firstPos > lastPos {
			return runManifestSpan{}, storage.ErrCorruptStorage
		}
		return runManifestSpan{firstPosition: firstPos, lastPosition: lastPos, coalesced: true}, nil
	default:
		return runManifestSpan{}, storage.ErrCorruptStorage
	}
}

func loadCoalescedRunManifest(ctx context.Context, root *os.Root, decision coalescedDecision, limits runPreparationLimits) (_ []preparedRun, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if decision.FirstPosition == 0 || decision.LastPosition < decision.FirstPosition || decision.FirstCommitTS <= 0 || decision.LastCommitTS < decision.FirstCommitTS || decision.MutationCount == 0 {
		return nil, storage.ErrInvalidMutation
	}
	if root == nil {
		return nil, storage.ErrClosed
	}
	file, err := root.Open(runManifestName(decision.LastPosition))
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var header [runManifestHeaderBytes]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	checksum := sha256.Sum256(header[:128])
	if string(header[:8]) != runManifestMagicV2 || !bytes.Equal(header[128:], checksum[:]) || binary.LittleEndian.Uint64(header[120:128]) != decision.MutationCount {
		return nil, storage.ErrCorruptStorage
	}
	lastPos := binary.LittleEndian.Uint64(header[16:24])
	lastTS := binary.LittleEndian.Uint64(header[24:32])
	firstPos := binary.LittleEndian.Uint64(header[32:40])
	firstTS := binary.LittleEndian.Uint64(header[40:48])
	if lastPos != decision.LastPosition || lastTS != uint64(decision.LastCommitTS) || firstPos != decision.FirstPosition || firstTS != uint64(decision.FirstCommitTS) || !bytes.Equal(header[48:80], decision.Identity[:]) {
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
		run := preparedRun{
			Name: preparedRunName(lastPos, index),
			Target: storage.ObjectRef{
				ObjectID:          model.ObjectID(binary.LittleEndian.Uint64(entry[0:8])),
				Generation:        binary.LittleEndian.Uint64(entry[8:16]),
				DescriptorVersion: binary.LittleEndian.Uint64(entry[16:24]),
				EncodingVersion:   binary.LittleEndian.Uint32(entry[24:28]),
			},
			Rows:  binary.LittleEndian.Uint64(entry[32:40]),
			Bytes: binary.LittleEndian.Uint64(entry[40:48]),
		}
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
