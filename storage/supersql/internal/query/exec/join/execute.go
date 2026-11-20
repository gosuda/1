package join

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (c *cursor) initialize(ctx context.Context) error {
	if c.options.Kind == ir.MarkJoin {
		return c.initializeMark(ctx)
	}
	for i := range c.partitions {
		p := &c.partitions[i]
		var err error
		p.left, err = c.newSpool(ctx, Left)
		if err != nil {
			return err
		}
		p.right, err = c.newSpool(ctx, Right)
		if err != nil {
			return err
		}
	}
	if err := c.consume(ctx, Left); err != nil {
		return err
	}
	if err := c.consume(ctx, Right); err != nil {
		return err
	}
	return nil
}

func (c *cursor) consume(ctx context.Context, side Side) error {
	input := c.left
	layouts := c.options.LeftLayouts
	if side == Right {
		input = c.right
		layouts = c.options.RightLayouts
	}
	batch := columnar.Batch{MaxRows: c.options.BatchRows}
	defer batch.Reset()
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		err := input.Next(ctx, &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err = batch.Validate(max(batch.Length, c.options.BatchRows)); err != nil {
			return err
		}
		if batch.Rows() > c.options.BatchRows || len(batch.Columns) != len(layouts) {
			return columnar.ErrInvalidBatch
		}
		for i := range layouts {
			if !batch.Columns[i].MatchesLayout(layouts[i]) {
				return columnar.ErrInvalidBatch
			}
		}
		if c.partitioned != nil && batch.Rows() > 0 {
			c.partitioned.input = input.(PartitionedInput).Partition()
			if c.partitioned.input >= c.partitioned.count {
				return errInputPartition
			}
		}
		for logical := range batch.Rows() {
			if err = c.check(ctx); err != nil {
				return err
			}
			row := c.row[:len(layouts)+3]
			if err := c.prepareRow(ctx, &batch, batch.Lane(logical), side, row); err != nil {
				return err
			}
			var index uint64
			if c.partitioned != nil {
				index = uint64(c.partitioned.input)
			} else {
				index = xxh3.Hash(row[0].BytesVal) % uint64(len(c.partitions))
			}
			spool := c.partitions[index].left
			if side == Right {
				spool = c.partitions[index].right
			}
			if _, e := spool.AppendRow(ctx, row); e != nil {
				return e
			}
			clear(row)
			clear(c.keyValues)
		}
		batch.Reset()
	}
	if side == Left {
		c.left = nil
	} else {
		c.right = nil
	}
	if err := input.Close(); err != nil {
		return err
	}
	for i := range c.partitions {
		spool := c.partitions[i].left
		if side == Right {
			spool = c.partitions[i].right
		}
		if err := spool.Seal(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *cursor) prepareRow(ctx context.Context, batch *columnar.Batch, lane int, side Side, row []model.Value) error {
	clear(row)
	defer func() { clear(c.keyValues) }()
	remaining := c.options.MaxRecordBytes * 8
	for col := range row[3:] {
		value, used, err := columnar.CopyValue(ctx, &batch.Columns[col], lane, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		row[col+3] = value
	}
	matchable := true
	for i, key := range c.options.Keys {
		col := key.Left
		if side == Right {
			col = key.Right
		}
		c.keyValues[i] = row[col+3]
		if row[col+3].IsNull && !key.NullEqual {
			matchable = false
		}
	}
	encoded, err := c.options.Encoder.EncodePrefix(ctx, c.keyLayout, c.keyValues, c.options.MaxRecordBytes)
	if err != nil {
		return err
	}
	row[0], row[1], row[2] = model.BytesVal(encoded), model.BoolVal(matchable), model.BoolVal(false)
	if c.partitioned != nil {
		return c.partitioned.validate(ctx, c.options.Encoder, c.keyValues, encoded, c.options.MaxRecordBytes)
	}
	return nil
}

func (c *cursor) openPartition(ctx context.Context) error {
	p := &c.partitions[c.part]
	var err error
	c.leftReader, err = p.left.Reader(ctx)
	if err != nil {
		return err
	}
	c.rightReader, err = p.right.Reader(ctx)
	if err != nil {
		return err
	}
	if len(c.options.Keys) == 0 {
		return nil
	}
	rows := p.right.Rows()
	perEntry := uint64(unsafe.Sizeof(entry{})) + uint64(unsafe.Sizeof(int(0)))*2
	if max(uint64(1), rows) > c.hashBytes/perEntry {
		return nil
	}
	// Fixed arrays avoid map growth or a per-key allocation during build/probe.
	size := max(uint64(1), rows) * perEntry
	c.hashClaim, err = c.options.Query.Allocator(runtime.WorkMemory).Reserve(size)
	if errors.Is(err, runtime.ErrBudget) {
		return nil
	}
	if err != nil {
		return err
	}
	c.entries = make([]entry, 0, int(rows))
	c.buckets = make([]int, max(1, int(rows)*2))
	for i := range c.buckets {
		c.buckets[i] = -1
	}
	for {
		position, row, e := c.rightReader.NextRow(ctx)
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		hash := xxh3.Hash(row[0].BytesVal)
		bucket := hash % uint64(len(c.buckets))
		index := len(c.entries)
		c.entries = append(c.entries, entry{position: position, next: c.buckets[bucket]})
		if row[1].IntVal != 0 {
			c.buckets[bucket] = index
		}
	}
	c.hashMode = true
	return nil
}

func (c *cursor) mapRow(dst []model.Value, mapping []Column, left, right []model.Value) {
	for i, col := range mapping {
		if col.Side == Mark {
			dst[i] = c.mark
			continue
		}
		row := left
		if col.Side == Right {
			row = right
		}
		if row == nil {
			layout, _ := c.layout(col)
			dst[i] = model.NullVal(layout.Type.Storage)
		} else {
			dst[i] = row[col.Index+3]
		}
	}
}
func (c *cursor) matches(ctx context.Context, left, right []model.Value) (model.Value, error) {
	keyResult := model.BoolVal(true)
	if c.options.Kind == ir.MarkJoin {
		var err error
		keyResult, err = c.markKeys(ctx, left, right)
		if err != nil {
			return model.Value{}, err
		}
	} else if left[1].IntVal == 0 || right[1].IntVal == 0 || !bytes.Equal(left[0].BytesVal, right[0].BytesVal) {
		return model.BoolVal(false), nil
	}
	if c.predicate == nil {
		return keyResult, nil
	}
	c.mapRow(c.predicateRow, c.options.ResidualInputs, left, right)
	c.predicateBuilder.Reset()
	if err := c.predicateBuilder.Append(ctx, c.predicateRow); err != nil {
		return model.Value{}, err
	}
	clear(c.predicateRow)
	var input, out columnar.Batch
	defer input.Reset()
	defer out.Reset()
	if err := c.predicateBuilder.Publish(&input); err != nil {
		return model.Value{}, err
	}
	if err := c.predicate.Eval(ctx, &input, c.options.Parameters, &out, expr.Direct); err != nil {
		return model.Value{}, err
	}
	result := model.NullVal(model.TypeBoolean)
	if !out.Columns[0].IsNull(0) {
		result = model.BoolVal(out.Columns[0].Int64[out.Columns[0].ValueIndex(0)] != 0)
	}
	return andTruth(keyResult, result), nil
}

func andTruth(left, right model.Value) model.Value {
	leftFalse := !left.IsNull && left.IntVal == 0
	rightFalse := !right.IsNull && right.IntVal == 0
	if leftFalse || rightFalse {
		return model.BoolVal(false)
	}
	if left.IsNull || right.IsNull {
		return model.NullVal(model.TypeBoolean)
	}
	return model.BoolVal(true)
}

func (c *cursor) markKeys(ctx context.Context, left, right []model.Value) (model.Value, error) {
	result := model.BoolVal(true)
	if left[1].IntVal != 0 && right[1].IntVal != 0 {
		return model.BoolVal(bytes.Equal(left[0].BytesVal, right[0].BytesVal)), nil
	}
	for i, key := range c.options.Keys {
		l, r := left[key.Left+3], right[key.Right+3]
		value := model.BoolVal(true)
		if l.IsNull || r.IsNull {
			if key.NullEqual {
				value = model.BoolVal(l.IsNull && r.IsNull)
			} else {
				value = model.NullVal(model.TypeBoolean)
			}
		} else {
			layout := c.keyLayout[i : i+1]
			encodedLeft, err := c.options.Encoder.EncodePrefix(ctx, layout, left[key.Left+3:key.Left+4], c.options.MaxRecordBytes)
			if err != nil {
				return model.Value{}, err
			}
			encodedRight, err := c.options.Encoder.EncodePrefix(ctx, layout, right[key.Right+3:key.Right+4], c.options.MaxRecordBytes)
			if err != nil {
				return model.Value{}, err
			}
			value = model.BoolVal(bytes.Equal(encodedLeft, encodedRight))
		}
		result = andTruth(result, value)
	}
	return result, nil
}

func (c *cursor) accumulateMark(value model.Value) {
	switch c.options.MarkKind {
	case ir.ExistsSubquery:
		if !value.IsNull && value.IntVal != 0 {
			c.mark = model.BoolVal(true)
		}
	case ir.AnySubquery:
		if !value.IsNull && value.IntVal != 0 {
			c.mark = model.BoolVal(true)
		} else if value.IsNull && (c.mark.IsNull || c.mark.IntVal == 0) {
			c.mark = value
		}
	case ir.AllSubquery:
		c.mark = andTruth(c.mark, value)
	}
}
func (c *cursor) preservesRight() bool {
	return c.options.Kind == ir.RightJoin || c.options.Kind == ir.FullJoin
}
func (c *cursor) emitsUnmatchedLeft() bool {
	return c.options.Kind == ir.LeftJoin || c.options.Kind == ir.FullJoin || c.options.Kind == ir.AntiJoin
}

func (c *cursor) startLeft(ctx context.Context) error {
	_, row, err := c.leftReader.NextRow(ctx)
	if err != nil {
		return err
	}
	c.leftRow = row
	c.leftMatched = false
	c.scanning = true
	if c.hashMode {
		c.candidate = -1
		if row[1].IntVal != 0 {
			hash := xxh3.Hash(row[0].BytesVal)
			c.candidate = c.buckets[hash%uint64(len(c.buckets))]
		}
		return nil
	}
	if c.rightReader != nil {
		if err = c.rightReader.Close(); err != nil {
			return err
		}
		c.rightReader = nil
	}
	c.rightReader, err = c.partitions[c.part].right.Reader(ctx)
	if err != nil {
		return err
	}
	if c.preservesRight() {
		c.replacement, err = c.newSpool(ctx, Right)
	}
	return err
}

func (c *cursor) finishLeft(ctx context.Context) error {
	if c.replacement != nil {
		if err := c.replacement.Seal(ctx); err != nil {
			return err
		}
		if err := c.rightReader.Close(); err != nil {
			return err
		}
		c.rightReader = nil
		p := &c.partitions[c.part]
		if err := p.right.Close(); err != nil {
			return err
		}
		p.right = c.replacement
		c.replacement = nil
	}
	c.scanning = false
	return nil
}

func (c *cursor) next(ctx context.Context) error {
	if c.options.Kind == ir.MarkJoin {
		return c.nextMark(ctx)
	}
	for c.part < len(c.partitions) {
		if err := c.check(ctx); err != nil {
			return err
		}
		if c.leftReader == nil {
			if err := c.openPartition(ctx); err != nil {
				return err
			}
		}
		if c.draining {
			err := c.drainRight(ctx)
			if err == nil {
				return nil
			}
			if err != io.EOF {
				return err
			}
			if err = c.closePartition(); err != nil {
				return err
			}
			c.part++
			continue
		}
		if !c.scanning {
			err := c.startLeft(ctx)
			if err == io.EOF {
				if err = c.beginRightDrain(ctx); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
		}
		var right []model.Value
		index := -1
		var err error
		if c.hashMode {
			if c.candidate < 0 {
				err = io.EOF
			} else {
				index = c.candidate
				c.candidate = c.entries[index].next
				right, err = c.rightReader.Read(ctx, c.entries[index].position)
			}
		} else {
			_, right, err = c.rightReader.NextRow(ctx)
		}
		if err == io.EOF {
			if err = c.finishLeft(ctx); err != nil {
				return err
			}
			if !c.leftMatched && c.emitsUnmatchedLeft() {
				c.mapRow(c.output, c.options.Output, c.leftRow, nil)
				return nil
			}
			continue
		}
		if err != nil {
			return err
		}
		truth, err := c.matches(ctx, c.leftRow, right)
		if err != nil {
			return err
		}
		matched := !truth.IsNull && truth.IntVal != 0
		if matched {
			c.leftMatched = true
			if index >= 0 {
				c.entries[index].matched = true
			}
		}
		if c.replacement != nil {
			if matched {
				right[2] = model.BoolVal(true)
			}
			if _, err = c.replacement.AppendRow(ctx, right); err != nil {
				return err
			}
		}
		if !matched {
			continue
		}
		switch c.options.Kind {
		case ir.AntiJoin:
			c.candidate = -1
			if !c.hashMode {
				if err = c.finishLeft(ctx); err != nil {
					return err
				}
			}
			continue
		case ir.SemiJoin:
			c.candidate = -1
			if !c.hashMode {
				if err = c.finishLeft(ctx); err != nil {
					return err
				}
			}
			c.mapRow(c.output, c.options.Output, c.leftRow, nil)
		default:
			c.mapRow(c.output, c.options.Output, c.leftRow, right)
		}
		return nil
	}
	return io.EOF
}

func (c *cursor) beginRightDrain(ctx context.Context) error {
	c.leftRow = nil
	c.draining = true
	if c.hashMode || !c.preservesRight() {
		return nil
	}
	if c.rightReader != nil {
		if err := c.rightReader.Close(); err != nil {
			return err
		}
		c.rightReader = nil
	}
	var err error
	c.rightReader, err = c.partitions[c.part].right.Reader(ctx)
	return err
}

func (c *cursor) drainRight(ctx context.Context) error {
	if !c.preservesRight() {
		return io.EOF
	}
	if c.hashMode {
		for c.unmatched < len(c.entries) {
			index := c.unmatched
			c.unmatched++
			if c.entries[index].matched {
				continue
			}
			right, err := c.rightReader.Read(ctx, c.entries[index].position)
			if err != nil {
				return err
			}
			c.mapRow(c.output, c.options.Output, nil, right)
			return nil
		}
		return io.EOF
	}
	for {
		_, right, err := c.rightReader.NextRow(ctx)
		if err != nil {
			return err
		}
		if right[2].IntVal != 0 {
			continue
		}
		c.mapRow(c.output, c.options.Output, nil, right)
		return nil
	}
}
