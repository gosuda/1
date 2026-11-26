package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	exdistinctset "github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	exrecursive "github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	exrowstore "github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	exsetop "github.com/gosuda/supersql-alpha/internal/query/exec/setop"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	exwindow "github.com/gosuda/supersql-alpha/internal/query/exec/window"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type ws3Fixture struct {
	backend     *neosepia.Backend
	manager     *txn.Manager
	catalog     *catalog.Store
	encoder     *builtin.KeyEncoder
	settings    *Settings
	session     *Transactions
	pool        *runtime.Pool
	budget      *runtime.Budget
	spillDir    string
	table       *model.Descriptor
	tableTarget storage.ObjectRef
	tableLayout []storage.ColumnLayout
	tableKeys   []storage.KeyColumn
	normalizers map[storage.ObjectRef]*txn.RowNormalizer
}

type ws3PolicyResolver map[storage.ObjectRef]*txn.RowNormalizer

func (r ws3PolicyResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer, ok := r[target]; ok {
		return normalizer, nil
	}
	return nil, storage.ErrInvalidTarget
}

type ws3CountingAccess struct {
	inner          opt.AccessPlanner
	accessCalls    *atomic.Int64
	negotiateCalls *atomic.Int64
}

func (c ws3CountingAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	if c.accessCalls != nil {
		c.accessCalls.Add(1)
	}
	return c.inner.Access(ctx, request)
}

func (c ws3CountingAccess) Negotiate(ctx context.Context, request storage.PushdownRequest, maxBytes uint64) (physical.ScanSpec, error) {
	if c.negotiateCalls != nil {
		c.negotiateCalls.Add(1)
	}
	return c.inner.Negotiate(ctx, request, maxBytes)
}

type ws3UnresolvedFunctions struct{}

func (ws3UnresolvedFunctions) Resolve(expr.Function) (expr.ScalarCall, error) {
	return nil, &model.Error{Code: "42883", Message: "function does not exist"}
}

type ws3CatalogScanner struct{}

func (ws3CatalogScanner) OpenCatalog(context.Context, catalogrel.ScanRequest) (storage.BatchCursor, error) {
	return nil, &model.Error{Code: "0A000", Message: "unused in unit test"}
}

func (s ws3CatalogScanner) WithSettings(catalogrel.Settings) catalogrel.Scanner { return s }

func (s ws3CatalogScanner) WithCatalogView(catalogrel.ReadView) catalogrel.Scanner { return s }

func ws3Int8Type() model.Type {
	return model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
}

func ws3Int8Layout() model.TypeLayout {
	return model.TypeLayout{Type: ws3Int8Type()}
}

func ws3IRLimits() ir.Limits {
	return ir.Limits{
		MaxNodes:    4096,
		MaxOperands: 16384,
		MaxSlots:    256,
		MaxDepth:    64,
		MaxBytes:    4 << 20,
		SourceBytes: 1 << 20,
	}
}

func ws3DescriptorLimits() model.DescriptorLimits {
	return model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
}

func newWS3Fixture(t *testing.T, extraCols int, secondaryIndexCount int) *ws3Fixture {
	t.Helper()
	ctx := t.Context()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{
		Name:                 clock.ScaleName,
		LeapHistoryID:        sha256.Sum256(history),
		CanonicalLeapHistory: history,
	}
	epoch := clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}
	provider := &constraintModeClock{
		scale:   scale,
		reading: clock.Reading{Earliest: 100, Latest: 110, Epoch: epoch, Status: clock.Synced},
	}
	authority, err := txn.NewFenceAuthority([32]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := neosepia.Open(ctx, neosepia.Config{
		Directory:     t.TempDir(),
		Scale:         scale,
		KeyEncoder:    encoder,
		FenceVerifier: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	state, err := backend.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := txn.NewManager(ctx, txn.ManagerConfig{
		Backend:       backend,
		Clock:         provider,
		Authority:     authority,
		MaxOperations: 16,
		Recovered: txn.RecoveredState{
			LogPosition:  state.LogPosition,
			CommitTS:     state.CommitTS,
			ClosedTS:     state.ClosedTS,
			HistoryFloor: state.HistoryFloor,
			Epoch:        state.Epoch,
			LeapHistory:  state.Scale.LeapHistoryID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	descLimits := ws3DescriptorLimits()
	cat, err := catalog.New(encoder, catalog.Limits{
		Descriptor:      descLimits,
		MaxEncodedBytes: 2 << 20,
		MaxKeyBytes:     4096,
		ScanBatchRows:   16,
	})
	if err != nil {
		t.Fatal(err)
	}
	int8Type := ws3Int8Type()
	int8Layout := ws3Int8Layout()
	// Lookup int8 btree operator class so CatalogAccess emits ordering operators.
	opClass, ok := builtin.LookupDefaultOperatorClass("btree", int8Type.ID)
	if !ok {
		t.Fatal("missing default btree operator class for int8")
	}
	int8OpClass := opClass.ID

	cols := []model.ColumnDescriptor{
		{ID: 1, Name: "id", Layout: int8Layout, State: model.StatePublic, Nullable: false},
		{ID: 2, Name: "val", Layout: int8Layout, State: model.StatePublic, Nullable: false},
	}
	for i := range extraCols {
		cols = append(cols, model.ColumnDescriptor{
			ID:       model.ColumnID(3 + i),
			Name:     "c" + string(rune('0'+i)),
			Layout:   int8Layout,
			State:    model.StatePublic,
			Nullable: false,
		})
	}
	if extraCols > 9 {
		for i := 9; i < extraCols; i++ {
			cols[2+i].Name = "c" + string(rune('a'+(i-9)))
		}
	}
	const tableID = model.ObjectID(20)
	indexIDs := make([]model.ObjectID, secondaryIndexCount)
	for i := range secondaryIndexCount {
		indexIDs[i] = model.ObjectID(30 + i)
	}
	schemaDesc, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID:      2,
		OID:     10002,
		Name:    model.NamespaceName{Kind: model.ObjectSchema, Name: "public"},
		Version: 1,
		State:   model.StatePublic,
		Schema:  &model.SchemaMetadata{Owner: 1},
	}, descLimits)
	if err != nil {
		t.Fatal(err)
	}
	tableDesc, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID:       tableID,
		OID:      20020,
		Name:     model.NamespaceName{Kind: model.ObjectTable, Parent: 2, Name: "items"},
		Version:  1,
		State:    model.StatePublic,
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			Owner:                 1,
			ColumnIDHighWatermark: model.ColumnID(len(cols)),
			Columns:               cols,
			PrimaryKey: []model.KeyElement{{
				Column:        1,
				Type:          int8Type,
				OperatorClass: int8OpClass,
			}},
			Indexes: indexIDs,
		},
	}, descLimits)
	if err != nil {
		t.Fatal(err)
	}
	descriptors := []*model.Descriptor{schemaDesc, tableDesc}
	var indexDescs []*model.Descriptor
	for i := range secondaryIndexCount {
		colID := model.ColumnID(2 + (i % (len(cols) - 1)))
		idxDesc, err := model.NewDescriptor(ctx, model.DescriptorSpec{
			ID:       indexIDs[i],
			OID:      model.OID(30030 + i),
			Name:     model.NamespaceName{Kind: model.ObjectIndex, Parent: 2, Name: "items_idx_" + string(rune('a'+i))},
			Version:  1,
			State:    model.StatePublic,
			Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
			Index: &model.IndexMetadata{
				Table: tableID,
				Keys: []model.KeyElement{{
					Column:        colID,
					Type:          int8Type,
					OperatorClass: int8OpClass,
				}},
				Include: []model.ColumnID{1},
			},
		}, descLimits)
		if err != nil {
			t.Fatal(err)
		}
		indexDescs = append(indexDescs, idxDesc)
		descriptors = append(descriptors, idxDesc)
	}

	tableTarget := storage.ObjectRef{ObjectID: tableID, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	tableLayout := make([]storage.ColumnLayout, len(cols))
	rules := make([]txn.ColumnRule, len(cols))
	for i, col := range cols {
		tableLayout[i] = storage.ColumnLayout{ID: col.ID, Type: col.Layout.Type, Nullable: col.Nullable}
		rules[i] = txn.ColumnRule{Layout: tableLayout[i]}
	}
	rules[0].KeyPosition = 1
	tableKeys := []storage.KeyColumn{{ColumnID: 1, Type: int8Type}}
	normalizer, err := txn.NewRowNormalizer(ctx, tableTarget, rules, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	normalizers := map[storage.ObjectRef]*txn.RowNormalizer{tableTarget: normalizer}
	for _, layout := range cat.Layouts() {
		catNorm, err := txn.NewRowNormalizer(ctx, layout.Target, []txn.ColumnRule{{Layout: layout.Columns[0], KeyPosition: 1}, {Layout: layout.Columns[1]}}, 2<<20)
		if err != nil {
			t.Fatal(err)
		}
		normalizers[layout.Target] = catNorm
	}

	// Publish catalog descriptors and seed rows (1..5).
	seedTx, err := manager.Begin(ctx, txn.TransactionOptions{
		Mode:        txn.ReadWrite,
		MemoryBytes: 8 << 20,
		DisableSpill: true,
		MaxViews:    16,
		Resolver:    ws3PolicyResolver(normalizers),
		KeyEncoder:  encoder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := seedTx.Statement(ctx, func(stmtCtx context.Context) error {
		for _, descriptor := range descriptors {
			deps, err := cat.CreateDependencies(stmtCtx, descriptor)
			if err != nil {
				return err
			}
			view, err := seedTx.ReadView(stmtCtx, deps)
			if err != nil {
				return err
			}
			snap, err := cat.TransactionSnapshot(view, catalog.BorrowReader)
			if err != nil {
				return errors.Join(err, view.Close())
			}
			change, err := cat.Create(stmtCtx, snap, descriptor)
			err = errors.Join(err, snap.Close(), view.Close())
			if err != nil {
				return err
			}
			if err := seedTx.ApplyMutations(stmtCtx, change.Mutations); err != nil {
				return err
			}
		}
		var indexPlans []*schema.IndexPlan
		for _, idx := range indexDescs {
			plan, err := schema.NewIndexPlan(stmtCtx, tableDesc, idx, encoder, nil, schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64})
			if err != nil {
				return err
			}
			indexPlans = append(indexPlans, plan)
			idxLayout := plan.Layout()
			idxKeys := plan.KeyLayout()
			idxRules := make([]txn.ColumnRule, len(idxLayout.Columns))
			for j, c := range idxLayout.Columns {
				idxRules[j] = txn.ColumnRule{Layout: c}
				for kPos, kc := range idxKeys {
					if kc.ColumnID == c.ID {
						idxRules[j].KeyPosition = uint32(kPos + 1)
					}
				}
			}
			idxNorm, err := txn.NewRowNormalizer(stmtCtx, plan.Target(), idxRules, 1<<20)
			if err != nil {
				return err
			}
			normalizers[plan.Target()] = idxNorm
		}
		colIDs := make([]model.ColumnID, len(cols))
		for i, col := range cols {
			colIDs[i] = col.ID
		}
		for rowID := int64(1); rowID <= 5; rowID++ {
			vals := make([]model.Value, len(cols))
			vals[0] = model.Int64Val(rowID)
			vals[1] = model.Int64Val(rowID * 100)
			for c := 2; c < len(cols); c++ {
				vals[c] = model.Int64Val(rowID*10 + int64(c))
			}
			key, err := encoder.EncodePrefix(stmtCtx, tableKeys, []model.Value{vals[0]}, 4096)
			if err != nil {
				return err
			}
			row := model.Row{Key: key, Values: vals}
			if err := seedTx.ApplyMutations(stmtCtx, []storage.Mutation{
				storage.NewInsertMutation(tableTarget, key, colIDs, vals),
			}); err != nil {
				return err
			}
			for _, plan := range indexPlans {
				entry, err := plan.Entry(stmtCtx, &row)
				if err != nil {
					return err
				}
				if entry == nil {
					continue
				}
				if err := seedTx.ApplyMutations(stmtCtx, []storage.Mutation{
					storage.NewInsertMutation(plan.Target(), entry.Key, entry.ColumnIDs, entry.Values),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := seedTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 2, QueueCapacity: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(128<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	settings := newTestSettings(t)
	txns, err := NewTransactions(ctx, TransactionConfig{
		Manager:  manager,
		Settings: settings,
		Options: txn.TransactionOptions{
			Mode:        txn.ReadWrite,
			MemoryBytes: 8 << 20,
		DisableSpill: true,
			MaxViews:    32,
			Resolver:    ws3PolicyResolver(normalizers),
			KeyEncoder:  encoder,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := txns.Close(); err != nil {
			t.Error(err)
		}
	})
	return &ws3Fixture{
		backend:     backend,
		manager:     manager,
		catalog:     cat,
		encoder:     encoder,
		settings:    settings,
		session:     txns,
		pool:        pool,
		budget:      budget,
		spillDir:    t.TempDir(),
		table:       tableDesc,
		tableTarget: tableTarget,
		tableLayout: tableLayout,
		tableKeys:   tableKeys,
		normalizers: normalizers,
	}
}

func (f *ws3Fixture) newQuery(t *testing.T, workLimit int64) *runtime.Query {
	t.Helper()
	query, err := runtime.NewQuery(t.Context(), f.pool, f.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
		WorkBytes:   workLimit,
		OutputBytes: 4 << 20,
		SpillBytes:  32 << 20,
		Directory:   f.spillDir,
		MaxFiles:    16,
		MaxReaders:  8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	return query
}

func (f *ws3Fixture) prepareOptions(snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, maxPaths int, accessCalls, negotiateCalls *atomic.Int64) (PrepareOptions, error) {
	limits := ws3IRLimits()
	builtins := f.settings.BuiltinOptions(950, 950)
	access, err := NewCatalogAccess(snapshot, reader, f.encoder, CatalogAccessOptions{
		MaxPaths:        maxPaths,
		MaxBytes:        4 << 20,
		CapabilityEpoch: 1,
		IndexLimits:     schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
		IndexExpressions: IndexExpressionOptions{
			Binding:     limits,
			Evaluation:  expr.DefaultLimits(),
			InputBytes:  1 << 20,
			ResultBytes: 1 << 20,
		},
		Builtin: builtins,
	})
	if err != nil {
		return PrepareOptions{}, err
	}
	var planner opt.AccessPlanner = access
	if accessCalls != nil || negotiateCalls != nil {
		planner = ws3CountingAccess{inner: access, accessCalls: accessCalls, negotiateCalls: negotiateCalls}
	}
	optMaxPaths := maxPaths
	if optMaxPaths == 0 {
		optMaxPaths = defaultCatalogAccessMaxPaths
	}
	return PrepareOptions{
		Bind:      bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits, Builtin: builtins},
		Normalize: normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: builtins},
		Optimize: opt.Options{
			IR:               limits,
			Plan:             physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20},
			MaxBytes:         256 << 20,
			MaxWork:          100000000,
			MaxOutputColumns: 256,
			MaxAccessPaths:   optMaxPaths,
			WorkingMemory:    1 << 20,
			Access:           planner,
		},
		Fragment: fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20},
	}, nil
}

func (f *ws3Fixture) baseEnvironment(query *runtime.Query) exec.Environment {
	builtins := f.settings.BuiltinOptions(950, 950)
	return exec.Environment{
		Query:     query,
		Keys:      f.encoder,
		Builtin:   builtins,
		Functions: ws3UnresolvedFunctions{},
		Limits: exec.Limits{
			Expressions:    expr.DefaultLimits(),
			BatchRows:      64,
			BatchBytes:     1 << 20,
			ParameterBytes: 1 << 20,
			MetadataBytes:  8 << 20,
		},
		Spool:     exrowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 1024, MaxRecordBytes: 4096, MetadataBytes: 16 << 10, DecodeBytes: 4096, MaxReaders: 8},
		Sort:      exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2},
		Join:      exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2},
		Aggregate: exaggregate.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4},
		Set:       exsetop.Options{MaxRecordBytes: 4096, GroupBytes: 1 << 20, PartitionMemoryBytes: 64 << 10, MaxSpillBytes: 16 << 20, WorkBytes: 16 << 20, MaxPasses: 16},
		Window:    exwindow.Options{MaxRecordBytes: 4096, PartitionMemoryBytes: 64 << 10, MaxPartitionBytes: 16 << 20, MaxPartitionRows: 10000, WorkBytes: 16 << 20},
		Apply:     exapply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10},
		Modify:    exmodify.Options{BatchRows: 64, BatchBytes: 1 << 20, MaxRecordBytes: 4096, MaxSpoolBytes: 8 << 20, MaxRows: 1000, WorkBytes: 64 << 20, SortRunBytes: 1 << 20, SortMaxRuns: 8, SortMergeFanIn: 2},
		Recursive: exrecursive.Options{
			MaxIterations: 10000,
			Spool:         exrowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 8},
			Seen:          exdistinctset.Options{MemoryBytes: 4096, WorkBytes: 256 << 10, MaxKeyBytes: 4096, MaxKeys: 10000, MaxLevels: 8},
		},
	}
}

func parseSingleStmt(t *testing.T, sql string) *zpgparse.RawStmt {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 || parsed.Stmts[0] == nil {
		t.Fatalf("parse(%q) = %v, err=%v", sql, parsed, err)
	}
	return parsed.Stmts[0]
}

func drainInt64Col0(t *testing.T, rows *Rows) []int64 {
	t.Helper()
	var out []int64
	var batch columnar.Batch
	for {
		err := rows.Next(t.Context(), &batch)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range batch.Rows() {
			out = append(out, batch.Columns[0].Int64[batch.Lane(row)])
		}
	}
}

// TestWS3ParameterizedIndexSpanAndNarrowedReadLock proves that parameterized
// WHERE id = $1 produces a narrowed point index span and a narrowed read-lock
// dependency (not an unbounded full-table scan/lock) for both typed and
// text-encoded parameters, and for SELECT as well as UPDATE/DELETE.
func TestWS3ParameterizedIndexSpanAndNarrowedReadLock(t *testing.T) {
	f := newWS3Fixture(t, 0, 0)
	ctx := t.Context()

	// 1. Verify RunStatement with typed and text-format $1 produces a narrowed
	//    index span on PrimaryKeyScan and returns the exact matching row.
	for _, tc := range []struct {
		name  string
		param expr.ParameterValue
		want  int64
	}{
		{name: "typed_int8", param: expr.ParameterValue{Number: 1, Layout: ws3Int8Layout(), Value: model.Int64Val(2)}, want: 200},
		{name: "text_coerced", param: expr.ParameterValue{Number: 1, Value: model.StringVal("3")}, want: 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := f.newQuery(t, 16<<20)
			env := f.baseEnvironment(query)
			env.Parameters = []expr.ParameterValue{tc.param}
			stmt := parseSingleStmt(t, "SELECT val FROM items WHERE id = $1")
			var capturedPrepared *Prepared
			var got []int64
			_, err := f.session.RunStatement(ctx, stmt, StatementExecution{
				Execution: PreparedExecution{
					Catalog:     f.catalog,
					Environment: env,
				},
				Planning: func(_ context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (PrepareOptions, error) {
					return f.prepareOptions(snapshot, reader, 16, nil, nil)
				},
			}, func(_ context.Context, prepared *Prepared, rows *Rows) error {
				capturedPrepared = prepared
				got = drainInt64Col0(t, rows)
				return nil
			})
			if err != nil {
				t.Fatalf("RunStatement(%s) failed: %v", tc.name, err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got rows %v, want [%d]", got, tc.want)
			}
			if capturedPrepared == nil || capturedPrepared.graph == nil {
				t.Fatal("missing compiled prepared plan")
			}
			plan := capturedPrepared.graph.Plan()
			foundNarrowScan := false
			for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
				node, _ := plan.Node(id)
				if node.Metadata().Algorithm != physical.PrimaryKeyScan {
					continue
				}
				readSpan, _, spanErr := node.CloneReadSpan(ctx, 1<<20)
				if spanErr != nil {
					t.Fatal(spanErr)
				}
				spans := readSpan.Spans
				if len(spans) != 1 || spans[0].Lower.Unbounded || spans[0].Upper.Unbounded {
					t.Fatalf("expected narrowed point span on PrimaryKeyScan, got %+v", spans)
				}
				foundNarrowScan = true
			}
			if !foundNarrowScan {
				t.Fatal("did not find PrimaryKeyScan in plan")
			}
		})
	}

	// 2. Verify statementScanDependenciesWithTrial narrows read-lock
	//    dependencies for parameterized SELECT and UPDATE (not [Unbounded, Unbounded]).
	tx, err := f.manager.Begin(ctx, txn.TransactionOptions{
		Mode:        txn.ReadWrite,
		MemoryBytes: 8 << 20,
		DisableSpill: true,
		MaxViews:    16,
		Resolver:    ws3PolicyResolver(f.normalizers),
		KeyEncoder:  f.encoder,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	bindingView, err := tx.ReadView(ctx, catalogLayoutDependencies(f.catalog))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := bindingView.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := f.catalog.TransactionSnapshot(bindingView, catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	planOpts, err := f.prepareOptions(snapshot, bindingView, 16, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"SELECT val FROM items WHERE id = $1",
		"DELETE FROM items WHERE id = $1",
	} {
		bound, err := bind.Bind(ctx, parseSingleStmt(t, sql), snapshot, planOpts.Bind, []model.TypeLayout{ws3Int8Layout()})
		if err != nil {
			t.Fatalf("bind(%q): %v", sql, err)
		}
		execOpts := PreparedExecution{
			Catalog: f.catalog,
			Environment: exec.Environment{
				Parameters: []expr.ParameterValue{{Number: 1, Layout: ws3Int8Layout(), Value: model.Int64Val(4)}},
				Limits:     f.baseEnvironment(nil).Limits,
			},
		}
		deps, trialPrepared, err := f.session.statementScanDependenciesWithTrial(ctx, bindingView, bound, execOpts, func(_ context.Context, view txn.ReadView, _ builtin.ResolveOptions) (*catalog.CatalogSnapshot, PrepareOptions, error) {
			s, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
			if err != nil {
				return nil, PrepareOptions{}, err
			}
			p, err := f.prepareOptions(s, view, 16, nil, nil)
			if err != nil {
				return nil, PrepareOptions{}, errors.Join(err, s.Close())
			}
			return s, p, nil
		}, f.settings.BuiltinOptions(950, 950))
		if err != nil {
			t.Fatalf("statementScanDependenciesWithTrial(%q): %v", sql, err)
		}
		if trialPrepared == nil {
			t.Fatalf("statementScanDependenciesWithTrial(%q) returned nil trialPrepared", sql)
		}
		foundNarrowDep := false
		for _, dep := range deps {
			if dep.Target != f.tableTarget {
				continue
			}
			if len(dep.Spans) != 1 || dep.Spans[0].Lower.Unbounded || dep.Spans[0].Upper.Unbounded {
				t.Fatalf("expected narrowed read-lock dependency for %q, got spans %+v", sql, dep.Spans)
			}
			if len(dep.Spans[0].Lower.Values) != 1 || dep.Spans[0].Lower.Values[0].IntVal != 4 ||
				len(dep.Spans[0].Upper.Values) != 1 || dep.Spans[0].Upper.Values[0].IntVal != 4 {
				t.Fatalf("expected point lock span [4, 4] for %q, got %+v", sql, dep.Spans[0])
			}
			foundNarrowDep = true
		}
		if !foundNarrowDep {
			t.Fatalf("missing table read dependency for %q in %+v", sql, deps)
		}
	}
}

// TestWS3EliminateDoublePlanning verifies that runPreparedStatement reuses the
// trial-compiled Prepared plan instead of running prepareBound twice when the
// catalog identity is unchanged between the binding view and the locked view.
func TestWS3EliminateDoublePlanning(t *testing.T) {
	f := newWS3Fixture(t, 0, 0)
	ctx := t.Context()

	var planningCalls atomic.Int64
	var negotiateCalls atomic.Int64
	query := f.newQuery(t, 16<<20)
	env := f.baseEnvironment(query)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: ws3Int8Layout(), Value: model.Int64Val(2)}}
	stmt := parseSingleStmt(t, "SELECT val FROM items WHERE id = $1")

	var got []int64
	_, err := f.session.RunStatement(ctx, stmt, StatementExecution{
		Execution: PreparedExecution{
			Catalog:     f.catalog,
			Environment: env,
		},
		Planning: func(_ context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (PrepareOptions, error) {
			planningCalls.Add(1)
			return f.prepareOptions(snapshot, reader, 16, nil, &negotiateCalls)
		},
	}, func(_ context.Context, _ *Prepared, rows *Rows) error {
		got = drainInt64Col0(t, rows)
		return nil
	})
	if err != nil {
		t.Fatalf("RunStatement failed: %v", err)
	}
	if len(got) != 1 || got[0] != 200 {
		t.Fatalf("got %v, want [200]", got)
	}
	// Call #1: bind.Bind. Call #2: trial prepareBound inside statementScanDependenciesWithTrial.
	// Call #3 (second prepareBound under view) MUST be eliminated!
	if gotCalls := planningCalls.Load(); gotCalls != 2 {
		t.Fatalf("Planning callback called %d times, want 2 (1 bind + 1 single plan, no double-planning)", gotCalls)
	}
	// Primary key table exposes 2 candidate paths (forward + reverse); a single
	// opt.Optimize pass negotiates each path once (2 calls total, not 4).
	if gotNeg := negotiateCalls.Load(); gotNeg != 2 {
		t.Fatalf("Negotiate called %d times, want 2 (single optimization pass)", gotNeg)
	}
}

// TestWS3CatalogAccessScanReverseAndMaxPaths verifies that:
// 1) CatalogAccess with MaxPaths=0 defaults to 64 and exposes >7 secondary indexes.
// 2) ScanReverse access paths are offered for primary and secondary indexes with
//    reversed negotiated ordering (Descending + NullsFirst).
// 3) ORDER BY pk DESC LIMIT N selects ScanReverse and eliminates full Sort.
func TestWS3CatalogAccessScanReverseAndMaxPaths(t *testing.T) {
	const secondaryCount = 10
	f := newWS3Fixture(t, 10, secondaryCount)
	ctx := t.Context()

	view, err := f.manager.CurrentReadView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()

	limits := ws3IRLimits()
	builtins := f.settings.BuiltinOptions(950, 950)
	access, err := NewCatalogAccess(snapshot, view, f.encoder, CatalogAccessOptions{
		MaxPaths:        0, // must default to 64, not reject or cap at 8
		MaxBytes:        8 << 20,
		CapabilityEpoch: 1,
		IndexLimits:     schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 64},
		IndexExpressions: IndexExpressionOptions{
			Binding:     limits,
			Evaluation:  expr.DefaultLimits(),
			InputBytes:  1 << 20,
			ResultBytes: 1 << 20,
		},
		Builtin: builtins,
	})
	if err != nil {
		t.Fatalf("NewCatalogAccess(MaxPaths=0) failed: %v", err)
	}

	got, err := access.Access(ctx, opt.AccessRequest{
		Object:            f.table.ID(),
		Generation:        1,
		DescriptorVersion: 1,
		Columns:           []model.ColumnID{1, 2},
		MaxPaths:          0,
		MaxBytes:          8 << 20,
	})
	if err != nil {
		t.Fatalf("CatalogAccess.Access failed: %v", err)
	}
	wantPaths := 2 * (1 + secondaryCount)
	if len(got.Paths) != wantPaths {
		t.Fatalf("CatalogAccess returned %d paths, want %d (forward + reverse for PK and all %d secondary indexes)", len(got.Paths), wantPaths, secondaryCount)
	}
	var pkFwd, pkRev, secFwd, secRev int
	var samplePKRev, sampleSecRev opt.AccessPath
	for _, path := range got.Paths {
		switch {
		case path.Algorithm == physical.PrimaryKeyScan && path.Direction == storage.ScanForward:
			pkFwd++
		case path.Algorithm == physical.PrimaryKeyScan && path.Direction == storage.ScanReverse:
			pkRev++
			samplePKRev = path
		case path.Algorithm == physical.SecondaryScan && path.Direction == storage.ScanForward:
			secFwd++
		case path.Algorithm == physical.SecondaryScan && path.Direction == storage.ScanReverse:
			secRev++
			if sampleSecRev.Target.ObjectID == 0 {
				sampleSecRev = path
			}
		}
	}
	if pkFwd != 1 || pkRev != 1 || secFwd != secondaryCount || secRev != secondaryCount {
		t.Fatalf("path breakdown pkFwd=%d pkRev=%d secFwd=%d secRev=%d, want 1/1/%d/%d", pkFwd, pkRev, secFwd, secRev, secondaryCount, secondaryCount)
	}

	// Tight pathLimit must prioritize all forward paths before reverse paths so
	// secondary indexes are never dropped in favor of reverse scans.
	tight, err := access.Access(ctx, opt.AccessRequest{
		Object:            f.table.ID(),
		Generation:        1,
		DescriptorVersion: 1,
		Columns:           []model.ColumnID{1, 2},
		MaxPaths:          1 + secondaryCount,
		MaxBytes:          8 << 20,
	})
	if err != nil {
		t.Fatalf("tight CatalogAccess.Access failed: %v", err)
	}
	if len(tight.Paths) != 1+secondaryCount {
		t.Fatalf("tight paths = %d, want %d", len(tight.Paths), 1+secondaryCount)
	}
	for i, path := range tight.Paths {
		if path.Direction != storage.ScanForward {
			t.Fatalf("tight path[%d] has direction %v, want ScanForward prioritized first", i, path.Direction)
		}
	}

	// Negotiate both primary and secondary ScanReverse paths and verify
	// delivered ordering is reversed (Descending: true, NullsFirst: true).
	for _, tc := range []struct {
		name string
		path opt.AccessPath
	}{
		{name: "primary_reverse", path: samplePKRev},
		{name: "secondary_reverse", path: sampleSecRev},
	} {
		spec, err := access.Negotiate(ctx, storage.PushdownRequest{
			Target:    tc.path.Target,
			Layout:    tc.path.Layout,
			KeyLayout: tc.path.Keys,
			IndexID:   tc.path.Index,
			ColumnIDs: []model.ColumnID{tc.path.Keys[0].ColumnID},
			Direction: tc.path.Direction,
			Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
		}, 1<<20)
		if err != nil {
			t.Fatalf("Negotiate(%s) failed: %v", tc.name, err)
		}
		if spec.Request.Direction != storage.ScanReverse {
			t.Fatalf("Negotiate(%s) direction = %v, want ScanReverse", tc.name, spec.Request.Direction)
		}
		if len(spec.Ordering) == 0 || !spec.Ordering[0].Descending || !spec.Ordering[0].NullsFirst {
			t.Fatalf("Negotiate(%s) ordering = %+v, want Descending=true NullsFirst=true", tc.name, spec.Ordering)
		}
	}

	// End-to-end: optimizing a scan with Required.Ordering = [id DESC NULLS FIRST]
	// must select PrimaryKeyScan(ScanReverse) directly with ZERO Sort enforcer
	// and stream [5, 4, 3, 2, 1] directly from storage's reverse cursor.
	builder, err := ir.NewBuilder(limits)
	if err != nil {
		t.Fatal(err)
	}
	scopeID, err := builder.AddScope(ctx, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	int8LayoutID, err := builder.AddLayout(ctx, ws3Int8Layout())
	if err != nil {
		t.Fatal(err)
	}
	idSlot, err := builder.AddSlot(ctx, ir.SlotInfo{Scope: scopeID, Layout: int8LayoutID, Object: f.table.ID(), Column: 1, Nullable: false})
	if err != nil {
		t.Fatal(err)
	}
	scanRel, err := builder.AddRel(ctx, ir.RelSpec{
		Rel: ir.Rel{
			Kind:              ir.Scan,
			Scope:             scopeID,
			Occurrence:        1,
			Object:            f.table.ID(),
			DescriptorVersion: 1,
			Generation:        1,
			EncodingVersion:   1,
		},
		Outputs: []model.SlotID{idSlot},
	})
	if err != nil {
		t.Fatal(err)
	}
	scanGraph, err := builder.Build(ctx, scanRel)
	if err != nil {
		t.Fatal(err)
	}
	planOpts, err := f.prepareOptions(snapshot, view, 64, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	planOpts.Optimize.Required = physical.Properties{
		Distribution:  physical.Distribution{Kind: physical.SingleNode, Partitions: 1},
		Rewindability: physical.OnePass,
		Ordering: []physical.OrderKey{{
			Key:       physical.Key{Slot: physical.Slot{Graph: 1, ID: idSlot, Layout: int8LayoutID}, Type: ws3Int8Type()},
			Operator:  samplePKRev.Operators[0],
			Direction: ir.Descending,
			Nulls:     ir.NullsFirst,
		}},
	}
	optimized, err := opt.Optimize(ctx, scanGraph, planOpts.Optimize)
	if err != nil {
		t.Fatalf("opt.Optimize(DESC required) failed: %v", err)
	}
	plan := optimized.Plan
	foundReverse := false
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, _ := plan.Node(id)
		if node.Metadata().Algorithm == physical.Sort {
			t.Fatal("DESC-ordered scan unexpectedly inserted a Sort enforcer instead of using ScanReverse")
		}
		if node.Metadata().Algorithm == physical.PrimaryKeyScan {
			delivered := node.Delivered()
			if delivered.OrderingCount() == 0 {
				t.Fatal("PrimaryKeyScan delivered no ordering")
			}
			first, _ := delivered.Ordering(0)
			if first.Direction != ir.Descending || first.Nulls != ir.NullsFirst {
				t.Fatalf("PrimaryKeyScan delivered ordering = %+v, want Descending NullsFirst (ScanReverse)", first)
			}
			foundReverse = true
		}
	}
	if !foundReverse {
		t.Fatal("DESC-ordered query did not select PrimaryKeyScan with ScanReverse delivered ordering")
	}
	// Execute the ScanReverse fragment end-to-end and verify descending rows [5,4,3,2,1].
	fragGraph, err := fragment.Build(ctx, plan, planOpts.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	query := f.newQuery(t, 16<<20)
	env := f.baseEnvironment(query)
	env.Reader = view
	env.BorrowReader = true
	env.BorrowQuery = true
	portal, err := exec.Open(ctx, fragGraph, env)
	if err != nil {
		t.Fatalf("exec.Open(ScanReverse) failed: %v", err)
	}
	defer func() {
		if err := portal.Close(); err != nil {
			t.Error(err)
		}
	}()
	var gotRows []int64
	var batch columnar.Batch
	for {
		nextErr := portal.Next(ctx, &batch)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		for row := range batch.Rows() {
			gotRows = append(gotRows, batch.Columns[0].Int64[batch.Lane(row)])
		}
	}
	wantRows := []int64{5, 4, 3, 2, 1}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("ScanReverse rows = %v, want %v", gotRows, wantRows)
	}
	for i := range wantRows {
		if gotRows[i] != wantRows[i] {
			t.Fatalf("ScanReverse rows = %v, want %v", gotRows, wantRows)
		}
	}
}

// TestWS3ConstrainEnvironmentBidirectionalWorkMem verifies that
// constrainEnvironment scales operator memory budgets both upward (when
// work_mem is increased) and downward (when work_mem is decreased), respects
// the query ceiling, and leaves unconfigured (zero) budgets untouched.
func TestWS3ConstrainEnvironmentBidirectionalWorkMem(t *testing.T) {
	env := exec.Environment{
		Spool:     exrowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20},
		Sort:      exsort.Options{RunBytes: 64 << 10, WorkBytes: 8 << 20},
		Join:      exjoin.Options{MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20},
		Aggregate: exaggregate.Options{GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10},
		Set:       exsetop.Options{GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxSpillBytes: 16 << 20},
		Window:    exwindow.Options{WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitionBytes: 16 << 20},
		Apply:     exapply.Options{WorkBytes: 64 << 10},
		Modify:    exmodify.Options{WorkBytes: 64 << 20, SortRunBytes: 1 << 20, SpoolMemoryBytes: 64 << 10},
		Recursive: exrecursive.Options{
			Spool: exrowstore.Limits{MemoryBytes: 64 << 10},
			Seen:  exdistinctset.Options{MemoryBytes: 4096, WorkBytes: 256 << 10},
		},
		// Exchange.WorkBytes intentionally 0 (unconfigured).
	}

	const largeWork = int64(128 << 20)
	constrainEnvironment(&env, largeWork, 256<<20)

	if env.Sort.WorkBytes != uint64(largeWork) {
		t.Fatalf("Sort.WorkBytes = %d, want %d", env.Sort.WorkBytes, largeWork)
	}
	if env.Sort.RunBytes <= 64<<10 || env.Sort.RunBytes != uint64(largeWork)/8 {
		t.Fatalf("Sort.RunBytes = %d, want scaled up to %d", env.Sort.RunBytes, uint64(largeWork)/8)
	}
	if env.Join.WorkBytes != uint64(largeWork) || env.Join.HashBytes != uint64(largeWork)/2 || env.Join.MemoryBytes != uint64(largeWork)/16 {
		t.Fatalf("Join budgets did not scale up: %+v", env.Join)
	}
	if env.Aggregate.WorkBytes != uint64(largeWork) || env.Aggregate.GroupBytes != uint64(largeWork)/4 {
		t.Fatalf("Aggregate budgets did not scale up: %+v", env.Aggregate)
	}
	if env.Window.WorkBytes != uint64(largeWork) {
		t.Fatalf("Window.WorkBytes = %d, want %d", env.Window.WorkBytes, largeWork)
	}
	if env.Set.WorkBytes != uint64(largeWork) || env.Set.GroupBytes != uint64(largeWork)/2 {
		t.Fatalf("Set budgets did not scale up: %+v", env.Set)
	}
	if env.Spool.MemoryBytes != uint64(largeWork)/4 {
		t.Fatalf("Spool.MemoryBytes = %d, want %d", env.Spool.MemoryBytes, uint64(largeWork)/4)
	}
	if env.Exchange.WorkBytes != 0 {
		t.Fatalf("unconfigured Exchange.WorkBytes = %d, want 0", env.Exchange.WorkBytes)
	}

	// Scaling downward when work_mem is reduced.
	const smallWork = int64(1 << 20)
	constrainEnvironment(&env, smallWork, 4<<20)
	if env.Sort.WorkBytes != uint64(smallWork) || env.Sort.RunBytes != uint64(smallWork)/8 {
		t.Fatalf("Sort did not scale down: %+v", env.Sort)
	}
	if env.Join.WorkBytes != uint64(smallWork) || env.Join.HashBytes != uint64(smallWork)/2 || env.Join.MemoryBytes != uint64(smallWork)/16 {
		t.Fatalf("Join did not scale down: %+v", env.Join)
	}
	if env.Spool.MaxBytes != 4<<20 {
		t.Fatalf("Spool.MaxBytes = %d, want 4MiB", env.Spool.MaxBytes)
	}

	// Query ceiling safely bounds upward scaling even when work_mem exceeds Query.WorkBytes.
	f := newWS3Fixture(t, 0, 0)
	env.Query = f.newQuery(t, 32<<20)
	constrainEnvironment(&env, 128<<20, 256<<20)
	if env.Sort.WorkBytes != 32<<20 || env.Join.HashBytes != 16<<20 || env.Spool.MemoryBytes != 8<<20 {
		t.Fatalf("constrainEnvironment exceeded query work ceiling: sort=%d joinHash=%d spool=%d", env.Sort.WorkBytes, env.Join.HashBytes, env.Spool.MemoryBytes)
	}
}

// TestWS3RoutinePlanCacheAndInvalidation verifies that routineExecutor.executeSQL:
// 1) Reuses cached bound+planned statements across repeated loop iterations with
//    changing parameter values (zero re-binding and zero re-planning on hits).
// 2) Reuses cached bound statements for parameterized scan queries while passing
//    per-iteration parameter values cleanly without stale results.
// 3) Automatically invalidates cached plans when CatalogReadIdentity changes.
func TestWS3RoutinePlanCacheAndInvalidation(t *testing.T) {
	f := newWS3Fixture(t, 0, 0)
	ctx := t.Context()

	registry := nativeCatalogTestRegistry(t)
	seqAllocator := &sessionSequenceAllocator{}
	seqSession := newSequenceSessionTest(t, seqAllocator, 8)
	builtins := f.settings.BuiltinOptions(950, 950)
	if err := f.session.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	tx := f.session.transaction
	frameView, err := tx.ReadView(ctx, catalogLayoutDependencies(f.catalog))
	if err != nil {
		t.Fatal(err)
	}
	frameViewClosed := false
	defer func() {
		if !frameViewClosed {
			if err := frameView.Close(); err != nil {
				t.Error(err)
			}
		}
	}()

	var planningCalls atomic.Int64
	var negotiateCalls atomic.Int64
	scope := &preparedMutationScope{transaction: tx, ambient: true}
	scope.active.Store(true)
	defer scope.active.Store(false)
	execCtx := context.WithValue(ctx, preparedScopeKey{}, scope)

	baseQuery := f.newQuery(t, 16<<20)
	env := f.baseEnvironment(baseQuery)
	env.Settings = f.session.settingRuntime(execCtx)
	typeRuntimeOpts := &DeclaredTypeRuntimeOptions{
		Descriptors: ws3DescriptorLimits(),
		Expressions: ColumnExpressionOptions{
			Binding:     ws3IRLimits(),
			Evaluation:  expr.DefaultLimits(),
			InputBytes:  1 << 20,
			ResultBytes: 1 << 20,
			Builtin:     builtins,
			Functions:   ws3UnresolvedFunctions{},
		},
		Routines: procedural.DefaultLimits(),
	}
	routineOpts := &RoutineExecutionOptions{
		Planning: func(_ context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader, _ *zpgparse.RawStmt, _ []model.TypeLayout) (PrepareOptions, error) {
			planningCalls.Add(1)
			return f.prepareOptions(snapshot, reader, 16, nil, &negotiateCalls)
		},
		NewQuery: func(qCtx context.Context) (*runtime.Query, error) {
			return runtime.NewQuery(qCtx, f.pool, f.budget, runtime.OSFileSystem{}, runtime.QueryConfig{
				WorkBytes:   16 << 20,
				OutputBytes: 4 << 20,
				SpillBytes:  32 << 20,
				Directory:   f.spillDir,
				MaxFiles:    16,
				MaxReaders:  8,
			})
		},
		Limits: procedural.DefaultLimits(),
		Capabilities: RoutineTransactionCapabilities{
			Sequence:         seqSession,
			CatalogRelations: registry,
			BaseFunctions:    ws3UnresolvedFunctions{},
			BaseBuiltin:      builtins,
			BaseKeys:         f.encoder,
			PathBytes:        1 << 20,
		},
	}
	preparedExec := PreparedExecution{
		Catalog:      f.catalog,
		Dependencies: catalogLayoutDependencies(f.catalog),
		Environment:  env,
		TypeRuntime:  typeRuntimeOpts,
		Routines:     routineOpts,
	}
	execCtx, executor, err := attachRoutineExecutionState(execCtx, f.session, tx, frameView, scope, &env, ws3CatalogScanner{}, f.catalog, preparedExec, routineOpts, nil, false)
	if err != nil {
		t.Fatalf("attachRoutineExecutionState failed: %v", err)
	}
	defer func() {
		if err := executor.closeCapabilities(); err != nil {
			t.Error(err)
		}
	}()

	// Part A: Pure PL/pgSQL-style expression loop (SELECT $1 + 1) across 10
	// iterations with freshly parsed ASTs each iteration (mirroring
	// procedural.Program.rawStatement cloning).
	for i := int64(1); i <= 10; i++ {
		res, err := executor.ExecuteSQL(execCtx, procedural.SQLRequest{
			Statement: parseSingleStmt(t, "SELECT $1 + 1"),
			Parameters: []procedural.Parameter{{
				Layout: ws3Int8Layout(),
				Value:  model.Int64Val(i),
			}},
			MaxRows:  16,
			MaxBytes: 1 << 20,
		})
		if err != nil {
			t.Fatalf("loop iter %d ExecuteSQL(SELECT $1 + 1) failed: %v", i, err)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0].IntVal != i+1 {
			t.Fatalf("loop iter %d got %+v, want [%d]", i, res.Rows, i+1)
		}
	}
	// Iteration 1 binds + plans once (2 Planning calls); iterations 2..10 must
	// all hit the cache with ZERO additional Planning calls!
	afterPureLoop := planningCalls.Load()
	if afterPureLoop != 2 {
		t.Fatalf("pure expression loop made %d Planning calls across 10 iterations, want 2 (1 miss + 9 full cache hits)", afterPureLoop)
	}

	// Part B: Parameterized table scan inside a loop (SELECT val FROM items WHERE id = $1).
	// Iteration 1: 1 bind + 1 trial plan (2 Planning calls, 2 Negotiate calls).
	// Iterations 2..3: bound statement hits cache (0 bind calls!), trial plan
	// narrows the scan for each iteration's $1 value (1 Planning call per iter,
	// no double-planning!) and returns the exact row without stale parameter reuse.
	planningCalls.Store(0)
	negotiateCalls.Store(0)
	for id := int64(1); id <= 3; id++ {
		res, err := executor.ExecuteSQL(execCtx, procedural.SQLRequest{
			Statement: parseSingleStmt(t, "SELECT val FROM items WHERE id = $1"),
			Parameters: []procedural.Parameter{{
				Layout: ws3Int8Layout(),
				Value:  model.Int64Val(id),
			}},
			MaxRows:  16,
			MaxBytes: 1 << 20,
		})
		if err != nil {
			t.Fatalf("scan iter %d ExecuteSQL failed: %v", id, err)
		}
		if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || res.Rows[0][0].IntVal != id*100 {
			t.Fatalf("scan iter %d got %+v, want [%d]", id, res.Rows, id*100)
		}
	}
	// Iter 1: bind (1) + trial plan (1) = 2. Iter 2: trial plan (1). Iter 3: trial plan (1). Total = 4.
	if got := planningCalls.Load(); got != 4 {
		t.Fatalf("parameterized scan loop made %d Planning calls across 3 iterations, want 4 (1 bind + 3 single plans)", got)
	}
	// 3 single optimization passes * 2 candidate paths (fwd + rev) = 6 Negotiate calls (not 12).
	if got := negotiateCalls.Load(); got != 6 {
		t.Fatalf("parameterized scan loop made %d Negotiate calls across 3 iterations, want 6", got)
	}

	// Part C: CatalogReadIdentity change must automatically invalidate cached plans.
	// Release the read transaction holding the catalog share lock before running
	// a separate DDL transaction so the catalog update lock acquires immediately.
	oldIdentity := readViewCatalogIdentity(frameView)
	frameViewClosed = true
	if err := frameView.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Rollback(ctx, false); err != nil {
		t.Fatal(err)
	}

	ddlTx, err := f.manager.Begin(ctx, txn.TransactionOptions{
		Mode:         txn.ReadWrite,
		MemoryBytes:  8 << 20,
		DisableSpill: true,
		MaxViews:     16,
		Resolver:     ws3PolicyResolver(f.normalizers),
		KeyEncoder:   f.encoder,
	})
	if err != nil {
		t.Fatal(err)
	}
	extraSchema, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID:      99,
		OID:     10099,
		Name:    model.NamespaceName{Kind: model.ObjectSchema, Name: "ws3_invalidation"},
		Version: 1,
		State:   model.StatePublic,
		Schema:  &model.SchemaMetadata{Owner: 1},
	}, ws3DescriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := ddlTx.Statement(ctx, func(stmtCtx context.Context) error {
		ddlView, err := ddlTx.ReadView(stmtCtx, catalogLayoutDependencies(f.catalog))
		if err != nil {
			return err
		}
		defer func() { _ = ddlView.Close() }()
		ddlSnap, err := f.catalog.TransactionSnapshot(ddlView, catalog.BorrowReader)
		if err != nil {
			return err
		}
		defer func() { _ = ddlSnap.Close() }()
		change, err := f.catalog.Create(stmtCtx, ddlSnap, extraSchema)
		if err != nil {
			return err
		}
		return ddlTx.ApplyMutations(stmtCtx, change.Mutations)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ddlTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Open a fresh read view on a new transaction so its CatalogReadIdentity
	// reflects the newly committed catalog version, and install it on executor.
	if err := f.session.Begin(ctx, TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	newView, err := f.session.transaction.ReadView(ctx, catalogLayoutDependencies(f.catalog))
	if err != nil {
		t.Fatal(err)
	}
	if readViewCatalogIdentity(newView) == oldIdentity {
		t.Fatalf("expected CatalogReadIdentity to advance after committed DDL: before=%+v after=%+v", oldIdentity, readViewCatalogIdentity(newView))
	}
	if err := executor.replaceFrame(execCtx, newView, true); err != nil {
		t.Fatal(err)
	}

	planningCalls.Store(0)
	res, err := executor.ExecuteSQL(execCtx, procedural.SQLRequest{
		Statement:  parseSingleStmt(t, "SELECT $1 + 1"),
		Parameters: []procedural.Parameter{{Layout: ws3Int8Layout(), Value: model.Int64Val(41)}},
		MaxRows:    16,
		MaxBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("post-DDL ExecuteSQL failed: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 42 {
		t.Fatalf("post-DDL result = %+v, want [42]", res.Rows)
	}
	if got := planningCalls.Load(); got != 2 {
		t.Fatalf("post-DDL first call made %d Planning calls, want 2 (cache invalidated by CatalogReadIdentity change)", got)
	}
	// Subsequent call at the new CatalogReadIdentity hits the cache again!
	res, err = executor.ExecuteSQL(execCtx, procedural.SQLRequest{
		Statement:  parseSingleStmt(t, "SELECT $1 + 1"),
		Parameters: []procedural.Parameter{{Layout: ws3Int8Layout(), Value: model.Int64Val(42)}},
		MaxRows:    16,
		MaxBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("post-DDL second ExecuteSQL failed: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].IntVal != 43 {
		t.Fatalf("post-DDL second result = %+v, want [43]", res.Rows)
	}
	if got := planningCalls.Load(); got != 2 {
		t.Fatalf("post-DDL cached call made %d total Planning calls, want 2 (cache hit at new identity)", got)
	}
}
