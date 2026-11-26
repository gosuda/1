package bind

import "github.com/gosuda/supersql-alpha/internal/zpgparse"

func (b *binder) indirection(n *zpgparse.A_Indirection, s *scope) (expression, error) {
	if n.Arg == nil || len(n.Indirection) == 0 {
		return expression{}, bindError("42601", "invalid indirection", 0)
	}
	for _, node := range n.Indirection {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		if node.GetAStar() != nil {
			return expression{}, bindError("0A000", "row expansion via * is not supported here", 0)
		}
	}
	source, err := b.scalar(n.Arg, s)
	if err != nil {
		return expression{}, err
	}
	for start := 0; start < len(n.Indirection); {
		if err := b.ctx.Err(); err != nil {
			return expression{}, err
		}
		node := n.Indirection[start]
		if field := node.GetString_(); field != nil {
			source, err = b.compositeField(s, source, field.Sval, source.position)
			start++
		} else if node.GetAIndices() != nil {
			end := start + 1
			for end < len(n.Indirection) && n.Indirection[end].GetAIndices() != nil {
				end++
			}
			source, err = b.arraySubscript(source, n.Indirection[start:end], s)
			start = end
		} else {
			return expression{}, bindError("42601", "invalid indirection", source.position)
		}
		if err != nil {
			return expression{}, err
		}
	}
	return source, nil
}
