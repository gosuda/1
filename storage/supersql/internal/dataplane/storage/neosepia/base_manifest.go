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
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

const (
	baseManifestMagic      = "NSBASE03"
	baseManifestHeader     = 176
	baseManifestEntryBytes = 144
	baseTransactionBytes   = 80
	baseManifestPending    = ".base.pending"
)

type runBase struct {
	Generation   uint64
	Position     uint64
	CommitTS     model.Timestamp
	Floor        model.Timestamp
	Epoch        clock.Epoch
	Files        []preparedRun
	Transactions ordered.Tree[recoveryTxnID, transactionRecord]
}

type baseTransaction struct {
	id     recoveryTxnID
	record transactionRecord
}

func baseManifestName(generation uint64) string {
	return fmt.Sprintf("base-%020d.runs", generation)
}

func (base runBase) validate(maxFiles, identityBytes uint64) error {
	if base.Generation == 0 || base.Position == 0 || base.CommitTS <= 0 || base.Floor < 0 || uint64(len(base.Files)) > maxFiles {
		return storage.ErrCorruptStorage
	}
	if base.Epoch.Assurance == ([16]byte{}) || base.Epoch.LeapHistory == ([32]byte{}) || base.Epoch.Config == ([32]byte{}) {
		return storage.ErrCorruptStorage
	}
	if base.Transactions.Len() != base.Position || base.Transactions.NodeBytes() > identityBytes {
		return storage.ErrCorruptStorage
	}
	for _, file := range base.Files {
		if !validBaseRunName(file.Name) || file.Rows == 0 || file.Bytes == 0 {
			return storage.ErrCorruptStorage
		}
		if len(file.LowerKey) != 0 && len(file.UpperKey) != 0 && bytes.Compare(file.LowerKey, file.UpperKey) >= 0 {
			return storage.ErrCorruptStorage
		}
		if err := file.Target.Validate(); err != nil {
			return errors.Join(storage.ErrCorruptStorage, err)
		}
	}
	return nil
}

func validBaseRunName(name string) bool {
	return name != "" && len(name) < 48 && name != "." && name != ".." && !strings.ContainsAny(name, "/\\")
}

func baseManifestSize(fileCount, transactionCount, boundBytes, entryBytes, metadataBytes, identityBytes uint64) (uint64, error) {
	if entryBytes == 0 || fileCount > (math.MaxUint64-baseManifestHeader-sha256.Size)/entryBytes {
		return 0, columnar.ErrBatchBudget
	}
	metadata := uint64(baseManifestHeader+sha256.Size) + fileCount*entryBytes
	if metadata > metadataBytes || boundBytes > metadataBytes-metadata {
		return 0, columnar.ErrBatchBudget
	}
	metadata += boundBytes
	if metadata > metadataBytes || transactionCount > identityBytes/baseTransactionBytes {
		return 0, columnar.ErrBatchBudget
	}
	transactions := transactionCount * baseTransactionBytes
	if transactions > math.MaxUint64-metadata || metadata+transactions > uint64(math.MaxInt) {
		return 0, columnar.ErrBatchBudget
	}
	return metadata + transactions, nil
}

func encodeRunBase(ctx context.Context, base runBase, metadataBytes, identityBytes, maxFiles uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := base.validate(maxFiles, identityBytes); err != nil {
		return nil, err
	}
	var boundBytes uint64
	for _, file := range base.Files {
		for _, key := range [][]byte{file.LowerKey, file.UpperKey} {
			if uint64(len(key)) > math.MaxUint32 || uint64(len(key)) > metadataBytes-boundBytes {
				return nil, columnar.ErrBatchBudget
			}
			boundBytes += uint64(len(key))
		}
	}
	size, err := baseManifestSize(uint64(len(base.Files)), base.Transactions.Len(), boundBytes, baseManifestEntryBytes, metadataBytes, identityBytes)
	if err != nil {
		return nil, err
	}
	transactions := make([]baseTransaction, 0, int(base.Transactions.Len()))
	cursor := base.Transactions.First(false)
	for {
		id, record, ok := cursor.Next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		transactions = append(transactions, baseTransaction{id: id, record: record})
	}
	slices.SortFunc(transactions, func(a, b baseTransaction) int {
		if a.record.Result.LogPosition < b.record.Result.LogPosition {
			return -1
		}
		if a.record.Result.LogPosition > b.record.Result.LogPosition {
			return 1
		}
		return a.id.Compare(b.id)
	})
	var previousTS model.Timestamp
	for i, transaction := range transactions {
		if transaction.record.Result.LogPosition != uint64(i+1) || transaction.record.Result.CommitTS <= previousTS || transaction.record.Result.AppliedCount == 0 {
			return nil, storage.ErrCorruptStorage
		}
		previousTS = transaction.record.Result.CommitTS
	}
	if previousTS != base.CommitTS {
		return nil, storage.ErrCorruptStorage
	}
	data := make([]byte, int(size))
	copy(data, baseManifestMagic)
	binary.LittleEndian.PutUint64(data[8:16], size)
	binary.LittleEndian.PutUint64(data[16:24], base.Generation)
	binary.LittleEndian.PutUint64(data[24:32], base.Position)
	binary.LittleEndian.PutUint64(data[32:40], uint64(base.CommitTS))
	binary.LittleEndian.PutUint64(data[40:48], uint64(base.Floor))
	binary.LittleEndian.PutUint64(data[48:56], uint64(len(base.Files)))
	binary.LittleEndian.PutUint64(data[56:64], uint64(len(transactions)))
	copy(data[64:80], base.Epoch.Assurance[:])
	copy(data[80:112], base.Epoch.LeapHistory[:])
	copy(data[112:144], base.Epoch.Config[:])
	binary.LittleEndian.PutUint64(data[144:152], boundBytes)
	boundOffset := baseManifestHeader + len(base.Files)*baseManifestEntryBytes
	offset := baseManifestHeader
	for _, file := range base.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := data[offset : offset+baseManifestEntryBytes]
		copy(entry[:48], file.Name)
		binary.LittleEndian.PutUint64(entry[48:56], uint64(file.Target.ObjectID))
		binary.LittleEndian.PutUint64(entry[56:64], file.Target.Generation)
		binary.LittleEndian.PutUint64(entry[64:72], file.Target.DescriptorVersion)
		binary.LittleEndian.PutUint32(entry[72:76], file.Target.EncodingVersion)
		binary.LittleEndian.PutUint64(entry[80:88], file.Rows)
		binary.LittleEndian.PutUint64(entry[88:96], file.Bytes)
		copy(entry[96:128], file.Checksum[:])
		binary.LittleEndian.PutUint32(entry[128:132], uint32(len(file.LowerKey)))
		binary.LittleEndian.PutUint32(entry[132:136], uint32(len(file.UpperKey)))
		binary.LittleEndian.PutUint64(entry[136:144], uint64(boundOffset))
		boundOffset += copy(data[boundOffset:], file.LowerKey)
		boundOffset += copy(data[boundOffset:], file.UpperKey)
		offset += baseManifestEntryBytes
	}
	offset = boundOffset
	for _, transaction := range transactions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry := data[offset : offset+baseTransactionBytes]
		copy(entry[:16], transaction.id[:])
		copy(entry[16:48], transaction.record.Identity[:])
		binary.LittleEndian.PutUint64(entry[48:56], transaction.record.Result.AppliedCount)
		binary.LittleEndian.PutUint64(entry[56:64], uint64(transaction.record.Result.CommitTS))
		binary.LittleEndian.PutUint32(entry[64:68], transaction.record.Result.ChecksumCRC)
		binary.LittleEndian.PutUint64(entry[72:80], transaction.record.Result.LogPosition)
		offset += baseTransactionBytes
	}
	digest := sha256.Sum256(data[:offset])
	copy(data[offset:], digest[:])
	return data, nil
}

func decodeRunBase(ctx context.Context, data []byte, expectedGeneration, metadataBytes, identityBytes, maxFiles uint64) (runBase, error) {
	if err := ctx.Err(); err != nil {
		return runBase{}, err
	}
	if len(data) < baseManifestHeader+sha256.Size || binary.LittleEndian.Uint64(data[8:16]) != uint64(len(data)) {
		return runBase{}, storage.ErrCorruptStorage
	}
	fileCount := binary.LittleEndian.Uint64(data[48:56])
	transactionCount := binary.LittleEndian.Uint64(data[56:64])
	entryBytes, boundBytes, err := runBaseFormat(data[:baseManifestHeader])
	if err != nil { return runBase{}, err }
	size, err := baseManifestSize(fileCount, transactionCount, boundBytes, entryBytes, metadataBytes, identityBytes)
	if err != nil {
		return runBase{}, err
	}
	if size != uint64(len(data)) || fileCount > maxFiles || fileCount > uint64(math.MaxInt) {
		return runBase{}, storage.ErrCorruptStorage
	}
	end := len(data) - sha256.Size
	if sha256.Sum256(data[:end]) != [32]byte(data[end:]) {
		return runBase{}, storage.ErrCorruptStorage
	}
	base := runBase{
		Generation: binary.LittleEndian.Uint64(data[16:24]),
		Position:   binary.LittleEndian.Uint64(data[24:32]),
		CommitTS:   model.Timestamp(binary.LittleEndian.Uint64(data[32:40])),
		Floor:      model.Timestamp(binary.LittleEndian.Uint64(data[40:48])),
		Epoch:      clock.Epoch{Assurance: [16]byte(data[64:80]), LeapHistory: [32]byte(data[80:112]), Config: [32]byte(data[112:144])},
		Files:      make([]preparedRun, int(fileCount)),
	}
	if base.Generation != expectedGeneration || transactionCount != base.Position {
		return runBase{}, storage.ErrCorruptStorage
	}
	offset := baseManifestHeader
	boundOffset := baseManifestHeader + int(fileCount*entryBytes)
	boundEnd := boundOffset + int(boundBytes)
	names := make(map[string]struct{}, len(base.Files))
	previousName := ""
	for i := range base.Files {
		if err := ctx.Err(); err != nil {
			return runBase{}, err
		}
		entry := data[offset : offset+int(entryBytes)]
		nameEnd := bytes.IndexByte(entry[:48], 0)
		if nameEnd < 0 {
			nameEnd = 48
		}
		for _, reserved := range entry[nameEnd:48] {
			if reserved != 0 {
				return runBase{}, storage.ErrCorruptStorage
			}
		}
		for _, reserved := range entry[76:80] {
			if reserved != 0 {
				return runBase{}, storage.ErrCorruptStorage
			}
		}
		file := &base.Files[i]
		file.Name = string(entry[:nameEnd])
		if _, exists := names[file.Name]; exists || (previousName != "" && file.Name < previousName) {
			return runBase{}, storage.ErrCorruptStorage
		}
		names[file.Name] = struct{}{}
		previousName = file.Name
		file.Target = storage.ObjectRef{ObjectID: model.ObjectID(binary.LittleEndian.Uint64(entry[48:56])), Generation: binary.LittleEndian.Uint64(entry[56:64]), DescriptorVersion: binary.LittleEndian.Uint64(entry[64:72]), EncodingVersion: binary.LittleEndian.Uint32(entry[72:76])}
		file.Rows = binary.LittleEndian.Uint64(entry[80:88])
		file.Bytes = binary.LittleEndian.Uint64(entry[88:96])
		file.Checksum = [32]byte(entry[96:128])
		if entryBytes == baseManifestEntryBytes {
			lowerBytes, upperBytes := uint64(binary.LittleEndian.Uint32(entry[128:132])), uint64(binary.LittleEndian.Uint32(entry[132:136]))
			if binary.LittleEndian.Uint64(entry[136:144]) != uint64(boundOffset) || lowerBytes+upperBytes > uint64(boundEnd-boundOffset) {
				return runBase{}, storage.ErrCorruptStorage
			}
			file.LowerKey = bytes.Clone(data[boundOffset:boundOffset+int(lowerBytes)])
			boundOffset += int(lowerBytes)
			file.UpperKey = bytes.Clone(data[boundOffset:boundOffset+int(upperBytes)])
			boundOffset += int(upperBytes)
		}
		offset += int(entryBytes)
	}
	if boundOffset != boundEnd {
		return runBase{}, storage.ErrCorruptStorage
	}
	offset = boundEnd
	var previousTS model.Timestamp
	for position := uint64(1); position <= transactionCount; position++ {
		if err := ctx.Err(); err != nil {
			return runBase{}, err
		}
		entry := data[offset : offset+baseTransactionBytes]
		for _, reserved := range entry[68:72] {
			if reserved != 0 {
				return runBase{}, storage.ErrCorruptStorage
			}
		}
		id := recoveryTxnID(entry[:16])
		record := transactionRecord{
			Identity: [32]byte(entry[16:48]),
			Result: storage.ApplyResult{
				AppliedCount: binary.LittleEndian.Uint64(entry[48:56]),
				CommitTS:     model.Timestamp(binary.LittleEndian.Uint64(entry[56:64])),
				ChecksumCRC:  binary.LittleEndian.Uint32(entry[64:68]),
				LogPosition:  binary.LittleEndian.Uint64(entry[72:80]),
			},
		}
		if id == (recoveryTxnID{}) || record.Identity == ([32]byte{}) || record.Result.LogPosition != position || record.Result.AppliedCount == 0 || record.Result.CommitTS <= previousTS {
			return runBase{}, storage.ErrCorruptStorage
		}
		next, _, err := base.Transactions.Set(id, record, identityBytes-base.Transactions.NodeBytes())
		if err != nil {
			return runBase{}, errors.Join(storage.ErrCorruptStorage, err)
		}
		if next.Len() != base.Transactions.Len()+1 {
			return runBase{}, errors.Join(storage.ErrCorruptStorage, storage.ErrTxnIDReused)
		}
		base.Transactions = next
		previousTS = record.Result.CommitTS
		offset += baseTransactionBytes
	}
	if offset != end || previousTS != base.CommitTS {
		return runBase{}, storage.ErrCorruptStorage
	}
	if err := base.validate(maxFiles, identityBytes); err != nil {
		return runBase{}, err
	}
	return base, nil
}

func writeRunBase(ctx context.Context, root *os.Root, base runBase, metadataBytes, identityBytes, maxFiles uint64) (err error) {
	data, err := encodeRunBase(ctx, base, metadataBytes, identityBytes, maxFiles)
	if err != nil {
		return err
	}
	if err := root.Remove(baseManifestPending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := root.OpenFile(baseManifestPending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if !published {
			removeErr := root.Remove(baseManifestPending)
			if !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + min(len(data)-offset, 64<<10)
		n, err := file.Write(data[offset:end])
		if err != nil {
			return err
		}
		if n != end-offset {
			return io.ErrShortWrite
		}
		offset = end
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	file = nil
	if err := root.Rename(baseManifestPending, baseManifestName(base.Generation)); err != nil {
		return err
	}
	published = true
	return syncCommitDirectory(root)
}

func loadRunBase(ctx context.Context, root *os.Root, generation, metadataBytes, identityBytes, maxFiles uint64) (_ runBase, err error) {
	file, err := root.Open(baseManifestName(generation))
	if err != nil {
		return runBase{}, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return runBase{}, err
	}
	if !info.Mode().IsRegular() || info.Size() < baseManifestHeader+sha256.Size || info.Size() > int64(math.MaxInt) {
		return runBase{}, storage.ErrCorruptStorage
	}
	var header [baseManifestHeader]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return runBase{}, errors.Join(storage.ErrCorruptStorage, err)
	}
	fileCount := binary.LittleEndian.Uint64(header[48:56])
	transactionCount := binary.LittleEndian.Uint64(header[56:64])
	if binary.LittleEndian.Uint64(header[8:16]) != uint64(info.Size()) || fileCount > maxFiles {
		return runBase{}, storage.ErrCorruptStorage
	}
	entryBytes, boundBytes, err := runBaseFormat(header[:])
	if err != nil { return runBase{}, err }
	size, err := baseManifestSize(fileCount, transactionCount, boundBytes, entryBytes, metadataBytes, identityBytes)
	if err != nil {
		return runBase{}, err
	}
	if size != uint64(info.Size()) {
		return runBase{}, storage.ErrCorruptStorage
	}
	data := make([]byte, int(size))
	copy(data, header[:])
	if _, err := io.ReadFull(file, data[len(header):]); err != nil {
		return runBase{}, errors.Join(storage.ErrCorruptStorage, err)
	}
	return decodeRunBase(ctx, data, generation, metadataBytes, identityBytes, maxFiles)
}

func openBaseFiles(ctx context.Context, root *os.Root, descriptors []preparedRun, limits sstLimits, maxBytes uint64) (_ []*sstFile, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(descriptors) == 0 {
		return nil, nil
	}
	pointerBytes := uint64(unsafe.Sizeof((*sstFile)(nil)))
	if uint64(len(descriptors)) > maxBytes/pointerBytes {
		return nil, columnar.ErrBatchBudget
	}
	used := uint64(len(descriptors)) * pointerBytes
	scratchBytes := min(uint64(64<<10), (maxBytes-used)/2)
	if scratchBytes == 0 {
		return nil, columnar.ErrBatchBudget
	}
	scratch := make([]byte, int(scratchBytes))
	used += scratchBytes
	opened := make([]*sstFile, 0, len(descriptors))
	defer func() {
		if err != nil {
			for _, file := range opened {
				err = errors.Join(err, file.Close())
			}
		}
	}()
	for _, descriptor := range descriptors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nativeBytes := uint64(unsafe.Sizeof(sstFile{}))
		if nativeBytes >= maxBytes-used {
			return nil, columnar.ErrBatchBudget
		}
		file, openErr := openPreparedRun(ctx, root, descriptor, limits, maxBytes-used-nativeBytes, scratch)
		if openErr != nil {
			return nil, openErr
		}
		opened = append(opened, file)
		cost := nativeBytes + uint64(cap(file.index)) + uint64(cap(file.descriptor.LowerKey)+cap(file.descriptor.UpperKey))
		if cost > maxBytes-used {
			return nil, columnar.ErrBatchBudget
		}
		used += cost
	}
	return opened, nil
}

func runBaseFormat(header []byte) (uint64, uint64, error) {
	entryBytes, boundBytes, reservedStart := uint64(baseManifestEntryBytes), uint64(0), 152
	switch string(header[:8]) {
	case baseManifestMagic:
		boundBytes = binary.LittleEndian.Uint64(header[144:152])
	case "NSBASE02":
		entryBytes, reservedStart = 128, 144
	default:
		return 0, 0, storage.ErrCorruptStorage
	}
	for _, reserved := range header[reservedStart:baseManifestHeader] {
		if reserved != 0 { return 0, 0, storage.ErrCorruptStorage }
	}
	return entryBytes, boundBytes, nil
}
