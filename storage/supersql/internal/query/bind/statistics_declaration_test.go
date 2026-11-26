package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindStatisticsTestCommand(t *testing.T, sql string, snapshot metadataSnapshot, identities *declarationIdentities) *bind.BoundStatisticsCommand {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bind.CreateDeclarationOptions{
		Bind:        bindingOptions(sql),
		Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		Identities:  identities,
		Owner:       (model.Principal{}).OID(),
	}
	command, handled, err := bind.BindStatisticsCommand(t.Context(), parsed.Stmts[0].Stmt, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("statistics statement was not routed")
	}
	return command
}

func TestStatisticsRenameAndSetSchemaPreserveIndependentNamespace(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	public := testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}})
	alternate := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 10012, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "alternate"}, Schema: &model.SchemaMetadata{Owner: 10}})
	table := testDescriptor(t, model.DescriptorSpec{
		ID: 11, OID: 10011, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: public.ID(), Kind: model.ObjectTable, Name: "measurements"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{Owner: 10, ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{
			{ID: 1, Name: "first", State: model.StatePublic, Layout: integer},
			{ID: 2, Name: "second", State: model.StatePublic, Layout: integer},
		}},
	})
	snapshot := metadataSnapshot{descriptors: []*model.Descriptor{public, alternate, table}}
	identities := &declarationIdentities{next: 100}

	created := bindStatisticsTestCommand(t, `CREATE STATISTICS alternate.pair_stats (ndistinct) ON first, second FROM public.measurements`, snapshot, identities)
	if len(created.Updates()) != 1 {
		t.Fatalf("create updates = %d", len(created.Updates()))
	}
	table = created.Updates()[0].Next
	snapshot.descriptors[2] = table
	metadata, _ := table.Table()
	if len(metadata.ExtendedStatistics) != 1 || metadata.ExtendedStatistics[0].Namespace != alternate.ID() {
		t.Fatalf("created statistics = %#v", metadata.ExtendedStatistics)
	}

	renamed := bindStatisticsTestCommand(t, `ALTER STATISTICS alternate.pair_stats RENAME TO renamed_stats`, snapshot, identities)
	table = renamed.Updates()[0].Next
	snapshot.descriptors[2] = table
	metadata, _ = table.Table()
	if metadata.ExtendedStatistics[0].Name != "renamed_stats" || metadata.ExtendedStatistics[0].Namespace != alternate.ID() {
		t.Fatalf("renamed statistics = %#v", metadata.ExtendedStatistics[0])
	}

	moved := bindStatisticsTestCommand(t, `ALTER STATISTICS alternate.renamed_stats SET SCHEMA public`, snapshot, identities)
	metadata, _ = moved.Updates()[0].Next.Table()
	if metadata.ExtendedStatistics[0].Name != "renamed_stats" || metadata.ExtendedStatistics[0].Namespace != public.ID() {
		t.Fatalf("moved statistics = %#v", metadata.ExtendedStatistics[0])
	}
}

func TestAnalyzeBufferUsageLimitUsesPhysicalStorageExclusion(t *testing.T) {
	const sql = `ANALYZE (BUFFER_USAGE_LIMIT '1 MB') public.measurements`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bind.BindAnalyze(t.Context(), parsed.Stmts[0].Stmt.GetVacuumStmt(), tableSnapshot(t), bind.CreateDeclarationOptions{
		Bind:        bindingOptions(sql),
		Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		Identities:  &declarationIdentities{next: 100},
		Owner:       (model.Principal{}).OID(),
	})
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "0A000" || diagnostic.FeatureID != string(bind.PhysicalStorage) {
		t.Fatalf("BUFFER_USAGE_LIMIT error = %#v", diagnostic)
	}
}
