package lander

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Project kinds. A directory may hold several kinds; each is a separate Project.
const (
	kindGo    = "go"
	kindCargo = "cargo"
	kindNode  = "node"
)

// Project is a build or test unit: a directory holding go.mod, Cargo.toml, or package.json.
type Project struct {
	Dir    string // slash-separated, relative to the repository root; "." for the root
	Kind   string // go, cargo, or node
	Module string // Go module path; empty for other kinds
}

// projectNode is a Project with the facts the presubmit needs and its dependency edges.
type projectNode struct {
	Project
	pkgName   string          // node: package.json name, matched by workspace:<name> dependencies
	lock      bool            // lockfile present: Cargo.lock; bun.lock, bun.lockb, or package-lock.json
	tests     bool            // node: a test script or *.test.ts files exist
	goVersion string          // go: the go directive of the module; empty when absent
	workspace bool            // go: listed by a use directive of the trunk's root go.work
	deps      map[string]bool // ids of the projects this one depends on
}

func (n *projectNode) id() string { return n.Kind + ":" + n.Dir }

// projectGraph holds every project of one tree and the edges between them.
type projectGraph struct {
	nodes map[string]*projectNode    // by id
	paths map[string]bool            // every blob path in the tree, symlinks included
	rdeps map[string]map[string]bool // dependency id -> ids of its dependents
	trunk goWork                     // the trunk's root go.work; zero when absent
}

// goModFile is the part of a go.mod the graph uses.
type goModFile struct {
	module    string
	goVersion string
	requires  []string
	replaces  map[string]string // module path -> local directory target ("./" or "../" relative)
}

// cargoFile is the part of a Cargo.toml the graph uses.
type cargoFile struct {
	workspace bool     // declares [workspace]
	pathDeps  []string // path = "..." entries in dependency and patch sections
}

// packageFile is the part of a package.json the graph uses.
type packageFile struct {
	Name                 string            `json:"name"`
	Scripts              map[string]string `json:"scripts"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// AffectedProjects returns the projects that own a changed path in newTree, plus every project that
// transitively depends on one of them. Changed paths resolve to the nearest ancestor project; a
// path with no ancestor project (including a deleted path) affects nothing. Results are sorted by
// Dir and then Kind. Manifests are read from the object store; no file is checked out.
func AffectedProjects(ctx context.Context, s *Store, newTree string, changed []string) ([]Project, error) {
	g, err := loadProjects(ctx, s, newTree)
	if err != nil {
		return nil, err
	}
	return projectsOf(g.affected(changed)), nil
}

// ProjectClosure returns the affected projects of AffectedProjects plus every project they depend
// on, transitively. It is the set whose sources the presubmit needs.
func ProjectClosure(ctx context.Context, s *Store, newTree string, changed []string) ([]Project, error) {
	g, err := loadProjects(ctx, s, newTree)
	if err != nil {
		return nil, err
	}
	return projectsOf(g.closure(g.affected(changed))), nil
}

func projectsOf(nodes []*projectNode) []Project {
	out := make([]Project, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Project)
	}
	return out
}

// loadProjects scans tree for manifests and builds the project graph.
func loadProjects(ctx context.Context, s *Store, tree string) (*projectGraph, error) {
	entries, err := s.ListTree(ctx, tree, true)
	if err != nil {
		return nil, err
	}
	g := &projectGraph{
		nodes: map[string]*projectNode{},
		paths: map[string]bool{},
		rdeps: map[string]map[string]bool{},
	}
	var manifests []TreeEntry
	for _, e := range entries {
		if e.Type != "blob" {
			continue // submodule commits are not part of any archive
		}
		g.paths[e.Path] = true
		if isManifestName(path.Base(e.Path)) && e.Mode != "120000" && !inNodeModules(e.Path) {
			manifests = append(manifests, e)
		}
	}

	if g.paths[goWorkName] {
		body, err := s.Git(ctx, "cat-file", "blob", tree+":"+goWorkName)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", goWorkName, err)
		}
		g.trunk = parseGoWork(body)
	}

	goFiles := map[string]goModFile{}
	cargoFiles := map[string]cargoFile{}
	pkgFiles := map[string]packageFile{}
	for _, m := range manifests {
		dir := path.Dir(m.Path)
		body, err := s.Git(ctx, "cat-file", "blob", m.OID)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", m.Path, err)
		}
		switch path.Base(m.Path) {
		case "go.mod":
			f, err := parseGoMod(body)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", m.Path, err)
			}
			goFiles[dir] = f
		case "Cargo.toml":
			cargoFiles[dir] = parseCargo(body)
		case "package.json":
			var f packageFile
			if err := json.Unmarshal([]byte(body), &f); err != nil {
				return nil, fmt.Errorf("%s: %w", m.Path, err)
			}
			pkgFiles[dir] = f
		}
	}

	for dir, f := range goFiles {
		g.add(&projectNode{
			Project:   Project{Dir: dir, Kind: kindGo, Module: f.module},
			goVersion: f.goVersion,
			workspace: g.trunk.uses[dir],
		})
	}
	for dir := range cargoFiles {
		if cargoMember(dir, cargoFiles) {
			continue // a workspace member is tested as part of its workspace root
		}
		g.add(&projectNode{
			Project: Project{Dir: dir, Kind: kindCargo},
			lock:    g.paths[path.Join(dir, "Cargo.lock")],
		})
	}
	for dir, f := range pkgFiles {
		g.add(&projectNode{
			Project: Project{Dir: dir, Kind: kindNode},
			pkgName: f.Name,
			lock:    g.anyPath(dir, "bun.lock", "bun.lockb", "package-lock.json"),
			tests:   f.Scripts["test"] != "" || g.hasTestFile(dir),
		})
	}

	goByModule := map[string][]*projectNode{}
	nodeByName := map[string][]*projectNode{}
	for _, n := range g.nodes {
		switch {
		case n.Kind == kindGo:
			goByModule[n.Module] = append(goByModule[n.Module], n)
		case n.Kind == kindNode && n.pkgName != "":
			nodeByName[n.pkgName] = append(nodeByName[n.pkgName], n)
		}
	}

	for dir, f := range goFiles {
		src := g.nodes[kindGo+":"+dir]
		for _, r := range f.requires {
			for _, t := range goByModule[r] {
				g.link(src, t)
			}
		}
		for _, target := range f.replaces {
			g.linkLocal(src, dir, target, kindGo)
		}
	}
	for dir, f := range cargoFiles {
		src := g.owner(kindCargo, dir)
		if src == nil {
			continue
		}
		for _, p := range f.pathDeps {
			g.linkLocal(src, dir, p, kindCargo)
		}
	}
	for dir, f := range pkgFiles {
		src := g.nodes[kindNode+":"+dir]
		for name, spec := range f.dependencySpecs() {
			switch {
			case strings.HasPrefix(spec, "file:"):
				g.linkLocal(src, dir, strings.TrimPrefix(spec, "file:"), kindNode)
			case strings.HasPrefix(spec, "link:"):
				g.linkLocal(src, dir, strings.TrimPrefix(spec, "link:"), kindNode)
			case strings.HasPrefix(spec, "workspace:"):
				rest := strings.TrimPrefix(spec, "workspace:")
				if isLocalRef(rest) {
					g.linkLocal(src, dir, rest, kindNode)
				} else {
					for _, t := range nodeByName[name] {
						g.link(src, t)
					}
				}
			}
		}
	}
	return g, nil
}

// add registers n and initialises its edge sets.
func (g *projectGraph) add(n *projectNode) {
	n.deps = map[string]bool{}
	g.nodes[n.id()] = n
}

// link records that src depends on dst. Self-edges are ignored.
func (g *projectGraph) link(src, dst *projectNode) {
	if src == nil || dst == nil || src == dst {
		return
	}
	src.deps[dst.id()] = true
	if g.rdeps[dst.id()] == nil {
		g.rdeps[dst.id()] = map[string]bool{}
	}
	g.rdeps[dst.id()][src.id()] = true
}

// linkLocal links src to the project of kind that owns rel, resolved against base.
func (g *projectGraph) linkLocal(src *projectNode, base, rel, kind string) {
	d, ok := joinLocal(base, rel)
	if !ok {
		return
	}
	g.link(src, g.owner(kind, d))
}

// owner returns the nearest project of kind at dir or above it.
func (g *projectGraph) owner(kind, dir string) *projectNode {
	for d := dir; ; d = path.Dir(d) {
		if n := g.nodes[kind+":"+d]; n != nil {
			return n
		}
		if d == "." {
			return nil
		}
	}
}

// ownersOf returns every project at the nearest directory at or above dir that holds any project.
func (g *projectGraph) ownersOf(dir string) []*projectNode {
	for d := dir; ; d = path.Dir(d) {
		var out []*projectNode
		for _, kind := range []string{kindGo, kindCargo, kindNode} {
			if n := g.nodes[kind+":"+d]; n != nil {
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			return out
		}
		if d == "." {
			return nil
		}
	}
}

// anyPath reports whether any of names exists directly inside dir.
func (g *projectGraph) anyPath(dir string, names ...string) bool {
	for _, n := range names {
		if g.paths[path.Join(dir, n)] {
			return true
		}
	}
	return false
}

// hasTestFile reports whether a *.test.ts file lies under dir, outside node_modules.
func (g *projectGraph) hasTestFile(dir string) bool {
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}
	for p := range g.paths {
		if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, ".test.ts") && !inNodeModules(p) {
			return true
		}
	}
	return false
}

// affected returns the projects owning a changed path and every project that transitively depends
// on them.
func (g *projectGraph) affected(changed []string) []*projectNode {
	seen := map[string]bool{}
	var queue []string
	mark := func(id string) {
		if !seen[id] {
			seen[id] = true
			queue = append(queue, id)
		}
	}
	for _, c := range changed {
		for _, n := range g.ownersOf(path.Dir(c)) {
			mark(n.id())
		}
	}
	for i := 0; i < len(queue); i++ {
		for dependent := range g.rdeps[queue[i]] {
			mark(dependent)
		}
	}
	return g.resolve(queue)
}

// closure returns seeds and every project they transitively depend on.
func (g *projectGraph) closure(seeds []*projectNode) []*projectNode {
	seen := map[string]bool{}
	var queue []string
	mark := func(id string) {
		if !seen[id] {
			seen[id] = true
			queue = append(queue, id)
		}
	}
	for _, s := range seeds {
		mark(s.id())
	}
	for i := 0; i < len(queue); i++ {
		for dep := range g.nodes[queue[i]].deps {
			mark(dep)
		}
	}
	return g.resolve(queue)
}

// resolve maps ids to nodes sorted by Dir and then Kind.
func (g *projectGraph) resolve(ids []string) []*projectNode {
	out := make([]*projectNode, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.nodes[id])
	}
	slices.SortFunc(out, func(a, b *projectNode) int {
		return cmp.Or(strings.Compare(a.Dir, b.Dir), strings.Compare(a.Kind, b.Kind))
	})
	return out
}

// cargoMember reports whether the crate at dir lies under a strict ancestor manifest that declares
// [workspace]. The check is deliberately simple: it does not consult the workspace's members list.
func cargoMember(dir string, files map[string]cargoFile) bool {
	if dir == "." {
		return false
	}
	for d := path.Dir(dir); ; d = path.Dir(d) {
		if f, ok := files[d]; ok && f.workspace {
			return true
		}
		if d == "." {
			return false
		}
	}
}

// hasCargoConfig reports whether a .cargo/config(.toml) lies at dir or any ancestor. Cargo reads
// such files from the working tree, and a registry credential provider they name would run repo
// code in the lander process, so the lander never runs cargo fetch under one.
func hasCargoConfig(g *projectGraph, dir string) bool {
	for d := dir; ; d = path.Dir(d) {
		if g.paths[path.Join(d, ".cargo", "config.toml")] || g.paths[path.Join(d, ".cargo", "config")] {
			return true
		}
		if d == "." {
			return false
		}
	}
}

func (f packageFile) dependencySpecs() map[string]string {
	out := map[string]string{}
	for _, m := range []map[string]string{f.Dependencies, f.DevDependencies, f.PeerDependencies, f.OptionalDependencies} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func isManifestName(base string) bool {
	return base == "go.mod" || base == "Cargo.toml" || base == "package.json"
}

func inNodeModules(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "node_modules")
}

// isLocalRef reports whether a module or package reference names a relative directory.
func isLocalRef(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// joinLocal resolves rel against the directory base. It fails for absolute references and for
// references that leave the repository root.
func joinLocal(base, rel string) (string, bool) {
	if path.IsAbs(rel) {
		return "", false
	}
	p := path.Clean(path.Join(base, rel))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", false
	}
	return p, true
}

var cargoPathRe = regexp.MustCompile(`(?:^|[\s,{])path\s*=\s*"([^"]*)"`)

// parseCargo reads the path dependencies and the [workspace] header of a Cargo.toml. It is a line
// scanner, not a TOML parser: it honours section headers and ignores comments starting with #.
func parseCargo(src string) cargoFile {
	var f cargoFile
	section := ""
	for _, raw := range strings.Split(src, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[] \t")
			f.workspace = f.workspace || section == "workspace"
		case isCargoDepSection(section):
			for _, m := range cargoPathRe.FindAllStringSubmatch(line, -1) {
				f.pathDeps = append(f.pathDeps, m[1])
			}
		}
	}
	return f
}

func isCargoDepSection(section string) bool {
	return strings.Contains(section, "dependencies") || strings.HasPrefix(section, "patch")
}

// goTokenRe matches one token of a directive line: a double-quoted or back-quoted string, which may hold
// spaces, or a run of other characters.
var goTokenRe = regexp.MustCompile("\"[^\"]*\"|`[^`]*`|[^\\s\"`]+")

// goDirectives splits a go.mod or go.work into its directives, one token list each. A line inside a block
// is returned with its verb first ("replace a => b" for a line of "replace ("). Comments are removed.
func goDirectives(src string) [][]string {
	var out [][]string
	block := ""
	for _, raw := range strings.Split(src, "\n") {
		line := raw
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			line = block + " " + line
		} else if verb, ok := strings.CutSuffix(line, "("); ok {
			block = strings.TrimSpace(verb)
			continue
		}
		if fields := goTokenRe.FindAllString(line, -1); len(fields) > 0 {
			out = append(out, fields)
		}
	}
	return out
}

// goWork is the part of a go.work the presubmit uses.
type goWork struct {
	goVersion string
	uses      map[string]bool // module directories listed by use directives
	replaces  []goWorkReplace
}

// goWorkReplace is a replace directive of a go.work whose target is a local directory.
type goWorkReplace struct {
	text   string // the directive without its verb, as written: module [version] => target [version]
	target string // the target, unquoted; relative to the go.work directory
}

// parseGoWork reads the go directive and the replace directives with local targets of a go.work. Both
// single-line and block forms are read.
func parseGoWork(src string) goWork {
	w := goWork{uses: map[string]bool{}}
	for _, fields := range goDirectives(src) {
		switch fields[0] {
		case "go":
			if len(fields) >= 2 {
				w.goVersion = fields[1]
			}
		case "use":
			if len(fields) >= 2 {
				if d, ok := joinLocal(".", unquoteModPath(fields[1])); ok {
					w.uses[d] = true
				}
			}
		case "replace":
			arrow := slices.Index(fields, "=>")
			if arrow >= 2 && arrow+1 < len(fields) {
				if target := unquoteModPath(fields[arrow+1]); isLocalRef(target) {
					w.replaces = append(w.replaces, goWorkReplace{text: strings.Join(fields[1:], " "), target: target})
				}
			}
		}
	}
	return w
}

// parseGoMod reads the module path, go version, require paths, and local replace targets of a go.mod.
func parseGoMod(src string) (goModFile, error) {
	f := goModFile{replaces: map[string]string{}}
	for _, fields := range goDirectives(src) {
		switch fields[0] {
		case "module":
			if len(fields) >= 2 {
				f.module = unquoteModPath(fields[1])
			}
		case "go":
			if len(fields) >= 2 {
				f.goVersion = fields[1]
			}
		case "require":
			if len(fields) >= 2 {
				f.requires = append(f.requires, unquoteModPath(fields[1]))
			}
		case "replace":
			arrow := slices.Index(fields, "=>")
			if arrow >= 2 && arrow+1 < len(fields) {
				if target := unquoteModPath(fields[arrow+1]); isLocalRef(target) {
					f.replaces[unquoteModPath(fields[1])] = target
				}
			}
		}
	}
	if f.module == "" {
		return f, errors.New("go.mod has no module directive")
	}
	return f, nil
}

func unquoteModPath(s string) string {
	return strings.Trim(s, "\"`")
}
