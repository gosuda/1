package catalog

import (
	"context"
	"errors"
	"reflect"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// CurrentReadViews transfers an owned, certified current reader on each call.
// Its timestamp must be selected after the caller's schema intent is acquired.
type CurrentReadViews interface {
	CurrentReadView(context.Context) (txn.ReadView, error)
}

// CurrentDescriptors borrows its collaborators but retains no reader or descriptor.
type CurrentDescriptors struct {
	store *Store
	views CurrentReadViews
}

func NewCurrentDescriptors(store *Store, views CurrentReadViews) (*CurrentDescriptors, error) {
	if store == nil || currentDescriptorNil(store.encoder) || currentDescriptorNil(views) {
		return nil, storage.ErrInvalidLayout
	}
	if err := store.limits.Validate(); err != nil {
		return nil, err
	}
	return &CurrentDescriptors{store: store, views: views}, nil
}

func (s *CurrentDescriptors) Descriptor(ctx context.Context, id model.ObjectID) (_ *model.Descriptor, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.store == nil || s.views == nil {
		return nil, storage.ErrInvalidLayout
	}
	view, err := s.views.CurrentReadView(ctx)
	if currentDescriptorNil(view) {
		if err != nil {
			return nil, err
		}
		return nil, storage.ErrSnapshotNotClosed
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	if err != nil {
		return nil, err
	}
	snapshot, err := s.store.Snapshot(view, view.Timestamp(), BorrowReader)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, snapshot.Close()) }()
	published, err := snapshot.Descriptor(ctx, id)
	return published.Descriptor, err
}

func currentDescriptorNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
