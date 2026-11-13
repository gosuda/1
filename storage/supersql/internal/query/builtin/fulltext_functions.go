package builtin

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileFullText(signature Signature) scalarKernel {
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		switch signature.ID {
		case 3319: return fullTextFilter(ctx,args,budget)
		case 3320: return fullTextSetWeightByLexemes(ctx,args,budget)
		case 3321,3323: return fullTextDelete(ctx,args,budget)
		case 3326: return fullTextVectorToArray(ctx,args,budget)
		case 3327: return fullTextArrayToVector(ctx,args,budget)
		case 3610:
			value, err := parseTSVector(ctx,args[0].StrVal,budget.OutputBytes); if err != nil { return model.Value{},err }; return model.BytesVal(value),nil
		case 3611:
			value, err := formatTSVector(ctx,args[0].BytesVal,budget.OutputBytes); if err != nil { return model.Value{},err }; return model.StringVal(value),nil
		case 3612:
			value, err := parseTSQuery(ctx,args[0].StrVal,budget.OutputBytes); if err != nil { return model.Value{},err }; return model.BytesVal(value),nil
		case 3613:
			value, err := formatTSQuery(ctx,args[0].BytesVal,budget.OutputBytes); if err != nil { return model.Value{},err }; return model.StringVal(value),nil
		case 3616,3617,3618,3619,3620,3621,3622: return fullTextVectorCompare(ctx,signature.ID,args)
		case 3623: return fullTextStrip(ctx,args,budget)
		case 3624: return fullTextSetWeight(ctx,args,budget)
		case 3625: return fullTextConcat(ctx,args,budget)
		case 3634: return fullTextMatch(ctx,args[0],args[1])
		case 3635: return fullTextMatch(ctx,args[1],args[0])
		case 3638,3640: return fullTextSend(args[0],budget)
		case 3639,3641: return fullTextReceive(ctx,signature.ID,args[0],budget)
		case 3662,3663,3664,3665,3666,3667,3668: return fullTextQueryCompare(ctx,signature.ID,args)
		case 3669,3670,3671,5003,5004: return fullTextQueryJoin(ctx,signature.ID,args,budget)
		case 3672:
			_,count,err:=decodeTSQuery(ctx,args[0].BytesVal); if err!=nil{return model.Value{},err}; return model.Int64Val(int64(count)),nil
		case 3673: return fullTextQueryTree(ctx,args,budget)
		case 3684: return fullTextRewrite(ctx,args,budget)
		case 3685:
			if budget.TextSearch==nil{return model.Value{},sqlError("55000","text search query runtime is unavailable")}; data,err:=budget.TextSearch.RewriteTextSearchQuery(ctx,args[0].BytesVal,args[1].StrVal,uint64(budget.OutputBytes));if err!=nil{return model.Value{},err};if _,_,err=decodeTSQuery(ctx,data);err!=nil{return model.Value{},err};return model.BytesVal(data),nil
		case 3691,3692: return fullTextQueryContains(ctx,signature.ID,args)
		case 3703,3704,3705,3706,3707,3708,3709,3710: return fullTextRank(ctx,signature.ID,args)
		case 3711:
			vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};return model.Int64Val(int64(len(vector))),nil
		case 3723: return fullTextLexize(ctx,args,budget)
		case 3736: return fullTextRegInput(ctx,true,args[0].StrVal,budget)
		case 3737: return fullTextRegOutput(ctx,true,model.OID(args[0].IntVal),budget)
		case 3738: return regReceive(args[0])
		case 3739: return regSend(ctx,args[0],budget)
		case 3743,3744,3754,3755: return fullTextHeadline(ctx,signature.ID,args,budget)
		case 3745,3746,3747,3749,3750,3751,5001,5006,5007,5009: return fullTextConstruct(ctx,signature.ID,args,budget)
		case 3756,3757,3758,3768: return fullTextVisible(ctx,signature.ID,model.OID(args[0].IntVal),budget)
		case 3759:
			configuration,err:=resolveTextSearchConfiguration(ctx,nil,budget);if err!=nil{return model.Value{},err};return model.Int64Val(int64(configuration.OID)),nil
		case 3760: return fullTextTextMatch(ctx,args[0].StrVal,nil,args[1].StrVal,budget)
		case 3761: return fullTextTextMatch(ctx,args[0].StrVal,&args[1],"",budget)
		case 3771: return fullTextRegInput(ctx,false,args[0].StrVal,budget)
		case 3772: return fullTextRegOutput(ctx,false,model.OID(args[0].IntVal),budget)
		case 3773: return regReceive(args[0])
		case 3774: return regSend(ctx,args[0],budget)
		case 4201,4202,4203,4204,4205,4206,4207,4208: return fullTextJSONHeadline(ctx,signature.ID,args,budget)
		case 4209,4210,4211,4212,4213,4214,4215,4216: return fullTextJSONVector(ctx,signature.ID,args,budget)
		default: return model.Value{},&MissingImplementationError{Signature:signature}
		}
	}
}

func resolveTextSearchConfiguration(ctx context.Context, explicit *model.Value, budget CallBudget) (model.TextSearchConfiguration,error) {
	if explicit != nil {
		oid:=model.OID(explicit.IntVal)
		if configuration,ok:=builtinTextSearchConfiguration(oid);ok{return configuration,nil}
		if budget.TextSearch==nil{return model.TextSearchConfiguration{},sqlError("42704","text search configuration does not exist")}
		return budget.TextSearch.TextSearchConfiguration(ctx,oid)
	}
	if budget.Settings==nil{return model.TextSearchConfiguration{},sqlError("55000","default_text_search_config runtime is unavailable")}
	value,err:=budget.Settings.CurrentSetting(ctx,"default_text_search_config",false);if err!=nil{return model.TextSearchConfiguration{},err}
	if configuration,ok:=builtinTextSearchConfigurationByName(value.StrVal);ok{return configuration,nil}
	if budget.TextSearch==nil{return model.TextSearchConfiguration{},sqlError("42704","text search configuration does not exist")}
	return budget.TextSearch.ResolveTextSearchConfiguration(ctx,value.StrVal)
}

func fullTextRegInput(ctx context.Context, configuration bool, input string, budget CallBudget) (model.Value,error) {
	text:=strings.TrimSpace(input)
	if oid,err:=strconv.ParseUint(text,10,32);err==nil{return model.Int64Val(int64(oid)),nil}
	if configuration {
		if value,ok:=builtinTextSearchConfigurationByName(text);ok{return model.Int64Val(int64(value.OID)),nil}
		if budget.TextSearch!=nil { value,err:=budget.TextSearch.ResolveTextSearchConfiguration(ctx,text);if err==nil{return model.Int64Val(int64(value.OID)),nil};return model.Value{},err }
	} else {
		if value,ok:=builtinTextSearchDictionaryByName(text);ok{return model.Int64Val(int64(value.OID)),nil}
		if budget.TextSearch!=nil { value,err:=budget.TextSearch.ResolveTextSearchDictionary(ctx,text);if err==nil{return model.Int64Val(int64(value.OID)),nil};return model.Value{},err }
	}
	return model.Value{},sqlError("42704","text search object does not exist")
}

func fullTextRegOutput(ctx context.Context, configuration bool, oid model.OID, budget CallBudget) (model.Value,error) {
	var name string
	var ok bool
	if configuration { name,ok=builtinTextSearchConfigurationName(oid) } else { name,ok=builtinTextSearchDictionaryName(oid) }
	if ok { name=strings.TrimPrefix(name,"pg_catalog.");if err:=outputBudget(int64(len(name)),budget.OutputBytes);err!=nil{return model.Value{},err};return model.StringVal(name),nil }
	if budget.TextSearch!=nil {
		var err error
		if configuration{name,err=budget.TextSearch.TextSearchConfigurationName(ctx,oid,uint64(budget.OutputBytes))}else{name,err=budget.TextSearch.TextSearchDictionaryName(ctx,oid,uint64(budget.OutputBytes))}
		if err==nil{return model.StringVal(name),nil}
		var state *model.Error;if !errors.As(err,&state)||state.Code!="42704"{return model.Value{},err}
	}
	name=strconv.FormatUint(uint64(oid),10);if err:=outputBudget(int64(len(name)),budget.OutputBytes);err!=nil{return model.Value{},err};return model.StringVal(name),nil
}

func fullTextSend(value model.Value,budget CallBudget)(model.Value,error){if err:=outputBudget(int64(len(value.BytesVal)),budget.OutputBytes);err!=nil{return model.Value{},err};return model.BytesVal(append([]byte(nil),value.BytesVal...)),nil}
func fullTextReceive(ctx context.Context,id model.FunctionID,value model.Value,budget CallBudget)(model.Value,error){data:=value.BytesVal;typ:=model.TypeID(3614);if id==3641{typ=3615};if typ==3614{vector,err:=decodeTSVector(ctx,data);if err!=nil{return model.Value{},err};normalized:=normalizeTSVector(vector);encoded,err:=encodeTSVector(ctx,normalized,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(encoded),nil};if _,_,err:=decodeTSQuery(ctx,data);err!=nil{return model.Value{},err};return fullTextSend(value,budget)}

func fullTextVectorCompare(ctx context.Context,id model.FunctionID,args []model.Value)(model.Value,error){left,e:=decodeTSVector(ctx,args[0].BytesVal);if e!=nil{return model.Value{},e};right,e:=decodeTSVector(ctx,args[1].BytesVal);if e!=nil{return model.Value{},e};order:=compareTSVector(left,right,tsVectorInternalSize(left),tsVectorInternalSize(right));switch id{case 3616:return model.BoolVal(order<0),nil;case 3617:return model.BoolVal(order<=0),nil;case 3618:return model.BoolVal(order==0),nil;case 3619:return model.BoolVal(order!=0),nil;case 3620:return model.BoolVal(order>=0),nil;case 3621:return model.BoolVal(order>0),nil;default:return model.Int64Val(int64(order)),nil}}
func fullTextQueryCompare(ctx context.Context,id model.FunctionID,args []model.Value)(model.Value,error){left,_,e:=decodeTSQuery(ctx,args[0].BytesVal);if e!=nil{return model.Value{},e};right,_,e:=decodeTSQuery(ctx,args[1].BytesVal);if e!=nil{return model.Value{},e};order:=compareTSQuery(left,right);switch id{case 3662:return model.BoolVal(order<0),nil;case 3663:return model.BoolVal(order<=0),nil;case 3664:return model.BoolVal(order==0),nil;case 3665:return model.BoolVal(order!=0),nil;case 3666:return model.BoolVal(order>=0),nil;case 3667:return model.BoolVal(order>0),nil;default:return model.Int64Val(int64(order)),nil}}

func fullTextStrip(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};for i:=range vector{vector[i].positions=nil};data,err:=encodeTSVector(ctx,vector,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextSetWeight(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};weight,err:=tsVectorWeight(args[1]);if err!=nil{return model.Value{},err};for i:=range vector{for j:=range vector[i].positions{vector[i].positions[j]=makeTSPosition(tsPositionNumber(vector[i].positions[j]),weight)}};data,err:=encodeTSVector(ctx,vector,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextSetWeightByLexemes(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};weight,err:=tsVectorWeight(args[1]);if err!=nil{return model.Value{},err};wanted:=make(map[string]struct{},len(args[2].ArrayVal));for _,value:=range args[2].ArrayVal{if !value.IsNull{wanted[value.StrVal]=struct{}{}}};for i:=range vector{if _,ok:=wanted[vector[i].text];ok{for j:=range vector[i].positions{vector[i].positions[j]=makeTSPosition(tsPositionNumber(vector[i].positions[j]),weight)}}};data,err:=encodeTSVector(ctx,vector,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextFilter(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};mask:=uint8(0);for _,value:=range args[1].ArrayVal{if value.IsNull{continue};weight,err:=tsVectorWeight(value);if err!=nil{return model.Value{},err};mask|=1<<weight};out:=vector[:0];for _,lexeme:=range vector{positions:=lexeme.positions[:0];for _,position:=range lexeme.positions{if mask&(1<<tsPositionWeight(position))!=0{positions=append(positions,position)}};if len(positions)!=0{lexeme.positions=positions;out=append(out,lexeme)}};data,err:=encodeTSVector(ctx,out,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextDelete(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};wanted:=map[string]struct{}{};if args[1].Type==model.TypeString{wanted[args[1].StrVal]=struct{}{}}else{for _,value:=range args[1].ArrayVal{if !value.IsNull{wanted[value.StrVal]=struct{}{}}}};out:=vector[:0];for _,lexeme:=range vector{if _,ok:=wanted[lexeme.text];!ok{out=append(out,lexeme)}};data,err:=encodeTSVector(ctx,out,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextConcat(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){left,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};right,err:=decodeTSVector(ctx,args[1].BytesVal);if err!=nil{return model.Value{},err};offset:=maxTSVectorPosition(left);for i:=range right{for j:=range right[i].positions{right[i].positions[j]=makeTSPosition(min(tsMaxPosition,tsPositionNumber(right[i].positions[j])+offset),tsPositionWeight(right[i].positions[j]))}};data,err:=encodeTSVector(ctx,normalizeTSVector(append(left,right...)),budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextVectorToArray(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){vector,err:=decodeTSVector(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};size:=int64(0);values:=make([]model.Value,len(vector));for i,lexeme:=range vector{size+=int64(len(lexeme.text));values[i]=model.StringVal(lexeme.text)};if err:=outputBudget(size,budget.OutputBytes);err!=nil{return model.Value{},err};return fullTextArray(values),nil}
func fullTextArrayToVector(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){lexemes:=make([]tsLexeme,0,len(args[0].ArrayVal));for _,value:=range args[0].ArrayVal{if value.IsNull{return model.Value{},sqlError("22004","array_to_tsvector array must not contain nulls")};lexemes=append(lexemes,tsLexeme{text:value.StrVal})};data,err:=encodeTSVector(ctx,normalizeTSVector(lexemes),budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}

func fullTextMatch(ctx context.Context,vectorValue,queryValue model.Value)(model.Value,error){vector,err:=decodeTSVector(ctx,vectorValue.BytesVal);if err!=nil{return model.Value{},err};query,_,err:=decodeTSQuery(ctx,queryValue.BytesVal);if err!=nil{return model.Value{},err};return model.BoolVal(matchTSQuery(vector,query)),nil}
func fullTextQueryJoin(ctx context.Context,id model.FunctionID,args []model.Value,budget CallBudget)(model.Value,error){left,_,err:=decodeTSQuery(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};var root *tsQueryNode;switch id{case 3671:if left!=nil{root=&tsQueryNode{kind:tsQueryOp,op:tsQueryNot,right:left}};default:right,_,e:=decodeTSQuery(ctx,args[1].BytesVal);if e!=nil{return model.Value{},e};op,distance:=uint8(tsQueryAnd),uint16(0);if id==3670{op=tsQueryOr}else if id==5003||id==5004{op=tsQueryPhrase;distance=1;if id==5004{if args[2].IntVal<0||args[2].IntVal>tsMaxPosition+1{return model.Value{},sqlError("22023","distance in phrase operator is out of range")};distance=uint16(args[2].IntVal)}};root=joinTSQuery(op,distance,left,right)};data,err:=encodeTSQuery(ctx,root,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextRewrite(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){query,_,err:=decodeTSQuery(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};target,_,err:=decodeTSQuery(ctx,args[1].BytesVal);if err!=nil{return model.Value{},err};substitute,_,err:=decodeTSQuery(ctx,args[2].BytesVal);if err!=nil{return model.Value{},err};root:=rewriteTSQuery(query,target,substitute);data,err:=encodeTSQuery(ctx,root,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextQueryContains(ctx context.Context,id model.FunctionID,args []model.Value)(model.Value,error){left,_,err:=decodeTSQuery(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};right,_,err:=decodeTSQuery(ctx,args[1].BytesVal);if err!=nil{return model.Value{},err};if id==3692{left,right=right,left};have,want:=tsQueryValues(left),tsQueryValues(right);i:=0;for _,value:=range want{for i<len(have)&&have[i]<value{i++};if i==len(have)||have[i]!=value{return model.BoolVal(false),nil}};return model.BoolVal(true),nil}

func cleanTSQueryTree(node *tsQueryNode)*tsQueryNode{if node==nil{return nil};if node.kind==tsQueryValue{return cloneTSQuery(node)};if node.op==tsQueryNot{return nil};left,right:=cleanTSQueryTree(node.left),cleanTSQueryTree(node.right);if node.op==tsQueryOr&&(left==nil||right==nil){return nil};if left==nil{return right};if right==nil{return left};return &tsQueryNode{kind:tsQueryOp,op:node.op,distance:node.distance,left:left,right:right}}
func fullTextQueryTree(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){query,_,err:=decodeTSQuery(ctx,args[0].BytesVal);if err!=nil{return model.Value{},err};data,err:=encodeTSQuery(ctx,cleanTSQueryTree(query),int64(tsMaxDataBytes));if err!=nil{return model.Value{},err};text,err:=formatTSQuery(ctx,data,budget.OutputBytes);if err!=nil{return model.Value{},err};if text==""{text="T"};return model.StringVal(text),nil}

func fullTextRank(ctx context.Context,id model.FunctionID,args []model.Value)(model.Value,error){offset:=0;var weightValue *model.Value;if id==3703||id==3704||id==3707||id==3708{weightValue=&args[0];offset=1};weights,err:=tsRankWeights(weightValue);if err!=nil{return model.Value{},err};vector,err:=decodeTSVector(ctx,args[offset].BytesVal);if err!=nil{return model.Value{},err};query,_,err:=decodeTSQuery(ctx,args[offset+1].BytesVal);if err!=nil{return model.Value{},err};normalization:=0;if len(args)>offset+2{normalization=int(args[offset+2].IntVal)};rank:=0.0;if id>=3707{rank=tsRankCD(vector,query,weights,normalization)}else{rank=tsRank(vector,query,weights,normalization)};return model.Float64Val(float64(float32(rank))),nil}
func fullTextLexize(ctx context.Context,args []model.Value,budget CallBudget)(model.Value,error){oid:=model.OID(args[0].IntVal);dictionary,ok:=builtinTextSearchDictionary(oid);if !ok{if budget.TextSearch==nil{return model.Value{},sqlError("42704","text search dictionary does not exist")};var err error;dictionary,err=budget.TextSearch.TextSearchDictionary(ctx,oid);if err!=nil{return model.Value{},err}};lexemes,err:=lexizeTextSearch(ctx,dictionary,args[1].StrVal);if err!=nil{return model.Value{},err};if lexemes==nil{return model.NullVal(model.TypeArray),nil};values:=make([]model.Value,len(lexemes));size:=int64(0);for i,lexeme:=range lexemes{values[i]=model.StringVal(lexeme);size+=int64(len(lexeme))};if err:=outputBudget(size,budget.OutputBytes);err!=nil{return model.Value{},err};return fullTextArray(values),nil}

func fullTextConstruct(ctx context.Context,id model.FunctionID,args []model.Value,budget CallBudget)(model.Value,error){explicit:=id==3745||id==3746||id==3747||id==5006||id==5007;var configValue *model.Value;textIndex:=0;if explicit{configValue=&args[0];textIndex=1};configuration,err:=resolveTextSearchConfiguration(ctx,configValue,budget);if err!=nil{return model.Value{},err};var data []byte;switch id{case 3745,3749:data,err=toTSVector(ctx,configuration,args[textIndex].StrVal,budget.OutputBytes);case 3746,3750:raw,e:=parseTSQuery(ctx,args[textIndex].StrVal,int64(tsMaxDataBytes));if e!=nil{return model.Value{},e};data,err=morphTSQuery(ctx,configuration,raw,budget.OutputBytes);case 3747,3751:data,err=plainTSQuery(ctx,configuration,args[textIndex].StrVal,false,budget.OutputBytes);case 5001,5006:data,err=plainTSQuery(ctx,configuration,args[textIndex].StrVal,true,budget.OutputBytes);case 5007,5009:data,err=websearchTSQuery(ctx,configuration,args[textIndex].StrVal,budget.OutputBytes)};if err!=nil{return model.Value{},err};return model.BytesVal(data),nil}
func fullTextTextMatch(ctx context.Context,text string,queryValue *model.Value,queryText string,budget CallBudget)(model.Value,error){configuration,err:=resolveTextSearchConfiguration(ctx,nil,budget);if err!=nil{return model.Value{},err};vectorData,err:=toTSVector(ctx,configuration,text,int64(tsMaxDataBytes));if err!=nil{return model.Value{},err};var queryData []byte;if queryValue!=nil{queryData=queryValue.BytesVal}else{queryData,err=plainTSQuery(ctx,configuration,queryText,false,int64(tsMaxDataBytes));if err!=nil{return model.Value{},err}};return fullTextMatch(ctx,model.BytesVal(vectorData),model.BytesVal(queryData))}

func fullTextVisible(ctx context.Context,id model.FunctionID,oid model.OID,budget CallBudget)(model.Value,error){
	if id==3756{return model.BoolVal(oid==tsDefaultParserOID),nil}
	if id==3768{
		if oid==3727||oid==3730||oid==3733||oid==3742{return model.BoolVal(true),nil}
		if budget.TextSearch==nil{return model.BoolVal(false),nil}
		name,err:=budget.TextSearch.TextSearchTemplateName(ctx,oid,uint64(budget.OutputBytes))
		return fullTextNameVisible(ctx,name,err,uint64(budget.OutputBytes))
	}
	if id==3757{
		if _,ok:=builtinTextSearchDictionary(oid);ok{return model.BoolVal(true),nil}
		if budget.TextSearch==nil{return model.BoolVal(false),nil}
		name,err:=budget.TextSearch.TextSearchDictionaryName(ctx,oid,uint64(budget.OutputBytes))
		return fullTextNameVisible(ctx,name,err,uint64(budget.OutputBytes))
	}
	if _,ok:=builtinTextSearchConfiguration(oid);ok{return model.BoolVal(true),nil}
	if budget.TextSearch==nil{return model.BoolVal(false),nil}
	name,err:=budget.TextSearch.TextSearchConfigurationName(ctx,oid,uint64(budget.OutputBytes))
	return fullTextNameVisible(ctx,name,err,uint64(budget.OutputBytes))
}

func fullTextNameVisible(ctx context.Context,name string,err error,maximum uint64)(model.Value,error){
	if err!=nil{
		var sql *model.Error
		if errors.As(err,&sql)&&sql.Code=="42704"{return model.BoolVal(false),nil}
		return model.Value{},err
	}
	parts,err:=ParseQualifiedName(ctx,name,maximum)
	if err!=nil{return model.Value{},err}
	return model.BoolVal(len(parts)==1),nil
}

