package lander

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

// fixtureStore returns a fresh bare store holding one tree. files become regular blobs and links become
// symlinks whose target is the map value.
func fixtureStore(t *testing.T, files, links map[string]string) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "store.git")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var entries []TreeEntry
	add := func(p, body, mode string) {
		oid, err := s.GitStdin(ctx, body, "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, TreeEntry{Mode: mode, Type: "blob", OID: oid, Path: p})
	}
	for p, body := range files {
		add(p, body, "100644")
	}
	for p, target := range links {
		add(p, target, "120000")
	}
	tree, err := s.BuildTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	return s, tree
}

// projectKeys renders projects as "dir kind" for comparison.
func projectKeys(ps []Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Dir+" "+p.Kind)
	}
	return out
}

func wantProjects(t *testing.T, got []Project, want ...string) {
	t.Helper()
	if !slices.Equal(projectKeys(got), want) {
		t.Fatalf("projects = %q, want %q", projectKeys(got), want)
	}
}

// hqqFiles is the HQQ fixture: a base module, two modules that replace it with a local directory, one
// that requires it through a block, and an unrelated leaf module.
var hqqFiles = map[string]string{
	"ipc/hqq/HQQ/go.mod":         "module gosuda.org/hqq\n\ngo 1.21\n",
	"ipc/hqq/HQQ/hqq.go":         "package hqq\n",
	"ipc/hqq/hqq-sqlite3/go.mod": "module gosuda.org/hqq/hqq-sqlite3\n\ngo 1.21\n\nrequire gosuda.org/hqq v0.0.0\n\nreplace gosuda.org/hqq => ../HQQ\n",
	"ipc/hqq/hqq-sqlite3/db.go":  "package sqlite3\n",
	"ipc/hqq/hqq-zstd/go.mod":    "module gosuda.org/hqq/hqq-zstd\n\ngo 1.21\n\nrequire (\n\tgosuda.org/hqq v0.0.0\n)\n\nreplace gosuda.org/hqq => ../HQQ\n",
	"ipc/hqq/hqq-zstd/z.go":      "package zstd\n",
	"net/grt/go.mod":             "module gosuda.org/0/net/grt\n\ngo 1.21\n",
	"net/grt/grt.go":             "package grt\n",
}

func TestAffectedGoReverseDependencies(t *testing.T) {
	s, tree := fixtureStore(t, hqqFiles, nil)
	ctx := context.Background()

	got, err := AffectedProjects(ctx, s, tree, []string{"ipc/hqq/HQQ/hqq.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "ipc/hqq/HQQ go", "ipc/hqq/hqq-sqlite3 go", "ipc/hqq/hqq-zstd go")
	if got[0].Module != "gosuda.org/hqq" {
		t.Fatalf("module of HQQ = %q", got[0].Module)
	}

	got, err = AffectedProjects(ctx, s, tree, []string{"net/grt/grt.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "net/grt go")

	closure, err := ProjectClosure(ctx, s, tree, []string{"ipc/hqq/hqq-sqlite3/db.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, closure, "ipc/hqq/HQQ go", "ipc/hqq/hqq-sqlite3 go")
}

func TestAffectedDeletedPathMapsToAncestor(t *testing.T) {
	s, tree := fixtureStore(t, hqqFiles, nil)
	ctx := context.Background()

	got, err := AffectedProjects(ctx, s, tree, []string{"ipc/hqq/HQQ/old/gone.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "ipc/hqq/HQQ go", "ipc/hqq/hqq-sqlite3 go", "ipc/hqq/hqq-zstd go")

	got, err = AffectedProjects(ctx, s, tree, []string{"ipc/unknown/gone.go", "docs/readme.md"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got)
}

func TestAffectedCargoAndNodeDependencies(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"crates/lib/Cargo.toml": "[package]\nname = \"lib\"\nversion = \"0.1.0\"\n",
		"crates/lib/src/lib.rs": "pub fn f() {}\n",
		"crates/lib/Cargo.lock": "# lock\n",
		"crates/app/Cargo.toml": "[package]\nname = \"app\"\n\n[dependencies]\nlib = { path = \"../lib\" }\n",
		"ws/Cargo.toml":         "[workspace]\nmembers = [\"m\"]\n\n[workspace.dependencies]\nlib = { path = \"../crates/lib\" }\n",
		"ws/m/Cargo.toml":       "[package]\nname = \"m\"\n\n[dependencies]\nlib = { path = \"../../crates/lib\" }\n",
		"web/package.json":      `{"name":"web","dependencies":{"ui":"workspace:*","tool":"file:../tool"}}`,
		"ui/package.json":       `{"name":"ui","scripts":{"test":"bun test"}}`,
		"ui/index.ts":           "export {}\n",
		"tool/package.json":     `{"name":"tool"}`,
		"tool/index.ts":         "export {}\n",
	}, nil)
	ctx := context.Background()

	got, err := AffectedProjects(ctx, s, tree, []string{"crates/lib/src/lib.rs"})
	if err != nil {
		t.Fatal(err)
	}
	// ws/m is a workspace member: it is tested through ws, not as a project of its own.
	wantProjects(t, got, "crates/app cargo", "crates/lib cargo", "ws cargo")

	got, err = AffectedProjects(ctx, s, tree, []string{"ui/index.ts"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "ui node", "web node")

	got, err = AffectedProjects(ctx, s, tree, []string{"tool/index.ts"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "tool node", "web node")
}

func TestAffectedRootProjectDependents(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.mod":     "module example.com/root\n\ngo 1.21\n",
		"README.md":  "readme\n",
		"sub/go.mod": "module example.com/root/sub\n\ngo 1.21\n\nrequire example.com/root v0.0.0\n\nreplace example.com/root => ../\n",
		"sub/s.go":   "package sub\n",
	}, nil)
	ctx := context.Background()

	got, err := AffectedProjects(ctx, s, tree, []string{"README.md"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, ". go", "sub go")

	got, err = AffectedProjects(ctx, s, tree, []string{"sub/s.go"})
	if err != nil {
		t.Fatal(err)
	}
	wantProjects(t, got, "sub go")
}

func TestAffectedParseGoModDirectives(t *testing.T) {
	f, err := parseGoMod(`// generated
module "example.com/m" // trailing comment
go 1.21
require (
	example.com/a v1.0.0 // indirect
)
replace example.com/b v1.0.0 => ../b
replace example.com/c => example.com/fork v1.2.3
replace (
	example.com/d => ./d
)
`)
	if err != nil {
		t.Fatal(err)
	}
	if f.module != "example.com/m" {
		t.Fatalf("module = %q", f.module)
	}
	if f.goVersion != "1.21" {
		t.Fatalf("goVersion = %q", f.goVersion)
	}
	if !slices.Equal(f.requires, []string{"example.com/a"}) {
		t.Fatalf("requires = %q", f.requires)
	}
	want := map[string]string{"example.com/b": "../b", "example.com/d": "./d"}
	if len(f.replaces) != len(want) {
		t.Fatalf("replaces = %v, want %v", f.replaces, want)
	}
	for k, v := range want {
		if f.replaces[k] != v {
			t.Fatalf("replaces[%s] = %q, want %q", k, f.replaces[k], v)
		}
	}
	if _, err := parseGoMod("go 1.21\n"); err == nil {
		t.Fatal("go.mod without module directive was accepted")
	}
}

func TestAffectedParseGoWork(t *testing.T) {
	w := parseGoWork("go 1.25 // trunk\n\nuse (\n\t./a // member\n\t\"./b c\"\n)\nuse ./d\n\n" +
		"replace github.com/x v1 => ./e\n" +
		"replace (\n\tgithub.com/y => ../f // local\n\tgithub.com/z => example.com/z v1.0.0\n)\n")
	if w.goVersion != "1.25" {
		t.Fatalf("goVersion = %q", w.goVersion)
	}
	if want := []string{"a", "b c", "d"}; len(w.uses) != len(want) {
		t.Fatalf("uses = %v, want %q", w.uses, want)
	} else {
		for _, d := range want {
			if !w.uses[d] {
				t.Fatalf("uses = %v, lacks %q", w.uses, d)
			}
		}
	}
	got := map[string]string{}
	for _, r := range w.replaces {
		got[r.text] = r.target
	}
	want := map[string]string{"github.com/x v1 => ./e": "./e", "github.com/y => ../f": "../f"}
	if len(got) != len(want) {
		t.Fatalf("replaces = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("replaces[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestAffectedParseCargoSections(t *testing.T) {
	f := parseCargo("[package]\nname = \"x\"\n[lib]\npath = \"src/lib.rs\"\n" +
		"[dependencies]\na = { path = \"../a\", version = \"1\" }\n" +
		"[dev-dependencies.b]\npath = \"../b\" # comment\n" +
		"[patch.crates-io]\nc = { path = \"../c\" }\n[workspace]\n")
	if !slices.Equal(f.pathDeps, []string{"../a", "../b", "../c"}) {
		t.Fatalf("pathDeps = %q", f.pathDeps)
	}
	if !f.workspace {
		t.Fatal("[workspace] not detected")
	}
}
