package zpgparse

import (
	"fmt"
	"github.com/zeebo/xxh3"
	"google.golang.org/protobuf/reflect/protoreflect"
	"sort"
	"strconv"
	"strings"
)

type FingerprintOption int

const (
	FingerprintDefault               FingerprintOption = 0
	FingerprintRangeVarIgnoreAliases FingerprintOption = 1
	FingerprintRangeVarIncludeSchema FingerprintOption = 2
	FingerprintRangeVarPG17Compat                      = FingerprintRangeVarIgnoreAliases | FingerprintRangeVarIncludeSchema
	FingerprintRelnameFull           FingerprintOption = 16
)

func HashXXH3_64(input []byte, seed uint64) uint64 { return xxh3.HashSeed(input, seed) }
func Fingerprint(input string) (string, error)     { return FingerprintWithOpts(input, FingerprintDefault) }
func FingerprintWithOpts(input string, opts FingerprintOption) (string, error) {
	hash, err := FingerprintToUInt64WithOpts(input, opts)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%016x", hash), nil
}
func FingerprintToUInt64(input string) (uint64, error) {
	return FingerprintToUInt64WithOpts(input, FingerprintDefault)
}
func FingerprintToUInt64WithOpts(input string, opts FingerprintOption) (uint64, error) {
	tree, err := Parse(input)
	if err != nil {
		return 0, err
	}
	var out []byte
	for _, stmt := range tree.Stmts {
		out = fingerprintMessage(out, stmt.ProtoReflect(), "", "", 1, opts)
	}
	return HashXXH3_64(out, 3), nil
}
func utilityField(m protoreflect.Message, name string) protoreflect.FieldDescriptor {
	fields := m.Descriptor().Fields()
	if f := fields.ByJSONName(name); f != nil {
		return f
	}
	if f := fields.ByName(protoreflect.Name(name)); f != nil {
		return f
	}
	for i := range fields.Len() {
		f := fields.Get(i)
		if strings.EqualFold(strings.ReplaceAll(string(f.Name()), "_", ""), strings.ReplaceAll(name, "_", "")) {
			return f
		}
	}
	return nil
}
func utilityUnwrap(m protoreflect.Message) protoreflect.Message {
	if !m.IsValid() {
		return m
	}
	if m.Descriptor().Name() == "Node" {
		o := m.Descriptor().Oneofs().Get(0)
		f := m.WhichOneof(o)
		if f != nil {
			return m.Get(f).Message()
		}
	}
	return m
}
func fingerprintMessage(out []byte, m protoreflect.Message, parent, field string, depth int, opts FingerprintOption) []byte {
	if depth >= 100 || !m.IsValid() {
		return out
	}
	m = utilityUnwrap(m)
	name := string(m.Descriptor().Name())
	switch name {
	case "Node", "A_Const", "Alias", "ParamRef", "SetToDefault":
		return out
	case "List":
		f := utilityField(m, "items")
		return fingerprintList(out, m.Get(f).List(), parent, field, depth, opts)
	case "Integer":
		v := m.Get(utilityField(m, "ival")).Int()
		if v == 0 {
			return out
		}
		return strconv.AppendInt(append(out, "Integerival"...), v, 10)
	case "String":
		return append(append(out, "Stringstr"...), m.Get(utilityField(m, "sval")).String()...)
	case "Float":
		return append(append(out, "Floatstr"...), m.Get(utilityField(m, "fval")).String()...)
	case "BitString":
		return append(append(out, "BitStringstr"...), m.Get(utilityField(m, "bsval")).String()...)
	case "Boolean":
		return strconv.AppendBool(append(out, "Booleanboolval"...), m.Get(utilityField(m, "boolval")).Bool())
	case "RangeVar":
		return fingerprintRangeVar(out, m.Interface().(*RangeVar), parent, field, opts)
	case "TypeCast":
		if fingerprintConstantCast(m) {
			return out
		}
	}
	fields, ok := fingerprintFields[name]
	if !ok {
		return out
	}
	out = append(out, name...)
	for _, key := range fields {
		if fingerprintSelectTargetName(name, key, parent, field) {
			continue
		}
		f := utilityField(m, key)
		if f == nil {
			continue
		}
		out = fingerprintField(out, m, f, name, key, depth, opts)
	}
	return out
}
func fingerprintConstantCast(m protoreflect.Message) bool {
	f := utilityField(m, "arg")
	if f == nil || !m.Has(f) {
		return false
	}
	n := utilityUnwrap(m.Get(f).Message())
	if !n.IsValid() {
		return false
	}
	switch string(n.Descriptor().Name()) {
	case "A_Const", "ParamRef":
		return true
	}
	return false
}
func fingerprintSelectTargetName(name, key, parent, field string) bool {
	if name != "ResTarget" || key != "name" {
		return false
	}
	return parent == "SelectStmt" && field == "targetList"
}
func fingerprintField(out []byte, m protoreflect.Message, f protoreflect.FieldDescriptor, name, key string, depth int, opts FingerprintOption) []byte {
	v := m.Get(f)
	start := len(out)
	if f.IsList() {
		list := v.List()
		if list.Len() == 0 {
			return out
		}
		out = append(out, key...)
		before := len(out)
		out = fingerprintList(out, list, name, key, depth+1, opts)
		if len(out) == before && !fingerprintEmptyNodeList(list, f) {
			out = out[:start]
		}
		return out
	}
	switch f.Kind() {
	case protoreflect.MessageKind:
		if !m.Has(f) {
			return out
		}
		out = append(out, key...)
		before := len(out)
		out = fingerprintMessageField(out, v.Message(), name, key, depth, opts)
		if len(out) == before {
			out = out[:start]
		}
	case protoreflect.EnumKind:
		e := f.Enum().Values().ByNumber(v.Enum())
		if e != nil {
			value := string(e.Name())
			if fingerprintOperatorKind(name, key, value) {
				value = "AEXPR_OP"
			}
			out = append(append(out, key...), value...)
		}
	case protoreflect.BoolKind:
		if v.Bool() {
			out = append(append(out, key...), "true"...)
		}
	case protoreflect.StringKind:
		if v.String() != "" {
			out = append(append(out, key...), v.String()...)
		}
	case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind:
		if v.Int() != 0 {
			out = strconv.AppendInt(append(out, key...), v.Int(), 10)
		}
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind:
		if v.Uint() != 0 {
			out = strconv.AppendUint(append(out, key...), v.Uint(), 10)
		}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		if v.Float() != 0 {
			out = strconv.AppendFloat(append(out, key...), v.Float(), 'f', 6, 64)
		}
	}
	return out
}
func fingerprintEmptyNodeList(list protoreflect.List, f protoreflect.FieldDescriptor) bool {
	if list.Len() != 1 || f.Kind() != protoreflect.MessageKind {
		return false
	}
	return utilityUnwrap(list.Get(0).Message()).Descriptor().Name() == "Node"
}
func fingerprintMessageField(out []byte, child protoreflect.Message, name, key string, depth int, opts FingerprintOption) []byte {
	if fingerprintScalarString[name+"."+key] {
		return append(out, child.Get(utilityField(child, "sval")).String()...)
	}
	if direct, ok := fingerprintDirect[name+"."+key]; ok {
		data := fingerprintMessage(nil, child, name, key, depth+1, opts)
		if len(data) >= len(direct) && string(data[:len(direct)]) == direct {
			data = data[len(direct):]
		}
		return append(out, data...)
	}
	return fingerprintMessage(out, child, name, key, depth+1, opts)
}
func fingerprintOperatorKind(name, key, value string) bool {
	if name != "A_Expr" || key != "kind" {
		return false
	}
	return value == "AEXPR_OP_ANY" || value == "AEXPR_IN"
}
func fingerprintList(out []byte, list protoreflect.List, parent, field string, depth int, opts FingerprintOption) []byte {
	if depth >= 100 {
		return out
	}
	sorted := field == "fromClause" || field == "targetList" || field == "cols" || field == "rexpr" || field == "valuesLists" || field == "args"
	if !sorted {
		for i := range list.Len() {
			if m, ok := list.Get(i).Interface().(protoreflect.Message); ok {
				out = fingerprintMessage(out, m, parent, field, depth+1, opts)
			}
		}
		return out
	}
	type item struct {
		hash uint64
		data []byte
	}
	items := make([]item, list.Len())
	for i := range items {
		if m, ok := list.Get(i).Interface().(protoreflect.Message); ok {
			items[i].data = fingerprintMessage(nil, m, parent, field, depth+1, opts)
		}
		items[i].hash = HashXXH3_64(items[i].data, 3)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].hash < items[j].hash })
	for i, item := range items {
		if i == 0 || item.hash != items[i-1].hash {
			out = append(out, item.data...)
		}
	}
	return out
}
func fingerprintRangeVar(out []byte, node *RangeVar, parent, field string, opts FingerprintOption) []byte {
	out = append(out, "RangeVar"...)
	ignore := opts&FingerprintRangeVarIgnoreAliases != 0
	dml := fingerprintDMLRange(parent, field)
	if !ignore && node.Alias != nil {
		out = append(append(out, "aliasname"...), node.Alias.Aliasname...)
	}
	if node.Catalogname != "" {
		out = append(append(out, "catalogname"...), node.Catalogname...)
	}
	if node.Inh {
		out = append(out, "inhtrue"...)
	}
	if node.Relname != "" && node.Relpersistence != "t" && !(dml && !ignore && node.Alias != nil) {
		out = append(out, "relname"...)
		for i := range len(node.Relname) {
			c := node.Relname[i]
			if opts&FingerprintRelnameFull == 0 && fingerprintRelnameDigitRun(node.Relname, i) {
				continue
			}
			out = append(out, c)
		}
	}
	if node.Relpersistence != "" {
		out = append(append(out, "relpersistence"...), node.Relpersistence...)
	}
	if node.Schemaname != "" && (!dml || opts&FingerprintRangeVarIncludeSchema != 0) {
		out = append(append(out, "schemaname"...), node.Schemaname...)
	}
	return out
}
func fingerprintDMLRange(parent, field string) bool {
	switch parent {
	case "SelectStmt":
		return field == "fromClause"
	case "InsertStmt":
		return field == "relation"
	case "UpdateStmt":
		return field == "relation" || field == "fromClause"
	case "DeleteStmt":
		return field == "relation" || field == "usingClause"
	case "MergeStmt":
		return field == "relation" || field == "sourceRelation"
	case "JoinExpr", "RangeTableSample", "LockingClause":
		return true
	}
	return false
}
func fingerprintRelnameDigitRun(name string, i int) bool {
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	if !digit(name[i]) {
		return false
	}
	if i > 0 && digit(name[i-1]) {
		return true
	}
	return i+1 < len(name) && digit(name[i+1])
}
