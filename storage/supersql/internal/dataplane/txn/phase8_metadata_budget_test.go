package txn

import (
	"context"
	"errors"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func phase8TargetPolicy(t *testing.T, objectID uint64) (*RowNormalizer, storage.ObjectRef) {
	t.Helper()
	target := storage.ObjectRef{
		ObjectID:          model.ObjectID(objectID),
		Generation:        1,
		DescriptorVersion: 1,
		EncodingVersion:   1,
	}
	policy := contractPolicy(t, target, ColumnRule{
		Layout: storage.ColumnLayout{
			ID:   1,
			Type: model.Type{ID: 20, Storage: model.TypeInt64},
		},
	})
	return policy, target
}

// TestPhase8SavepointRollbackNewTargetNoMetadataLeak verifies that repeatedly
// caching a new target inside a savepoint and rolling back never leaks
// transaction.metadataUsed or exhausts metadataLimit with SQLSTATE 53200.
func TestPhase8SavepointRollbackNewTargetNoMetadataLeak(t *testing.T) {
	const iterations = 10_000

	t.Run("fresh_savepoint_and_apply_mutations", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		// metadataLimit = MemoryBytes/8 = 32 KiB.
		// Previously 10,000 * 2 * sizeof(transactionTarget) (~1.44 MiB) far exceeded 32 KiB.
		options.MemoryBytes = 256 << 10
		tx := contractBegin(t, fixture.manager, options)

		initialMetadata := tx.metadataUsed
		if uint64(iterations)*2*uint64(unsafe.Sizeof(transactionTarget{})) <= tx.metadataLimit-initialMetadata {
			t.Fatalf("test budget too loose: leak=%d <= headroom=%d",
				uint64(iterations)*2*uint64(unsafe.Sizeof(transactionTarget{})),
				tx.metadataLimit-initialMetadata)
		}

		mutation := storage.NewUpsertMutation(
			fixture.request.Target,
			contractKey(2),
			fixture.request.ColumnIDs,
			[]model.Value{model.Int64Val(42)},
		)

		for i := range iterations {
			sp, err := tx.Savepoint(t.Context())
			if err != nil {
				t.Fatalf("iteration %d Savepoint failed (metadataUsed=%d limit=%d): %v", i, tx.metadataUsed, tx.metadataLimit, err)
			}
			if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mutation}); err != nil {
				t.Fatalf("iteration %d ApplyMutations failed (metadataUsed=%d limit=%d): %v", i, tx.metadataUsed, tx.metadataLimit, err)
			}
			if len(tx.targets) != 1 {
				t.Fatalf("iteration %d len(targets) = %d, want 1", i, len(tx.targets))
			}
			if err := tx.RollbackTo(t.Context(), sp); err != nil {
				t.Fatalf("iteration %d RollbackTo failed: %v", i, err)
			}
			if len(tx.targets) != 0 {
				t.Fatalf("iteration %d len(targets) after rollback = %d, want 0", i, len(tx.targets))
			}
			if err := tx.ReleaseSavepoint(t.Context(), sp); err != nil {
				t.Fatalf("iteration %d ReleaseSavepoint failed: %v", i, err)
			}
			if tx.metadataUsed != initialMetadata {
				t.Fatalf("iteration %d metadataUsed = %d, want %d", i, tx.metadataUsed, initialMetadata)
			}
		}
	})

	t.Run("reused_outer_savepoint_with_retained_prefix_target", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		policy1, target1 := phase8TargetPolicy(t, 100)
		policy2, target2 := phase8TargetPolicy(t, 101)
		options := contractOptions(t, policy1, policy2)
		options.MemoryBytes = 256 << 10
		tx := contractBegin(t, fixture.manager, options)

		// Retain target1 before taking the savepoint.
		mut1 := storage.NewUpsertMutation(target1, contractKey(1), []model.ColumnID{1}, []model.Value{model.Int64Val(1)})
		if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut1}); err != nil {
			t.Fatalf("initial ApplyMutations failed: %v", err)
		}
		afterFirstTarget := tx.metadataUsed

		sp, err := tx.Savepoint(t.Context())
		if err != nil {
			t.Fatalf("Savepoint failed: %v", err)
		}
		afterSavepoint := tx.metadataUsed

		mut2 := storage.NewUpsertMutation(target2, contractKey(2), []model.ColumnID{1}, []model.Value{model.Int64Val(2)})
		for i := range iterations {
			if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut2}); err != nil {
				t.Fatalf("iteration %d ApplyMutations failed (metadataUsed=%d limit=%d): %v", i, tx.metadataUsed, tx.metadataLimit, err)
			}
			if len(tx.targets) != 2 {
				t.Fatalf("iteration %d len(targets) = %d, want 2", i, len(tx.targets))
			}
			if err := tx.RollbackTo(t.Context(), sp); err != nil {
				t.Fatalf("iteration %d RollbackTo failed: %v", i, err)
			}
			if len(tx.targets) != 1 {
				t.Fatalf("iteration %d len(targets) after rollback = %d, want 1", i, len(tx.targets))
			}
			if tx.metadataUsed != afterSavepoint {
				t.Fatalf("iteration %d metadataUsed = %d, want %d", i, tx.metadataUsed, afterSavepoint)
			}
		}

		if err := tx.ReleaseSavepoint(t.Context(), sp); err != nil {
			t.Fatalf("ReleaseSavepoint failed: %v", err)
		}
		if tx.metadataUsed != afterFirstTarget {
			t.Fatalf("metadataUsed after ReleaseSavepoint = %d, want %d", tx.metadataUsed, afterFirstTarget)
		}
	})

	t.Run("admit_created_target_loop", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		policy, _ := phase8TargetPolicy(t, 200)
		options := contractOptions(t)
		options.MemoryBytes = 256 << 10
		tx := contractBegin(t, fixture.manager, options)
		initialMetadata := tx.metadataUsed

		sp, err := tx.Savepoint(t.Context())
		if err != nil {
			t.Fatalf("Savepoint failed: %v", err)
		}
		afterSavepoint := tx.metadataUsed

		for i := range iterations {
			if err := tx.Statement(t.Context(), func(ctx context.Context) error {
				return tx.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes())
			}); err != nil {
				t.Fatalf("iteration %d AdmitCreatedTarget failed (metadataUsed=%d limit=%d): %v", i, tx.metadataUsed, tx.metadataLimit, err)
			}
			if err := tx.RollbackTo(t.Context(), sp); err != nil {
				t.Fatalf("iteration %d RollbackTo failed: %v", i, err)
			}
			if tx.metadataUsed != afterSavepoint {
				t.Fatalf("iteration %d metadataUsed = %d, want %d", i, tx.metadataUsed, afterSavepoint)
			}
		}

		if err := tx.ReleaseSavepoint(t.Context(), sp); err != nil {
			t.Fatalf("ReleaseSavepoint failed: %v", err)
		}
		if tx.metadataUsed != initialMetadata {
			t.Fatalf("metadataUsed after release = %d, want %d", tx.metadataUsed, initialMetadata)
		}
	})
}

// TestPhase8RetainedTargetsStillEnforceMetadataLimit verifies that retained
// targets (not rolled back) still accurately accumulate metadataUsed and trigger
// SQLSTATE 53200 once metadataLimit is exhausted, and that rolling back restores
// the budget so new targets can be admitted again.
func TestPhase8RetainedTargetsStillEnforceMetadataLimit(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractOptions(t)
	options.MemoryBytes = 256 << 10 // metadataLimit = 32 KiB
	tx := contractBegin(t, fixture.manager, options)
	initialMetadata := tx.metadataUsed

	sp, err := tx.Savepoint(t.Context())
	if err != nil {
		t.Fatalf("Savepoint failed: %v", err)
	}
	afterSavepoint := tx.metadataUsed

	var retainedCount int
	var hitBudget bool
	prevUsed := afterSavepoint
	for id := uint64(1); id <= 1000; id++ {
		policy, _ := phase8TargetPolicy(t, 1000+id)
		err := tx.Statement(t.Context(), func(ctx context.Context) error {
			return tx.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes())
		})
		if err != nil {
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "53200" {
				t.Fatalf("target %d failed with %v, want SQLSTATE 53200", id, err)
			}
			hitBudget = true
			break
		}
		if tx.metadataUsed <= prevUsed {
			t.Fatalf("target %d metadataUsed did not increase: prev=%d now=%d", id, prevUsed, tx.metadataUsed)
		}
		if tx.metadataUsed > tx.metadataLimit {
			t.Fatalf("target %d metadataUsed=%d exceeded limit=%d", id, tx.metadataUsed, tx.metadataLimit)
		}
		prevUsed = tx.metadataUsed
		retainedCount++
	}
	if !hitBudget {
		t.Fatalf("retained %d targets without hitting metadataLimit=%d (metadataUsed=%d)", retainedCount, tx.metadataLimit, tx.metadataUsed)
	}
	if retainedCount == 0 {
		t.Fatal("expected at least one target to fit within metadataLimit")
	}

	// Rolling back all retained targets must restore metadataUsed exactly,
	// allowing the same number of targets to be admitted again.
	if err := tx.RollbackTo(t.Context(), sp); err != nil {
		t.Fatalf("RollbackTo failed: %v", err)
	}
	if tx.metadataUsed != afterSavepoint {
		t.Fatalf("metadataUsed after RollbackTo = %d, want %d", tx.metadataUsed, afterSavepoint)
	}
	if len(tx.targets) != 0 {
		t.Fatalf("len(targets) after RollbackTo = %d, want 0", len(tx.targets))
	}

	for id := uint64(1); id <= uint64(retainedCount); id++ {
		policy, _ := phase8TargetPolicy(t, 5000+id)
		if err := tx.Statement(t.Context(), func(ctx context.Context) error {
			return tx.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes())
		}); err != nil {
			t.Fatalf("re-admitting target %d after rollback failed: %v", id, err)
		}
	}

	if err := tx.RollbackTo(t.Context(), sp); err != nil {
		t.Fatalf("second RollbackTo failed: %v", err)
	}
	if err := tx.ReleaseSavepoint(t.Context(), sp); err != nil {
		t.Fatalf("ReleaseSavepoint failed: %v", err)
	}
	if tx.metadataUsed != initialMetadata {
		t.Fatalf("final metadataUsed = %d, want %d", tx.metadataUsed, initialMetadata)
	}
}

// TestPhase8LargeRolledBackBurstShrinksBackingCapacity verifies that a large
// burst of rolled-back targets or savepoints shrinks oversized slice backing
// arrays (both when empty and when a small prefix remains) while refunding
// metadataUsed accurately.
func TestPhase8LargeRolledBackBurstShrinksBackingCapacity(t *testing.T) {
	t.Run("targets_burst_to_empty_and_prefix", func(t *testing.T) {
		for _, retainPrefix := range []bool{false, true} {
			fixture := newCommitFixture(t, nil)
			options := contractOptions(t)
			options.MemoryBytes = 8 << 20 // metadataLimit = 1 MiB, fits >128 targets easily
			tx := contractBegin(t, fixture.manager, options)

			wantTargets := 0
			if retainPrefix {
				prefixPolicy, _ := phase8TargetPolicy(t, 1)
				if err := tx.Statement(t.Context(), func(ctx context.Context) error {
					return tx.AdmitCreatedTarget(ctx, prefixPolicy, prefixPolicy.RetainedBytes())
				}); err != nil {
					t.Fatalf("prefix AdmitCreatedTarget failed: %v", err)
				}
				wantTargets = 1
			}

			sp, err := tx.Savepoint(t.Context())
			if err != nil {
				t.Fatalf("Savepoint failed: %v", err)
			}
			afterSavepoint := tx.metadataUsed

			const burst = 128
			for id := range burst {
				policy, _ := phase8TargetPolicy(t, uint64(100+id))
				if err := tx.Statement(t.Context(), func(ctx context.Context) error {
					return tx.AdmitCreatedTarget(ctx, policy, policy.RetainedBytes())
				}); err != nil {
					t.Fatalf("burst %d AdmitCreatedTarget failed: %v", id, err)
				}
			}
			if cap(tx.targets) <= 64 {
				t.Fatalf("expected cap(targets) > 64 after burst, got %d", cap(tx.targets))
			}

			if err := tx.RollbackTo(t.Context(), sp); err != nil {
				t.Fatalf("RollbackTo failed: %v", err)
			}
			if len(tx.targets) != wantTargets {
				t.Fatalf("len(targets) = %d, want %d", len(tx.targets), wantTargets)
			}
			if cap(tx.targets) > wantTargets {
				t.Fatalf("cap(targets) = %d not shrunk after rollback (retainPrefix=%v)", cap(tx.targets), retainPrefix)
			}
			if tx.metadataUsed != afterSavepoint {
				t.Fatalf("metadataUsed = %d, want %d", tx.metadataUsed, afterSavepoint)
			}
		}
	})

	t.Run("marks_burst_with_retained_outer_savepoint", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractOptions(t)
		options.MemoryBytes = 8 << 20
		tx := contractBegin(t, fixture.manager, options)

		outer, err := tx.Savepoint(t.Context())
		if err != nil {
			t.Fatalf("outer Savepoint failed: %v", err)
		}
		afterOuter := tx.metadataUsed

		const burst = 128
		for i := range burst {
			if _, err := tx.Savepoint(t.Context()); err != nil {
				t.Fatalf("nested Savepoint %d failed: %v", i, err)
			}
		}
		if cap(tx.marks) <= 64 {
			t.Fatalf("expected cap(marks) > 64 after burst, got %d", cap(tx.marks))
		}

		if err := tx.RollbackTo(t.Context(), outer); err != nil {
			t.Fatalf("RollbackTo outer failed: %v", err)
		}
		if len(tx.marks) != 1 {
			t.Fatalf("len(marks) = %d, want 1", len(tx.marks))
		}
		if cap(tx.marks) > 1 {
			t.Fatalf("cap(marks) = %d not shrunk after rollback to outer savepoint", cap(tx.marks))
		}
		if tx.metadataUsed != afterOuter {
			t.Fatalf("metadataUsed = %d, want %d", tx.metadataUsed, afterOuter)
		}
	})
}

// TestPhase8StatementRollbackRefundsNewTargetMetadata verifies that failed
// statements (rolling back via rollbackStatement -> restore) also refund target
// metadata without leaking.
func TestPhase8StatementRollbackRefundsNewTargetMetadata(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	options.MemoryBytes = 256 << 10
	tx := contractBegin(t, fixture.manager, options)
	initialMetadata := tx.metadataUsed

	mutation := storage.NewUpsertMutation(
		fixture.request.Target,
		contractKey(3),
		fixture.request.ColumnIDs,
		[]model.Value{model.Int64Val(77)},
	)
	sentinel := errors.New("abort statement")

	for i := range 2_000 {
		err := tx.Statement(t.Context(), func(ctx context.Context) error {
			if err := tx.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("iteration %d Statement error = %v, want sentinel", i, err)
		}
		if len(tx.targets) != 0 {
			t.Fatalf("iteration %d len(targets) = %d, want 0", i, len(tx.targets))
		}
		if tx.metadataUsed != initialMetadata {
			t.Fatalf("iteration %d metadataUsed = %d, want %d", i, tx.metadataUsed, initialMetadata)
		}
	}
}
