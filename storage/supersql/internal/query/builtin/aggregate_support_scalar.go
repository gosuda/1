package builtin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileSQLCallableSupport(signature Signature) scalarKernel {
	switch signature.ID {
	case 198, 3358, 3407, 5021:
		return nativeRawSend
	case 208, 222:
		return compileFloatAccum(signature.ID)
	case 276:
		return floatAccumCombine
	case 1830, 1831, 1832, 2512, 2513:
		return compileFloatAccumFinal(signature.ID)
	case 1840, 1841:
		return integerSumTransition
	case 1842:
		return int8SumTransition
	case 1962, 1963, 3570, 3571:
		return compileIntegerAvgTransition(signature.ID)
	case 1964, 3572:
		return compileIntegerAvgFinal(signature.ID)
	case 2515:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { return model.BoolVal(values[0].IntVal != 0 && values[1].IntVal != 0), nil }
	case 2516:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { return model.BoolVal(values[0].IntVal != 0 || values[1].IntVal != 0), nil }
	case 2805:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { if values[1].IsNull || values[2].IsNull { return values[0], nil }; if values[0].IntVal == math.MaxInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }; return model.Int64Val(values[0].IntVal+1), nil }
	case 2806:
		return floatRegressionAccum
	case 2807, 2808, 2809, 2810, 2811, 2812, 2813, 2814, 2815, 2816, 2817:
		return compileFloatRegressionFinal(signature.ID)
	case 3324:
		return integerAvgCombine
	case 3342:
		return floatRegressionCombine
	case 338:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { _, known := LookupOperatorClass(model.OID(values[0].IntVal)); return model.BoolVal(known), nil }
	case 2316:
		return validatePostgresFDWOptions
	case 3163:
		return triggerDepth
	case 3480, 3724:
		return func(ctx context.Context, values []model.Value, _ CallBudget) (model.Value, error) { order, err := compareBytes(ctx, []byte(values[0].StrVal), []byte(values[1].StrVal)); return model.Int64Val(int64(order)), err }
	case 5050:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { info, known := LookupCollation(model.CollationID(values[0].IntVal)); return model.BoolVal(known && info.Deterministic), nil }
	case 5051:
		return func(context.Context, []model.Value, CallBudget) (model.Value, error) { return model.BoolVal(true), nil }
	case 6347:
		return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) { if values[0].IntVal < 1 || values[0].IntVal > 6 { return model.Value{}, sqlError("22023", "invalid GiST compare type") }; return model.Int64Val(values[0].IntVal), nil }
	default:
		return compileInternalAggregateSupport(signature)
	}
}

func floatState(value model.Value, count int) ([]model.Value, error) {
	if value.Type != model.TypeArray || len(value.Dimensions) != 1 || len(value.ArrayVal) != count { return nil, sqlError("2202E", "invalid floating-point aggregate state") }
	for _, field := range value.ArrayVal { if field.IsNull || field.Type != model.TypeFloat64 { return nil, sqlError("2202E", "invalid floating-point aggregate state") } }
	return value.ArrayVal, nil
}

func ownedState(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	value := model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}}
	owned, _, err := model.CopyValue(ctx, value, uint64(max(budget.OutputBytes, 0)))
	if err != nil { return model.Value{}, &model.Error{Code: "54000", Message: "aggregate state exceeds output budget", Cause: err} }
	return owned, nil
}

func compileFloatAccum(_ model.FunctionID) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		state, err := floatState(values[0], 3); if err != nil { return model.Value{}, err }
		n, sum, squares := state[0].FloatVal, state[1].FloatVal, state[2].FloatVal
		x := values[1].FloatVal; next := n+1; delta := x-sum/max(n, 1); sum += x; if n != 0 { squares += delta*delta*n/next }
		return ownedState(ctx, []model.Value{model.Float64Val(next), model.Float64Val(sum), model.Float64Val(squares)}, budget)
	}
}

func floatAccumCombine(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	left, err := floatState(values[0], 3); if err != nil { return model.Value{}, err }; right, err := floatState(values[1], 3); if err != nil { return model.Value{}, err }
	n1, n2 := left[0].FloatVal, right[0].FloatVal; if n1 == 0 { return ownedState(ctx, right, budget) }; if n2 == 0 { return ownedState(ctx, left, budget) }
	n := n1+n2; sum := left[1].FloatVal+right[1].FloatVal; delta := left[1].FloatVal/n1-right[1].FloatVal/n2; squares := left[2].FloatVal+right[2].FloatVal+delta*delta*n1*n2/n
	return ownedState(ctx, []model.Value{model.Float64Val(n), model.Float64Val(sum), model.Float64Val(squares)}, budget)
}

func compileFloatAccumFinal(id model.FunctionID) scalarKernel {
	return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) {
		state, err := floatState(values[0], 3); if err != nil { return model.Value{}, err }; n, sum, squares := state[0].FloatVal, state[1].FloatVal, state[2].FloatVal
		switch id { case 1830: if n == 0 { return model.NullVal(model.TypeFloat64), nil }; return model.Float64Val(sum/n), nil; case 1831, 1832: if n <= 1 { return model.NullVal(model.TypeFloat64), nil }; squares /= n-1; case 2512, 2513: if n == 0 { return model.NullVal(model.TypeFloat64), nil }; squares /= n }
		if id == 1832 || id == 2513 { squares = math.Sqrt(squares) }; return model.Float64Val(squares), nil
	}
}

func integerSumTransition(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) {
	if values[0].IsNull { return model.Int64Val(values[1].IntVal), nil }; if values[1].IsNull { return values[0], nil }
	sum, overflow := checkedInt64Add(values[0].IntVal, values[1].IntVal); if overflow { return model.Value{}, sqlError("22003", "bigint out of range") }; return model.Int64Val(sum), nil
}

func int8SumTransition(ctx context.Context, values []model.Value, budget CallBudget) (result model.Value, err error) {
	if values[0].IsNull { return numericFromPrimitive(ctx, values[1], 20, budget) }
	if values[1].IsNull { copied, _, err := model.CopyValue(ctx, values[0], uint64(max(budget.OutputBytes, 0))); return copied, err }
	work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }; defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
	left, err := parseNumeric(ctx, work, values[0].StrVal); if err != nil { return model.Value{}, err }; rightValue, err := numericFromPrimitive(ctx, values[1], 20, budget); if err != nil { return model.Value{}, err }; right, err := parseNumeric(ctx, work, rightValue.StrVal); if err != nil { return model.Value{}, err }; sum, err := numericArithmetic(ctx, work, left, right, opPl); if err != nil { return model.Value{}, err }; return numericValue(ctx, sum, budget.OutputLimit(work))
}

func compileIntegerAvgTransition(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		state := values[0]
		if state.Type != model.TypeArray || len(state.ArrayVal) != 2 { return model.Value{}, sqlError("2202E", "invalid integer average state") }
		count, sum := state.ArrayVal[0].IntVal, state.ArrayVal[1].IntVal
		inverse := id == 3570 || id == 3571
		countDelta, valueDelta := int64(1), values[1].IntVal
		if inverse { countDelta = -1; if valueDelta == math.MinInt64 { return model.Value{}, sqlError("22003", "bigint out of range") }; valueDelta = -valueDelta }
		count, overflow := checkedInt64Add(count, countDelta); if overflow { return model.Value{}, sqlError("22003", "bigint out of range") }
		sum, overflow = checkedInt64Add(sum, valueDelta); if overflow { return model.Value{}, sqlError("22003", "bigint out of range") }
		return ownedState(ctx, []model.Value{model.Int64Val(count), model.Int64Val(sum)}, budget)
	}
}

func compileIntegerAvgFinal(id model.FunctionID) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (result model.Value, err error) {
		state := values[0]
		if len(state.ArrayVal) != 2 { return model.Value{}, sqlError("2202E", "invalid integer average state") }
		if id == 3572 { return state.ArrayVal[1], nil }
		if state.ArrayVal[0].IntVal == 0 { return model.NullVal(model.TypeNumeric), nil }
		work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		sum, err := parseNumeric(ctx, work, strconv.FormatInt(state.ArrayVal[1].IntVal, 10)); if err != nil { return model.Value{}, err }
		count, err := parseNumeric(ctx, work, strconv.FormatInt(state.ArrayVal[0].IntVal, 10)); if err != nil { return model.Value{}, err }
		average, err := numericArithmetic(ctx, work, sum, count, opDiv); if err != nil { return model.Value{}, err }
		return numericValue(ctx, average, budget.OutputLimit(work))
	}
}

func integerAvgCombine(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	left, right := values[0].ArrayVal, values[1].ArrayVal
	if len(left) != 2 || len(right) != 2 { return model.Value{}, sqlError("2202E", "invalid integer average state") }
	count, overflow := checkedInt64Add(left[0].IntVal, right[0].IntVal); if overflow { return model.Value{}, sqlError("22003", "bigint out of range") }
	sum, overflow := checkedInt64Add(left[1].IntVal, right[1].IntVal); if overflow { return model.Value{}, sqlError("22003", "bigint out of range") }
	return ownedState(ctx, []model.Value{model.Int64Val(count), model.Int64Val(sum)}, budget)
}

func checkedInt64Add(left, right int64) (int64, bool) {
	result := left + right
	return result, right > 0 && result < left || right < 0 && result > left
}

func floatRegressionAccum(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) { state, err := floatState(values[0], 6); if err != nil { return model.Value{}, err }; n, sx, sxx, sy, syy, sxy := state[0].FloatVal,state[1].FloatVal,state[2].FloatVal,state[3].FloatVal,state[4].FloatVal,state[5].FloatVal; x,y := values[2].FloatVal,values[1].FloatVal; next:=n+1; if n==0 { sx,sy=x,y } else { dx,dy:=x-sx/n,y-sy/n; sx+=x;sy+=y;sxx+=dx*dx*n/next;syy+=dy*dy*n/next;sxy+=dx*dy*n/next }; return ownedState(ctx, []model.Value{model.Float64Val(next),model.Float64Val(sx),model.Float64Val(sxx),model.Float64Val(sy),model.Float64Val(syy),model.Float64Val(sxy)}, budget) }

func compileFloatRegressionFinal(id model.FunctionID) scalarKernel { return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value,error) { s,err:=floatState(values[0],6);if err!=nil{return model.Value{},err};n,sx,sxx,sy,syy,sxy:=s[0].FloatVal,s[1].FloatVal,s[2].FloatVal,s[3].FloatVal,s[4].FloatVal,s[5].FloatVal; switch id { case 2807:return model.Float64Val(sxx),nil;case 2808:return model.Float64Val(syy),nil;case 2809:return model.Float64Val(sxy),nil;case 2810:if n==0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sx/n),nil;case 2811:if n==0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sy/n),nil;case 2812:if n==0||sxx==0{return model.NullVal(model.TypeFloat64),nil};if syy==0{return model.Float64Val(1),nil};return model.Float64Val(sxy*sxy/(sxx*syy)),nil;case 2813:if n==0||sxx==0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sxy/sxx),nil;case 2814:if n==0||sxx==0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val((sy-sx*sxy/sxx)/n),nil;case 2815:if n==0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sxy/n),nil;case 2816:if n<=1{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sxy/(n-1)),nil;default:if n==0||sxx<=0||syy<=0{return model.NullVal(model.TypeFloat64),nil};return model.Float64Val(sxy/math.Sqrt(sxx*syy)),nil } } }

func floatRegressionCombine(ctx context.Context, values []model.Value, budget CallBudget) (model.Value,error) { a,err:=floatState(values[0],6);if err!=nil{return model.Value{},err};b,err:=floatState(values[1],6);if err!=nil{return model.Value{},err};if a[0].FloatVal==0{return ownedState(ctx,b,budget)};if b[0].FloatVal==0{return ownedState(ctx,a,budget)};n1,n2:=a[0].FloatVal,b[0].FloatVal;n:=n1+n2;dx:=a[1].FloatVal/n1-b[1].FloatVal/n2;dy:=a[3].FloatVal/n1-b[3].FloatVal/n2;return ownedState(ctx,[]model.Value{model.Float64Val(n),model.Float64Val(a[1].FloatVal+b[1].FloatVal),model.Float64Val(a[2].FloatVal+b[2].FloatVal+dx*dx*n1*n2/n),model.Float64Val(a[3].FloatVal+b[3].FloatVal),model.Float64Val(a[4].FloatVal+b[4].FloatVal+dy*dy*n1*n2/n),model.Float64Val(a[5].FloatVal+b[5].FloatVal+dx*dy*n1*n2/n)},budget) }

func validatePostgresFDWOptions(_ context.Context, values []model.Value, _ CallBudget) (model.Value,error) { catalog:=values[1].IntVal;for _,option:=range values[0].ArrayVal{key,_,ok:=strings.Cut(option.StrVal,"=");if !ok||!validPostgresFDWOption(key,catalog){return model.Value{},sqlError("42601",fmt.Sprintf("invalid option %q",key))}};return model.BoolVal(true),nil }
func validPostgresFDWOption(option string,catalog int64)bool{switch catalog{case 1417:switch option{case "authtype","service","connect_timeout","dbname","host","hostaddr","port","tty","options","requiressl","sslmode","gsslib","gssdelegation":return true};case 1418:return option=="user"||option=="password"};return false}

type triggerDepthRuntime interface { TriggerDepth(context.Context) (int,error) }
func triggerDepth(ctx context.Context,_ []model.Value,budget CallBudget)(model.Value,error){ runtime,ok:=budget.Native.Triggers.(triggerDepthRuntime);if !ok{return model.Value{},sqlError("55000","pg_trigger_depth requires a trigger runtime")};depth,err:=runtime.TriggerDepth(ctx);return model.Int64Val(int64(depth)),err }
