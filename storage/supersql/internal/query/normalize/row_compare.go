package normalize

import "github.com/gosuda/supersql-alpha/internal/query/ir"

func (n *normalizer) rowCompare(id ir.ScalarID, source, meta ir.Scalar, depth uint32) scalarResult {
	if depth+1 >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return scalarResult{}
	}
	pairs := allocate[[2]scalarResult](n, uint64(source.OperandCount()))
	comparators := allocate[scalarResult](n, uint64(source.OperandCount()))
	// PostgreSQL's generic RowCompareExpr mutator visits largs, then rargs.
	for side := range 2 {
		for i := range pairs {
			call, _ := n.g.Operand(id, uint32(i))
			input, _ := n.g.Operand(call, uint32(side))
			pairs[i][side] = n.scalar(input, depth+2)
			if n.err != nil {
				return scalarResult{}
			}
		}
	}
	for i := range comparators {
		if !n.step(1) {
			return scalarResult{}
		}
		comparatorID, _ := n.g.Operand(id, uint32(i))
		call, _ := n.g.Scalar(comparatorID)
		call = call.Metadata()
		call.Layout = n.layouts[call.Layout]
		// Support callbacks aren't SQL subexpressions. Keep this private copy out
		// of the scalar memo so a standalone use still undergoes normal folding.
		comparators[i] = n.addScalar(call, pairs[i][:])
		if n.err != nil {
			return scalarResult{}
		}
	}
	return n.addScalar(meta, comparators)
}
