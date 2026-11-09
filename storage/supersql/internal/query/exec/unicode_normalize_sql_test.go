package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUnicodeNormalizationFormsAndPredicates(t *testing.T) {
	graph := sqlGraph(t, `SELECT normalize(unistr('a\0308')),normalize(unistr('\FB00'),NFKC),normalize(unistr('\00C4'),NFD),unistr('a\0308') IS NFC NORMALIZED,unistr('\00E4') IS NFD NORMALIZED,unicode_version(),unicode_assigned(unistr('\+0105C0'))`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25, 16, 16, 25, 16}, [][]model.Value{{model.StringVal("\u00e4"), model.StringVal("ff"), model.StringVal("A\u0308"), model.BoolVal(false), model.BoolVal(false), model.StringVal("16.0"), model.BoolVal(true)}})
	aggregateProjectionReleased(t, env.Query)
}
