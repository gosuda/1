package modify_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestParsedSelectSchemaNamespaceRows(t *testing.T) {
	i := model.Int64Val
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{
			name: "qualified columns and stars retain relation order",
			sql: `SELECT archive.items.*, public.items.*, archive.items.value + public.items.value
				FROM public.items JOIN archive.items ON public.items.id = archive.items.id
				ORDER BY public.items.value DESC`,
			want: [][]model.Value{
				{i(1), i(300), i(1), i(30), i(330)},
				{i(3), i(200), i(3), i(20), i(220)},
				{i(2), i(100), i(2), i(10), i(110)},
			},
		},
		{
			name: "group ordinals follow expanded qualified star",
			sql: `SELECT archive.items.*, public.items.value, count(*)
				FROM public.items JOIN archive.items ON public.items.id = archive.items.id
				GROUP BY 1, 2, 3 ORDER BY 3 DESC`,
			want: [][]model.Value{
				{i(1), i(300), i(30), i(1)},
				{i(3), i(200), i(20), i(1)},
				{i(2), i(100), i(10), i(1)},
			},
		},
		{
			name: "lateral star captures outer schema past local same name",
			sql: `SELECT captured.* FROM public.items CROSS JOIN LATERAL (
					SELECT public.items.*, archive.items.value AS archived
					FROM archive.items WHERE archive.items.id = public.items.id
				) captured ORDER BY captured.value DESC`,
			want: [][]model.Value{
				{i(1), i(30), i(300)},
				{i(3), i(20), i(200)},
				{i(2), i(10), i(100)},
			},
		},
		{
			name: "aliased relation does not leak its original bare name",
			sql: `SELECT items.*, visible.value
				FROM public.items AS visible JOIN archive.items ON visible.id = archive.items.id
				ORDER BY items.value DESC`,
			want: [][]model.Value{
				{i(1), i(300), i(30)},
				{i(3), i(200), i(20)},
				{i(2), i(100), i(10)},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			portal := parsedSchemaNamespaceSelect(t, tc.sql)
			if got := defaultSQLRows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows %v, want %v", got, tc.want)
			}
		})
	}
}

type schemaNamespaceAccess struct {
	mutationAccess
	archive storage.ObjectRef
}

func (a schemaNamespaceAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	if request.Object == a.archive.ObjectID {
		a.mutationAccess.target = a.archive
	}
	return a.mutationAccess.Access(ctx, request)
}

func parsedSchemaNamespaceSelect(t *testing.T, sql string) *execution.Portal {
	t.Helper()
	s := newStore(t)
	s.seed(t, row(1, 30), row(2, 10), row(3, 20))
	if _, err := s.tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	catalog := mutationCatalog{descriptors: []*model.Descriptor{s.options.Descriptor}}
	for _, spec := range []model.DescriptorSpec{
		{ID: 2, OID: 10002, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: 1}},
		{ID: 4, OID: 10004, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "archive"}, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: 1}},
	} {
		descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
		if err != nil {
			t.Fatal(err)
		}
		catalog.descriptors = append(catalog.descriptors, descriptor)
	}
	spec := descriptorSpec()
	spec.ID, spec.OID, spec.Name.Parent = 3, 10003, 4
	archive, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	catalog.descriptors = append(catalog.descriptors, archive)

	archiveTarget := storage.ObjectRef{ObjectID: 3, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	policy, err := txn.NewRowNormalizer(t.Context(), archiveTarget, []txn.ColumnRule{{Layout: s.options.Layout[0], KeyPosition: 1}, {Layout: s.options.Layout[1]}}, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.manager.Begin(t.Context(), txn.TransactionOptions{Mode: txn.ReadWrite, Resolver: resolver{archiveTarget, policy}, KeyEncoder: s.options.Encoder, MemoryBytes: 8 << 20, SpillBytes: 32 << 20, SpillDirectory: t.TempDir(), MaxViews: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for _, values := range [][]model.Value{row(1, 300), row(2, 100), row(3, 200)} {
		request := s.request(t, values[0].IntVal)
		if err := tx.ApplyMutations(t.Context(), []storage.Mutation{{Op: storage.MutationInsert, Target: archiveTarget, Key: request.Key, ColumnIDs: request.ColumnIDs, Values: values}}); err != nil {
			t.Fatal(err)
		}
	}

	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	limits := ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: uint32(len(sql))}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], catalog, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := normalize.Normalize(t.Context(), bound.Graph(), normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	request := s.request(t, 1)
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	view, err := tx.ReadView(t.Context(), []txn.ReadDependency{
		{Target: request.Target, KeyLayout: s.options.KeyLayout, Spans: spans},
		{Target: archiveTarget, KeyLayout: s.options.KeyLayout, Spans: spans},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	access := schemaNamespaceAccess{mutationAccess: mutationAccess{reader: view, target: request.Target, layout: request.Layout, keys: s.options.KeyLayout}, archive: archiveTarget}
	planned, err := opt.Optimize(t.Context(), graph, opt.Options{IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Second, Access: access})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), planned.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	expressionLimits := expr.DefaultLimits()
	expressionLimits.MaxRows = 2
	environment := execution.Environment{
		Query: s.query, Reader: view, Keys: s.options.Encoder,
		Builtin:   builtin.ResolveOptions{Collation: 950, DefaultCollation: 950},
		Limits:    execution.Limits{Expressions: expressionLimits, BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 8 << 20},
		Join:      exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2},
		Sort:      exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2},
		Aggregate: aggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4},
		Apply:     apply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10},
		Spool:     rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4},
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
