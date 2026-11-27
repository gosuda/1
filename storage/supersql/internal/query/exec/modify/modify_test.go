package modify_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"

	"google.golang.org/protobuf/proto"
)

func requireImage(t *testing.T, image model.Value, id, value int64) {
	t.Helper()
	if image.Type != model.TypeComposite || image.IsNull || len(image.ArrayVal) != 2 || image.ArrayVal[0].IntVal != id || image.ArrayVal[1].IntVal != value {
		t.Fatalf("image = %+v, want (%d,%d)", image, id, value)
	}
}

func requireClean(t *testing.T, s *store) {
	t.Helper()
	usage := s.query.Usage()
	if usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("open spill resources: %+v", usage)
	}
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Spill.Live != 0 {
		t.Fatalf("retained query reservations: %+v", usage)
	}
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill files remain: %v", entries)
	}
}

func TestInsertStagesAllRowsBeforeReturningAndSurvivesReopen(t *testing.T) {
	s := newStore(t)
	cursor := s.cursor(t, row(1, 10), row(2, 20), row(3, 30))
	s.absent(t, 1)
	batch := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || !batch.Columns[0].IsNull(0) {
		t.Fatalf("INSERT OLD = %+v", batch)
	}
	image, _, err := columnar.CopyValue(t.Context(), &batch.Columns[1], 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	requireImage(t, image, 1, 10)
	for _, id := range []int64{1, 2, 3} {
		s.require(t, id, id*10)
	}
	if s.tx.State() != txn.Active {
		t.Fatalf("cursor committed transaction: %v", s.tx.State())
	}
	if s.query.Usage().Files == 0 {
		t.Fatal("RETURNING was not spilled")
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	requireClean(t, s)
	s.require(t, 3, 30)
	if _, err := s.tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.backend.Close(); err != nil {
		t.Fatal(err)
	}
	s.backend, err = neosepia.Open(t.Context(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.openManager(t)
	reader, err := s.manager.StrongSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, id := range []int64{1, 2, 3} {
		got, err := reader.Get(t.Context(), s.request(t, id))
		if err != nil {
			t.Fatal(err)
		}
		if got.Values[0].IntVal != id || got.Values[1].IntVal != id*10 {
			t.Fatalf("reopened row %d = %+v", id, got.Values)
		}
	}
}

func TestInsertReturningTableOIDUsesPhysicalTarget(t *testing.T) {
	s := newStore(t)
	s.options.Returning = program(t, s.options.ReturningInputLayouts, []int{2, 3}, nil)
	cursor := s.cursor(t, row(1, 10))
	rows := returned(t, cursor)
	if len(rows) != 1 || len(rows[0]) != 2 || !rows[0][0].IsNull || rows[0][1].IsNull || rows[0][1].IntVal != 10001 {
		t.Fatalf("RETURNING tableoid = %+v", rows)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	requireClean(t, s)
}

func TestUpdateDeduplicatesBeforeVolatileAssignments(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 11))
	s.options.Operation = storage.MutationUpdate
	s.options.OldKeyColumns = []int{0}
	s.options.AssignmentColumns = []model.ColumnID{2}
	calls := int64(0)
	s.options.Assignment = program(t, s.options.InputLayouts, []int{1}, func(ctx context.Context, values []model.Value, _ builtin.CallBudget) (model.Value, error) {
		calls++
		return model.Int64Val(values[0].IntVal + calls), ctx.Err()
	})
	cursor := s.cursor(t, row(1, 20), row(2, 30), row(1, 999), row(2, 999))
	rows := returned(t, cursor)
	if len(rows) != 2 {
		t.Fatalf("RETURNING rows = %d, want 2", len(rows))
	}
	requireImage(t, rows[0][0], 1, 10)
	requireImage(t, rows[0][1], 1, 21)
	requireImage(t, rows[1][0], 2, 11)
	requireImage(t, rows[1][1], 2, 32)
	s.require(t, 1, 21)
	s.require(t, 2, 32)
	if calls != 2 {
		t.Fatalf("volatile effects = %d, want 2", calls)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	requireClean(t, s)
}

func TestDeleteDeduplicatesOldRows(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20))
	s.options.Operation = storage.MutationDelete
	s.options.OldKeyColumns = []int{0}
	s.options.Assignment, s.options.AssignmentColumns = nil, nil
	cursor := s.cursor(t, row(2, 20), row(1, 10), row(1, 10))
	rows := returned(t, cursor)
	if len(rows) != 2 {
		t.Fatalf("RETURNING rows = %d, want 2", len(rows))
	}
	for i, result := range rows {
		requireImage(t, result[0], int64(i+1), int64(i+1)*10)
		if !result[1].IsNull || result[1].Type != model.TypeComposite {
			t.Fatalf("DELETE NEW = %+v", result[1])
		}
	}
	s.absent(t, 1)
	s.absent(t, 2)
}

func TestPrimaryKeyMovePreservesUnassignedColumns(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10))
	s.options.Operation = storage.MutationUpdate
	s.options.OldKeyColumns = []int{0}
	s.options.AssignmentColumns = []model.ColumnID{1}
	s.options.Assignment = program(t, s.options.InputLayouts, []int{1}, nil)
	rows := returned(t, s.cursor(t, row(1, 3)))
	if len(rows) != 1 {
		t.Fatalf("RETURNING rows = %d", len(rows))
	}
	requireImage(t, rows[0][0], 1, 10)
	requireImage(t, rows[0][1], 3, 10)
	s.absent(t, 1)
	s.require(t, 3, 10)
}

func TestPrimaryKeyCollisionRestoresEarlierMoves(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20), row(4, 40))
	s.options.Operation = storage.MutationUpdate
	s.options.OldKeyColumns = []int{0}
	s.options.AssignmentColumns = []model.ColumnID{1}
	s.options.Assignment = program(t, s.options.InputLayouts, []int{1}, nil)
	cursor := s.cursor(t, row(1, 3), row(2, 4))
	var batch columnar.Batch
	err := cursor.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "23505" {
		t.Fatalf("collision = %v, want 23505", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("published RETURNING prefix from failed statement")
	}
	for _, id := range []int64{1, 2, 4} {
		s.require(t, id, id*10)
	}
	s.absent(t, 3)
	requireClean(t, s)
}

func TestLaterAssignmentFailureRollsBackOnlyThisStatement(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(9, 90))
	failure := errors.New("assignment failed")
	s.options.Assignment = program(t, s.options.InputLayouts, []int{0, 1}, func(ctx context.Context, values []model.Value, _ builtin.CallBudget) (model.Value, error) {
		if values[0].IntVal == 0 {
			return model.Value{}, failure
		}
		return values[0], ctx.Err()
	})
	cursor := s.cursor(t, row(1, 10), row(2, 0))
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); !errors.Is(err, failure) {
		t.Fatalf("assignment error = %v", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("published failed statement rows")
	}
	s.absent(t, 1)
	s.absent(t, 2)
	s.require(t, 9, 90)
	if s.tx.State() != txn.Active {
		t.Fatalf("statement failure ended transaction: %v", s.tx.State())
	}
	requireClean(t, s)
}

func TestReturningFailureRollsBackAllWrites(t *testing.T) {
	s := newStore(t)
	failure := errors.New("returning failed")
	s.options.Returning = program(t, s.options.ReturningInputLayouts, []int{0, 1}, func(ctx context.Context, values []model.Value, _ builtin.CallBudget) (model.Value, error) {
		if values[0].ArrayVal[0].IntVal == 2 {
			return model.Value{}, failure
		}
		return values[0], ctx.Err()
	})
	cursor := s.cursor(t, row(1, 10), row(2, 20))
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); !errors.Is(err, failure) {
		t.Fatalf("RETURNING error = %v", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("published failed RETURNING prefix")
	}
	s.absent(t, 1)
	s.absent(t, 2)
	requireClean(t, s)
}

func TestCancellationDuringLaterAssignmentRollsBack(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.options.Assignment = program(t, s.options.InputLayouts, []int{0, 1}, func(callCtx context.Context, values []model.Value, _ builtin.CallBudget) (model.Value, error) {
		if values[0].IntVal == 20 {
			cancel()
		}
		return values[0], callCtx.Err()
	})
	cursor := s.cursor(t, row(1, 10), row(2, 20))
	var batch columnar.Batch
	if err := cursor.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled statement = %v", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("published canceled statement rows")
	}
	s.absent(t, 1)
	s.absent(t, 2)
	requireClean(t, s)
}

func TestCloseBeforeDemandDoesNotReadOrWrite(t *testing.T) {
	s := newStore(t)
	input := newSource(t, s.options.InputLayouts, [][]model.Value{row(1, 10)})
	cursor, err := modify.New(t.Context(), input, s.options)
	if err != nil {
		t.Fatal(err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if !input.closed || input.pulls != 0 {
		t.Fatalf("input closed=%v pulls=%d", input.closed, input.pulls)
	}
	s.absent(t, 1)
	requireClean(t, s)
}

func TestNoReturningStillStagesOnFirstDemand(t *testing.T) {
	s := newStore(t)
	s.options.Returning, s.options.ReturningInputLayouts = nil, nil
	s.options.ReturningTableOIDs = 0
	cursor := s.cursor(t, row(1, 10), row(2, 20))
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("write completion = %v", err)
	}
	s.require(t, 1, 10)
	s.require(t, 2, 20)
}

func TestSpoolLimitFailureRestoresTransaction(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(9, 90))
	s.options.MaxSpoolBytes = 160
	cursor := s.cursor(t, row(1, 10), row(2, 20), row(3, 30))
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); err == nil || err == io.EOF {
		t.Fatalf("undersized input spool = %v", err)
	}
	s.require(t, 9, 90)
	for _, id := range []int64{1, 2, 3} {
		s.absent(t, id)
	}
	requireClean(t, s)
}

func TestUnsupportedDescriptorClosesInputBeforeReading(t *testing.T) {
	features := []struct {
		name   string
		change func(*model.TableMetadata)
	}{
		{"foreign key", func(table *model.TableMetadata) {
			table.Constraints = []model.ConstraintDescriptor{{ID: 3, Name: "parent", Kind: model.ConstraintForeignKey, Columns: []model.ColumnID{2}, ReferencedObject: 4, ReferencedColumns: []model.ColumnID{1}, ReferencedConstraint: 5, ReferencedIndex: 6, ParentForeignEqualityOperators: []model.OperatorID{97}, ParentEqualityOperators: []model.OperatorID{97}, ForeignEqualityOperators: []model.OperatorID{97}, Enforced: true, OnDelete: "no action", OnUpdate: "no action", Match: "simple"}}
		}},
		{"secondary index", func(table *model.TableMetadata) { table.Indexes = []model.ObjectID{3} }},
		{"trigger", func(table *model.TableMetadata) {
			table.Triggers = []model.TriggerDescriptor{{ID: 3, OID: 30003, Name: "audit", SchemaName: "public", Function: 40004, Routine: 4, Timing: "after", Events: []string{"insert"}, EnableMode: model.TriggerEnabledOrigin}}
		}},
	}
	for _, feature := range features {
		t.Run(feature.name, func(t *testing.T) {
			s := newStore(t)
			spec := descriptorSpec()
			feature.change(spec.Table)
			descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
			if err != nil {
				t.Fatal(err)
			}
			s.options.Descriptor = descriptor
			input := newSource(t, s.options.InputLayouts, [][]model.Value{row(1, 10)})
			cursor, err := modify.New(t.Context(), input, s.options)
			if !errors.Is(err, modify.ErrUnsupported) || cursor != nil {
				t.Fatalf("preflight = %v, %v", cursor, err)
			}
			if !input.closed || input.pulls != 0 {
				t.Fatalf("input closed=%v pulls=%d", input.closed, input.pulls)
			}
			s.absent(t, 1)
			requireClean(t, s)
		})
	}
}

func TestReturningReadsCompletedDefaultImage(t *testing.T) {
	value := model.Int64Val(77)
	s := newStoreWithDefault(t, &value)
	s.options.AssignmentColumns = []model.ColumnID{1}
	s.options.Assignment = program(t, s.options.InputLayouts, []int{0}, nil)
	rows := returned(t, s.cursor(t, row(1, 0)))
	if len(rows) != 1 {
		t.Fatalf("RETURNING rows = %d", len(rows))
	}
	requireImage(t, rows[0][1], 1, 77)
	s.require(t, 1, 77)
}

func TestConstructorOwnsMutableColumnMappings(t *testing.T) {
	s := newStore(t)
	cursor := s.cursor(t, row(1, 10))
	s.options.AssignmentColumns[0] = 2
	s.options.InputLayouts[0] = record
	rows := returned(t, cursor)
	if len(rows) != 1 {
		t.Fatalf("RETURNING rows = %d", len(rows))
	}
	requireImage(t, rows[0][1], 1, 10)
	s.require(t, 1, 10)
}

func TestAffectedRowsReportsOnlySuccessfulDeduplicatedTargets(t *testing.T) {
	s := newStore(t)
	s.seed(t, row(1, 10), row(2, 20))
	s.options.Operation = storage.MutationDelete
	s.options.OldKeyColumns = []int{0}
	s.options.Assignment, s.options.AssignmentColumns = nil, nil
	s.options.Returning, s.options.ReturningInputLayouts = nil, nil
	s.options.ReturningTableOIDs = 0
	cursor := s.cursor(t, row(1, 10), row(2, 20), row(1, 10))
	if cursor.RowsAffected() != 0 {
		t.Fatal("reported rows before staging")
	}
	var batch columnar.Batch
	if err := cursor.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("DELETE completion = %v", err)
	}
	if cursor.RowsAffected() != 2 {
		t.Fatalf("affected rows = %d, want 2", cursor.RowsAffected())
	}
}

type insteadViewTrigger struct {
	begun, rows, finished int
}

func (trigger *insteadViewTrigger) BeginStatement(_ context.Context, _ *model.Descriptor, event model.TriggerEvent, _ []model.ColumnID) error {
	if event != model.TriggerInsert {
		return errors.New("unexpected trigger event")
	}
	trigger.begun++
	return nil
}

func (trigger *insteadViewTrigger) BeforeRow(_ context.Context, _ *model.Descriptor, event model.TriggerEvent, _, new *model.Row, _ uint64) (*model.Row, bool, error) {
	if event != model.TriggerInsert || new == nil {
		return nil, false, errors.New("unexpected trigger row")
	}
	values := make([]model.Value, len(new.Values))
	for i := range values {
		values[i] = new.Values[i].Clone()
	}
	trigger.rows++
	return &model.Row{Values: values}, true, nil
}

func (*insteadViewTrigger) AfterRow(context.Context, *model.Descriptor, model.TriggerEvent, *model.Row, *model.Row, uint64) error {
	return nil
}

func (trigger *insteadViewTrigger) FinishStatement(_ context.Context, _ *model.Descriptor, event model.TriggerEvent) error {
	if event != model.TriggerInsert {
		return errors.New("unexpected trigger event")
	}
	trigger.finished++
	return nil
}

func (*insteadViewTrigger) AbortStatement(*model.Descriptor, model.TriggerEvent) {}

func TestLogicalViewInsertExecutesInsteadTriggerWithoutPhysicalWrite(t *testing.T) {
	s := newStore(t)
	table := descriptorSpec().Table
	// A view's Definition holds the deparsed SQL and its Query holds the
	// compiled body encoding; Query.SQL stays empty. Production builds both in
	// bind/object_declaration.go, and model.validateViewMetadata requires them.
	raw, err := zpgparse.Parse("SELECT 1::bigint, 10::bigint")
	if err != nil {
		t.Fatal(err)
	}
	encoding, err := (proto.MarshalOptions{Deterministic: true}).Marshal(raw.Stmts[0])
	if err != nil {
		t.Fatal(err)
	}
	spec := model.DescriptorSpec{
		ID: 1, OID: 10001, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 2, Kind: model.ObjectView, Name: "logical_view"},
		View: &model.ViewMetadata{
			Owner:      1,
			Columns:    table.Columns,
			Definition: "SELECT 1::bigint, 10::bigint",
			Query:      model.ExpressionDefinition{SemanticVersion: 1, Encoding: encoding},
			Triggers:   []model.TriggerDescriptor{{ID: 3, OID: 30003, Name: "instead_insert", SchemaName: "public", Function: 40004, Routine: 4, Timing: "instead", Events: []string{"insert"}, ForEachRow: true, EnableMode: model.TriggerEnabledOrigin}},
		},
	}
	descriptor, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	trigger := &insteadViewTrigger{}
	options := s.options
	options.TargetKind = ir.LogicalViewModifyTarget
	options.Descriptor = descriptor
	options.KeyLayout = nil
	options.Expansion = nil
	options.Triggers = trigger
	// A logical view target has no physical table, so RETURNING publishes no
	// table OIDs, and it expects imageCount(2) composite images over the view's
	// own columns rather than the physical fixture's two images plus two OIDs.
	viewLayouts := make([]model.TypeLayout, len(table.Columns))
	for i, column := range table.Columns {
		viewLayouts[i] = column.Layout
	}
	viewImage := model.TypeLayout{Type: record.Type, Children: viewLayouts}
	viewReturning := []model.TypeLayout{viewImage, viewImage}
	options.ReturningTableOIDs = 0
	options.ReturningInputLayouts = viewReturning
	options.Returning = program(t, viewReturning, []int{0}, nil)
	input := newSource(t, options.InputLayouts, [][]model.Value{row(1, 10)})
	cursor, err := modify.New(t.Context(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := cursor.Complete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cursor.RowsAffected() != 1 || trigger.begun != 1 || trigger.rows != 1 || trigger.finished != 1 {
		t.Fatalf("affected=%d trigger=%+v", cursor.RowsAffected(), trigger)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	s.absent(t, 1)
	requireClean(t, s)
}

type checkOrderingTrigger struct {
	queued   int
	finished int
	aborted  int
}

func (*checkOrderingTrigger) BeginStatement(context.Context, *model.Descriptor, model.TriggerEvent, []model.ColumnID) error {
	return nil
}

func (*checkOrderingTrigger) BeforeRow(_ context.Context, _ *model.Descriptor, _ model.TriggerEvent, _ *model.Row, new *model.Row, _ uint64) (*model.Row, bool, error) {
	values := make([]model.Value, len(new.Values))
	for i := range values {
		values[i] = new.Values[i].Clone()
	}
	return &model.Row{Values: values}, true, nil
}

func (trigger *checkOrderingTrigger) AfterRow(context.Context, *model.Descriptor, model.TriggerEvent, *model.Row, *model.Row, uint64) error {
	trigger.queued++
	return nil
}

func (trigger *checkOrderingTrigger) FinishStatement(context.Context, *model.Descriptor, model.TriggerEvent) error {
	trigger.finished++
	return nil
}

func (trigger *checkOrderingTrigger) AbortStatement(*model.Descriptor, model.TriggerEvent) {
	trigger.aborted++
}

func TestViewCheckOptionRejectsBeforeAfterTriggerCallback(t *testing.T) {
	s := newStore(t)
	trigger := &checkOrderingTrigger{}
	options := s.options
	options.TargetKind = ir.AutomaticViewModifyTarget
	options.Triggers = trigger
	options.ViewMutationSteps = []modify.ViewMutationStep{{
		View:          10,
		InputColumns:  []model.ColumnID{1, 2},
		OutputColumns: []model.ColumnID{1, 2},
		Projection:    program(t, options.InputLayouts, []int{0, 1}, nil),
		Predicate:     booleanProgram(t, options.InputLayouts, false),
		Check:         true,
	}}
	input := newSource(t, options.InputLayouts, [][]model.Value{row(1, 10)})
	cursor, err := modify.New(t.Context(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	err = cursor.Complete(t.Context())
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "44000" {
		t.Fatalf("view check error = %v, want SQLSTATE 44000", err)
	}
	if trigger.queued != 1 || trigger.finished != 0 || trigger.aborted != 1 {
		t.Fatalf("trigger lifecycle = %+v, want queued event aborted before callback", trigger)
	}
	if closeErr := cursor.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	s.absent(t, 1)
	requireClean(t, s)
}
