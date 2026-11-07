package routing_test

import (
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestTableMatchesLogicalIdentityAcrossPlacementChanges(t *testing.T) {
	specification := spec()
	original := table(t, specification)
	if !original.Matches(specification.Target, specification.KeyLayout) {
		t.Fatal("original logical identity rejected")
	}
	changed := specification
	changed.Epoch++
	changed.Partitions = []routing.Partition{{ID: 900, Span: full()}}
	reassigned := table(t, changed)
	if !reassigned.Matches(specification.Target, specification.KeyLayout) {
		t.Fatal("placement change affected logical identity")
	}
	for _, change := range []func(*storage.ObjectRef){
		func(target *storage.ObjectRef) { target.ObjectID++ },
		func(target *storage.ObjectRef) { target.Generation++ },
		func(target *storage.ObjectRef) { target.DescriptorVersion++ },
		func(target *storage.ObjectRef) { target.EncodingVersion++ },
	} {
		target := specification.Target
		change(&target)
		if original.Matches(target, specification.KeyLayout) {
			t.Errorf("changed target matched: %+v", target)
		}
	}
	for _, change := range []func([]storage.KeyColumn){
		func(layout []storage.KeyColumn) { layout[0].ColumnID++ },
		func(layout []storage.KeyColumn) { layout[0].Type.ID++ },
		func(layout []storage.KeyColumn) { layout[0].Type.Storage++ },
		func(layout []storage.KeyColumn) { layout[0].Type.Collation++ },
		func(layout []storage.KeyColumn) { layout[0].Descending = !layout[0].Descending },
		func(layout []storage.KeyColumn) { layout[0].NullsFirst = !layout[0].NullsFirst },
		func(layout []storage.KeyColumn) { layout[0], layout[1] = layout[1], layout[0] },
	} {
		layout := slices.Clone(specification.KeyLayout)
		change(layout)
		if original.Matches(specification.Target, layout) {
			t.Errorf("changed key layout matched: %+v", layout)
		}
	}
	if original.Matches(specification.Target, specification.KeyLayout[:1]) {
		t.Fatal("partial layout matched")
	}
	if (*routing.Table)(nil).Matches(specification.Target, specification.KeyLayout) {
		t.Fatal("nil table matched")
	}
}
