package expr

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestVerifyRejectsInvalidCacheFlow(t *testing.T) {
	for _, defect := range []string{"ordinary-write", "ordinary-read", "recursive", "unbalanced", "forward", "early-read", "wrong-cache", "wrong-type", "wrong-nullability", "filtered-cache"} {
		t.Run(defect, func(t *testing.T) {
			integer := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
			p := &Program{
				limits: DefaultLimits(), registers: []register{{layout: integer}, {layout: integer, once: true}, {layout: integer}},
				constants: []model.Value{model.Int64Val(1)}, outputs: []int{0},
				code: []instruction{
					{op: opInit, dst: 0},
					{op: opOnceEnter, index: 1},
					{op: opInit, dst: 2},
					{op: opConst, dst: 2, index: 0},
					{op: opOnceStore, index: 1, args: []int{2}},
					{op: opOnceRead, dst: 0, index: 1},
				},
			}
			if err := p.Verify(); err != nil {
				t.Fatalf("valid cache program: %v", err)
			}
			switch defect {
			case "filtered-cache":
				p.code[3] = instruction{op: opFilter, mask: maskNull, args: []int{2}}
			case "ordinary-write":
				p.code[2] = instruction{op: opInit, dst: 1}
			case "ordinary-read":
				p.code[3] = instruction{op: opCopy, dst: 2, args: []int{1}}
			case "recursive":
				p.code[2] = instruction{op: opOnceEnter, index: 1}
			case "unbalanced":
				p.code[4] = instruction{op: opLeave}
			case "forward":
				p.code[4].args[0] = 3
			case "early-read":
				p.code[4] = instruction{op: opOnceRead, dst: 0, index: 1}
			case "wrong-cache":
				p.code[5].index = 2
			case "wrong-type":
				p.registers[1].layout = model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}}
			case "wrong-nullability":
				p.registers[1].nullable = true
			}
			if err := p.Verify(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid cache flow accepted: %v", err)
			}
		})
	}
}
