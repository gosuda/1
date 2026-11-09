package builtin

import (
	"encoding/binary"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func appendTimetzKey(key []byte, raw []byte, descending bool) ([]byte, error) {
	local, zone, err := DecodeTimetz(raw)
	if err != nil {
		return nil, err
	}
	utc := uint64(local+int64(zone)*1_000_000) ^ (uint64(1) << 63)
	offset := uint32(zone) ^ (uint32(1) << 31)
	if descending {
		utc, offset = ^utc, ^offset
	}
	key = binary.BigEndian.AppendUint64(key, utc)
	return binary.BigEndian.AppendUint32(key, offset), nil
}

func (d *keyDecoder) timetz(mask byte) (model.Value, error) {
	utcBits, err := d.number(8, mask)
	if err != nil {
		return model.Value{}, err
	}
	zoneBits, err := d.number(4, mask)
	if err != nil {
		return model.Value{}, err
	}
	zone := int32(uint32(zoneBits) ^ (uint32(1) << 31))
	utc := int64(utcBits ^ (uint64(1) << 63))
	displacement := int64(zone) * 1_000_000
	if utc < displacement || utc > intervalMicrosPerDay+displacement {
		return model.Value{}, storage.ErrInvalidEncoding
	}
	if err := d.charge(12); err != nil {
		return model.Value{}, err
	}
	return timetzValue(utc-displacement, zone)
}
