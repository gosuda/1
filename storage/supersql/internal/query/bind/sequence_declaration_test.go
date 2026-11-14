package bind_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func sequenceDeclaration(t *testing.T, ctx context.Context, sql string, snapshot metadataSnapshot, identities *declarationIdentities) (*bind.BoundSequenceDefinition, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bind.CreateDeclarationOptions{Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}, Identities: identities, Owner: (model.Principal{}).OID(), Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}}
	if statement := parsed.Stmts[0].Stmt.GetCreateSeqStmt(); statement != nil {
		return bind.BindCreateSequence(ctx, statement, snapshot, options)
	}
	return bind.BindAlterSequence(ctx, parsed.Stmts[0].Stmt.GetAlterSeqStmt(), snapshot, options)
}

func TestSequenceDeclarationDefersAllocationIncarnation(t *testing.T) {
	bound, err := sequenceDeclaration(t, t.Context(), `CREATE SEQUENCE descending AS smallint INCREMENT -2 CACHE 5 CYCLE OWNED BY public."Mixed"."X"`, tableSnapshot(t), &declarationIdentities{next: 100})
	if err != nil {
		t.Fatal(err)
	}
	config := bound.Configuration()
	if config.Identity != 0 || config.Incarnation != 0 {
		t.Fatalf("binding supplied uncommitted allocation identity: %+v", config)
	}
	if config.Min != math.MinInt16 || config.Max != -1 || config.Start != -1 {
		t.Fatalf("descending range = %+v", config)
	}
	if config.Cache != 5 || !config.Cycle {
		t.Fatalf("sequence cache/cycle = %+v", config)
	}
	if config.OwnerTable != 11 || config.OwnerColumn != 1 {
		t.Fatalf("ownership = %+v", config)
	}
	if err := model.ValidateSequenceConfiguration(config); err != nil {
		t.Fatal(err)
	}
	if _, err := bound.Finalize(t.Context(), 0); err == nil {
		t.Fatal("accepted missing committed incarnation")
	}
	descriptor, err := bound.Finalize(t.Context(), 900)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := descriptor.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != 101 || spec.OID != 100101 || spec.Sequence.Identity != 900 || spec.Sequence.Incarnation != 900 {
		t.Fatalf("finalized identity = %+v", spec)
	}
	found := false
	for _, dep := range bound.Dependencies() {
		if dep.Object == 11 && dep.Version == 3 && dep.Column == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing owning column dependency")
	}
	config.Start = -20
	if bound.Configuration().Start != -1 {
		t.Fatal("configuration accessor leaked mutable draft")
	}
}

func sequenceSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	bound, err := sequenceDeclaration(t, t.Context(), `CREATE SEQUENCE seq AS smallint START 7 OWNED BY "Mixed"."X"`, snapshot, &declarationIdentities{next: 100})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := bound.Finalize(t.Context(), 900)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.descriptors = append(snapshot.descriptors, descriptor)
	return snapshot
}

func TestAlterSequencePreservesIdentityAndSeparatesRestart(t *testing.T) {
	snapshot := sequenceSnapshot(t)
	bound, err := sequenceDeclaration(t, t.Context(), `ALTER SEQUENCE seq AS integer START 12 OWNED BY NONE`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, restart := bound.Restart(); restart {
		t.Fatal("START restarted allocation")
	}
	if bound.Configuration().Max != math.MaxInt32 || bound.Configuration().Start != 12 || bound.Configuration().OwnerTable != 0 {
		t.Fatalf("alter configuration = %+v", bound.Configuration())
	}
	next, err := bound.Finalize(t.Context(), 901)
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.Base().ValidateSuccessor(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	spec, err := next.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range spec.Dependencies {
		if dep.Object == 11 && dep.Column == 1 {
			t.Fatal("OWNED BY NONE retained owning-column dependency")
		}
	}
	for _, sql := range []string{`ALTER SEQUENCE seq START 12 RESTART`, `ALTER SEQUENCE seq RESTART 12`} {
		bound, err := sequenceDeclaration(t, t.Context(), sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := bound.Restart(); !ok || value != 12 {
			t.Fatalf("%s restart = %d, %v", sql, value, ok)
		}
	}
}

func TestSequenceDeclarationRejectsInvalidOptions(t *testing.T) {
	for _, test := range []struct{ name, sql, code string }{
		{"zero increment", `CREATE SEQUENCE s INCREMENT 0`, "22023"},
		{"zero cache", `CREATE SEQUENCE s CACHE 0`, "22023"},
		{"empty range", `CREATE SEQUENCE s MINVALUE 3 MAXVALUE 3 START 3`, "22023"},
		{"smallint overflow", `CREATE SEQUENCE s AS smallint MAXVALUE 32768`, "22023"},
		{"bigint overflow", `CREATE SEQUENCE s START 9223372036854775808`, "22003"},
		{"fractional start", `CREATE SEQUENCE s START 1.5`, "22003"},
		{"noninteger type", `CREATE SEQUENCE s AS numeric`, "22023"},
		{"duplicate cycle", `CREATE SEQUENCE s CYCLE NO CYCLE`, "42601"},
		{"duplicate min", `CREATE SEQUENCE s MINVALUE 1 NO MINVALUE`, "42601"},
		{"restart outside range", `CREATE SEQUENCE s MAXVALUE 10 RESTART 11`, "22023"},
		{"missing column", `CREATE SEQUENCE s OWNED BY "Mixed".absent`, "42703"},
		{"wrong alter kind", `ALTER SEQUENCE "Mixed" RESTART`, "42809"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := sequenceDeclaration(t, t.Context(), test.sql, tableSnapshot(t), &declarationIdentities{next: 100})
			var sqlError *model.Error
			if !errors.As(err, &sqlError) || sqlError.Code != test.code {
				t.Fatalf("error = %v, want SQLSTATE %s", err, test.code)
			}
		})
	}
}

func TestSequenceDeclarationExistenceSkipsWithoutAllocation(t *testing.T) {
	identities := &declarationIdentities{next: 100}
	bound, err := sequenceDeclaration(t, t.Context(), `CREATE SEQUENCE IF NOT EXISTS "Mixed" INCREMENT 0`, tableSnapshot(t), identities)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Existing() == nil || bound.Existing().ID() != 11 || identities.next != 100 {
		t.Fatal("existing relation was not preserved")
	}
	if descriptor, err := bound.Finalize(t.Context(), 0); descriptor != nil || err != nil {
		t.Fatalf("skip finalization = %v, %v", descriptor, err)
	}
	bound, err = sequenceDeclaration(t, t.Context(), `ALTER SEQUENCE IF EXISTS absent RESTART`, tableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Base() != nil || len(bound.Warnings()) != 1 {
		t.Fatal("missing sequence was not skipped with notice")
	}
}

func TestSequenceAlterTypeExtremaAndExplicitBounds(t *testing.T) {
	for _, test := range []struct {
		sql      string
		min, max int64
	}{
		{`ALTER SEQUENCE seq AS integer INCREMENT -1`, 1, math.MaxInt32},
		{`ALTER SEQUENCE seq AS integer INCREMENT -1 NO MINVALUE NO MAXVALUE`, math.MinInt32, math.MaxInt32},
		{`ALTER SEQUENCE seq AS integer MINVALUE 2 MAXVALUE 100`, 2, 100},
	} {
		bound, err := sequenceDeclaration(t, t.Context(), test.sql, sequenceSnapshot(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := bound.Configuration(); got.Min != test.min || got.Max != test.max {
			t.Fatalf("%s bounds = %d..%d", test.sql, got.Min, got.Max)
		}
	}
}

func TestSequenceBindingCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sequenceDeclaration(t, ctx, `CREATE SEQUENCE seq`, tableSnapshot(t), &declarationIdentities{next: 100})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled binding = %v", err)
	}
}

func TestSequenceAlterPreservesCustomBounds(t *testing.T) {
	snapshot := tableSnapshot(t)
	created, err := sequenceDeclaration(t, t.Context(), `CREATE SEQUENCE custom AS integer MINVALUE -40000 MAXVALUE 40000 START 1`, snapshot, &declarationIdentities{next: 100})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := created.Finalize(t.Context(), 900)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.descriptors = append(snapshot.descriptors, descriptor)
	altered, err := sequenceDeclaration(t, t.Context(), `ALTER SEQUENCE custom AS bigint`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := altered.Configuration(); got.Min != -40000 || got.Max != 40000 {
		t.Fatalf("custom bounds changed: %+v", got)
	}
	_, err = sequenceDeclaration(t, t.Context(), `ALTER SEQUENCE custom AS smallint`, snapshot, nil)
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "22023" {
		t.Fatalf("narrowing custom bounds = %v", err)
	}
}

func TestSequenceDeclarationDoesNotIgnoreUnsupportedClauses(t *testing.T) {
	for _, sql := range []string{`CREATE TEMP SEQUENCE s`, `CREATE UNLOGGED SEQUENCE s`, `CREATE SEQUENCE s SEQUENCE NAME other`, `CREATE SEQUENCE s LOGGED`} {
		_, err := sequenceDeclaration(t, t.Context(), sql, tableSnapshot(t), &declarationIdentities{next: 100})
		if err == nil {
			t.Fatalf("ignored unsupported clause: %s", sql)
		}
	}
}
