package schema

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errMutationPolicyOptions     = &model.Error{Code: "22023", Message: "invalid checked mutation resolver options", Cause: storage.ErrInvalidTarget}
	errMutationPolicyLimit       = &model.Error{Code: "54000", Message: "internal mutation targets exceed metadata limit", Cause: model.ErrDescriptorLimit}
	errMutationDescriptorChanged = errors.Join(catalog.ErrDescriptorChanged, storage.ErrInvalidTarget)
)

// CurrentDescriptorSource returns owned immutable metadata from a new certified
// current read, never a binding-time snapshot. It retains no reader after return.
type CurrentDescriptorSource interface {
	Descriptor(context.Context, model.ObjectID) (*model.Descriptor, error)
}

// CheckedMutationResolver checks publication before consulting pinned row policies.
// The caller must hold the target's schema intent through transaction completion.
type CheckedMutationResolver struct {
	source   CurrentDescriptorSource
	base     txn.MutationResolver
	internal []storage.ObjectRef
}

// internal contains only composition-owned catalog/job collections with no user
// descriptor. maxTargets must be positive and bounds the copied allowlist.
func NewCheckedMutationResolver(source CurrentDescriptorSource, base txn.MutationResolver, internal []storage.ObjectRef, maxTargets int) (*CheckedMutationResolver, error) {
	if mutationPolicyNil(source) || mutationPolicyNil(base) {
		return nil, errMutationPolicyOptions
	}
	if maxTargets <= 0 || len(internal) > maxTargets {
		return nil, errMutationPolicyLimit
	}
	owned := slices.Clone(internal)
	slices.SortFunc(owned, func(a, b storage.ObjectRef) int {
		return cmp.Compare(a.ObjectID, b.ObjectID)
	})
	for i, target := range owned {
		if err := target.Validate(); err != nil {
			return nil, errors.Join(errMutationPolicyOptions, err)
		}
		if i > 0 && owned[i-1].ObjectID == target.ObjectID {
			return nil, errMutationPolicyOptions
		}
	}
	return &CheckedMutationResolver{source: source, base: base, internal: owned}, nil
}

func (r *CheckedMutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.source == nil || r.base == nil {
		return nil, errMutationPolicyOptions
	}
	if err := target.Validate(); err != nil {
		return nil, errors.Join(errMutationDescriptorChanged, err)
	}
	position, internal := slices.BinarySearchFunc(r.internal, target.ObjectID, func(candidate storage.ObjectRef, id model.ObjectID) int {
		return cmp.Compare(candidate.ObjectID, id)
	})
	if internal {
		if r.internal[position] != target {
			return nil, errMutationDescriptorChanged
		}
		return r.base.ResolveMutation(ctx, target)
	}

	descriptor, err := r.source.Descriptor(ctx, target.ObjectID)
	if err != nil {
		if errors.Is(err, catalog.ErrUnknownObject) || errors.Is(err, storage.ErrRowNotFound) {
			return nil, errors.Join(errMutationDescriptorChanged, err)
		}
		return nil, err
	}
	if err := validateMutationDescriptor(descriptor, target); err != nil {
		return nil, err
	}
	return r.base.ResolveMutation(ctx, target)
}

func validateMutationDescriptor(descriptor *model.Descriptor, target storage.ObjectRef) error {
	if descriptor == nil || descriptor.ID() != target.ObjectID || descriptor.Version() != target.DescriptorVersion {
		return errMutationDescriptorChanged
	}
	physical := descriptor.Physical()
	if physical.Generation != target.Generation || physical.EncodingVersion != target.EncodingVersion {
		return errMutationDescriptorChanged
	}
	switch descriptor.Kind() {
	case model.ObjectTable, model.ObjectMaterializedView:
		if descriptor.State() != model.StatePublic {
			return errMutationDescriptorChanged
		}
	case model.ObjectIndex:
		switch descriptor.State() {
		case model.StateDeleteOnly, model.StateWriteOnly, model.StatePublic, model.StateDropWriteOnly, model.StateDropDeleteOnly:
		default:
			return errMutationDescriptorChanged
		}
	default:
		return errMutationDescriptorChanged
	}
	return nil
}

func mutationPolicyNil(value any) bool {
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
