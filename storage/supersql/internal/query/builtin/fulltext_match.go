package builtin

import (
	"bytes"
	"context"
	"math"
	"sort"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type tsMatchSpan struct{ start, end int }

func tsVectorInternalSize(vector []tsLexeme) int {
	size := 8 + len(vector)*4
	data := 0
	for _, lexeme := range vector {
		data += len(lexeme.text)
		if len(lexeme.positions) != 0 {
			if data&1 != 0 { data++ }
			data += 2 + len(lexeme.positions)*2
		}
	}
	return size+data
}

func findTSLexemes(vector []tsLexeme, value *tsQueryNode) []tsLexeme {
	index := sort.Search(len(vector), func(i int) bool { return bytes.Compare([]byte(vector[i].text), []byte(value.word)) >= 0 })
	if !value.prefix {
		if index < len(vector) && vector[index].text == value.word { return vector[index:index+1] }
		return nil
	}
	end := index
	for end < len(vector) && strings.HasPrefix(vector[end].text, value.word) { end++ }
	return vector[index:end]
}

func matchTSValue(vector []tsLexeme, value *tsQueryNode) bool {
	for _, lexeme := range findTSLexemes(vector, value) {
		if value.weight == 0 { return true }
		for _, position := range lexeme.positions { if value.weight&(1<<tsPositionWeight(position)) != 0 { return true } }
	}
	return false
}

func tsValueSpans(vector []tsLexeme, value *tsQueryNode) []tsMatchSpan {
	spans := make([]tsMatchSpan, 0)
	for _, lexeme := range findTSLexemes(vector, value) {
		for _, position := range lexeme.positions {
			if value.weight == 0 || value.weight&(1<<tsPositionWeight(position)) != 0 {
				p := tsPositionNumber(position); spans = append(spans, tsMatchSpan{start:p, end:p})
			}
		}
	}
	return compactTSSpans(spans)
}

func compactTSSpans(spans []tsMatchSpan) []tsMatchSpan {
	sort.Slice(spans, func(i, j int) bool { if spans[i].start != spans[j].start { return spans[i].start < spans[j].start }; return spans[i].end < spans[j].end })
	out := spans[:0]
	for _, span := range spans { if len(out) == 0 || out[len(out)-1] != span { out = append(out, span) } }
	return out
}

func maxTSVectorPosition(vector []tsLexeme) int {
	maximum := 0
	for _, lexeme := range vector { if n := len(lexeme.positions); n != 0 { maximum = max(maximum, tsPositionNumber(lexeme.positions[n-1])) } }
	return maximum
}

func tsQuerySpans(vector []tsLexeme, node *tsQueryNode, universe int) []tsMatchSpan {
	if node == nil { return nil }
	if node.kind == tsQueryValue { return tsValueSpans(vector, node) }
	if node.op == tsQueryNot {
		excluded := tsQuerySpans(vector, node.right, universe)
		positions := make(map[int]struct{}, len(excluded))
		for _, span := range excluded { if span.start == span.end { positions[span.start] = struct{}{} } }
		out := make([]tsMatchSpan, 0, universe-len(positions))
		for position := 1; position <= universe; position++ { if _, found := positions[position]; !found { out = append(out, tsMatchSpan{start:position,end:position}) } }
		return out
	}
	left, right := tsQuerySpans(vector, node.left, universe), tsQuerySpans(vector, node.right, universe)
	switch node.op {
	case tsQueryOr:
		return compactTSSpans(append(left, right...))
	case tsQueryAnd:
		if !matchTSQueryNode(vector, node.left, universe) || !matchTSQueryNode(vector, node.right, universe) { return nil }
		return compactTSSpans(append(left, right...))
	case tsQueryPhrase:
		out := make([]tsMatchSpan, 0)
		for _, l := range left { for _, r := range right { if r.start-l.end == int(node.distance) { out = append(out, tsMatchSpan{start:l.start,end:r.end}) } } }
		return compactTSSpans(out)
	default:
		return nil
	}
}

func matchTSQueryNode(vector []tsLexeme, node *tsQueryNode, universe int) bool {
	if node == nil { return false }
	if node.kind == tsQueryValue { return matchTSValue(vector, node) }
	switch node.op {
	case tsQueryNot: return !matchTSQueryNode(vector, node.right, universe)
	case tsQueryAnd: return matchTSQueryNode(vector, node.left, universe) && matchTSQueryNode(vector, node.right, universe)
	case tsQueryOr: return matchTSQueryNode(vector, node.left, universe) || matchTSQueryNode(vector, node.right, universe)
	case tsQueryPhrase: return len(tsQuerySpans(vector, node, universe)) != 0
	default: return false
	}
}

func matchTSQuery(vector []tsLexeme, query *tsQueryNode) bool {
	if query == nil { return false }
	return matchTSQueryNode(vector, query, max(1, maxTSVectorPosition(vector)+tsMaxQueryWidth(query)))
}

func tsMaxQueryWidth(node *tsQueryNode) int {
	if node == nil || node.kind == tsQueryValue { return 1 }
	if node.op == tsQueryNot { return tsMaxQueryWidth(node.right) }
	if node.op == tsQueryPhrase { return tsMaxQueryWidth(node.left)+int(node.distance)+tsMaxQueryWidth(node.right) }
	return max(tsMaxQueryWidth(node.left), tsMaxQueryWidth(node.right))
}

func tsQueryOperands(root *tsQueryNode) []*tsQueryNode {
	values := make([]*tsQueryNode, 0)
	var visit func(*tsQueryNode)
	visit = func(node *tsQueryNode) { if node == nil { return }; if node.kind == tsQueryValue { values = append(values, node); return }; visit(node.left); visit(node.right) }
	visit(root)
	sort.Slice(values, func(i,j int) bool { return bytes.Compare([]byte(values[i].word), []byte(values[j].word)) < 0 })
	out := values[:0]
	for _, value := range values { if len(out) == 0 || out[len(out)-1].word != value.word { out = append(out, value) } }
	return out
}

func tsRankWeights(value *model.Value) ([4]float64, error) {
	weights := [4]float64{0.1,0.2,0.4,1.0}
	if value == nil { return weights, nil }
	if len(value.ArrayVal) < 4 { return weights, sqlError("2202E", "array of weight is too short") }
	for i := range 4 {
		if value.ArrayVal[i].IsNull { return weights, sqlError("22004", "array of weight must not contain nulls") }
		weight := value.ArrayVal[i].FloatVal
		if weight >= 0 { weights[i] = weight }
		if weights[i] > 1 { return weights, sqlError("22023", "weight out of range") }
	}
	return weights, nil
}

func tsVectorDocumentLength(vector []tsLexeme) int {
	length := 0
	for _, lexeme := range vector { if len(lexeme.positions) == 0 { length++ } else { length += len(lexeme.positions) } }
	return length
}

func tsWordDistance(distance int) float64 {
	if distance > 100 { return 1e-30 }
	return 1.0/(1.005+0.05*math.Exp(float64(distance)/1.5-2))
}

func tsRank(vector []tsLexeme, query *tsQueryNode, weights [4]float64, normalization int) float64 {
	if len(vector) == 0 || query == nil { return 0 }
	operands := tsQueryOperands(query)
	if query.kind == tsQueryOp && (query.op == tsQueryAnd || query.op == tsQueryPhrase) && len(operands) >= 2 {
		found := make([][]tsPosition, len(operands))
		stripped := make([]bool, len(operands))
		rank := -1.0
		for i, operand := range operands {
			for _, lexeme := range findTSLexemes(vector, operand) {
				positions := lexeme.positions
				stripped[i] = len(positions) == 0
				if stripped[i] { positions = []tsPosition{makeTSPosition(tsMaxPosition-1, 0)} }
				found[i] = positions
				for previous := 0; previous < i; previous++ {
					for _, left := range found[previous] {
						for _, right := range positions {
							distance := tsPositionNumber(left)-tsPositionNumber(right)
							if distance < 0 { distance = -distance }
							if distance == 0 && (stripped[i] || stripped[previous]) { distance = tsMaxPosition }
							if distance == 0 { continue }
							current := math.Sqrt(weights[tsPositionWeight(left)]*weights[tsPositionWeight(right)]*tsWordDistance(distance))
							if rank < 0 { rank = current } else { rank = 1-(1-rank)*(1-current) }
						}
					}
				}
			}
		}
		if rank < 0 { rank = 1e-20 }
		return normalizeTSRank(rank, vector, normalization, 0, 0)
	}
	rank := 0.0
	for _, operand := range operands {
		for _, lexeme := range findTSLexemes(vector, operand) {
			positions := lexeme.positions
			if len(positions) == 0 { positions = []tsPosition{0} }
			lexemeRank, maximum, maximumIndex := 0.0, -1.0, 0
			for i, position := range positions {
				weight := weights[tsPositionWeight(position)]
				lexemeRank += weight/float64((i+1)*(i+1))
				if weight > maximum { maximum, maximumIndex = weight, i }
			}
			rank += (maximum+lexemeRank-maximum/float64((maximumIndex+1)*(maximumIndex+1)))/1.64493406685
		}
	}
	if len(operands) != 0 { rank /= float64(len(operands)) }
	return normalizeTSRank(rank, vector, normalization, 0, 0)
}

func normalizeTSRank(rank float64, vector []tsLexeme, method, extents int, distance float64) float64 {
	length := tsVectorDocumentLength(vector)
	if method&1 != 0 && len(vector) > 0 { rank /= math.Log2(float64(length+1)) }
	if method&2 != 0 && length > 0 { rank /= float64(length) }
	if method&4 != 0 && extents > 0 && distance > 0 { rank /= float64(extents)/distance }
	if method&8 != 0 && len(vector) > 0 { rank /= float64(len(vector)) }
	if method&16 != 0 && len(vector) > 0 { rank /= math.Log2(float64(len(vector)+1)) }
	if method&32 != 0 { rank /= rank+1 }
	return rank
}

func tsRankCD(vector []tsLexeme, query *tsQueryNode, weights [4]float64, normalization int) float64 {
	if len(vector) == 0 || query == nil { return 0 }
	type occurrence struct { position int; weight int; word string }
	occurrences := make([]occurrence,0)
	for _, operand := range tsQueryOperands(query) {
		for _, lexeme := range findTSLexemes(vector, operand) { for _, position := range lexeme.positions {
			if operand.weight == 0 || operand.weight&(1<<tsPositionWeight(position)) != 0 { occurrences = append(occurrences, occurrence{tsPositionNumber(position),tsPositionWeight(position),operand.word}) }
		} }
	}
	if len(occurrences) == 0 { return 0 }
	sort.Slice(occurrences,func(i,j int) bool { if occurrences[i].position != occurrences[j].position { return occurrences[i].position < occurrences[j].position }; return occurrences[i].word < occurrences[j].word })
	needed := tsQueryValues(query); rank, previous, extents, sumDistance := 0.0, 0.0, 0, 0.0
	for start := 0; start < len(occurrences); start++ {
		seen := make(map[string]struct{},len(needed)); end := start
		for ; end < len(occurrences); end++ { seen[occurrences[end].word]=struct{}{}; if len(seen)==len(needed) { break } }
		if end == len(occurrences) { break }
		inverse := 0.0
		for i:=start;i<=end;i++ { weight:=weights[occurrences[i].weight]; if weight==0 { inverse=math.Inf(1); break }; inverse += 1/weight }
		if inverse != 0 && !math.IsInf(inverse,1) {
			noise := (occurrences[end].position-occurrences[start].position)-(end-start); if noise<0 { noise=(end-start)/2 }
			rank += float64(end-start+1)/inverse/float64(1+noise)
		}
		center:=float64(occurrences[end].position+occurrences[start].position)/2
		if extents>0 && center>previous { sumDistance += 1/(center-previous) }
		previous=center; extents++
	}
	return normalizeTSRank(rank,vector,normalization,extents,sumDistance)
}

func validateFullTextValue(ctx context.Context, id model.TypeID, value []byte) error {
	if id == 3614 { _, err := decodeTSVector(ctx,value); return err }
	if id == 3615 { _,_,err := decodeTSQuery(ctx,value); return err }
	return nil
}
