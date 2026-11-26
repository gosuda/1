package bind

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestPrepareRejectsCopyStatement(t *testing.T) {
	copyNode := &zpgparse.Node{Node: &zpgparse.Node_CopyStmt{CopyStmt: &zpgparse.CopyStmt{IsFrom: true}}}
	prepareNode := &zpgparse.Node{Node: &zpgparse.Node_PrepareStmt{PrepareStmt: &zpgparse.PrepareStmt{Name: "p", Query: copyNode}}}
	command, _, err := (&binder{}).command(prepareNode)
	var sqlErr *model.Error
	if command != nil || !errors.As(err, &sqlErr) || sqlErr.Code != "42601" {
		t.Fatalf("PREPARE COPY admission = %#v, %v", command, err)
	}
}
