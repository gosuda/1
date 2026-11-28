package builtin

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func fullTextKeySize(ctx context.Context,id model.TypeID,raw []byte,budget uint64)(uint64,error){
	size:=uint64(1+8)
	if id==3614{
		vector,err:=decodeTSVector(ctx,raw);if err!=nil{return 0,err}
		for _,lexeme:=range vector{size+=uint64(len(lexeme.text)+3);for _,b:=range []byte(lexeme.text){if b==0{size++}};if len(lexeme.positions)!=0{size+=2+uint64(len(lexeme.positions))*2}}
	}else{
		query,_,err:=decodeTSQuery(ctx,raw);if err!=nil{return 0,err}
		var measure func(*tsQueryNode)
		measure=func(node *tsQueryNode){if node==nil{return};if node.kind==tsQueryValue{size+=1+4+uint64(len(node.word))+2;for _,b:=range []byte(node.word){if b==0{size++}};return};size+=3;measure(node.right);if node.op!=tsQueryNot{measure(node.left)};if node.op==tsQueryPhrase{size+=2}}
		measure(query)
	}
	if size>budget{return 0,columnar.ErrBatchBudget};return size,ctx.Err()
}

func appendMaskedUint16(out []byte,value uint16,mask byte)[]byte{var data [2]byte;binary.BigEndian.PutUint16(data[:],value);return append(out,data[0]^mask,data[1]^mask)}
func appendMaskedUint32(out []byte,value uint32,mask byte)[]byte{var data [4]byte;binary.BigEndian.PutUint32(data[:],value);for _,b:=range data{out=append(out,b^mask)};return out}
func appendEscapedFullTextKey(ctx context.Context,out []byte,text string,mask byte)([]byte,error){for i,b:=range []byte(text){if i%cancellationChunk==0{if err:=ctx.Err();err!=nil{return nil,err}};out=append(out,b^mask);if b==0{out=append(out,0xff^mask)}};return append(out,mask,mask),nil}

func appendFullTextKey(ctx context.Context,key []byte,id model.TypeID,raw []byte,mask byte)([]byte,error){
	if id==3614{
		vector,err:=decodeTSVector(ctx,raw);if err!=nil{return nil,err}
		key=appendMaskedUint32(key,uint32(tsVectorInternalSize(vector)),mask);key=appendMaskedUint32(key,uint32(len(vector)),mask)
		for _,lexeme:=range vector{
			has:=byte(1);if len(lexeme.positions)!=0{has=0};key=append(key,has^mask);key,err=appendEscapedFullTextKey(ctx,key,lexeme.text,mask);if err!=nil{return nil,err}
			if len(lexeme.positions)!=0{key=appendMaskedUint16(key,^uint16(len(lexeme.positions)),mask);for _,position:=range lexeme.positions{key=appendMaskedUint16(key,^uint16(tsPositionNumber(position)),mask);key=append(key,byte(^uint8(tsPositionWeight(position)))^mask)}}
		}
		return key,ctx.Err()
	}
	query,nodes,err:=decodeTSQuery(ctx,raw);if err!=nil{return nil,err};operandBytes:=tsQueryOperandBytes(query)
	key=appendMaskedUint32(key,uint32(nodes),mask);key=appendMaskedUint32(key,uint32(8+nodes*12+operandBytes),mask)
	var appendNode func(*tsQueryNode)error
	appendNode=func(node *tsQueryNode)error{if err:=ctx.Err();err!=nil{return err};if node==nil{return nil};key=append(key,(^node.kind)^mask);if node.kind==tsQueryValue{crc:=uint32(int32(crc32.ChecksumIEEE([]byte(node.word))))^0x80000000;key=appendMaskedUint32(key,^crc,mask);var err error;key,err=appendEscapedFullTextKey(ctx,key,node.word,mask);return err};key=append(key,(^node.op)^mask);children:=byte(2);if node.op==tsQueryNot{children=1};key=append(key,(^children)^mask);if err:=appendNode(node.right);err!=nil{return err};if node.op!=tsQueryNot{if err:=appendNode(node.left);err!=nil{return err}};if node.op==tsQueryPhrase{key=appendMaskedUint16(key,^node.distance,mask)};return nil}
	if err:=appendNode(query);err!=nil{return nil,err};return key,nil
}

func tsQueryOperandBytes(node *tsQueryNode)int{if node==nil{return 0};size:=0;if node.kind==tsQueryValue{size=len(node.word)+1};return size+tsQueryOperandBytes(node.left)+tsQueryOperandBytes(node.right)}

func (d *keyDecoder) fullTextBytes(ctx context.Context,id model.TypeID,mask byte)(model.Value,error){
	first,err:=d.number(4,mask);if err!=nil{return model.Value{},err};second,err:=d.number(4,mask);if err!=nil{return model.Value{},err}
	if id==3614{
		count:=int(second);if count<0||count>tsMaxDataBytes{return model.Value{},storage.ErrInvalidEncoding};lexemes:=make([]tsLexeme,count)
		for i:=range lexemes{has,err:=d.byte(mask);if err!=nil||has>1{return model.Value{},storage.ErrInvalidEncoding};raw,err:=d.escaped(ctx,mask,false,0);if err!=nil||len(raw)==0||len(raw)>tsMaxLexemeBytes||!utf8.Valid(raw){return model.Value{},storage.ErrInvalidEncoding};lexemes[i].text=string(raw);if has==0{length,err:=d.number(2,mask);if err!=nil{return model.Value{},err};length=uint64(^uint16(length));if length<1||length>tsMaxPositions{return model.Value{},storage.ErrInvalidEncoding};lexemes[i].positions=make([]tsPosition,length);for j:=range lexemes[i].positions{position,err:=d.number(2,mask);if err!=nil{return model.Value{},err};weight,err:=d.byte(mask);if err!=nil{return model.Value{},err};p,w:=int(^uint16(position)),int(^weight);if p<0||p>tsMaxPosition||w<0||w>3{return model.Value{},storage.ErrInvalidEncoding};lexemes[i].positions[j]=makeTSPosition(p,w)}}}
		if int(first)!=tsVectorInternalSize(lexemes){return model.Value{},storage.ErrInvalidEncoding};data,err:=encodeTSVector(ctx,lexemes,int64(d.remaining));if err!=nil{return model.Value{},storage.ErrInvalidEncoding};if err:=d.charge(uint64(len(data)));err!=nil{return model.Value{},err};return model.BytesVal(data),nil
	}
	nodes,internalSize:=int(first),int(second);seen:=0
	var readNode func(int)(*tsQueryNode,error)
	readNode=func(depth int)(*tsQueryNode,error){if depth>512||seen>=nodes{return nil,storage.ErrInvalidEncoding};kind,err:=d.byte(mask);if err!=nil{return nil,err};kind=^kind;seen++;node:=&tsQueryNode{kind:kind};if kind==tsQueryValue{encodedCRC,err:=d.number(4,mask);if err!=nil{return nil,err};raw,err:=d.escaped(ctx,mask,false,0);if err!=nil||len(raw)==0||len(raw)>tsMaxLexemeBytes||!utf8.Valid(raw){return nil,storage.ErrInvalidEncoding};node.word=string(raw);crc:=uint32(int32(crc32.ChecksumIEEE(raw)))^0x80000000;if uint32(encodedCRC)!=^crc{return nil,storage.ErrInvalidEncoding};return node,nil};if kind!=tsQueryOp{return nil,storage.ErrInvalidEncoding};op,err:=d.byte(mask);if err!=nil{return nil,err};node.op=^op;children,err:=d.byte(mask);if err!=nil{return nil,err};children=^children;if node.op<tsQueryNot||node.op>tsQueryPhrase||children!=2&&!(node.op==tsQueryNot&&children==1){return nil,storage.ErrInvalidEncoding};node.right,err=readNode(depth+1);if err!=nil{return nil,err};if node.op!=tsQueryNot{node.left,err=readNode(depth+1);if err!=nil{return nil,err}};if node.op==tsQueryPhrase{distance,err:=d.number(2,mask);if err!=nil{return nil,err};node.distance=^uint16(distance);if node.distance>tsMaxPosition+1{return nil,storage.ErrInvalidEncoding}};return node,nil}
	var root *tsQueryNode;if nodes!=0{root,err=readNode(0);if err!=nil{return model.Value{},err}};if seen!=nodes||internalSize!=8+nodes*12+tsQueryOperandBytes(root){return model.Value{},storage.ErrInvalidEncoding};data,err:=encodeTSQuery(ctx,root,int64(d.remaining));if err!=nil{return model.Value{},storage.ErrInvalidEncoding};if err:=d.charge(uint64(len(data)));err!=nil{return model.Value{},err};return model.BytesVal(data),nil
}
