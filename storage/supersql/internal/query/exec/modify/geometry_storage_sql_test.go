package modify_test

import "testing"

func TestGeometricRowsPreserveInternalValuesAcrossNativeStorage(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE geometry_rows(id bigint PRIMARY KEY,p point,b box,c circle,s lseg,l line,path path,poly polygon)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO geometry_rows VALUES(1,'(1,2)',box '(1,1),(0,0)' # box '(2,1),(1.0000005,0)',circle(point(0,0),-1::float8),'[(0,0),(2,2)]','{1,-1,0}','[(0,0),(1,1)]','((0,0),(2,0),(0,2))') RETURNING id`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT CASE WHEN p ~= point '(1,2)' AND width(b)<0 AND radius(c)=-1 AND s::text='[(0,0),(2,2)]' AND l::text='{1,-1,0}' AND path::text='[(0,0),(1,1)]' AND poly::text='((0,0),(2,0),(0,2))' THEN id ELSE -id END FROM geometry_rows`, [][]int64{{1}})
}
