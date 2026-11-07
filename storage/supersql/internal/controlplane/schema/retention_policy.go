package schema

import (
	"cmp"
	"context"
	"encoding/binary"
	"math"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type HistoryPolicy struct {
	Revision  uint64
	Retention time.Duration
}

var (
	errHistoryPolicyCorrupt = &model.Error{Code: "XX001", Message: "invalid durable history retention policy", Cause: storage.ErrCorruptStorage}
	errHistoryPolicyExists  = &model.Error{Code: "42710", Message: "history retention policy already exists"}
	errHistoryPolicyChanged = &model.Error{Code: "40001", Message: "history retention policy revision changed"}
)

// The policy shares the composition-owned metadata collection, not a new global
// object ID. Prefix 3 lies outside both table guards and the [2,3) job scan.
func historyPolicyKey() []byte { return jobLogicalKey(3, 0) }

// HistoryPolicy never substitutes a default for missing/corrupt durable state.
func (s *JobStore) HistoryPolicy(ctx context.Context, reader JobReader) (HistoryPolicy, error) {
	if s == nil || s.normalizer == nil || mutationPolicyNil(reader) {
		return HistoryPolicy{}, errRetentionOptions
	}
	data, err := s.read(ctx, reader, historyPolicyKey())
	if err != nil {
		return HistoryPolicy{}, err
	}
	if len(data) != 24 || string(data[:8]) != "HRST0001" {
		return HistoryPolicy{}, errHistoryPolicyCorrupt
	}
	revision := binary.LittleEndian.Uint64(data[8:16])
	duration := binary.LittleEndian.Uint64(data[16:24])
	if revision == 0 || duration == 0 || duration > math.MaxInt64 {
		return HistoryPolicy{}, errHistoryPolicyCorrupt
	}
	return HistoryPolicy{Revision: revision, Retention: time.Duration(duration)}, nil
}

// CreateHistoryPolicy stages initial database policy in the caller's statement.
// Only creation accepts zero as the 24-hour default; the caller owns commit.
func (s *JobStore) CreateHistoryPolicy(ctx context.Context, transaction txn.MutationApplier, retention time.Duration) error {
	if s == nil || s.normalizer == nil || mutationPolicyNil(transaction) || retention < 0 {
		return errRetentionOptions
	}
	retention = cmp.Or(retention, DefaultHistoryRetention)
	_, err := s.HistoryPolicy(ctx, transaction)
	if err == nil {
		return errHistoryPolicyExists
	}
	if !indexRowAbsent(err) {
		return err
	}
	return s.writeHistoryPolicy(ctx, transaction, storage.MutationInsert, HistoryPolicy{Revision: 1, Retention: retention})
}

// UpdateHistoryPolicy acquires the same exclusive key lock that conflicts with
// the coordinator's statement-held read lease. Increasing retention cannot race
// collection authorized by the previous policy, or restore already-collected data.
func (s *JobStore) UpdateHistoryPolicy(ctx context.Context, transaction txn.MutationApplier, expected uint64, retention time.Duration) error {
	if s == nil || s.normalizer == nil || mutationPolicyNil(transaction) {
		return errRetentionOptions
	}
	if expected == 0 || expected == math.MaxUint64 || retention <= 0 {
		return errRetentionOptions
	}
	current, err := s.HistoryPolicy(ctx, transaction)
	if err != nil {
		return err
	}
	if current.Revision != expected {
		return errHistoryPolicyChanged
	}
	return s.writeHistoryPolicy(ctx, transaction, storage.MutationUpdate, HistoryPolicy{Revision: expected + 1, Retention: retention})
}

func (s *JobStore) writeHistoryPolicy(ctx context.Context, transaction txn.MutationApplier, operation storage.MutationOp, policy HistoryPolicy) error {
	var payload [24]byte
	copy(payload[:8], "HRST0001")
	binary.LittleEndian.PutUint64(payload[8:16], policy.Revision)
	binary.LittleEndian.PutUint64(payload[16:24], uint64(policy.Retention))
	mutation, err := s.mutation(ctx, operation, historyPolicyKey(), payload[:])
	if err != nil {
		return err
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}
