package builtin

import (
	"context"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type tsHeadlineOptions struct {
	start, stop, fragment string
	maxWords, minWords, shortWord, maxFragments int
	highlightAll bool
}

func parseTSHeadlineOptions(text string)(tsHeadlineOptions,error){
	options:=tsHeadlineOptions{start:"<b>",stop:"</b>",fragment:" ... ",maxWords:35,minWords:15,shortWord:3,maxFragments:0}
	if strings.TrimSpace(text)==""{return options,nil}
	for _,part:=range strings.Split(text,","){
		name,value,ok:=strings.Cut(strings.TrimSpace(part),"=");if !ok{return options,sqlError("22023","invalid headline option")}
		name=strings.ToLower(strings.TrimSpace(name));value=strings.TrimSpace(value)
		switch name{
		case "startsel":options.start=value
		case "stopsel":options.stop=value
		case "fragmentdelimiter":options.fragment=value
		case "maxwords","minwords","shortword","maxfragments":
			number,err:=strconv.Atoi(value);if err!=nil||number<0{return options,sqlError("22023","invalid integer headline option")}
			switch name{case "maxwords":options.maxWords=number;case "minwords":options.minWords=number;case "shortword":options.shortWord=number;case "maxfragments":options.maxFragments=number}
		case "highlightall":
			switch strings.ToLower(value){case "true","on","yes","1":options.highlightAll=true;case "false","off","no","0":options.highlightAll=false;default:return options,sqlError("22023","invalid HighlightAll option")}
		default:return options,sqlError("22023","unrecognized headline parameter")
		}
	}
	return options,nil
}

func textSearchTokenMatches(ctx context.Context,configuration model.TextSearchConfiguration,token tsParsedToken,operands []*tsQueryNode)(bool,error){
	for _,dictionary:=range textSearchMapping(configuration,token.typ){
		lexemes,err:=lexizeTextSearch(ctx,dictionary,token.text);if err!=nil{return false,err};if lexemes==nil{continue}
		for _,lexeme:=range lexemes{value:=&tsQueryNode{kind:tsQueryValue,word:lexeme};for _,operand:=range operands{if operand.prefix&&strings.HasPrefix(value.word,operand.word)||!operand.prefix&&value.word==operand.word{return true,nil}}}
		return false,nil
	}
	return false,nil
}

func headlineText(ctx context.Context,configuration model.TextSearchConfiguration,text string,query *tsQueryNode,rawOptions string,budget int64)(string,error){
	options,err:=parseTSHeadlineOptions(rawOptions);if err!=nil{return "",err}
	tokens,err:=parseDefaultText(ctx,text,max(1,min(len(text)+1,tsMaxDataBytes)));if err!=nil{return "",err}
	operands:=tsQueryPositiveOperands(query);matched:=make([]bool,len(tokens));matches:=0
	for i,token:=range tokens{matched[i],err=textSearchTokenMatches(ctx,configuration,token,operands);if err!=nil{return "",err};if matched[i]{matches++}}
	first,last:=0,len(tokens)-1
	if !options.highlightAll&&matches>0&&options.maxWords>0{
		anchor:=0;for i:=range tokens{if matched[i]{anchor=i;break}}
		words:=0;first=anchor
		for first>0&&words<options.maxWords/2{first--;if tokens[first].typ!=12&&tokens[first].typ!=13{words++}}
		last=anchor;for last+1<len(tokens)&&words<options.maxWords{last++;if tokens[last].typ!=12&&tokens[last].typ!=13{words++}}
	}
	if len(tokens)==0{return "",nil}
	startByte,endByte:=tokens[first].start,tokens[last].start+tokens[last].length
	var out strings.Builder
	if startByte>0{out.WriteString(options.fragment)}
	cursor:=startByte
	for i:=first;i<=last;i++{
		token:=tokens[i];if token.start<cursor{continue};out.WriteString(text[cursor:token.start]);if matched[i]{out.WriteString(options.start)};out.WriteString(text[token.start:token.start+token.length]);if matched[i]{out.WriteString(options.stop)};cursor=token.start+token.length
		if err := outputBudget(int64(out.Len()), budget); err != nil { return "", err }
	}
	out.WriteString(text[cursor:endByte]);if endByte<len(text){out.WriteString(options.fragment)}
	if err := outputBudget(int64(out.Len()), budget); err != nil { return "", err };return out.String(),nil
}

func fullTextHeadline(ctx context.Context,id model.FunctionID,args []model.Value,budget CallBudget)(model.Value,error){
	explicit:=id==3743||id==3744;index:=0;var config *model.Value;if explicit{config=&args[0];index=1}
	configuration,err:=resolveTextSearchConfiguration(ctx,config,budget);if err!=nil{return model.Value{},err}
	query,_,err:=decodeTSQuery(ctx,args[index+1].BytesVal);if err!=nil{return model.Value{},err};options:="";if len(args)>index+2{options=args[index+2].StrVal}
	text,err:=headlineText(ctx,configuration,args[index].StrVal,query,options,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.StringVal(text),nil
}

type tsJSONFlags struct{ strings,numbers,booleans,keys bool }
func parseTSJSONFlags(ctx context.Context,text string)(tsJSONFlags,error){
	document,err:=parseJSONBDocument(ctx,text,int64(tsMaxDataBytes));if err!=nil{return tsJSONFlags{},err};if len(document.nodes)==0||document.nodes[0].kind!=jsonTextArray{return tsJSONFlags{},sqlError("22023","wrong flag in JSON text search configuration")}
	var flags tsJSONFlags
	for child:=document.nodes[0].child;child>=0;child=document.nodes[child].next{node:=document.nodes[child];if node.kind!=jsonTextString{return flags,sqlError("22023","wrong flag in JSON text search configuration")};switch node.text{case "all":flags=tsJSONFlags{true,true,true,true};case "string":flags.strings=true;case "numeric":flags.numbers=true;case "boolean":flags.booleans=true;case "key":flags.keys=true;default:return flags,sqlError("22023","wrong flag in JSON text search configuration")}}
	return flags,nil
}

func jsonTSVector(ctx context.Context,configuration model.TextSearchConfiguration,text string,flags tsJSONFlags,budget int64)([]byte,error){
	document,err:=parseJSONBDocument(ctx,text,int64(tsMaxDataBytes));if err!=nil{return nil,err}
	lexemes:=make([]tsLexeme,0);base:=0
	add:=func(value string)error{words,err:=lexizeText(ctx,configuration,value,max(1,min(len(value)+1,tsMaxDataBytes)));if err!=nil{return err};if len(words)==0{return nil};for _,word:=range words{lexemes=append(lexemes,tsLexeme{text:word.lexeme,positions:[]tsPosition{makeTSPosition(min(tsMaxPosition,base+word.position),0)}})};base+=words[len(words)-1].position+1;return nil}
	for index,node:=range document.nodes{
		if err:=ctx.Err();err!=nil{return nil,err}
		if index!=0&&node.key!=""&&flags.keys{if err:=add(node.key);err!=nil{return nil,err}}
		include:=node.kind==jsonTextString&&flags.strings||node.kind==jsonTextNumber&&flags.numbers||node.kind==jsonTextBoolean&&flags.booleans
		if include{if err:=add(node.text);err!=nil{return nil,err}}
	}
	return encodeTSVector(ctx,normalizeTSVector(lexemes),budget)
}

func fullTextJSONVector(ctx context.Context,id model.FunctionID,args []model.Value,budget CallBudget)(model.Value,error){
	explicit:=id==4211||id==4212||id==4214||id==4216;index:=0;var config *model.Value;if explicit{config=&args[0];index=1}
	configuration,err:=resolveTextSearchConfiguration(ctx,config,budget);if err!=nil{return model.Value{},err}
	flags:=tsJSONFlags{strings:true};if id>=4213{flags,err=parseTSJSONFlags(ctx,args[index+1].StrVal);if err!=nil{return model.Value{},err}}
	data,err:=jsonTSVector(ctx,configuration,args[index].StrVal,flags,budget.OutputBytes);if err!=nil{return model.Value{},err};return model.BytesVal(data),nil
}

func fullTextJSONHeadline(ctx context.Context,id model.FunctionID,args []model.Value,budget CallBudget)(model.Value,error){
	explicit:=id==4201||id==4202||id==4205||id==4206;index:=0;var config *model.Value;if explicit{config=&args[0];index=1}
	configuration,err:=resolveTextSearchConfiguration(ctx,config,budget);if err!=nil{return model.Value{},err};query,_,err:=decodeTSQuery(ctx,args[index+1].BytesVal);if err!=nil{return model.Value{},err};options:="";if len(args)>index+2{options=args[index+2].StrVal}
	document,err:=parseJSONBDocument(ctx,args[index].StrVal,int64(min(uint64(tsMaxDataBytes),budget.MemoryBytes)));if err!=nil{return model.Value{},err}
	for i:=range document.nodes{if document.nodes[i].kind!=jsonTextString{continue};document.nodes[i].text,err=headlineText(ctx,configuration,document.nodes[i].text,query,options,budget.OutputBytes);if err!=nil{return model.Value{},err}}
	text,err:=document.render(ctx,budget.OutputBytes,false);if err!=nil{return model.Value{},err};return model.Value{Type:model.TypeJSONB,StrVal:text},nil
}
