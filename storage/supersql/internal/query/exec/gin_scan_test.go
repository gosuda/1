package exec_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func TestGINScanExecutionAndExplain(t *testing.T) {
	ctx := t.Context()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}
	epoch := clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := neosepia.Open(ctx, neosepia.Config{
		Directory: t.TempDir(), Scale: scale, KeyEncoder: encoder, FenceVerifier: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	int64Type := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	jsonbType := model.Type{ID: 3802, Modifier: -1, Storage: model.TypeJSONB}
	textType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}
	descLimits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}

	tableDesc, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: 10, OID: 10010, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "docs"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 2,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: int64Type}},
				{ID: 2, Name: "doc", State: model.StatePublic, Nullable: true, Layout: model.TypeLayout{Type: jsonbType}},
			},
			PrimaryKey: []model.KeyElement{{Column: 1, Type: int64Type}},
		},
	}, descLimits)
	if err != nil {
		t.Fatal(err)
	}
	idxDesc, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: 11, OID: 10011, Version: 1, State: model.StatePublic,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "idx_docs_gin"},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{
			Table:   10,
			Method:  "gin",
			OpClass: "jsonb_ops",
			Keys:    []model.KeyElement{{Column: 2, Type: jsonbType}},
		},
	}, descLimits)
	if err != nil {
		t.Fatal(err)
	}

	ginPlan, err := schema.NewIndexPlan(ctx, tableDesc, idxDesc, encoder, nil, schema.IndexLimits{
		MetadataBytes: 1 << 20, EntryBytes: 1 << 20, MaxColumns: 64,
	})
	if err != nil {
		t.Fatal(err)
	}

	baseRef := storage.ObjectRef{ObjectID: 10, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	baseCols := []storage.ColumnLayout{
		{ID: 1, Type: int64Type},
		{ID: 2, Type: jsonbType, Nullable: true},
	}
	baseKeys := []storage.KeyColumn{{ColumnID: 1, Type: int64Type}}
	ginLayout := ginPlan.Layout()
	ginKeys := ginPlan.KeyLayout()

	// Seed 4 rows + their GIN postings into NeoSepia!
	seedRows := []struct {
		id  int64
		doc *string
	}{
		{1, strPtr(`{"a": 1, "b": "hello"}`)},
		{2, strPtr(`{"a": 2, "c": "world"}`)},
		{3, strPtr(`{}`)},
		{4, nil},
		{5, strPtr(`{"a": 1, "c": "world"}`)},
	}
	batch := storage.MutationBatch{
		TxID:            model.TxnID{1},
		CommitTS:        20,
		Epoch:           epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts: []storage.ObjectLayout{
			{Target: baseRef, Columns: baseCols},
			ginLayout,
		},
	}
	ordinal := uint64(1)
	for _, r := range seedRows {
		pk, err := encoder.EncodePrefix(ctx, baseKeys, []model.Value{model.Int64Val(r.id)}, 1024)
		if err != nil {
			t.Fatal(err)
		}
		docVal := model.NullVal(model.TypeJSONB)
		if r.doc != nil {
			docVal = model.Value{Type: model.TypeJSONB, StrVal: *r.doc}
		}
		row := model.Row{Key: pk, Values: []model.Value{model.Int64Val(r.id), docVal}}
		batch.Mutations = append(batch.Mutations, storage.Mutation{
			Op: storage.MutationUpsert, Target: baseRef, Key: pk, Ordinal: ordinal,
			ColumnIDs: []model.ColumnID{1, 2}, Values: row.Values,
		})
		ordinal++
		entries, err := ginPlan.Entries(ctx, &row)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			batch.Mutations = append(batch.Mutations, storage.Mutation{
				Op: storage.MutationUpsert, Target: ginPlan.Target(), Key: e.Key, Ordinal: ordinal,
				ColumnIDs: e.ColumnIDs, Values: e.Values,
			})
			ordinal++
		}
	}
	batch.ChecksumCRC, err = batch.ComputeChecksum(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(ctx, batch); err != nil {
		t.Fatal(err)
	}

	fence, err := authority.Issue(ctx, 20, 1, epoch)
	if err != nil {
		t.Fatal(err)
	}

	runCase := func(name string, opID model.OperatorID, rhsType model.Type, rhsVal model.Value, wantIDs []int64) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			ib, err := ir.NewBuilder(ir.Limits{MaxNodes: 64, MaxOperands: 128, MaxSlots: 16, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1})
			if err != nil {
				t.Fatal(err)
			}
			scope, err := ib.AddScope(ctx, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			idLayout, err := ib.AddLayout(ctx, model.TypeLayout{Type: int64Type})
			if err != nil {
				t.Fatal(err)
			}
			docLayout, err := ib.AddLayout(ctx, model.TypeLayout{Type: jsonbType})
			if err != nil {
				t.Fatal(err)
			}
			rhsLayout, err := ib.AddLayout(ctx, model.TypeLayout{Type: rhsType})
			if err != nil {
				t.Fatal(err)
			}
			idSlot, err := ib.AddSlot(ctx, ir.SlotInfo{Scope: scope, Layout: idLayout, Object: 10, Column: 1})
			if err != nil {
				t.Fatal(err)
			}
			docSlot, err := ib.AddSlot(ctx, ir.SlotInfo{Scope: scope, Layout: docLayout, Nullable: true, Object: 10, Column: 2})
			if err != nil {
				t.Fatal(err)
			}
			rhsScalar, err := ib.AddScalar(ctx, ir.ScalarSpec{
				Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: rhsLayout},
				Value:  rhsVal,
			})
			if err != nil {
				t.Fatal(err)
			}
			scanRel, err := ib.AddRel(ctx, ir.RelSpec{
				Rel:     ir.Rel{Kind: ir.Scan, Scope: scope, Object: 10, Generation: 1, DescriptorVersion: 1},
				Outputs: []model.SlotID{idSlot, docSlot},
			})
			if err != nil {
				t.Fatal(err)
			}
			graph, err := ib.Build(ctx, scanRel)
			if err != nil {
				t.Fatal(err)
			}

			unbounded := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
			ginReq := storage.PushdownRequest{
				Target: ginPlan.Target(), IndexID: model.IndexID(ginPlan.Target().ObjectID),
				Layout: ginLayout.Columns, KeyLayout: ginKeys, ColumnIDs: []model.ColumnID{3},
				Spans: unbounded, Direction: storage.ScanForward,
			}
			baseReq := storage.PushdownRequest{
				Target: baseRef, Layout: baseCols, KeyLayout: baseKeys, ColumnIDs: []model.ColumnID{1, 2},
				Spans: unbounded, Direction: storage.ScanForward,
			}
			plan, err := physical.NewPlan(ctx, physical.Spec{
				Graphs: []*ir.Graph{graph},
				Root:   1,
				Nodes: []physical.NodeSpec{{
					Logical:   physical.RelRef{Graph: 1, Rel: scanRel},
					Algorithm: physical.SecondaryScan,
					Break:     physical.Streaming,
					Output: []physical.Slot{
						{Graph: 1, ID: idSlot, Layout: idLayout},
						{Graph: 1, ID: docSlot, Layout: docLayout, Nullable: true},
					},
					Delivered: physical.Properties{
						Distribution:  physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
						Rewindability: physical.OnePass,
					},
					Scan: physical.ScanSpec{
						Source:          baseRef,
						CapabilityEpoch: 1,
						Request:         ginReq,
						Projection:      []model.ColumnID{3},
						Lookup: physical.LookupScanSpec{
							Enabled:        true,
							OwnerColumn:    3,
							BaseColumnMap:  []physical.ScanColumn{{Source: 1, Target: 1}, {Source: 2, Target: 2}},
							BaseRequest:    baseReq,
							BaseProjection: []model.ColumnID{1, 2},
						},
						GIN: physical.GINScanSpec{
							Enabled:    true,
							IndexName:  "idx_docs_gin",
							TableName:  "docs",
							OpClass:    "jsonb_ops",
							Column:     2,
							Layout:     model.TypeLayout{Type: jsonbType},
							OperatorID: opID,
							QueryExpr:  physical.ScalarRef{Graph: 1, Scalar: rhsScalar},
						},
					},
				}},
			}, physical.Limits{MaxNodes: 16, MaxOperands: 128, MaxProperties: 128, MaxDepth: 16, MaxBytes: 1 << 20})
			if err != nil {
				t.Fatalf("NewPlan: %v", err)
			}
			fg, err := fragment.Build(ctx, plan, fragment.Limits{MaxNodes: 16, MaxEdges: 16, MaxDepth: 16, MaxWork: 1024, MaxBytes: 1 << 20})
			if err != nil {
				t.Fatalf("fragment.Build: %v", err)
			}

			// Verify EXPLAIN formats render "GIN Index Scan using idx_docs_gin on docs".
			for _, f := range []explain.Format{explain.FormatText, explain.FormatJSON, explain.FormatYAML, explain.FormatXML} {
				var buf bytes.Buffer
				if err := explain.Write(ctx, &buf, fg, f, explain.Options{
					Limits: explain.Limits{MaxOutputBytes: 1 << 20, MaxNodeBytes: 1 << 20, MaxValueDepth: 16},
				}); err != nil {
					t.Fatalf("explain format %d: %v", f, err)
				}
				if !strings.Contains(buf.String(), "GIN Index Scan using idx_docs_gin on docs") {
					t.Fatalf("explain format %d missing GIN Index Scan:\n%s", f, buf.String())
				}
			}

			reader, err := backend.OpenSnapshot(ctx, storage.SnapshotSpec{Timestamp: 20, Fence: fence})
			if err != nil {
				t.Fatal(err)
			}
			env, _ := environment(t)
			env.Reader = reader
			env.Keys = encoder
			portal, err := exec.Open(ctx, fg, env)
			if err != nil {
				t.Fatalf("exec.Open: %v", err)
			}
			defer closePortal(t, portal)

			var gotIDs []int64
			for _, r := range rows(t, portal) {
				gotIDs = append(gotIDs, r[0].IntVal)
			}
			if !slices.Equal(gotIDs, wantIDs) {
				t.Fatalf("got IDs = %v, want %v", gotIDs, wantIDs)
			}
		})
	}

	// Token intersection: {"a": 1} matches rows 1 and 5!
	runCase("jsonb_contains_a1", 3246, jsonbType, model.Value{Type: model.TypeJSONB, StrVal: `{"a": 1}`}, []int64{1, 5})
	// Multi-token intersection: {"a": 1, "c": "world"} matches ONLY row 5!
	runCase("jsonb_contains_a1_c_world", 3246, jsonbType, model.Value{Type: model.TypeJSONB, StrVal: `{"a": 1, "c": "world"}`}, []int64{5})
	// Key existence: ? 'b' matches ONLY row 1!
	runCase("jsonb_exists_b", 3247, textType, model.StringVal("b"), []int64{1})
	// Empty jsonb {} falls back safely to full base scan (rows 1..5 returned for parent Filter recheck)!
	runCase("jsonb_empty_fallback", 3246, jsonbType, model.Value{Type: model.TypeJSONB, StrVal: `{}`}, []int64{1, 2, 3, 4, 5})
}

func strPtr(s string) *string { return &s }
