package exec_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

type pushdownStore struct {
	backend   *neosepia.Backend
	encoder   storage.KeyEncoder
	authority *txn.FenceAuthority
	epoch     clock.Epoch
	directory string
	request   storage.PushdownRequest
}

func newPushdownStore(t *testing.T, configure ...func(*neosepia.Limits)) *pushdownStore {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	s := &pushdownStore{encoder: encoder, authority: authority, epoch: clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}, directory: t.TempDir()}
	limits := neosepia.DefaultLimits()
	limits.BlockRows = 2
	for _, apply := range configure {
		apply(&limits)
	}
	s.backend, err = neosepia.Open(t.Context(), neosepia.Config{Directory: s.directory, Scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}, KeyEncoder: encoder, FenceVerifier: authority, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.backend.Close(); err != nil {
			t.Error(err)
		}
	})
	integer := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	numeric := model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}
	s.request = storage.PushdownRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: integer}, {ID: 2, Type: integer, Nullable: true}, {ID: 3, Type: numeric, Nullable: true}}, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: integer}}, ColumnIDs: []model.ColumnID{1}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
	s.apply(t, 20, s.upsert(t, 1, model.Int64Val(10), "0"), s.upsert(t, 2, model.NullVal(model.TypeInt64), "0"), s.upsert(t, 3, model.Int64Val(10), "0"), s.upsert(t, 4, model.Int64Val(20), "1.0"), s.upsert(t, 5, model.Int64Val(10), "0"), s.upsert(t, 6, model.Int64Val(0), ""))
	s.apply(t, 30, s.upsert(t, 1, model.Int64Val(99), "0"))
	return s
}

func (s *pushdownStore) key(t *testing.T, id int64) []byte {
	t.Helper()
	key, err := s.encoder.EncodePrefix(t.Context(), s.request.KeyLayout, []model.Value{model.Int64Val(id)}, 128)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
func (s *pushdownStore) upsert(t *testing.T, id int64, value model.Value, decimal string) storage.Mutation {
	t.Helper()
	amount := model.Value{Type: model.TypeNumeric, StrVal: decimal}
	if decimal == "" {
		amount = model.NullVal(model.TypeNumeric)
	}
	return storage.NewUpsertMutation(s.request.Target, s.key(t, id), []model.ColumnID{1, 2, 3}, []model.Value{model.Int64Val(id), value, amount})
}
func (s *pushdownStore) apply(t *testing.T, timestamp model.Timestamp, mutations ...storage.Mutation) {
	t.Helper()
	for i := range mutations {
		mutations[i].Ordinal = uint64(i + 1)
	}
	batch := storage.MutationBatch{TxID: model.TxnID{byte(timestamp)}, CommitTS: timestamp, Epoch: s.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: s.request.Target, Columns: s.request.Layout}}, Mutations: mutations}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}
func (s *pushdownStore) snapshot(t *testing.T, timestamp model.Timestamp, overlay storage.OverlayView) storage.SnapshotReader {
	t.Helper()
	state, err := s.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := s.authority.Issue(t.Context(), max(timestamp, state.ClosedTS), state.LogPosition, s.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: timestamp, Fence: fence, Overlay: overlay})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}
func (s *pushdownStore) predicate(t *testing.T, disjunction bool) storage.Predicate {
	t.Helper()
	boolean := model.Type{ID: 16, Storage: model.TypeBoolean}
	nodes := []storage.PredicateNode{{Kind: storage.PredicateColumn, ColumnID: 2, Type: s.request.Layout[1].Type}, {Kind: storage.PredicateConstant, ConstantIndex: 0, Type: s.request.Layout[1].Type}, {Kind: storage.PredicateComparison, Operator: 410, Left: 0, Right: 1, Type: boolean}}
	constants := []model.Value{model.Int64Val(10)}
	if disjunction {
		nodes = append(nodes, storage.PredicateNode{Kind: storage.PredicateColumn, ColumnID: 3, Type: s.request.Layout[2].Type}, storage.PredicateNode{Kind: storage.PredicateConstant, ConstantIndex: 1, Type: s.request.Layout[2].Type}, storage.PredicateNode{Kind: storage.PredicateComparison, Operator: 1752, Left: 3, Right: 4, Type: boolean}, storage.PredicateNode{Kind: storage.PredicateOr, Left: 2, Right: 5, Type: boolean})
		constants = append(constants, model.Value{Type: model.TypeNumeric, StrVal: "1"})
	}
	predicate, err := storage.NewPredicate(nodes, nil, constants)
	if err != nil {
		t.Fatal(err)
	}
	return predicate
}

// This adapter declines optional native filtering, not query execution. The
// native plan still owns its binding and performs visibility, ordering and I/O.
type residualPolicyReader struct {
	storage.SnapshotReader
	residual     bool
	native       storage.PushdownPlan
	opened       int
	closed       bool
	cursorClosed bool
	accepted     bool
}

func (r *residualPolicyReader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	nativeRequest := request
	if r.residual {
		nativeRequest.Predicate = storage.Predicate{}
		nativeRequest.SafeLimit = storage.ScanLimit{}
		nativeRequest.ColumnIDs = make([]model.ColumnID, len(request.Layout))
		for i, column := range request.Layout {
			nativeRequest.ColumnIDs[i] = column.ID
		}
	}
	plan, err := r.SnapshotReader.PlanScan(ctx, nativeRequest)
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	r.native = plan
	if r.residual {
		plan.Request = request
		plan.Accepted = storage.Predicate{}
		plan.Residual = request.Predicate
		plan.AppliedLimit = storage.ScanLimit{}
	}
	r.accepted = !plan.Accepted.Empty()
	return plan, nil
}
func (r *residualPolicyReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	if plan.Binding != r.native.Binding {
		return nil, storage.ErrInvalidScanPlan
	}
	cursor, err := r.SnapshotReader.VectorizedScan(ctx, r.native)
	if err != nil {
		return nil, err
	}
	r.opened++
	return &residualPolicyCursor{BatchCursor: cursor, owner: r}, nil
}
func (r *residualPolicyReader) Close() error { r.closed = true; return r.SnapshotReader.Close() }

type residualPolicyCursor struct {
	storage.BatchCursor
	owner *residualPolicyReader
}

func (c *residualPolicyCursor) Close() error {
	c.owner.cursorClosed = true
	return c.BatchCursor.Close()
}

func pushdownReleased(t *testing.T, query *runtime.Query, budget *runtime.Budget, reader *residualPolicyReader) {
	t.Helper()
	if !reader.closed || !reader.cursorClosed || reader.opened != 1 {
		t.Errorf("scan ownership: snapshot=%v cursor=%v opens=%d", reader.closed, reader.cursorClosed, reader.opened)
	}
	usage := query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 || budget.Usage().Reservations != 0 {
		t.Errorf("released scan retains resources: query=%+v shared=%+v", usage, budget.Usage())
	}
}

func TestPushdownResidualContract(t *testing.T) {
	t.Run("visibility_projection_and_limit", func(t *testing.T) {
		s := newPushdownStore(t)
		overlay, err := txn.NewOverlay(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := overlay.Close(); err != nil {
				t.Error(err)
			}
		})
		updates := []storage.Mutation{s.upsert(t, 2, model.Int64Val(10), "0"), storage.NewDeleteMutation(s.request.Target, s.key(t, 3))}
		for i := range updates {
			updates[i].Ordinal = uint64(i + 1)
		}
		if err := overlay.StageOwned(t.Context(), updates); err != nil {
			t.Fatal(err)
		}
		own, err := overlay.Freeze(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := own.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, sample := range []struct {
			name               string
			timestamp          model.Timestamp
			overlay            storage.OverlayView
			equal, disjunction []int64
		}{
			{"historical", 20, nil, []int64{1, 3, 5}, []int64{1, 3, 4, 5}},
			{"newer_nonmatching_version", 30, nil, []int64{3, 5}, []int64{3, 4, 5}},
			{"own_update_and_delete", 30, own, []int64{2, 5}, []int64{2, 4, 5}},
		} {
			t.Run(sample.name, func(t *testing.T) {
				for _, disjunction := range []bool{false, true} {
					for _, limit := range []bool{false, true} {
						for _, residual := range []bool{false, true} {
							t.Run(fmt.Sprintf("or_%v_limit_%v_residual_%v", disjunction, limit, residual), func(t *testing.T) {
								reader := &residualPolicyReader{SnapshotReader: s.snapshot(t, sample.timestamp, sample.overlay), residual: residual}
								request := s.request
								request.Predicate = s.predicate(t, disjunction)
								env, budget := environment(t)
								env.Reader = reader
								env.Keys = s.encoder
								env.Sort = exsort.Options{MaxRecordBytes: 1024, RunBytes: 64 << 10, WorkBytes: 4 << 20, MaxRuns: 4, MergeFanIn: 2}
								query := env.Query
								portal, err := exec.Open(t.Context(), pushdownOrderedGraph(t, reader, request, limit), env)
								if err != nil {
									t.Fatal(err)
								}
								defer closePortal(t, portal)
								got := rows(t, portal)
								wantIDs := sample.equal
								if disjunction {
									wantIDs = sample.disjunction
								}
								if limit {
									wantIDs = wantIDs[:1]
								}
								want := make([][]model.Value, len(wantIDs))
								for i, id := range wantIDs {
									want[i] = []model.Value{model.Int64Val(id)}
								}
								if !reflect.DeepEqual(got, want) {
									t.Fatalf("rows=%v want=%v", got, want)
								}
								if reader.accepted != (!residual && !disjunction) {
									t.Fatalf("native acceptance=%v residual policy=%v OR=%v", reader.accepted, residual, disjunction)
								}
								closePortal(t, portal)
								pushdownReleased(t, query, budget, reader)
							})
						}
					}
				}
			})
		}
	})
	t.Run("early_close_releases_pins_and_writer_progresses", pushdownEarlyClose)
	t.Run("corruption_is_not_retried", func(t *testing.T) {
		for _, residual := range []bool{false, true} {
			t.Run(fmt.Sprintf("residual_%v", residual), func(t *testing.T) {
				s := newPushdownStore(t)
				reader := &residualPolicyReader{SnapshotReader: s.snapshot(t, 30, nil), residual: residual}
				request := s.request
				request.Predicate = s.predicate(t, false)
				env, budget := environment(t)
				env.Reader = reader
				query := env.Query
				portal, err := exec.Open(t.Context(), scanGraph(t, reader, request, false, false), env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				files, err := filepath.Glob(filepath.Join(s.directory, "runs", "*.sst"))
				if err != nil || len(files) == 0 {
					t.Fatalf("SST inputs=%v err=%v", files, err)
				}
				for _, file := range files {
					if err := os.Truncate(file, 0); err != nil {
						t.Fatal(err)
					}
				}
				var batch columnar.Batch
				err = portal.Next(t.Context(), &batch)
				if !errors.Is(err, storage.ErrCorruptStorage) || batch.Rows() != 0 {
					t.Fatalf("corrupt scan=%v rows=%d", err, batch.Rows())
				}
				closePortal(t, portal)
				pushdownReleased(t, query, budget, reader)
			})
		}
	})
}

func pushdownEarlyClose(t *testing.T) {
	for _, residual := range []bool{false, true} {
		t.Run(fmt.Sprintf("residual_%v", residual), func(t *testing.T) {
			s := newPushdownStore(t)
			reader := &residualPolicyReader{SnapshotReader: s.snapshot(t, 30, nil), residual: residual}
			request := s.request
			request.Predicate = s.predicate(t, false)
			env, budget := environment(t)
			env.Reader = reader
			query := env.Query
			portal, err := exec.Open(t.Context(), scanGraph(t, reader, request, false, false), env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			for {
				if err := portal.Next(t.Context(), &batch); err != nil {
					t.Fatal(err)
				}
				if batch.Rows() != 0 {
					break
				}
			}
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(0), 128)
			if err != nil || batch.Rows() != 1 || value.IntVal != 3 {
				t.Fatalf("first short batch=%v rows=%d err=%v", value, batch.Rows(), err)
			}
			closePortal(t, portal)
			pushdownReleased(t, query, budget, reader)
			s.apply(t, 40, s.upsert(t, 7, model.Int64Val(10), "0"))
			oldFiles, err := filepath.Glob(filepath.Join(s.directory, "runs", "*.sst"))
			if err != nil || len(oldFiles) == 0 {
				t.Fatalf("pre-collection SST files=%v err=%v", oldFiles, err)
			}
			if err := s.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
				t.Fatal(err)
			}
			for _, name := range oldFiles {
				if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("released scan still pins %s: %v", name, err)
				}
			}
			current := s.snapshot(t, 40, nil)
			got, err := current.Get(t.Context(), storage.ReadRequest{Target: request.Target, Key: s.key(t, 7), ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: request.Layout})
			if err != nil || len(got.Values) != 3 || got.Values[0].IntVal != 7 || got.Values[1].IntVal != 10 {
				t.Fatalf("writer result after close and collection=%+v err=%v", got, err)
			}
		})
	}
}
