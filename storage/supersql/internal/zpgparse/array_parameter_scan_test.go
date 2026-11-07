package zpgparse_test

import (
	"testing"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestArraySliceParametersAreNotDollarQuoteTags(t *testing.T) {
	parsed, err := pg_query.Parse(`SELECT a[$1:$2], $3[$4:]`)
	if err != nil {
		t.Fatal(err)
	}
	targets := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList
	first := targets[0].GetResTarget().Val.GetAIndirection().Indirection[0].GetAIndices()
	if first.Lidx.GetParamRef().Number != 1 || first.Uidx.GetParamRef().Number != 2 {
		t.Fatalf("first slice bounds = %+v", first)
	}
	second := targets[1].GetResTarget().Val.GetAIndirection()
	bound := second.Indirection[0].GetAIndices()
	if second.Arg.GetParamRef().Number != 3 || bound.Lidx.GetParamRef().Number != 4 || bound.Uidx != nil {
		t.Fatalf("second slice = %+v", second)
	}
}

func TestDollarQuoteTagsRetainIdentifierRules(t *testing.T) {
	parsed, err := pg_query.Parse(`SELECT $tag_1$first$tag_1$, $界2$second$界2$`)
	if err != nil {
		t.Fatal(err)
	}
	targets := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList
	for i, want := range []string{"first", "second"} {
		if got := targets[i].GetResTarget().Val.GetAConst().GetSval().Sval; got != want {
			t.Fatalf("quoted value %d=%q, want %q", i, got, want)
		}
	}
}
