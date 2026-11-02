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
	"go.work":                    "go 1.21\n\nuse (\n\t./ipc/hqq/HQQ\n\t./ipc/hqq/hqq-sqlite3\n\t./ipc/hqq/hqq-zstd\n\t./net/grt\n)\n",
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
		{"go in workspace", &projectNode{Project: Project{Kind: kindGo}, workspace: true}, []string{"go vet ./...", "go test ./..."}},
		{"go outside workspace", &projectNode{Project: Project{Kind: kindGo}}, []string{"GOWORK=off go vet ./...", "GOWORK=off go test ./..."}},
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
		"bad/go.mod": "module example.com/bad\n\ngo 1.21\n\nrequire example.com/nothere\n",
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
	if !res.Passed || !strings.Contains(res.Details, "skipped go module warm-up: go not found") {
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

// TestSandboxWorkspaceBuildsOffline runs the generated workspace in a real container under the sandbox's own
// environment: the sibling resolves to its local source and the proxy stays off. It skips unless the docker
// daemon is reachable and the sandbox image (LANDER_PRESUBMIT_IMAGE, default golang:1.27) is present locally.
func TestSandboxWorkspaceBuildsOffline(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := exec.CommandContext(ctx, docker, "info").Run(); err != nil {
		t.Skipf("docker daemon unavailable: %v", err)
	}
	image := os.Getenv("LANDER_PRESUBMIT_IMAGE")
	if image == "" {
		image = "golang:1.27"
	}
	if err := exec.CommandContext(ctx, docker, "image", "inspect", image).Run(); err != nil {
		t.Skipf("image %s is not present locally (set LANDER_PRESUBMIT_IMAGE): %v", image, err)
	}
	dst := siblingStage(t)
	cfg, err := (&DockerPresubmit{Image: image, Docker: docker, CacheDir: t.TempDir()}).config()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.CacheDir, "gomod"), 0o755); err != nil {
		t.Fatal(err)
	}
	name, err := sandboxName()
	if err != nil {
		t.Fatal(err)
	}
	p := Project{Dir: "app", Kind: kindGo}
	args := sandboxArgs(cfg, name, dst, cfg.CacheDir, p, containerScript(p, []string{"go vet ./...", "go test ./..."}))
	cmd := exec.CommandContext(ctx, docker, args...)
	cmd.Env = inheritedEnv(dockerEnvNames)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox workspace run failed: %v\n%s", err, out)
	}
}

func TestTarExportCoversClosureOnly(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.work":                 "go 1.21\n\nuse (\n\t./ipc/hqq/HQQ\n\t./ipc/hqq/hqq-zstd\n)\n",
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
	for _, p := range []string{"ipc/hqq/HQQ/hqq.go", "ipc/hqq/HQQ/go.mod"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Fatalf("export lacks %s: %v", p, err)
		}
	}
	// The trunk's go.work is never exported; the generated one names the closure's workspace modules only.
	if got, err := os.ReadFile(filepath.Join(dst, "go.work")); err != nil || string(got) != "go 1.21\n\nuse (\n\t./ipc/hqq/HQQ\n)\n" {
		t.Fatalf("go.work = %q, %v", got, err)
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

// stagedClosure exports the closure of the changed paths from tree into a fresh directory, as Run does,
// and returns it with symlinks resolved: go compares its working directory with GOWORK textually.
func stagedClosure(t *testing.T, s *Store, tree string, changed ...string) string {
	t.Helper()
	ctx := context.Background()
	g, err := loadProjects(ctx, s, tree)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := exportProjects(ctx, s, tree, g, g.closure(g.affected(changed)), dst); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(dst)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// readGoWork returns the go.work written at the root of a staged directory.
func readGoWork(t *testing.T, dst string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dst, goWorkName))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goCommand runs the go tool in dir with the proxy off and fresh caches. gowork is the GOWORK value: a
// go.work path, or "off".
func goCommand(t *testing.T, dir, gowork string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GOWORK="+gowork,
		"GOPROXY=off",
		"GOFLAGS=-mod=readonly",
		"GOTOOLCHAIN=local",
		"GOENV=off",
		"GOMODCACHE="+filepath.Join(t.TempDir(), "mod"),
		"GOCACHE="+t.TempDir(),
	)
	return cmd.CombinedOutput()
}

// workspaceFixture has a trunk workspace of two modules and a Go module that the trunk's go.work does not
// list. That module is tested on its own, with GOWORK=off.
var workspaceFixture = map[string]string{
	"go.work":               "go 1.22\n\nuse (\n\t./app\n\t./lib\n)\n",
	"app/go.mod":            "module example.org/app\n\ngo 1.21\n\nrequire example.org/lib v0.0.0\n",
	"app/app.go":            "package app\n",
	"lib/go.mod":            "module example.org/lib\n\ngo 1.21\n",
	"lib/lib.go":            "package lib\n",
	"devtools/potly/go.mod": "module example.org/potly\n\ngo 1.20\n",
	"devtools/potly/p.go":   "package potly\n",
}

func TestGoWorkListsClosureModulesSorted(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.work":          "go 1.22\n\nuse (\n\t./lib\n\t./app/inner\n\t./app\n)\n",
		"app/go.mod":       "module example.org/app\n\ngo 1.22\n\nrequire (\n\texample.org/lib v0.0.0\n\texample.org/app/inner v0.0.0\n)\n",
		"app/main.go":      "package main\n",
		"app/inner/go.mod": "module example.org/app/inner\n\ngo 1.23\n",
		"app/inner/in.go":  "package inner\n",
		"lib/go.mod":       "module example.org/lib\n\ngo 1.21\n",
		"lib/lib.go":       "package lib\n",
		"unused/go.mod":    "module example.org/unused\n\ngo 1.21\n",
		"web/package.json": `{"name":"web","scripts":{"test":"bun test"}}`,
		"web/index.ts":     "export {}\n",
	}, nil)
	dst := stagedClosure(t, s, tree, "lib/lib.go", "web/index.ts")
	want := "go 1.23\n\nuse (\n\t./app\n\t./app/inner\n\t./lib\n)\n"
	if got := readGoWork(t, dst); got != want {
		t.Fatalf("go.work = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dst, "unused")); !os.IsNotExist(err) {
		t.Fatalf("unaffected project exported: %v", err)
	}
	if got := readGoWork(t, stagedClosure(t, s, tree, "web/index.ts", "lib/lib.go")); got != want {
		t.Fatalf("go.work differs between runs: %q", got)
	}
}

func TestGoWorkWholeTreeReplacesTrunkFile(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.work":      "go 1.25\n\nuse (\n\t.\n\t./other\n)\n",
		"go.mod":       "module example.org/root\n\ngo 1.22\n",
		"root.go":      "package root\n",
		"other/go.mod": "module example.org/other\n\ngo 1.21\n",
		"other/o.go":   "package other\n",
	}, nil)
	dst := stagedClosure(t, s, tree, "root.go")
	if want := "go 1.22\n\nuse (\n\t.\n)\n"; readGoWork(t, dst) != want {
		t.Fatalf("go.work = %q, want %q", readGoWork(t, dst), want)
	}
	if _, err := os.Stat(filepath.Join(dst, "go.mod")); err != nil {
		t.Fatalf("root go.mod not exported: %v", err)
	}
}

func TestGoWorkVersionFallback(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{
			name: "trunk go.work",
			files: map[string]string{
				"go.work":    "go 1.24\n\nuse (\n\t./lib\n)\n",
				"lib/go.mod": "module example.org/lib\n",
				"lib/lib.go": "package lib\n",
			},
			want: "go 1.24\n\nuse (\n\t./lib\n)\n",
		},
		{
			name:  "default",
			files: map[string]string{"lib/go.mod": "module example.org/lib\n", "lib/lib.go": "package lib\n"},
			want:  "go 1.21\n\nuse (\n)\n",
		},
	}
	for _, c := range cases {
		s, tree := fixtureStore(t, c.files, nil)
		if got := readGoWork(t, stagedClosure(t, s, tree, "lib/lib.go")); got != c.want {
			t.Fatalf("%s: go.work = %q, want %q", c.name, got, c.want)
		}
	}

	s, tree := fixtureStore(t, map[string]string{
		"lib/go.mod": "module example.org/lib\n\ngo banana\n",
		"lib/lib.go": "package lib\n",
	}, nil)
	ctx := context.Background()
	g, err := loadProjects(ctx, s, tree)
	if err != nil {
		t.Fatal(err)
	}
	err = exportProjects(ctx, s, tree, g, g.closure(g.affected([]string{"lib/lib.go"})), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "invalid go version") {
		t.Fatalf("invalid go directive accepted: %v", err)
	}
}

func TestGoWorkRenderSortsAndQuotes(t *testing.T) {
	got := renderGoWork("1.22", []string{"a b", ".", "a"}, []string{"b => ./x", "a v1 => ./y"})
	want := "go 1.22\n\nuse (\n\t.\n\t./a\n\t\"./a b\"\n)\n\nreplace (\n\ta v1 => ./y\n\tb => ./x\n)\n"
	if got != want {
		t.Fatalf("renderGoWork = %q, want %q", got, want)
	}
	if got := renderGoWork("1.21", nil, nil); got != "go 1.21\n\nuse (\n)\n" {
		t.Fatalf("empty workspace = %q", got)
	}
}

// TestGoWorkCarriesTrunkReplacesIntoClosure checks the trunk's replace directives: those whose local target
// lies in the closure are carried over in both forms, the others are dropped. The layout mirrors a nested
// module that a sibling requires at an unpublished version. With Go installed, the generated workspace must
// also build offline.
func TestGoWorkCarriesTrunkReplacesIntoClosure(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{
		"go.work": "go 1.25\n\nuse (\n\t./app\n\t./crypto/x402-facilitator\n\t./crypto/x402-facilitator/scheme/evm\n\t./other\n)\n\n" +
			"replace github.com/gosuda/x402-facilitator v0.0.5 => ./crypto/x402-facilitator\n" +
			"replace (\n" +
			"\tgithub.com/gosuda/x402-facilitator/scheme/evm v0.0.1 => ./crypto/x402-facilitator/scheme/evm // local\n" +
			"\texample.com/outside v1.0.0 => ./other\n" +
			"\texample.com/fork v1.2.3 => example.com/forked v1.2.4\n" +
			")\n",
		"app/go.mod":                                "module example.org/app\n\ngo 1.21\n\nrequire github.com/gosuda/x402-facilitator v0.0.5\n",
		"app/main.go":                               "package main\n\nimport \"github.com/gosuda/x402-facilitator\"\n\nfunc main() { println(x402.Name()) }\n",
		"crypto/x402-facilitator/go.mod":            "module github.com/gosuda/x402-facilitator\n\ngo 1.22\n\nrequire github.com/gosuda/x402-facilitator/scheme/evm v0.0.1\n",
		"crypto/x402-facilitator/x402.go":           "package x402\n\nimport \"github.com/gosuda/x402-facilitator/scheme/evm\"\n\nfunc Name() string { return evm.Name() }\n",
		"crypto/x402-facilitator/scheme/evm/go.mod": "module github.com/gosuda/x402-facilitator/scheme/evm\n\ngo 1.23\n",
		"crypto/x402-facilitator/scheme/evm/evm.go": "package evm\n\nfunc Name() string { return \"evm\" }\n",
		"other/go.mod":                              "module example.org/other\n\ngo 1.21\n",
	}, nil)
	dst := stagedClosure(t, s, tree, "crypto/x402-facilitator/scheme/evm/evm.go")
	want := "go 1.23\n\nuse (\n\t./app\n\t./crypto/x402-facilitator\n\t./crypto/x402-facilitator/scheme/evm\n)\n\n" +
		"replace (\n" +
		"\tgithub.com/gosuda/x402-facilitator v0.0.5 => ./crypto/x402-facilitator\n" +
		"\tgithub.com/gosuda/x402-facilitator/scheme/evm v0.0.1 => ./crypto/x402-facilitator/scheme/evm\n" +
		")\n"
	if got := readGoWork(t, dst); got != want {
		t.Fatalf("go.work = %q, want %q", got, want)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return
	}
	if out, err := goCommand(t, filepath.Join(dst, "app"), filepath.Join(dst, goWorkName), "build", "./..."); err != nil {
		t.Fatalf("workspace with carried replaces does not build: %v\n%s", err, out)
	}
}

// TestGoWorkModulesOutsideTrunkUseRunWithoutWorkspace checks membership: modules listed in the trunk's go.work
// are in the generated workspace and get the workspace; a closure module it does not list gets GOWORK=off
// commands and a standalone prepare step.
func TestGoWorkModulesOutsideTrunkUseRunWithoutWorkspace(t *testing.T) {
	s, tree := fixtureStore(t, workspaceFixture, nil)
	ctx := context.Background()
	g, err := loadProjects(ctx, s, tree)
	if err != nil {
		t.Fatal(err)
	}
	closure := g.closure(g.affected([]string{"lib/lib.go", "devtools/potly/p.go"}))
	if got, err := goWorkFile(g, closure); err != nil || got != "go 1.21\n\nuse (\n\t./app\n\t./lib\n)\n" {
		t.Fatalf("goWorkFile = %q, %v", got, err)
	}
	for dir, inWorkspace := range map[string]bool{"app": true, "lib": true, "devtools/potly": false} {
		if n := g.nodes[kindGo+":"+dir]; n == nil || n.workspace != inWorkspace {
			t.Fatalf("%s: workspace = %v, want %v", dir, n != nil && n.workspace, inWorkspace)
		}
	}
	cfg := DockerPresubmit{}
	if got := cfg.testCommands(g.nodes["go:devtools/potly"]); !slices.Equal(got, []string{"GOWORK=off go vet ./...", "GOWORK=off go test ./..."}) {
		t.Fatalf("potly commands = %q", got)
	}
	if got := cfg.testCommands(g.nodes["go:lib"]); !slices.Equal(got, []string{"go vet ./...", "go test ./..."}) {
		t.Fatalf("lib commands = %q", got)
	}
	spec, skip := cfg.prepareSpecFor(g, g.nodes["go:devtools/potly"])
	if spec == nil || skip != "" || !slices.Equal(spec.argv, []string{"go", "mod", "download"}) || !slices.Contains(spec.env, "GOWORK=off") {
		t.Fatalf("potly prepare = %+v %q", spec, skip)
	}
	if spec, _ := cfg.prepareSpecFor(g, g.nodes["go:lib"]); spec != nil {
		t.Fatalf("workspace module prepared per module: %+v", spec)
	}
}

// TestPresubmitOutsideWorkspaceCommandsDisableWorkspace runs the presubmit over the workspace fixture and
// checks the sandbox invocations: workspace modules run plain commands, the module outside the trunk's
// go.work runs with GOWORK=off.
func TestPresubmitOutsideWorkspaceCommandsDisableWorkspace(t *testing.T) {
	s, tree := fixtureStore(t, workspaceFixture, nil)
	bin, logPath := fakeDocker(t, "", "")
	p := &DockerPresubmit{Image: "lander/sandbox:test", Docker: bin, CacheDir: t.TempDir()}

	res, err := p.Run(context.Background(), PresubmitInput{Store: s, NewTree: tree, Changed: []string{"lib/lib.go", "devtools/potly/p.go"}})
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
	for _, c := range calls {
		switch {
		case strings.Contains(c, "'/work/devtools/potly'"):
			if !strings.Contains(c, "GOWORK=off go test ./...") {
				t.Fatalf("module outside the workspace runs with the workspace: %s", c)
			}
		case strings.Contains(c, "'/work/app'"), strings.Contains(c, "'/work/lib'"):
			if strings.Contains(c, "GOWORK=off") || !strings.Contains(c, "cd '/work/") || !strings.Contains(c, " go test ./...") {
				t.Fatalf("workspace module lacks the workspace: %s", c)
			}
		default:
			t.Fatalf("unexpected invocation: %s", c)
		}
	}
}

// TestGoWorkFetchListSkipsWorkspaceAndLocal checks which build-list entries the prepare step downloads.
func TestGoWorkFetchListSkipsWorkspaceAndLocal(t *testing.T) {
	out := `{"Path":"example.org/app","Main":true,"Dir":"/w/app"}
{"Path":"example.org/local","Version":"v0.0.0","Replace":{"Path":"./local","Dir":"/w/local"}}
{"Path":"golang.org/x/text","Version":"v0.14.0","Replace":{"Path":"golang.org/x/text","Version":"v0.14.1"}}
{"Path":"golang.org/x/mod","Version":"v0.17.0"}
{"Path":"golang.org/x/tools","Version":"v0.13.0","Indirect":true}
`
	got, err := goFetchList([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"golang.org/x/mod@v0.17.0", "golang.org/x/text@v0.14.1", "golang.org/x/tools@v0.13.0"}
	if !slices.Equal(got, want) {
		t.Fatalf("fetch = %q, want %q", got, want)
	}
	if _, err := goFetchList([]byte(`{"Path":"example.org/x","Version":"v1.0.0","Error":{"Err":"not found"}}`)); err == nil {
		t.Fatal("a module that did not load was accepted")
	}
}

// TestSandboxWorkspaceEnv checks that the sandbox selects the generated workspace and never disables it.
func TestSandboxWorkspaceEnv(t *testing.T) {
	if slices.Contains(sandboxEnv, "GOWORK=off") {
		t.Fatal("sandbox environment disables the workspace")
	}
	cfg, err := (&DockerPresubmit{Image: "lander/sandbox:test", CacheDir: t.TempDir()}).config()
	if err != nil {
		t.Fatal(err)
	}
	args := sandboxArgs(cfg, "lander-presubmit-test", "/srv/src", "/srv/cache", Project{Dir: "ipc/x", Kind: kindGo}, "true")
	if slices.Contains(args, "GOWORK=off") {
		t.Fatalf("argv disables the workspace: %q", args)
	}
	envs := flagValues(args, "-e")
	for _, want := range []string{"GOWORK=/work/go.work", "GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOMODCACHE=/cache/gomod"} {
		if !slices.Contains(envs, want) {
			t.Fatalf("container env lacks %s: %q", want, envs)
		}
	}
}

// siblingStage stages a dependent that requires its sibling at a version nobody published, with no replace
// directive, after the sibling's API changed. It returns the staging root with symlinks resolved.
func siblingStage(t *testing.T) string {
	t.Helper()
	s, tree := fixtureStore(t, map[string]string{
		"go.work":     "go 1.21\n\nuse (\n\t./app\n\t./dep\n)\n",
		"dep/go.mod":  "module example.org/dep\n\ngo 1.21\n",
		"dep/dep.go":  "package dep\n\n// Greeting is the API the candidate introduces.\nfunc Greeting() string { return \"hello\" }\n",
		"app/go.mod":  "module example.org/app\n\ngo 1.21\n\nrequire example.org/dep v0.1.0\n",
		"app/main.go": "package main\n\nimport \"example.org/dep\"\n\nfunc main() { println(dep.Greeting()) }\n",
	}, nil)
	return stagedClosure(t, s, tree, "dep/dep.go")
}

// TestGoWorkResolvesSiblingLocally runs the host prepare step and a real build on the staging root: the
// dependent resolves the sibling's local source, and without the workspace the build fails.
func TestGoWorkResolvesSiblingLocally(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	dst := siblingStage(t)
	cfg, err := (&DockerPresubmit{Image: "lander/sandbox:test", CacheDir: t.TempDir()}).config()
	if err != nil {
		t.Fatal(err)
	}
	var r presubmitReport
	if reason, tail, err := cfg.prepareGo(context.Background(), dst, t.TempDir(), &r); err != nil || reason != "" {
		t.Fatalf("prepare: reason %q, err %v\n%s", reason, err, tail)
	}
	if out, err := goCommand(t, filepath.Join(dst, "app"), filepath.Join(dst, goWorkName), "build", "./..."); err != nil {
		t.Fatalf("build in the workspace: %v\n%s", err, out)
	}
	if out, err := goCommand(t, filepath.Join(dst, "app"), "off", "build", "./..."); err == nil {
		t.Fatalf("build without the workspace succeeded; the fixture does not exercise resolution:\n%s", out)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
