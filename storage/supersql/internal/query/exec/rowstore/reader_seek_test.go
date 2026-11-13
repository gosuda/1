package rowstore

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSeekValidatesBoundariesWithoutMovingOnFailure(t *testing.T) {
	for _, spill := range []bool{false, true} {
		name := "memory"
		if spill {
			name = "spill"
		}
		t.Run(name, func(t *testing.T) {
			q := testQuery(t, runtime.OSFileSystem{}, runtime.QueryConfig{})
			limits := testLimits()
			if spill {
				limits.MemoryBytes = 0
			}
			s := newSpool(t, q, []model.TypeLayout{testLayout(model.TypeString)}, limits)
			var positions []Position
			for _, value := range []string{"first", "a longer second row", "third"} {
				position, err := s.AppendRow(t.Context(), []model.Value{model.StringVal(value)})
				if err != nil {
					t.Fatal(err)
				}
				positions = append(positions, position)
			}
			if err := s.Seal(t.Context()); err != nil {
				t.Fatal(err)
			}
			r := openReader(t, s)
			if err := r.Seek(t.Context(), positions[1]); err != nil {
				t.Fatal(err)
			}
			position, row, err := r.NextRow(t.Context())
			if err != nil || position != positions[1] || row[0].StrVal != "a longer second row" {
				t.Fatalf("seek second row: position=%d, row=%v, err=%v", position, row, err)
			}
			for _, invalid := range []Position{positions[0] + 1, Position(s.Bytes() + 1)} {
				if err := r.Seek(t.Context(), invalid); !errors.Is(err, ErrPosition) {
					t.Fatalf("seek invalid %d = %v", invalid, err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := r.Seek(ctx, positions[0]); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled seek = %v", err)
			}
			position, row, err = r.NextRow(t.Context())
			if err != nil || position != positions[2] || row[0].StrVal != "third" {
				t.Fatalf("failed seeks moved cursor: position=%d, row=%v, err=%v", position, row, err)
			}
			if err := r.Seek(t.Context(), Position(s.Bytes())); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.NextRow(t.Context()); err != io.EOF {
				t.Fatalf("seek sealed end = %v", err)
			}
			if err := r.Seek(t.Context(), positions[0]); err != nil {
				t.Fatal(err)
			}
			_, row, err = r.NextRow(t.Context())
			if err != nil || row[0].StrVal != "first" {
				t.Fatalf("rewind after EOF: row=%v, err=%v", row, err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if err := r.Seek(t.Context(), 0); !errors.Is(err, runtime.ErrClosed) {
				t.Fatalf("seek closed reader = %v", err)
			}
		})
	}
}
