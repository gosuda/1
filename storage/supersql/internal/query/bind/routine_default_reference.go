package bind

import (
	"cmp"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Relation constants depend on object identity, not mutable sequence parameters.
// Sequence identity additionally distinguishes provisional ObjectID reuse.
type routineDefaultIdentity struct {
	Object    model.ObjectID
	OID       model.OID
	Kind      model.ObjectKind
	Sequence  uint64 `json:",omitempty"`
	namespace model.ObjectID
}

func (b *binder) defaultIdentityDependencies(graph *ir.Graph) ([]model.Dependency, []routineDefaultIdentity, error) {
	var inline [8]model.OID
	oids := inline[:0]
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind != ir.Const {
			continue
		}
		typ, _ := graph.Type(scalar.Layout)
		if typ.ID != 2205 {
			continue
		}
		value, err := graph.CloneConstant(b.ctx, id, b.options.Limits.MaxBytes)
		if err != nil {
			return nil, nil, err
		}
		if !value.IsNull {
			oids = append(oids, model.OID(value.IntVal))
		}
	}
	if len(oids) == 0 {
		return b.dependencies, nil, nil
	}
	if uint64(len(b.dependencies))*3*uint64(unsafe.Sizeof(routineDefaultIdentity{})) > b.options.Limits.MaxBytes {
		return nil, nil, model.ErrValueBudget
	}
	slices.Sort(oids)
	var identities []routineDefaultIdentity
	for _, dep := range b.dependencies {
		if dep.Column != 0 {
			continue
		}
		descriptor, err := b.declarationDescriptor(dep.Object)
		if err != nil {
			return nil, nil, err
		}
		if _, found := slices.BinarySearch(oids, descriptor.OID()); !found {
			continue
		}
		if !defaultRelationKind(descriptor.Kind()) {
			continue
		}
		reference := routineDefaultIdentity{Object: descriptor.ID(), OID: descriptor.OID(), Kind: descriptor.Kind(), namespace: descriptor.Name().Parent}
		if sequence, ok := descriptor.Sequence(); ok {
			reference.Sequence = sequence.Identity
		}
		identities = append(identities, reference)
	}
	if len(identities) == 0 {
		return b.dependencies, nil, nil
	}
	dependencies := make([]model.Dependency, 0, len(b.dependencies))
	for _, dep := range b.dependencies {
		namespace := slices.ContainsFunc(identities, func(reference routineDefaultIdentity) bool { return reference.namespace == dep.Object })
		if !namespace {
			dependencies = append(dependencies, dep)
		}
	}
	slices.SortFunc(identities, func(a, b routineDefaultIdentity) int { return cmp.Compare(a.Object, b.Object) })
	return dependencies, identities, nil
}
func defaultRelationKind(kind model.ObjectKind) bool {
	switch kind {
	case model.ObjectTable, model.ObjectSequence, model.ObjectIndex, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation:
		return true
	}
	return false
}
func (b *binder) validateDefaultDependencies(dependencies []model.Dependency, identities []routineDefaultIdentity, pos int) error {
	if len(identities) > len(dependencies) {
		return bindError("22023", "invalid default relation identity count", pos)
	}
	for i, identity := range identities {
		invalid := identity.Object == 0 || identity.OID == 0 || !defaultRelationKind(identity.Kind)
		outOfOrder := i > 0 && identities[i-1].Object >= identity.Object
		if invalid || outOfOrder {
			return bindError("22023", "invalid default relation identity", pos)
		}
		if (identity.Kind == model.ObjectSequence) != (identity.Sequence != 0) {
			return bindError("22023", "invalid default sequence identity", pos)
		}
	}
	matched := 0
	for i, dependency := range dependencies {
		if dependency.Object == 0 || dependency.Version == 0 {
			return bindError("22023", "invalid routine default dependency", pos)
		}
		for _, prior := range dependencies[:i] {
			if prior == dependency {
				return bindError("22023", "duplicate routine default dependency", pos)
			}
		}
		if b.catalog == nil {
			return bindError("0A000", "routine default requires its catalog snapshot", pos)
		}
		descriptor, err := b.declarationDescriptor(dependency.Object)
		if err != nil {
			return err
		}
		if descriptor.State() != model.StatePublic {
			return bindError("0A000", "routine default dependency is not public", pos)
		}
		index, identity := slices.BinarySearchFunc(identities, dependency.Object, func(reference routineDefaultIdentity, id model.ObjectID) int {
			return cmp.Compare(reference.Object, id)
		})
		if identity && dependency.Column == 0 {
			reference := identities[index]
			if descriptor.OID() != reference.OID || descriptor.Kind() != reference.Kind {
				return bindError("0A000", "routine default relation identity changed", pos)
			}
			if sequence, ok := descriptor.Sequence(); ok && sequence.Identity != reference.Sequence {
				return bindError("0A000", "routine default sequence identity changed", pos)
			}
			matched++
		} else if descriptor.Version() != dependency.Version {
			return bindError("0A000", "routine default dependency version changed", pos)
		}
		b.dependency(descriptor, dependency.Column)
	}
	if matched != len(identities) {
		return bindError("22023", "default relation identity lacks dependency", pos)
	}
	return b.ctx.Err()
}
