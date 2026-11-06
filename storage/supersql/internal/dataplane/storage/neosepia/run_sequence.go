package neosepia

import (
	"context"
	"io"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type runSequenceState struct {
	files []*sstFile
	target storage.ObjectRef
	spans []encodedSpan
	next int
	direction storage.ScanDirection
	current runVersionCursor
	reverse reverseRunState
	key []byte
}

func newRunSequenceCursor(files []*sstFile, target storage.ObjectRef, spans []encodedSpan, direction storage.ScanDirection, blockBytes uint64, reverseLimits mergeLimits, memory *sourceMemory) *runVersionCursor {
	state := &runSequenceState{files: files, target: target, spans: spans, direction: direction, current: runVersionCursor{closed: true}}
	if direction == storage.ScanReverse {
		state.next = len(files)-1
		state.key = make([]byte, 0, int(reverseLimits.MaxKeyBytes))
	}
	return &runVersionCursor{sequence: state, maxBytes: blockBytes, memory: memory}
}

func (cursor *runVersionCursor) nextSequence(ctx context.Context) (versionRow, error) {
	state := cursor.sequence
	for {
		if err := ctx.Err(); err != nil { return versionRow{}, err }
		if !state.current.closed {
			row, err := state.current.Next(ctx)
			if err != io.EOF { return row, err }
		}
		found := false
		for state.next >= 0 && state.next < len(state.files) {
			if err := ctx.Err(); err != nil { return versionRow{}, err }
			file := state.files[state.next]
			if state.direction == storage.ScanReverse { state.next-- } else { state.next++ }
			if targetGeneration(file.target) != targetGeneration(state.target) || file.rangeCount != 0 { continue }
			first, end, err := file.spanCandidates(ctx, state.spans, cursor.snapshotTS)
			if err != nil { return versionRow{}, err }
			if first == end { continue }
			state.current = runVersionCursor{run: file, nextBlock: first, endBlock: end, maxBytes: cursor.maxBytes, memory: cursor.memory, snapshotTS: cursor.snapshotTS}
			if state.direction == storage.ScanReverse {
				state.reverse = reverseRunState{first: first, nextBlock: end-1, nextRow: -1, key: state.key[:0]}
				state.current.reverse = &state.reverse
			}
			found = true
			break
		}
		if !found { return versionRow{}, io.EOF }
	}
}

func (pin *runPin) visitScanSources(ctx context.Context, timestamp model.Timestamp, spec paxScanSpec, observer storage.ScanObserver, visit func(*sstFile, []*sstFile) error) error {
	if pin.closed { return storage.ErrClosed }
	for generation := pin.generation; generation != nil; generation = generation.parent {
		var sequenced [MaxLevels]bool
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil { return err }
			if targetGeneration(file.target) != targetGeneration(spec.Target) { continue }
			if file.tier == inactiveTier && timestamp >= file.historyUntil {
				if observer != nil {
					observer.ObserveScan(storage.ScanObservation{
						BlocksPruned: storage.ObservationCount{Value: uint64(file.count), Known: true},
						RangesPruned: storage.ObservationCount{Value: uint64(len(spec.Spans)), Known: true},
					})
				}
				continue
			}
			first, end, err := file.observedSpanCandidates(ctx, spec.Spans, timestamp, observer)
			if err != nil { return err }
			if first == end { continue }
			// Reverse range events are ordered by their ends, not file key fences.
			// Such files retain independent merge heads across level boundaries.
			if file.tier == activeTier && file.level != 0 && file.rangeCount == 0 {
				if sequenced[file.level] { continue }
				sequenced[file.level] = true
				if err := visit(nil, generation.levels[file.level]); err != nil { return err }
			} else if err := visit(file, nil); err != nil { return err }
		}
	}
	return nil
}
