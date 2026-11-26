// genyacc normalizes the pinned goyacc skeleton without modifying grammar actions.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	maxParserBytes = 16 << 20
	yyMaxStackSize = 4096
)

// provenance is inserted after goyacc's own "// Code generated" line. Both
// grammars are vendored from the pg18_query_go copy of libpg_query, so every
// goyacc re-run keeps the attribution.
const provenance = "// Source: internal/zpgparse/internal/neoparse/gram.y and internal/zpgparse/internal/plpgsql/gram.y, vendored from https://github.com/gosuda/pg18_query_go at 10d566809991b10ff1182369705c19743e16c155\n" +
	"// License: BSD-3-Clause AND PostgreSQL (see internal/zpgparse/NOTICE)"

const stackGrowReplacement = "if yyp >= len(yyS) {\n" +
	"\tif yyp >= yyMaxStackSize {\n" +
	"\t\tyylex.Error(\"parser stack overflow\")\n" +
	"\t\tgoto ret1\n" +
	"\t}\n" +
	"\tnewcap := min(len(yyS)*2, yyMaxStackSize)\n" +
	"\tnyys := make([]yySymType, newcap)\n" +
	"\tcopy(nyys, yyS)\n" +
	"\tyyS = nyys\n" +
	"}"

var (
	errSkeleton    = errors.New("unrecognized pinned goyacc skeleton")
	errUsage       = errors.New("usage: genyacc parser.go")
	errParserLimit = errors.New("generated parser exceeds byte limit")
)

type replacement struct {
	start, end int
	text       string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	input, err := os.Open(args[0])
	if err != nil {
		return err
	}
	source, readErr := io.ReadAll(io.LimitReader(input, maxParserBytes+1))
	if err := errors.Join(readErr, input.Close()); err != nil {
		return err
	}
	if len(source) > maxParserBytes {
		return errParserLimit
	}
	output, err := normalize(source)
	if err != nil {
		return err
	}
	return publish(args[0], output)
}

func normalize(source []byte) ([]byte, error) {
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "y.go", source, 0)
	if err != nil {
		return nil, err
	}
	offset := func(position token.Pos) int { return files.File(position).Offset(position) }
	var initialStack *ast.GenDecl
	var fallback *ast.IfStmt
	var stackGrow *ast.IfStmt
	var recovery *ast.ForStmt
	var resume *ast.BranchStmt
	var initialStackCount, fallbackCount, stackGrowCount, recoveryCount, resumeCount int
	for _, declaration := range file.Decls {
		if decl, ok := declaration.(*ast.GenDecl); ok && decl.Tok == token.CONST {
			for _, spec := range decl.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range valueSpec.Names {
					switch name.Name {
					case "yyMaxStackSize":
						return nil, errSkeleton
					case "yyInitialStackSize":
						if len(decl.Specs) == 1 && len(valueSpec.Names) == 1 {
							initialStack = decl
							initialStackCount++
						}
					}
				}
			}
			continue
		}
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch function.Name.Name {
		case "yyErrorRecovery":
			return nil, errSkeleton
		case "yylex1":
			ast.Inspect(function.Body, func(node ast.Node) bool {
				branch, ok := node.(*ast.IfStmt)
				if ok && compareZero(branch.Cond, "token", token.EQL) {
					fallback = branch
					fallbackCount++
				}
				return true
			})
		case "Parse":
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if branch, ok := node.(*ast.IfStmt); ok && isStackGrowCond(branch.Cond) {
					stackGrow = branch
					stackGrowCount++
					return false
				}
				loop, ok := node.(*ast.ForStmt)
				if !ok || !compareZero(loop.Cond, "yyp", token.GEQ) {
					return true
				}
				recovery = loop
				recoveryCount++
				ast.Inspect(loop.Body, func(child ast.Node) bool {
					jump, ok := child.(*ast.BranchStmt)
					if ok && jump.Tok == token.GOTO && jump.Label != nil && jump.Label.Name == "yystack" {
						resume = jump
						resumeCount++
					}
					return true
				})
				return false
			})
		}
	}
	if initialStackCount != 1 || fallbackCount != 1 || stackGrowCount != 1 || recoveryCount != 1 || resumeCount != 1 {
		return nil, errSkeleton
	}
	fallbackSource := string(source[offset(fallback.Pos()):offset(fallback.End())])
	if !strings.Contains(fallbackSource, "token = int(yyTok2[1])") || len(fallback.Body.List) != 1 || fallback.Else != nil {
		return nil, errSkeleton
	}
	stackGrowSource := string(source[offset(stackGrow.Pos()):offset(stackGrow.End())])
	if stackGrow.Init != nil || stackGrow.Else != nil || len(stackGrow.Body.List) != 3 ||
		!strings.Contains(stackGrowSource, "make([]yySymType, len(yyS)*2)") ||
		!strings.Contains(stackGrowSource, "copy(nyys, yyS)") ||
		!strings.Contains(stackGrowSource, "yyS = nyys") {
		return nil, errSkeleton
	}
	start, end := offset(recovery.Pos()), offset(recovery.End())
	loop := string(source[start:offset(resume.Pos())]) + "return yystate, yyp" + string(source[offset(resume.End()):end])
	helper := "\nfunc yyErrorRecovery(yyS []yySymType, yyp int) (yystate, position int) {\nvar yyn int\n" + loop + "\nreturn 0, -1\n}\n"
	headerEnd := bytes.IndexByte(source, '\n')
	if headerEnd < 0 || !strings.HasPrefix(string(source[:headerEnd]), "// Code generated by goyacc") {
		return nil, errSkeleton
	}
	edits := []replacement{
		{headerEnd, headerEnd, "\n" + provenance},
		{offset(file.Name.End()), offset(file.Name.End()), "\nimport __yycmp__ \"cmp\"\n"},
		{offset(initialStack.End()), offset(initialStack.End()), fmt.Sprintf("\nconst yyMaxStackSize = %d", yyMaxStackSize)},
		{offset(fallback.Pos()), offset(fallback.End()), "token = __yycmp__.Or(token, int(yyTok2[1]))"},
		{offset(stackGrow.Pos()), offset(stackGrow.End()), stackGrowReplacement},
		{start, end, "yystate, yyp = yyErrorRecovery(yyS, yyp)\nif yyp >= 0 { goto yystack }"},
		{len(source), len(source), helper},
	}
	slices.SortFunc(edits, func(a, b replacement) int { return a.start - b.start })
	var output bytes.Buffer
	output.Grow(len(source) + len(helper) + len(stackGrowReplacement))
	position := 0
	for _, edit := range edits {
		output.Write(source[position:edit.start])
		output.WriteString(edit.text)
		position = edit.end
	}
	output.Write(source[position:])
	if _, err := parser.ParseFile(token.NewFileSet(), "y.go", output.Bytes(), parser.AllErrors); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func compareZero(expression ast.Expr, name string, operator token.Token) bool {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != operator {
		return false
	}
	identifier, ok := binary.X.(*ast.Ident)
	if !ok || identifier.Name != name {
		return false
	}
	literal, ok := binary.Y.(*ast.BasicLit)
	return ok && literal.Kind == token.INT && literal.Value == "0"
}

func isStackGrowCond(expression ast.Expr) bool {
	binary, ok := expression.(*ast.BinaryExpr)
	if !ok || binary.Op != token.GEQ {
		return false
	}
	identifier, ok := binary.X.(*ast.Ident)
	if !ok || identifier.Name != "yyp" {
		return false
	}
	call, ok := binary.Y.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	callee, ok := call.Fun.(*ast.Ident)
	if !ok || callee.Name != "len" {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == "yyS"
}

func publish(path string, data []byte) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".genyacc-*")
	if err != nil {
		return err
	}
	defer func() {
		removeErr := os.Remove(temporary.Name())
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	writeErr := temporary.Chmod(info.Mode().Perm())
	if writeErr == nil {
		_, writeErr = temporary.Write(data)
	}
	if err := errors.Join(writeErr, temporary.Close()); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
