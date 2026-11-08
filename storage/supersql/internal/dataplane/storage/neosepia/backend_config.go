package neosepia

import (
	"errors"
	"math"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var ErrInvalidConfig = errors.New("invalid native backend configuration")
var ErrIncompatibleFormat = errors.New("database requires explicit copy-upgrade to the native format")

// Config borrows the encoder and verifier. Open copies the scale metadata.
// A zero Limits selects DefaultLimits; a partial policy is rejected.
// ExtraCheckpointFiles lists composition-owned regular files at the database
// root that each checkpoint links and authenticates like database.state. They
// are required only when Checkpoint runs, not at Open.
type Config struct {
	Directory            string
	Scale                clock.ScaleDescriptor
	KeyEncoder           storage.KeyEncoder
	FenceVerifier        storage.FenceVerifier
	Limits               Limits
	ExtraCheckpointFiles []string
}

type Limits struct {
	ResidentBytes, MaxFiles, MaxPins                                               uint64
	MetadataBytes, RecoveryFiles, IdentityBytes                                    uint64
	WALBytes, WALAllocation, MutationBytes                                         uint64
	WriteBytes, SortBytes, BlockBytes, DecodedBlockBytes, ReadBlockBytes           uint64
	BlockRows                                                                      int
	RunBytes, RunIndexBytes                                                        uint64
	RunBlocks                                                                      uint32
	Builders                                                                       int
	Runs, PreparationBytes, PublicationBytes                                       uint64
	ReadSourceBytes, ReadMergeBytes, KeyBytes, ScanBytes, RowBytes, PredicateBytes uint64
	OverlayBytes, OverlayBlockBytes, PlanBytes                                     uint64
	BatchRows, PredicateNodes, Plans                                               int
}

func DefaultLimits() Limits {
	return Limits{
		ResidentBytes: 64 << 20, MaxFiles: 65536, MaxPins: 512,
		MetadataBytes: 8 << 20, RecoveryFiles: 1 << 20, IdentityBytes: 32 << 20,
		WALBytes: 1 << 30, WALAllocation: 64 << 20, MutationBytes: 32 << 20,
		WriteBytes: 16 << 20, SortBytes: 8 << 20, BlockBytes: 4 << 20, DecodedBlockBytes: 16 << 20, ReadBlockBytes: 20 << 20, BlockRows: 1024,
		RunBytes: 64 << 20, RunIndexBytes: 4 << 20, RunBlocks: 16384, Builders: 8, Runs: 65536, PreparationBytes: 16 << 20, PublicationBytes: 32 << 20,
		ReadSourceBytes: 256 << 20, ReadMergeBytes: 8 << 20, KeyBytes: 64 << 10, ScanBytes: 16 << 20, RowBytes: 32 << 20, PredicateBytes: 8 << 20,
		OverlayBytes: 8 << 20, OverlayBlockBytes: 16 << 20, PlanBytes: 32 << 20, BatchRows: 1024, PredicateNodes: 4096, Plans: 128,
	}
}

func (limits Limits) validate() error {
	for _, value := range [...]uint64{
		limits.ResidentBytes, limits.MaxFiles, limits.MaxPins, limits.MetadataBytes, limits.RecoveryFiles, limits.IdentityBytes,
		limits.WALBytes, limits.WALAllocation, limits.MutationBytes, limits.WriteBytes, limits.SortBytes, limits.BlockBytes, limits.DecodedBlockBytes, limits.ReadBlockBytes,
		limits.RunBytes, limits.RunIndexBytes, uint64(limits.RunBlocks), limits.Runs, limits.PreparationBytes, limits.PublicationBytes,
		limits.ReadSourceBytes, limits.ReadMergeBytes, limits.KeyBytes, limits.ScanBytes, limits.RowBytes, limits.PredicateBytes, limits.OverlayBytes, limits.OverlayBlockBytes, limits.PlanBytes,
	} {
		if value == 0 {
			return ErrInvalidConfig
		}
	}
	for _, count := range [...]int{limits.BlockRows, limits.Builders, limits.BatchRows, limits.PredicateNodes, limits.Plans} {
		if count <= 0 {
			return ErrInvalidConfig
		}
	}
	if uint64(limits.BlockRows) > math.MaxUint32 || uint64(limits.BatchRows) > math.MaxUint32 {
		return ErrInvalidConfig
	}
	if limits.BlockBytes > math.MaxUint64-limits.DecodedBlockBytes || limits.ReadBlockBytes < limits.BlockBytes+limits.DecodedBlockBytes {
		return ErrInvalidConfig
	}
	if err := limits.preparation().File.validate(); err != nil {
		return errors.Join(ErrInvalidConfig, err)
	}
	return nil
}

func (limits Limits) preparation() runPreparationLimits {
	block := blockLimits{MaxRows: limits.BlockRows, MaxBytes: limits.BlockBytes, MaxDecodedBytes: limits.DecodedBlockBytes}
	return runPreparationLimits{
		MaxBuilders: limits.Builders, MaxRuns: limits.Runs, MetadataBytes: limits.PreparationBytes,
		Chunk: mutationBlockLimits{BuildBytes: limits.WriteBytes, SortBytes: limits.SortBytes, Block: block},
		File:  sstLimits{Block: block, MaxBytes: limits.RunBytes, MaxIndexBytes: limits.RunIndexBytes, MaxBlocks: limits.RunBlocks},
	}
}

func (limits Limits) reads() readLimits {
	return readLimits{
		SourceBytes: limits.ReadSourceBytes, BlockBytes: limits.ReadBlockBytes, MergeBytes: limits.ReadMergeBytes, KeyBytes: limits.KeyBytes,
		ScanBytes: limits.ScanBytes, RowBytes: limits.RowBytes, PredicateBytes: limits.PredicateBytes, OverlayBytes: limits.OverlayBytes, OverlayBlockBytes: limits.OverlayBlockBytes,
		PlanBytes: limits.PlanBytes, MaxRows: limits.BatchRows, MaxNodes: limits.PredicateNodes, MaxPlans: limits.Plans,
	}
}

func (limits Limits) resident() runSetLimits {
	return runSetLimits{MaxBytes: limits.ResidentBytes, MaxFiles: limits.MaxFiles, MaxPins: limits.MaxPins}
}
func (limits Limits) recovery() recoveryLimits {
	return recoveryLimits{Batch: storage.BatchDecodeLimits{MaxBytes: limits.WALBytes, MaxAllocation: limits.WALAllocation, MaxMutationBytes: limits.MutationBytes}, MaxFiles: limits.RecoveryFiles}
}
