package builtin

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errJSONPathStop = errors.New("jsonpath evaluation stopped")

type jsonPathTruth uint8

const (
	jsonPathUnknown jsonPathTruth = iota
	jsonPathFalse
	jsonPathTrue
)

type jsonPathItem struct {
	document *jsonbDocument
	index    int
	kind     jsonTextKind
	text     string
	boolean  bool
	temporal model.Value
	typeID   model.TypeID
	baseID   int64
	baseRoot int
}

func jsonPathNodeItem(document *jsonbDocument, index int) jsonPathItem {
	return jsonPathItem{document: document, index: index, kind: document.nodes[index].kind}
}
func (item jsonPathItem) child(index int) jsonPathItem {
	return jsonPathItem{document: item.document, index: index, kind: item.document.nodes[index].kind, baseID: item.baseID, baseRoot: item.baseRoot}
}


func (item jsonPathItem) node() *jsonbNode {
	if item.document == nil || item.index < 0 || item.index >= len(item.document.nodes) { return nil }
	return &item.document.nodes[item.index]
}

func (item jsonPathItem) scalarText() string {
	if node := item.node(); node != nil { return node.text }
	return item.text
}

func (item jsonPathItem) isTemporal() bool { return item.typeID != 0 }

type jsonPathExecution struct {
	ctx        context.Context
	plan       *JSONPathPlan
	root       jsonPathItem
	variables  *jsonbDocument
	passing    []jsonPathVariableItem
	budget     CallBudget
	options    ResolveOptions
	useTZ      bool
	work       uint64
	workLimit  uint64
	depth      int
	nextBaseID int64
	regexp     *regexpRuntime
}

// ExecuteJSONPath evaluates one immutable plan and emits owned canonical JSONB
// text for each result.
func ExecuteJSONPath(ctx context.Context, document string, plan *JSONPathPlan, variables string, useTZ bool, budget CallBudget, options ResolveOptions, emit func(string) error) error {
	return ExecuteJSONPathTyped(ctx, document, plan, variables, nil, useTZ, budget, options, emit)
}

// ExecuteJSONPathTyped retains SQL identities for PASSING values, including the
// temporal identities used by JSONPath datetime methods and comparisons.
func ExecuteJSONPathTyped(ctx context.Context, document string, plan *JSONPathPlan, variables string, passing []JSONPathVariable, useTZ bool, budget CallBudget, options ResolveOptions, emit func(string) error) error {
	if emit == nil {
		return sqlError("22023", "invalid jsonpath result callback")
	}
	return executeJSONPathTypedItems(ctx, document, plan, variables, passing, useTZ, budget, options, func(execution *jsonPathExecution, item jsonPathItem) error {
		text, err := execution.render(item)
		if err != nil {
			return err
		}
		if int64(len(text)) > budget.OutputBytes {
			return sqlError("54000", "builtin output byte budget exceeded")
		}
		return emit(text)
	})
}

func executeJSONPathItems(ctx context.Context, document string, plan *JSONPathPlan, variables string, useTZ bool, budget CallBudget, options ResolveOptions, emit func(*jsonPathExecution, jsonPathItem) error) error {
	return executeJSONPathTypedItems(ctx, document, plan, variables, nil, useTZ, budget, options, emit)
}

func executeJSONPathTypedItems(ctx context.Context, document string, plan *JSONPathPlan, variables string, passing []JSONPathVariable, useTZ bool, budget CallBudget, options ResolveOptions, emit func(*jsonPathExecution, jsonPathItem) error) error {
	if emit == nil {
		return sqlError("22023", "invalid jsonpath execution request")
	}
	execution, resources, err := prepareJSONPathExecution(ctx, document, plan, variables, passing, useTZ, budget, options)
	if err != nil {
		return err
	}
	defer resources.release()
	return execution.evaluate(plan.expression, jsonPathItem{}, -1, func(item jsonPathItem) error { return emit(execution, item) })
}

type jsonPathExecutionResources struct {
	root      columnar.Allocation
	variable  columnar.Allocation
	passing   []columnar.Allocation
	execution *jsonPathExecution
}

func (resources *jsonPathExecutionResources) release() {
	if resources == nil {
		return
	}
	if resources.execution != nil && resources.execution.regexp != nil {
		resources.execution.regexp.close()
		resources.execution.regexp = nil
	}
	if resources.root != nil {
		resources.root.Release()
	}
	if resources.variable != nil {
		resources.variable.Release()
	}
	for _, claim := range resources.passing {
		if claim != nil {
			claim.Release()
		}
	}
}

func prepareJSONPathExecution(ctx context.Context, document string, plan *JSONPathPlan, variables string, passing []JSONPathVariable, useTZ bool, budget CallBudget, options ResolveOptions) (*jsonPathExecution, *jsonPathExecutionResources, error) {
	if plan == nil || plan.expression == nil {
		return nil, nil, sqlError("22023", "invalid jsonpath execution request")
	}
	rootDocument, rootClaim, remaining, err := claimJSONBDocument(ctx, document, budget)
	if err != nil {
		return nil, nil, err
	}
	resources := &jsonPathExecutionResources{root: rootClaim}
	fail := func(err error) (*jsonPathExecution, *jsonPathExecutionResources, error) {
		resources.release()
		return nil, nil, err
	}
	variableDocument, variableClaim, remaining, err := claimJSONBDocument(ctx, variables, remaining)
	if err != nil {
		return fail(err)
	}
	resources.variable = variableClaim
	if len(variableDocument.nodes) == 0 || variableDocument.nodes[0].kind != jsonTextObject {
		return fail(sqlError("22023", "vars argument must be an object"))
	}
	typed, claims, remaining, err := resolveJSONPathVariables(ctx, passing, remaining, options)
	if err != nil {
		return fail(err)
	}
	resources.passing = claims
	for i := range typed {
		typed[i].item.baseID = int64(i) + 2
	}
	workLimit := remaining.MemoryBytes
	if workLimit > math.MaxUint64/32 {
		workLimit = math.MaxUint64
	} else {
		workLimit *= 32
	}
	execution := &jsonPathExecution{
		ctx: ctx, plan: plan, root: jsonPathNodeItem(&rootDocument, 0), variables: &variableDocument, passing: typed,
		budget: remaining, options: options, useTZ: useTZ, workLimit: max(workLimit, 1024), nextBaseID: int64(len(typed)) + 2,
	}
	resources.execution = execution
	return execution, resources, nil
}

func (execution *jsonPathExecution) tick(units uint64) error {
	if err := execution.ctx.Err(); err != nil { return err }
	if execution.work > execution.workLimit || units > execution.workLimit-execution.work { return sqlError("54000", "jsonpath execution work budget exceeded") }
	execution.work += units
	return nil
}

func (execution *jsonPathExecution) enter() error {
	if execution.depth >= jsonPathCompileDepth { return sqlError("54001", "jsonpath execution nesting limit exceeded") }
	execution.depth++
	return execution.tick(1)
}

func (execution *jsonPathExecution) evaluate(node *jsonPathExpr, current jsonPathItem, last int, emit func(jsonPathItem) error) error {
	if err := execution.enter(); err != nil { return err }
	defer func() { execution.depth-- }()
	switch node.kind {
	case jpNull:
		return emit(jsonPathItem{kind: jsonTextNull, text: "null"})
	case jpString:
		return emit(jsonPathItem{kind: jsonTextString, text: node.text})
	case jpNumber:
		return emit(jsonPathItem{kind: jsonTextNumber, text: node.text})
	case jpBoolean:
		return emit(jsonPathItem{kind: jsonTextBoolean, text: strconv.FormatBool(node.boolean), boolean: node.boolean})
	case jpRoot:
		return emit(execution.root)
	case jpCurrent:
		if current.kind == 0 { return sqlError("42601", "@ is not available in this jsonpath expression") }
		return emit(current)
	case jpVariable:
		for _, variable := range execution.passing {
			if variable.name == node.text {
				return emit(variable.item)
			}
		}
		for child := execution.variables.nodes[0].child; child >= 0; child = execution.variables.nodes[child].next {
			if execution.variables.nodes[child].key == node.text {
				return emit(jsonPathItem{document: execution.variables, index: child, kind: execution.variables.nodes[child].kind, baseID: 1})
			}
		}
		return sqlError("42704", "could not find jsonpath variable "+strconv.Quote(node.text))
	case jpLast:
		if last < 0 { return sqlError("42601", "LAST is not available in this jsonpath expression") }
		return emit(jsonPathItem{kind: jsonTextNumber, text: strconv.Itoa(last)})
	case jpMember, jpAnyMember, jpAnyArray, jpSubscript, jpRecursive, jpFilter, jpMethod:
		return execution.evaluateAccessor(node, current, last, emit)
	case jpExists, jpNot, jpIsUnknown, jpAnd, jpOr, jpEqual, jpNotEqual, jpLess, jpGreater, jpLessEqual, jpGreaterEqual, jpStartsWith, jpLikeRegex:
		truth, err := execution.predicate(node, current, last)
		if err != nil { return err }
		return emit(jsonPathTruthItem(truth))
	case jpAdd, jpSubtract, jpMultiply, jpDivide, jpModulo, jpPlus, jpMinus:
		return execution.arithmetic(node, current, last, emit)
	default:
		return sqlError("XX000", "unknown jsonpath expression operation")
	}
}

func jsonPathTruthItem(truth jsonPathTruth) jsonPathItem {
	if truth == jsonPathUnknown { return jsonPathItem{kind: jsonTextNull, text: "null"} }
	value := truth == jsonPathTrue
	return jsonPathItem{kind: jsonTextBoolean, text: strconv.FormatBool(value), boolean: value}
}

func (execution *jsonPathExecution) evaluateAccessor(node *jsonPathExpr, current jsonPathItem, last int, emit func(jsonPathItem) error) error {
	return execution.evaluate(node.left, current, last, func(input jsonPathItem) error {
		switch node.kind {
		case jpMember:
			return execution.member(input, node.text, emit)
		case jpAnyMember:
			return execution.anyMember(input, emit)
		case jpAnyArray:
			return execution.anyArray(input, emit)
		case jpSubscript:
			return execution.subscript(input, node, current, emit)
		case jpRecursive:
			return execution.recursive(input, node.first, node.last, 0, emit)
		case jpFilter:
			truth, err := execution.predicate(node.right, input, last)
			if err != nil { return err }
			if truth == jsonPathTrue { return emit(input) }
			return nil
		case jpMethod:
			return execution.method(input, node, emit)
		}
		return sqlError("XX000", "unknown jsonpath accessor")
	})
}

func (execution *jsonPathExecution) member(item jsonPathItem, name string, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax {
		return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.member(child, name, emit) })
	}
	if item.kind != jsonTextObject {
		if execution.plan.lax { return nil }
		return sqlError("2203C", "jsonpath member accessor can only be applied to an object")
	}
	node := item.node()
	for child := node.child; child >= 0; child = item.document.nodes[child].next {
		if err := execution.tick(1); err != nil { return err }
		if item.document.nodes[child].key == name { return emit(item.child(child)) }
	}
	if execution.plan.lax { return nil }
	return sqlError("2203A", "jsonpath member not found: "+name)
}

func (execution *jsonPathExecution) anyMember(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax {
		return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.anyMember(child, emit) })
	}
	if item.kind != jsonTextObject {
		if execution.plan.lax { return nil }
		return sqlError("2203C", "jsonpath member wildcard can only be applied to an object")
	}
	for child := item.node().child; child >= 0; child = item.document.nodes[child].next {
		if err := execution.tick(1); err != nil { return err }
		if err := emit(item.child(child)); err != nil { return err }
	}
	return nil
}

func (execution *jsonPathExecution) anyArray(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind != jsonTextArray {
		if execution.plan.lax { return emit(item) }
		return sqlError("22039", "jsonpath array wildcard can only be applied to an array")
	}
	for child := item.node().child; child >= 0; child = item.document.nodes[child].next {
		if err := execution.tick(1); err != nil { return err }
		if err := emit(item.child(child)); err != nil { return err }
	}
	return nil
}

func (execution *jsonPathExecution) forEachArrayRecursive(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind != jsonTextArray { return emit(item) }
	for child := item.node().child; child >= 0; child = item.document.nodes[child].next {
		value := item.child(child)
		if err := execution.tick(1); err != nil { return err }
		if value.kind == jsonTextArray {
			if err := execution.forEachArrayRecursive(value, emit); err != nil { return err }
		} else if err := emit(value); err != nil { return err }
	}
	return nil
}

func (execution *jsonPathExecution) subscript(item jsonPathItem, node *jsonPathExpr, current jsonPathItem, emit func(jsonPathItem) error) error {
	length := 1
	if item.kind == jsonTextArray {
		var err error
		length, err = jsonbChildCount(execution.ctx, item.document.nodes, item.index)
		if err != nil { return err }
	} else if !execution.plan.lax {
		return sqlError("22039", "jsonpath array subscript can only be applied to an array")
	}
	for _, subscript := range node.subscripts {
		from, err := execution.index(subscript.from, current, length-1)
		if err != nil { return err }
		to := from
		if subscript.to != nil { to, err = execution.index(subscript.to, current, length-1); if err != nil { return err } }
		if to < from { continue }
		first, last := max(from, 0), min(to, length-1)
		if first > last { continue }
		if item.kind != jsonTextArray {
			if err := execution.tick(1); err != nil { return err }
			if first == 0 { if err := emit(item); err != nil { return err } }
			continue
		}
		position := 0
		for child := item.node().child; child >= 0 && position <= last; child, position = item.document.nodes[child].next, position+1 {
			if err := execution.tick(1); err != nil { return err }
			if position >= first { if err := emit(item.child(child)); err != nil { return err } }
		}
	}
	return nil
}

func (execution *jsonPathExecution) index(node *jsonPathExpr, current jsonPathItem, last int) (int, error) {
	count, value := 0, jsonPathItem{}
	err := execution.evaluate(node, current, last, func(item jsonPathItem) error { count++; value = item; if count > 1 { return errJSONPathStop }; return nil })
	if err != nil && !errors.Is(err, errJSONPathStop) { return 0, err }
	if count != 1 || value.kind != jsonTextNumber { return 0, sqlError("22033", "jsonpath array subscript is not a single numeric value") }
	text := value.scalarText()
	if strings.ContainsAny(text, ".eE") { return 0, sqlError("22033", "jsonpath array subscript is not an integer") }
	parsed, err := strconv.ParseInt(text, 10, 32)
	if err != nil { return 0, sqlError("22033", "jsonpath array subscript is out of range") }
	return int(parsed), nil
}

func (execution *jsonPathExecution) recursive(item jsonPathItem, first, last, level int, emit func(jsonPathItem) error) error {
	if err := execution.enter(); err != nil { return err }
	defer func() { execution.depth-- }()
	if level >= first && (last < 0 || level <= last) {
		if err := emit(item); err != nil { return err }
	}
	if last >= 0 && level >= last || item.kind != jsonTextArray && item.kind != jsonTextObject { return nil }
	for child := item.node().child; child >= 0; child = item.document.nodes[child].next {
		if err := execution.tick(1); err != nil { return err }
		if err := execution.recursive(item.child(child), first, last, level+1, emit); err != nil { return err }
	}
	return nil
}

func (execution *jsonPathExecution) predicate(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathTruth, error) {
	if err := execution.enter(); err != nil { return jsonPathUnknown, err }
	defer func() { execution.depth-- }()
	switch node.kind {
	case jpAnd:
		left, err := execution.predicate(node.left, current, last); if err != nil { return jsonPathUnknown, err }
		if left == jsonPathFalse { return jsonPathFalse, nil }
		right, err := execution.predicate(node.right, current, last); if err != nil { return jsonPathUnknown, err }
		if right == jsonPathFalse { return jsonPathFalse, nil }
		if left == jsonPathUnknown || right == jsonPathUnknown { return jsonPathUnknown, nil }
		return jsonPathTrue, nil
	case jpOr:
		left, err := execution.predicate(node.left, current, last); if err != nil { return jsonPathUnknown, err }
		if left == jsonPathTrue { return jsonPathTrue, nil }
		right, err := execution.predicate(node.right, current, last); if err != nil { return jsonPathUnknown, err }
		if right == jsonPathTrue { return jsonPathTrue, nil }
		if left == jsonPathUnknown || right == jsonPathUnknown { return jsonPathUnknown, nil }
		return jsonPathFalse, nil
	case jpNot:
		value, err := execution.predicate(node.left, current, last)
		if value == jsonPathTrue { value = jsonPathFalse } else if value == jsonPathFalse { value = jsonPathTrue }
		return value, err
	case jpIsUnknown:
		value, err := execution.predicate(node.left, current, last)
		if err != nil { return jsonPathUnknown, err }
		if value == jsonPathUnknown { return jsonPathTrue, nil }
		return jsonPathFalse, nil
	case jpExists:
		found := false
		err := execution.evaluate(node.left, current, last, func(jsonPathItem) error { found = true; return errJSONPathStop })
		if errors.Is(err, errJSONPathStop) { return jsonPathTrue, nil }
		if err != nil { return jsonPathUnknown, err }
		if found { return jsonPathTrue, nil }
		return jsonPathFalse, nil
	case jpEqual, jpNotEqual, jpLess, jpGreater, jpLessEqual, jpGreaterEqual:
		return execution.comparison(node, current, last)
	case jpStartsWith:
		return execution.startsWith(node, current, last)
	case jpLikeRegex:
		return execution.likeRegex(node, current, last)
	default:
		count, truth := 0, jsonPathUnknown
		err := execution.evaluate(node, current, last, func(item jsonPathItem) error {
			count++
			if item.kind == jsonTextNull { truth = jsonPathUnknown } else if item.kind == jsonTextBoolean && item.scalarText() == "true" { truth = jsonPathTrue } else if item.kind == jsonTextBoolean { truth = jsonPathFalse } else { return sqlError("22038", "jsonpath predicate requires a singleton boolean") }
			if count > 1 { return sqlError("22038", "jsonpath predicate requires a singleton boolean") }
			return nil
		})
		if err != nil { return jsonPathUnknown, err }
		return truth, nil
	}
}

func (execution *jsonPathExecution) comparison(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathTruth, error) {
	result, pairs := jsonPathFalse, 0
	err := execution.evaluate(node.left, current, last, func(left jsonPathItem) error {
		return execution.unwrapComparison(left, func(left jsonPathItem) error {
			return execution.evaluate(node.right, current, last, func(right jsonPathItem) error {
				return execution.unwrapComparison(right, func(right jsonPathItem) error {
					pairs++
					order, comparable, unknown, err := execution.compare(left, right)
					if err != nil { return err }
					if unknown { result = jsonPathUnknown; return nil }
					matched := comparable && jsonPathOrderMatches(node.kind, order)
					if matched { result = jsonPathTrue; return errJSONPathStop }
					return nil
				})
			})
		})
	})
	if errors.Is(err, errJSONPathStop) { return jsonPathTrue, nil }
	if err != nil { return jsonPathUnknown, err }
	if pairs == 0 { return jsonPathUnknown, nil }
	return result, nil
}

func (execution *jsonPathExecution) unwrapComparison(item jsonPathItem, emit func(jsonPathItem) error) error {
	if execution.plan.lax && item.kind == jsonTextArray { return execution.forEachArrayRecursive(item, emit) }
	return emit(item)
}

func jsonPathOrderMatches(kind jsonPathKind, order int) bool {
	switch kind { case jpEqual: return order == 0; case jpNotEqual: return order != 0; case jpLess: return order < 0; case jpGreater: return order > 0; case jpLessEqual: return order <= 0; case jpGreaterEqual: return order >= 0 }
	return false
}

func (execution *jsonPathExecution) compare(left, right jsonPathItem) (order int, comparable, unknown bool, err error) {
	if left.kind == jsonTextNull && right.kind == jsonTextNull { return 0, true, false, nil }
	if left.kind == jsonTextNull || right.kind == jsonTextNull { return 0, false, true, nil }
	if left.isTemporal() || right.isTemporal() {
		if !left.isTemporal() || !right.isTemporal() { return 0, false, true, nil }
		order, comparable, err = execution.compareTemporal(left, right)
		return order, comparable, !comparable && err == nil, err
	}
	if left.kind != right.kind { return 0, false, true, nil }
	switch left.kind {
	case jsonTextString:
		return strings.Compare(left.scalarText(), right.scalarText()), true, false, nil
	case jsonTextNumber:
		order, err = compareJSONBNumbers(execution.ctx, left.scalarText(), right.scalarText())
		return order, err == nil, false, err
	case jsonTextBoolean:
		return strings.Compare(left.scalarText(), right.scalarText()), true, false, nil
	default:
		return 0, false, true, nil
	}
}

func (execution *jsonPathExecution) compareTemporal(left, right jsonPathItem) (int, bool, error) {
	if left.typeID != right.typeID {
		var err error
		switch {
		case left.typeID == 1082 && (right.typeID == 1114 || right.typeID == 1184):
			left.temporal, err = execution.castDatetime(left.temporal, left.typeID, right.typeID); left.typeID = right.typeID
		case right.typeID == 1082 && (left.typeID == 1114 || left.typeID == 1184):
			right.temporal, err = execution.castDatetime(right.temporal, right.typeID, left.typeID); right.typeID = left.typeID
		case left.typeID == 1114 && right.typeID == 1184:
			left.temporal, err = execution.castDatetime(left.temporal, left.typeID, right.typeID); left.typeID = right.typeID
		case right.typeID == 1114 && left.typeID == 1184:
			right.temporal, err = execution.castDatetime(right.temporal, right.typeID, left.typeID); right.typeID = left.typeID
		case left.typeID == 1083 && right.typeID == 1266:
			left.temporal, err = execution.castDatetime(left.temporal, left.typeID, right.typeID); left.typeID = right.typeID
		case right.typeID == 1083 && left.typeID == 1266:
			right.temporal, err = execution.castDatetime(right.temporal, right.typeID, left.typeID); right.typeID = left.typeID
		default:
			return 0, false, nil
		}
		if err != nil { return 0, false, err }
	}
	id := model.FunctionID(0)
	switch left.typeID { case 1082: id = 1092; case 1083: id = 1107; case 1266: id = 1358; case 1114: id = 2045; case 1184: id = 1314 }
	call, err := Resolve(id, execution.options); if err != nil { return 0, false, err }
	value, err := call.Call(execution.ctx, []model.Value{left.temporal, right.temporal}, execution.budget.WithOutput(8))
	if err != nil { return 0, false, err }
	return int(value.IntVal), true, nil
}

func (execution *jsonPathExecution) startsWith(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathTruth, error) {
	right, err := execution.singleton(node.right, current, last)
	if err != nil { return jsonPathUnknown, err }
	if right.kind != jsonTextString { return jsonPathUnknown, sqlError("22038", "jsonpath starts with right operand must be a string") }
	matched, found := false, false
	err = execution.evaluate(node.left, current, last, func(item jsonPathItem) error {
		return execution.unwrapComparison(item, func(item jsonPathItem) error {
			if item.kind != jsonTextString { return nil }
			found = true
			if strings.HasPrefix(item.scalarText(), right.scalarText()) { matched = true; return errJSONPathStop }
			return nil
		})
	})
	if errors.Is(err, errJSONPathStop) { return jsonPathTrue, nil }
	if err != nil { return jsonPathUnknown, err }
	if !found { return jsonPathUnknown, nil }
	if matched { return jsonPathTrue, nil }
	return jsonPathFalse, nil
}

func (execution *jsonPathExecution) likeRegex(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathTruth, error) {
	matched, found := false, false
	err := execution.evaluate(node.left, current, last, func(item jsonPathItem) error {
		return execution.unwrapComparison(item, func(item jsonPathItem) error {
			if item.kind != jsonTextString { return nil }
			found = true
			value := item.scalarText()
			if execution.regexp == nil {
				runtime, err := newRegexpRuntime(execution.ctx, execution.budget.MemoryBytes, execution.budget.Allocator)
				if err != nil { return err }
				execution.regexp = runtime
			}
			if err := execution.regexp.bindInput(value, node.text); err != nil { return err }
			text, _, err := decodeRegexpRunes(execution.ctx, value)
			if err != nil { return err }
			execution.regexp.bindGroups(node.regex.groups)
			_, ok, err := node.regex.find(execution.regexp, text, 0)
			if err != nil { return err }
			if ok { matched = true; return errJSONPathStop }
			return nil
		})
	})
	if errors.Is(err, errJSONPathStop) { return jsonPathTrue, nil }
	if err != nil { return jsonPathUnknown, err }
	if !found { return jsonPathUnknown, nil }
	if matched { return jsonPathTrue, nil }
	return jsonPathFalse, nil
}

func (execution *jsonPathExecution) singleton(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathItem, error) {
	count, result := 0, jsonPathItem{}
	err := execution.evaluate(node, current, last, func(item jsonPathItem) error { count++; result = item; if count > 1 { return errJSONPathStop }; return nil })
	if err != nil && !errors.Is(err, errJSONPathStop) { return jsonPathItem{}, err }
	if count != 1 { return jsonPathItem{}, sqlError("22038", "jsonpath expression must return a singleton") }
	return result, nil
}

func (execution *jsonPathExecution) arithmetic(node *jsonPathExpr, current jsonPathItem, last int, emit func(jsonPathItem) error) error {
	if node.kind == jpPlus || node.kind == jpMinus {
		return execution.evaluate(node.left, current, last, func(item jsonPathItem) error {
			return execution.numericItems(item, func(item jsonPathItem) error {
				if node.kind == jpPlus { return emit(item) }
				value, err := execution.numericCall(1771, item.scalarText())
				if err != nil { return err }
				return emit(jsonPathItem{kind: jsonTextNumber, text: value.StrVal})
			})
		})
	}
	left, err := execution.singletonNumeric(node.left, current, last); if err != nil { return err }
	right, err := execution.singletonNumeric(node.right, current, last); if err != nil { return err }
	id := model.FunctionID(0)
	switch node.kind { case jpAdd: id = 1724; case jpSubtract: id = 1725; case jpMultiply: id = 1726; case jpDivide: id = 1727; case jpModulo: id = 1729 }
	value, err := execution.numericCall(id, left.scalarText(), right.scalarText())
	if err != nil { return err }
	return emit(jsonPathItem{kind: jsonTextNumber, text: value.StrVal})
}

func (execution *jsonPathExecution) numericItems(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.numericItems(child, emit) }) }
	if item.kind != jsonTextNumber { return sqlError("22036", "jsonpath item is not a number") }
	return emit(item)
}

func (execution *jsonPathExecution) singletonNumeric(node *jsonPathExpr, current jsonPathItem, last int) (jsonPathItem, error) {
	count, result := 0, jsonPathItem{}
	err := execution.evaluate(node, current, last, func(item jsonPathItem) error {
		return execution.numericItems(item, func(item jsonPathItem) error { count++; result = item; if count > 1 { return errJSONPathStop }; return nil })
	})
	if err != nil && !errors.Is(err, errJSONPathStop) { return jsonPathItem{}, err }
	if count != 1 { return jsonPathItem{}, sqlError("22038", "jsonpath arithmetic operand must be a singleton") }
	return result, nil
}

func (execution *jsonPathExecution) numericCall(id model.FunctionID, values ...string) (model.Value, error) {
	call, err := Resolve(id, execution.options); if err != nil { return model.Value{}, err }
	arguments := make([]model.Value, len(values))
	for index, value := range values { arguments[index] = model.Value{Type: model.TypeNumeric, StrVal: value} }
	return call.Call(execution.ctx, arguments, execution.budget)
}

func (execution *jsonPathExecution) method(item jsonPathItem, method *jsonPathExpr, emit func(jsonPathItem) error) error {
	switch method.text {
	case "type":
		return emit(jsonPathItem{kind: jsonTextString, text: execution.typeName(item)})
	case "size":
		size := 1
		if item.kind == jsonTextArray { var err error; size, err = jsonbChildCount(execution.ctx, item.document.nodes, item.index); if err != nil { return err } } else if !execution.plan.lax { return sqlError("22039", "jsonpath size() can only be applied to an array") }
		return emit(jsonPathItem{kind: jsonTextNumber, text: strconv.Itoa(size)})
	case "keyvalue":
		return execution.keyValue(item, emit)
	case "abs", "floor", "ceiling", "double", "bigint", "integer", "number", "decimal":
		return execution.numericMethod(item, method, emit)
	case "boolean":
		return execution.booleanMethod(item, emit)
	case "string":
		return execution.stringMethod(item, emit)
	case "date", "time", "time_tz", "timestamp", "timestamp_tz", "datetime":
		return execution.datetimeMethod(item, method, emit)
	}
	return sqlError("XX000", "unknown jsonpath item method")
}

func (execution *jsonPathExecution) typeName(item jsonPathItem) string {
	if item.isTemporal() {
		switch item.typeID { case 1082: return "date"; case 1083: return "time without time zone"; case 1266: return "time with time zone"; case 1114: return "timestamp without time zone"; case 1184: return "timestamp with time zone" }
	}
	switch item.kind { case jsonTextNull: return "null"; case jsonTextBoolean: return "boolean"; case jsonTextNumber: return "number"; case jsonTextString: return "string"; case jsonTextArray: return "array"; case jsonTextObject: return "object" }
	return "unknown"
}

func (execution *jsonPathExecution) numericMethod(item jsonPathItem, method *jsonPathExpr, emit func(jsonPathItem) error) error {
	return execution.numericMethodItems(item, func(item jsonPathItem) error {
		text := item.scalarText()
		if item.kind == jsonTextString && (method.text == "abs" || method.text == "floor" || method.text == "ceiling") { return sqlError("22036", "jsonpath item is not a number") }
		if item.kind == jsonTextString {
			var err error
			text, err = canonicalJSONPathNumber(execution.ctx, text, execution.budget.MemoryBytes)
			if err != nil { return sqlError("22036", "jsonpath string is not a valid number") }
		}
		id := model.FunctionID(0)
		switch method.text { case "abs": id = 1704; case "floor": id = 1712; case "ceiling": id = 2167 }
		if id != 0 {
			value, err := execution.numericCall(id, text); if err != nil { return err }
			return emit(jsonPathItem{kind: jsonTextNumber, text: value.StrVal})
		}
		if method.text == "double" {
			call, err := Resolve(1746, execution.options); if err != nil { return err }
			value, err := call.Call(execution.ctx, []model.Value{{Type: model.TypeNumeric, StrVal: text}}, execution.budget.WithOutput(8)); if err != nil { return err }
			if math.IsInf(value.FloatVal, 0) || math.IsNaN(value.FloatVal) { return sqlError("22003", "jsonpath double() result is out of range") }
			return emit(jsonPathItem{kind: jsonTextNumber, text: strconv.FormatFloat(value.FloatVal, 'g', -1, 64)})
		}
		if method.text == "bigint" || method.text == "integer" {
			cast := model.FunctionID(1779); if method.text == "integer" { cast = 1744 }
			call, err := Resolve(cast, execution.options); if err != nil { return err }
			value, err := call.Call(execution.ctx, []model.Value{{Type: model.TypeNumeric, StrVal: text}}, execution.budget.WithOutput(8)); if err != nil { return err }
			return emit(jsonPathItem{kind: jsonTextNumber, text: strconv.FormatInt(value.IntVal, 10)})
		}
		if method.text == "decimal" && len(method.args) != 0 {
			precision, _ := strconv.ParseInt(method.args[0].text, 10, 32)
			scale := int64(0); if len(method.args) > 1 { scale, _ = strconv.ParseInt(method.args[1].text, 10, 32) }
			if precision < 1 || precision > 1000 || scale < -1000 || scale > 1000 { return sqlError("22023", "invalid jsonpath decimal precision or scale") }
			typmod := ((precision << 16) | (scale & 0x7ff)) + 4
			call, err := Resolve(1703, execution.options); if err != nil { return err }
			value, err := call.Call(execution.ctx, []model.Value{{Type: model.TypeNumeric, StrVal: text}, model.Int64Val(typmod)}, execution.budget); if err != nil { return err }
			text = value.StrVal
		}
		return emit(jsonPathItem{kind: jsonTextNumber, text: text})
	})
}

func (execution *jsonPathExecution) numericMethodItems(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.numericMethodItems(child, emit) }) }
	if item.kind != jsonTextNumber && item.kind != jsonTextString { return sqlError("22036", "jsonpath item is not a number") }
	return emit(item)
}

func (execution *jsonPathExecution) booleanMethod(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.booleanMethod(child, emit) }) }
	switch item.kind {
	case jsonTextBoolean:
		return emit(item)
	case jsonTextNumber:
		order, err := compareJSONBNumbers(execution.ctx, item.scalarText(), "0")
		if err != nil { return err }
		if order != 0 { return emit(jsonPathTruthItem(jsonPathTrue)) }
		return emit(jsonPathTruthItem(jsonPathFalse))
	case jsonTextString:
		value := strings.ToLower(strings.TrimSpace(item.scalarText()))
		if value == "true" || value == "t" || value == "yes" || value == "y" || value == "on" || value == "1" { return emit(jsonPathTruthItem(jsonPathTrue)) }
		if value == "false" || value == "f" || value == "no" || value == "n" || value == "off" || value == "0" { return emit(jsonPathTruthItem(jsonPathFalse)) }
	}
	return sqlError("22023", "jsonpath item cannot be converted to boolean")
}

func (execution *jsonPathExecution) stringMethod(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.stringMethod(child, emit) }) }
	if item.isTemporal() {
		text, err := execution.temporalOutput(item); if err != nil { return err }
		return emit(jsonPathItem{kind: jsonTextString, text: text})
	}
	switch item.kind {
	case jsonTextString:
		return emit(item)
	case jsonTextBoolean, jsonTextNumber:
		return emit(jsonPathItem{kind: jsonTextString, text: item.scalarText()})
	default:
		return sqlError("2203F", "jsonpath item cannot be converted to string")
	}
}

func (execution *jsonPathExecution) datetimeMethod(item jsonPathItem, method *jsonPathExpr, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.datetimeMethod(child, method, emit) }) }
	if item.kind != jsonTextString { return sqlError("22031", "jsonpath datetime method requires a string") }
	text := item.scalarText()
	sourceName := inferJSONPathDatetime(text)
	if method.text == "datetime" && len(method.args) == 1 {
		converted, inferred, err := parseJSONPathDatetimeTemplate(text, method.args[0].text)
		if err != nil { return err }
		text, sourceName = converted, inferred
	}
	value, sourceID, err := execution.parseDatetime(sourceName, text)
	if err != nil { return err }
	targetName := method.text
	if targetName == "datetime" { targetName = sourceName }
	targetID := jsonPathDatetimeType(targetName)
	if targetID == 0 { return sqlError("22031", "jsonpath datetime format is not recognized") }
	if sourceID != targetID {
		value, err = execution.castDatetime(value, sourceID, targetID)
		if err != nil { return err }
	}
	if len(method.args) == 1 && method.text != "datetime" {
		precision, parseErr := strconv.ParseInt(method.args[0].text, 10, 32)
		if parseErr != nil || precision < 0 || precision > 6 { return sqlError("22031", "datetime precision must be between 0 and 6") }
		typmod := model.FunctionID(0)
		switch targetID { case 1083: typmod = 1968; case 1266: typmod = 1969; case 1114: typmod = 1961; case 1184: typmod = 1967 }
		if typmod == 0 { return sqlError("22031", "datetime precision is not valid for this type") }
		call, resolveErr := Resolve(typmod, execution.options)
		if resolveErr != nil { return resolveErr }
		value, err = call.Call(execution.ctx, []model.Value{value, model.Int64Val(precision)}, execution.budget)
		if err != nil { return err }
	}
	return emit(jsonPathItem{kind: jsonTextString, temporal: value, typeID: targetID})
}

func jsonPathDatetimeType(name string) model.TypeID {
	switch name { case "date": return 1082; case "time": return 1083; case "time_tz": return 1266; case "timestamp": return 1114; case "timestamp_tz": return 1184 }
	return 0
}

func (execution *jsonPathExecution) parseDatetime(name, text string) (model.Value, model.TypeID, error) {
	typeID := jsonPathDatetimeType(name)
	id := model.FunctionID(0)
	switch typeID { case 1082: id = 1084; case 1083: id = 1143; case 1266: id = 1350; case 1114: id = 1312; case 1184: id = 1150 }
	if id == 0 { return model.Value{}, 0, sqlError("22031", "jsonpath datetime format is not recognized") }
	call, err := Resolve(id, execution.options)
	if err != nil { return model.Value{}, 0, err }
	arguments := []model.Value{model.StringVal(text)}
	if typeID != 1082 { arguments = append(arguments, model.Int64Val(0), model.Int64Val(-1)) }
	value, err := call.Call(execution.ctx, arguments, execution.budget)
	return value, typeID, err
}

func (execution *jsonPathExecution) castDatetime(value model.Value, source, target model.TypeID) (model.Value, error) {
	cast := model.FunctionID(0)
	timezone := false
	switch {
	case source == 1082 && target == 1114: cast = 2024
	case source == 1082 && target == 1184: cast, timezone = 1174, true
	case source == 1083 && target == 1266: cast, timezone = 2047, true
	case source == 1266 && target == 1083: cast, timezone = 2046, true
	case source == 1114 && target == 1082: cast = 2029
	case source == 1114 && target == 1083: cast = 1316
	case source == 1114 && target == 1184: cast, timezone = 2028, true
	case source == 1184 && target == 1082: cast, timezone = 1178, true
	case source == 1184 && target == 1083: cast, timezone = 2019, true
	case source == 1184 && target == 1266: cast, timezone = 1388, true
	case source == 1184 && target == 1114: cast, timezone = 2027, true
	default: return model.Value{}, sqlError("22031", "incompatible jsonpath datetime types")
	}
	if timezone && !execution.useTZ { return model.Value{}, sqlError("0A000", "jsonpath datetime conversion requires time zone usage") }
	call, err := Resolve(cast, execution.options)
	if err != nil { return model.Value{}, err }
	return call.Call(execution.ctx, []model.Value{value}, execution.budget)
}

func inferJSONPathDatetime(text string) string {
	zone := strings.HasSuffix(text, "Z") || strings.HasSuffix(text, "z")
	timeStart := strings.IndexAny(text, "T ")
	zoneStart := 1
	if timeStart >= 0 { zoneStart = timeStart + 1 }
	if offset := strings.IndexAny(text[zoneStart:], "+-"); offset >= 0 { zone = true }
	if timeStart >= 0 { if zone { return "timestamp_tz" }; return "timestamp" }
	if strings.Contains(text, ":") { if zone { return "time_tz" }; return "time" }
	return "date"
}

func parseJSONPathDatetimeTemplate(input, template string) (string, string, error) {
	layout := template
	replacements := []struct{ token, layout string }{{"HH24", "15"}, {"HH12", "03"}, {"MONTH", "January"}, {"YYYY", "2006"}, {"MON", "Jan"}, {"DDD", "002"}, {"TZH", "-07"}, {"TZM", "00"}, {"MS", "000"}, {"US", "000000"}, {"YY", "06"}, {"MM", "01"}, {"DD", "02"}, {"MI", "04"}, {"SS", "05"}, {"AM", "PM"}, {"PM", "PM"}, {"TZ", "MST"}}
	var built strings.Builder
	for len(layout) != 0 {
		if layout[0] == '"' {
			end := strings.IndexByte(layout[1:], '"')
			if end < 0 { return "", "", sqlError("22031", "unterminated quoted text in jsonpath datetime template") }
			built.WriteString(layout[1 : end+1])
			layout = layout[end+2:]
			continue
		}
		upper := strings.ToUpper(layout)
		matched := false
		for _, replacement := range replacements {
			if strings.HasPrefix(upper, replacement.token) { built.WriteString(replacement.layout); layout = layout[len(replacement.token):]; matched = true; break }
		}
		if !matched { built.WriteByte(layout[0]); layout = layout[1:] }
	}
	if strings.Contains(strings.ToUpper(template), "TZH") {
		zone := strings.LastIndexAny(input, "+-")
		if zone >= 0 && zone+1 < len(input) && (zone+2 == len(input) || input[zone+2] == ':') {
			input = input[:zone+1] + "0" + input[zone+1:]
		}
	}
	parsed, err := time.Parse(built.String(), input)
	if err != nil { return "", "", sqlError("22031", "jsonpath datetime string does not match template") }
	hasDate := strings.ContainsAny(strings.ToUpper(template), "YD")
	hasTime := strings.Contains(strings.ToUpper(template), "HH") || strings.Contains(strings.ToUpper(template), "MI") || strings.Contains(strings.ToUpper(template), "SS")
	hasZone := strings.Contains(strings.ToUpper(template), "TZ")
	if hasDate && hasTime { if hasZone { return parsed.Format(time.RFC3339Nano), "timestamp_tz", nil }; return parsed.Format("2006-01-02 15:04:05.999999"), "timestamp", nil }
	if hasTime { if hasZone { return parsed.Format("15:04:05.999999-07:00"), "time_tz", nil }; return parsed.Format("15:04:05.999999"), "time", nil }
	if hasDate { return parsed.Format("2006-01-02"), "date", nil }
	return "", "", sqlError("22031", "jsonpath datetime template has no date or time fields")
}

func (execution *jsonPathExecution) temporalOutput(item jsonPathItem) (string, error) {
	id := model.FunctionID(0)
	switch item.typeID { case 1082: id = 1085; case 1083: id = 1144; case 1266: id = 1351; case 1114: id = 1313; case 1184: id = 1151 }
	call, err := Resolve(id, execution.options); if err != nil { return "", err }
	value, err := call.Call(execution.ctx, []model.Value{item.temporal}, execution.budget)
	if err != nil { return "", err }
	return value.StrVal, nil
}

func (execution *jsonPathExecution) keyValue(item jsonPathItem, emit func(jsonPathItem) error) error {
	if item.kind == jsonTextArray && execution.plan.lax { return execution.forEachArrayRecursive(item, func(child jsonPathItem) error { return execution.keyValue(child, emit) }) }
	if item.kind != jsonTextObject { return sqlError("2203C", "jsonpath keyvalue() can only be applied to an object") }
	offset, found, err := execution.jsonPathBinaryOffset(item.document, item.baseRoot, item.index, 0)
	if err != nil { return err }
	if !found || item.baseID > (math.MaxInt64-offset)/10000000000 { return sqlError("22003", "jsonpath keyvalue object identifier is out of range") }
	id := item.baseID*10000000000 + offset
	for child := item.node().child; child >= 0; child = item.document.nodes[child].next {
		keyBytes, err := jsonQuotedBytes(execution.ctx, item.document.nodes[child].key)
		if err != nil { return err }
		bytes64 := int64(len(`{"id": `)+len(strconv.FormatInt(id, 10))+len(`, "key": `)+len(`, "value": `)+1) + keyBytes + item.document.nodes[child].bytes
		if bytes64 < 0 || bytes64 > math.MaxInt { return sqlError("54000", "jsonpath execution memory budget exceeded") }
		bytes := int(bytes64)
		if err := execution.renderBudget(bytes); err != nil { return err }
		var output strings.Builder
		output.Grow(bytes)
		output.WriteString(`{"id": `); output.WriteString(strconv.FormatInt(id, 10)); output.WriteString(`, "key": `)
		if err := appendJSONQuoted(execution.ctx, &output, item.document.nodes[child].key); err != nil { return err }
		output.WriteString(`, "value": `)
		if err := appendJSONPathNode(execution.ctx, &output, item.document, child); err != nil { return err }
		output.WriteByte('}')
		baseID := execution.nextBaseID
		if baseID == math.MaxInt64 { return sqlError("22003", "jsonpath keyvalue object identifier is out of range") }
		execution.nextBaseID++
		if err := execution.emitGenerated(output.String(), baseID, emit); err != nil { return err }
	}
	return nil
}
func (execution *jsonPathExecution) emitGenerated(text string, baseID int64, emit func(jsonPathItem) error) error {
	remaining, err := execution.budget.After(uint64(len(text)))
	if err != nil { return err }
	var textClaim columnar.Allocation
	if execution.budget.Allocator != nil {
		textClaim, err = execution.budget.Allocator.Reserve(uint64(len(text)))
		if err != nil { return err }
		defer textClaim.Release()
	}
	document, claim, _, err := claimJSONBDocument(execution.ctx, text, remaining)
	if err != nil { return err }
	if claim != nil { defer claim.Release() }
	return emit(jsonPathItem{document: &document, index: 0, kind: document.nodes[0].kind, baseID: baseID})
}

func (execution *jsonPathExecution) jsonPathBinaryOffset(document *jsonbDocument, current, target int, base int64) (int64, bool, error) {
	if err := execution.tick(1); err != nil { return 0, false, err }
	if current == target { return base, true, nil }
	node := &document.nodes[current]
	if node.kind != jsonTextArray && node.kind != jsonTextObject { return 0, false, nil }
	count, err := jsonbChildCount(execution.ctx, document.nodes, current)
	if err != nil { return 0, false, err }
	offset := base + 4 + int64(count)*4
	if node.kind == jsonTextObject {
		offset += int64(count) * 4
		for child := node.child; child >= 0; child = document.nodes[child].next { if err := execution.tick(1); err != nil { return 0, false, err }; offset += int64(len(document.nodes[child].key)) }
	}
	for child := node.child; child >= 0; child = document.nodes[child].next {
		if err := execution.tick(1); err != nil { return 0, false, err }
		kind := document.nodes[child].kind
		if kind == jsonTextNumber || kind == jsonTextArray || kind == jsonTextObject { offset += (-offset) & 3 }
		if child == target { return offset, true, nil }
		if kind == jsonTextArray || kind == jsonTextObject {
			result, found, err := execution.jsonPathBinaryOffset(document, child, target, offset)
			if err != nil || found { return result, found, err }
		}
		offset += document.nodes[child].storageBytes
	}
	return 0, false, nil
}


func (execution *jsonPathExecution) render(item jsonPathItem) (string, error) {
	if item.isTemporal() {
		text, err := execution.temporalOutput(item); if err != nil { return "", err }
		var output strings.Builder
		if err := appendJSONQuoted(execution.ctx, &output, text); err != nil { return "", err }
		if err := execution.renderBudget(output.Len()); err != nil { return "", err }
		return output.String(), nil
	}
	if item.document == nil {
		if item.kind == jsonTextString {
			var output strings.Builder
			if err := appendJSONQuoted(execution.ctx, &output, item.text); err != nil { return "", err }
			if err := execution.renderBudget(output.Len()); err != nil { return "", err }
			return output.String(), nil
		}
		if err := execution.renderBudget(len(item.text)); err != nil { return "", err }
		return item.text, execution.ctx.Err()
	}
	var output strings.Builder
	bytes := item.document.nodes[item.index].bytes
	if bytes < 0 || uint64(bytes) > execution.budget.MemoryBytes || bytes > execution.budget.OutputBytes { return "", sqlError("54000", "builtin output byte budget exceeded") }
	output.Grow(int(bytes))
	if err := appendJSONPathNode(execution.ctx, &output, item.document, item.index); err != nil { return "", err }
	return output.String(), nil
}

func (execution *jsonPathExecution) renderBudget(bytes int) error {
	if bytes < 0 || uint64(bytes) > execution.budget.MemoryBytes || int64(bytes) > execution.budget.OutputBytes { return sqlError("54000", "builtin output byte budget exceeded") }
	return execution.ctx.Err()
}

func appendJSONPathNode(ctx context.Context, output *strings.Builder, document *jsonbDocument, start int) error {
	frames := document.frames
	current, depth := start, 0
	for {
		if err := ctx.Err(); err != nil { return err }
		node := &document.nodes[current]
		switch node.kind {
		case jsonTextObject, jsonTextArray:
			open, close := byte('['), byte(']')
			if node.kind == jsonTextObject { open, close = '{', '}' }
			output.WriteByte(open)
			if node.child >= 0 {
				frames[depth].parent = current
				depth++
				current = node.child
				if err := appendJSONBMemberKey(ctx, output, node.kind, document.nodes[current].key); err != nil { return err }
				continue
			}
			output.WriteByte(close)
		case jsonTextString:
			if err := appendJSONQuoted(ctx, output, node.text); err != nil { return err }
		case jsonTextNumber:
			number, err := inspectJSONBNumber(ctx, node.text)
			if err != nil { return err }
			if err := number.append(ctx, output); err != nil { return err }
		case jsonTextBoolean, jsonTextNull:
			output.WriteString(node.text)
		}
		for {
			if depth == 0 { return ctx.Err() }
			parent := frames[depth-1].parent
			if sibling := document.nodes[current].next; sibling >= 0 {
				output.WriteString(", ")
				current = sibling
				if err := appendJSONBMemberKey(ctx, output, document.nodes[parent].kind, document.nodes[current].key); err != nil { return err }
				break
			}
			if document.nodes[parent].kind == jsonTextObject { output.WriteByte('}') } else { output.WriteByte(']') }
			current, depth = parent, depth-1
		}
	}
}

func isSuppressibleJSONPathError(err error) bool {
	var sql *model.Error
	if !errors.As(err, &sql) { return false }
	switch sql.Code {
	case "22003", "22008", "22012", "22031", "22033", "22036", "22038", "22039", "2203A", "2203B", "2203C", "2203F", "22P02":
		return true
	case "22023":
		return strings.HasPrefix(sql.Message, "jsonpath item") || strings.HasPrefix(sql.Message, "invalid jsonpath decimal")
	}
	return false
}

func jsonPathFunctionUseTZ(id model.FunctionID) bool { return id == 1177 || id == 1179 || id == 1180 || id == 2023 || id == 2030 }
