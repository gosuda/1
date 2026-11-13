package bind

import (
	"context"
	"strings"
	"unsafe"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type callArguments struct {
	values []expression
	names  []string
}

func (call callArguments) routineOrder(parameters []model.RoutineParameter) ([builtin.MaxCallArguments]int16, bool) {
	// Call arguments bind only to input-capable parameters (in, inout,
	// variadic); out and table positions never take a call argument, so the
	// order is computed over the input parameter list and expanded back to
	// declaration positions with non-input positions left unassigned.
	var order [builtin.MaxCallArguments]int16
	for i := range order {
		order[i] = -1
	}
	index := make([]int16, 0, len(parameters))
	for i, parameter := range parameters {
		if routineParameterIsInput(parameter) {
			index = append(index, int16(i))
		}
	}
	inputOrder, ok := call.order(len(index), func(i int) (string, bool) {
		name := parameters[index[i]].Name
		return name, name != ""
	}, func(i int) bool {
		return parameters[index[i]].Default != nil
	})
	if !ok {
		return order, false
	}
	for i, target := range index {
		order[target] = inputOrder[i]
	}
	return order, true
}

func (b *binder) completeRoutineArguments(parameters []model.RoutineParameter, call callArguments, s *scope, pos int) ([]expression, error) {
	if call.names == nil && len(call.values) == len(parameters) {
		return call.values, nil
	}
	order, ok := call.routineOrder(parameters)
	if !ok {
		return nil, bindError("42883", "function does not exist", pos)
	}
	if uint64(len(parameters))*uint64(unsafe.Sizeof(expression{})) > b.options.Limits.MaxBytes {
		return nil, bindError("54000", "routine argument metadata limit exceeded", pos)
	}
	arguments := make([]expression, len(parameters))
	for i, source := range order[:len(parameters)] {
		if source >= 0 {
			arguments[i] = call.values[source]
			continue
		}
		value, err := b.routineDefault(parameters[i].Default, parameters[i].Layout, s, pos)
		if err != nil {
			return nil, err
		}
		arguments[i] = value
	}
	return arguments, nil
}

func (b *binder) callArguments(nodes []*zpgparse.Node, s *scope, pos int) (callArguments, error) {
	var call callArguments
	if len(nodes) > builtin.MaxCallArguments {
		return call, bindError("54023", "cannot pass more than 100 arguments to a function", pos)
	}
	bytes := uint64(len(nodes)) * uint64(unsafe.Sizeof(expression{})+unsafe.Sizeof(string("")))
	if bytes > b.options.Limits.MaxBytes {
		return call, bindError("54000", "function argument metadata limit exceeded", pos)
	}
	call.values = make([]expression, len(nodes))
	for i, node := range nodes {
		if named := node.GetNamedArgExpr(); named != nil {
			if call.names == nil {
				call.names = make([]string, len(nodes))
			}
			call.names[i] = named.Name
			node = named.Arg
		}
		value, err := b.scalar(node, s)
		if err != nil {
			return call, err
		}
		call.values[i] = value
	}
	namedSeen := false
	for i, name := range call.names {
		if name == "" {
			if namedSeen {
				return call, bindError("42601", "positional argument cannot follow named argument", pos)
			}
			continue
		}
		namedSeen = true
		for _, prior := range call.names[:i] {
			if prior == name {
				return call, bindError("42601", "argument name used more than once", pos)
			}
		}
	}
	return call, nil
}

func (call callArguments) order(count int, nameAt func(int) (string, bool), hasDefault func(int) bool) ([builtin.MaxCallArguments]int16, bool) {
	var order [builtin.MaxCallArguments]int16
	if count > len(order) || len(call.values) > count {
		return order, false
	}
	for i := range count {
		order[i] = -1
	}
	for i := range call.values {
		position := i
		if call.names != nil && call.names[i] != "" {
			position = -1
			for candidate := range count {
				if name, ok := nameAt(candidate); ok && name == call.names[i] {
					position = candidate
					break
				}
			}
		}
		if position < 0 || position >= count || order[position] >= 0 {
			return order, false
		}
		order[position] = int16(i)
	}
	for i := range count {
		if order[i] < 0 && !hasDefault(i) {
			return order, false
		}
	}
	return order, true
}

func (call callArguments) projectBuiltin(signature builtin.Signature, variadicArray bool) (builtin.Signature, bool) {
	if variadicArray {
		if signature.Variadic == 0 || len(call.values) > int(signature.ArgumentCount) {
			return signature, false
		}
		if signature.Variadic != 2276 {
			signature.Variadic = 0
		}
	} else if signature.Variadic != 0 && len(call.values) >= int(signature.ArgumentCount) {
		// PostgreSQL expands at least one element; omitted arguments use defaults.
		signature.Arguments[signature.ArgumentCount-1] = signature.Variadic
	}
	if signature.Variadic != 0 && call.names != nil && !variadicArray {
		return signature, false
	}
	if call.names == nil && len(call.values) == int(signature.ArgumentCount) {
		return signature, true
	}
	if call.names == nil && signature.Variadic != 0 && len(call.values) >= int(signature.ArgumentCount) {
		return signature, true
	}
	parameters, _ := builtin.LookupFunctionParameters(signature.ID)
	order, ok := call.order(int(signature.ArgumentCount), parameters.ArgumentName, func(i int) bool { _, ok := parameters.DefaultSQL(i); return ok })
	if !ok {
		return signature, false
	}
	projected := signature
	projected.Arguments = [8]model.TypeID{}
	projected.ArgumentCount = uint8(len(call.values))
	for target, source := range order[:signature.ArgumentCount] {
		if source >= 0 {
			projected.Arguments[source] = signature.Arguments[target]
		}
	}
	if !variadicArray {
		projected.Variadic = 0
	}
	return projected, true
}

func (b *binder) completeBuiltinArguments(signature builtin.Signature, call callArguments, s *scope, pos int) ([]expression, error) {
	if call.names == nil && len(call.values) >= int(signature.ArgumentCount) {
		return call.values, nil
	}
	parameters, _ := builtin.LookupFunctionParameters(signature.ID)
	order, ok := call.order(int(signature.ArgumentCount), parameters.ArgumentName, func(i int) bool { _, ok := parameters.DefaultSQL(i); return ok })
	if !ok {
		return nil, bindError("42883", "function does not exist", pos)
	}
	bytes := uint64(signature.ArgumentCount) * uint64(unsafe.Sizeof(expression{}))
	if bytes > b.options.Limits.MaxBytes {
		return nil, bindError("54000", "function argument metadata limit exceeded", pos)
	}
	result := make([]expression, signature.ArgumentCount)
	for i, source := range order[:signature.ArgumentCount] {
		if source >= 0 {
			result[i] = call.values[source]
			continue
		}
		sql, _ := parameters.DefaultSQL(i)
		value, err := b.defaultArgument(sql, s, pos)
		if err != nil {
			return nil, err
		}
		result[i] = value
	}
	return result, nil
}

func (b *binder) defaultArgument(sql string, s *scope, pos int) (expression, error) {
	node, err := b.defaultExpression(sql, pos)
	if err != nil {
		return expression{}, err
	}
	scope := *s
	scope.allowAggregate, scope.allowWindow = false, false
	return b.scalar(node, &scope)
}

func (b *binder) defaultExpression(sql string, pos int) (*zpgparse.Node, error) {
	if uint64(len(sql))+9 > b.options.Limits.MaxBytes {
		return nil, bindError("54000", "default source limit exceeded", pos)
	}
	parsed, err := zpgparse.Parse("SELECT (" + sql + ")")
	if err != nil {
		return nil, err
	}
	if len(parsed.Stmts) != 1 {
		return nil, bindError("XX000", "invalid default definition", pos)
	}
	selectStmt := parsed.Stmts[0].Stmt.GetSelectStmt()
	if selectStmt == nil || len(selectStmt.TargetList) != 1 || len(selectStmt.FromClause) != 0 {
		return nil, bindError("XX000", "invalid default definition", pos)
	}
	target := selectStmt.TargetList[0].GetResTarget()
	if target == nil || target.Val == nil {
		return nil, bindError("XX000", "invalid default definition", pos)
	}
	remaining, payload := b.options.Limits.MaxNodes, b.options.Limits.MaxBytes
	if err := b.checkAST(target.Val.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	if err := validateDefaultExpression(b.ctx, target.Val, pos); err != nil {
		return nil, err
	}
	setDefaultPositions(target.Val.ProtoReflect(), int32(pos-1))
	return target.Val, nil
}

func validateDefaultExpression(ctx context.Context, expression *zpgparse.Node, pos int) error {
	return rewriteIndexAST(ctx, expression.ProtoReflect(), func(node *zpgparse.Node) error {
		if node.GetParamRef() != nil {
			return bindError("42P02", "function defaults cannot reference parameters", pos)
		}
		if node.GetColumnRef() != nil {
			return bindError("42703", "function defaults cannot reference columns", pos)
		}
		if node.GetSubLink() != nil {
			return bindError("0A000", "function defaults cannot contain subqueries", pos)
		}
		return nil
	})
}

func setDefaultPositions(message protoreflect.Message, position int32) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() == protoreflect.Int32Kind && strings.HasSuffix(string(field.Name()), "location") {
			message.Set(field, protoreflect.ValueOfInt32(position))
			return true
		}
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			list := value.List()
			for i := range list.Len() {
				setDefaultPositions(list.Get(i).Message(), position)
			}
		} else {
			setDefaultPositions(value.Message(), position)
		}
		return true
	})
}
