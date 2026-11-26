package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLBinaryEncodingMasksMalformedInput(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN k=1 THEN decode(s,'hex') ELSE decode(s,'base64') END,
 encode(decode('AAH/','base64'),'escape'),encode(decode('00 ff','hex'),'base64')
 FROM (VALUES(1,'00ff'::text),(2,'AP8='::text),(3,NULL::text)) t(k,s) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, b := model.StringVal, model.BytesVal
	assertAnyValueRows(t, portal, []model.TypeID{17, 25, 25}, [][]model.Value{
		{b([]byte{0, 255}), s("\\000\x01\\377"), s("AP8=")},
		{b([]byte{0, 255}), s("\\000\x01\\377"), s("AP8=")},
		{model.NullVal(model.TypeBytes), s("\\000\x01\\377"), s("AP8=")},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLDigestsComposeWithBinaryEncoding(t *testing.T) {
	graph := sqlGraph(t, `SELECT md5(s),md5(decode('616263','hex')),encode(sha256(decode('616263','hex')),'hex'),
 octet_length(sha224(decode('616263','hex'))),octet_length(sha384(decode('616263','hex'))),octet_length(sha512(decode('616263','hex')))
 FROM (VALUES('abc'::text)) t(s)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	s, i := model.StringVal, model.Int64Val
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 23, 23, 23}, [][]model.Value{{
		s("900150983cd24fb0d6963f7d28e17f72"), s("900150983cd24fb0d6963f7d28e17f72"),
		s("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"), i(28), i(48), i(64),
	}})
	aggregateProjectionReleased(t, env.Query)
}
