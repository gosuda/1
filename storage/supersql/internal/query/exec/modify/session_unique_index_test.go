package modify_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func uniqueSessionFixture(t *testing.T) (*sessionExecutionFixture, *schema.IndexPlan) {
	t.Helper()
	index, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 3, OID: 10003, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "items_value_key"},
		Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{Table: 1, Unique: true, Keys: []model.KeyElement{{Column: 2, Type: integer.Type}}},
	}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newSessionExecutionFixtureWithIndexes(t, index)
	plan, err := schema.NewIndexPlan(t.Context(), fixture.store.options.Descriptor, index, fixture.store.options.Encoder, nil, schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, plan
}

func (fixture *sessionExecutionFixture) peer(t *testing.T) *session.Transactions {
	t.Helper()
	settings, err := session.NewSettings(t.Context(), session.SettingsOptions{Resolver: sessionSettingsResolver{}, Limits: session.SettingLimits{MaxValueBytes: 4096, MaxSavepoints: 8, WorkMemoryBytes: 128 << 20, MaintenanceMemoryBytes: 128 << 20, TemporaryFileBytes: 64 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	peer, err := session.NewTransactions(t.Context(), session.TransactionConfig{Manager: fixture.store.manager, Settings: settings, Options: fixture.transactionOptions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := peer.Close(); err != nil {
			t.Error(err)
		}
	})
	return peer
}

func runUniqueSQL(t *testing.T, fixture *sessionExecutionFixture, transaction *session.Transactions, sql string) error {
	t.Helper()
	prepared := fixture.prepare(t, sql, nil)
	invocation := fixture.invocation(t)
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	statement := parsed.Stmts[0].Stmt
	if statement.GetUpdateStmt() != nil || statement.GetDeleteStmt() != nil {
		spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
		invocation.Dependencies = append(append([]txn.ReadDependency(nil), invocation.Dependencies...), txn.ReadDependency{Target: fixture.store.request(t, 1).Target, KeyLayout: fixture.store.options.KeyLayout, Spans: spans})
	}
	_, err = transaction.RunPrepared(t.Context(), prepared, invocation, func(context.Context, *session.Rows) error { return nil })
	return err
}

func requireUniqueSQLState(t *testing.T, err error, codes ...string) {
	t.Helper()
	var sqlError *model.Error
	if errors.As(err, &sqlError) {
		for _, code := range codes {
			if sqlError.Code == code {
				return
			}
		}
	}
	t.Fatalf("SQL error = %v, want SQLSTATE in %v", err, codes)
}

func requireUniquePair(t *testing.T, fixture *sessionExecutionFixture, plan *schema.IndexPlan, id, value int64) {
	t.Helper()
	if got, err := fixture.committedValue(t, id); err != nil || got != value {
		t.Fatalf("base row %d = %d, %v; want %d", id, got, err, value)
	}
	entry, err := committedUniqueEntry(t, fixture, plan, value)
	if err != nil {
		t.Fatalf("index key %d: %v", value, err)
	}
	owner := fixture.store.request(t, id).Key
	if len(entry.Values) != 3 || entry.Values[0].IntVal != value || !bytes.Equal(entry.Values[2].BytesVal, owner) {
		t.Fatalf("index key %d does not own base row %d: %+v", value, id, entry)
	}
}

func committedUniqueEntry(t *testing.T, fixture *sessionExecutionFixture, plan *schema.IndexPlan, value int64) (model.Row, error) {
	t.Helper()
	key, err := fixture.store.options.Encoder.EncodePrefix(t.Context(), plan.KeyLayout(), []model.Value{model.Int64Val(value), model.BytesVal(nil)}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	return view.Get(t.Context(), storage.ReadRequest{Target: plan.Target(), Key: key, ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: plan.Layout().Columns})
}

func requireUniqueAbsent(t *testing.T, fixture *sessionExecutionFixture, plan *schema.IndexPlan, id, value int64) {
	t.Helper()
	if _, err := fixture.committedValue(t, id); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("base row %d survived rollback: %v", id, err)
	}
	if _, err := committedUniqueEntry(t, fixture, plan, value); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("index key %d survived rollback: %v", value, err)
	}
}

func TestSessionUniqueIndexContenderWaitsForOwner(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture, plan := uniqueSessionFixture(t)
				if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
					t.Fatal(err)
				}
				if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(1,10)`); err != nil {
					t.Fatal(err)
				}
				peer := fixture.peer(t)
				prepared := fixture.prepare(t, `INSERT INTO items VALUES(2,10) RETURNING id`, nil)
				invocation := fixture.invocation(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := peer.RunPrepared(ctx, prepared, invocation, func(context.Context, *session.Rows) error { return nil })
					done <- err
				}()
				defer func() {
					cancel()
					synctest.Wait()
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("contender bypassed uncommitted unique owner: %v", err)
				default:
				}
				if commit {
					result, err := fixture.session.Commit(t.Context(), false)
					if err != nil || !result.Committed {
						t.Fatalf("owner commit = %+v, %v", result, err)
					}
				} else if err := fixture.session.Rollback(t.Context(), false); err != nil {
					t.Fatal(err)
				}
				err := <-done
				if peer.Active() {
					t.Fatal("implicit contender retained transaction")
				}
				if commit {
					requireUniqueSQLState(t, err, "23505", "40001")
					requireUniquePair(t, fixture, plan, 1, 10)
					if _, err := fixture.committedValue(t, 2); !errors.Is(err, storage.ErrRowNotFound) {
						t.Fatalf("conflicting base row persisted: %v", err)
					}
					requireUniqueSQLState(t, runUniqueSQL(t, fixture, peer, `INSERT INTO items VALUES(2,10)`), "23505")
				} else {
					if err != nil {
						t.Fatal(err)
					}
					requireUniquePair(t, fixture, plan, 2, 10)
					if _, err := fixture.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
						t.Fatalf("rolled back owner persisted: %v", err)
					}
				}
			})
		})
	}
}

func TestSessionUniqueIndexFailedMovePreservesOldPair(t *testing.T) {
	fixture, plan := uniqueSessionFixture(t)
	if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
		t.Fatal(err)
	}
	requireUniqueSQLState(t, runUniqueSQL(t, fixture, fixture.session, `UPDATE items SET value=20 WHERE id=1 RETURNING id`), "23505")
	requireUniquePair(t, fixture, plan, 1, 10)
	requireUniquePair(t, fixture, plan, 2, 20)
	if err := runUniqueSQL(t, fixture, fixture.session, `UPDATE items SET value=30 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	requireUniquePair(t, fixture, plan, 1, 30)
	if _, err := committedUniqueEntry(t, fixture, plan, 10); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("old unique key survived move: %v", err)
	}
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`DELETE FROM items WHERE id=1`, `INSERT INTO items VALUES(3,30)`} {
		if err := runUniqueSQL(t, fixture, fixture.session, sql); err != nil {
			t.Fatal(err)
		}
	}
	if result, err := fixture.session.Commit(t.Context(), false); err != nil || !result.Committed {
		t.Fatalf("delete and reinsert commit = %+v, %v", result, err)
	}
	requireUniquePair(t, fixture, plan, 3, 30)
	requireUniquePair(t, fixture, plan, 2, 20)
	if _, err := fixture.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("deleted base row survived replacement: %v", err)
	}
}

func TestSessionUniqueIndexReturningFailureReleasesPair(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "consumer_error"
		if canceled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			fixture, plan := uniqueSessionFixture(t)
			prepared := fixture.prepare(t, `INSERT INTO items VALUES(1,10),(2,20) RETURNING id`, nil)
			invocation := fixture.invocation(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, err := fixture.session.RunPrepared(ctx, prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
				batch := columnar.Batch{MaxRows: 1}
				if err := rows.Next(ctx, &batch); err != nil {
					return err
				}
				if canceled {
					cancel()
					return nil
				}
				return errSessionConsumer
			})
			want := errSessionConsumer
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || fixture.session.Active() {
				t.Fatalf("failed RETURNING = %v, active=%v; want %v and no transaction", err, fixture.session.Active(), want)
			}
			requireUniqueAbsent(t, fixture, plan, 1, 10)
			requireUniqueAbsent(t, fixture, plan, 2, 20)
			usage := invocation.Environment.Query.Usage()
			if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
				t.Fatalf("failed RETURNING retained resources: %+v", usage)
			}
			if err := runUniqueSQL(t, fixture, fixture.peer(t), `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
				t.Fatalf("failed RETURNING retained unique/base ownership: %v", err)
			}
			requireUniquePair(t, fixture, plan, 1, 10)
			requireUniquePair(t, fixture, plan, 2, 20)
		})
	}
}
