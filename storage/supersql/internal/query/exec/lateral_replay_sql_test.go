package exec_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type lateralReplayCatalog []*model.Descriptor

// compiledRoutine turns a SQL routine fixture into a durable, executable
// definition. A hand-rolled ExpressionDefinition carries no encoding, and
// procedural.Decode rejects any body without one, so a stub cannot reproduce
// what CREATE FUNCTION stores.
func compiledRoutine(t *testing.T, snapshot bind.CatalogSnapshot, body string, metadata model.RoutineMetadata) model.RoutineMetadata {
	t.Helper()
	parsed, err := zpgparse.Parse("CREATE FUNCTION public.fixture() RETURNS void LANGUAGE sql AS '" + strings.ReplaceAll(body, "'", "''") + "'")
	if err != nil {
		t.Fatal(err)
	}
	metadata.Atomic = true
	compiled, err := procedural.CompileRoutine(t.Context(), parsed.Stmts[0].Stmt.GetCreateFunctionStmt(), metadata, snapshot, procedural.CompileOptions{
		Bind:   bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()},
		Limits: procedural.DefaultLimits(), CheckBodies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func (s lateralReplayCatalog) Timestamp() model.Timestamp { return 1 }

func (s lateralReplayCatalog) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, descriptor := range s {
		if descriptor.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: descriptor, PublishedAt: 1}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}

func (s lateralReplayCatalog) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, descriptor := range s {
		if descriptor.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: descriptor, PublishedAt: 1}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}

func (s lateralReplayCatalog) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, descriptor := range s {
		if err := ctx.Err(); err != nil {
			return err
		}
		if descriptor.Name().Parent == parent && descriptor.Kind() == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: descriptor, PublishedAt: 1}); err != nil {
				return err
			}
		}
	}
	return nil
}

func openLateralReplaySQL(t *testing.T, sql string, env exec.Environment) (*exec.Portal, *int64) {
	t.Helper()
	snapshot := make(lateralReplayCatalog, 2)
	for i, spec := range []model.DescriptorSpec{
		{
			ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
			Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
			Schema: &model.SchemaMetadata{Owner: 10},
		},
		{
			ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
			Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "tick"},
			Routine: new(compiledRoutine(t, lateralReplayCatalog{}, "SELECT 0::bigint", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}, Language: "sql", Volatility: "volatile"})),
		},
	} {
		var err error
		snapshot[i], err = model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := normalize.Normalize(t.Context(), bound.Graph(), normalize.Options{Limits: irLimits(), MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950}})
	if err != nil {
		t.Fatal(err)
	}
	optimized, err := opt.Optimize(t.Context(), logical, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := fragment.Build(t.Context(), optimized.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	tick := new(int64)
	env.Functions = aggregateProjectionResolver(func(fn expr.Function) (expr.ScalarCall, error) {
		if fn.ID != 90012 {
			return nil, &builtin.UnknownFunctionError{ID: fn.ID}
		}
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			*tick += 1
			return model.Int64Val(*tick), nil
		}, nil
	})
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closePortal(t, portal)
		aggregateProjectionReleased(t, env.Query)
		if usage := env.Query.Usage().Spill; usage.Live != 0 || usage.Reservations != 0 {
			t.Errorf("LATERAL replay retained spill reservations: %+v", usage)
		}
	})
	return portal, tick
}

func assertLateralReplayRows(t *testing.T, portal *exec.Portal, tick *int64, want [][]model.Value, wantTick int64) {
	t.Helper()
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Errorf("LATERAL rows=%v, want %v", got, want)
	}
	if *tick != wantTick {
		t.Errorf("tick sequence=%d, want %d", *tick, wantTick)
	}
}

func TestSQLLateralReplaysIndependentMultirowBody(t *testing.T) {
	for _, fixture := range []struct {
		name string
		env  func(*testing.T) exec.Environment
	}{
		{"memory", applyEnvironment},
		{"spill", lexicalInitplanEnvironment},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			env := fixture.env(t)
			portal, tick := openLateralReplaySQL(t, `SELECT l.x,r.v
				FROM (VALUES(1),(2),(3)) l(x)
				CROSS JOIN LATERAL (
					SELECT public.tick() AS v FROM (VALUES(0),(0),(0)) d(n)
				) r ORDER BY l.x,r.v`, env)
			i := model.Int64Val
			assertLateralReplayRows(t, portal, tick, [][]model.Value{
				{i(1), i(1)}, {i(1), i(2)}, {i(1), i(3)},
				{i(2), i(1)}, {i(2), i(2)}, {i(2), i(3)},
				{i(3), i(1)}, {i(3), i(2)}, {i(3), i(3)},
			}, 3)
		})
	}
}

func TestSQLLateralReplaysBodyButMatchesEachLeftRow(t *testing.T) {
	for _, join := range []struct {
		name string
		sql  string
	}{
		{"ON", "LEFT JOIN LATERAL (SELECT public.tick() AS x) r ON r.x=l.x"},
		{"USING", "LEFT JOIN LATERAL (SELECT public.tick() AS x) r USING (x)"},
		{"NATURAL", "NATURAL LEFT JOIN LATERAL (SELECT public.tick() AS x) r"},
	} {
		t.Run(join.name, func(t *testing.T) {
			portal, tick := openLateralReplaySQL(t, `SELECT l.x,r.x
				FROM (VALUES(1),(2),(3),(NULL)) l(x) `+join.sql+`
				ORDER BY l.x NULLS LAST`, applyEnvironment(t))
			i := model.Int64Val
			null := model.NullVal(model.TypeInt64)
			assertLateralReplayRows(t, portal, tick, [][]model.Value{
				{i(1), i(1)}, {i(2), null}, {i(3), null}, {null, null},
			}, 1)
		})
	}
}

func TestSQLLateralReplayResetsWithHigherCaptureOwner(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT o.x,z.k,z.v
		FROM (VALUES(10),(10)) o(x)
		CROSS JOIN LATERAL (
			SELECT l.k,r.v FROM (VALUES(1),(2),(3)) l(k)
			CROSS JOIN LATERAL (SELECT o.x+public.tick() AS v) r
		) z ORDER BY z.v,z.k`, lexicalInitplanEnvironment(t))
	i := model.Int64Val
	assertLateralReplayRows(t, portal, tick, [][]model.Value{
		{i(10), i(1), i(11)}, {i(10), i(2), i(11)}, {i(10), i(3), i(11)},
		{i(10), i(1), i(12)}, {i(10), i(2), i(12)}, {i(10), i(3), i(12)},
	}, 2)
}

func TestSQLLateralCorrelatedBodyReevaluatesRepeatedCaptures(t *testing.T) {
	portal, tick := openLateralReplaySQL(t, `SELECT l.x,r.v
		FROM (VALUES(10),(10),(10)) l(x)
		CROSS JOIN LATERAL (SELECT l.x+public.tick() AS v) r
		ORDER BY r.v`, applyEnvironment(t))
	i := model.Int64Val
	assertLateralReplayRows(t, portal, tick, [][]model.Value{
		{i(10), i(11)}, {i(10), i(12)}, {i(10), i(13)},
	}, 3)
}

func TestSQLLateralReplayWithoutDemandSuppressesBodyEffects(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
	}{
		{"empty left", `SELECT r.v FROM (SELECT x FROM (VALUES(1)) l(x) WHERE FALSE) l
			CROSS JOIN LATERAL (SELECT public.tick() AS v) r`},
		{"zero limit", `SELECT r.v FROM (VALUES(1),(2),(3)) l(x)
			CROSS JOIN LATERAL (SELECT public.tick() AS v) r LIMIT 0`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			portal, tick := openLateralReplaySQL(t, scenario.sql, applyEnvironment(t))
			assertLateralReplayRows(t, portal, tick, nil, 0)
		})
	}
}

func TestSQLLateralReplayEarlyCloseStopsBodyEffects(t *testing.T) {
	env := lexicalInitplanEnvironment(t)
	env.Limits.BatchRows = 1
	env.Limits.Expressions.MaxRows = 1
	portal, tick := openLateralReplaySQL(t, `SELECT l.x,r.v
		FROM (VALUES(10),(10),(10)) l(x)
		CROSS JOIN LATERAL (
			SELECT public.tick() AS v FROM (VALUES(0),(0),(0)) d(n)
		) r`, env)
	batch := columnar.Batch{MaxRows: 1}
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || len(batch.Columns) != 2 {
		t.Fatalf("first LATERAL batch=%+v, want one two-column row", batch)
	}
	for i, want := range []model.Value{model.Int64Val(10), model.Int64Val(1)} {
		got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[i], batch.Lane(0), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("first LATERAL row column %d=%v, want %v", i, got, want)
		}
	}
	closePortal(t, portal)
	if *tick != 1 {
		t.Errorf("tick sequence after early close=%d, want 1", *tick)
	}
}
