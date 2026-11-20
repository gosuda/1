package main

import (
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// callName returns the name a call expression invokes: the identifier itself or
// the selected name of a qualified call such as pkg.bindError.
func callName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name
	case *ast.SelectorExpr:
		return function.Sel.Name
	default:
		return ""
	}
}

// stringLiteral returns the value of a string literal expression.
func stringLiteral(expr ast.Expr) (string, bool) {
	literal, ok := expr.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// exprName renders the type name of a composite literal or type expression:
// "Error" for Error{...} and "model.Error" for model.Error{...}.
func exprName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		prefix := exprName(typed.X)
		if prefix == "" {
			return typed.Sel.Name
		}
		return prefix + "." + typed.Sel.Name
	case *ast.StarExpr:
		return exprName(typed.X)
	case *ast.ArrayType:
		return exprName(typed.Elt)
	case *ast.IndexExpr:
		return exprName(typed.X)
	case *ast.IndexListExpr:
		return exprName(typed.X)
	default:
		return ""
	}
}

// lastSegment returns the final dot-separated segment of a qualified name.
func lastSegment(name string) string {
	if index := strings.LastIndex(name, "."); index >= 0 {
		return name[index+1:]
	}
	return name
}

// compositeField returns the value of one named field of a composite literal.
func compositeField(literal *ast.CompositeLit, name string) (ast.Expr, bool) {
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok || key.Name != name {
			continue
		}
		return field.Value, true
	}
	return nil, false
}

// constSpec is one named constant found by a file scan.
type constSpec struct {
	name     string
	value    string
	kind     token.Token
	file     string
	position token.Pos
}

// fileConstants collects every named constant of one parsed file, in
// declaration order. Values are rendered as source text so that iota
// expressions stay readable in the generated document.
func (g *generator) fileConstants(rel string, file *ast.File) []constSpec {
	var constants []constSpec
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				spec := constSpec{name: name.Name, kind: token.CONST, file: rel, position: name.Pos()}
				switch {
				case index < len(value.Values):
					spec.value = g.print(value.Values[index])
				case len(value.Values) > 0:
					spec.value = g.print(value.Values[len(value.Values)-1])
				}
				constants = append(constants, spec)
			}
		}
	}
	return constants
}

// findConst returns the first constant with the given name.
func findConst(constants []constSpec, name string) (constSpec, bool) {
	for _, constant := range constants {
		if constant.name == name {
			return constant, true
		}
	}
	return constSpec{}, false
}

// findStruct returns the named struct type declaration of a file.
func findStruct(file *ast.File, name string) (*ast.StructType, bool) {
	for _, declaration := range file.Decls {
		gen, ok := declaration.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != name {
				continue
			}
			structure, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				return nil, false
			}
			return structure, true
		}
	}
	return nil, false
}

// structField is one field of a struct type as documented.
type structField struct {
	names []string
	typ   string
	doc   string
	tag   string
}

// structFields flattens a struct type into one entry per field name.
func (g *generator) structFields(structure *ast.StructType) []structField {
	if structure == nil {
		return nil
	}
	var fields []structField
	for _, field := range structure.Fields.List {
		entry := structField{typ: g.print(field.Type)}
		if field.Doc != nil {
			entry.doc = strings.TrimSpace(field.Doc.Text())
		} else if field.Comment != nil {
			entry.doc = strings.TrimSpace(field.Comment.Text())
		}
		if field.Tag != nil {
			entry.tag = field.Tag.Value
		}
		if len(field.Names) == 0 {
			entry.names = []string{g.print(field.Type)}
			entry.typ = ""
			fields = append(fields, entry)
			continue
		}
		for _, name := range field.Names {
			copied := entry
			copied.names = []string{name.Name}
			fields = append(fields, copied)
		}
	}
	return fields
}

// findFuncBody returns the body of the named top-level function.
func findFuncBody(file *ast.File, name string) (*ast.BlockStmt, bool) {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name || function.Recv != nil || function.Body == nil {
			continue
		}
		return function.Body, true
	}
	return nil, false
}

// returnComposite returns the composite literal of the first return statement
// whose expression is a composite literal of the wanted type.
func returnComposite(body *ast.BlockStmt, wanted string) (*ast.CompositeLit, bool) {
	if body == nil {
		return nil, false
	}
	var found *ast.CompositeLit
	ast.Inspect(body, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		statement, ok := node.(*ast.ReturnStmt)
		if !ok || len(statement.Results) != 1 {
			return true
		}
		literal, ok := statement.Results[0].(*ast.CompositeLit)
		if !ok || lastSegment(exprName(literal.Type)) != wanted {
			return true
		}
		found = literal
		return false
	})
	if found == nil {
		return nil, false
	}
	return found, true
}

// fieldValues maps each named field of a composite literal to its printed value.
func (g *generator) fieldValues(literal *ast.CompositeLit) map[string]string {
	values := map[string]string{}
	if literal == nil {
		return values
	}
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			continue
		}
		values[key.Name] = g.print(field.Value)
	}
	return values
}
