package txn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func mutationLogTestLimits(t *testing.T, spilled bool) mutationLogLimits {
	t.Helper()
	limits := mutationLogLimits{MemoryBytes: 256 << 10, SpillBytes: 1 << 20, CursorBytes: 256 << 10, MaxCursors: 16, SpillDirectory: t.TempDir()}
	if spilled {
		limits.MemoryBytes = 16 << 10
	}
	return limits
}

func mutationLogTestOpen(t *testing.T, limits mutationLogLimits) *mutationLog {
	t.Helper()
	log, err := newMutationLog(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := log.Close(); err != nil {
			t.Error(err)
		}
	})
	return log
}

func mutationLogTestRow(ordinal uint64) storage.Mutation {
	return storage.Mutation{
		Op:     storage.MutationUpsert,
		Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Key:    []byte("key"), ColumnIDs: []model.ColumnID{1},
		Values:  []model.Value{{Type: model.TypeBytes, BytesVal: bytes.Repeat([]byte{byte(ordinal)}, 32<<10)}},
		Ordinal: ordinal,
	}
}

func mutationLogTestCursor(t *testing.T, sequence storage.MutationSequence) storage.MutationCursor {
	t.Helper()
	cursor, err := sequence.Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}

func mutationLogTestSeal(t *testing.T, log *mutationLog) storage.MutationSequence {
	t.Helper()
	sequence, err := log.seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return sequence
}

func mutationLogTestOrder(t *testing.T, sequence storage.MutationSequence, ordinals []uint64, operations []storage.MutationOp) {
	t.Helper()
	if sequence.Count() != uint64(len(ordinals)) {
		t.Fatalf("count = %d, want %d", sequence.Count(), len(ordinals))
	}
	cursor := mutationLogTestCursor(t, sequence)
	for i, ordinal := range ordinals {
		mutation, err := cursor.Next(t.Context())
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if mutation.Ordinal != ordinal || mutation.Op != operations[i] {
			t.Errorf("row %d = ordinal %d / %s, want %d / %s", i, mutation.Ordinal, mutation.Op, ordinal, operations[i])
		}
	}
	if _, err := cursor.Next(t.Context()); err != io.EOF {
		t.Fatalf("end of sequence = %v, want bare EOF", err)
	}
}

func TestMutationLogOwnsPayloadAndRepeatsCursors(t *testing.T) {
	for _, spilled := range []bool{false, true} {
		name := "memory"
		if spilled {
			name = "spill"
		}
		t.Run(name, func(t *testing.T) {
			log := mutationLogTestOpen(t, mutationLogTestLimits(t, spilled))
			mutation := mutationLogTestRow(1)
			if err := log.append(t.Context(), []storage.Mutation{mutation}); err != nil {
				t.Fatal(err)
			}
			mutation.Key[0] = 'x'
			mutation.ColumnIDs[0] = 99
			mutation.Values[0].BytesVal[0] = 99
			sequence := mutationLogTestSeal(t, log)
			first, second := mutationLogTestCursor(t, sequence), mutationLogTestCursor(t, sequence)
			for _, cursor := range []storage.MutationCursor{first, second} {
				got, err := cursor.Next(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if string(got.Key) != "key" || got.ColumnIDs[0] != 1 || got.Values[0].BytesVal[0] != 1 {
					t.Fatalf("sealed row retained caller or another cursor's aliases: key %q, column %d, byte %d", got.Key, got.ColumnIDs[0], got.Values[0].BytesVal[0])
				}
				got.Key[0], got.Values[0].BytesVal[0] = 'x', 99
			}
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, errMutationLogSealed) {
				t.Fatalf("append after seal = %v", err)
			}
			if _, err := log.savepoint(); !errors.Is(err, errMutationLogSealed) {
				t.Fatalf("savepoint after seal = %v", err)
			}
		})
	}
}

func TestMutationLogSavepointsRetainRangePointOrder(t *testing.T) {
	for _, spilled := range []bool{false, true} {
		for _, rangeFirst := range []bool{false, true} {
			name := "memory/point-first"
			if spilled {
				name = "spill/point-first"
			}
			if rangeFirst {
				name += "/range-first"
			}
			t.Run(name, func(t *testing.T) {
				log := mutationLogTestOpen(t, mutationLogTestLimits(t, spilled))
				point := mutationLogTestRow(1)
				rangeDelete := storage.NewDeleteRangeMutation(point.Target, []byte("a"), []byte("z"))
				rangeDelete.Ordinal = 2
				rows := []storage.Mutation{point, rangeDelete}
				if rangeFirst {
					rangeDelete.Ordinal, point.Ordinal = 1, 2
					rows = []storage.Mutation{rangeDelete, point}
				}
				if err := log.append(t.Context(), rows[:1]); err != nil {
					t.Fatal(err)
				}
				early, err := log.savepoint()
				if err != nil {
					t.Fatal(err)
				}
				if err := log.append(t.Context(), rows[1:]); err != nil {
					t.Fatal(err)
				}
				prefix, err := log.savepoint()
				if err != nil {
					t.Fatal(err)
				}
				if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(3)}); err != nil {
					t.Fatal(err)
				}
				discarded, err := log.savepoint()
				if err != nil {
					t.Fatal(err)
				}
				if err := log.rollback(t.Context(), prefix); err != nil {
					t.Fatal(err)
				}
				if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(3)}); err != nil {
					t.Fatal(err)
				}
				if err := log.rollback(t.Context(), discarded); !errors.Is(err, errMutationLogPosition) {
					t.Fatalf("discarded-branch savepoint = %v", err)
				}
				if err := log.rollback(t.Context(), early); err != nil {
					t.Fatalf("earlier retained savepoint: %v", err)
				}
				if err := log.append(t.Context(), rows[1:]); err != nil {
					t.Fatal(err)
				}
				mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1, 2}, []storage.MutationOp{rows[0].Op, rows[1].Op})
			})
		}
	}
}

func TestMutationLogRejectsInvalidStatementAtomically(t *testing.T) {
	for _, spilled := range []bool{false, true} {
		name := "memory"
		if spilled {
			name = "spill"
		}
		t.Run(name, func(t *testing.T) {
			log := mutationLogTestOpen(t, mutationLogTestLimits(t, spilled))
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
				t.Fatal(err)
			}
			invalid := mutationLogTestRow(3)
			invalid.Op = storage.MutationUpdate
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2), invalid}); !errors.Is(err, storage.ErrUnnormalizedBatch) {
				t.Fatalf("unnormalized statement = %v", err)
			}
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); !errors.Is(err, storage.ErrUnnormalizedBatch) {
				t.Fatalf("reused ordinal = %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := log.append(ctx, []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled statement = %v", err)
			}
			mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1}, []storage.MutationOp{storage.MutationUpsert})
		})
	}
}

type mutationLogFaultFile struct {
	mutationLogFile
	write                     func([]byte, int64) (int, error)
	truncateError, closeError error
}

func (f *mutationLogFaultFile) WriteAt(data []byte, offset int64) (int, error) {
	if f.write != nil {
		return f.write(data, offset)
	}
	return f.mutationLogFile.WriteAt(data, offset)
}

func (f *mutationLogFaultFile) Truncate(size int64) error {
	if f.truncateError != nil {
		return f.truncateError
	}
	return f.mutationLogFile.Truncate(size)
}

func (f *mutationLogFaultFile) Close() error {
	return errors.Join(f.mutationLogFile.Close(), f.closeError)
}

func TestMutationLogCanceledWriteRestoresSpilledPrefix(t *testing.T) {
	log := mutationLogTestOpen(t, mutationLogTestLimits(t, true))
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	file := log.file
	log.file = &mutationLogFaultFile{mutationLogFile: file, write: func(data []byte, offset int64) (int, error) {
		n, err := file.WriteAt(data, offset)
		cancel()
		return n, err
	}}
	if err := log.append(ctx, []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during file write = %v", err)
	}
	log.file = file
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); err != nil {
		t.Fatalf("append after canceled statement: %v", err)
	}
	mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1, 2}, []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
}

func TestMutationLogFailedTruncateFencesLog(t *testing.T) {
	log := mutationLogTestOpen(t, mutationLogTestLimits(t, true))
	empty, err := log.savepoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected truncate failure")
	log.file = &mutationLogFaultFile{mutationLogFile: log.file, truncateError: failure}
	if err := log.rollback(t.Context(), empty); !errors.Is(err, failure) {
		t.Fatalf("rollback = %v", err)
	}
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, failure) {
		t.Fatalf("append after failed rollback = %v", err)
	}
	if _, err := log.seal(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("seal after failed rollback = %v", err)
	}
}

func TestMutationLogWriteAndRollbackFailuresAreJoined(t *testing.T) {
	log := mutationLogTestOpen(t, mutationLogTestLimits(t, true))
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	writeFailure, truncateFailure := errors.New("injected write failure"), errors.New("injected truncate failure")
	log.file = &mutationLogFaultFile{mutationLogFile: log.file, truncateError: truncateFailure, write: func([]byte, int64) (int, error) {
		return 0, writeFailure
	}}
	err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)})
	if !errors.Is(err, writeFailure) || !errors.Is(err, truncateFailure) {
		t.Fatalf("append error = %v, want write and rollback causes", err)
	}
}

func TestMutationLogPartialWriteTruncatesBeforeRetry(t *testing.T) {
	log := mutationLogTestOpen(t, mutationLogTestLimits(t, true))
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	prefix, err := os.Stat(log.path)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected partial write failure")
	file := log.file
	log.file = &mutationLogFaultFile{mutationLogFile: file, write: func(data []byte, offset int64) (int, error) {
		n, err := file.WriteAt(data[:len(data)/2], offset)
		return n, errors.Join(failure, err)
	}}
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, failure) {
		t.Fatalf("partial write = %v", err)
	}
	after, err := os.Stat(log.path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != prefix.Size() {
		t.Fatalf("failed statement left %d spill bytes, want prefix %d", after.Size(), prefix.Size())
	}
	log.file = file
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); err != nil {
		t.Fatal(err)
	}
	mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1, 2}, []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
}

func TestMutationLogSpillCreationFailureRetainsMemoryPrefix(t *testing.T) {
	limits := mutationLogTestLimits(t, true)
	limits.SpillDirectory = filepath.Join(limits.SpillDirectory, "missing")
	log := mutationLogTestOpen(t, limits)
	row := mutationLogTestRow(2)
	pointDelete := storage.NewDeleteMutation(row.Target, []byte("first"))
	pointDelete.Ordinal = 1
	if err := log.append(t.Context(), []storage.Mutation{pointDelete}); err != nil {
		t.Fatal(err)
	}
	if err := log.append(t.Context(), []storage.Mutation{row}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spill creation = %v", err)
	}
	if err := os.Mkdir(limits.SpillDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := log.append(t.Context(), []storage.Mutation{row}); err != nil {
		t.Fatal(err)
	}
	mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1, 2}, []storage.MutationOp{storage.MutationDelete, storage.MutationUpsert})
}

func TestMutationLogSavepointsAreOwnerBoundAndReusable(t *testing.T) {
	log := mutationLogTestOpen(t, mutationLogTestLimits(t, false))
	other := mutationLogTestOpen(t, mutationLogTestLimits(t, false))
	position, err := log.savepoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.rollback(t.Context(), position); !errors.Is(err, errMutationLogPosition) {
		t.Fatalf("foreign savepoint = %v", err)
	}
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := log.rollback(ctx, position); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled rollback = %v", err)
	}
	if err := log.rollback(t.Context(), position); err != nil {
		t.Fatal(err)
	}
	if err := log.rollback(t.Context(), position); err != nil {
		t.Fatalf("repeated rollback = %v", err)
	}
	mutationLogTestOrder(t, mutationLogTestSeal(t, log), nil, nil)
}

func TestMutationLogCloseRetainsExistingCursors(t *testing.T) {
	for _, spilled := range []bool{false, true} {
		name := "memory"
		if spilled {
			name = "spill"
		}
		t.Run(name, func(t *testing.T) {
			log := mutationLogTestOpen(t, mutationLogTestLimits(t, spilled))
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
				t.Fatal(err)
			}
			sequence := mutationLogTestSeal(t, log)
			first, second := mutationLogTestCursor(t, sequence), mutationLogTestCursor(t, sequence)
			path := log.path
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := sequence.Open(t.Context()); !errors.Is(err, storage.ErrClosed) {
				t.Fatalf("open after owner close = %v", err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			if spilled {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("live cursor lost spill file: %v", err)
				}
			}
			got, err := second.Next(t.Context())
			if err != nil || got.Ordinal != 1 {
				t.Fatalf("read after owner close = %d, %v", got.Ordinal, err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
			if spilled {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("last cursor left spill file: %v", err)
				}
			}
			if err := log.Close(); err != nil {
				t.Fatalf("repeated close: %v", err)
			}
		})
	}
}

func TestMutationLogResourceBounds(t *testing.T) {
	t.Run("explicit directory", func(t *testing.T) {
		limits := mutationLogTestLimits(t, false)
		limits.SpillDirectory = ""
		if _, err := newMutationLog(limits); !errors.Is(err, errMutationLogDirectory) {
			t.Fatalf("missing directory = %v", err)
		}
	})
	t.Run("disabled spill", func(t *testing.T) {
		limits := mutationLogTestLimits(t, true)
		limits.DisableSpill, limits.SpillDirectory = true, ""
		log := mutationLogTestOpen(t, limits)
		err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)})
		var sqlError *model.Error
		if !errors.As(err, &sqlError) || sqlError.Code != "53200" {
			t.Fatalf("disabled spill = %v, want SQLSTATE 53200", err)
		}
		if count := mutationLogTestSeal(t, log).Count(); count != 0 {
			t.Fatalf("failed statement retained %d rows", count)
		}
	})
	t.Run("inclusive spill framing", func(t *testing.T) {
		limits := mutationLogTestLimits(t, true)
		row := mutationLogTestRow(1)
		size, err := storage.EncodedMutationSize(row)
		if err != nil {
			t.Fatal(err)
		}
		limits.SpillBytes = size + mutationLogHeaderBytes
		log := mutationLogTestOpen(t, limits)
		if err := log.append(t.Context(), []storage.Mutation{row}); err != nil {
			t.Fatalf("exact framed budget: %v", err)
		}
		if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(2)}); !errors.Is(err, errMutationLogBudget) {
			t.Fatalf("spill over budget = %v", err)
		}
		mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1}, []storage.MutationOp{storage.MutationUpsert})
	})
	t.Run("oversized frame", func(t *testing.T) {
		limits := mutationLogTestLimits(t, false)
		limits.CursorBytes = mutationLogCursorOverhead() + 64
		log := mutationLogTestOpen(t, limits)
		if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); !errors.Is(err, errMutationLogBudget) {
			t.Fatalf("oversized frame = %v", err)
		}
	})
	t.Run("decoded allocation", func(t *testing.T) {
		row := mutationLogTestRow(1)
		row.Values = []model.Value{{Type: model.TypeComposite, ArrayVal: make([]model.Value, 100)}}
		for i := range row.Values[0].ArrayVal {
			row.Values[0].ArrayVal[i] = model.Value{Type: model.TypeInt64, IsNull: true}
		}
		limits := mutationLogTestLimits(t, false)
		limits.CursorBytes = mutationLogCursorOverhead() + 2048
		log := mutationLogTestOpen(t, limits)
		if err := log.append(t.Context(), []storage.Mutation{row}); err != nil {
			t.Fatal(err)
		}
		cursor := mutationLogTestCursor(t, mutationLogTestSeal(t, log))
		if _, err := cursor.Next(t.Context()); !errors.Is(err, storage.ErrDecodeBudget) || !errors.Is(err, errMutationLogBudget) {
			t.Fatalf("decoded values outside cursor budget = %v", err)
		}
	})
}

func TestMutationLogCorruptSpillIsNotEndOfSequence(t *testing.T) {
	for _, kind := range []string{"checksum", "serial", "length", "truncated", "closed file"} {
		t.Run(kind, func(t *testing.T) {
			log, err := newMutationLog(mutationLogTestLimits(t, true))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := log.Close(); err != nil && (kind != "closed file" || !errors.Is(err, os.ErrClosed)) {
					t.Error(err)
				}
			})
			if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
				t.Fatal(err)
			}
			sequence := mutationLogTestSeal(t, log)
			file := log.file
			switch kind {
			case "checksum":
				_, err = file.WriteAt([]byte{0xff}, mutationLogHeaderBytes)
			case "serial":
				_, err = file.WriteAt([]byte{2}, 12)
			case "length":
				var length [8]byte
				binary.LittleEndian.PutUint64(length[:], math.MaxUint64)
				_, err = file.WriteAt(length[:], 0)
			case "truncated":
				err = file.Truncate(mutationLogHeaderBytes + 1)
			case "closed file":
				err = file.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			cursor := mutationLogTestCursor(t, sequence)
			if _, err := cursor.Next(t.Context()); err == nil || err == io.EOF {
				t.Fatalf("corruption returned %v, want a non-EOF error", err)
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			path := log.path
			err = log.Close()
			if kind == "closed file" {
				if !errors.Is(err, os.ErrClosed) {
					t.Fatalf("closed file cleanup = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("corrupt source left spill file: %v", err)
			}
		})
	}
}

func TestMutationLogCleanupJoinsErrorsIdempotently(t *testing.T) {
	log, err := newMutationLog(mutationLogTestLimits(t, true))
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("injected close failure")
	t.Cleanup(func() {
		if err := log.Close(); err != nil && !errors.Is(err, closeFailure) {
			t.Error(err)
		}
	})
	if err := log.append(t.Context(), []storage.Mutation{mutationLogTestRow(1)}); err != nil {
		t.Fatal(err)
	}
	log.file = &mutationLogFaultFile{mutationLogFile: log.file, closeError: closeFailure}
	path := log.path
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupied"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	first := log.Close()
	var pathError *os.PathError
	if !errors.Is(first, closeFailure) || !errors.As(first, &pathError) {
		t.Fatalf("cleanup = %v, want close and remove errors", first)
	}
	if second := log.Close(); second != first {
		t.Fatalf("repeated close changed cleanup result: %v / %v", first, second)
	}
}

func TestMutationLogSpillsBeforeCanonicalValidationScratch(t *testing.T) {
	limits := mutationLogTestLimits(t, true)
	log := mutationLogTestOpen(t, limits)
	row := mutationLogTestRow(1)
	row.Values[0].BytesVal = nil
	size, err := storage.EncodedMutationSize(row)
	if err != nil {
		t.Fatal(err)
	}
	row.Values[0].BytesVal = make([]byte, limits.MemoryBytes-log.memoryOverhead-size-mutationLogHeaderBytes-128)
	if err := log.append(t.Context(), []storage.Mutation{row}); err != nil {
		t.Fatal(err)
	}
	wide := mutationLogTestRow(2)
	wide.ColumnIDs = make([]model.ColumnID, 100)
	wide.Values = make([]model.Value, 100)
	for i := range wide.ColumnIDs {
		wide.ColumnIDs[i] = model.ColumnID(100 - i)
		wide.Values[i] = model.Value{Type: model.TypeInt64, IsNull: true}
	}
	if err := log.append(t.Context(), []storage.Mutation{wide}); err != nil {
		t.Fatalf("spill for validation workspace: %v", err)
	}
	mutationLogTestOrder(t, mutationLogTestSeal(t, log), []uint64{1, 2}, []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert})
}
