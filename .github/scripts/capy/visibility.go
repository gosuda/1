package capy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The visibility check enforces the tier matrix of AGENTS.md section 2 by resolving references rather
// than matching text (hardening plan section 8). Module paths come from the go.mod files of the tree, so
// vanity paths such as gosuda.org/hqq resolve to their directories. The check reads git objects only and
// never runs go, cargo, or npm.
//
// Checked references, from files whose tier is not untiered:
//   - .go: import paths, read with ImportsOnly. Build constraints are ignored, so every file counts.
//   - go.mod: require and replace module paths that resolve in the tree, and local replace directories.
//   - Cargo.toml: path dependencies.
//   - package.json: file: and link: dependencies.
//   - .ts, .tsx, .mts, .cts, .js, .jsx, .mjs, .cjs: relative specifiers of import, export ... from,
//     require(), and import(). Bare specifiers are not resolved.
//
// Rust sources, Python files, and all other files are not checked.

// tier is a visibility tier from AGENTS.md section 2. Untiered paths (root files, .github, docs) are never
// checked as sources.
type tier string

const (
	tierNone         tier = "untiered"
	tierPublic       tier = "public"
	tierThirdParty   tier = "third_party"
	tierCorp         tier = "corp"
	tierExperimental tier = "experimental"
	tierOutside      tier = "outside tree"
)

// tierOf returns the tier of a tree directory from its first path segment.
func tierOf(dir string) tier {
	top, _, _ := strings.Cut(dir, "/")
	switch top {
	case "web", "net", "ipc", "storage", "crypto", "ai", "devtools":
		return tierPublic
	case "third_party":
		return tierThirdParty
	case "corp":
		return tierCorp
	case "experimental":
		return tierExperimental
	}
	return tierNone
}

// allowed reports whether code in tier from may refer to code in tier to. Untiered sources are not checked.
// Untiered targets are allowed only from experimental, which may import anything except corp.
func allowed(from, to tier) bool {
	switch from {
	case tierPublic:
		return to == tierPublic || to == tierThirdParty
	case tierCorp:
		return to == tierPublic || to == tierCorp || to == tierThirdParty
	case tierThirdParty:
		return to == tierThirdParty
	case tierExperimental:
		return to != tierCorp
	}
	return true
}

// visCheck collects the violation lines of one check.
type visCheck struct {
	mods moduleMap
	out  []string
}

// report records a reference that code in tier from may not make to tier to.
func (c *visCheck) report(p string, line int, from tier, what string, to tier) {
	c.out = append(c.out, fmt.Sprintf("%s:%d: prohibited import in %s domain: %s -> %s", p, line, from, what, to))
}

// unparsable records a file whose syntax the check cannot read. Such a file is never skipped silently.
func (c *visCheck) unparsable(p, kind, msg string) {
	c.out = append(c.out, fmt.Sprintf("%s: unparsable %s: %s", p, kind, msg))
}

// checkDir reports a reference from tier from to the tree directory dir when the matrix forbids it.
func (c *visCheck) checkDir(p string, line int, from tier, what, dir string) {
	if to := tierOf(dir); !allowed(from, to) {
		c.report(p, line, from, what, to)
	}
}

// checkModule checks a module path or import path against every directory the module map assigns to it.
func (c *visCheck) checkModule(p string, line int, from tier, what, importPath string) {
	for _, dir := range c.mods.resolve(importPath) {
		c.checkDir(p, line, from, what, dir)
	}
}

// checkRelative checks a relative path written in a file or manifest located in baseDir. A path that is
// absolute or climbs above the tree root cannot name tree code, so it is reported as leaving the tree.
func (c *visCheck) checkRelative(p string, line int, from tier, what, baseDir, rel string) {
	target := path.Join(baseDir, rel)
	if path.IsAbs(rel) || target == ".." || strings.HasPrefix(target, "../") {
		c.report(p, line, from, what, tierOutside)
		return
	}
	c.checkDir(p, line, from, what, target)
}

// moduleMap maps each module path declared by a go.mod in the tree to the sorted directories declaring it.
type moduleMap map[string][]string

// buildModuleMap reads the go.mod files among blobs. A go.mod that fails to parse still contributes its
// module line; its own problems are reported when it is checked.
func buildModuleMap(ctx context.Context, repoDir string, blobs []treeEntry) (moduleMap, error) {
	mods := moduleMap{}
	for _, e := range blobs {
		if path.Base(e.Path) != "go.mod" {
			continue
		}
		src, err := gitReadBlob(ctx, repoDir, e.OID)
		if err != nil {
			return nil, err
		}
		gm, _ := parseGoMod(src)
		if gm.module != "" {
			mods[gm.module] = append(mods[gm.module], path.Dir(e.Path))
		}
	}
	for _, dirs := range mods {
		slices.Sort(dirs)
	}
	return mods, nil
}

// resolve returns the directories importPath may name. The module whose path is importPath or its longest
// prefix supplies the directory, joined with the remainder. Nil means no tree module matches, so the import
// is external.
func (m moduleMap) resolve(importPath string) []string {
	best, found := "", false
	for mod := range m {
		if importPath != mod && !strings.HasPrefix(importPath, mod+"/") {
			continue
		}
		if !found || len(mod) > len(best) {
			best, found = mod, true
		}
	}
	if !found {
		return nil
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(importPath, best), "/")
	dirs := make([]string, 0, len(m[best]))
	for _, dir := range m[best] {
		dirs = append(dirs, path.Join(dir, rest))
	}
	return dirs
}

// changedModules returns the module paths whose declaring directories differ between before and after:
// added, removed, renamed, or moved modules.
func changedModules(before, after moduleMap) map[string]bool {
	changed := map[string]bool{}
	for mod, dirs := range before {
		if !slices.Equal(dirs, after[mod]) {
			changed[mod] = true
		}
	}
	for mod, dirs := range after {
		if !slices.Equal(dirs, before[mod]) {
			changed[mod] = true
		}
	}
	return changed
}

// manifestDirs maps the directory of each file named name to whether that file was added or modified (in
// changed). Files inherit evaluation from the nearest such directory at or above them.
func manifestDirs(blobs []treeEntry, name string, changed map[string]bool) map[string]bool {
	dirs := map[string]bool{}
	for _, e := range blobs {
		if path.Base(e.Path) == name {
			dirs[path.Dir(e.Path)] = changed[e.Path]
		}
	}
	return dirs
}

// ownerChanged reports whether the nearest directory at or above p that holds a manifest has a manifest that
// was added or modified.
func ownerChanged(p string, dirs map[string]bool) bool {
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		if changedManifest, ok := dirs[dir]; ok {
			return changedManifest
		}
		if dir == "." {
			return false
		}
	}
}

// goImport is one import spec of a Go file.
type goImport struct {
	importPath string
	line       int
}

// parseGoImports returns the import specs of a Go file. Only the file header is parsed, so syntax errors
// after the imports are not reported here.
func parseGoImports(p string, src []byte) ([]goImport, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make([]goImport, 0, len(f.Imports))
	for _, spec := range f.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		imports = append(imports, goImport{importPath: importPath, line: fset.Position(spec.Path.Pos()).Line})
	}
	return imports, nil
}

// importsAny reports whether an import is a module path in mods or a package below one.
func importsAny(imports []goImport, mods map[string]bool) bool {
	for _, imp := range imports {
		for mod := range mods {
			if imp.importPath == mod || strings.HasPrefix(imp.importPath, mod+"/") {
				return true
			}
		}
	}
	return false
}

// checkGo checks the imports of the Go file at p, in tier from. When evaluate is false, the file is checked
// only if it imports a module whose path or directory changed.
func (c *visCheck) checkGo(p string, src []byte, from tier, evaluate bool, changedMods map[string]bool) {
	imports, err := parseGoImports(p, src)
	if err != nil {
		c.unparsable(p, "Go file", strings.TrimPrefix(err.Error(), p+":"))
		return
	}
	if !evaluate && !importsAny(imports, changedMods) {
		return
	}
	for _, imp := range imports {
		c.checkModule(p, imp.line, from, imp.importPath, imp.importPath)
	}
}

// goMod is the part of a go.mod file that the check reads.
type goMod struct {
	module   string
	requires []goRequire
	replaces []goReplace
}

// goRequire is one require directive.
type goRequire struct {
	modPath string
	line    int
}

// goReplace is one replace directive. to is a module path or a local directory.
type goReplace struct {
	from string
	to   string
	line int
}

// parseGoMod reads the module, require, and replace directives of a go.mod file and ignores the rest. On
// error it still returns everything it read, and the error names the first bad line.
func parseGoMod(src []byte) (goMod, error) {
	var gm goMod
	var firstErr error
	block := ""
	for i, raw := range strings.Split(string(src), "\n") {
		line := i + 1
		fields, err := goModFields(raw)
		var dirErr error
		switch {
		case err != nil:
			dirErr = err
		case len(fields) == 0:
			continue
		case block != "" && len(fields) == 1 && fields[0] == ")":
			block = ""
			continue
		case block != "":
			dirErr = gm.directive(block, fields, line)
		case len(fields) == 2 && fields[1] == "(":
			block = fields[0]
			continue
		default:
			dirErr = gm.directive(fields[0], fields[1:], line)
		}
		if dirErr != nil && firstErr == nil {
			firstErr = fmt.Errorf("line %d: %w", line, dirErr)
		}
	}
	if block != "" && firstErr == nil {
		firstErr = fmt.Errorf("unterminated %s block", block)
	}
	return gm, firstErr
}

// directive applies one go.mod directive. Verbs other than module, require, and replace are ignored.
func (gm *goMod) directive(verb string, args []string, line int) error {
	switch verb {
	case "module":
		if len(args) != 1 {
			return errors.New("module takes one path")
		}
		gm.module = args[0]
	case "require":
		if len(args) < 1 {
			return errors.New("require without a module path")
		}
		gm.requires = append(gm.requires, goRequire{modPath: args[0], line: line})
	case "replace":
		arrow := slices.Index(args, "=>")
		if arrow < 1 || arrow+1 >= len(args) {
			return errors.New("replace without a => target")
		}
		gm.replaces = append(gm.replaces, goReplace{from: args[0], to: args[arrow+1], line: line})
	}
	return nil
}

// goModFields splits a go.mod line into tokens, dropping a // comment and unquoting quoted tokens.
func goModFields(raw string) ([]string, error) {
	if i := strings.Index(raw, "//"); i >= 0 {
		raw = raw[:i]
	}
	fields := strings.Fields(raw)
	for i, f := range fields {
		if f[0] == '"' || f[0] == '`' {
			u, err := strconv.Unquote(f)
			if err != nil {
				return nil, err
			}
			fields[i] = u
		}
	}
	return fields, nil
}

// checkGoMod checks the module references of the go.mod at p, in tier from. A local replace directory is
// resolved against the directory of the go.mod.
func (c *visCheck) checkGoMod(p string, src []byte, from tier) {
	gm, err := parseGoMod(src)
	if err != nil {
		c.unparsable(p, "go.mod", err.Error())
	}
	for _, r := range gm.requires {
		c.checkModule(p, r.line, from, r.modPath, r.modPath)
	}
	for _, rp := range gm.replaces {
		what := rp.from + " => " + rp.to
		c.checkModule(p, rp.line, from, what, rp.from)
		if isLocalPath(rp.to) {
			c.checkRelative(p, rp.line, from, what, path.Dir(p), rp.to)
		} else {
			c.checkModule(p, rp.line, from, what, rp.to)
		}
	}
}

// isLocalPath reports whether a go.mod replace target is a directory rather than a module path.
func isLocalPath(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || path.IsAbs(s)
}

// cargoPathRe matches a path key in a TOML line: a top-level key, an inline table entry, or a dotted key,
// with a basic or literal string value.
var cargoPathRe = regexp.MustCompile(`(?:^|[{,.])\s*path\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// checkCargo checks the path dependencies of the Cargo.toml at p, in tier from.
func (c *visCheck) checkCargo(p string, src []byte, from tier) {
	for i, raw := range strings.Split(string(src), "\n") {
		for _, m := range cargoPathRe.FindAllStringSubmatch(tomlCode(raw), -1) {
			// Only one of the two captures participates in a match, so their concatenation is the value.
			value := m[1] + m[2]
			c.checkRelative(p, i+1, from, "path "+value, path.Dir(p), value)
		}
	}
}

// tomlCode drops a TOML comment from one line. A # inside a quoted string is kept.
func tomlCode(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}

// checkPackageJSON checks the file: and link: dependencies of the package.json at p, in tier from.
func (c *visCheck) checkPackageJSON(p string, src []byte, from tier) {
	var pkg struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		PeerDependencies     map[string]string `json:"peerDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(src, &pkg); err != nil {
		c.unparsable(p, "package.json", err.Error())
		return
	}
	for _, deps := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
		for name, value := range deps {
			if rel, ok := npmLocalPath(value); ok {
				c.checkRelative(p, jsonLine(src, value), from, name+" "+value, path.Dir(p), rel)
			}
		}
	}
}

// npmLocalPath returns the directory named by a file: or link: dependency value.
func npmLocalPath(value string) (string, bool) {
	if rel, ok := strings.CutPrefix(value, "file:"); ok {
		return rel, true
	}
	return strings.CutPrefix(value, "link:")
}

// jsonLine returns the 1-based line of the first occurrence of the JSON string value, or 0 when absent.
func jsonLine(src []byte, value string) int {
	i := bytes.Index(src, []byte(`"`+value+`"`))
	if i < 0 {
		return 0
	}
	return bytes.Count(src[:i], []byte("\n")) + 1
}

// jsExtensions are the TypeScript and JavaScript extensions whose relative specifiers are checked.
var jsExtensions = map[string]bool{".cjs": true, ".cts": true, ".js": true, ".jsx": true, ".mjs": true, ".mts": true, ".ts": true, ".tsx": true}

// jsToken is one token of a TypeScript or JavaScript file. Comments and whitespace produce no tokens.
type jsToken struct {
	kind byte // 'w' word, 's' string literal, 'p' punctuation, 'o' other literal
	text string
	line int
}

// jsSpecifier is a relative module specifier and the line it appears on.
type jsSpecifier struct {
	value string
	line  int
}

// jsRegexKeywords are words after which a slash begins a regular expression rather than a division.
var jsRegexKeywords = map[string]bool{"await": true, "case": true, "delete": true, "do": true, "else": true, "in": true, "instanceof": true, "new": true, "of": true, "return": true, "throw": true, "typeof": true, "void": true, "yield": true}

// checkJS checks the relative module specifiers of the TypeScript or JavaScript file at p, in tier from.
func (c *visCheck) checkJS(p string, src []byte, from tier) {
	for _, s := range jsRelativeSpecifiers(src) {
		c.checkRelative(p, s.line, from, s.value, path.Dir(p), s.value)
	}
}

// jsRelativeSpecifiers returns the relative specifiers that follow from, import, require(, or import(. A
// string that merely appears in a comment or in other code is not a specifier.
func jsRelativeSpecifiers(src []byte) []jsSpecifier {
	toks := scanJS(bytes.TrimPrefix(src, []byte("\uFEFF")))
	var out []jsSpecifier
	for i, t := range toks {
		if t.kind != 's' || i == 0 || !isRelativeSpecifier(t.text) {
			continue
		}
		prev := toks[i-1]
		viaKeyword := prev.kind == 'w' && (prev.text == "from" || prev.text == "import")
		viaCall := prev.kind == 'p' && prev.text == "(" && i >= 2 && toks[i-2].kind == 'w' &&
			(toks[i-2].text == "require" || toks[i-2].text == "import") && !(i >= 3 && toks[i-3].text == ".")
		if viaKeyword || viaCall {
			out = append(out, jsSpecifier{value: t.text, line: t.line})
		}
	}
	return out
}

// isRelativeSpecifier reports whether a module specifier is relative to the importing file.
func isRelativeSpecifier(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// scanJS tokenizes a TypeScript or JavaScript file. It is not a parser. It understands comments, string and
// template literals, regular expression literals (by the usual preceding-token rule), and punctuation, which
// is enough to find module specifiers without matching comments or strings that merely mention them.
func scanJS(src []byte) []jsToken {
	var toks []jsToken
	line := 1
	for i := 0; i < len(src); {
		kind, text, end := jsNext(src, i, toks)
		if kind != 0 {
			toks = append(toks, jsToken{kind: kind, text: text, line: line})
		}
		line += bytes.Count(src[i:end], []byte("\n"))
		i = end
	}
	return toks
}

// jsNext scans the item at src[i]. It returns a token, or kind 0 for whitespace and comments, and the index
// after the item. The index always advances.
func jsNext(src []byte, i int, prev []jsToken) (byte, string, int) {
	n := len(src)
	c := src[i]
	switch {
	case isJSSpace(c):
		end := i
		for end < n && isJSSpace(src[end]) {
			end++
		}
		return 0, "", end
	case c == '/' && i+1 < n && src[i+1] == '/':
		if end := bytes.IndexByte(src[i:], '\n'); end >= 0 {
			return 0, "", i + end
		}
		return 0, "", n
	case c == '/' && i+1 < n && src[i+1] == '*':
		if end := bytes.Index(src[i+2:], []byte("*/")); end >= 0 {
			return 0, "", i + 2 + end + 2
		}
		return 0, "", n
	case c == '\'' || c == '"':
		return jsQuoted(src, i)
	case c == '`':
		return jsTemplate(src, i)
	case c == '/' && jsRegexAllowed(prev):
		if end, ok := jsRegexEnd(src, i); ok {
			return 'o', "", end
		}
	case c >= '0' && c <= '9':
		end := i
		for end < n && (isJSWordByte(src[end]) || src[end] == '.') {
			end++
		}
		return 'o', "", end
	case isJSWordByte(c):
		end := i
		for end < n && isJSWordByte(src[end]) {
			end++
		}
		return 'w', string(src[i:end]), end
	}
	return 'p', string(c), i + 1
}

// jsQuoted scans a single- or double-quoted string literal at src[i]. A raw newline ends the literal early,
// so an unterminated quote cannot swallow the rest of the file.
func jsQuoted(src []byte, i int) (byte, string, int) {
	q := src[i]
	var b []byte
	for j := i + 1; j < len(src) && src[j] != '\n'; j++ {
		if src[j] == q {
			return 's', string(b), j + 1
		}
		if src[j] == '\\' && j+1 < len(src) {
			j++
		}
		b = append(b, src[j])
	}
	return 'p', string(q), i + 1
}

// jsTemplate scans a template literal at src[i]. A template with a substitution is an 'o' token, so its text
// is never read as a specifier.
func jsTemplate(src []byte, i int) (byte, string, int) {
	var b []byte
	subst := false
	for j := i + 1; j < len(src); j++ {
		if src[j] == '`' {
			if subst {
				return 'o', "", j + 1
			}
			return 's', string(b), j + 1
		}
		if src[j] == '\\' && j+1 < len(src) {
			j++
		}
		if src[j] == '$' && j+1 < len(src) && src[j+1] == '{' {
			subst = true
		}
		b = append(b, src[j])
	}
	return 'p', "`", i + 1
}

// jsRegexAllowed reports whether a slash after prev begins a regular expression literal.
func jsRegexAllowed(prev []jsToken) bool {
	if len(prev) == 0 {
		return true
	}
	last := prev[len(prev)-1]
	switch last.kind {
	case 'w':
		return jsRegexKeywords[last.text]
	case 'p':
		return last.text != ")" && last.text != "]" && last.text != "}"
	}
	return false
}

// jsRegexEnd returns the end of the regular expression literal that starts at src[i]. It returns false when
// the line ends first, so the slash is then read as division.
func jsRegexEnd(src []byte, i int) (int, bool) {
	inClass := false
	for j := i + 1; j < len(src) && src[j] != '\n'; j++ {
		c := src[j]
		switch {
		case c == '\\':
			j++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '/':
			end := j + 1
			for end < len(src) && isJSWordByte(src[end]) {
				end++
			}
			return end, true
		}
	}
	return 0, false
}

func isJSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// isJSWordByte reports whether c can appear in an identifier or number. Bytes of multi-byte UTF-8 sequences
// count as identifier bytes.
func isJSWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// CheckVisibility returns the violations in every file of treeOID.
func CheckVisibility(ctx context.Context, repoDir string, treeOID string) ([]string, error) {
	return CheckVisibilityDiff(ctx, repoDir, "", treeOID)
}

// CheckVisibilityDiff returns the sorted violation lines of treeOID, each formatted as
// "<path>:<line>: prohibited import in <tier> domain: <reference> -> <target tier>". With a non-empty
// baseTreeOID, a file is evaluated only if it was added or modified since the base, or if its nearest go.mod
// (for Go) or package.json (for TypeScript and JavaScript) was. Violations already present in the base are
// therefore ignored. If a module path or directory changed, every Go file importing that path is evaluated
// too, because the import may now resolve into another tier.
func CheckVisibilityDiff(ctx context.Context, repoDir string, baseTreeOID string, treeOID string) ([]string, error) {
	blobs, err := listBlobs(ctx, repoDir, treeOID)
	if err != nil {
		return nil, err
	}
	mods, err := buildModuleMap(ctx, repoDir, blobs)
	if err != nil {
		return nil, err
	}

	full := baseTreeOID == ""
	changed := map[string]bool{}
	var changedMods map[string]bool
	if !full {
		raw, err := gitOutput(ctx, repoDir, nil, "diff-tree", "-r", "-z", "--no-commit-id", "--no-renames", "--diff-filter=AM", baseTreeOID, treeOID)
		if err != nil {
			return nil, fmt.Errorf("diff-tree %s %s: %w", baseTreeOID, treeOID, err)
		}
		diffs, err := parseDiffTree(raw)
		if err != nil {
			return nil, err
		}
		for _, e := range diffs {
			if e.Type == "blob" {
				changed[e.Path] = true
			}
		}
		baseBlobs, err := listBlobs(ctx, repoDir, baseTreeOID)
		if err != nil {
			return nil, err
		}
		baseMods, err := buildModuleMap(ctx, repoDir, baseBlobs)
		if err != nil {
			return nil, err
		}
		changedMods = changedModules(baseMods, mods)
	}

	goDirs := manifestDirs(blobs, "go.mod", changed)
	pkgDirs := manifestDirs(blobs, "package.json", changed)
	c := &visCheck{mods: mods}
	for _, e := range blobs {
		from := tierOf(path.Dir(e.Path))
		if from == tierNone {
			continue
		}
		name, ext := path.Base(e.Path), path.Ext(e.Path)
		isGo, isJS := ext == ".go", jsExtensions[ext]
		if !isGo && !isJS && name != "go.mod" && name != "Cargo.toml" && name != "package.json" {
			continue
		}
		evaluate := full || changed[e.Path]
		if isGo {
			evaluate = evaluate || ownerChanged(e.Path, goDirs)
		}
		if isJS {
			evaluate = evaluate || ownerChanged(e.Path, pkgDirs)
		}
		// An unchanged Go file is still read when a module path changed, because its imports may now resolve elsewhere.
		if !evaluate && !(isGo && len(changedMods) > 0) {
			continue
		}
		src, err := gitReadBlob(ctx, repoDir, e.OID)
		if err != nil {
			return nil, err
		}
		switch {
		case name == "go.mod":
			c.checkGoMod(e.Path, src, from)
		case name == "Cargo.toml":
			c.checkCargo(e.Path, src, from)
		case name == "package.json":
			c.checkPackageJSON(e.Path, src, from)
		case isGo:
			c.checkGo(e.Path, src, from, evaluate, changedMods)
		default:
			c.checkJS(e.Path, src, from)
		}
	}
	slices.Sort(c.out)
	return slices.Compact(c.out), nil
}

// listBlobs returns the blob entries of a tree, recursively, in path order.
func listBlobs(ctx context.Context, repoDir string, treeOID string) ([]treeEntry, error) {
	raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", treeOID)
	if err != nil {
		return nil, fmt.Errorf("ls-tree %s: %w", treeOID, err)
	}
	entries, err := parseLSTree(raw)
	if err != nil {
		return nil, err
	}
	blobs := make([]treeEntry, 0, len(entries))
	for _, e := range entries {
		if e.Type == "blob" {
			blobs = append(blobs, e)
		}
	}
	return blobs, nil
}
