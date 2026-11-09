package modify_test

import "testing"

func TestJSONBPrimaryKeysUseSQLNumericEqualityAndContainerOrder(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE json_keys(key jsonb PRIMARY KEY, label bigint)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO json_keys VALUES('1.0',1),('[]',2),('null',3),('{"a":1}',4) RETURNING label`, [][]int64{{1}, {2}, {3}, {4}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM json_keys ORDER BY key`, [][]int64{{2}, {3}, {1}, {4}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO json_keys VALUES('1.00',5)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO json_keys VALUES('{"a":1.00}',5)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM json_keys WHERE key='1.00'`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT count(*) FROM json_keys`, [][]int64{{4}})
}

func TestJSONBSubscriptAssignmentsAcrossUpdateAndMergeInsert(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE json_subscripts(id bigint PRIMARY KEY, payload jsonb DEFAULT '{}'::jsonb)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO json_subscripts VALUES(1,'{"a":{"b":1}}'),(4,NULL),(5,'[0]'),(6,'{"a":1}'),(7,NULL),(8,NULL),(9,'{}'),(10,'{"0":"old"}') RETURNING id`, [][]int64{{1}, {4}, {5}, {6}, {7}, {8}, {9}, {10}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO json_subscripts(id,payload['a'][0]['b']) VALUES(3,'1'::jsonb) RETURNING CASE WHEN payload='{"a":[{"b":1}]}'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['a']['b']='9'::jsonb WHERE id=1 RETURNING CASE WHEN payload['a']['b']='9'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['a']['b']=NULL WHERE id=1 RETURNING CASE WHEN payload IS NOT NULL AND payload['a']['b']='null'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['a']='1'::jsonb WHERE id=4 RETURNING CASE WHEN payload='{"a":1}'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload[2]='2'::jsonb WHERE id=5 RETURNING CASE WHEN payload='[0,null,2]'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload[-1]='3'::jsonb WHERE id=5 RETURNING CASE WHEN payload='[0,null,3]'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['0']='4'::jsonb WHERE id=5 RETURNING CASE WHEN payload='[4,null,3]'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['0']='1'::jsonb WHERE id=7 RETURNING CASE WHEN payload='{"0":1}'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload[0]='1'::jsonb WHERE id=8 RETURNING CASE WHEN payload='[1]'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload[0]='"new"'::jsonb WHERE id=10 RETURNING CASE WHEN payload='{"0":"new"}'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE json_subscripts SET payload['a']['0']['b']='1'::jsonb WHERE id=9 RETURNING CASE WHEN payload='{"a":[{"b":1}]}'::jsonb THEN 1 ELSE 0 END`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `MERGE INTO json_subscripts t USING (VALUES(2)) s(id) ON t.id=s.id WHEN NOT MATCHED THEN INSERT(id,payload['a'][0]['b'],payload['c']) VALUES(s.id,'1'::jsonb,'2'::jsonb)`, nil)
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT CASE WHEN payload='{"a":[{"b":1}],"c":2}'::jsonb THEN 1 ELSE 0 END FROM json_subscripts WHERE id=2`, [][]int64{{1}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `UPDATE json_subscripts SET payload['a']['b']='2'::jsonb WHERE id=6`, table.Mutations)
	requireUniqueSQLState(t, err, "22023")
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `UPDATE json_subscripts SET payload[-4]='2'::jsonb WHERE id=5`, table.Mutations)
	requireUniqueSQLState(t, err, "22023")
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `UPDATE json_subscripts SET payload[NULL::text]='2'::jsonb WHERE id=5`, table.Mutations)
	requireUniqueSQLState(t, err, "22004")
}
