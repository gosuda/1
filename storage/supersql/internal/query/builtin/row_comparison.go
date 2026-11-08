package builtin

import (
	"iter"
	"sort"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// RowComparisonInfo is a B-tree operator interpretation, including cross-type
// family members. CompareFunction is zero when support procedure 1 is absent.
// Strategies are PostgreSQL's <, <=, =, >=, >, <> numbers 1 through 6.
type RowComparisonInfo struct {
	Family          model.OID
	Strategy        uint8
	Left, Right     model.TypeID
	CompareFunction model.FunctionID
}

type rowComparisonEntry struct {
	operator model.OperatorID
	RowComparisonInfo
}

// RowComparisonInterpretations yields immutable metadata by value, ordered by
// strategy, family and input types. Unknown operators have no interpretations.
func RowComparisonInterpretations(operator model.OperatorID) iter.Seq[RowComparisonInfo] {
	return func(yield func(RowComparisonInfo) bool) {
		first := sort.Search(len(rowComparisonCatalog), func(i int) bool { return rowComparisonCatalog[i].operator >= operator })
		for _, entry := range rowComparisonCatalog[first:] {
			if entry.operator != operator || !yield(entry.RowComparisonInfo) {
				return
			}
		}
	}
}
