package schema

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSequenceReservationArithmetic(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     SequenceConfig
		last       int64
		called     bool
		first, end int64
		count      uint64
		code       string
	}{
		{name: "full width ascending", config: SequenceConfig{Start: math.MinInt64, Min: math.MinInt64, Max: math.MaxInt64, Increment: 1, Cache: math.MaxInt64}, last: math.MinInt64, first: math.MinInt64, end: -2, count: math.MaxInt64},
		{name: "minimum increment", config: SequenceConfig{Start: math.MaxInt64, Min: math.MinInt64, Max: math.MaxInt64, Increment: math.MinInt64, Cache: 8}, last: math.MaxInt64, first: math.MaxInt64, end: -1, count: 2},
		{name: "descending called minimum increment", config: SequenceConfig{Start: math.MaxInt64, Min: math.MinInt64, Max: math.MaxInt64, Increment: math.MinInt64, Cache: 8}, last: math.MaxInt64, called: true, first: -1, end: -1, count: 1},
		{name: "ascending exhausted", config: SequenceConfig{Start: 1, Min: 1, Max: math.MaxInt64, Increment: 1, Cache: 2}, last: math.MaxInt64, called: true, code: "2200H"},
		{name: "descending exhausted", config: SequenceConfig{Start: -1, Min: math.MinInt64, Max: -1, Increment: -1, Cache: 2}, last: math.MinInt64, called: true, code: "2200H"},
		{name: "ascending cycle begins at minimum", config: SequenceConfig{Start: 3, Min: 1, Max: 9, Increment: 3, Cache: 8, Cycle: true}, last: 9, called: true, first: 1, end: 7, count: 3},
		{name: "descending cycle begins at maximum", config: SequenceConfig{Start: 3, Min: 1, Max: 9, Increment: -3, Cache: 8, Cycle: true}, last: 1, called: true, first: 9, end: 3, count: 3},
		{name: "cache stops before wrap", config: SequenceConfig{Start: 1, Min: 1, Max: 9, Increment: 3, Cache: 8, Cycle: true}, last: 7, first: 7, end: 7, count: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := SequenceState{Incarnation: 1, Config: test.config, Last: test.last, Called: test.called, Revision: 2}
			next, result, err := reserveSequence(state)
			if test.code != "" {
				var sqlerr *model.Error
				if !errors.As(err, &sqlerr) || sqlerr.Code != test.code {
					t.Fatalf("error = %v, want %s", err, test.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.First != test.first || result.Last != test.end || result.Count != test.count {
				t.Fatalf("reservation = %+v, want first=%d last=%d count=%d", result, test.first, test.end, test.count)
			}
			if next.Last != result.Last || !next.Called {
				t.Fatalf("next durable state = %+v", next)
			}
		})
	}
}

func TestSequenceDecodeRejectsCorruption(t *testing.T) {
	state := SequenceState{Incarnation: 7, Revision: 1, Config: SequenceConfig{Start: 1, Min: 1, Max: 10, Increment: 1, Cache: 2}, Last: 1}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }},
		{"trailing", func(b []byte) []byte { return append(b, 0) }},
		{"wrong identity", func(b []byte) []byte { b[8] = 8; return b }},
		{"unknown version", func(b []byte) []byte { b[7] = '2'; return b }},
		{"invalid boolean", func(b []byte) []byte { b[73] = 2; return b }},
		{"reserved byte", func(b []byte) []byte { b[79] = 1; return b }},
		{"zero increment", func(b []byte) []byte { clear(b[32:40]); return b }},
		{"out of range last", func(b []byte) []byte { b[64] = 11; return b }},
		{"impossible initial state", func(b []byte) []byte { b[73] = 1; return b }},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeSequence(test.mutate(encodeSequence(state)), 7, 8)
			if !errors.Is(err, storage.ErrCorruptStorage) {
				t.Fatalf("decode error = %v, want corrupt storage", err)
			}
		})
	}
}

func TestSequenceDurableReservationAndReclamation(t *testing.T) {
	f := newIndexRestartFixture(t)
	s, err := NewSequenceStore(t.Context(), SequenceStoreOptions{Target: storage.ObjectRef{ObjectID: 90, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.keys, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 1, MaxCache: 8})
	if err != nil {
		t.Fatal(err)
	}
	run := func(commit bool, work func(context.Context, txn.MutationApplier) error) error {
		options := f.options
		options.Resolver = s
		transaction, err := f.manager.Begin(t.Context(), options)
		if err != nil {
			return err
		}
		defer func() {
			if err := transaction.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		if err := transaction.Statement(t.Context(), func(ctx context.Context) error { return work(ctx, transaction) }); err != nil {
			return err
		}
		if commit {
			_, err = transaction.Commit(t.Context())
		}
		return err
	}
	if err := run(true, s.InitializeAllocator); err != nil {
		t.Fatal(err)
	}
	config := SequenceConfig{Start: 1, Min: 1, Max: 20, Increment: 1, Cache: 3}
	var original SequenceState
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		var err error
		original, err = s.Initialize(ctx, tx, config)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Initialize(ctx, tx, config)
		return err
	}); err == nil {
		t.Fatal("store exceeded incarnation bound")
	}
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		return s.Reclaim(ctx, tx, original.Incarnation)
	}); err == nil {
		t.Fatal("reclaimed a live incarnation")
	}
	if err := run(false, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Set(ctx, tx, original.Incarnation, 15, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reserve := func(want int64) {
		t.Helper()
		if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
			r, err := s.Reserve(ctx, tx, original.Incarnation)
			if err == nil && r.First != want {
				t.Fatalf("reservation first = %d, want %d", r.First, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	reserve(1)
	f.reopen(t)
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Set(ctx, tx, original.Incarnation, 21, true)
		return err
	}); err == nil {
		t.Fatal("setval accepted an out-of-range value")
	}
	reserve(4)
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Set(ctx, tx, original.Incarnation, 10, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reserve(10)
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Set(ctx, tx, original.Incarnation, 10, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reserve(11)
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		return s.Retire(ctx, tx, original.Incarnation)
	}); err != nil {
		t.Fatal(err)
	}
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		_, err := s.Reserve(ctx, tx, original.Incarnation)
		return err
	}); err == nil {
		t.Fatal("reserved a retired incarnation")
	}
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var retired []SequenceIncarnation
	err = errors.Join(s.Scan(t.Context(), view, func(state SequenceState) error {
		if state.Retired {
			retired = append(retired, state.Incarnation)
		}
		return nil
	}), view.Close())
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0] != original.Incarnation {
		t.Fatalf("retired incarnations = %v, want [%d]", retired, original.Incarnation)
	}
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		return s.Reclaim(ctx, tx, original.Incarnation)
	}); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	if err := run(true, func(ctx context.Context, tx txn.MutationApplier) error {
		next, err := s.Initialize(ctx, tx, config)
		if err == nil && next.Incarnation <= original.Incarnation {
			t.Fatalf("reused committed incarnation %d", next.Incarnation)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
