package builtin

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	numericAggregateTag byte = 'N'
	integerAggregateTag byte = 'I'
	intervalAggregateTag byte = 'V'
	booleanAggregateTag byte = 'B'
	textAggregateTag byte = 'S'
	bytesAggregateTag byte = 's'
)

func compileInternalAggregateSupport(signature Signature) scalarKernel {
	switch signature.ID {
	case 1833, 1834, 1835, 1836, 2858, 3548, 3567, 3568, 3569:
		return compileNumericInternalTransition(signature.ID, numericAggregateTag)
	case 1837, 1838, 1839, 2514, 2596, 3178:
		return compileNumericInternalFinal(signature.ID, numericAggregateTag)
	case 3337, 3338, 3341:
		return compileNumericInternalCombine(numericAggregateTag)
	case 2740, 3335, 3339:
		return serializeInternalState
	case 2741, 3336, 3340:
		return deserializeInternalState
	case 2746, 3387:
		return compileNumericInternalTransition(signature.ID, integerAggregateTag)
	case 2785:
		return compileNumericInternalCombine(integerAggregateTag)
	case 2786:
		return serializeInternalState
	case 2787:
		return deserializeInternalState
	case 3388, 3389, 3390, 3391, 3392, 3393:
		return compileNumericInternalFinal(signature.ID, integerAggregateTag)
	case 1843, 3549:
		return compileIntervalInternalTransition(signature.ID)
	case 1844, 6326:
		return compileIntervalInternalFinal(signature.ID)
	case 3325:
		return intervalInternalCombine
	case 6324:
		return serializeInternalState
	case 6325:
		return deserializeInternalState
	case 3496, 3497:
		return compileBooleanInternalTransition(signature.ID)
	case 3498, 3499:
		return compileBooleanInternalFinal(signature.ID)
	case 3535, 3543:
		return compileStringInternalTransition(signature.ID)
	case 3536, 3544:
		return compileStringInternalFinal(signature.ID)
	case 6299:
		return stringInternalCombine
	case 6300:
		return serializeInternalState
	case 6301:
		return deserializeInternalState
	case 3174, 3196, 3266, 3269:
		return compileJSONAggregateFinal(signature.ID)
	case 6293:
		return compileAggregateFragmentCombine(arrayElementAggregateTag)
	case 6294, 6297:
		return serializeInternalState
	case 6295, 6298:
		return deserializeInternalState
	case 6296:
		return compileAggregateFragmentCombine(arrayArrayAggregateTag)
	default:
		return nil
	}
}

func decodeInternalState(value model.Value, tag byte, count int, defaults ...string) ([]string, error) {
	if value.IsNull {
		if len(defaults) != count { return nil, sqlError("XX000", "invalid aggregate state defaults") }
		return append([]string(nil), defaults...), nil
	}
	if value.Type != model.TypeBytes || len(value.BytesVal) == 0 || value.BytesVal[0] != tag {
		return nil, sqlError("22023", "invalid executor-owned aggregate state")
	}
	fields := strings.Split(string(value.BytesVal[1:]), "\x00")
	if len(fields) != count { return nil, sqlError("22023", "invalid executor-owned aggregate state") }
	return fields, nil
}

func encodeInternalState(ctx context.Context, budget CallBudget, tag byte, fields ...string) (model.Value, error) {
	if err := ctx.Err(); err != nil { return model.Value{}, err }
	size := 1
	for _, field := range fields { size += len(field) }
	if len(fields) > 1 { size += len(fields)-1 }
	if err := outputBudget(int64(size), budget.OutputBytes); err != nil { return model.Value{}, err }
	payload := make([]byte, 1, size)
	payload[0] = tag
	for index, field := range fields { if index != 0 { payload = append(payload, 0) }; payload = append(payload, field...) }
	return model.BytesVal(payload), nil
}

func compileNumericInternalTransition(id model.FunctionID, tag byte) scalarKernel {
	inverse := id == 3548 || id == 3567 || id == 3568 || id == 3569 || id == 3387
	return func(ctx context.Context, values []model.Value, budget CallBudget) (result model.Value, err error) {
		fields, err := decodeInternalState(values[0], tag, 3, "0", "0", "0")
		if err != nil { return model.Value{}, err }
		if values[1].IsNull { return encodeInternalState(ctx, budget, tag, fields...) }
		count, err := strconv.ParseInt(fields[0], 10, 64); if err != nil { return model.Value{}, sqlError("22023", "invalid aggregate count") }
		delta := int64(1); operation := opPl
		if inverse { delta, operation = -1, opMi }
		count, overflow := checkedInt64Add(count, delta); if overflow || count < 0 { return model.Value{}, sqlError("22003", "aggregate count out of range") }
		input := values[1].StrVal
		if values[1].Type == model.TypeInt64 { input = strconv.FormatInt(values[1].IntVal, 10) }
		work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		sum, err := parseNumeric(ctx, work, fields[1]); if err != nil { return model.Value{}, err }
		squares, err := parseNumeric(ctx, work, fields[2]); if err != nil { return model.Value{}, err }
		number, err := parseNumeric(ctx, work, input); if err != nil { return model.Value{}, err }
		nextSum, err := numericArithmetic(ctx, work, sum, number, operation); if err != nil { return model.Value{}, err }
		square, err := numericArithmetic(ctx, work, number, number, opMul); if err != nil { return model.Value{}, err }
		nextSquares, err := numericArithmetic(ctx, work, squares, square, operation); if err != nil { return model.Value{}, err }
		sumText, err := numericText(ctx, nextSum, budget.OutputBytes); if err != nil { return model.Value{}, err }
		squaresText, err := numericText(ctx, nextSquares, budget.OutputBytes); if err != nil { return model.Value{}, err }
		return encodeInternalState(ctx, budget, tag, strconv.FormatInt(count, 10), sumText, squaresText)
	}
}

func compileNumericInternalCombine(tag byte) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (result model.Value, err error) {
		left, err := decodeInternalState(values[0], tag, 3, "0", "0", "0"); if err != nil { return model.Value{}, err }
		right, err := decodeInternalState(values[1], tag, 3, "0", "0", "0"); if err != nil { return model.Value{}, err }
		leftCount, err := strconv.ParseInt(left[0], 10, 64); if err != nil { return model.Value{}, sqlError("22023", "invalid aggregate count") }
		rightCount, err := strconv.ParseInt(right[0], 10, 64); if err != nil { return model.Value{}, sqlError("22023", "invalid aggregate count") }
		count, overflow := checkedInt64Add(leftCount, rightCount); if overflow { return model.Value{}, sqlError("22003", "aggregate count out of range") }
		work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		combined := [2]string{}
		for index := range combined {
			a, err := parseNumeric(ctx, work, left[index+1]); if err != nil { return model.Value{}, err }
			b, err := parseNumeric(ctx, work, right[index+1]); if err != nil { return model.Value{}, err }
			sum, err := numericArithmetic(ctx, work, a, b, opPl); if err != nil { return model.Value{}, err }
			combined[index], err = numericText(ctx, sum, budget.OutputBytes); if err != nil { return model.Value{}, err }
		}
		return encodeInternalState(ctx, budget, tag, strconv.FormatInt(count, 10), combined[0], combined[1])
	}
}

func compileNumericInternalFinal(id model.FunctionID, tag byte) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (result model.Value, err error) {
		fields, err := decodeInternalState(values[0], tag, 3, "0", "0", "0"); if err != nil { return model.Value{}, err }
		count, err := strconv.ParseInt(fields[0], 10, 64); if err != nil { return model.Value{}, sqlError("22023", "invalid aggregate count") }
		sumOnly := id == 3178 || id == 3388
		average := id == 1837 || id == 3389
		sample := id == 1838 || id == 1839 || id == 3391 || id == 3393
		deviation := id == 1839 || id == 2596 || id == 3392 || id == 3393
		if count == 0 || sample && count <= 1 { return model.NullVal(model.TypeNumeric), nil }
		work, err := budget.NewWorkspace(ctx); if err != nil { return model.Value{}, numericAllocationError(err) }
		defer func() { err = numericAllocationError(errors.Join(err, work.Close())) }()
		sum, err := parseNumeric(ctx, work, fields[1]); if err != nil { return model.Value{}, err }
		if sumOnly { return numericValue(ctx, sum, budget.OutputLimit(work)) }
		n, err := work.FromInt64(ctx, count); if err != nil { return model.Value{}, err }
		if average { quotient, err := numericDivide(ctx, work, sum, decimal{coefficient:n}, false); if err != nil { return model.Value{}, err }; return numericValue(ctx, quotient, budget.OutputLimit(work)) }
		squares, err := parseNumeric(ctx, work, fields[2]); if err != nil { return model.Value{}, err }
		sumSquared, err := numericArithmetic(ctx, work, sum, sum, opMul); if err != nil { return model.Value{}, err }
		scaledSquares, err := numericArithmetic(ctx, work, decimal{coefficient:n}, squares, opMul); if err != nil { return model.Value{}, err }
		numerator, err := numericArithmetic(ctx, work, scaledSquares, sumSquared, opMi); if err != nil { return model.Value{}, err }
		if numerator.special == numericFinite && numerator.coefficient.Sign() <= 0 { zero, e := work.FromInt64(ctx, 0); if e != nil { return model.Value{}, e }; return numericValue(ctx, decimal{coefficient:zero}, budget.OutputLimit(work)) }
		factor := count; if sample { factor-- }
		factorValue, err := work.FromInt64(ctx, factor); if err != nil { return model.Value{}, err }
		denominator, err := numericArithmetic(ctx, work, decimal{coefficient:n}, decimal{coefficient:factorValue}, opMul); if err != nil { return model.Value{}, err }
		quotient, err := numericDivide(ctx, work, numerator, denominator, false); if err != nil { return model.Value{}, err }
		if deviation { quotient, err = numericSqrt(ctx, work, quotient, quotient.scale); if err != nil { return model.Value{}, err } }
		return numericValue(ctx, quotient, budget.OutputLimit(work))
	}
}

func serializeInternalState(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	if values[0].IsNull { return model.NullVal(model.TypeBytes), nil }
	if values[0].Type != model.TypeBytes { return model.Value{}, sqlError("22023", "invalid executor-owned aggregate state") }
	return ownedBytes(ctx, budget.OutputBytes, values[0].BytesVal)
}

func deserializeInternalState(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
	if values[0].IsNull { return model.NullVal(model.TypeBytes), nil }
	return ownedBytes(ctx, budget.OutputBytes, values[0].BytesVal)
}

func compileIntervalInternalTransition(id model.FunctionID) scalarKernel {
	inverse := id == 3549
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		fields, err := decodeInternalState(values[0], intervalAggregateTag, 4, "0", "0", "0", "0"); if err != nil { return model.Value{}, err }
		if values[1].IsNull { return encodeInternalState(ctx, budget, intervalAggregateTag, fields...) }
		parts := [4]int64{}; for index, field := range fields { parts[index], err = strconv.ParseInt(field,10,64); if err != nil { return model.Value{}, sqlError("22023", "invalid interval aggregate state") } }
		deltas := [4]int64{1,int64(values[1].IntervalVal.Months),int64(values[1].IntervalVal.Days),values[1].IntervalVal.Microseconds}; if inverse { for index := range deltas { if deltas[index] == -1<<63 { return model.Value{}, sqlError("22015", "interval field overflow") }; deltas[index] = -deltas[index] } }
		for index := range parts { var overflow bool; parts[index],overflow=checkedInt64Add(parts[index],deltas[index]);if overflow{return model.Value{},sqlError("22015","interval field overflow")} }
		return encodeInternalState(ctx,budget,intervalAggregateTag,strconv.FormatInt(parts[0],10),strconv.FormatInt(parts[1],10),strconv.FormatInt(parts[2],10),strconv.FormatInt(parts[3],10))
	}
}

func intervalInternalCombine(ctx context.Context, values []model.Value, budget CallBudget) (model.Value,error) {
	left,err:=decodeInternalState(values[0],intervalAggregateTag,4,"0","0","0","0");if err!=nil{return model.Value{},err};right,err:=decodeInternalState(values[1],intervalAggregateTag,4,"0","0","0","0");if err!=nil{return model.Value{},err};result:=[4]string{}
	for index:=range result{a,e:=strconv.ParseInt(left[index],10,64);if e!=nil{return model.Value{},sqlError("22023","invalid interval aggregate state")};b,e:=strconv.ParseInt(right[index],10,64);if e!=nil{return model.Value{},sqlError("22023","invalid interval aggregate state")};sum,overflow:=checkedInt64Add(a,b);if overflow{return model.Value{},sqlError("22015","interval field overflow")};result[index]=strconv.FormatInt(sum,10)}
	return encodeInternalState(ctx,budget,intervalAggregateTag,result[:]...)
}

func compileIntervalInternalFinal(id model.FunctionID) scalarKernel {
	return func(_ context.Context, values []model.Value, _ CallBudget) (model.Value, error) {
		fields, err := decodeInternalState(values[0], intervalAggregateTag, 4, "0", "0", "0", "0")
		if err != nil { return model.Value{}, err }
		parts := [4]int64{}
		for index, field := range fields {
			parts[index], err = strconv.ParseInt(field, 10, 64)
			if err != nil { return model.Value{}, sqlError("22023", "invalid interval aggregate state") }
		}
		if parts[0] == 0 { return model.NullVal(model.TypeInterval), nil }
		if parts[1] < -1<<31 || parts[1] > 1<<31-1 || parts[2] < -1<<31 || parts[2] > 1<<31-1 {
			return model.Value{}, sqlError("22015", "interval field overflow")
		}
		total := model.Interval{Months: int32(parts[1]), Days: int32(parts[2]), Microseconds: parts[3]}
		if id == 1844 { return intervalScale(total, float64(parts[0]), true) }
		return intervalValue(total), nil
	}
}

func compileBooleanInternalTransition(id model.FunctionID) scalarKernel { inverse:=id==3497;return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){if values[1].IsNull{if values[0].IsNull{return model.NullVal(model.TypeBytes),nil};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal)};fields,err:=decodeInternalState(values[0],booleanAggregateTag,2,"0","0");if err!=nil{return model.Value{},err};count,err:=strconv.ParseInt(fields[0],10,64);if err!=nil{return model.Value{},sqlError("22023","invalid boolean aggregate state")};trueCount,err:=strconv.ParseInt(fields[1],10,64);if err!=nil{return model.Value{},sqlError("22023","invalid boolean aggregate state")};delta:=int64(1);if inverse{delta=-1};count,overflow:=checkedInt64Add(count,delta);if overflow||count<0{return model.Value{},sqlError("22003","aggregate count out of range")};if values[1].IntVal!=0{trueCount,overflow=checkedInt64Add(trueCount,delta);if overflow||trueCount<0{return model.Value{},sqlError("22003","aggregate count out of range")}};return encodeInternalState(ctx,budget,booleanAggregateTag,strconv.FormatInt(count,10),strconv.FormatInt(trueCount,10))} }
func compileBooleanInternalFinal(id model.FunctionID) scalarKernel { return func(_ context.Context,values []model.Value,_ CallBudget)(model.Value,error){fields,err:=decodeInternalState(values[0],booleanAggregateTag,2,"0","0");if err!=nil{return model.Value{},err};count,err:=strconv.ParseInt(fields[0],10,64);if err!=nil{return model.Value{},sqlError("22023","invalid boolean aggregate state")};trueCount,err:=strconv.ParseInt(fields[1],10,64);if err!=nil{return model.Value{},sqlError("22023","invalid boolean aggregate state")};if count==0{return model.NullVal(model.TypeBoolean),nil};if id==3498{return model.BoolVal(trueCount==count),nil};return model.BoolVal(trueCount!=0),nil} }

func compileStringInternalTransition(id model.FunctionID) scalarKernel { tag:=textAggregateTag;if id==3543{tag=bytesAggregateTag};return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){if values[1].IsNull{if values[0].IsNull{return model.NullVal(model.TypeBytes),nil};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal)};value:=[]byte(values[1].StrVal);delimiter:=[]byte(values[2].StrVal);if tag==bytesAggregateTag{value=values[1].BytesVal;delimiter=values[2].BytesVal};if values[0].IsNull{header:=strconv.AppendInt([]byte{tag},int64(len(delimiter)),10);header=append(header,0);return ownedBytes(ctx,budget.OutputBytes,header,delimiter,value)};fields:=values[0].BytesVal;if len(fields)<2||fields[0]!=tag{return model.Value{},sqlError("22023","invalid string aggregate state")};separator:=bytes.IndexByte(fields[1:],0);if separator<0{return model.Value{},sqlError("22023","invalid string aggregate state")};return ownedBytes(ctx,budget.OutputBytes,fields,delimiter,value)} }
func compileStringInternalFinal(id model.FunctionID) scalarKernel { return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){if values[0].IsNull{if id==3536{return model.NullVal(model.TypeString),nil};return model.NullVal(model.TypeBytes),nil};payload,_,err:=stringInternalPayload(values[0]);if err!=nil{return model.Value{},err};if id==3536{return ownedText(ctx,budget.OutputBytes,string(payload))};return ownedBytes(ctx,budget.OutputBytes,payload)} }
func stringInternalCombine(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){if values[0].IsNull&&values[1].IsNull{return model.NullVal(model.TypeBytes),nil};if values[0].IsNull{return ownedBytes(ctx,budget.OutputBytes,values[1].BytesVal)};if values[1].IsNull{return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal)};right,delimiter,err:=stringInternalPayload(values[1]);if err!=nil{return model.Value{},err};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal,delimiter,right)}
func stringInternalPayload(value model.Value)([]byte,[]byte,error){if value.Type!=model.TypeBytes||len(value.BytesVal)<2||(value.BytesVal[0]!=textAggregateTag&&value.BytesVal[0]!=bytesAggregateTag){return nil,nil,sqlError("22023","invalid string aggregate state")};separator:=bytes.IndexByte(value.BytesVal[1:],0);if separator<0{return nil,nil,sqlError("22023","invalid string aggregate state")};length,err:=strconv.Atoi(string(value.BytesVal[1:1+separator]));if err!=nil||length<0||1+separator+1+length>len(value.BytesVal){return nil,nil,sqlError("22023","invalid string aggregate state")};start:=1+separator+1;return value.BytesVal[start+length:],value.BytesVal[start:start+length],nil}
