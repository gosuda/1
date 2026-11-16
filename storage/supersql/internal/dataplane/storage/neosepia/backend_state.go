package neosepia

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	databaseStateMagic   = "NSDB0001"
	databaseStateName    = "database.state"
	databaseStatePending = "database.state.pending"
	databaseStateHeader  = 176
	databaseScaleHeader  = 56
)

type scaleVersion struct {
	Position  uint64
	Timestamp model.Timestamp
	Scale     clock.ScaleDescriptor
}

type databaseState struct {
	Generation     uint64
	HistoryFloor   model.Timestamp
	Closed         storage.ReadFence
	BaseGeneration uint64
	Scales         []scaleVersion
}

func (state databaseState) validate(ctx context.Context, maxBytes uint64) error {
	if state.Generation == 0 || state.BaseGeneration > state.Generation || state.HistoryFloor < 0 || state.Closed.ClosedTS < 0 || len(state.Scales) == 0 {
		return storage.ErrCorruptStorage
	}
	if state.Closed.ClosedTS == 0 {
		if state.Closed != (storage.ReadFence{}) {
			return storage.ErrCorruptStorage
		}
	} else if state.Closed.ID == ([16]byte{}) {
		return storage.ErrCorruptStorage
	}
	var previous scaleVersion
	closedScale := state.Closed.ClosedTS == 0
	for i, version := range state.Scales {
		if err := ctx.Err(); err != nil {
			return err
		}
		if version.Timestamp < 0 {
			return storage.ErrCorruptStorage
		}
		if i == 0 {
			if version.Position != 0 || version.Timestamp != 0 {
				return storage.ErrCorruptStorage
			}
			if err := clock.ValidateScale(ctx, version.Scale, maxBytes); err != nil {
				return errors.Join(storage.ErrCorruptStorage, err)
			}
		} else {
			if version.Position < previous.Position || version.Timestamp < previous.Timestamp {
				return storage.ErrCorruptStorage
			}
			if err := clock.ValidateScaleExtension(ctx, previous.Scale, version.Scale, clock.Instant(version.Timestamp), maxBytes); err != nil {
				return errors.Join(storage.ErrCorruptStorage, err)
			}
		}
		if version.Scale.LeapHistoryID == state.Closed.Epoch.LeapHistory {
			closedScale = true
		}
		previous = version
	}
	if !closedScale {
		return storage.ErrCorruptStorage
	}
	return nil
}

func encodeDatabaseState(ctx context.Context, state databaseState, maxBytes uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size := uint64(databaseStateHeader + sha256.Size)
	if size > maxBytes {
		return nil, storage.ErrDecodeBudget
	}
	for _, version := range state.Scales {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bytes := uint64(databaseScaleHeader) + uint64(len(version.Scale.CanonicalLeapHistory))
		if bytes > maxBytes-size {
			return nil, storage.ErrDecodeBudget
		}
		size += bytes
	}
	if size > uint64(math.MaxInt) {
		return nil, storage.ErrDecodeBudget
	}
	if err := state.validate(ctx, maxBytes); err != nil {
		return nil, err
	}
	data := make([]byte, int(size))
	copy(data, databaseStateMagic)
	binary.LittleEndian.PutUint64(data[8:16], size)
	binary.LittleEndian.PutUint64(data[16:24], state.Generation)
	binary.LittleEndian.PutUint64(data[24:32], uint64(state.HistoryFloor))
	binary.LittleEndian.PutUint64(data[32:40], uint64(state.Closed.ClosedTS))
	binary.LittleEndian.PutUint64(data[40:48], state.Closed.LogPosition)
	copy(data[48:64], state.Closed.ID[:])
	copy(data[64:80], state.Closed.Epoch.Assurance[:])
	copy(data[80:112], state.Closed.Epoch.LeapHistory[:])
	copy(data[112:144], state.Closed.Epoch.Config[:])
	binary.LittleEndian.PutUint64(data[168:176], state.BaseGeneration)
	binary.LittleEndian.PutUint64(data[144:152], uint64(len(state.Scales)))
	copy(data[152:168], clock.ScaleName)
	offset := databaseStateHeader
	for _, version := range state.Scales {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		binary.LittleEndian.PutUint64(data[offset:offset+8], version.Position)
		binary.LittleEndian.PutUint64(data[offset+8:offset+16], uint64(version.Timestamp))
		copy(data[offset+16:offset+48], version.Scale.LeapHistoryID[:])
		history := version.Scale.CanonicalLeapHistory
		binary.LittleEndian.PutUint64(data[offset+48:offset+56], uint64(len(history)))
		offset += databaseScaleHeader
		copy(data[offset:], history)
		offset += len(history)
	}
	digest, err := checksumStateBytes(ctx, data[:offset])
	if err != nil {
		return nil, err
	}
	copy(data[offset:], digest[:])
	return data, nil
}

// Decode consumes data on success. Scale payloads retain this one bounded buffer.
func decodeDatabaseState(ctx context.Context, data []byte, maxAllocation uint64) (databaseState, error) {
	if err := ctx.Err(); err != nil {
		return databaseState{}, err
	}
	if len(data) < databaseStateHeader+sha256.Size || string(data[:8]) != databaseStateMagic || binary.LittleEndian.Uint64(data[8:16]) != uint64(len(data)) {
		return databaseState{}, storage.ErrCorruptStorage
	}
	if uint64(len(data)) > maxAllocation {
		return databaseState{}, storage.ErrDecodeBudget
	}
	end := len(data) - sha256.Size
	digest, err := checksumStateBytes(ctx, data[:end])
	if err != nil {
		return databaseState{}, err
	}
	if digest != [32]byte(data[end:]) {
		return databaseState{}, storage.ErrCorruptStorage
	}
	if string(data[152:152+len(clock.ScaleName)]) != clock.ScaleName {
		return databaseState{}, storage.ErrCorruptStorage
	}
	for _, reserved := range data[152+len(clock.ScaleName) : 168] {
		if reserved != 0 {
			return databaseState{}, storage.ErrCorruptStorage
		}
	}
	count := binary.LittleEndian.Uint64(data[144:152])
	remaining := maxAllocation - uint64(len(data))
	if count > remaining/uint64(unsafe.Sizeof(scaleVersion{})) || count > uint64((end-databaseStateHeader)/databaseScaleHeader) {
		return databaseState{}, storage.ErrDecodeBudget
	}
	state := databaseState{
		Generation: binary.LittleEndian.Uint64(data[16:24]), BaseGeneration: binary.LittleEndian.Uint64(data[168:176]),
		HistoryFloor: model.Timestamp(binary.LittleEndian.Uint64(data[24:32])),
		Closed:       storage.ReadFence{ID: [16]byte(data[48:64]), ClosedTS: model.Timestamp(binary.LittleEndian.Uint64(data[32:40])), LogPosition: binary.LittleEndian.Uint64(data[40:48]), Epoch: clock.Epoch{Assurance: [16]byte(data[64:80]), LeapHistory: [32]byte(data[80:112]), Config: [32]byte(data[112:144])}},
		Scales:       make([]scaleVersion, int(count)),
	}
	offset := databaseStateHeader
	for i := range state.Scales {
		if err := ctx.Err(); err != nil {
			return databaseState{}, err
		}
		if end-offset < databaseScaleHeader {
			return databaseState{}, storage.ErrCorruptStorage
		}
		version := &state.Scales[i]
		version.Position = binary.LittleEndian.Uint64(data[offset : offset+8])
		version.Timestamp = model.Timestamp(binary.LittleEndian.Uint64(data[offset+8 : offset+16]))
		version.Scale.Name = clock.ScaleName
		version.Scale.LeapHistoryID = [32]byte(data[offset+16 : offset+48])
		length := binary.LittleEndian.Uint64(data[offset+48 : offset+56])
		offset += databaseScaleHeader
		if length > uint64(end-offset) {
			return databaseState{}, storage.ErrCorruptStorage
		}
		version.Scale.CanonicalLeapHistory = data[offset : offset+int(length) : offset+int(length)]
		offset += int(length)
	}
	if offset != end {
		return databaseState{}, storage.ErrCorruptStorage
	}
	if err := state.validate(ctx, maxAllocation); err != nil {
		return databaseState{}, err
	}
	return state, nil
}

func checksumStateBytes(ctx context.Context, data []byte) ([32]byte, error) {
	digest := sha256.New()
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		end := offset + min(len(data)-offset, 64<<10)
		digest.Write(data[offset:end])
		offset = end
	}
	var result [32]byte
	digest.Sum(result[:0])
	return result, nil
}
