package storage

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type Backend interface {
	ApplyMutations(context.Context, MutationBatch) (ApplyResult, error)
	OpenSnapshot(context.Context, SnapshotSpec) (SnapshotReader, error)
	ResolveCommit(context.Context, model.TxnID) (CommitResolution, error)
	Close() error
}

type SnapshotReader interface {
	Get(context.Context, ReadRequest) (model.Row, error)
	PlanScan(context.Context, PushdownRequest) (PushdownPlan, error)
	VectorizedScan(context.Context, PushdownPlan) (BatchCursor, error)
	// ReleasePlan retires this reader's issued plan after all cursors opened
	// from it close. Active, unknown, or foreign plans return ErrInvalidScanPlan.
	// A released plan cannot be reopened; reader Close releases remaining plans.
	ReleasePlan(PushdownPlan) error
	Close() error
}

// ObservationCount distinguishes an observed zero from an unavailable metric.
// SnapshotObservation values describe the immutable reader timestamp and target.
type ObservationCount struct {
	Value uint64
	Known bool
}

type SnapshotObservation struct {
	Target                         ObjectRef
	Snapshot                       model.Timestamp
	VersionRows, Blocks, Files     ObservationCount
	DeltaBytes                     ObservationCount
}

// SnapshotObserver is an optional SnapshotReader capability. Every known value
// is measured from the same pinned generation as reads through that reader.
type SnapshotObserver interface {
	ObserveSnapshot(context.Context, ObjectRef) (SnapshotObservation, error)
}

// A cursor's caller serializes Next and Close; cancellation uses the context.
type BatchCursor interface {
	// Next honors a positive Batch.MaxRows. Only bare io.EOF means completion;
	// empty batches are not EOF. Payloads are borrowed until Next/Close.
	Next(context.Context, *columnar.Batch) error
	Close() error
}
// SamplingCursor exposes a stable storage-native sample unit for each physical
// lane published by the most recent Next call.
type SamplingCursor interface {
	BatchCursor
	SampleUnit(uint32) (uint64, bool)
}


type CommitState uint8

const (
	CommitUnknown CommitState = iota
	CommitNotCommitted
	CommitCommitted
)

type CommitResolution struct {
	State  CommitState
	Result ApplyResult
}

// ReadFence is issued by the database commit sequencer after its certified
// clock wait and publication barrier. ClosedTS is not an implicit latest read.
// LogPosition binds the fence to already-published durable commit decisions.
type ReadFence struct {
	ID          [16]byte
	ClosedTS    model.Timestamp
	LogPosition uint64
	Epoch       clock.Epoch
}

// FenceVerifier authenticates sequencer-issued publication barriers. Storage
// verifies provenance without importing the clock sequencer or transaction manager.
type FenceVerifier interface {
	VerifyReadFence(context.Context, ReadFence) error
}

type SnapshotSpec struct {
	Timestamp model.Timestamp
	Fence     ReadFence
	Overlay   OverlayView
}

type ColumnLayout struct {
	ID              model.ColumnID
	Type            model.Type
	Children        []model.TypeLayout
	Nullable        bool
	HasMissingValue bool
	MissingValue    model.Value
}

type ReadRequest struct {
	Target    ObjectRef
	Key       []byte
	ColumnIDs []model.ColumnID
	Layout    []ColumnLayout
}

// KeyEncoder is the common SQL type/collation service used by writes and scans.
// Encoded prefixes contain complete leading fields: appending fields extends the
// byte prefix. Results are owned and bounded by maxBytes; the backend borrows the
// service and does not close it.
type KeyEncoder interface {
	EncodePrefix(context.Context, []KeyColumn, []model.Value, uint64) ([]byte, error)
}

// IndexKeyEncoder certifies that a resolved operator class uses EncodePrefix's
// ordering and equality for this type. Non-equivalent classes require a different
// executable codec; accepting their metadata alone is not sufficient.
type IndexKeyEncoder interface {
	KeyEncoder
	ValidateOperatorClass(context.Context, model.TypeLayout, model.OID) error
}

// KeyDecoder reconstructs a complete canonical key tuple for typed resume
// bounds. It need not recover original numeric spelling or character padding.
// Returned values are owned and must re-encode to the original key bytes.
type KeyDecoder interface {
	DecodeKey(context.Context, []KeyColumn, []byte, uint64) ([]model.Value, error)
}

type Maintenance interface {
	Checkpoint(context.Context, CheckpointRequest) (CheckpointHandle, error)
	Collect(context.Context, GCRequest) error
	ScheduleCompaction(context.Context, CompactionRequest) (CompactionTaskHandle, error)
	ScrubRange(context.Context, ScrubRequest) (ScrubReport, error)
}

type CheckpointRequest struct {
	Snapshot SnapshotSpec
}

type CheckpointFile struct {
	Name   string
	Size   uint64
	SHA256 [32]byte
}

type CheckpointManifest struct {
	FormatVersion uint32
	Backend       string
	SnapshotTS    model.Timestamp
	HistoryFloor  model.Timestamp
	Fence         ReadFence
	Scale         clock.ScaleDescriptor
	Files         []CheckpointFile
}
type CheckpointManifestLimits struct {
	MaxBytes uint64
	MaxFiles uint64
}

// A checkpoint pins its immutable file set until Close. OpenFile accepts only a
// manifest name, never an arbitrary filesystem path. Its streams must be closed
// before the handle. Manifest admits all owned metadata before copying it;
// zero limits admit nothing, and a closed handle returns ErrClosed.
type CheckpointHandle interface {
	Manifest(context.Context, CheckpointManifestLimits) (CheckpointManifest, error)
	OpenFile(context.Context, string) (io.ReadCloser, error)
	Close() error
}

// ObjectGeneration names one independently reclaimable physical keyspace.
// Descriptor and encoding versions do not distinguish stored keyspaces.
type ObjectGeneration struct {
	ObjectID   model.ObjectID
	Generation uint64
}

func (object ObjectGeneration) Validate() error {
	if object.ObjectID == 0 || object.Generation == 0 {
		return ErrInvalidTarget
	}
	return nil
}

// ObjectRetirement is immutable evidence that a physical keyspace stopped being
// reachable at RetiredAt. Collection may remove it only at that timestamp or a
// later history boundary.
type ObjectRetirement struct {
	target    ObjectGeneration
	retiredAt model.Timestamp
}

func NewObjectRetirement(target ObjectGeneration, retiredAt model.Timestamp) (ObjectRetirement, error) {
	if err := target.Validate(); err != nil {
		return ObjectRetirement{}, err
	}
	if retiredAt <= 0 {
		return ObjectRetirement{}, ErrInvalidMutation
	}
	return ObjectRetirement{target: target, retiredAt: retiredAt}, nil
}

func (retirement ObjectRetirement) Target() ObjectGeneration { return retirement.target }
func (retirement ObjectRetirement) RetiredAt() model.Timestamp { return retirement.retiredAt }
func (retirement ObjectRetirement) Validate(boundary model.Timestamp) error {
	if err := retirement.target.Validate(); err != nil {
		return err
	}
	if retirement.retiredAt <= 0 || retirement.retiredAt > boundary {
		return ErrInvalidMutation
	}
	return nil
}


type GCRequest struct {
	Boundary model.Timestamp
	// Retired is strictly target-sorted, unique and borrowed only for Collect.
	// Its immutable timestamps are the atomic catalog/job retirement commits.
	Retired []ObjectRetirement
}

type CompactionRequest struct {
	Target      ObjectRef
	GCWatermark model.Timestamp
	TargetBlock int
}

type CompactionTaskHandle interface {
	TaskID() string
	Wait(context.Context) error
}

type ScrubRequest struct {
	Target   ObjectRef
	StartKey []byte
	EndKey   []byte
}

type ScrubReport struct {
	StartKey      []byte
	EndKey        []byte
	ScannedRows   uint64
	CalculatedCRC uint32
	BlockCount    uint64
}

var (
	ErrClosed              = errors.New("storage resource closed")
	ErrSnapshotTooOld      = errors.New("snapshot is below the retained history boundary")
	ErrSnapshotNotClosed   = errors.New("snapshot has no valid closed read fence")
	ErrSnapshotPinned      = errors.New("history is protected by an active snapshot")
	ErrInvalidScanPlan     = errors.New("scan plan does not match its snapshot or layout")
	ErrCommitIndeterminate = errors.New("commit outcome requires resolution")
	ErrCorruptStorage      = errors.New("storage corruption")
)
