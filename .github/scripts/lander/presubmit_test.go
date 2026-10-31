package lander

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// runFixture is the tree the sandbox tests run against: the HQQ chain plus an unaffected leaf.
var runFixture = map[string]string{
	"ipc/hqq/HQQ/go.mod":         "module gosuda.org/hqq\n\ngo 1.21\n",
	"ipc/hqq/HQQ/hqq.go":         "package hqq\n",
	"ipc/hqq/hqq-sqlite3/go.mod": "module gosuda.org/hqq/hqq-sqlite3\n\ngo 1.21\n\nrequire gosuda.org/hqq v0.0.0\n\nreplace gosuda.org/hqq => ../HQQ\n",
	"ipc/hqq/hqq-sqlite3/db.go":  "package sqlite3\n",
	"ipc/hqq/hqq-zstd/go.mod":    "module gosuda.org/hqq/hqq-zstd\n\ngo 1.21\n\nrequire gosuda.org/hqq v0.0.0\n\nreplace gosuda.org/hqq => ../HQQ\n",
	"ipc/hqq/hqq-zstd/z.go":      "package zstd\n",
	"net/grt/go.mod":             "module gosuda.org/0/net/grt\n\ngo 1.21\n",
	"net/grt/grt.go":             "package grt\n",
}

// fakeDocker writes a docker stand-in that appends each invocation's arguments to a log file. An
// invocation whose arguments contain failPat prints output and exits 1; one containing hangPat sleeps
// past any timeout. It returns the executable and the log path.
func fakeDocker(t *testing.T, failPat, hangPat string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	var cases strings.Builder
	if hangPat != "" {
		fmt.Fprintf(&cases, "  *%s*) exec sleep 30 ;;\n", hangPat)
	}
	if failPat != "" {
		fmt.Fprintf(&cases, "  *%s*) echo \"boom in %s\"; exit 1 ;;\n", failPat, failPat)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\ncase \"$*\" in\n%s  *) ;;\nesac\nexit 0\n",
		shellQuote(logPath), cases.String())
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// readCalls returns one line per logged docker invocation, or none when docker was never run.
func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// flagValues returns the value following every occurrence of flag in args.
func flagValues(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestPresubmitNoAffectedProjectsPasses(t *testing.T) {
	s, tree := fixtureStore(t, runFixture, nil)
	p := &DockerPresubmit{Image: "lander/sandbox:test", CacheDir: t.TempDir()}
	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"docs/readme.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Summary != "no affected projects" {
		t.Fatalf("result = %+v", res)
	}
}

func TestPresubmitRequiresImageAndCache(t *testing.T) {
	s, tree := fixtureStore(t, runFixture, nil)
	in := PresubmitInput{Store: s, NewTree: tree, Changed: []string{"net/grt/grt.go"}}
	if _, err := (&DockerPresubmit{CacheDir: t.TempDir()}).Run(context.Background(), in); err == nil {
		t.Fatal("missing Image accepted")
	}
	if _, err := (&DockerPresubmit{Image: "img"}).Run(context.Background(), in); err == nil {
		t.Fatal("missing CacheDir accepted")
	}
}

func TestPresubmitRunsEveryAffectedProjectInSandbox(t *testing.T) {
	t.Setenv("LANDER_TEST_HOST_SECRET", "hunter2")
	s, tree := fixtureStore(t, runFixture, nil)
	bin, logPath := fakeDocker(t, "", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"ipc/hqq/HQQ/hqq.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Summary != "3 projects passed" {
		t.Fatalf("result = %+v\n%s", res, res.Details)
	}
	calls := readCalls(t, logPath)
	if len(calls) != 3 {
		t.Fatalf("docker ran %d times, want 3:\n%s", len(calls), strings.Join(calls, "\n"))
	}
	for _, dir := range []string{"ipc/hqq/HQQ", "ipc/hqq/hqq-sqlite3", "ipc/hqq/hqq-zstd"} {
		if !slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "'/work/"+dir+"'") }) {
			t.Fatalf("no sandbox run for %s", dir)
		}
	}
	for _, c := range calls {
		for _, want := range []string{"run --rm", "--network none", "--read-only", "--cap-drop ALL", "--entrypoint sh"} {
			if !strings.Contains(c, want) {
				t.Fatalf("invocation lacks %q: %s", want, c)
			}
		}
		if n := strings.Count(c, " -v "); n != 2 {
			t.Fatalf("invocation has %d -v mounts, want 2: %s", n, c)
		}
		if strings.Contains(c, "hunter2") || strings.Contains(c, "LANDER_TEST_HOST_SECRET") {
			t.Fatalf("host environment leaked into invocation: %s", c)
		}
	}
}

func TestPresubmitFailureNamesProjectAndKeepsOutputTail(t *testing.T) {
	s, tree := fixtureStore(t, runFixture, nil)
	bin, _ := fakeDocker(t, "hqq-zstd", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"ipc/hqq/HQQ/hqq.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Summary != "FAILED: ipc/hqq/hqq-zstd" {
		t.Fatalf("result = %+v", res)
	}
	for _, want := range []string{"== ipc/hqq/hqq-zstd [go]: exit 1", "boom in hqq-zstd"} {
		if !strings.Contains(res.Details, want) {
			t.Fatalf("details lack %q:\n%s", want, res.Details)
		}
	}
}

func TestPresubmitTimeoutKillsNamedContainer(t *testing.T) {
	s, tree := fixtureStore(t, runFixture, nil)
	bin, logPath := fakeDocker(t, "", "hqq-sqlite3")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir(), Timeout: 2 * time.Second}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"ipc/hqq/hqq-sqlite3/db.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Summary != "FAILED: ipc/hqq/hqq-sqlite3" {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Details, "timed out after 2s") {
		t.Fatalf("details lack timeout:\n%s", res.Details)
	}
	if calls := readCalls(t, logPath); !slices.ContainsFunc(calls, func(c string) bool {
		return strings.HasPrefix(c, "rm -f lander-presubmit-")
	}) {
		t.Fatalf("timed-out container was not removed by name; calls:\n%s", strings.Join(calls, "\n"))
	}
}

func TestPresubmitNoTestsDeclaredPasses(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"web/package.json": `{"name":"web"}`,
		"web/index.ts":     "export {}\n",
	}, nil)
	bin, logPath := fakeDocker(t, "", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"web/index.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Summary != "no tests declared" {
		t.Fatalf("result = %+v", res)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("docker ran for a project without tests: %v", calls)
	}
}

func TestPresubmitArchiveIsVerifiedAgainstTree(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"pkg/go.mod":         "module example.com/pkg\n\ngo 1.21\n",
		"pkg/a.go":           "package pkg\n",
		"pkg/.gitattributes": "secret.txt export-ignore\n",
		"pkg/secret.txt":     "hidden from presubmit\n",
	}, nil)
	bin, logPath := fakeDocker(t, "", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"pkg/a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Summary != "FAILED: archive" || !strings.Contains(res.Details, "pkg/secret.txt") {
		t.Fatalf("result = %+v", res)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("docker ran on an incomplete archive: %v", calls)
	}
}

func TestPresubmitSummaryIsBounded(t *testing.T) {
	r := &presubmitReport{}
	for i := range 40 {
		r.fail(Project{Dir: fmt.Sprintf("projects/very-long-directory-name-%02d", i), Kind: kindGo}, "exit 1", nil)
	}
	res := r.result()
	if res.Passed || utf8.RuneCountInString(res.Summary) > summaryRunes {
		t.Fatalf("summary not bounded: passed=%v len=%d", res.Passed, utf8.RuneCountInString(res.Summary))
	}
}

func TestPresubmitKeepsOnlyOutputTail(t *testing.T) {
	out := &tailBuffer{max: logTailBytes}
	big := strings.Repeat("a", 10000) + "END"
	if _, err := io.WriteString(out, big); err != nil {
		t.Fatal(err)
	}
	if got := out.Bytes(); len(got) != logTailBytes || !strings.HasSuffix(string(got), "END") {
		t.Fatalf("tail has %d bytes, want the last %d ending in END", len(got), logTailBytes)
	}
}

func TestPresubmitDefaultTestCommands(t *testing.T) {
	var cfg DockerPresubmit
	cases := []struct {
		name string
		n    *projectNode
		want []string
	}{
		{"go", &projectNode{Project: Project{Kind: kindGo}}, []string{"go vet ./...", "go test ./..."}},
		{"cargo locked", &projectNode{Project: Project{Kind: kindCargo}, lock: true}, []string{"cargo test --offline --locked"}},
		{"cargo unlocked", &projectNode{Project: Project{Kind: kindCargo}}, nil},
		{"node tests", &projectNode{Project: Project{Kind: kindNode}, tests: true}, []string{"bun test"}},
		{"node none", &projectNode{Project: Project{Kind: kindNode}}, nil},
	}
	for _, c := range cases {
		if got := cfg.testCommands(c.n); !slices.Equal(got, c.want) {
			t.Fatalf("%s: commands = %q, want %q", c.name, got, c.want)
		}
	}
	override := DockerPresubmit{TestCommands: func(Project) []string { return []string{"make check"} }}
	if got := override.testCommands(&projectNode{Project: Project{Kind: kindGo}}); !slices.Equal(got, []string{"make check"}) {
		t.Fatalf("override ignored: %q", got)
	}
}

func TestPresubmitPrepareFailureFailsProject(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"bad/go.mod": "module example.com/bad\n\ngo 1.21\n\nrequire example.com/nothere v0.0.0\n\nreplace example.com/nothere => ../missing\n",
		"bad/bad.go": "package bad\n",
	}, nil)
	bin, logPath := fakeDocker(t, "", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"bad/bad.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Summary != "FAILED: bad" || !strings.Contains(res.Details, "prepare go mod download in bad") {
		t.Fatalf("result = %+v\n%s", res, res.Details)
	}
	if calls := readCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("tests ran for a project whose prepare failed: %v", calls)
	}
}

func TestPresubmitSkipsMissingPrepareToolAndRecordsIt(t *testing.T) {
	s, tree := fixtureStore(t, runFixture, nil)
	bin, _ := fakeDocker(t, "", "")
	// The lander's PATH holds git but no go, so only the go prepare step is skipped.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(pathDir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"net/grt/grt.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || !strings.Contains(res.Details, "skipped go mod download in net/grt: go not found") {
		t.Fatalf("result = %+v\n%s", res, res.Details)
	}
}

func TestPresubmitPrepareSkipsRepoCargoConfig(t *testing.T) {
	ctx := context.Background()
	cfg := DockerPresubmit{CacheDir: "/cache"}

	s, tree := fixtureStore(t, map[string]string{
		"crates/a/Cargo.toml": "[package]\nname = \"a\"\n",
		"crates/a/Cargo.lock": "# lock\n",
		".cargo/config.toml":  "[registries.evil]\ncredential-provider = [\"./evil\"]\n",
	}, nil)
	g, err := loadProjects(ctx, s, tree)
	if err != nil {
		t.Fatal(err)
	}
	spec, skip := cfg.prepareSpecFor(g, g.nodes["cargo:crates/a"])
	if spec != nil || !strings.Contains(skip, "repository cargo config present") {
		t.Fatalf("cargo fetch ran under a repository cargo config: spec=%v skip=%q", spec, skip)
	}

	s2, tree2 := fixtureStore(t, map[string]string{
		"crates/a/Cargo.toml": "[package]\nname = \"a\"\n",
		"crates/a/Cargo.lock": "# lock\n",
	}, nil)
	g2, err := loadProjects(ctx, s2, tree2)
	if err != nil {
		t.Fatal(err)
	}
	spec, skip = cfg.prepareSpecFor(g2, g2.nodes["cargo:crates/a"])
	if skip != "" || spec == nil || !slices.Equal(spec.argv, []string{"cargo", "fetch", "--locked"}) ||
		!slices.Contains(spec.env, "CARGO_HOME=/cache/cargo") {
		t.Fatalf("cargo fetch not prepared: spec=%+v skip=%q", spec, skip)
	}
}

func TestSandboxArgsIsolation(t *testing.T) {
	t.Setenv("LANDER_TEST_HOST_SECRET", "hunter2")
	cfg, err := (&DockerPresubmit{Image: "lander/sandbox:test", CacheDir: t.TempDir()}).config()
	if err != nil {
		t.Fatal(err)
	}
	args := sandboxArgs(cfg, "lander-presubmit-test", "/srv/src", "/srv/cache", Project{Dir: "ipc/x", Kind: kindGo}, "true")

	for _, pair := range [][2]string{
		{"--network", "none"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"},
		{"--user", "65534:65534"}, {"--name", "lander-presubmit-test"},
	} {
		if !slices.Contains(flagValues(args, pair[0]), pair[1]) {
			t.Fatalf("argv lacks %s %s: %q", pair[0], pair[1], args)
		}
	}
	if !slices.Contains(args, "--read-only") {
		t.Fatalf("argv lacks --read-only: %q", args)
	}

	mounts := append(append(flagValues(args, "-v"), flagValues(args, "--mount")...), flagValues(args, "--volume")...)
	if want := []string{"/srv/src:/src:ro", "/srv/cache:/cache:ro"}; !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %q, want %q", mounts, want)
	}

	if envs := flagValues(args, "-e"); !slices.Equal(envs, sandboxEnv) {
		t.Fatalf("container env = %q, want the fixed sandbox env %q", envs, sandboxEnv)
	}
	for _, forbidden := range []string{"--privileged", "--cap-add", "--env-file", "--volumes-from", "--device", "--pid", "--userns", "--ipc", "--add-host"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("argv contains forbidden %s", forbidden)
		}
	}
	if joined := strings.Join(args, " "); strings.Contains(joined, "hunter2") || strings.Contains(joined, "LANDER_TEST_HOST_SECRET") {
		t.Fatalf("host environment reached argv: %s", joined)
	}

	if n := len(args); n < 5 || args[n-5] != "--entrypoint" || args[n-4] != "sh" ||
		args[n-3] != "lander/sandbox:test" || args[n-2] != "-c" || args[n-1] != "true" {
		t.Fatalf("argv does not end with the entrypoint, image, and script: %q", args)
	}
}

func TestSandboxScriptQuotesProjectDir(t *testing.T) {
	script := containerScript(Project{Dir: "a b/it's", Kind: kindGo}, []string{"go test ./..."})
	if want := "cd '/work/a b/it'\\''s'"; !strings.Contains(script, want) {
		t.Fatalf("script %q lacks %q", script, want)
	}
	if !strings.HasPrefix(script, "cp -R /src/. /work && ") {
		t.Fatalf("script does not copy the sources first: %q", script)
	}
}

// TestSandboxContainerHasNoNetworkAndReadOnlyRoot runs a real container through sandboxArgs. It skips
// unless the docker daemon is reachable and the image (LANDER_SANDBOX_TEST_IMAGE, default
// busybox:latest) is already present.
func TestSandboxContainerHasNoNetworkAndReadOnlyRoot(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, docker, "info").Run(); err != nil {
		t.Skipf("docker daemon unavailable: %v", err)
	}
	image := os.Getenv("LANDER_SANDBOX_TEST_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}
	if err := exec.CommandContext(ctx, docker, "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not present locally (set LANDER_SANDBOX_TEST_IMAGE): %v", image, err)
	}
	cfg, err := (&DockerPresubmit{Image: image, Docker: docker, CacheDir: t.TempDir()}).config()
	if err != nil {
		t.Fatal(err)
	}
	// Exit 3: a non-loopback interface exists. Exit 4: the root filesystem is writable. Exit 5: no
	// network device listing, so the first check is meaningless. Exit 6: /tmp is not writable.
	script := `[ -d /sys/class/net/lo ] || exit 5
for i in /sys/class/net/*; do [ "${i##*/}" = lo ] || exit 3; done
touch /lander-probe 2>/dev/null && exit 4
touch /tmp/ok || exit 6
exit 0`
	name, err := sandboxName()
	if err != nil {
		t.Fatal(err)
	}
	args := sandboxArgs(cfg, name, t.TempDir(), t.TempDir(), Project{Dir: ".", Kind: kindGo}, script)
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = inheritedEnv(dockerEnvNames)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox probe failed: %v\n%s", err, out)
	}
}

func TestTarExportCoversClosureOnly(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.work":                 "go 1.21\n",
		"ipc/hqq/HQQ/go.mod":      "module gosuda.org/hqq\n\ngo 1.21\n",
		"ipc/hqq/HQQ/hqq.go":      "package hqq\n",
		"ipc/hqq/hqq-zstd/go.mod": "module gosuda.org/hqq/hqq-zstd\n\ngo 1.21\n",
		"ipc/hqq/hqq-zstd/z.go":   "package zstd\n",
	}, map[string]string{"ipc/hqq/HQQ/hqq-link.go": "hqq.go"})
	ctx := context.Background()
	g, err := loadProjects(ctx, s, tree)
	if err != nil {
		t.Fatal(err)
	}
	closure := g.closure([]*projectNode{g.nodes["go:ipc/hqq/HQQ"]})

	dst := t.TempDir()
	if err := exportProjects(ctx, s, tree, g, closure, dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"go.work", "ipc/hqq/HQQ/hqq.go", "ipc/hqq/HQQ/go.mod"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Fatalf("export lacks %s: %v", p, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(dst, "ipc/hqq/HQQ/hqq-link.go")); err != nil || target != "hqq.go" {
		t.Fatalf("symlink not exported as a symlink: %q %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "ipc/hqq/hqq-zstd/z.go")); !os.IsNotExist(err) {
		t.Fatalf("export includes an unaffected, non-dependency project: %v", err)
	}
}

func TestTarExtractRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/abs/evil", "a/../b"} {
		root := t.TempDir()
		dst := filepath.Join(root, "dst")
		if err := os.Mkdir(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := extractTar(tarOf(t, tarItem{name: name, typ: tar.TypeReg, body: "x"}), dst)
		if err == nil {
			t.Fatalf("entry %q was accepted", name)
		}
		if _, err := os.Lstat(filepath.Join(root, "evil")); err == nil {
			t.Fatalf("entry %q wrote outside the root", name)
		}
	}
}

func TestTarExtractRejectsEscapingSymlinks(t *testing.T) {
	cases := []tarItem{
		{name: "ln", typ: tar.TypeSymlink, link: "../../etc/passwd"},
		{name: "ln", typ: tar.TypeSymlink, link: "/etc/passwd"},
		{name: "d/ln", typ: tar.TypeSymlink, link: "../../x"},
		{name: "ln", typ: tar.TypeSymlink, link: ""},
		{name: "hard", typ: tar.TypeLink, link: "go.mod"},
	}
	for _, c := range cases {
		dst := t.TempDir()
		if _, err := extractTar(tarOf(t, c), dst); err == nil {
			t.Fatalf("entry %+v was accepted", c)
		}
	}
}

func TestTarExtractRefusesSymlinkParents(t *testing.T) {
	dst := t.TempDir()
	_, err := extractTar(tarOf(t,
		tarItem{name: "d", typ: tar.TypeSymlink, link: "sub"},
		tarItem{name: "d/x", typ: tar.TypeReg, body: "x"},
	), dst)
	if err == nil {
		t.Fatal("file written through a symlinked parent")
	}
	if _, err := os.Lstat(filepath.Join(dst, "sub", "x")); err == nil {
		t.Fatal("file landed at the symlink target")
	}
}

func TestTarExtractAcceptsContainedEntries(t *testing.T) {
	dst := t.TempDir()
	got, err := extractTar(tarOf(t,
		tarItem{name: "sub/", typ: tar.TypeDir},
		tarItem{name: "sub/f", typ: tar.TypeReg, body: "x", mode: 0o755},
		tarItem{name: "sub/deep/g", typ: tar.TypeReg, body: "y"},
		tarItem{name: "ok", typ: tar.TypeSymlink, link: "sub/f"},
	), dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ok", "sub/deep/g", "sub/f"}; !slices.Equal(sortedKeys(got), want) {
		t.Fatalf("extracted = %q, want %q", sortedKeys(got), want)
	}
	fi, err := os.Stat(filepath.Join(dst, "sub", "f"))
	if err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("executable bit lost: %v %v", fi, err)
	}
}

// tarItem describes one entry of a synthetic archive.
type tarItem struct {
	name, link, body string
	typ              byte
	mode             int64
}

// tarOf serialises items as a tar stream.
func tarOf(t *testing.T, items ...tarItem) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, it := range items {
		hdr := &tar.Header{Name: it.name, Typeflag: it.typ, Linkname: it.link, Mode: it.mode}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if it.typ == tar.TypeReg {
			hdr.Size = int64(len(it.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if it.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(it.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
