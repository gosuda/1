package schema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// MaxRows bounds the combined visible rows visited in both scans. MaxBytes
// bounds validation-owned metadata and per-record workspace, not reader buffers
// or expression-program internals. Every limit must be positive.
type IndexValidationLimits struct {
	BatchRows int
	MaxRows   uint64
	MaxBytes  uint64
}

type IndexValidationResult struct {
	BaseRows  uint64
	IndexRows uint64
}

var (
	errIndexValidationCorrupt = &model.Error{Code: "XX001", Message: "index validation found inconsistent entries", Cause: storage.ErrCorruptStorage}
	errIndexValidationPlan    = &model.Error{Code: "XX001", Message: "index validation scan contract is inconsistent", Cause: storage.ErrInvalidScanPlan}
	errIndexValidationLimit   = &model.Error{Code: "54000", Message: "index validation exceeds resource limits", Cause: errors.Join(ErrIndexLimit, model.ErrValueBudget)}
)

// ValidateIndex borrows one fixed snapshot; foreground writes must already
// maintain the index atomically. It neither closes the reader nor publishes the
// index. Failure, including cursor/plan cleanup failure, returns a zero result.
func (s *IndexSet) ValidateIndex(ctx context.Context, reader storage.SnapshotReader, index model.ObjectID, limits IndexValidationLimits) (result IndexValidationResult, err error) {
	defer func() {
		if err != nil {
			result = IndexValidationResult{}
			err = indexValidationFailure(err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if limits.BatchRows <= 0 || limits.MaxRows == 0 || limits.MaxBytes == 0 {
		return result, errIndexValidationLimit
	}
	binding, ok := s.index(index)
	if !ok || reader == nil {
		return result, errIndexSetDescriptor
	}
	if binding.plan.State() != model.StateWriteOnly && binding.plan.State() != model.StatePublic {
		return result, errIndexSetDescriptor
	}
	p := binding.plan
	memory := indexSetBudget{remaining: limits.MaxBytes}
	// The wire encoder and plan hasher each use bounded 4 KiB scratch. They do
	// not overlap; the second reservation also covers SHA state and headers.
	if err := memory.charge(1, 8<<10); err != nil {
		return result, err
	}
	if err := memory.charge(1, uint64(unsafe.Sizeof(indexValidator{}))); err != nil {
		return result, err
	}
	if err := memory.charge(uint64(len(p.baseColumns)+len(p.layout.Columns)), uint64(unsafe.Sizeof(model.ColumnID(0)))); err != nil {
		return result, err
	}
	columns := max(len(p.baseColumns), len(p.layout.Columns))
	if err := memory.charge(uint64(columns), uint64(unsafe.Sizeof(model.Value{}))+uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return result, err
	}
	if err := memory.charge(uint64(len(p.baseKeyColumns)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return result, err
	}
	metadata := p.metadataBytes()
	metadataExtra := uint64(unsafe.Sizeof(storage.KeySpan{})) + uint64(columns)*uint64(unsafe.Sizeof(model.ColumnID(0)))
	if err := memory.charge(metadata, 1); err != nil {
		return result, err
	}
	if err := memory.charge(metadataExtra, 1); err != nil {
		return result, err
	}
	metadata += metadataExtra
	// Five disjoint regions admit the scan image, point image, generated entry,
	// key/entry scratch, and preceding scan key. Tightening a shallow plan copy
	// preserves the encoder and expression semantics without mutating the set.
	entryBytes := min(p.entryBytes(), memory.remaining/5)
	if entryBytes == 0 {
		return result, errIndexValidationLimit
	}
	v := indexValidator{
		reader: reader, base: s.target, plan: *p, limits: limits,
		metadataBytes: metadata, entryBytes: entryBytes,
		baseIDs:  make([]model.ColumnID, len(p.baseColumns)),
		indexIDs: make([]model.ColumnID, len(p.layout.Columns)),
		values:   make([]model.Value, columns),
		baseKeys: make([]model.Value, len(p.baseKeyColumns)),
		hasher:   indexValidationHasher{Hash: sha256.New()},
	}
	v.plan.limits.EntryBytes = entryBytes
	for i, column := range p.baseColumns {
		v.baseIDs[i] = column.ID
	}
	for i, column := range p.layout.Columns {
		v.indexIDs[i] = column.ID
	}
	if err := v.scan(ctx, true); err != nil {
		return result, err
	}
	if err := v.scan(ctx, false); err != nil {
		return result, err
	}
	if v.expected != v.result.IndexRows {
		return result, errIndexValidationCorrupt
	}
	return v.result, ctx.Err()
}

type indexValidator struct {
	reader                    storage.SnapshotReader
	base                      storage.ObjectRef
	plan                      IndexPlan
	limits                    IndexValidationLimits
	metadataBytes, entryBytes uint64
	baseIDs, indexIDs         []model.ColumnID
	values, baseKeys          []model.Value
	hasher                    indexValidationHasher
	snapshot                  model.Timestamp
	fence                     [16]byte
	result                    IndexValidationResult
	expected                  uint64
}

func (v *indexValidator) scan(ctx context.Context, base bool) (err error) {
	layout, keys, ids, target := v.plan.layout.Columns, v.plan.keyLayout, v.indexIDs, v.plan.target
	if base {
		layout, keys, ids, target = v.plan.baseColumns, v.plan.baseKeyLayout, v.baseIDs, v.base
	}
	request := storage.PushdownRequest{
		Target: target, Layout: layout, KeyLayout: keys, ColumnIDs: ids,
		Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}
	want, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request}, v.metadataBytes)
	if err != nil {
		return err
	}
	plan, err := v.reader.PlanScan(ctx, request)
	if err == nil || plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, v.reader.ReleasePlan(plan)) }()
	}
	if err != nil {
		return err
	}
	if err := v.checkPlan(ctx, request, plan, want); err != nil {
		return err
	}
	cursor, err := v.reader.VectorizedScan(ctx, plan)
	if cursor != nil {
		defer func() { err = errors.Join(err, cursor.Close()) }()
	}
	if err != nil {
		return err
	}
	if cursor == nil {
		return errIndexValidationPlan
	}
	batch := columnar.Batch{MaxRows: v.limits.BatchRows, Columns: make([]columnar.Vector, 0, len(layout))}
	defer batch.Reset()
	defer clear(v.values)
	defer clear(v.baseKeys)
	var previous []byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch.Reset()
		if err := cursor.Next(ctx, &batch); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if batch.Rows() > v.limits.BatchRows || len(batch.Columns) != len(layout) {
			return errIndexValidationPlan
		}
		// Native selections can reference a larger physical block; its storage
		// belongs to the reader's quota, not to the scalar bridge below.
		if err := batch.Validate(max(batch.Length, v.limits.BatchRows)); err != nil {
			return errors.Join(errIndexValidationCorrupt, err)
		}
		for i := range batch.Columns {
			if batch.Columns[i].Type != layout[i].Type {
				return errIndexValidationPlan
			}
		}
		for ordinal := range batch.Rows() {
			if v.result.BaseRows >= v.limits.MaxRows-v.result.IndexRows {
				return errIndexValidationLimit
			}
			row, err := v.scanRow(ctx, &batch, ordinal, layout, keys, base)
			if err != nil {
				return err
			}
			if previous != nil && bytes.Compare(previous, row.Key) >= 0 {
				return indexJobFailure(row.Key, errIndexValidationCorrupt)
			}
			if base {
				v.result.BaseRows++
				err = v.checkBase(ctx, &row)
			} else {
				v.result.IndexRows++
				err = v.checkIndex(ctx, &row)
			}
			if err != nil {
				key := row.Key
				owner := len(v.plan.keys) + 1
				if !base && owner < len(row.Values) && row.Values[owner].Type == model.TypeBytes && !row.Values[owner].IsNull {
					key = row.Values[owner].BytesVal
				}
				return indexJobFailure(key, err)
			}
			previous = row.Key
			clear(v.values)
			clear(v.baseKeys)
		}
	}
}

func (v *indexValidator) checkPlan(ctx context.Context, request storage.PushdownRequest, plan storage.PushdownPlan, want [32]byte) error {
	missingIdentity := plan.Binding == ([32]byte{}) || plan.SnapshotTS <= 0 || plan.FenceID == ([16]byte{})
	filtered := !plan.Accepted.Empty() || !plan.Residual.Empty() || plan.AppliedLimit != (storage.ScanLimit{})
	wrongOutput := !slices.Equal(plan.Projection, request.ColumnIDs) || len(plan.Ordering) != len(request.KeyLayout)
	if missingIdentity || filtered || wrongOutput {
		return errIndexValidationPlan
	}
	if v.snapshot != 0 && (v.snapshot != plan.SnapshotTS || v.fence != plan.FenceID) {
		return errIndexValidationPlan
	}
	for i, key := range request.KeyLayout {
		if plan.Ordering[i] != (storage.ScanOrdering{KeyOrdinal: uint32(i), Type: key.Type, Descending: key.Descending, NullsFirst: key.NullsFirst}) {
			return errIndexValidationPlan
		}
	}
	// Copying first admits recursive layout/default metadata before hashing it.
	owned, _, err := storage.CopyPushdownRequest(ctx, plan.Request, v.metadataBytes)
	if err != nil {
		return err
	}
	got, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: owned}, v.metadataBytes)
	if err != nil {
		return err
	}
	if got != want {
		return errIndexValidationPlan
	}
	v.snapshot, v.fence = plan.SnapshotTS, plan.FenceID
	return ctx.Err()
}

func (v *indexValidator) scanRow(ctx context.Context, batch *columnar.Batch, ordinal int, layout []storage.ColumnLayout, keys []storage.KeyColumn, base bool) (model.Row, error) {
	row := model.Row{Values: v.values[:len(layout)]}
	memory := indexSetBudget{remaining: v.entryBytes}
	if err := memory.charge(1, uint64(unsafe.Sizeof(model.Row{}))); err != nil {
		return model.Row{}, err
	}
	if err := memory.charge(uint64(len(row.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return model.Row{}, err
	}
	for i, column := range layout {
		value, size, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(ordinal), memory.remaining)
		if err != nil {
			return model.Row{}, err
		}
		memory.remaining -= size
		if err := validateIndexPlanValue(ctx, value, column); err != nil {
			return model.Row{}, err
		}
		row.Values[i] = value
	}
	keyValues := row.Values[:len(keys)]
	if base {
		for i, column := range v.plan.baseKeyColumns {
			v.baseKeys[i] = row.Values[column]
		}
		keyValues = v.baseKeys
	}
	key, err := v.plan.encoder.EncodePrefix(ctx, keys, keyValues, memory.remaining)
	if err != nil {
		return model.Row{}, err
	}
	if err := memory.charge(uint64(cap(key)), 1); err != nil {
		return model.Row{}, err
	}
	if len(key) == 0 {
		return model.Row{}, errIndexValidationCorrupt
	}
	row.Key = key
	return row, ctx.Err()
}

func (v *indexValidator) pointRow(ctx context.Context, target storage.ObjectRef, key []byte, ids []model.ColumnID, layout []storage.ColumnLayout) (model.Row, error) {
	row, err := v.reader.Get(ctx, storage.ReadRequest{Target: target, Key: key, ColumnIDs: ids, Layout: layout})
	if err != nil {
		if indexRowAbsent(err) {
			return model.Row{}, errIndexValidationCorrupt
		}
		return model.Row{}, err
	}
	if row.Deleted || !bytes.Equal(row.Key, key) || len(row.Values) != len(layout) {
		return model.Row{}, errIndexValidationCorrupt
	}
	memory := indexSetBudget{remaining: v.entryBytes}
	if err := memory.charge(1, uint64(unsafe.Sizeof(model.Row{}))); err != nil {
		return model.Row{}, err
	}
	if err := memory.charge(uint64(len(row.Key)), 1); err != nil {
		return model.Row{}, err
	}
	if err := memory.charge(uint64(len(row.Values)), uint64(unsafe.Sizeof(model.Value{}))); err != nil {
		return model.Row{}, err
	}
	for i, value := range row.Values {
		size, err := model.ValueBytes(ctx, value, memory.remaining)
		if err != nil {
			return model.Row{}, err
		}
		memory.remaining -= size
		if err := validateIndexPlanValue(ctx, value, layout[i]); err != nil {
			return model.Row{}, err
		}
	}
	return row, ctx.Err()
}

func (v *indexValidator) checkBase(ctx context.Context, row *model.Row) error {
	entry, err := v.plan.Entry(ctx, row)
	if err != nil || entry == nil {
		return err
	}
	stored, err := v.pointRow(ctx, v.plan.target, entry.Key, v.indexIDs, v.plan.layout.Columns)
	if err != nil {
		return err
	}
	if err := v.compare(ctx, &stored, entry); err != nil {
		return err
	}
	v.expected++
	return nil
}

func (v *indexValidator) checkIndex(ctx context.Context, row *model.Row) error {
	owner := row.Values[len(v.plan.keyLayout)]
	if owner.Type != model.TypeBytes || owner.IsNull || len(owner.BytesVal) == 0 {
		return errIndexOwnerCorrupt
	}
	base, err := v.pointRow(ctx, v.base, owner.BytesVal, v.baseIDs, v.plan.baseColumns)
	if err != nil {
		return err
	}
	entry, err := v.plan.Entry(ctx, &base)
	if err != nil {
		return err
	}
	if entry == nil {
		return errIndexValidationCorrupt
	}
	return v.compare(ctx, row, entry)
}

func (v *indexValidator) compare(ctx context.Context, stored *model.Row, entry *IndexEntry) error {
	if !bytes.Equal(stored.Key, entry.Key) || len(stored.Values) != len(entry.Values) || !slices.Equal(entry.ColumnIDs, v.indexIDs) {
		return errIndexValidationCorrupt
	}
	owner := stored.Values[len(v.plan.keyLayout)]
	if owner.Type != model.TypeBytes || owner.IsNull || len(owner.BytesVal) == 0 {
		return errIndexOwnerCorrupt
	}
	if !bytes.Equal(owner.BytesVal, entry.Values[len(v.plan.keyLayout)].BytesVal) {
		if v.plan.isUnique() {
			return errIndexUniqueViolation
		}
		return errIndexOwnerCorrupt
	}
	want, err := v.digest(ctx, entry.Values)
	if err != nil {
		return err
	}
	got, err := v.digest(ctx, stored.Values)
	if err != nil {
		return err
	}
	if got != want {
		return errIndexValidationCorrupt
	}
	return ctx.Err()
}

// The mutation wire representation preserves NaN bits, numeric spellings,
// array dimensions and nested NULLs without SQL equality or payload copies.
func (v *indexValidator) digest(ctx context.Context, values []model.Value) ([32]byte, error) {
	v.hasher.Reset()
	v.hasher.remaining = v.entryBytes
	for _, value := range values {
		if err := storage.EncodeValue(ctx, &v.hasher, value, v.hasher.remaining); err != nil {
			return [32]byte{}, err
		}
	}
	var result [32]byte
	v.hasher.Sum(result[:0])
	return result, ctx.Err()
}

type indexValidationHasher struct {
	hash.Hash
	remaining uint64
}

func (h *indexValidationHasher) Write(data []byte) (int, error) {
	if uint64(len(data)) > h.remaining {
		return 0, errIndexValidationLimit
	}
	n, err := h.Hash.Write(data)
	h.remaining -= uint64(n)
	return n, err
}

func indexValidationFailure(err error) error {
	code := "XX001"
	var sqlError *model.Error
	if errors.As(err, &sqlError) {
		code = sqlError.Code
	}
	if errors.Is(err, model.ErrValueBudget) || errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, ErrIndexLimit) || errors.Is(err, storage.ErrDecodeBudget) {
		code = "54000"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "57014"
	}
	return &model.Error{Code: code, Message: "index validation failed", Cause: err}
}
