package modify_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type mutationCatalog struct{ descriptors []*model.Descriptor }

func (m mutationCatalog) Timestamp() model.Timestamp { return 1 }
func (m mutationCatalog) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, d := range m.descriptors {
		if d.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: d, PublishedAt: 1}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (m mutationCatalog) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, d := range m.descriptors {
		if d.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: d, PublishedAt: 1}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (m mutationCatalog) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, d := range m.descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		if name.Parent == parent && name.Kind == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: d, PublishedAt: 1}); err != nil {
				return err
			}
		}
	}
	return nil
}

func parsedMutation(t *testing.T, s *store, sql string, configure ...func(*execution.Environment)) *execution.Portal {
	t.Helper()
	schema, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 2, OID: 10002, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: 1}}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	limits := ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: uint32(len(sql))}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], mutationCatalog{[]*model.Descriptor{schema, s.options.Descriptor}}, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := normalize.Normalize(t.Context(), bound.Graph(), normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	request := s.request(t, 1)
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	view, err := s.tx.ReadView(t.Context(), []txn.ReadDependency{{Target: request.Target, KeyLayout: s.options.KeyLayout, Spans: spans}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	planned, err := opt.Optimize(t.Context(), graph, opt.Options{IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Second, Access: mutationAccess{reader: view, target: request.Target, layout: request.Layout, keys: s.options.KeyLayout}})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), planned.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	expressionLimits := expr.DefaultLimits()
	expressionLimits.MaxRows = 2
	environment := execution.Environment{Query: s.query, Reader: view, Keys: s.options.Encoder, Modify: s.options, Join: exjoin.Options{MaxRecordBytes: 1024, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2}, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}, Limits: execution.Limits{Expressions: expressionLimits, BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 8 << 20}}
	environment.MutationTargets = execution.MutationTarget{Descriptor: s.options.Descriptor, Expansion: s.options.Expansion}
	for _, apply := range configure {
		apply(&environment)
	}
	portal, err := execution.Open(t.Context(), fragments, environment)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := portal.Close(); err != nil {
			t.Error(err)
		}
	})
	return portal
}

func TestParsedInsertReturningStagesAtomicallyWithoutCommit(t *testing.T) {
	s := newStore(t)
	portal := parsedMutation(t, s, `INSERT INTO items (id,value) VALUES(1,10),(2,20) RETURNING old.id,new.id,new.value`)
	var batch columnar.Batch
	seen := 0
	for {
		err := portal.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range batch.Rows() {
			old, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(row), 0)
			if err != nil {
				t.Fatal(err)
			}
			id, _, err := columnar.CopyValue(t.Context(), &batch.Columns[1], batch.Lane(row), 0)
			if err != nil {
				t.Fatal(err)
			}
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[2], batch.Lane(row), 0)
			if err != nil {
				t.Fatal(err)
			}
			seen++
			if !old.IsNull || id.IntVal != int64(seen) || value.IntVal != int64(seen*10) {
				t.Fatalf("RETURNING: old=%+v id=%+v value=%+v", old, id, value)
			}
		}
	}
	if seen != 2 || portal.RowsAffected() != 2 {
		t.Fatalf("rows=%d affected=%d", seen, portal.RowsAffected())
	}
	s.require(t, 1, 10)
	s.require(t, 2, 20)
	requireClean(t, s)
	if s.tx.State() != txn.Active {
		t.Fatalf("statement ended caller transaction: %v", s.tx.State())
	}
	if _, err := s.tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

type mutationAccess struct {
	reader storage.SnapshotReader
	target storage.ObjectRef
	layout []storage.ColumnLayout
	keys   []storage.KeyColumn
}

func (a mutationAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	if err := ctx.Err(); err != nil {
		return opt.Access{}, err
	}
	if request.Object != a.target.ObjectID || request.Generation != a.target.Generation || request.DescriptorVersion != a.target.DescriptorVersion {
		return opt.Access{}, storage.ErrInvalidLayout
	}
	columnMap := make([]physical.ScanColumn, len(a.layout))
	for i, column := range a.layout {
		columnMap[i] = physical.ScanColumn{Source: column.ID, Target: column.ID}
	}
	return opt.Access{Target: a.target, Layout: slices.Clone(a.layout), Paths: []opt.AccessPath{{Algorithm: physical.TableScan, Target: a.target, Layout: slices.Clone(a.layout), ColumnMap: columnMap, Keys: slices.Clone(a.keys)}}}, nil
}

func (a mutationAccess) Negotiate(ctx context.Context, request storage.PushdownRequest, _ uint64) (physical.ScanSpec, error) {
	plan, err := a.reader.PlanScan(ctx, request)
	if err != nil {
		return physical.ScanSpec{}, err
	}
	return physical.ScanSpec{Request: request, CapabilityEpoch: 1, Accepted: plan.Accepted, Residual: plan.Residual, Projection: plan.Projection, Ordering: plan.Ordering, Estimate: plan.Estimate, AppliedLimit: plan.AppliedLimit}, nil
}

func TestParsedUpdateMovesPrimaryKeysFromStatementImages(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(3, 30), row(9, 90))
	portal := parsedMutation(t, s, `UPDATE items SET id=id+1,value=id WHERE id<9 RETURNING old.id,old.value,new.id,new.value`)
	got := returned(t, portal)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10), model.Int64Val(2), model.Int64Val(1)},
		{model.Int64Val(3), model.Int64Val(30), model.Int64Val(4), model.Int64Val(3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UPDATE RETURNING = %v, want %v", got, want)
	}
	if portal.RowsAffected() != 2 {
		t.Fatalf("affected = %d, want 2", portal.RowsAffected())
	}
	s.absent(t, 1)
	s.absent(t, 3)
	s.require(t, 2, 1)
	s.require(t, 4, 3)
	s.require(t, 9, 90)
	requireClean(t, s)
	if s.tx.State() != txn.Active {
		t.Fatalf("UPDATE ended caller transaction: %v", s.tx.State())
	}
	if _, err := s.tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestParsedDeleteReturningUsesOldImageAndNullNewImage(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(9, 90))
	portal := parsedMutation(t, s, `DELETE FROM items WHERE id<9 RETURNING old.id,old.value,new.id,new.value`)
	got := returned(t, portal)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(10), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)},
		{model.Int64Val(2), model.Int64Val(20), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DELETE RETURNING = %v, want %v", got, want)
	}
	if portal.RowsAffected() != 2 {
		t.Fatalf("affected = %d, want 2", portal.RowsAffected())
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 9, 90)
	requireClean(t, s)
	if s.tx.State() != txn.Active {
		t.Fatalf("DELETE ended caller transaction: %v", s.tx.State())
	}
}

func TestParsedUpdateDuplicateMatchesAffectEachTargetOnce(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20))
	portal := parsedMutation(t, s, `UPDATE items SET value=value+1 FROM (VALUES(1),(1),(2),(2)) matches(id) WHERE items.id=matches.id RETURNING old.id,new.value`)
	got := returned(t, portal)
	want := [][]model.Value{row(1, 11), row(2, 21)}
	if !reflect.DeepEqual(got, want) || portal.RowsAffected() != 2 {
		t.Fatalf("duplicate UPDATE returned %v, affected=%d; want %v, affected=2", got, portal.RowsAffected(), want)
	}
	s.require(t, 1, 11)
	s.require(t, 2, 21)
	requireClean(t, s)
}

func TestParsedUpdateLaterFailureRestoresOnlyCurrentStatement(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
		code string
	}{
		{name: "assignment", sql: `UPDATE items SET value=100/(2-id) WHERE id<9 RETURNING old.id,new.value`, code: "22012"},
		{name: "primary key collision", sql: `UPDATE items SET id=id+7 WHERE id<9 RETURNING old.id,new.id`, code: "23505"},
		{name: "returning", sql: `UPDATE items SET value=value+1 WHERE id<9 RETURNING 100/(2-old.id)`, code: "22012"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(2, 20), row(9, 90))
			portal := parsedMutation(t, s, scenario.sql)
			var batch columnar.Batch
			err := portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != scenario.code {
				t.Fatalf("UPDATE error = %v, want SQLSTATE %s", err, scenario.code)
			}
			if batch.Rows() != 0 || portal.RowsAffected() != 0 {
				t.Fatalf("failed UPDATE published %d rows, affected=%d", batch.Rows(), portal.RowsAffected())
			}
			s.require(t, 1, 10)
			s.require(t, 2, 20)
			s.require(t, 9, 90)
			s.absent(t, 8)
			requireClean(t, s)
			if s.tx.State() != txn.Active {
				t.Fatalf("statement failure ended caller transaction: %v", s.tx.State())
			}
			if _, err := s.tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParsedDeleteEarlyCloseKeepsCallerOwnedWrites(t *testing.T) {
	for _, demand := range []bool{false, true} {
		name := "before demand"
		if demand {
			name = "after first returning row"
		}
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			s.seed(t, row(1, 10), row(2, 20), row(9, 90))
			portal := parsedMutation(t, s, `DELETE FROM items WHERE id<9 RETURNING old.id`)
			if demand {
				batch := columnar.Batch{MaxRows: 1}
				if err := portal.Next(t.Context(), &batch); err != nil {
					t.Fatal(err)
				}
				if batch.Rows() != 1 || batch.Columns[0].Int64[batch.Columns[0].ValueIndex(batch.Lane(0))] != 1 {
					t.Fatalf("first DELETE RETURNING batch = %+v", batch)
				}
			}
			if err := portal.Close(); err != nil {
				t.Fatal(err)
			}
			if demand {
				s.absent(t, 1)
				s.absent(t, 2)
			} else {
				s.require(t, 1, 10)
				s.require(t, 2, 20)
			}
			s.require(t, 9, 90)
			requireClean(t, s)
			if s.tx.State() != txn.Active {
				t.Fatalf("portal close ended caller transaction: %v", s.tx.State())
			}
		})
	}
}

func TestParsedUpdateNoMatchesReportsZeroWithoutAssignments(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10))
	portal := parsedMutation(t, s, `UPDATE items SET value=10/(id-id) WHERE id=99 RETURNING new.id,new.value`)
	var batch columnar.Batch
	if err := portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("zero-match UPDATE error=%v, want EOF", err)
	}
	if portal.RowsAffected() != 0 || batch.Rows() != 0 {
		t.Fatalf("zero-match UPDATE affected=%d returned=%d", portal.RowsAffected(), batch.Rows())
	}
	s.require(t, 1, 10)
	s.absent(t, 99)
	requireClean(t, s)
	if s.tx.State() != txn.Active {
		t.Fatalf("zero-match UPDATE ended caller transaction: %v", s.tx.State())
	}
	if _, err := s.tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
