package exec_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUnicodeStringConversionAndMaskedErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT unistr('d\0061t\+000061'),unistr('\D801\uDC37'),CASE WHEN false THEN unistr('\0000') ELSE unistr('safe\\text') END`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 25, 25}, [][]model.Value{{model.StringVal("data"), model.StringVal("\U00010437"), model.StringVal("safe\\text")}})
	aggregateProjectionReleased(t, env.Query)
}
