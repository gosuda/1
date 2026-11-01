package modify_test

import (
	"cmp"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

func TestParsedUpdateFromReturningIncludesSourceAndImages(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(9, 90))
	portal := parsedMutation(t, s, `UPDATE items AS i SET value=i.value+src.delta
		FROM (VALUES (1::bigint,7::bigint,'chosen'::text)) src(match_id,delta,label)
		WHERE i.id=src.match_id
		RETURNING WITH (OLD AS o, NEW AS n) label,o.value,n.value,i.value,src.*,*`)
	want := [][]model.Value{{
		model.StringVal("chosen"), model.Int64Val(10), model.Int64Val(17), model.Int64Val(17),
		model.Int64Val(1), model.Int64Val(7), model.StringVal("chosen"),
		model.Int64Val(1), model.Int64Val(17),
		model.Int64Val(1), model.Int64Val(7), model.StringVal("chosen"),
	}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("UPDATE FROM RETURNING = %v, want %v", got, want)
	}
	s.require(t, 1, 17)
	s.require(t, 9, 90)
}

func TestParsedDeleteUsingReturningIncludesSourceAndImages(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(9, 90))
	portal := parsedMutation(t, s, `DELETE FROM items AS i
		USING (VALUES (1::bigint,7::bigint,'chosen'::text)) src(match_id,delta,label)
		WHERE i.id=src.match_id
		RETURNING WITH (OLD AS o, NEW AS n) label,o.value,n.value,i.value,src.*,*`)
	want := [][]model.Value{{
		model.StringVal("chosen"), model.Int64Val(10), model.NullVal(model.TypeInt64), model.Int64Val(10),
		model.Int64Val(1), model.Int64Val(7), model.StringVal("chosen"),
		model.Int64Val(1), model.Int64Val(10),
		model.Int64Val(1), model.Int64Val(7), model.StringVal("chosen"),
	}}
	if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("DELETE USING RETURNING = %v, want %v", got, want)
	}
	s.absent(t, 1)
	s.require(t, 9, 90)
}

func TestParsedUpdateFromReturningKeepsSelfJoinSourceBeforeKeyMoves(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(9, 90))
	portal := parsedMutation(t, s, `UPDATE items AS i SET id=i.id+10,value=src.value+100
		FROM items AS src WHERE i.id<9 AND src.id=3-i.id
		RETURNING old.id,old.value,new.id,new.value,i.id,i.value,src.id,src.value`, func(env *execution.Environment) {
		env.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 8 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4}
	})
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10), model.Int64Val(11), model.Int64Val(120), model.Int64Val(11), model.Int64Val(120), model.Int64Val(2), model.Int64Val(20)},
		{model.Int64Val(2), model.Int64Val(20), model.Int64Val(12), model.Int64Val(110), model.Int64Val(12), model.Int64Val(110), model.Int64Val(1), model.Int64Val(10)},
	}
	got := defaultSQLRows(t, portal)
	slices.SortFunc(got, func(a, b []model.Value) int { return cmp.Compare(a[0].IntVal, b[0].IntVal) })
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("self-join RETURNING = %v, want %v", got, want)
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 11, 120)
	s.require(t, 12, 110)
	s.require(t, 9, 90)
}

func TestParsedReturningUsesOneDuplicateSourceWitness(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		sql     string
		choices [][]model.Value
		deleted bool
	}{
		{
			name: "update",
			sql: `UPDATE items AS i SET value=i.value+src.delta
				FROM (VALUES (1::bigint,7::bigint,70::bigint),(1::bigint,8::bigint,80::bigint)) src(match_id,delta,marker)
				WHERE i.id=src.match_id RETURNING i.id,i.value,src.delta,src.marker`,
			choices: [][]model.Value{
				{model.Int64Val(1), model.Int64Val(17), model.Int64Val(7), model.Int64Val(70)},
				{model.Int64Val(1), model.Int64Val(18), model.Int64Val(8), model.Int64Val(80)},
			},
		},
		{
			name: "delete",
			sql: `DELETE FROM items AS i
				USING (VALUES (1::bigint,7::bigint,70::bigint),(1::bigint,8::bigint,80::bigint)) src(match_id,delta,marker)
				WHERE i.id=src.match_id RETURNING i.id,i.value,src.delta,src.marker`,
			choices: [][]model.Value{
				{model.Int64Val(1), model.Int64Val(10), model.Int64Val(7), model.Int64Val(70)},
				{model.Int64Val(1), model.Int64Val(10), model.Int64Val(8), model.Int64Val(80)},
			},
			deleted: true,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(9, 90))
			portal := parsedMutation(t, s, scenario.sql)
			got := defaultSQLRows(t, portal)
			if len(got) != 1 || portal.RowsAffected() != 1 {
				t.Fatalf("duplicate matches returned %v, affected=%d; want one mutation and one source witness", got, portal.RowsAffected())
			}
			if !slices.ContainsFunc(scenario.choices, func(choice []model.Value) bool { return reflect.DeepEqual(got[0], choice) }) {
				t.Fatalf("selected source witness = %v, want one of %v", got[0], scenario.choices)
			}
			if scenario.deleted {
				s.absent(t, 1)
			} else {
				s.require(t, 1, got[0][1].IntVal)
			}
			s.require(t, 9, 90)
		})
	}
}

func TestParsedSourceReturningLaterFailureRestoresStatementWrites(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
	}{
		{
			name: "update",
			sql: `UPDATE items AS i SET id=i.id+10,value=i.value+1
				FROM (VALUES (1::bigint,1::bigint,'first'::text),(2::bigint,0::bigint,'later'::text)) src(match_id,divisor,label)
				WHERE i.id=src.match_id RETURNING src.label,new.value/src.divisor`,
		},
		{
			name: "delete",
			sql: `DELETE FROM items AS i
				USING (VALUES (1::bigint,1::bigint,'first'::text),(2::bigint,0::bigint,'later'::text)) src(match_id,divisor,label)
				WHERE i.id=src.match_id RETURNING src.label,old.value/src.divisor`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(2, 20), row(9, 90))
			portal := parsedMutation(t, s, scenario.sql)
			batch := columnar.Batch{MaxRows: 1}
			defer batch.Reset()
			err := portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
				t.Fatalf("source-dependent RETURNING error = %v, want SQLSTATE 22012", err)
			}
			if batch.Rows() != 0 || portal.RowsAffected() != 0 {
				t.Fatalf("failed RETURNING published %d rows, affected=%d", batch.Rows(), portal.RowsAffected())
			}
			s.require(t, 1, 10)
			s.require(t, 2, 20)
			s.require(t, 9, 90)
			s.absent(t, 11)
			s.absent(t, 12)
			if s.tx.State() != txn.Active {
				t.Fatalf("statement failure ended transaction: %v", s.tx.State())
			}
			requireClean(t, s)
		})
	}
}

func TestReturningSourcePreflightRejectsInvalidMappingAndReleasesInput(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		columns []int
		layouts []model.TypeLayout
	}{
		{name: "negative column", columns: []int{-1}, layouts: []model.TypeLayout{integer}},
		{name: "column past input", columns: []int{2}, layouts: []model.TypeLayout{integer}},
		{name: "duplicate column", columns: []int{0, 0}, layouts: []model.TypeLayout{integer, integer}},
		{name: "descending columns", columns: []int{1, 0}, layouts: []model.TypeLayout{integer, integer}},
		{name: "missing source layout", columns: []int{0}},
		{name: "mismatched source layout", columns: []int{0}, layouts: []model.TypeLayout{record}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.options.Operation = storage.MutationUpdate
			s.options.Source = s.sourceView(t)
			s.options.OldKeyColumns = []int{0}
			s.options.ReturningSourceColumns = scenario.columns
			s.options.ReturningInputLayouts = append(s.options.ReturningInputLayouts, scenario.layouts...)
			s.options.Returning = program(t, s.options.ReturningInputLayouts, []int{0, 1}, nil)
			input := newSource(t, s.options.InputLayouts, [][]model.Value{row(1, 10)})
			cursor, err := modify.New(t.Context(), input, s.options)
			if cursor != nil {
				t.Cleanup(func() {
					if err := cursor.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if !errors.Is(err, storage.ErrInvalidLayout) || cursor != nil {
				t.Fatalf("source preflight = %v, %v; want invalid layout", cursor, err)
			}
			if !input.closed || input.pulls != 0 {
				t.Fatalf("rejected input closed=%v pulls=%d; want closed without reading", input.closed, input.pulls)
			}
			s.absent(t, 1)
			requireClean(t, s)
		})
	}
}
