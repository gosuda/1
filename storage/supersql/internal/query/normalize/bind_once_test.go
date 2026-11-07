package normalize_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type routineSnapshot struct{ descriptors []*model.Descriptor }

func (s routineSnapshot) Timestamp() model.Timestamp { return 1 }
func (s routineSnapshot) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, descriptor := range s.descriptors {
		if descriptor.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s routineSnapshot) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, descriptor := range s.descriptors {
		if descriptor.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: descriptor}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s routineSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, descriptor := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if descriptor.Name().Parent == parent && descriptor.Kind() == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: descriptor}); err != nil {
				return err
			}
		}
	}
	return nil
}

func TestBoundSimpleCaseKeepsVolatileSingleEvaluation(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	// Binding a user SQL routine requires its durable encoded body. Compile the
	// stub the same way CREATE FUNCTION does, so the descriptor is executable.
	create, err := zpgparse.Parse("CREATE FUNCTION public.changing() RETURNS integer LANGUAGE sql VOLATILE AS 'SELECT 2'")
	if err != nil {
		t.Fatal(err)
	}
	body, err := procedural.CompileRoutine(t.Context(), create.Stmts[0].Stmt.GetCreateFunctionStmt(), model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile", Atomic: true}, routineSnapshot{}, procedural.CompileOptions{Bind: bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: options().Limits}, Limits: procedural.DefaultLimits(), CheckBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := routineSnapshot{}
	for _, spec := range []model.DescriptorSpec{
		{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}},
		{ID: 12, OID: 90012, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"}, Routine: &body},
	} {
		descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
		snapshot.descriptors = append(snapshot.descriptors, descriptor)
	}
	parsed, err := zpgparse.Parse("SELECT CASE public.changing() WHEN 0 THEN 10 WHEN 2 THEN 20 ELSE 30 END")
	if err != nil {
		t.Fatal(err)
	}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: options().Limits}, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := normalized(t, statement.Graph())
	calls := 0
	values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, resolver(func(function expr.Function) (expr.ScalarCall, error) {
		if function.ID != 90012 {
			t.Fatalf("unexpected function %d", function.ID)
		}
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			return model.Int64Val(2), nil
		}, nil
	}), 17)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 17 {
		t.Fatalf("simple CASE evaluated its left expression %d times, want 17", calls)
	}
	for _, value := range values[0] {
		if value.IsNull || value.IntVal != 20 {
			t.Fatalf("CASE result=%+v", value)
		}
	}
}
