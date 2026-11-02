package bind_test

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type checkSequenceRuntime struct {
	model.SequenceRuntime
	oid model.OID
}

func (r checkSequenceRuntime) CurrentSequence(_ context.Context, oid model.OID) (int64, error) {
	if oid != r.oid {
		return 0, &model.Error{Code: "42P01", Message: "unexpected sequence identity"}
	}
	return 7, nil
}

func TestCheckRegclassIdentitySurvivesAlterAndRename(t *testing.T) {
	for _, literal := range []string{`'seq'`, `'seq'::regclass`} {
		t.Run(literal, func(t *testing.T) {
			snapshot := sequenceSnapshot(t)
			table := snapshot.descriptors[1]
			original := snapshot.descriptors[len(snapshot.descriptors)-1]
			sql := `SELECT currval(` + literal + `) = 7 AND "X" > 0 AND 'seq' = 'seq'`
			parsed, err := zpgparse.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			ast := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
			callerAST := proto.Clone(ast)
			options := bindingOptions(sql)
			definition, err := bind.DefineCheck(t.Context(), ast, table, 50, snapshot, options)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(ast, callerAST) {
				t.Fatal("defining CHECK mutated caller AST")
			}

			altered, err := sequenceDeclaration(t, t.Context(), `ALTER SEQUENCE seq START 12 INCREMENT 3`, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			next, err := altered.Finalize(t.Context(), 901)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := next.Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			spec.Name.Name = "renamed_sequence"
			snapshot.descriptors[len(snapshot.descriptors)-1] = testDescriptor(t, spec)
			tableSpec, err := table.Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			tableSpec.Version++
			tableSpec.Name.Name = "renamed_table"
			tableSpec.Table.Columns[0].Name = "renamed_column"
			snapshot.descriptors[1] = testDescriptor(t, tableSpec)
			columns := []storage.ColumnLayout{{ID: 1, Type: tableSpec.Table.Columns[0].Layout.Type}}
			bound, err := bind.BindCheck(t.Context(), definition, 50, columns, snapshot, options)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(bound.Dependencies, model.Dependency{Object: original.ID(), Version: spec.Version}) {
				t.Fatal("rebound CHECK omitted current sequence dependency")
			}
			if !slices.Contains(bound.Dependencies, model.Dependency{Object: table.ID(), Version: tableSpec.Version, Column: 1}) {
				t.Fatal("rebound CHECK omitted current column dependency")
			}
			compiler, err := session.NewColumnExpressionCompiler(snapshot, session.ColumnExpressionOptions{Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			program, err := compiler.CompileCheck(t.Context(), definition, 50, columns)
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range []int64{1, -1} {
				value, err := program.Evaluate(t.Context(), []model.Value{model.Int64Val(input)}, model.ExpressionRuntime{Sequences: checkSequenceRuntime{oid: original.OID()}})
				if err != nil || value.IsNull || (value.IntVal == 1) != (input > 0) {
					t.Fatalf("CHECK(%d) = %v, %v", input, value, err)
				}
			}
			spec.Sequence.Identity++
			snapshot.descriptors[len(snapshot.descriptors)-1] = testDescriptor(t, spec)
			_, err = bind.BindCheck(t.Context(), definition, 50, columns, snapshot, options)
			requireCode(t, err, "0A000")
		})
	}
}

func TestCheckShadowedCurrvalKeepsTextLiteral(t *testing.T) {
	snapshot := sequenceSnapshot(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	integer := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	routine := defaultRoutineDescriptor(t, 40, 90040, 10, "currval", []model.RoutineParameter{{Name: "name", Layout: text, Mode: "in"}}, integer, "immutable")
	snapshot.descriptors = append(snapshot.descriptors, routine)
	const sql = `SELECT public.currval('seq') = 7 AND 'seq'::regclass IS NOT NULL`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	ast := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
	definition, err := bind.DefineCheck(t.Context(), ast, snapshot.descriptors[1], 50, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	sequenceIndex := len(snapshot.descriptors) - 2
	spec, err := snapshot.descriptors[sequenceIndex].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Version++
	spec.Name.Name = "renamed"
	snapshot.descriptors[sequenceIndex] = testDescriptor(t, spec)
	compiler, err := session.NewColumnExpressionCompiler(snapshot, session.ColumnExpressionOptions{
		Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
		Functions: onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
			if function.ID != 90040 {
				return nil, &builtin.UnknownFunctionError{ID: function.ID}
			}
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				if args[0].StrVal != "seq" {
					return model.Int64Val(-1), nil
				}
				return model.Int64Val(7), nil
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	program, err := compiler.CompileCheck(t.Context(), definition, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := program.Evaluate(t.Context(), nil, model.ExpressionRuntime{})
	if err != nil || value.IsNull || value.IntVal != 1 {
		t.Fatalf("CHECK with shadowed text argument = %v, %v", value, err)
	}
}

func TestGeneratedRegclassIdentitySurvivesSequenceRename(t *testing.T) {
	snapshot := sequenceSnapshot(t)
	table := snapshot.descriptors[1]
	sequenceIndex := len(snapshot.descriptors) - 1
	original := snapshot.descriptors[sequenceIndex]
	const sql = `SELECT 'seq'::regclass::integer`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	ast := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
	target := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	definition, err := bind.DefineStoredGenerated(t.Context(), ast, table, 1, target, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	tableSpec, err := table.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tableSpec.Version++
	tableSpec.Table.Columns[0].Generated = definition
	tableSpec.Table.Columns[0].GeneratedStored = true
	tableSpec.Table.Columns[0].DefaultOID = 10002
	snapshot.descriptors[1] = testDescriptor(t, tableSpec)
	spec, err := original.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Name.Name = "renamed"
	spec.Version++
	spec.Name.Parent = 20
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 20, OID: 10020, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "moved"}, Schema: &model.SchemaMetadata{Owner: 10},
	}))
	snapshot.descriptors[sequenceIndex] = testDescriptor(t, spec)
	compiler, err := session.NewColumnExpressionCompiler(snapshot, session.ColumnExpressionOptions{Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	program, err := compiler.CompileStoredGenerated(t.Context(), definition, 1, nil, target)
	if err != nil {
		t.Fatal(err)
	}
	value, err := program.Evaluate(t.Context(), nil, model.ExpressionRuntime{})
	if err != nil || value.IsNull || value.IntVal != int64(original.OID()) {
		t.Fatalf("generated relation identity = %v, %v; want %d", value, err, original.OID())
	}
}

func TestIndexRegclassIdentitySurvivesSequenceRename(t *testing.T) {
	snapshot := indexClassSnapshot(t)
	sequence, err := sequenceDeclaration(t, t.Context(), `CREATE SEQUENCE seq`, snapshot, &declarationIdentities{next: 100})
	if err != nil {
		t.Fatal(err)
	}
	original, err := sequence.Finalize(t.Context(), 900)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.descriptors = append(snapshot.descriptors, original)
	const sql = `CREATE INDEX relation_identity ON public."Mixed" ("X") WHERE 'seq'::regclass IS NOT NULL`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	bound, err := bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), snapshot, bind.CreateIndexOptions{
		Bind: options, Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		ObjectID: 12, OID: 10012, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Expressions: literalIndexCompiler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err := bound.Index.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec, err := original.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Name.Name = "renamed"
	spec.Version++
	snapshot.descriptors[len(snapshot.descriptors)-1] = testDescriptor(t, spec)
	compiler, err := session.NewIndexExpressionCompiler(snapshot, session.IndexExpressionOptions{Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	program, err := compiler.CompileIndexExpression(t.Context(), index.Index.Predicate, nil, boolean)
	if err != nil {
		t.Fatal(err)
	}
	value, err := program.Evaluate(t.Context(), nil, model.ExpressionRuntime{})
	if err != nil || value.IsNull || value.IntVal != 1 {
		t.Fatalf("index relation predicate = %v, %v", value, err)
	}
}
