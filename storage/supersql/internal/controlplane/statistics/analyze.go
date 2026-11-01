package statistics

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ObservedCounters must describe the same target and row snapshot. A nil input
// leaves storage-only counters unknown; SnapshotReader cannot measure them.
type ObservedCounters struct {
	Target                                              storage.ObjectRef
	Snapshot                                            model.Timestamp
	DeadRows, HistoricalRows, Blocks, Files, DeltaBytes Count
	VersionChainWork                                    float64
	VersionChainWorkKnown                               bool
}

type ColumnTarget struct {
	ID     model.ColumnID
	Target int
}

// IndexColumn.ID is the one-based index key ordinal. Layout is the frozen
// descriptor layout, including its resolved collation.
type IndexColumn struct {
	ID        model.ColumnID
	Expression ValueExpression
	Layout    model.TypeLayout
	Target    int
	TargetSet bool
}

// IndexDefinition evaluates immutable expressions against the base table
// reservoir. A nil Predicate includes every base row.
type IndexDefinition struct {
	Target    storage.ObjectRef
	Epoch     uint64
	Columns   []IndexColumn
	Predicate ValueExpression
}

// AnalyzeResult.Indexes is ordered by Target.ObjectID.
type AnalyzeResult struct {
	Table   *Table
	Indexes []*Table
}

type AnalyzeOptions struct {
	Target                                           storage.ObjectRef
	Snapshot                                         model.Timestamp
	Epoch, Seed                                      uint64
	Layout                                           []storage.ColumnLayout
	KeyLayout                                        []storage.KeyColumn
	SampleRows, BatchRows, MostCommon, HistogramBins int
	MaxScanRows, MaxBatches                          uint64
	// MaxLiveBytes includes owned setup, reservoir, scratch and output. Borrowed
	// backend plan/batch buffers remain governed by the backend's own limits.
	MaxLiveBytes, MaxReservoirBytes, MaxKeyBytes uint64
	Limits                                       Limits
	Observed                                     *ObservedCounters
	ColumnTargets                                []ColumnTarget
	Extended                                     []ExtendedDefinition
	Indexes                                      []IndexDefinition
}

type reservoir struct {
	values        []model.Value
	ordinals      []uint64
	sizes         []uint64
	rows, columns int
	payload       uint64
}

type sampleEntry struct {
	index int
	rank  float64
	key   []byte
}
type sampleGroup struct{ start, count int }

// Analyze borrows an exact, immutable, full-table snapshot reader. The reader
// stays open; only the newly opened cursor belongs to Analyze. Seed reproducibility
// additionally requires stable forward key traversal and the same sampler version.
func Analyze(ctx context.Context, reader storage.SnapshotReader, encoder storage.KeyEncoder, options AnalyzeOptions) (*Table, error) {
	if len(options.Indexes) != 0 {
		return nil, ErrInvalid
	}
	result, err := AnalyzeBundle(ctx, reader, encoder, options)
	if err != nil {
		return nil, err
	}
	return result.Table, nil
}

// AnalyzeBundle reuses the table reservoir for every declared expression index.
func AnalyzeBundle(ctx context.Context, reader storage.SnapshotReader, encoder storage.KeyEncoder, o AnalyzeOptions) (result *AnalyzeResult, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := o.Limits.Validate(); err != nil {
		return nil, err
	}
	if reader == nil || encoder == nil || o.Snapshot <= 0 || o.Epoch == 0 {
		return nil, ErrLimit
	}
	invalidTargets := o.MostCommon < 0 || o.HistogramBins < 0 || o.MostCommon > o.SampleRows || o.HistogramBins > o.SampleRows
	if o.SampleRows <= 0 || o.BatchRows <= 0 || invalidTargets {
		return nil, ErrLimit
	}
	invalidWork := o.MaxScanRows == 0 || o.MaxBatches == 0
	invalidMemory := o.MaxLiveBytes == 0 || o.MaxLiveBytes > math.MaxInt || o.MaxReservoirBytes == 0
	invalidKeyBudget := o.MaxKeyBytes == 0 || o.MaxKeyBytes > o.MaxLiveBytes
	invalidLayout := len(o.Layout) == 0 || len(o.Layout) > o.Limits.MaxColumns || len(o.KeyLayout) > len(o.Layout)
	if invalidWork || invalidMemory || invalidKeyBudget || invalidLayout {
		return nil, ErrLimit
	}
	if err := o.Target.Validate(); err != nil {
		return nil, err
	}
	for i, target := range o.ColumnTargets {
		if target.ID == 0 || target.Target < 0 || target.Target > 10000 || i > 0 && o.ColumnTargets[i-1].ID >= target.ID {
			return nil, ErrInvalid
		}
		if !slices.ContainsFunc(o.Layout, func(layout storage.ColumnLayout) bool {
			return layout.ID == target.ID
		}) {
			return nil, ErrInvalid
		}
	}
	if err := validateExtendedDefinitions(o); err != nil {
		return nil, err
	}
	if o.Observed != nil && (o.Observed.Target != o.Target || o.Observed.Snapshot != o.Snapshot) {
		return nil, ErrStale
	}
	if x := o.Observed; x != nil {
		if !finiteNonnegative(x.VersionChainWork) || (!x.VersionChainWorkKnown && x.VersionChainWork != 0) {
			return nil, ErrInvalid
		}
		for _, count := range [...]Count{x.DeadRows, x.HistoricalRows, x.Blocks, x.Files, x.DeltaBytes} {
			if !count.Known && count.Value != 0 {
				return nil, ErrInvalid
			}
		}
	}
	var backendObservation *storage.SnapshotObservation
	if o.Observed == nil {
		if observer, ok := reader.(storage.SnapshotObserver); ok {
			observation, err := observer.ObserveSnapshot(ctx, o.Target)
			if err != nil {
				return nil, err
			}
			if observation.Target != o.Target || observation.Snapshot != o.Snapshot {
				return nil, ErrStale
			}
			backendObservation = &observation
		}
	}
	if err := validateIndexDefinitions(o); err != nil {
		return nil, err
	}
	outputBudget := meter{ctx: ctx, limit: o.Limits.MaxBytes, maxValues: o.Limits.MaxValues, maxDepth: o.Limits.MaxDepth}
	if err := outputBudget.add(1, uint64(unsafe.Sizeof(AnalyzeResult{}))); err != nil {
		return nil, err
	}
	if err := outputBudget.add(uint64(1+len(o.Indexes)), uint64(unsafe.Sizeof(Table{}))); err != nil {
		return nil, err
	}
	if err := outputBudget.add(uint64(len(o.Layout)), uint64(unsafe.Sizeof(Column{}))); err != nil {
		return nil, err
	}
	budget := meter{ctx: ctx, limit: o.MaxLiveBytes, maxValues: o.Limits.MaxValues, maxDepth: o.Limits.MaxDepth}
	// Reserve the complete output allowance before any sample allocation.
	if err := budget.add(o.Limits.MaxBytes, 1); err != nil {
		return nil, err
	}
	if err := budget.add(uint64(len(o.Layout)), uint64(unsafe.Sizeof(model.ColumnID(0)))); err != nil {
		return nil, err
	}
	ids := make([]model.ColumnID, len(o.Layout))
	for i, c := range o.Layout {
		if c.ID == 0 || (i > 0 && o.Layout[i-1].ID >= c.ID) {
			return nil, ErrInvalid
		}
		if err := budget.layout(model.TypeLayout{Type: c.Type, Children: c.Children}, 0); err != nil {
			return nil, err
		}
		ids[i] = c.ID
	}
	request, setupBytes, err := storage.CopyPushdownRequest(ctx, storage.PushdownRequest{Target: o.Target, Layout: o.Layout, KeyLayout: o.KeyLayout, ColumnIDs: ids, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}, budget.limit-budget.used)
	if err != nil {
		return nil, err
	}
	if err := budget.add(setupBytes, 1); err != nil {
		return nil, err
	}
	if err := (storage.ObjectLayout{Target: o.Target, Columns: request.Layout}).Validate(ctx); err != nil {
		return nil, err
	}
	if uint64(o.SampleRows) > uint64(math.MaxInt)/uint64(len(ids)) {
		return nil, ErrLimit
	}
	// A reservoir row costs len(ids)*sizeof(model.Value) fixed bytes plus 16
	// bytes of ordinals/sizes bookkeeping, and an unbounded variable payload.
	// Cap the fixed cost at half of MaxReservoirBytes so a wide table samples
	// fewer rows instead of failing, while the other half stays available for
	// variable-length values. The clamp never grows the requested sample.
	fixedPerRow := uint64(len(ids))*uint64(unsafe.Sizeof(model.Value{})) + 16
	if maxRows := o.MaxReservoirBytes / 2 / fixedPerRow; uint64(o.SampleRows) > maxRows {
		o.SampleRows = int(maxRows)
	}
	if o.SampleRows <= 0 {
		return nil, ErrLimit
	}
	cells := o.SampleRows * len(ids)
	for _, a := range [...]struct{ n, size uint64 }{
		{uint64(cells), uint64(unsafe.Sizeof(model.Value{}))},
		{uint64(o.SampleRows), 16 + uint64(unsafe.Sizeof(sampleEntry{})) + uint64(unsafe.Sizeof(sampleGroup{})) + 8},
		{uint64(len(ids)), uint64(unsafe.Sizeof(columnar.Vector{}))},
	} {
		if err := budget.add(a.n, a.size); err != nil {
			return nil, err
		}
	}
	fixedReservoir := uint64(cells)*uint64(unsafe.Sizeof(model.Value{})) + uint64(o.SampleRows)*16
	if fixedReservoir > o.MaxReservoirBytes {
		return nil, ErrLimit
	}
	r := reservoir{values: make([]model.Value, cells), ordinals: make([]uint64, o.SampleRows), sizes: make([]uint64, o.SampleRows), columns: len(ids)}
	indexLive := make([]uint64, len(o.Indexes))
	if err := budget.add(uint64(len(o.Indexes)), uint64(unsafe.Sizeof(uint64(0)))); err != nil {
		return nil, err
	}
	var predicateRow []model.Value
	if slices.ContainsFunc(o.Indexes, func(index IndexDefinition) bool { return index.Predicate != nil }) {
		if err := budget.add(uint64(len(ids)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return nil, err
		}
		predicateRow = make([]model.Value, len(ids))
	}
	plan, err := reader.PlanScan(ctx, request)
	if plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, reader.ReleasePlan(plan)) }()
	}
	if err != nil {
		return nil, err
	}
	if plan.SnapshotTS != o.Snapshot || plan.Request.Target != o.Target {
		return nil, ErrStale
	}
	filtered := !plan.Accepted.Empty() || !plan.Residual.Empty() || !plan.Request.Predicate.Empty()
	capped := plan.AppliedLimit.Enabled || plan.Request.SafeLimit.Enabled
	fullSpan := len(plan.Request.Spans) == 1 && plan.Request.Spans[0].Lower.Unbounded && plan.Request.Spans[0].Upper.Unbounded
	changedTraversal := plan.Request.Direction != storage.ScanForward || plan.Request.IndexID != 0 || !slices.Equal(plan.Projection, ids)
	if filtered || capped || !fullSpan || changedTraversal {
		return nil, ErrInvalid
	}
	cursor, err := reader.VectorizedScan(ctx, plan)
	if err != nil {
		if cursor != nil {
			err = errors.Join(err, cursor.Close())
		}
		return nil, err
	}
	if cursor == nil {
		return nil, ErrInvalid
	}
	defer func() {
		err = errors.Join(err, cursor.Close())
		if err != nil {
			result = nil
		}
	}()
	batch := columnar.Batch{MaxRows: o.BatchRows}
	random := rand.New(rand.NewPCG(o.Seed, uint64(o.Snapshot)))
	var live uint64
	for batches := uint64(0); ; batches++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if batches >= o.MaxBatches {
			return nil, ErrLimit
		}
		batch.Reset()
		err := cursor.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		// A bounded selection may borrow a larger physical block without copying it.
		if batch.Rows() > o.BatchRows {
			return nil, columnar.ErrInvalidBatch
		}
		if len(batch.Columns) != len(ids) {
			return nil, ErrInvalid
		}
		if err := batch.Validate(max(batch.Length, o.BatchRows)); err != nil {
			return nil, err
		}
		for i := range batch.Columns {
			if batch.Columns[i].Type != request.Layout[i].Type {
				return nil, ErrInvalid
			}
		}
		for row := range batch.Rows() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if live >= o.MaxScanRows {
				return nil, ErrLimit
			}
			live++
			if err := observeIndexPredicates(ctx, &batch, batch.Lane(row), predicateRow, o.Indexes, indexLive, &budget); err != nil {
				return nil, err
			}
			slot := live - 1
			if slot >= uint64(o.SampleRows) {
				slot = random.Uint64N(live)
			}
			if slot >= uint64(o.SampleRows) {
				continue
			}
			start := int(slot) * r.columns
			clear(r.values[start : start+r.columns])
			r.payload -= r.sizes[slot]
			budget.used -= r.sizes[slot]
			r.sizes[slot] = 0
			for col := range ids {
				remaining := min(budget.limit-budget.used, o.MaxReservoirBytes-fixedReservoir-r.payload)
				v, size, err := columnar.CopyValue(ctx, &batch.Columns[col], batch.Lane(row), remaining)
				if err != nil {
					return nil, err
				}
				if err := budget.add(size, 1); err != nil {
					return nil, err
				}
				r.payload += size
				r.sizes[slot] += size
				r.values[start+col] = v
			}
			r.ordinals[slot] = live
			r.rows = int(min(live, uint64(o.SampleRows)))
		}
	}
	for i, index := range o.Indexes {
		if index.Predicate == nil {
			indexLive[i] = live
		}
	}
	batch.Reset()
	table := &Table{Target: o.Target, Epoch: o.Epoch, Snapshot: o.Snapshot, Seed: o.Seed, SampleRows: uint64(r.rows), LiveRows: Count{Value: live, Known: true}, Columns: make([]Column, len(ids))}
	if live == 0 {
		table.Confidence = 1
	} else {
		table.Confidence = float64(r.rows) / float64(live)
	}
	if o.Observed != nil {
		x := o.Observed
		table.DeadRows = x.DeadRows
		table.HistoricalRows = x.HistoricalRows
		table.Blocks = x.Blocks
		table.Files = x.Files
		table.DeltaBytes = x.DeltaBytes
		table.VersionChainWork = x.VersionChainWork
		table.VersionChainWorkKnown = x.VersionChainWorkKnown
	}
	if backendObservation != nil {
		table.Blocks = Count{Value: backendObservation.Blocks.Value, Known: backendObservation.Blocks.Known}
		table.Files = Count{Value: backendObservation.Files.Value, Known: backendObservation.Files.Known}
		table.DeltaBytes = Count{Value: backendObservation.DeltaBytes.Value, Known: backendObservation.DeltaBytes.Known}
		if backendObservation.VersionRows.Known {
			if backendObservation.VersionRows.Value < live {
				return nil, ErrInvalid
			}
			table.HistoricalRows = Count{Value: backendObservation.VersionRows.Value - live, Known: true}
			if live != 0 {
				table.VersionChainWork = float64(backendObservation.VersionRows.Value) / float64(live)
				table.VersionChainWorkKnown = true
			} else if backendObservation.VersionRows.Value == 0 {
				table.VersionChainWorkKnown = true
			}
		}
	}
	order := make([]int, r.rows)
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Compare(r.ordinals[a], r.ordinals[b]) })
	entries := make([]sampleEntry, r.rows)
	groups := make([]sampleGroup, r.rows)
	for i := range table.Columns {
		column := &table.Columns[i]
		column.ID = ids[i]
		layout, used, err := model.CopyTypeLayout(ctx, model.TypeLayout{Type: request.Layout[i].Type, Children: request.Layout[i].Children}, outputBudget.limit-outputBudget.used)
		if err != nil {
			return nil, err
		}
		if err := outputBudget.add(used, 1); err != nil {
			return nil, err
		}
		column.Layout = layout
		mostCommon, histogramBins := columnTargets(o, column.ID)
		if err := summarize(ctx, encoder, &r, i, order, entries, groups, column, live, mostCommon, histogramBins, o, &budget, &outputBudget); err != nil {
			return nil, err
		}
		table.AverageRowBytes += column.AverageBytes
	}
	table.Extended, err = summarizeExtended(ctx, encoder, &r, order, live, o, &budget, &outputBudget)
	if err != nil {
		return nil, err
	}
	if err := Validate(ctx, table, o.Limits); err != nil {
		return nil, err
	}
	indexes, err := summarizeIndexes(ctx, encoder, &r, indexLive, o, &budget, &outputBudget)
	if err != nil {
		return nil, err
	}
	return &AnalyzeResult{Table: table, Indexes: indexes}, ctx.Err()
}


func validateIndexDefinitions(options AnalyzeOptions) error {
	if len(options.Indexes) > options.Limits.MaxValues {
		return ErrLimit
	}
	for i, index := range options.Indexes {
		if err := index.Target.Validate(); err != nil {
			return err
		}
		if index.Target == options.Target || index.Epoch == 0 || len(index.Columns) == 0 || len(index.Columns) > options.Limits.MaxColumns || i > 0 && options.Indexes[i-1].Target.ObjectID >= index.Target.ObjectID {
			return ErrInvalid
		}
		for position, column := range index.Columns {
			if column.ID == 0 || position > 0 && index.Columns[position-1].ID >= column.ID || column.Expression == nil || column.Layout.Validate() != nil || column.Target < 0 || column.Target > 10000 || !column.TargetSet && column.Target != 0 {
				return ErrInvalid
			}
		}
	}
	return nil
}

func observeIndexPredicates(ctx context.Context, batch *columnar.Batch, row int, scratch []model.Value, indexes []IndexDefinition, counts []uint64, budget *meter) error {
	if len(scratch) == 0 {
		return ctx.Err()
	}
	var payload uint64
	defer func() {
		clear(scratch)
		budget.used -= payload
	}()
	for column := range scratch {
		value, size, err := columnar.CopyValue(ctx, &batch.Columns[column], row, budget.limit-budget.used)
		if err != nil {
			return err
		}
		if err := budget.add(size, 1); err != nil {
			return err
		}
		payload += size
		scratch[column] = value
	}
	for i, index := range indexes {
		if index.Predicate == nil {
			continue
		}
		matches, err := indexPredicateMatches(ctx, index.Predicate, scratch)
		if err != nil {
			return err
		}
		if matches {
			if counts[i] == math.MaxUint64 {
				return ErrLimit
			}
			counts[i]++
		}
	}
	return ctx.Err()
}

func indexPredicateMatches(ctx context.Context, predicate ValueExpression, row []model.Value) (bool, error) {
	value, err := predicate.Evaluate(ctx, row, model.ExpressionRuntime{})
	if err != nil {
		return false, err
	}
	if value.Type != model.TypeBoolean || !value.IsNull && value.IntVal != 0 && value.IntVal != 1 {
		return false, ErrInvalid
	}
	return !value.IsNull && value.IntVal == 1, ctx.Err()
}

func summarizeIndexes(ctx context.Context, encoder storage.KeyEncoder, base *reservoir, live []uint64, options AnalyzeOptions, working, output *meter) ([]*Table, error) {
	if err := output.add(uint64(len(options.Indexes)), uint64(unsafe.Sizeof((*Table)(nil)))); err != nil {
		return nil, err
	}
	result := make([]*Table, len(options.Indexes))
	for i, definition := range options.Indexes {
		index, err := summarizeIndex(ctx, encoder, base, live[i], definition, options, working, output)
		if err != nil {
			return nil, err
		}
		result[i] = index
	}
	return result, ctx.Err()
}

func summarizeIndex(ctx context.Context, encoder storage.KeyEncoder, base *reservoir, live uint64, definition IndexDefinition, options AnalyzeOptions, working, output *meter) (*Table, error) {
	fixed := uint64(base.rows) * uint64(unsafe.Sizeof(int(0)))
	if err := working.add(uint64(base.rows), uint64(unsafe.Sizeof(int(0)))); err != nil {
		return nil, err
	}
	defer func() { working.used -= fixed }()
	rows := make([]int, 0, base.rows)
	for row := range base.rows {
		if definition.Predicate != nil {
			matches, err := indexPredicateMatches(ctx, definition.Predicate, base.values[row*base.columns:(row+1)*base.columns])
			if err != nil {
				return nil, err
			}
			if !matches {
				continue
			}
		}
		rows = append(rows, row)
	}
	if len(rows) != 0 && len(definition.Columns) > math.MaxInt/len(rows) {
		return nil, ErrLimit
	}
	cells := len(rows) * len(definition.Columns)
	temporary := uint64(cells)*uint64(unsafe.Sizeof(model.Value{})) +
		uint64(len(rows))*uint64(unsafe.Sizeof(uint64(0))) +
		uint64(len(rows))*(uint64(unsafe.Sizeof(int(0)))+uint64(unsafe.Sizeof(sampleEntry{}))+uint64(unsafe.Sizeof(sampleGroup{})))
	if err := working.add(temporary, 1); err != nil {
		return nil, err
	}
	var payload uint64
	defer func() { working.used -= temporary + payload }()
	sample := reservoir{values: make([]model.Value, cells), ordinals: make([]uint64, len(rows)), rows: len(rows), columns: len(definition.Columns)}
	for sampled, row := range rows {
		source := base.values[row*base.columns : (row+1)*base.columns]
		sample.ordinals[sampled] = base.ordinals[row]
		for column, projected := range definition.Columns {
			value, err := projected.Expression.Evaluate(ctx, source, model.ExpressionRuntime{})
			if err != nil {
				return nil, err
			}
			if err := storage.ValidateStoredValue(ctx, value, projected.Layout); err != nil {
				return nil, err
			}
			owned, size, err := model.CopyValue(ctx, value, working.limit-working.used)
			if err != nil {
				return nil, err
			}
			if err := working.add(size, 1); err != nil {
				return nil, err
			}
			payload += size
			sample.values[sampled*sample.columns+column] = owned
		}
	}
	if err := output.add(uint64(len(definition.Columns)), uint64(unsafe.Sizeof(Column{}))); err != nil {
		return nil, err
	}
	table := &Table{Target: definition.Target, Epoch: definition.Epoch, Snapshot: options.Snapshot, Seed: options.Seed, SampleRows: uint64(len(rows)), LiveRows: Count{Value: live, Known: true}, Columns: make([]Column, len(definition.Columns))}
	if live == 0 {
		table.Confidence = 1
	} else {
		table.Confidence = min(1, float64(len(rows))/float64(live))
	}
	order := make([]int, len(rows))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(left, right int) int { return cmp.Compare(sample.ordinals[left], sample.ordinals[right]) })
	entries := make([]sampleEntry, len(rows))
	groups := make([]sampleGroup, len(rows))
	for i, projected := range definition.Columns {
		column := &table.Columns[i]
		column.ID = projected.ID
		layout, size, err := model.CopyTypeLayout(ctx, projected.Layout, output.limit-output.used)
		if err != nil {
			return nil, err
		}
		if err := output.add(size, 1); err != nil {
			return nil, err
		}
		column.Layout = layout
		mostCommon, histogramBins := options.MostCommon, options.HistogramBins
		if projected.TargetSet {
			target := min(projected.Target, options.SampleRows, options.Limits.MaxValues)
			mostCommon, histogramBins = target, target
		}
		if err := summarize(ctx, encoder, &sample, i, order, entries, groups, column, live, mostCommon, histogramBins, options, working, output); err != nil {
			return nil, err
		}
		table.AverageRowBytes += column.AverageBytes
	}
	if err := Validate(ctx, table, options.Limits); err != nil {
		return nil, err
	}
	return table, ctx.Err()
}
func summarize(ctx context.Context, encoder storage.KeyEncoder, r *reservoir, column int, order []int, scratch []sampleEntry, groupScratch []sampleGroup, out *Column, live uint64, mostCommon, histogramBins int, o AnalyzeOptions, working, output *meter) error {
	if r.rows == 0 {
		return ctx.Err()
	}
	entries := scratch[:0]
	var nulls int
	var width, keyBytes uint64
	defer func() { clear(scratch); working.used -= keyBytes }()
	keyLayout := []storage.KeyColumn{{ColumnID: out.ID, Type: out.Layout.Type, Children: out.Layout.Children}}
	for rank, row := range order {
		v := r.values[row*r.columns+column]
		if v.IsNull {
			nulls++
			continue
		}
		key, err := encoder.EncodePrefix(ctx, keyLayout, []model.Value{v}, min(o.MaxKeyBytes, working.limit-working.used))
		if err != nil {
			return err
		}
		if err := working.add(uint64(cap(key)), 1); err != nil {
			return err
		}
		keyBytes += uint64(cap(key))
		entries = append(entries, sampleEntry{index: row*r.columns + column, rank: float64(rank), key: key})
		counter := wireWriter{ctx: ctx, limit: o.MaxReservoirBytes}
		if err := storage.EncodeValue(ctx, &counter, v, counter.limit); err != nil {
			return err
		}
		if counter.used > math.MaxUint64-width {
			return ErrLimit
		}
		width += counter.used
	}
	out.NullFraction = float64(nulls) / float64(r.rows)
	// Width uses the storage scalar wire representation, including its type header.
	out.AverageBytes = float64(width) / float64(r.rows)
	if len(entries) == 0 {
		return ctx.Err()
	}
	var sortErr error
	slices.SortFunc(entries, func(a, b sampleEntry) int {
		if sortErr == nil {
			sortErr = ctx.Err()
		}
		if sortErr != nil {
			return 0
		}
		if n := bytes.Compare(a.key, b.key); n != 0 {
			return n
		}
		return cmp.Compare(a.rank, b.rank)
	})
	if sortErr != nil {
		return sortErr
	}
	groups := groupScratch[:0]
	var singletons int
	var sx, sy, sxx, syy, sxy float64
	for start := 0; start < len(entries); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + 1
		for end < len(entries) && bytes.Equal(entries[start].key, entries[end].key) {
			end++
		}
		groups = append(groups, sampleGroup{start: start, count: end - start})
		if end-start == 1 {
			singletons++
		}
		y := float64(start+end-1) / 2
		for _, e := range entries[start:end] {
			x := e.rank
			sx += x
			sy += y
			sxx += x * x
			syy += y * y
			sxy += x * y
		}
		start = end
	}
	n := float64(len(entries))
	nonNullRows := max(n, float64(live)*(1-out.NullFraction))
	out.Distinct = min(nonNullRows, max(float64(len(groups)), float64(len(groups))/(1-float64(singletons)/n+float64(singletons)/nonNullRows)))
	denominator := (n*sxx - sx*sx) * (n*syy - sy*sy)
	if denominator > 0 {
		out.Correlation = min(1, max(-1, (n*sxy-sx*sy)/math.Sqrt(denominator)))
		out.CorrelationKnown = true
	}
	bins := min(histogramBins, len(entries))
	if err := output.add(uint64(bins), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return err
	}
	out.Histogram = make([]model.Value, bins)
	for i := range out.Histogram {
		position := 0
		if bins > 1 {
			position = int(uint64(i) * uint64(len(entries)-1) / uint64(bins-1))
		}
		v, size, err := model.CopyValue(ctx, r.values[entries[position].index], output.limit-output.used)
		if err != nil {
			return err
		}
		if err := output.add(size, 1); err != nil {
			return err
		}
		out.Histogram[i] = v
	}
	slices.SortFunc(groups, func(a, b sampleGroup) int {
		if n := cmp.Compare(b.count, a.count); n != 0 {
			return n
		}
		return cmp.Compare(a.start, b.start)
	})
	common := min(mostCommon, len(groups))
	if err := output.add(uint64(common), uint64(unsafe.Sizeof(Frequency{}))); err != nil {
		return err
	}
	out.MostCommon = make([]Frequency, common)
	for i, g := range groups[:common] {
		v, size, err := model.CopyValue(ctx, r.values[entries[g.start].index], output.limit-output.used)
		if err != nil {
			return err
		}
		if err := output.add(size, 1); err != nil {
			return err
		}
		out.MostCommon[i] = Frequency{Value: v, Fraction: float64(g.count) / float64(r.rows)}
	}
	return ctx.Err()
}

func columnTargets(options AnalyzeOptions, id model.ColumnID) (int, int) {
	index, found := slices.BinarySearchFunc(options.ColumnTargets, id, func(target ColumnTarget, id model.ColumnID) int {
		return cmp.Compare(target.ID, id)
	})
	if !found {
		return options.MostCommon, options.HistogramBins
	}
	target := min(options.ColumnTargets[index].Target, options.SampleRows, options.Limits.MaxValues)
	return target, target
}
