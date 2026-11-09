package neosepia

import (
	"errors"
	"hash/crc32"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// PAX v2 uses little-endian fields and absolute, 8-aligned payload offsets.
// Payload lengths exclude trailing alignment padding. Every node retains one
// slot per logical lane, including NULLs; its bitmap marks NULL values.
// The block checksum covers metadata and payloads, excluding its own field.
const (
	paxMagic       uint32 = 0x32584150
	paxVersion     uint32 = 2
	paxHeaderSize         = 64
	paxColumnSize         = 64
	paxVersionSize        = 40
	paxZoneInteger uint16 = 1
)

var errPAXBlock = errors.Join(storage.ErrCorruptStorage, errors.New("invalid PAX block"))

type blockLimits struct {
	MaxRows         int
	MaxBytes        uint64
	MaxDecodedBytes uint64
}

// Visibility metadata is independently accessible even when a column zone map
// proves that no payload lane matches a predicate. Range ends are [key,end).
type blockVersions struct {
	Keys     columnar.Vector
	EndKeys  columnar.Vector
	CommitTS []model.Timestamp
	Ordinals []uint64
	Kinds    []storage.MutationOp
}

type columnZone struct {
	Min   int64
	Max   int64
	Valid bool
}

func paxChecksum(data []byte) uint32 {
	crc := crc32.Update(0, crc32.IEEETable, data[:56])
	return crc32.Update(crc, crc32.IEEETable, data[60:])
}

func aligned8(n uint64) uint64 { return (n + 7) &^ 7 }
