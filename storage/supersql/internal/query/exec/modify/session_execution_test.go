package modify_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	indexschema "github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type sessionPolicies map[storage.ObjectRef]*txn.RowNormalizer

func (policies sessionPolicies) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy, ok := policies[target]; ok {
		return policy, nil
	}
	return nil, storage.ErrInvalidTarget
}

type sessionSettingsResolver struct{}

func (sessionSettingsResolver) ResolveTimeZone(ctx context.Context, name string) (session.Zone, error) {
	if name != "UTC" && name != "GMT" {
		return session.Zone{}, storage.ErrInvalidTarget
	}
	return session.Zone{Name: name, Fixed: true}, ctx.Err()
}
func (sessionSettingsResolver) ResolveTextSearchConfig(ctx context.Context, name string, _ []string) (string, error) {
	if name != "pg_catalog.simple" {
		return "", storage.ErrInvalidTarget
	}
	return name, ctx.Err()
}
func (sessionSettingsResolver) ResolveFormatLocale(ctx context.Context, numeric, monetary, clock string) (model.FormatLocale, error) {
	if err := ctx.Err(); err != nil {
		return model.FormatLocale{}, err
	}
	if !strings.EqualFold(numeric, "C") || !strings.EqualFold(monetary, "C") || !strings.EqualFold(clock, "C") {
		return model.FormatLocale{}, storage.ErrInvalidTarget
	}
	return model.FormatLocale{
		NumericName: "C", MonetaryName: "C", TimeName: "C",
		DecimalPoint: ".", ThousandsSeparator: ",", PositiveSign: "+", NegativeSign: "-", CurrencySymbol: " ", ISOCurrencySymbol: " ",
		MonthNames:            [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
		AbbreviatedMonthNames: [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
		DayNames:              [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
		AbbreviatedDayNames:   [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
	}, nil
}

type sessionExecutionFixture struct {
	store              *store
	catalog            *catalog.Store
	settings           *session.Settings
	session            *session.Transactions
	pool               *runtime.Pool
	budget             *runtime.Budget
	descriptors        []*model.Descriptor
	dependencies       []txn.ReadDependency
	transactionOptions txn.TransactionOptions
}

func newSessionExecutionFixture(t *testing.T) *sessionExecutionFixture {
	t.Helper()
	return newSessionExecutionFixtureWithIndexes(t)
}

func newSessionExecutionFixtureWithIndexes(t *testing.T, indexes ...*model.Descriptor) *sessionExecutionFixture {
	t.Helper()
	return newSessionExecutionFixtureWithStore(t, newStore(t), indexes...)
}

// newNullableSessionExecutionFixture is the shared session fixture with a
// NULL-able value column, for cases whose semantics need a real NULL row.
func newNullableSessionExecutionFixture(t *testing.T) *sessionExecutionFixture {
	t.Helper()
	return newSessionExecutionFixtureWithStore(t, newNullableStore(t))
}

func newSessionExecutionFixtureWithDefault(t *testing.T, value model.Value) *sessionExecutionFixture {
	t.Helper()
	return newSessionExecutionFixtureWithStore(t, newStoreWithDefault(t, &value))
}

func newSessionExecutionFixtureWithStore(t *testing.T, s *store, indexes ...*model.Descriptor) *sessionExecutionFixture {
	t.Helper()
	if err := s.tx.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.query.Close(); err != nil {
		t.Fatal(err)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	cat, err := catalog.New(s.options.Encoder, catalog.Limits{Descriptor: limits, MaxEncodedBytes: 2 << 20, MaxKeyBytes: 4096, ScanBatchRows: 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 0 {
		spec := descriptorSpec()
		for _, index := range indexes {
			spec.Table.Indexes = append(spec.Table.Indexes, index.ID())
		}
		s.options.Descriptor, err = model.NewDescriptor(t.Context(), spec, limits)
		if err != nil {
			t.Fatal(err)
		}
	}
	schema, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 2, OID: 10002, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: 1}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sessionExecutionFixture{store: s, catalog: cat, descriptors: []*model.Descriptor{schema, s.options.Descriptor}}
	fixture.descriptors = append(fixture.descriptors, indexes...)
	policies := sessionPolicies{}
	target := s.request(t, 1).Target
	rules := []txn.ColumnRule{{Layout: s.options.Layout[0], KeyPosition: 1}, {Layout: s.options.Layout[1]}}
	spec, err := s.options.Descriptor.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	if value := spec.Table.Columns[1].ConstantDefault; value != nil {
		rules[1].Default = constantDefault(*value)
	}
	table, err := txn.NewRowNormalizer(t.Context(), target, rules, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	policies[target] = table
	for _, layout := range cat.Layouts() {
		policy, err := txn.NewRowNormalizer(t.Context(), layout.Target, []txn.ColumnRule{{Layout: layout.Columns[0], KeyPosition: 1}, {Layout: layout.Columns[1]}}, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		policies[layout.Target] = policy
	}
	options := txn.TransactionOptions{Mode: txn.ReadWrite, Resolver: policies, KeyEncoder: s.options.Encoder, MemoryBytes: 8 << 20, SpillBytes: 32 << 20, SpillDirectory: t.TempDir(), MaxViews: 16}
	bootstrap, err := s.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bootstrap.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	for _, descriptor := range fixture.descriptors {
		dependencies, err := cat.CreateDependencies(t.Context(), descriptor)
		if err != nil {
			t.Fatal(err)
		}
		view, err := bootstrap.ReadView(t.Context(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := cat.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			t.Fatal(err)
		}
		change, err := cat.Create(t.Context(), snapshot, descriptor)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			t.Fatal(err)
		}
		if err := bootstrap.ApplyMutations(t.Context(), change.Mutations); err != nil {
			t.Fatal(err)
		}
		fixture.dependencies = append(fixture.dependencies, cat.DescriptorDependency(descriptor.ID()))
	}
	if _, err := bootstrap.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 0 {
		jobs, err := indexschema.NewJobStore(t.Context(), indexschema.JobStoreOptions{
			Target:  storage.ObjectRef{ObjectID: 4, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
			Encoder: s.options.Encoder, MetadataBytes: 1 << 20, RecordBytes: 1 << 16, KeyBytes: 4096, MaxJobs: 16, BatchRows: 16,
		})
		if err != nil {
			t.Fatal(err)
		}
		provider, err := indexschema.NewIndexPolicyProvider(t.Context(), indexschema.IndexPolicyOptions{
			Catalog: cat, Views: s.manager, Jobs: jobs, Encoders: func(context.Context, *catalog.CatalogSnapshot) (indexschema.KeyCodec, error) {
				codec, ok := s.options.Encoder.(indexschema.KeyCodec)
				if !ok {
					return nil, storage.ErrInvalidLayout
				}
				return codec, nil
			}, Base: policies,
			Limits: indexschema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16},
		})
		if err != nil {
			t.Fatal(err)
		}
		policy, err := provider.TablePolicy(t.Context(), target)
		if err != nil {
			t.Fatal(err)
		}
		options.Resolver = policy.Resolver
		s.options.Expansion = policy.Indexes
	}
	fixture.transactionOptions = options
	fixture.settings, err = session.NewSettings(t.Context(), session.SettingsOptions{Resolver: sessionSettingsResolver{}, Limits: session.SettingLimits{MaxValueBytes: 4096, MaxSavepoints: 8, WorkMemoryBytes: 128 << 20, MaintenanceMemoryBytes: 128 << 20, TemporaryFileBytes: 64 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.session, err = session.NewTransactions(t.Context(), session.TransactionConfig{Manager: s.manager, Settings: fixture.settings, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	fixture.pool, err = runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	fixture.budget, err = runtime.NewBudget(256<<20, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fixture.pool.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		if err := fixture.session.Close(); err != nil {
			t.Error(err)
		}
	})
	return fixture
}

func (fixture *sessionExecutionFixture) prepare(t *testing.T, sql string, parameters []model.TypeLayout, configure ...func(*session.PrepareOptions)) *session.Prepared {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := fixture.planning(sql)
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	request := fixture.store.request(t, 1)
	options.Optimize.Access = mutationAccess{reader: view, target: request.Target, layout: request.Layout, keys: fixture.store.options.KeyLayout}
	for _, apply := range configure {
		apply(&options)
	}
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], mutationCatalog{fixture.descriptors}, options, parameters)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func (fixture *sessionExecutionFixture) planning(sql string) session.PrepareOptions {
	limits := ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: uint32(len(sql))}
	return session.PrepareOptions{Bind: bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: limits}, Normalize: normalize.Options{Limits: limits, MaxWork: 1000000, MaxBytes: 16 << 20, Builtin: fixture.settings.BuiltinOptions(950, 950)}, Optimize: opt.Options{IR: limits, Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20}, Fragment: fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20}}
}

func (fixture *sessionExecutionFixture) invocation(t *testing.T, parameters ...expr.ParameterValue) session.PreparedExecution {
	t.Helper()
	query, err := runtime.NewQuery(t.Context(), fixture.pool, fixture.budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 128 << 20, OutputBytes: 16 << 20, SpillBytes: 64 << 20, Directory: t.TempDir(), MaxFiles: 8, MaxReaders: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := expr.DefaultLimits()
	limits.MaxRows = 2
	options := fixture.store.options
	options.Transaction = nil
	environment := execution.Environment{Query: query, Keys: options.Encoder, Modify: options, Parameters: parameters, Builtin: fixture.settings.BuiltinOptions(950, 950), Limits: execution.Limits{Expressions: limits, BatchRows: 2, BatchBytes: 1 << 20, ParameterBytes: 1 << 20, MetadataBytes: 8 << 20}}
	environment.MutationTargets = execution.MutationTarget{Descriptor: options.Descriptor, Expansion: options.Expansion}
	return session.PreparedExecution{Catalog: fixture.catalog, Dependencies: fixture.dependencies, Environment: environment}
}

func (fixture *sessionExecutionFixture) committedValue(t *testing.T, id int64) (int64, error) {
	t.Helper()
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	row, err := view.Get(t.Context(), fixture.store.request(t, id))
	if err != nil {
		return 0, err
	}
	return row.Values[1].IntVal, nil
}

func TestSessionPreparedEarlyReturningCommitsWholeStatement(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	prepared := fixture.prepare(t, `INSERT INTO items VALUES(1,10),(2,20),(3,30) RETURNING id`, nil)
	invocation := fixture.invocation(t)
	var retained *session.Rows
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		retained = rows
		batch := columnar.Batch{MaxRows: 1}
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		if batch.Rows() != 1 {
			t.Fatalf("first RETURNING rows = %d", batch.Rows())
		}
		value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], 0, 4096)
		if err != nil {
			return err
		}
		if value.IntVal != 1 {
			t.Fatalf("first RETURNING id = %d", value.IntVal)
		}
		return nil
	})
	if err != nil || !result.Commit.Committed || result.RowsAffected != 3 {
		t.Fatalf("autocommit = %+v, %v", result, err)
	}
	for _, id := range []int64{1, 2, 3} {
		value, err := fixture.committedValue(t, id)
		if err != nil || value != id*10 {
			t.Fatalf("committed %d = %d, %v", id, value, err)
		}
	}
	var batch columnar.Batch
	if err := retained.Next(t.Context(), &batch); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("expired rows = %v", err)
	}
	usage := invocation.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("retained invocation resources: %+v", usage)
	}
}

var errSessionConsumer = errors.New("consumer rejected result")

func TestSessionPreparedConsumerFailureRollsBackAndSavepointRecovers(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	first := fixture.prepare(t, `INSERT INTO items VALUES(1,10)`, nil)
	if _, err := fixture.session.RunPrepared(t.Context(), first, fixture.invocation(t), func(context.Context, *session.Rows) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Savepoint(t.Context(), "keep"); err != nil {
		t.Fatal(err)
	}
	second := fixture.prepare(t, `INSERT INTO items VALUES(2,20) RETURNING id`, nil)
	_, err := fixture.session.RunPrepared(t.Context(), second, fixture.invocation(t), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		return errSessionConsumer
	})
	if !errors.Is(err, errSessionConsumer) || !fixture.session.Failed() {
		t.Fatalf("consumer failure = %v failed=%v", err, fixture.session.Failed())
	}
	if err := fixture.session.RollbackTo(t.Context(), "keep"); err != nil {
		t.Fatal(err)
	}
	committed, err := fixture.session.Commit(t.Context(), false)
	if err != nil || !committed.Committed {
		t.Fatalf("commit recovered transaction = %+v, %v", committed, err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("earlier statement = %d, %v", value, err)
	}
	if _, err := fixture.committedValue(t, 2); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("failed statement persisted: %v", err)
	}
}

func TestSessionPreparedTypedParametersAndFetchContextLifetime(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1}}
	prepared := fixture.prepare(t, `SELECT $1::text FROM generate_series(1,3)`, []model.TypeLayout{text})
	const payload = "x'); DELETE FROM items; --"
	invocation := fixture.invocation(t, expr.ParameterValue{Number: 1, Layout: text, Value: model.StringVal(payload)})
	seen := 0
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(_ context.Context, rows *session.Rows) error {
		for {
			fetch, cancel := context.WithCancel(context.Background())
			batch := columnar.Batch{MaxRows: 1}
			err := rows.Next(fetch, &batch)
			cancel()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 4096)
			if err != nil {
				return err
			}
			if value.StrVal != payload {
				t.Fatalf("typed parameter = %q", value.StrVal)
			}
			seen++
		}
	})
	if err != nil || !result.Commit.Committed || seen != 3 {
		t.Fatalf("typed execution = %+v rows=%d error=%v", result, seen, err)
	}
}

func TestSessionPreparedRejectsChangedDescriptorBeforeWrites(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	prepared := fixture.prepare(t, `INSERT INTO items VALUES(1,10)`, nil)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	spec, err := fixture.store.options.Descriptor.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.Version++
	next, err := model.NewDescriptor(t.Context(), spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	err = fixture.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		view, err := transaction.ReadView(ctx, fixture.dependencies)
		if err != nil {
			return err
		}
		snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		change, err := fixture.catalog.Update(ctx, snapshot, 1, next)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.session.RunPrepared(t.Context(), prepared, fixture.invocation(t), func(context.Context, *session.Rows) error { return nil })

	var state *model.Error
	if !errors.As(err, &state) || state.Code != "0A000" {
		t.Fatalf("stale descriptor execution = %v", err)
	}
	if _, err := fixture.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("stale plan wrote row: %v", err)
	}
}

type copySessionTrigger struct {
	begin, before, after, finish, abort int
}

func (trigger *copySessionTrigger) BeginStatement(_ context.Context, _ *model.Descriptor, event model.TriggerEvent, _ []model.ColumnID) error {
	if event != model.TriggerInsert {
		return errors.New("unexpected COPY trigger event")
	}
	trigger.begin++
	return nil
}

func (trigger *copySessionTrigger) BeforeRow(_ context.Context, _ *model.Descriptor, event model.TriggerEvent, _ *model.Row, next *model.Row, _ uint64) (*model.Row, bool, error) {
	if event != model.TriggerInsert || next == nil || len(next.Values) != 2 {
		return nil, false, errors.New("unexpected COPY trigger row")
	}
	trigger.before++
	values := make([]model.Value, len(next.Values))
	for i := range next.Values {
		values[i] = next.Values[i].Clone()
	}
	values[1].IntVal++
	return &model.Row{Values: values}, true, nil
}

func (trigger *copySessionTrigger) AfterRow(_ context.Context, _ *model.Descriptor, event model.TriggerEvent, _, next *model.Row, _ uint64) error {
	if event != model.TriggerInsert || next == nil {
		return errors.New("unexpected COPY after-row trigger")
	}
	trigger.after++
	return nil
}

func (trigger *copySessionTrigger) FinishStatement(_ context.Context, _ *model.Descriptor, event model.TriggerEvent) error {
	if event != model.TriggerInsert {
		return errors.New("unexpected COPY finish trigger")
	}
	trigger.finish++
	return nil
}

func (trigger *copySessionTrigger) AbortStatement(_ *model.Descriptor, event model.TriggerEvent) {
	if event == model.TriggerInsert {
		trigger.abort++
	}
}

func TestSessionCopyFromUsesDefaultsTriggersAndAtomicRollback(t *testing.T) {
	fixture := newSessionExecutionFixtureWithDefault(t, model.Int64Val(99))
	prepared := fixture.prepare(t, `COPY items (id,value) FROM STDIN WITH (DEFAULT 'default', ON_ERROR ignore)`, nil)
	invocation := fixture.invocation(t)
	trigger := &copySessionTrigger{}
	invocation.Environment.Triggers = trigger
	invocation.Copy = &qcopy.Streams{Reader: strings.NewReader("1\tdefault\n2\tbad\n3\t30\n"), MaxRecordBytes: 4096}
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, nil)
	if err != nil || !result.Commit.Committed || result.RowsAffected != 2 {
		t.Fatalf("COPY FROM result = %+v, %v", result, err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 100 {
		t.Fatalf("default and trigger value = %d, %v", value, err)
	}
	if _, err := fixture.committedValue(t, 2); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("ignored conversion row persisted: %v", err)
	}
	if value, err := fixture.committedValue(t, 3); err != nil || value != 31 {
		t.Fatalf("ordinary triggered value = %d, %v", value, err)
	}
	if trigger.begin != 1 || trigger.before != 2 || trigger.after != 2 || trigger.finish != 1 || trigger.abort != 0 {
		t.Fatalf("successful COPY trigger lifecycle = %+v", trigger)
	}

	failedFixture := newSessionExecutionFixture(t)
	failed := failedFixture.prepare(t, `COPY items FROM STDIN`, nil)
	failedInvocation := failedFixture.invocation(t)
	failedInvocation.Environment.Limits.BatchRows = 1
	failedTrigger := &copySessionTrigger{}
	failedInvocation.Environment.Triggers = failedTrigger
	failedInvocation.Copy = &qcopy.Streams{Reader: &copySessionErrorReader{}, MaxRecordBytes: 4096}
	if _, err := failedFixture.session.RunPrepared(t.Context(), failed, failedInvocation, nil); !errors.Is(err, errCopySessionRead) {
		t.Fatalf("COPY read failure = %v", err)
	}
	if _, err := failedFixture.committedValue(t, 4); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("row before COPY read failure persisted: %v", err)
	}
	if failedTrigger.begin != 1 || failedTrigger.before != 1 || failedTrigger.after != 1 || failedTrigger.abort != 1 || failedTrigger.finish != 0 {
		t.Fatalf("failed COPY trigger lifecycle = %+v", failedTrigger)
	}
}

var errCopySessionRead = errors.New("COPY input failed")

type copySessionErrorReader struct{ sent bool }

func (reader *copySessionErrorReader) Read(buffer []byte) (int, error) {
	if !reader.sent {
		reader.sent = true
		return copy(buffer, "4\t40\n5\t"), nil
	}
	return 0, errCopySessionRead
}
