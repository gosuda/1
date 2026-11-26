package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func descriptorSpec() model.DescriptorSpec {
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	return model.DescriptorSpec{ID: 1, OID: 100, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "table"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1}, Table: &model.TableMetadata{ColumnIDHighWatermark: 2, Columns: []model.ColumnDescriptor{{ID: 1, Name: "first", Layout: model.TypeLayout{Type: typ}, State: model.StatePublic}, {ID: 2, Name: "second", Layout: model.TypeLayout{Type: typ}, State: model.StatePublic}}, PrimaryKey: []model.KeyElement{{Column: 2, Type: typ, Descending: true, NullsFirst: true}, {Column: 1, Type: typ}}}}
}
func descriptorLimits() model.DescriptorLimits {
	return model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
}
func TestDescriptorRejectsAmbiguousIdentities(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*model.DescriptorSpec)
	}{
		{"column_id", func(d *model.DescriptorSpec) { d.Table.Columns[1].ID = 1 }},
		{"column_name", func(d *model.DescriptorSpec) { d.Table.Columns[1].Name = "first" }},
		{"missing_allocation_history", func(d *model.DescriptorSpec) { d.Table.ColumnIDHighWatermark = 0 }},
		{"column_exceeds_watermark", func(d *model.DescriptorSpec) { d.Table.ColumnIDHighWatermark = 1 }},
		{"key_id", func(d *model.DescriptorSpec) { d.Table.PrimaryKey[1].Column = 2 }},
		{"unknown_key", func(d *model.DescriptorSpec) { d.Table.PrimaryKey[1].Column = 3 }},
		{"physical_identity", func(d *model.DescriptorSpec) { d.Physical.Generation = 0 }},
		{"tagged_metadata", func(d *model.DescriptorSpec) { d.Index = &model.IndexMetadata{Table: 2} }},
		{"conflicting_dependency_versions", func(d *model.DescriptorSpec) {
			d.Dependencies = []model.Dependency{{Object: 3, Version: 1}, {Object: 3, Version: 2}}
		}},
		{"duplicate_dependency", func(d *model.DescriptorSpec) {
			d.Dependencies = []model.Dependency{{Object: 3, Version: 1}, {Object: 3, Version: 1}}
		}},
		{"unknown_semantic_encoding", func(d *model.DescriptorSpec) { d.Table.Columns[0].Default = &model.ExpressionDefinition{SQL: "1"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := descriptorSpec()
			test.change(&spec)
			if _, err := model.NewDescriptor(t.Context(), spec, descriptorLimits()); !errors.Is(err, model.ErrInvalidDescriptor) {
				t.Fatalf("ambiguous metadata accepted: %v", err)
			}
		})
	}
}

func TestDescriptorNotNullConstraintShape(t *testing.T) {
	// The reachable NOT NULL states are pinned by the PostgreSQL 18.4 grammar:
	// processCASbits rejects NOT ENFORCED for NOT NULL (it passes a NULL
	// is_enforced output), so Enforced is always true, while Validated may be
	// false for the "NOT NULL ... NOT VALID" form. expected/constraints.out
	// renders that state as `"nn" NOT NULL "a" NOT VALID` with convalidated = f,
	// and an unenforced NOT NULL is unrepresentable.
	for _, test := range []struct {
		name      string
		columns   []model.ColumnID
		nullable  bool
		enforced  bool
		validated bool
		valid     bool
	}{
		{"enforced_not_validated", []model.ColumnID{1}, false, true, false, true},
		{"unenforced_nonnullable", []model.ColumnID{1}, false, false, false, false},
		{"unenforced_nullable", []model.ColumnID{1}, true, false, true, false},
		{"contradictory_nullable", []model.ColumnID{1}, true, true, false, false},
		{"missing_column", nil, false, true, true, false},
		{"multiple_columns", []model.ColumnID{1, 2}, false, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := descriptorSpec()
			spec.Table.PrimaryKey = nil
			spec.Table.Columns[0].Nullable = test.nullable
			spec.Table.Constraints = []model.ConstraintDescriptor{{ID: 4, Name: "required", Kind: model.ConstraintNotNull, Columns: test.columns, Enforced: test.enforced, Validated: test.validated}}
			descriptor, err := model.NewDescriptor(t.Context(), spec, descriptorLimits())
			if !test.valid {
				if !errors.Is(err, model.ErrInvalidDescriptor) {
					t.Fatalf("invalid NOT NULL accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			owned, err := descriptor.Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if owned.Table.Columns[0].Nullable != test.nullable || owned.Table.Constraints[0].Enforced != test.enforced || owned.Table.Constraints[0].Validated != test.validated {
				t.Fatal("constraint flags changed effective nullability")
			}
		})
	}
}
func TestDescriptorKeepsDeclaredKeyOrderAndOwnsDependencies(t *testing.T) {
	spec := descriptorSpec()
	spec.Dependencies = []model.Dependency{{Object: 30, Version: 2}, {Object: 20, Version: 3}}
	descriptor, err := model.NewDescriptor(t.Context(), spec, descriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	spec.Dependencies[0].Object = 999
	spec.Table.PrimaryKey[0].Descending = false
	owned, err := descriptor.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if owned.Table.PrimaryKey[0].Column != 2 || !owned.Table.PrimaryKey[0].Descending || !owned.Table.PrimaryKey[0].NullsFirst || owned.Table.PrimaryKey[1].Column != 1 {
		t.Fatal("key order or ordering metadata changed")
	}
	if owned.Dependencies[0].Object != 20 || owned.Dependencies[1].Object != 30 {
		t.Fatal("dependencies are aliased or not normalized")
	}
}

func TestDescriptorOwnsPendingConstraintLifecycleMetadata(t *testing.T) {
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	pending := &model.ConstraintDescriptor{ID: 3, OID: 103, Name: "candidate", Kind: model.ConstraintUnique, Columns: []model.ColumnID{1}, Index: 2, Deferrable: true, Enforced: true, Validated: true}
	spec := model.DescriptorSpec{
		ID: 2, OID: 102, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: "candidate"},
		Version: 1, State: model.StateAbsent, Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1},
		Dependencies: []model.Dependency{{Object: 1, Version: 1}},
		Index: &model.IndexMetadata{Table: 1, Keys: []model.KeyElement{{Column: 1, Type: typ}}, Unique: true, DeferredUnique: true, PendingConstraint: pending, PendingDependencies: []model.Dependency{{Object: 30, Version: 2}}},
	}
	descriptor, err := model.NewDescriptor(t.Context(), spec, descriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	pending.Name = "changed"
	spec.Index.PendingDependencies[0].Object = 99
	owned, err := descriptor.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if owned.Index.PendingConstraint.Name != "candidate" || owned.Index.PendingDependencies[0].Object != 30 {
		t.Fatal("pending constraint lifecycle metadata aliases caller memory")
	}
}
func TestDescriptorBoundsCyclicDefaultBeforeOwnership(t *testing.T) {
	spec := descriptorSpec()
	value := model.Value{Type: model.TypeComposite, ArrayVal: make([]model.Value, 1)}
	value.ArrayVal[0] = value
	spec.Table.Columns[0].ConstantDefault = &value
	if _, err := model.NewDescriptor(t.Context(), spec, descriptorLimits()); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("cyclic default accepted: %v", err)
	}
	limits := descriptorLimits()
	limits.MaxItems = 1
	if _, err := model.NewDescriptor(t.Context(), descriptorSpec(), limits); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("column count bound: %v", err)
	}
	limits = descriptorLimits()
	limits.MaxBytes = 1
	if _, err := model.NewDescriptor(t.Context(), descriptorSpec(), limits); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("metadata byte bound: %v", err)
	}
}

func TestDescriptorCopyUsesReceivingOwnersLimits(t *testing.T) {
	spec := descriptorSpec()
	spec.Table.Columns[0].Default = &model.ExpressionDefinition{SQL: strings.Repeat("1+", 4096) + "1", SemanticVersion: 1, Encoding: []byte{1}}
	spec.Table.Columns[0].DefaultOID = 1001
	descriptor, err := model.NewDescriptor(t.Context(), spec, descriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	receiving := descriptorLimits()
	receiving.MaxBytes = 4096
	partial, err := descriptor.SpecWithin(t.Context(), receiving)
	if !errors.Is(err, model.ErrDescriptorLimit) || partial.ID != 0 || partial.Table != nil {
		t.Fatalf("oversized descriptor copy escaped receiving limit: id=%d table=%v err=%v", partial.ID, partial.Table, err)
	}
	receiving = descriptorLimits()
	receiving.MaxItems = 1
	if _, err := descriptor.SpecWithin(t.Context(), receiving); !errors.Is(err, model.ErrDescriptorLimit) {
		t.Fatalf("receiver column count limit ignored: %v", err)
	}
	owned, err := descriptor.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owned.Table.Columns[0].Default.Encoding[0] = 9
	unchanged, err := descriptor.SpecWithin(t.Context(), descriptorLimits())
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Table.Columns[0].Default.Encoding[0] != 1 {
		t.Fatal("receiving copy aliases immutable descriptor")
	}
}
