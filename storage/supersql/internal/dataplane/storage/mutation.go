package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type MutationOp uint8

const (
	MutationInsert MutationOp = iota + 1
	MutationUpdate
	MutationUpsert
	MutationReplace
	MutationDelete
	MutationDeleteRange
)

func (op MutationOp) String() string {
	switch op {
	case MutationInsert:
		return "INSERT"
	case MutationUpdate:
		return "UPDATE"
	case MutationUpsert:
		return "UPSERT"
	case MutationReplace:
		return "REPLACE"
	case MutationDelete:
		return "DELETE"
	case MutationDeleteRange:
		return "DELETE_RANGE"
	}
	return fmt.Sprintf("UNKNOWN_OP(%d)", op)
}

type ObjectRef struct {
	ObjectID          model.ObjectID
	Generation        uint64
	DescriptorVersion uint64
	EncodingVersion   uint32
}

type Mutation struct {
	Op          MutationOp
	Target      ObjectRef
	Key         []byte
	EndKey      []byte
	ColumnIDs   []model.ColumnID
	Values      []model.Value
	Ordinal     uint64
	ChecksumCRC uint32
}

// Constructors clone caller buffers. Internal producers may instead transfer
// their already-owned buffers with a Mutation literal into the transaction.
func NewInsertMutation(target ObjectRef, key []byte, columns []model.ColumnID, values []model.Value) Mutation {
	return ownedMutation(MutationInsert, target, key, columns, values)
}
func NewUpdateMutation(target ObjectRef, key []byte, columns []model.ColumnID, values []model.Value) Mutation {
	return ownedMutation(MutationUpdate, target, key, columns, values)
}
func NewUpsertMutation(target ObjectRef, key []byte, columns []model.ColumnID, values []model.Value) Mutation {
	return ownedMutation(MutationUpsert, target, key, columns, values)
}
func NewReplaceMutation(target ObjectRef, key []byte, columns []model.ColumnID, values []model.Value) Mutation {
	return ownedMutation(MutationReplace, target, key, columns, values)
}
func NewDeleteMutation(target ObjectRef, key []byte) Mutation {
	return Mutation{Op: MutationDelete, Target: target, Key: bytes.Clone(key)}
}
func NewDeleteRangeMutation(target ObjectRef, key, endKey []byte) Mutation {
	return Mutation{Op: MutationDeleteRange, Target: target, Key: bytes.Clone(key), EndKey: bytes.Clone(endKey)}
}
func ownedMutation(op MutationOp, target ObjectRef, key []byte, columns []model.ColumnID, values []model.Value) Mutation {
	return Mutation{Op: op, Target: target, Key: bytes.Clone(key), ColumnIDs: slices.Clone(columns), Values: model.CloneValues(values)}
}

// MutationSequence opens repeatable views of an immutable spilled write set.
// It is mutually exclusive with MutationBatch.Mutations, not another apply path.
// The transaction owns the sequence; each cursor owns its own resources.
type MutationSequence interface {
	Count() uint64
	Open(context.Context) (MutationCursor, error)
}

type MutationCursor interface {
	// Next borrows returned buffers until the next Next/Close; only bare io.EOF ends iteration.
	Next(context.Context) (Mutation, error)
	Close() error
}

type MutationBatch struct {
	TxID            model.TxnID
	CommitTS        model.Timestamp
	Epoch           clock.Epoch
	Layouts         []ObjectLayout
	Mutations       []Mutation
	Sequence        MutationSequence
	EncodingVersion uint32
	ChecksumCRC     uint32
}

type ApplyResult struct {
	AppliedCount uint64
	CommitTS     model.Timestamp
	ChecksumCRC  uint32
	LogPosition  uint64
}

var (
	ErrBatchEmpty        = errors.New("mutation batch cannot be empty")
	ErrInvalidMutation   = errors.New("invalid mutation")
	ErrRangeInvalid      = errors.New("invalid key range bounds")
	ErrInvalidTarget     = errors.New("invalid object generation or descriptor")
	ErrRowNotFound       = errors.New("row not found")
	ErrChecksumMismatch  = errors.New("mutation checksum mismatch")
	ErrUnnormalizedBatch = errors.New("backend requires normalized mutations")
	ErrTxnIDReused       = errors.New("transaction ID reused with different mutations")
)

func (target ObjectRef) Validate() error {
	if target.ObjectID == 0 || target.Generation == 0 {
		return ErrInvalidTarget
	}
	if target.DescriptorVersion == 0 || target.EncodingVersion == 0 {
		return ErrInvalidTarget
	}
	return nil
}

// ValidateShape checks representation only. Existence, defaults, constraints,
// generated columns and index/shadow expansion belong to the transaction layer.
func (mutation *Mutation) ValidateShape() error {
	if err := mutation.Target.Validate(); err != nil {
		return err
	}
	switch mutation.Op {
	case MutationInsert, MutationUpdate, MutationUpsert, MutationReplace:
		if len(mutation.EndKey) != 0 {
			return ErrInvalidMutation
		}
		if len(mutation.ColumnIDs) != len(mutation.Values) {
			return ErrInvalidMutation
		}
		return validateColumnIDs(mutation.ColumnIDs)
	case MutationDelete:
		if len(mutation.EndKey) != 0 {
			return ErrInvalidMutation
		}
	case MutationDeleteRange:
		if len(mutation.Key) == 0 || len(mutation.EndKey) == 0 {
			return ErrRangeInvalid
		}
		if bytes.Compare(mutation.Key, mutation.EndKey) >= 0 {
			return ErrRangeInvalid
		}
	default:
		return ErrInvalidMutation
	}
	if len(mutation.ColumnIDs) != 0 || len(mutation.Values) != 0 {
		return ErrInvalidMutation
	}
	return nil
}

func validateColumnIDs(columns []model.ColumnID) error {
	ordered := true
	for i, id := range columns {
		if id == 0 {
			return ErrInvalidMutation
		}
		if i > 0 && id <= columns[i-1] {
			ordered = false
		}
	}
	if ordered {
		return nil
	}
	var local [64]model.ColumnID
	var sorted []model.ColumnID
	if len(columns) <= len(local) {
		sorted = local[:len(columns)]
	} else {
		sorted = make([]model.ColumnID, len(columns))
	}
	copy(sorted, columns)
	slices.Sort(sorted)
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return ErrInvalidMutation
		}
	}
	return nil
}
