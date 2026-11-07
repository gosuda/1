package modify_test

import (
	"testing"
	"time"
)

func TestRangeKeysEnforceCanonicalUniquenessAndRollback(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE range_keys(k numrange PRIMARY KEY,m int4multirange UNIQUE,label bigint)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO range_keys VALUES('[1.0,3.00)','{[1,2),[2,3)}',1),('empty','{}',2),('(,)','{[5,6)}',3) RETURNING label`, [][]int64{{1}, {2}, {3}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM range_keys ORDER BY k`, [][]int64{{2}, {3}, {1}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO range_keys VALUES('[10,11)','{[8,9)}',4),('[1.00,3.0)','{[9,10)}',5)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO range_keys VALUES('[20,21)','{[1,3)}',6)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM range_keys WHERE k='[1.000,3.000)'::numrange`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT count(*) FROM range_keys`, [][]int64{{3}})
}

func TestMultirangeTransitionRejectsScalarInvocation(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	_, err := runDeclarationQuery(t, fixture, sequences, dependencies, `SELECT multirange_intersect_agg_transfn('{[1,4)}'::int4multirange,'{[2,5)}'::int4multirange)`, nil)
	requireUniqueSQLState(t, err, "XX000")
}

func TestPersistedTemporalRangeLiteralKeepsDefinitionTime(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	start := time.Date(2021, 11, 7, 5, 30, 12, 123456000, time.UTC)
	frozen := temporalClockAt(fixture, start)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE temporal_ranges(id bigint,r tstzrange DEFAULT '[now,now]',g tstzrange GENERATED ALWAYS AS ('[now,now]'::tstzrange) STORED)`)
	if err != nil {
		t.Fatal(err)
	}
	temporalClockAt(fixture, start.Add(10*time.Second))
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO temporal_ranges(id) VALUES(1) RETURNING lower(r),lower(g)`, [][]int64{{frozen, frozen}})
}
