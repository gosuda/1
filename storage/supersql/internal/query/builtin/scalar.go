package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func isComparison(op primitiveOp) bool {
	switch op {
	case opEq, opNe, opLt, opLe, opGt, opGe, opCmp:
		return true
	default:
		return false
	}
}

func comparisonResult(order int, op primitiveOp) model.Value {
	switch op {
	case opEq:
		return model.BoolVal(order == 0)
	case opNe:
		return model.BoolVal(order != 0)
	case opLt:
		return model.BoolVal(order < 0)
	case opLe:
		return model.BoolVal(order <= 0)
	case opGt:
		return model.BoolVal(order > 0)
	case opGe:
		return model.BoolVal(order >= 0)
	default:
		return model.Int64Val(int64(order))
	}
}

func compilePrimitive(signature Signature, op primitiveOp, options ResolveOptions) scalarKernel {
	switch op {
	case opNativeScalar:
		return compileNativeScalar(signature, options)
	case opContextOnly:
		return compileContextOnly(signature)
	case opAdvisory:
		return compileAdvisory(signature)
	case opFullText:
		return compileFullText(signature)
	case opRegexp:
		return compileRegexpFunction(signature, options)
	case opSettings:
		return compileSettings(signature)
	case opGeoIO:
		return compileGeoIO(signature, options)
	case opGeoPointBox:
		return compileGeoPointBox(signature, options)
	case opGeoLinear:
		return compileGeoLinear(signature, options)
	case opGeoCircle:
		return compileGeoCircle(signature, options)
	case opGeoPathPolygon:
		return compileGeoPathPolygon(signature, options)
	case opValueHash:
		return compileValueHash(signature, options)
	case opScalarHash:
		return compileScalarHash(signature, options)
	case opCatalogVector:
		return compileCatalogVector(signature, options)
	case opTemporalOverlaps:
		return compileTemporalOverlaps(signature)
	case opTimestampBin:
		return compileTimestampBin(signature)
	case opTimestampFields:
		return compileTimestampFields(signature)
	case opTimestampCalendar:
		return compileTimestampCalendar(signature)
	case opTimeFields:
		return compileTimeFields(signature)
	case opTimestamp:
		return compileTimestamp(signature)
	case opTimestampOutput:
		return compileTimestampOutput(signature, options)
	case opTime:
		return compileTime(signature)
	case opDateFields:
		return compileDateFields(signature)
	case opDateOutput:
		return compileDateOutput(signature, options)
	case opDate:
		return compileDate(signature)
	case opJSONStrip:
		return compileJSONStrip(signature)
	case opJSONBStrip:
		return compileJSONBStrip(signature)
	case opJSONBPretty:
		return compileJSONBPretty(signature)
	case opJSONBConcat:
		return compileJSONBConcat(signature)
	case opJSONBDelete:
		return compileJSONBDelete(signature)
	case opJSONBPath:
		return compileJSONBPath(signature)
	case opJSONBExists:
		return compileJSONBExists(signature)
	case opJSONBExtract:
		return compileJSONBExtract(signature)
	case opJSONPath:
		return compileJSONPath(signature)
	case opJSONPathRuntime:
		return compilePostgresJSONPath(signature, options)
	case opJSONB:
		return compileJSONB(signature)
	case opJSONTextExtract:
		return compileJSONTextExtract(signature)
	case opJSONText:
		return compileJSONText(signature)
	case opUnicodeNormalization:
		return compileUnicodeNormalization(signature)
	case opUnicodeInfo:
		return compileUnicodeInfo(signature)
	case opUnicodeString:
		return unicodeString
	case opParseIdentifier:
		return parseIdentifier
	case opIntervalField:
		return compileIntervalField(signature)
	case opIntervalExtremum:
		return compileIntervalExtremum(signature)
	case opIntervalSend:
		return intervalSend
	case opIntervalInput:
		return compileIntervalInput(signature, options)
	case opIntervalOutput:
		return compileIntervalOutput(signature, options)
	case opIntervalArithmetic:
		return compileIntervalArithmetic(signature)
	case opIntervalTypmod:
		return intervalTypmod
	case opOIDName:
		return compileOIDName(signature, options)
	case opBinaryValue:
		return compileBinaryValue(signature, options)
	case opNumericExtra:
		return compileNumericExtra(signature, options)
	case opBitString:
		return compileBitString(signature, options)
	case opNetwork:
		return compileNetwork(signature, options)
	case opMACAddress:
		return compileMACAddress(signature, options)
	case opCharValue:
		return compileCharValue(signature, options)
	case opJSONBPathWrite:
		return compileJSONBPathWrite(signature, options)
	case opJSONBBuild:
		return compileJSONBBuild(signature, options)
	case opTimetzComparison:
		return compileTimetzComparison(signature)
	case opJSONBComparison:
		return compileJSONBComparison(signature)
	case opJSONBHash:
		return compileJSONBHash(signature)
	case opNumericHash:
		return compileNumericHash(signature)
	case opJSONBCast:
		return compileJSONBNumericCast(signature)
	case opJSONBuild:
		return compileJSONBuild(signature)
	case opMoney:
		return compileMoney(signature, options)
	}
	if op == opCString {
		return copyCString
	}
	if op == opFormatText {
		return compileFormatText(&variadicTextCodec{signature: signature, prefix: 1})
	}
	if op == opTextQuote {
		return compileTextQuote(signature)
	}
	if op == opNameIO {
		return compileNameIO(signature)
	}
	if op == opUUID {
		return compileUUID(signature)
	}
	if op == opWidthBucketRange {
		return compileWidthBucketRange(signature)
	}
	if op == opArrayReverse {
		return reverseArray
	}
	if op == opArrayTrim {
		return trimArray
	}
	if op == opBtreeComparison {
		return compileBtreeComparison(signature, options)
	}
	if op == opStringToArray {
		return compileStringToArray(signature, options)
	}
	if op == opFloatCore {
		return compileFloatCore(signature)
	}
	if op == opFloatTrig {
		return compileFloatTrig(signature)
	}
	if op == opLike {
		return compileLike(signature, options)
	}
	if op == opLikeEscape {
		return compileLikeEscape(signature)
	}
	if op == opTextSearch {
		return compileTextSearch(signature, options)
	}
	if op == opTextPadding {
		return compileTextPadding(signature)
	}
	if op == opTextCase {
		return compileTextCase(signature, options)
	}
	if op == opTextCode {
		return compileTextCode(signature)
	}
	if op == opDigest {
		return compileDigest(signature)
	}
	if op == opBinaryEncoding {
		return compileBinaryEncoding(signature)
	}
	if op == opOverlay {
		return compileOverlay(signature)
	}
	if op == opByteAccess {
		return compileByteAccess(signature)
	}
	if op == opArrayMetadata {
		return compileArrayMetadata(signature)
	}
	if signature.Result == 1700 || signature.Arguments[0] == 1700 {
		return compileNumeric(signature, op)
	}
	if signature.Result == 1042 || signature.Result == 1043 || signature.Arguments[0] == 1042 || signature.Arguments[0] == 1043 {
		return compileCharacter(signature, op)
	}
	if op == opCast {
		return compileCast(signature)
	}
	if op == opInput {
		return compileInput(signature)
	}
	if op == opOutput {
		return compileOutput(signature, options)
	}
	first := signature.Arguments[0]
	if first == 25 || first == 17 {
		return compileText(signature, op)
	}
	if isFloat(first) {
		return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
			left := args[0].FloatVal
			if len(args) == 1 {
				return floatUnary(left, op), nil
			}
			right := args[1].FloatVal
			if isComparison(op) {
				return comparisonResult(compareFloat(left, right), op), nil
			}
			return floatArithmetic(left, right, signature.Result, op)
		}
	}
	return func(_ context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
		left := args[0].IntVal
		if len(args) == 1 {
			return integerUnary(left, signature.Result, op)
		}
		right := args[1].IntVal
		if isComparison(op) {
			order := 0
			if left < right {
				order = -1
			} else if left > right {
				order = 1
			}
			return comparisonResult(order, op), nil
		}
		return integerArithmetic(left, right, signature.Result, op)
	}
}
