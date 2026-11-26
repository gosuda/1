package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func prepareSemanticQuery(t *testing.T, builtins builtin.ResolveOptions) *session.Prepared {
	t.Helper()
	return prepareSemanticSQL(t, `SELECT '\x01'::bytea::text,$1::text`, []model.TypeLayout{{Type: model.Type{ID: 17, Storage: model.TypeBytes, Modifier: -1}}}, builtins)
}

func prepareSemanticSQL(t *testing.T, sql string, parameters []model.TypeLayout, builtins builtin.ResolveOptions) *session.Prepared {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	limits := irLimits()
	options := session.PrepareOptions{
		Bind:      bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits},
		Normalize: normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtins},
		Optimize:  opt.Options{IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20},
		Fragment:  fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20},
	}
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], nil, options, parameters)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

var errPreparedReaderClose = errors.New("prepared reader cleanup failure")

type preparedCloseReader struct {
	storage.SnapshotReader
	closed int
}

func (reader *preparedCloseReader) Close() error {
	reader.closed++
	return errPreparedReaderClose
}

func TestPreparedRejectsSemanticSettingsMismatchAndReleasesOwners(t *testing.T) {
	settings := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, ByteaEscape: true}
	prepared := prepareSemanticQuery(t, settings)
	for _, name := range []string{"bytea output", "float output", "default collation", "array nulls"} {
		t.Run(name, func(t *testing.T) {
			env := statefulEnvironment(t)
			buffer, err := env.Query.NewBuffer(runtime.WorkMemory, 1)
			if err != nil {
				t.Fatal(err)
			}
			env.Builtin = settings
			switch name {
			case "bytea output":
				env.Builtin.ByteaEscape = false
			case "float output":
				env.Builtin.ExtraFloatDigits = 1
			case "default collation":
				env.Builtin.DefaultCollation = 951
			case "array nulls":
				env.Builtin.ArrayNullsDisabled = true
			}
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}, Value: model.BytesVal([]byte{1})}}
			reader := &preparedCloseReader{}
			env.Reader = reader
			portal, err := prepared.Open(t.Context(), env)
			if portal != nil {
				err = errors.Join(err, portal.Close())
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "0A000" {
				t.Fatalf("mismatched prepared settings: %v; want 0A000", err)
			}
			if !errors.Is(err, errPreparedReaderClose) || reader.closed != 1 {
				t.Fatalf("reader cleanup lost: count=%d error=%v", reader.closed, err)
			}
			if len(buffer.Bytes()) != 0 {
				t.Fatal("query buffer remains owned after rejected Open")
			}
		})
	}
}

func TestPreparedUsesConsistentFoldedAndRuntimeSettings(t *testing.T) {
	settings := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, ByteaEscape: true}
	prepared := prepareSemanticQuery(t, settings)
	env := statefulEnvironment(t)
	env.Builtin = settings
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}, Value: model.BytesVal([]byte{1})}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25}, [][]model.Value{{model.StringVal(`\001`), model.StringVal(`\001`)}})
	aggregateProjectionReleased(t, env.Query)
}
