package exec_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLNumericSeriesDisplayScale(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want []string
	}{
		{"fractional", `SELECT n::text FROM generate_series(0.1::numeric,0.4::numeric,0.10::numeric) g(n)`, []string{"0.1", "0.20", "0.30", "0.40"}},
		{"default step", `SELECT n::text FROM generate_series(1.00::numeric,3.0::numeric) g(n)`, []string{"1.00", "2.00", "3.00"}},
		{"descending", `SELECT n::text FROM generate_series(0.30::numeric,-0.10::numeric,-0.2::numeric) g(n)`, []string{"0.30", "0.10", "-0.10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := statefulEnvironment(t)
			env.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			rows := make([][]model.Value, len(tc.want))
			for i, value := range tc.want {
				rows[i] = []model.Value{model.StringVal(value)}
			}
			assertAnyValueRows(t, portal, []model.TypeID{25}, rows)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLNumericSeriesLateralOrdinality(t *testing.T) {
	graph := sqlGraph(t, `SELECT n::text,ord FROM (VALUES(0.2::numeric),(0.4::numeric)) v(stop),generate_series(0.2::numeric,v.stop,0.2::numeric) WITH ORDINALITY g(n,ord)`, nil)
	env := applyEnvironment(t)
	env.Limits.BatchRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{
		{model.StringVal("0.2"), model.Int64Val(1)},
		{model.StringVal("0.2"), model.Int64Val(1)},
		{model.StringVal("0.4"), model.Int64Val(2)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericSeriesStrictNullSuppressesInvalidStep(t *testing.T) {
	graph := sqlGraph(t, `SELECT count(*) FROM generate_series(NULL::numeric,1::numeric,0::numeric)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{20}, [][]model.Value{{model.Int64Val(0)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericSeriesInvalidStepsReleaseResources(t *testing.T) {
	for _, step := range []string{"0", "NaN", "Infinity"} {
		t.Run(step, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT * FROM generate_series(2::numeric,1::numeric,'`+step+`'::numeric)`, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlError *model.Error
			if !errors.As(err, &sqlError) || sqlError.Code != "22023" || batch.Length != 0 {
				t.Fatalf("invalid numeric step published %d rows: %v", batch.Length, err)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLNumericSeriesLimitDoesNotMaterializeRange(t *testing.T) {
	graph := sqlGraph(t, `SELECT n::text FROM generate_series(0.1::numeric,1e100::numeric,0.1::numeric) g(n) LIMIT 2`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25}, [][]model.Value{{model.StringVal("0.1")}, {model.StringVal("0.2")}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLNumericSeriesBytePressureKeepsPendingRow(t *testing.T) {
	graph := sqlGraph(t, `SELECT n,ord FROM generate_series(0.1::numeric,100::numeric,0.1::numeric) WITH ORDINALITY g(n,ord)`, nil)
	env := statefulEnvironment(t)
	env.Limits.BatchRows, env.Limits.BatchBytes = 1000, 2048
	env.Limits.Expressions.MaxRows = 1000
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	defer batch.Reset()
	var next int64 = 1
	for {
		err := portal.Next(t.Context(), &batch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if batch.Length == 0 || batch.Length >= 1000 {
			t.Fatalf("byte pressure returned %d rows", batch.Length)
		}
		for row := 0; row < batch.Length; row++ {
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], row, 2048)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("%d.%d", next/10, next%10)
			if value.StrVal != want || batch.Columns[1].Int64[row] != next {
				t.Fatalf("row %d: value=%s ordinal=%d, want %s", next, value.StrVal, batch.Columns[1].Int64[row], want)
			}
			next++
		}
	}
	if next != 1001 {
		t.Fatalf("series stopped at %d", next)
	}
	aggregateProjectionReleased(t, env.Query)
}
