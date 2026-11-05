package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUUIDCanonicalGroupingAndOrdering(t *testing.T) {
	graph := sqlGraph(t, `SELECT u::text,count(*) FROM (VALUES('{A0EEBC999C0B4EF8BB6D6BB9BD380A11}'::uuid),('a0ee-bc99-9c0b-4ef8-bb6d-6bb9-bd38-0a11'::uuid),('00000000-0000-0000-0000-000000000000'::uuid),(NULL::uuid))t(u) GROUP BY u ORDER BY u NULLS LAST`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("00000000-0000-0000-0000-000000000000"), model.Int64Val(1)}, {model.StringVal("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"), model.Int64Val(2)}, {model.NullVal(model.TypeString), model.Int64Val(1)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUUIDArrayDependenciesAndVersion(t *testing.T) {
	graph := sqlGraph(t, `SELECT array_position(a,u),width_bucket(u,a),array_to_string(a,','),uuid_extract_version(u),encode(uuid_send(u),'hex') FROM (VALUES('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid,'{00000000-0000-0000-0000-000000000000,a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11}'::uuid[]))t(u,a)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 25, 21, 25}, [][]model.Value{{model.Int64Val(2), model.Int64Val(2), model.StringVal("00000000-0000-0000-0000-000000000000,a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"), model.Int64Val(4), model.StringVal("a0eebc999c0b4ef8bb6d6bb9bd380a11")}})
	aggregateProjectionReleased(t, env.Query)
}
