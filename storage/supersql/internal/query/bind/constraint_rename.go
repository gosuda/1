package bind

import (
	"context"
	"errors"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// constraintInheritanceMember is one descendant reached from the rename root.
// parents counts the hierarchy edges that reach it, mirroring the numparents
// list PostgreSQL derives from find_all_inheritors.
type constraintInheritanceMember struct {
	descriptor *model.Descriptor
	parents    uint32
}

// bindRenameTableConstraint implements ALTER TABLE [ONLY] t RENAME CONSTRAINT
// a TO b for an already-modeled ordinary table, following PostgreSQL's
// rename_constraint_internal: the constraint and, for an index-backed
// constraint, its owning index are renamed. An inheritable CHECK or NOT NULL
// constraint is renamed in every descendant reached through the stored parent
// lineage when the relation reference recurses. A constraint inherited from
// more parents than the operation accounts for is refused, as is ONLY while
// direct children still carry it.
//
// The descendant rename looks the constraint up by name in each table, exactly
// as PostgreSQL does; it deliberately does not assume a descendant's
// ParentConstraint names the root, because a grandchild records its immediate
// parent and a child may merge a local constraint with the inherited one.
func bindRenameTableConstraint(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Subname == "" || statement.Newname == "" || statement.Relation.Catalogname != "" {
		return nil, bindError("42601", "invalid ALTER TABLE RENAME CONSTRAINT", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	root, _, err := resolveNamespaceObject(ctx, snapshot, options.Bind.SearchPath, parts, []model.ObjectKind{model.ObjectTable}, "42P01", 0)
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && (diagnostic.Code == "42704" || diagnostic.Code == "42P01") {
				return &BoundObjectCommand{tag: "ALTER TABLE", warnings: []model.Error{{Code: "42P01", Message: `relation "` + statement.Relation.Relname + `" does not exist, skipping`}}}, nil
			}
		}
		return nil, err
	}
	b.dependency(root, 0)
	rootSpec, err := root.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if rootSpec.Table == nil {
		return nil, bindError("42809", "relation is not a table", 0)
	}
	rootIndex := constraintIndexByName(rootSpec.Table.Constraints, statement.Subname)
	if rootIndex < 0 {
		return nil, constraintMissingError(statement.Subname, rootSpec.Name.Name)
	}
	target := rootSpec.Table.Constraints[rootIndex]

	// InheritanceChildren only avoids the catalog walk for the common
	// childless case; the walk itself decides which descendants are renamed.
	var members []constraintInheritanceMember
	if inheritableConstraint(target) && rootSpec.Table.InheritanceChildren != 0 {
		members, err = constraintInheritanceMembers(ctx, snapshot, root, options)
		if err != nil {
			return nil, err
		}
	}
	if inheritableConstraint(target) && !statement.Relation.Inh && len(members) != 0 {
		return nil, bindError("42P16", `inherited constraint "`+statement.Subname+`" must be renamed in child tables too`, 0)
	}

	// The root expects no parents from within the hierarchy, so an inherited
	// constraint is refused there by the same count comparison that guards each
	// descendant against parents outside this operation.
	targets := make([]constraintInheritanceMember, 0, len(members)+1)
	targets = append(targets, constraintInheritanceMember{descriptor: root})
	targets = append(targets, members...)
	// Staged updates are locked in slice order, so every table is ordered by
	// identity rather than by hierarchy position. Root-first ordering would
	// invert under a concurrent rename rooted at a descendant and deadlock.
	slices.SortFunc(targets, func(left, right constraintInheritanceMember) int {
		return compareObjectID(left.descriptor.ID(), right.descriptor.ID())
	})
	updates := make([]ObjectMutation, 0, len(targets)*2)
	for _, rename := range targets {
		table := rename.descriptor
		spec, err := table.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		if table.ID() != root.ID() {
			b.dependency(table, 0)
		}
		index := constraintIndexByName(spec.Table.Constraints, statement.Subname)
		if index < 0 {
			return nil, constraintMissingError(statement.Subname, spec.Name.Name)
		}
		constraint := spec.Table.Constraints[index]
		if inheritableConstraint(constraint) && constraint.InheritanceCount > rename.parents {
			return nil, bindError("42P16", `cannot rename inherited constraint "`+statement.Subname+`"`, 0)
		}
		if slices.ContainsFunc(spec.Table.Constraints, func(candidate model.ConstraintDescriptor) bool {
			return !candidate.Mutation && candidate.Name == statement.Newname && candidate.ID != constraint.ID
		}) {
			return nil, bindError("42710", `constraint "`+statement.Newname+`" for relation "`+spec.Name.Name+`" already exists`, 0)
		}
		if table.Version() == math.MaxUint64 {
			return nil, model.ErrDescriptorLimit
		}
		associatedIndex := constraint.Index
		spec.Table.Constraints[index].Name = statement.Newname
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if err := table.ValidateSuccessor(ctx, next); err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: table, Next: next})
		// Only an index-backed constraint carries an index, and such a
		// constraint never inherits, so at most the root reaches this point.
		if associatedIndex == 0 || !indexBackedConstraint(constraint.Kind) {
			continue
		}
		published, err := snapshot.Descriptor(ctx, associatedIndex)
		if err != nil {
			return nil, err
		}
		indexDescriptor := published.Descriptor
		if indexDescriptor.Kind() != model.ObjectIndex || indexDescriptor.Name().Name == statement.Newname {
			continue
		}
		if conflict, conflictErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: indexDescriptor.Name().Parent, Kind: model.ObjectIndex, Name: statement.Newname}); conflictErr == nil && conflict.Descriptor.ID() != indexDescriptor.ID() {
			return nil, bindError("42P07", `relation "`+statement.Newname+`" already exists`, 0)
		} else if conflictErr != nil && !isMissingObject(conflictErr) {
			return nil, conflictErr
		}
		nextIndex, err := nextObjectDescriptor(ctx, indexDescriptor, options, func(next *model.DescriptorSpec) error {
			next.Name.Name = statement.Newname
			return nil
		})
		if err != nil {
			return nil, err
		}
		b.dependency(indexDescriptor, 0)
		updates = append(updates, ObjectMutation{Base: indexDescriptor, Next: nextIndex})
	}
	return &BoundObjectCommand{
		updates:      updates,
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "ALTER TABLE",
	}, ctx.Err()
}

func constraintMissingError(name, relation string) error {
	return bindError("42704", `constraint "`+name+`" of relation "`+relation+`" does not exist`, 0)
}

// inheritableConstraint reports whether a constraint propagates to descendants,
// matching PostgreSQL's contype IN (CHECK, NOT NULL) AND NOT connoinherit test.
func inheritableConstraint(constraint model.ConstraintDescriptor) bool {
	return (constraint.Kind == model.ConstraintCheck || constraint.Kind == model.ConstraintNotNull) && !constraint.NoInherit
}

// indexBackedConstraint reports whether renaming the constraint renames its
// owning index, matching PostgreSQL's conindid check for PRIMARY, UNIQUE, and
// EXCLUSION constraints.
func indexBackedConstraint(kind model.ConstraintKind) bool {
	return kind == model.ConstraintPrimaryKey || kind == model.ConstraintUnique || kind == model.ConstraintExclude
}

// constraintInheritanceMembers mirrors PostgreSQL's find_all_inheritors over the
// stored parent lineage: every descendant reachable from root is returned once,
// carrying the number of hierarchy edges that reach it. A relation reached more
// than once keeps its position and only gains a parent count, so a diamond-
// shaped hierarchy is walked once while its shared leaf accounts for both paths.
func constraintInheritanceMembers(ctx context.Context, snapshot CatalogSnapshot, root *model.Descriptor, options CreateDeclarationOptions) ([]constraintInheritanceMember, error) {
	if snapshot == nil || root == nil {
		return nil, model.ErrInvalidDescriptor
	}
	byID := make(map[model.ObjectID]*model.Descriptor)
	children := make(map[model.ObjectID][]model.ObjectID)
	var scanned uint64
	visit := func(published catalog.PublishedDescriptor) error {
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return nil
		}
		scanned++
		if scanned > uint64(options.Bind.Limits.MaxOperands) {
			return bindError("54000", "constraint inheritance catalog scan limit exceeded", 0)
		}
		metadata, ok := published.Descriptor.Table()
		if !ok {
			return nil
		}
		if metadata.PartitionBound != nil && metadata.PartitionState == model.PartitionDetachPending {
			return nil
		}
		byID[published.Descriptor.ID()] = published.Descriptor
		for _, parent := range metadata.Parents {
			children[parent] = append(children[parent], published.Descriptor.ID())
		}
		return nil
	}
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(namespace catalog.PublishedDescriptor) error {
		if namespace.Descriptor == nil || namespace.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, namespace.Descriptor.ID(), model.ObjectTable, visit)
	}); err != nil {
		return nil, err
	}
	for parent := range children {
		slices.SortFunc(children[parent], compareObjectID)
	}
	// The root occupies no descendant slot, so a cycle that returns to it is
	// ignored rather than counted as an extra parent.
	seen := map[model.ObjectID]int{root.ID(): -1}
	queue := []model.ObjectID{root.ID()}
	members := make([]constraintInheritanceMember, 0)
	for len(queue) != 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range children[parent] {
			if at, ok := seen[child]; ok {
				if at >= 0 {
					members[at].parents++
				}
				continue
			}
			if uint64(len(members)) >= uint64(options.Bind.Limits.MaxOperands) {
				return nil, bindError("54000", "constraint inheritance member limit exceeded", 0)
			}
			seen[child] = len(members)
			members = append(members, constraintInheritanceMember{descriptor: byID[child], parents: 1})
			queue = append(queue, child)
		}
	}
	return members, nil
}
