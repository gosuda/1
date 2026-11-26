package capy_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"gosuda.org/0/scripts/capy"
)

// writeTree writes files given as alternating paths and contents.
func writeTree(t *testing.T, repo string, pathsAndContents ...string) {
	t.Helper()
	if len(pathsAndContents)%2 != 0 {
		t.Fatalf("writeTree: odd argument count %d", len(pathsAndContents))
	}
	files := make(map[string]string, len(pathsAndContents)/2)
	for i := 0; i < len(pathsAndContents); i += 2 {
		files[pathsAndContents[i]] = pathsAndContents[i+1]
	}
	writeRepoFiles(t, repo, files)
}

// wantViolations fails the test unless got holds exactly the want lines, in sorted order.
func wantViolations(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("violations:\n got: %q\nwant: %q", got, want)
	}
}

// visibilityOf runs CheckVisibility, or CheckVisibilityDiff when base is non-empty.
func visibilityOf(t *testing.T, repo, base, tree string) []string {
	t.Helper()
	var (
		got []string
		err error
	)
	if base == "" {
		got, err = capy.CheckVisibility(context.Background(), repo, tree)
	} else {
		got, err = capy.CheckVisibilityDiff(context.Background(), repo, base, tree)
	}
	if err != nil {
		t.Fatalf("visibility check: %v", err)
	}
	return got
}

func TestVisibilityResolvesVanityModulePaths(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"web/hqq/proto/proto.go", "package proto\n",
		"web/hqq/client/client.go", "package client\n\nimport \"gosuda.org/lemonsso/api\"\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
		"corp/lemonsso/api/api.go", "package api\n\nimport \"gosuda.org/hqq/proto\"\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, "", tree),
		"web/hqq/client/client.go:3: prohibited import in public domain: gosuda.org/lemonsso/api -> corp",
	)
}

func TestVisibilityIgnoresCommentsStringsAndUntieredSources(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
		"web/hqq/doc.go", `// Package hqq wraps gosuda.org/lemonsso/api and gosuda.org/0/corp/lemonsso.
package hqq

/*
import "gosuda.org/lemonsso/api"
*/

const mention = "import \"gosuda.org/lemonsso/api\""
`,
		"docs/example.go", "package example\n\nimport \"gosuda.org/lemonsso/api\"\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, "", tree))
}

func TestVisibilityMatrixAcrossTiers(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"docs/go.mod", "module gosuda.org/0/docs\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"web/hqq/hqq.go", "package hqq\n\nimport _ \"gosuda.org/zstd\"\nimport _ \"gosuda.org/hqq/sub\"\nimport _ \"gosuda.org/0/docs\"\n",
		"third_party/zstd/go.mod", "module gosuda.org/zstd\n",
		"third_party/zstd/zstd.go", "package zstd\n\nimport _ \"gosuda.org/zstd/internal\"\nimport _ \"gosuda.org/hqq\"\n",
		"experimental/bob/go.mod", "module gosuda.org/bob\n",
		"experimental/bob/bob.go", "package bob\n\nimport _ \"gosuda.org/lemonsso\"\nimport _ \"gosuda.org/hqq\"\nimport _ \"gosuda.org/0/docs\"\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
		"corp/lemonsso/lemon.go", "package lemonsso\n\nimport _ \"gosuda.org/bob\"\nimport _ \"gosuda.org/hqq\"\nimport _ \"gosuda.org/zstd\"\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, "", tree),
		"corp/lemonsso/lemon.go:3: prohibited import in corp domain: gosuda.org/bob -> experimental",
		"experimental/bob/bob.go:3: prohibited import in experimental domain: gosuda.org/lemonsso -> corp",
		"third_party/zstd/zstd.go:4: prohibited import in third_party domain: gosuda.org/hqq -> public",
		"web/hqq/hqq.go:5: prohibited import in public domain: gosuda.org/0/docs -> untiered",
	)
}

func TestVisibilityChecksGoModReferences(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", `module gosuda.org/hqq

require (
	gosuda.org/lemonsso v0.0.0
	gosuda.org/zstd v1.0.0
)

replace gosuda.org/zstd => ../../third_party/zstd
replace gosuda.org/bob => ../../experimental/bob
replace (
	gosuda.org/other => ../../corp/lemonsso
)
replace gosuda.org/outside => ../../../escape
`,
		"third_party/zstd/go.mod", "module gosuda.org/zstd\n",
		"experimental/bob/go.mod", "module gosuda.org/bob\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, "", tree),
		"web/hqq/go.mod:4: prohibited import in public domain: gosuda.org/lemonsso -> corp",
		"web/hqq/go.mod:9: prohibited import in public domain: gosuda.org/bob => ../../experimental/bob -> experimental",
		"web/hqq/go.mod:11: prohibited import in public domain: gosuda.org/other => ../../corp/lemonsso -> corp",
		"web/hqq/go.mod:13: prohibited import in public domain: gosuda.org/outside => ../../../escape -> outside tree",
	)
}

func TestVisibilityChecksCargoPackageJSONAndScriptImports(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/Cargo.toml", `[package]
name = "hqq"
# lemonsso = { path = "../../corp/lemonsso" }

[dependencies]
lemonsso = { path = "../../corp/lemonsso" }
zstd = { version = "1", path = "../../third_party/zstd" }
escape = { path = "../../../../escape" }
`,
		"web/hqq/package.json", `{
  "name": "hqq-web",
  "dependencies": {
    "lemonsso": "file:../../corp/lemonsso",
    "zstd": "link:../../third_party/zstd"
  },
  "devDependencies": {
    "local": "file:../local"
  }
}
`,
		"web/hqq/src/index.ts", "import { a } from '../../../corp/lemonsso/api';\n"+
			"export * from '../util';\n"+
			"const x = require('../../../corp/lemonsso');\n"+
			"// import { b } from '../../../corp/lemonsso/b';\n"+
			"const s = \"from '../../../corp/lemonsso'\";\n"+
			"const y = import('../../../corp/lemonsso/z');\n",
		"web/hqq/src/tpl.js", "const u = `../../../corp/lemonsso`;\nconst v = require(`../../../corp/lemonsso`);\n",
		"corp/lemonsso/src/a.js", "import b from '../../../experimental/bob/b';\n",
		"web/hqq/tool.py", "import corp.lemonsso  # ../../../corp/lemonsso\n",
		"web/hqq/lib.rs", "#[path = \"../../../corp/lemonsso/x.rs\"]\nmod x;\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, "", tree),
		"corp/lemonsso/src/a.js:1: prohibited import in corp domain: ../../../experimental/bob/b -> experimental",
		"web/hqq/Cargo.toml:6: prohibited import in public domain: path ../../corp/lemonsso -> corp",
		"web/hqq/Cargo.toml:8: prohibited import in public domain: path ../../../../escape -> outside tree",
		"web/hqq/package.json:4: prohibited import in public domain: lemonsso file:../../corp/lemonsso -> corp",
		"web/hqq/src/index.ts:1: prohibited import in public domain: ../../../corp/lemonsso/api -> corp",
		"web/hqq/src/index.ts:3: prohibited import in public domain: ../../../corp/lemonsso -> corp",
		"web/hqq/src/index.ts:6: prohibited import in public domain: ../../../corp/lemonsso/z -> corp",
		"web/hqq/src/tpl.js:2: prohibited import in public domain: ../../../corp/lemonsso -> corp",
	)
}

func TestVisibilityDiffIgnoresViolationsAlreadyInBase(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
		"web/securl/leak.go", "package main\n\nimport _ \"gosuda.org/lemonsso\"\n",
	)
	base := commitTree(t, repo)
	writeTree(t, repo, "web/hqq/ok.go", "package hqq\n")
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, base, tree))
	wantViolations(t, visibilityOf(t, repo, "", tree),
		"web/securl/leak.go:3: prohibited import in public domain: gosuda.org/lemonsso -> corp",
	)
}

func TestVisibilityDiffFlagsModuleRenameThatRepointsImporter(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"web/hqq/api/api.go", "package api\n",
		"web/app/go.mod", "module gosuda.org/app\n",
		"web/app/main.go", "package main\n\nimport _ \"gosuda.org/hqq/api\"\n",
	)
	base := commitTree(t, repo)

	// The public module is renamed away, and a corp module takes over its path. web/app/main.go is unchanged.
	writeTree(t, repo,
		"web/hqq/go.mod", "module gosuda.org/hqq-v1\n",
		"corp/hqqcorp/go.mod", "module gosuda.org/hqq\n",
		"corp/hqqcorp/api/api.go", "package api\n",
	)
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, base, tree),
		"web/app/main.go:3: prohibited import in public domain: gosuda.org/hqq/api -> corp",
	)
}

func TestVisibilityDiffRechecksGoFilesOwnedByChangedGoMod(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/go.mod", "module gosuda.org/hqq\n",
		"web/hqq/leak.go", "package hqq\n\nimport _ \"gosuda.org/lemonsso\"\n",
		"corp/lemonsso/go.mod", "module gosuda.org/lemonsso\n",
	)
	base := commitTree(t, repo)
	writeTree(t, repo, "web/hqq/go.mod", "module gosuda.org/hqq\n\ngo 1.27\n")
	tree := commitTree(t, repo)

	wantViolations(t, visibilityOf(t, repo, base, tree),
		"web/hqq/leak.go:3: prohibited import in public domain: gosuda.org/lemonsso -> corp",
	)
}

func TestVisibilityReportsUnparsableGoFile(t *testing.T) {
	repo := initTestRepo(t)
	writeTree(t, repo,
		"go.mod", "module gosuda.org/0\n",
		"web/hqq/broken.go", "package hqq\n\nimport \"fmt\n",
	)
	tree := commitTree(t, repo)

	got := visibilityOf(t, repo, "", tree)
	if len(got) != 1 || !strings.HasPrefix(got[0], "web/hqq/broken.go: unparsable Go file: ") {
		t.Fatalf("violations = %q, want one unparsable-file line for web/hqq/broken.go", got)
	}
}
