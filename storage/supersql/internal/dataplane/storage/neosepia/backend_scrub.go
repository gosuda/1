package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ScrubRange validates complete candidate blocks, including unprojected columns.
// ScannedRows counts overlapping version records; CRC covers the inspected blocks.
func (backend *Backend) ScrubRange(ctx context.Context, request storage.ScrubRequest) (report storage.ScrubReport, err error) {
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ScrubReport{}, storage.ErrClosed
	}
	limits := backend.limits
	pin, pinErr := backend.runs.pin(ctx)
	backend.mu.Unlock()
	if pinErr != nil {
		return storage.ScrubReport{}, pinErr
	}
	defer func() {
		err = errors.Join(err, pin.Close())
		if err != nil {
			report = storage.ScrubReport{}
		}
	}()
	if err := request.Target.Validate(); err != nil {
		return storage.ScrubReport{}, err
	}
	if len(request.StartKey) != 0 && len(request.EndKey) != 0 && bytes.Compare(request.StartKey, request.EndKey) >= 0 {
		return storage.ScrubReport{}, storage.ErrRangeInvalid
	}
	if uint64(len(request.StartKey)) > limits.KeyBytes || uint64(len(request.EndKey)) > limits.KeyBytes-uint64(len(request.StartKey)) {
		return storage.ScrubReport{}, columnar.ErrBatchBudget
	}
	report.StartKey = bytes.Clone(request.StartKey)
	report.EndKey = bytes.Clone(request.EndKey)
	span := encodedSpan{lower: keyCut{infinity: -1}, upper: keyCut{infinity: 1}}
	if len(report.StartKey) != 0 {
		span.lower = keyCut{prefix: report.StartKey}
	}
	if len(report.EndKey) != 0 {
		span.upper = keyCut{prefix: report.EndKey}
	}
	err = pin.visit(ctx, request.Target, func(file *sstFile) error {
		first, end, err := file.spanCandidates(ctx, []encodedSpan{span}, 0)
		if err != nil {
			return err
		}
		for index := first; index < end; index++ {
			block, err := file.ReadBlock(ctx, index, limits.ReadBlockBytes, nil)
			if err != nil {
				return err
			}
			rows, crc, scrubErr := scrubBlock(ctx, file, block, report.StartKey, report.EndKey, report.CalculatedCRC)
			if err := errors.Join(scrubErr, block.Close()); err != nil {
				return err
			}
			if report.BlockCount == math.MaxUint64 || rows > math.MaxUint64-report.ScannedRows {
				return storage.ErrCorruptStorage
			}
			report.BlockCount++
			report.ScannedRows += rows
			report.CalculatedCRC = crc
		}
		return nil
	})
	return report, err
}

func scrubBlock(ctx context.Context, file *sstFile, block *paxBlock, start, end []byte, crc uint32) (uint64, uint32, error) {
	for column := 0; column < block.columns; column++ {
		id := model.ColumnID(binary.LittleEndian.Uint32(block.descriptors[column*paxColumnSize:]))
		if _, err := block.Column(ctx, id); err != nil {
			return 0, 0, err
		}
	}
	var rows uint64
	for row := 0; row < block.rows; row++ {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		key, rangeEnd, _, _, kind, err := block.Version(row)
		if err != nil {
			return 0, 0, err
		}
		if !file.containsKey(key) {
			continue
		}
		if len(end) != 0 && bytes.Compare(key, end) >= 0 {
			continue
		}
		if len(start) != 0 {
			if kind == storage.MutationDeleteRange {
				if bytes.Compare(rangeEnd, start) <= 0 {
					continue
				}
			} else if bytes.Compare(key, start) < 0 {
				continue
			}
		}
		rows++
	}
	for offset := 0; offset < len(block.data); {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		limit := offset + min(len(block.data)-offset, 64<<10)
		crc = crc32.Update(crc, crc32.IEEETable, block.data[offset:limit])
		offset = limit
	}
	return rows, crc, nil
}
