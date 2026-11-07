package storage_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type corruptTerminalSequence struct{ batch storage.MutationBatch }

func (sequence corruptTerminalSequence) Count() uint64 { return sequence.batch.Count() }
func (sequence corruptTerminalSequence) Open(ctx context.Context) (storage.MutationCursor, error) {
	cursor, err := sequence.batch.OpenMutations(ctx)
	if err != nil {
		return nil, err
	}
	return corruptTerminalCursor{MutationCursor: cursor}, nil
}

type corruptTerminalCursor struct{ storage.MutationCursor }

func (cursor corruptTerminalCursor) Next(ctx context.Context) (storage.Mutation, error) {
	mutation, err := cursor.MutationCursor.Next(ctx)
	if err == io.EOF {
		return storage.Mutation{}, errors.Join(storage.ErrCorruptStorage, io.EOF)
	}
	return mutation, err
}

func TestDurableBatchRejectsWrappedEOFFromWriteSet(t *testing.T) {
	batch := emptyArrayBatch()
	sealLayoutBatch(t, &batch)
	batch.Sequence = corruptTerminalSequence{batch: batch}
	batch.Mutations = nil
	if err := batch.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("write-set terminal corruption became successful validation: %v", err)
	}
}
