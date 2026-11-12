package model_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestTimestampArithmeticPreservesPositiveRange(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		start      model.Timestamp
		delta      time.Duration
		want       model.Timestamp
		outOfRange bool
	}{
		{"upper boundary", math.MaxInt64 - 1, 1, math.MaxInt64, false},
		{"upper overflow", math.MaxInt64, 1, 0, true},
		{"lower boundary", 2, -1, 1, false},
		{"reserved zero", 1, -1, 0, true},
		{"minimum duration", math.MaxInt64, time.Duration(math.MinInt64), 0, true},
		{"maximum backwards distance", math.MaxInt64, -time.Duration(math.MaxInt64 - 1), 1, false},
		{"invalid origin", 0, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.start.Add(tc.delta)
			if tc.outOfRange {
				if !errors.Is(err, model.ErrTimestampRange) {
					t.Fatalf("got (%d,%v); want range error", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got (%d,%v); want %d", got, err, tc.want)
			}
		})
	}
}
