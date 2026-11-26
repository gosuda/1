package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// MultirangeIterator borrows one immutable payload. NextBorrowed results remain
// valid until that payload is released or modified. Calls must be serialized;
// Close and exhaustion drop the iterator's borrowed references.
type MultirangeIterator struct {
	cursor multirangeCursor
	done   bool
}

func NewMultirangeIterator(ctx context.Context, id model.TypeID, value model.Value) (MultirangeIterator, error) {
	if err := ctx.Err(); err != nil {
		return MultirangeIterator{}, err
	}
	family, known := LookupRangeFamily(id)
	if !known || id != family.Multirange || value.Type != model.TypeBytes {
		return MultirangeIterator{}, sqlError("42804", "multirange iterator requires its declared type")
	}
	if value.IsNull {
		return MultirangeIterator{done: true}, nil
	}
	cursor, err := newMultirangeCursor(family, value)
	return MultirangeIterator{cursor: cursor}, err
}

func (iterator *MultirangeIterator) NextBorrowed(ctx context.Context) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if iterator.done {
		return nil, false, nil
	}
	start := iterator.cursor.offset
	_, more, err := iterator.cursor.next(ctx)
	if err != nil {
		return nil, false, err
	}
	if !more {
		iterator.Close()
		return nil, false, nil
	}
	return iterator.cursor.data[start+4 : iterator.cursor.offset], true, nil
}

func (iterator *MultirangeIterator) Close() {
	*iterator = MultirangeIterator{done: true}
}
