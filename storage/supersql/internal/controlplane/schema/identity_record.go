package schema

import (
	"context"
	"encoding/binary"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const identityRecordBytes = 40
const identityEncoding = "IDST0001"

type identityState struct {
	revision  uint64
	next      IdentityAllocation
	exhausted bool
}

func (s *JobStore) readIdentityState(ctx context.Context, reader JobReader) (identityState, error) {
	data, err := s.read(ctx, reader, jobLogicalKey(4, 0))
	if err != nil {
		if errors.Is(err, ErrJobRecord) {
			return identityState{}, identityCorruptError(err)
		}
		return identityState{}, err
	}
	if err := ctx.Err(); err != nil {
		return identityState{}, err
	}
	if len(data) != identityRecordBytes || string(data[:8]) != identityEncoding {
		return identityState{}, identityCorruptError(nil)
	}
	if data[36] > 1 || data[37] != 0 || data[38] != 0 || data[39] != 0 {
		return identityState{}, identityCorruptError(nil)
	}
	state := identityState{
		revision: binary.LittleEndian.Uint64(data[8:16]),
		next: IdentityAllocation{
			ObjectID: model.ObjectID(binary.LittleEndian.Uint64(data[16:24])),
			OID:      model.OID(binary.LittleEndian.Uint32(data[24:28])),
			JobID:    JobID(binary.LittleEndian.Uint64(data[28:36])),
		},
		exhausted: data[36] == 1,
	}
	if state.revision == 0 {
		return identityState{}, identityCorruptError(nil)
	}
	if state.exhausted {
		if state.revision == 1 || state.next != (IdentityAllocation{}) {
			return identityState{}, identityCorruptError(nil)
		}
		return state, nil
	}
	if state.next.ObjectID == 0 || state.next.ObjectID >= catalog.NamespaceObjectID || state.next.ObjectID == s.options.Target.ObjectID || state.next.OID == 0 || state.next.JobID == 0 {
		return identityState{}, identityCorruptError(nil)
	}
	return state, nil
}

func (s *JobStore) writeIdentityState(ctx context.Context, transaction txn.MutationApplier, operation storage.MutationOp, state identityState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var payload [identityRecordBytes]byte
	copy(payload[:8], identityEncoding)
	binary.LittleEndian.PutUint64(payload[8:16], state.revision)
	binary.LittleEndian.PutUint64(payload[16:24], uint64(state.next.ObjectID))
	binary.LittleEndian.PutUint32(payload[24:28], uint32(state.next.OID))
	binary.LittleEndian.PutUint64(payload[28:36], uint64(state.next.JobID))
	if state.exhausted {
		payload[36] = 1
	}
	mutation, err := s.mutation(ctx, operation, jobLogicalKey(4, 0), payload[:])
	if err != nil {
		return err
	}
	if uint64(cap(mutation.Key)) > s.options.KeyBytes {
		return ErrJobLimit
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}

func identityCorruptError(cause error) error {
	return &model.Error{Code: "XX001", Message: "invalid durable identity allocator record", Cause: errors.Join(storage.ErrCorruptStorage, cause)}
}
