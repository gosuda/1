package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func rowLockSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	schema := tableSnapshot(t).descriptors[0]
	table := func(id model.ObjectID, name string) (*model.Descriptor, *model.Descriptor) {
		key := model.KeyElement{Column: 1, Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
		indexID := id + 200
		column := model.ColumnDescriptor{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: key.Type}}
		primary := model.ConstraintDescriptor{
			ID: id + 100, OID: model.OID(30000 + id), Name: name + "_pkey", Kind: model.ConstraintPrimaryKey,
			Columns: []model.ColumnID{column.ID}, Index: indexID, Validated: true, Enforced: true, Local: true,
		}
		base := testDescriptor(t, model.DescriptorSpec{
			ID: id, OID: model.OID(10000 + id), Version: 1, State: model.StatePublic,
			Name:     model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: name},
			Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
			Table: &model.TableMetadata{
				ColumnIDHighWatermark: column.ID, Columns: []model.ColumnDescriptor{column},
				PrimaryKey: []model.KeyElement{key}, Indexes: []model.ObjectID{indexID}, Constraints: []model.ConstraintDescriptor{primary},
			},
		})
		index := testDescriptor(t, model.DescriptorSpec{
			ID: indexID, OID: model.OID(40000 + id), Version: 1, State: model.StatePublic,
			Name:         model.NamespaceName{Parent: 10, Kind: model.ObjectIndex, Name: name + "_pkey"},
			Physical:     model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
			Dependencies: []model.Dependency{{Object: id, Version: 1}, {Object: id, Version: 1, Column: column.ID}},
			Index:        &model.IndexMetadata{Owner: 1, Table: id, Keys: []model.KeyElement{key}, Unique: true},
		})
		return base, index
	}
	first, firstIndex := table(11, "first")
	second, secondIndex := table(12, "second")
	return metadataSnapshot{descriptors: []*model.Descriptor{schema, first, firstIndex, second, secondIndex}}
}

func statementRowLocks(t *testing.T, statement *bind.BoundStatement) ([]ir.RowLock, map[model.ObjectID][]model.ScanOccurrence) {
	t.Helper()
	graph := statement.Graph()
	var locks []ir.RowLock
	scans := make(map[model.ObjectID][]model.ScanOccurrence)
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		relation, ok := graph.Rel(id)
		if !ok {
			continue
		}
		if relation.Kind == ir.Scan {
			scans[relation.Object] = append(scans[relation.Object], relation.Occurrence)
		}
		if relation.Kind != ir.LockRows {
			continue
		}
		for index := range relation.LockCount() {
			lock, ok := graph.RowLock(id, index)
			if !ok {
				t.Fatal("missing row-lock metadata")
			}
			locks = append(locks, lock)
		}
	}
	return locks, scans
}

func TestBindRowLockOfTargetsOneJoinedTableOccurrence(t *testing.T) {
	statement, err := bindSQL(t, `SELECT a.id, b.id FROM first a JOIN second b ON a.id=b.id FOR UPDATE OF a SKIP LOCKED`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans := statementRowLocks(t, statement)
	if len(scans[11]) != 1 || len(scans[12]) != 1 || scans[11][0] == 0 || scans[12][0] == 0 || scans[11][0] == scans[12][0] {
		t.Fatalf("scan occurrences = %#v", scans)
	}
	if len(locks) != 1 || locks[0].Object != 11 || locks[0].Occurrence != scans[11][0] || locks[0].Strength != ir.UpdateLock || locks[0].Wait != ir.SkipLocked {
		t.Fatalf("row locks = %#v", locks)
	}
}

func TestBindRowLockDistinguishesAliasesOfSameTable(t *testing.T) {
	statement, err := bindSQL(t, `SELECT a.id, b.id FROM first a JOIN first b ON a.id=b.id FOR KEY SHARE OF a NOWAIT`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans := statementRowLocks(t, statement)
	if len(scans[11]) != 2 || scans[11][0] == 0 || scans[11][1] == 0 || scans[11][0] == scans[11][1] {
		t.Fatalf("self-join scan occurrences = %#v", scans[11])
	}
	if len(locks) != 1 || locks[0].Occurrence != scans[11][0] || locks[0].Strength != ir.KeyShare || locks[0].Wait != ir.NoWait {
		t.Fatalf("self-join row locks = %#v", locks)
	}
}

func TestBindRowLockMergesStrengthAndPostgresWaitPrecedencePerTarget(t *testing.T) {
	statement, err := bindSQL(t, `SELECT a.id, b.id FROM first a JOIN second b ON a.id=b.id FOR KEY SHARE OF a SKIP LOCKED FOR UPDATE OF a NOWAIT FOR SHARE OF b`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans := statementRowLocks(t, statement)
	if len(locks) != 2 {
		t.Fatalf("row locks = %#v", locks)
	}
	byObject := make(map[model.ObjectID]ir.RowLock, len(locks))
	for _, lock := range locks {
		byObject[lock.Object] = lock
	}
	if lock := byObject[11]; lock.Occurrence != scans[11][0] || lock.Strength != ir.UpdateLock || lock.Wait != ir.NoWait {
		t.Fatalf("first policy = %#v", lock)
	}
	if lock := byObject[12]; lock.Occurrence != scans[12][0] || lock.Strength != ir.Share || lock.Wait != ir.Wait {
		t.Fatalf("second policy = %#v", lock)
	}
}

func TestBindRowLockPropagatesThroughNestedAndAliasedRelations(t *testing.T) {
	statement, err := bindSQL(t, `SELECT q.id FROM (SELECT id FROM first) q FOR NO KEY UPDATE OF q SKIP LOCKED`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans := statementRowLocks(t, statement)
	if len(locks) != 1 || locks[0].Object != 11 || locks[0].Occurrence != scans[11][0] || locks[0].Strength != ir.NoKeyUpdate || locks[0].Wait != ir.SkipLocked {
		t.Fatalf("nested row locks = %#v scans=%#v", locks, scans)
	}

	statement, err = bindSQL(t, `SELECT q.id FROM (SELECT id FROM first FOR KEY SHARE NOWAIT) q FOR UPDATE OF q SKIP LOCKED`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans = statementRowLocks(t, statement)
	if len(locks) != 1 || locks[0].Occurrence != scans[11][0] || locks[0].Strength != ir.UpdateLock || locks[0].Wait != ir.NoWait {
		t.Fatalf("nested merged row locks = %#v scans=%#v", locks, scans)
	}

	statement, err = bindSQL(t, `SELECT 1 FROM (first a JOIN second b ON a.id=b.id) j FOR SHARE`, rowLockSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	locks, scans = statementRowLocks(t, statement)
	if len(locks) != 2 || locks[0].Occurrence == locks[1].Occurrence || locks[0].Wait != ir.Wait || locks[1].Wait != ir.Wait {
		t.Fatalf("aliased join row locks = %#v scans=%#v", locks, scans)
	}
}

func TestBindRowLockRejectsNullableOuterJoinTarget(t *testing.T) {
	_, err := bindSQL(t, `SELECT 1 FROM first a LEFT JOIN second b ON a.id=b.id FOR UPDATE OF b`, rowLockSnapshot(t), nil)
	requireCode(t, err, "0A000")
}
