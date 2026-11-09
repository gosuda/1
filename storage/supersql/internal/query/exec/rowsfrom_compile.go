package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exrowsfrom "github.com/gosuda/supersql-alpha/internal/query/exec/rowsfrom"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) rowsFrom(node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor, ordinality bool) (storage.BatchCursor, error) {
	if c.preparing {
		return nil, nil
	}
	if len(children) != len(node.Children) || len(children) < 2 {
		return nil, ErrInvalid
	}
	if err := c.charge(uint64(len(children)) * uint64(unsafe.Sizeof(exrowsfrom.Input{}))); err != nil {
		return nil, err
	}
	inputs := make([]exrowsfrom.Input, len(children))
	for i, id := range node.Children {
		inputs[i] = exrowsfrom.Input{Cursor: children[i], Layouts: c.prepared.nodes[id].layouts}
	}
	env := &c.portal.env
	return exrowsfrom.New(c.portal.ctx, inputs, exrowsfrom.Options{Output: layouts, Ordinality: ordinality, Query: env.Query, BatchRows: env.Limits.BatchRows, BatchBytes: env.Limits.BatchBytes})
}
