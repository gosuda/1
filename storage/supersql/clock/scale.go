package clock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

var ErrClockScale = errors.New("incompatible SI clock scale or leap history")

// ValidateScale validates the provider-independent GSTL1 persistence contract.
// It borrows the descriptor only through this call and allocates no entry table.
func ValidateScale(ctx context.Context, scale ScaleDescriptor, maxBytes uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data := scale.CanonicalLeapHistory
	if scale.Name != ScaleName || uint64(len(data)) > maxBytes || len(data) < 13 || string(data[:5]) != "GSTL1" {
		return ErrClockScale
	}
	count := uint64(binary.BigEndian.Uint32(data[9:13]))
	if uint64(len(data)) != 13+count*16 {
		return ErrClockScale
	}
	var previous int64
	for i := uint64(0); i < count; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := data[13+i*16 : 13+(i+1)*16]
		second := int64(binary.BigEndian.Uint64(entry))
		if i != 0 && second <= previous {
			return ErrClockScale
		}
		delta := int8(entry[8])
		if delta != 1 && delta != -1 {
			return ErrClockScale
		}
		for _, reserved := range entry[9:] {
			if reserved != 0 {
				return ErrClockScale
			}
		}
		previous = second
	}
	digest := sha256.New()
	for offset := 0; offset < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + min(len(data)-offset, 64<<10)
		digest.Write(data[offset:end])
		offset = end
	}
	var identity [32]byte
	digest.Sum(identity[:0])
	if identity != scale.LeapHistoryID {
		return ErrClockScale
	}
	return nil
}

// ValidateScaleExtension permits only an unchanged history or appended entries
// whose first affected SI instant is strictly beyond the protected timestamp.
func ValidateScaleExtension(ctx context.Context, previous, next ScaleDescriptor, protected Instant, maxBytes uint64) error {
	if protected < 0 {
		return ErrClockRange
	}
	if err := ValidateScale(ctx, previous, maxBytes); err != nil {
		return err
	}
	if previous.Name == next.Name && previous.LeapHistoryID == next.LeapHistoryID && bytes.Equal(previous.CanonicalLeapHistory, next.CanonicalLeapHistory) {
		return nil
	}
	if err := ValidateScale(ctx, next, maxBytes); err != nil {
		return err
	}
	oldData, newData := previous.CanonicalLeapHistory, next.CanonicalLeapHistory
	if len(newData) < len(oldData) || !bytes.Equal(oldData[5:9], newData[5:9]) || !bytes.Equal(oldData[13:], newData[13:len(oldData)]) {
		return ErrClockScale
	}
	var cumulative int64
	for offset := 13; offset < len(newData); offset += 16 {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := newData[offset : offset+16]
		delta := int64(int8(entry[8]))
		if offset >= len(oldData) {
			second := int64(binary.BigEndian.Uint64(entry))
			// Rearranging the SI comparison avoids overflowing an arbitrary encoded
			// Unix second. Cumulative deltas are bounded by the uint32 entry count.
			boundary := int64(protected)/1_000_000_000 - cumulative - min(delta, 0)
			if second <= boundary {
				return ErrClockScale
			}
		}
		cumulative += delta
	}
	return nil
}
