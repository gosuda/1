package txn

import (
	"errors"
	"testing"
)

func TestCompositePrefixLocksCoverEveryKeyExtension(t *testing.T) {
	for _, test := range []struct {
		name         string
		lower, upper lockBound
		key          []byte
		conflict     bool
	}{
		{"prefix_first_extension", lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, []byte{255, 0}, true},
		{"prefix_last_extension", lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, []byte{255, 255}, true},
		{"adjacent_prefix", lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, lockBound{Key: []byte{255}, Prefix: true, Inclusive: true}, []byte{254, 255}, false},
		{"exclusive_upper_excludes_extensions", lockBound{Unbounded: true}, lockBound{Key: []byte{127}, Prefix: true}, []byte{127, 0}, false},
		{"exclusive_upper_keeps_prior_prefix", lockBound{Unbounded: true}, lockBound{Key: []byte{127}, Prefix: true}, []byte{126, 255}, true},
		{"exclusive_lower_excludes_extensions", lockBound{Key: []byte{127}, Prefix: true}, lockBound{Unbounded: true}, []byte{127, 255}, false},
		{"exclusive_lower_keeps_next_prefix", lockBound{Key: []byte{127}, Prefix: true}, lockBound{Unbounded: true}, []byte{128, 0}, true},
		{"full_lower_inside_prefix", lockBound{Key: []byte{127, 32}, Inclusive: true}, lockBound{Key: []byte{127}, Prefix: true, Inclusive: true}, []byte{127, 64}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry := testLockRegistry(t, 2, 4, 1<<20)
			reader, writer := testLockOwner(t, registry, 1), testLockOwner(t, registry, 2)
			requireLock(t, registry, reader, lockRequest{ObjectID: 1, Generation: 1, Lower: test.lower, Upper: test.upper}, lockShare)
			point := lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Key: test.key, Inclusive: true}, Upper: lockBound{Key: test.key, Inclusive: true}}
			acquired, err := registry.acquire(t.Context(), writer, point, lockUpdate, lockNoWait)
			if test.conflict {
				if acquired || !errors.Is(err, errLockUnavailable) {
					t.Fatalf("prefix lock missed insertion: acquired=%v err=%v", acquired, err)
				}
			} else if !acquired || err != nil {
				t.Fatalf("prefix lock blocked adjacent range: acquired=%v err=%v", acquired, err)
			}
		})
	}
}

func TestAdjacentPrefixIntervalsDoNotConflict(t *testing.T) {
	registry := testLockRegistry(t, 2, 4, 1<<20)
	left, right := testLockOwner(t, registry, 1), testLockOwner(t, registry, 2)
	requireLock(t, registry, left, lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true}, Upper: lockBound{Key: []byte{127}, Prefix: true, Inclusive: true}}, lockShare)
	acquired, err := registry.acquire(t.Context(), right, lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Key: []byte{127}, Prefix: true}, Upper: lockBound{Unbounded: true}}, lockUpdate, lockNoWait)
	if !acquired || err != nil {
		t.Fatalf("adjacent prefix intervals conflict: acquired=%v err=%v", acquired, err)
	}
}

func TestIntentLockAllowsDataWritesButBlocksSchemaChanges(t *testing.T) {
	registry := testLockRegistry(t, 3, 4, 1<<20)
	reader, writer, ddl := testLockOwner(t, registry, 1), testLockOwner(t, registry, 2), testLockOwner(t, registry, 3)
	intent := lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
	requireLock(t, registry, reader, intent, lockIntent)
	acquired, err := registry.acquire(t.Context(), writer, lockPoint(1), lockUpdate, lockNoWait)
	if !acquired || err != nil {
		t.Fatalf("descriptor intent blocked data write: acquired=%v err=%v", acquired, err)
	}
	if err := registry.release(writer); err != nil {
		t.Fatal(err)
	}
	changedGeneration := intent
	changedGeneration.Generation = 2
	if acquired, err := registry.acquire(t.Context(), ddl, changedGeneration, lockSchema, lockNoWait); acquired || !errors.Is(err, errLockUnavailable) {
		t.Fatalf("schema change ignored descriptor intent: acquired=%v err=%v", acquired, err)
	}
	if err := registry.release(reader); err != nil {
		t.Fatal(err)
	}
	requireLock(t, registry, ddl, changedGeneration, lockSchema)
}
