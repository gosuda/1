package builtin

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	arrayElementAggregateTag byte = 'A'
	arrayArrayAggregateTag byte = 'a'
	jsonArrayAggregateTag byte = 'J'
	jsonObjectAggregateTag byte = 'O'
	jsonbArrayAggregateTag byte = 'j'
	jsonbObjectAggregateTag byte = 'o'
)

func resolveCollectionAggregateTyped(ctx context.Context, signature Signature, arguments []model.TypeLayout, result model.TypeLayout, options ResolveOptions, maximum uint64) (Callable, error) {
	switch signature.ID {
	case 2333, 4051:
		if len(arguments) != 2 || arguments[0].Type.ID != 2281 || arguments[0].Type.Storage != model.TypeBytes || result.Type.ID != 2281 || result.Type.Storage != model.TypeBytes { return Callable{}, sqlError("42804", "array aggregate transition signature mismatch") }
		tag := arrayElementAggregateTag; if signature.ID == 4051 { tag = arrayArrayAggregateTag }
		return nativeTypedCallable(signature, arguments, result, compileArrayAggregateTransition(tag), 0), nil
	case 2334, 4052:
		if len(arguments) != 2 || arguments[0].Type.ID != 2281 || arguments[0].Type.Storage != model.TypeBytes || result.Type.Storage != model.TypeArray { return Callable{}, sqlError("42804", "array aggregate final signature mismatch") }
		tag := arrayElementAggregateTag; if signature.ID == 4052 { tag = arrayArrayAggregateTag }
		return nativeTypedCallable(signature, arguments, result, compileArrayAggregateFinal(tag), 0), nil
	case 3173, 6275, 3265, 6283:
		if len(arguments) != 2 || arguments[0].Type.ID != 2281 || arguments[0].Type.Storage != model.TypeBytes || result.Type.ID != 2281 || result.Type.Storage != model.TypeBytes { return Callable{}, sqlError("42804", "JSON aggregate transition signature mismatch") }
		encoder, err := ResolveJSONValue(ctx, arguments[1], options, maximum); if err != nil { return Callable{}, err }
		tag := jsonArrayAggregateTag; if signature.ID == 3265 || signature.ID == 6283 { tag = jsonbArrayAggregateTag }
		strict := signature.ID == 6275 || signature.ID == 6283
		return nativeTypedCallable(signature, arguments, result, compileJSONArrayTransition(encoder, tag, strict), encoder.OwnedBytes()), nil
	case 3180, 6277, 6278, 6279, 3268, 6285, 6286, 6287:
		if len(arguments) != 3 || arguments[0].Type.ID != 2281 || arguments[0].Type.Storage != model.TypeBytes || result.Type.ID != 2281 || result.Type.Storage != model.TypeBytes { return Callable{}, sqlError("42804", "JSON object aggregate transition signature mismatch") }
		key, err := ResolveJSONValue(ctx, arguments[1], options, maximum); if err != nil { return Callable{}, err }
		if key.OwnedBytes() > maximum { return Callable{}, sqlError("54000", "JSON aggregate metadata budget exceeded") }
		value, err := ResolveJSONValue(ctx, arguments[2], options, maximum-key.OwnedBytes()); if err != nil { return Callable{}, err }
		tag := jsonObjectAggregateTag; if signature.ID == 3268 || signature.ID >= 6285 { tag = jsonbObjectAggregateTag }
		strict := signature.ID == 6277 || signature.ID == 6279 || signature.ID == 6285 || signature.ID == 6287
		unique := signature.ID == 6278 || signature.ID == 6279 || signature.ID == 6286 || signature.ID == 6287
		return nativeTypedCallable(signature, arguments, result, compileJSONObjectTransition(key, value, tag, strict, unique), key.OwnedBytes()+value.OwnedBytes()), nil
	default:
		return Callable{}, &MissingImplementationError{Signature: signature}
	}
}

func compileArrayAggregateTransition(tag byte) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		var encoded bytes.Buffer
		if err := storage.EncodeValue(ctx, &encoded, values[1], uint64(max(budget.OutputBytes, 0))); err != nil { if cause:=ctx.Err();cause!=nil{return model.Value{},cause};return model.Value{}, &model.Error{Code:"54000",Message:"array aggregate state exceeds output budget",Cause:err} }
		return appendAggregateFragment(ctx, values[0], tag, encoded.Bytes(), budget)
	}
}

func compileArrayAggregateFinal(tag byte) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value, error) {
		fragments, err := aggregateFragments(values[0], tag); if err != nil { return model.Value{}, err }
		if values[0].IsNull { return model.NullVal(model.TypeArray), nil }
		if uint64(len(fragments)) > uint64(1<<31-1) { return model.Value{}, sqlError("54000", "array aggregate dimension exceeds int32") }
		decoded := make([]model.Value, len(fragments))
		for index, fragment := range fragments { decoded[index], err = storage.DecodeValue(ctx, fragment, uint64(max(budget.OutputBytes,0))); if err != nil { return model.Value{}, err } }
		if tag == arrayElementAggregateTag {
			result := model.Value{Type:model.TypeArray,ArrayVal:decoded,Dimensions:[]model.ArrayDimension{{LowerBound:1,Length:int32(len(decoded))}}}
			if _,err:=model.ValueBytes(ctx,result,uint64(max(budget.OutputBytes,0)));err!=nil{return model.Value{},&model.Error{Code:"54000",Message:"array aggregate result exceeds output budget",Cause:err}};return result,nil
		}
		if len(decoded)==0 { return model.NullVal(model.TypeArray), nil }
		base:=decoded[0];if base.Type!=model.TypeArray||len(base.Dimensions)==0{return model.Value{},sqlError("2202E","cannot accumulate empty arrays")}
		result:=model.Value{Type:model.TypeArray,Dimensions:make([]model.ArrayDimension,1,len(base.Dimensions)+1)};result.Dimensions[0]=model.ArrayDimension{LowerBound:1,Length:int32(len(decoded))};result.Dimensions=append(result.Dimensions,base.Dimensions...)
		for _,array:=range decoded{if array.Type!=model.TypeArray||!slices.Equal(array.Dimensions,base.Dimensions){return model.Value{},sqlError("2202E","cannot accumulate arrays of different dimensionality")};result.ArrayVal=append(result.ArrayVal,array.ArrayVal...)}
		if _,err:=model.ValueBytes(ctx,result,uint64(max(budget.OutputBytes,0)));err!=nil{return model.Value{},&model.Error{Code:"54000",Message:"array aggregate result exceeds output budget",Cause:err}};return result,nil
	}
}

func compileJSONArrayTransition(encoder *JSONValueEncoder, tag byte, strict bool) scalarKernel {
	return func(ctx context.Context, values []model.Value, budget CallBudget) (model.Value,error) {
		if strict&&values[1].IsNull { if values[0].IsNull{return model.NullVal(model.TypeBytes),nil};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal) }
		encoded,err:=encoder.EncodeValue(ctx,values[1],JSONCompact,budget);if err!=nil{return model.Value{},err}
		return appendAggregateFragment(ctx,values[0],tag,[]byte(encoded.StrVal),budget)
	}
}

func compileJSONObjectTransition(keyEncoder,valueEncoder *JSONValueEncoder, tag byte, strict, unique bool) scalarKernel {
	return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){
		if values[1].IsNull{return model.Value{},sqlError("22004","field name must not be null")}
		if strict&&values[2].IsNull{if values[0].IsNull{return model.NullVal(model.TypeBytes),nil};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal)}
		key,err:=keyEncoder.EncodeKey(ctx,values[1],budget);if err!=nil{return model.Value{},err};value,err:=valueEncoder.EncodeValue(ctx,values[2],JSONCompact,budget);if err!=nil{return model.Value{},err}
		if unique{fragments,err:=aggregateFragments(values[0],tag);if err!=nil{return model.Value{},err};for _,fragment:=range fragments{existing,_,_:=bytes.Cut(fragment,[]byte{':'});if bytes.Equal(existing,[]byte(key.StrVal)){return model.Value{},sqlError("22030","duplicate JSON object key value")}}}
		fragment:=make([]byte,0,len(key.StrVal)+1+len(value.StrVal));fragment=append(fragment,key.StrVal...);fragment=append(fragment,':');fragment=append(fragment,value.StrVal...)
		return appendAggregateFragment(ctx,values[0],tag,fragment,budget)
	}
}

func compileJSONAggregateFinal(id model.FunctionID) scalarKernel {
	return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){
		tag,open,close:=jsonArrayAggregateTag,byte('['),byte(']')
		binaryJSON:=id==3266||id==3269
		if id==3196||id==3269{tag,open,close=jsonObjectAggregateTag,'{','}'}
		if binaryJSON{tag+=32}
		fragments,err:=aggregateFragments(values[0],tag)
		if err!=nil{return model.Value{},err}
		if values[0].IsNull{return model.NullVal(model.TypeJSONB),nil}
		size:=2
		for _,fragment:=range fragments{size+=len(fragment)}
		if len(fragments)>1{size+=len(fragments)-1}
		if err:=outputBudget(int64(size),budget.OutputBytes);err!=nil{return model.Value{},err}
		document:=make([]byte,1,size)
		document[0]=open
		for index,fragment:=range fragments{if index!=0{document=append(document,',')};document=append(document,fragment...)}
		document=append(document,close)
		nested,err:=budget.After(uint64(len(document)));if err!=nil{return model.Value{},err}
		if binaryJSON{return jsonbInput(ctx,[]model.Value{model.StringVal(string(document))},nested)}
		return jsonTextInput(ctx,[]model.Value{model.StringVal(string(document))},nested)
	}
}

func appendAggregateFragment(ctx context.Context,state model.Value,tag byte,fragment []byte,budget CallBudget)(model.Value,error){
	if err:=ctx.Err();err!=nil{return model.Value{},err};if uint64(len(fragment))>uint64(^uint32(0)){return model.Value{},sqlError("54000","aggregate state fragment is too large")}
	prefix:=make([]byte,4);binary.BigEndian.PutUint32(prefix,uint32(len(fragment)))
	if state.IsNull{return ownedBytes(ctx,budget.OutputBytes,[]byte{tag},prefix,fragment)}
	if _,err:=aggregateFragments(state,tag);err!=nil{return model.Value{},err}
	return ownedBytes(ctx,budget.OutputBytes,state.BytesVal,prefix,fragment)
}

func aggregateFragments(state model.Value,tag byte)([][]byte,error){
	if state.IsNull{return nil,nil};if state.Type!=model.TypeBytes||len(state.BytesVal)==0||state.BytesVal[0]!=tag{return nil,sqlError("22023","invalid executor-owned aggregate state")}
	payload:=state.BytesVal[1:];fragments:=make([][]byte,0)
	for len(payload)!=0{if len(payload)<4{return nil,sqlError("22023","invalid executor-owned aggregate state")};length:=int(binary.BigEndian.Uint32(payload));payload=payload[4:];if length>len(payload){return nil,sqlError("22023","invalid executor-owned aggregate state")};fragments=append(fragments,payload[:length]);payload=payload[length:]}
	return fragments,nil
}

func compileAggregateFragmentCombine(tag byte) scalarKernel{return func(ctx context.Context,values []model.Value,budget CallBudget)(model.Value,error){if values[0].IsNull&&values[1].IsNull{return model.NullVal(model.TypeBytes),nil};if values[0].IsNull{if _,err:=aggregateFragments(values[1],tag);err!=nil{return model.Value{},err};return ownedBytes(ctx,budget.OutputBytes,values[1].BytesVal)};if values[1].IsNull{if _,err:=aggregateFragments(values[0],tag);err!=nil{return model.Value{},err};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal)};if _,err:=aggregateFragments(values[0],tag);err!=nil{return model.Value{},err};if _,err:=aggregateFragments(values[1],tag);err!=nil{return model.Value{},err};return ownedBytes(ctx,budget.OutputBytes,values[0].BytesVal,values[1].BytesVal[1:])}}

