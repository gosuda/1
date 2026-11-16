package neosepia

import (
	"context"
	"errors"
)

func (p *predicateProgram) zoneMayMatch(ctx context.Context, block *paxBlock, index uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p.trivialTrue {
		return true, nil
	}
	node := &p.nodes[index]
	switch node.kind {
	case predBoolConst:
		return !node.bnull && node.bval, nil
	case predAnd, predOr:
		left, err := p.zoneMayMatch(ctx, block, node.a)
		if err != nil || (node.kind == predAnd && !left) || (node.kind == predOr && left) {
			return left, err
		}
		return p.zoneMayMatch(ctx, block, node.b)
	case predCompare:
		if node.family != predInt && node.family != predTimestamp {
			return true, nil
		}
		column, constant, op := node.left, node.right, node.op
		if column.col < 0 {
			column, constant = constant, column
			switch op {
			case predLt:
				op = predGt
			case predLe:
				op = predGe
			case predGt:
				op = predLt
			case predGe:
				op = predLe
			}
		}
		if column.col < 0 || constant.cnst < 0 {
			return true, nil
		}
		value := p.consts[constant.cnst]
		if value.IsNull {
			return false, nil
		}
		zone, err := block.Zone(p.cols[column.col])
		if errors.Is(err, errColumnMissing) {
			return true, nil
		}
		if err != nil || !zone.Valid {
			return true, err
		}
		switch op {
		case predEq:
			return value.IntVal >= zone.Min && value.IntVal <= zone.Max, nil
		case predNe:
			return zone.Min != value.IntVal || zone.Max != value.IntVal, nil
		case predLt:
			return zone.Min < value.IntVal, nil
		case predLe:
			return zone.Min <= value.IntVal, nil
		case predGt:
			return zone.Max > value.IntVal, nil
		case predGe:
			return zone.Max >= value.IntVal, nil
		}
	}
	return true, nil
}
