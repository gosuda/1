package lander

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type rejectedBlobBody struct{ read bool }

func (r *rejectedBlobBody) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("unexpected read past a rejected blob header")
}

type countedBlobReader struct {
	io.Reader
	bytes int
}

func (r *countedBlobReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestBlobBatchRejectsOverlongHeaderBeforeReadingItAll(t *testing.T) {
	header := strings.Repeat("a", 4096)
	r := &countedBlobReader{Reader: strings.NewReader(header)}
	entry := TreeEntry{OID: strings.Repeat("a", 40), Path: "app/go.mod"}
	got, err := parseBlobBatch(r, []TreeEntry{entry})
	if err == nil || !strings.Contains(err.Error(), "header exceeds limit") || got != nil {
		t.Fatalf("parseBlobBatch = %v, %v; want header limit and no contents", got, err)
	}
	if r.bytes >= len(header) {
		t.Fatalf("read all %d bytes of an overlong header", r.bytes)
	}
}

func TestBlobBatchIndividualLimit(t *testing.T) {
	entry := TreeEntry{OID: strings.Repeat("a", 40), Path: "app/go.mod"}
	t.Run("exact limit", func(t *testing.T) {
		body := strings.Repeat("x", maxManifestBlobBytes)
		out := io.MultiReader(strings.NewReader(fmt.Sprintf("%s blob %d\n", entry.OID, len(body))),
			strings.NewReader(body), strings.NewReader("\n"))
		got, err := parseBlobBatch(out, []TreeEntry{entry})
		if err != nil || len(got) != 1 || got[0] != body {
			t.Fatalf("exact blob limit: %d results, error %v", len(got), err)
		}
	})
	t.Run("one byte over", func(t *testing.T) {
		body := &rejectedBlobBody{}
		out := io.MultiReader(strings.NewReader(fmt.Sprintf("%s blob %d\n", entry.OID, maxManifestBlobBytes+1)), body)
		got, err := parseBlobBatch(out, []TreeEntry{entry})
		if err == nil || !strings.Contains(err.Error(), "exceeds limit") || !strings.Contains(err.Error(), entry.Path) || got != nil {
			t.Fatalf("oversized blob: %d results, error %v; want size limit naming %s", len(got), err, entry.Path)
		}
		if body.read {
			t.Fatal("read body after its header exceeded the blob limit")
		}
	})
}

func TestBlobBatchAggregateLimitCountsDuplicateObjects(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprintf("extra bytes %d", extra), func(t *testing.T) {
			body := strings.Repeat("x", maxManifestBlobBytes)
			oid := strings.Repeat("a", 40)
			var entries []TreeEntry
			var frames []io.Reader
			for remaining := maxManifestBatchBytes; remaining > 0; {
				size := min(remaining, len(body))
				entries = append(entries, TreeEntry{OID: oid, Path: fmt.Sprintf("app%d/go.mod", len(entries))})
				frames = append(frames, strings.NewReader(fmt.Sprintf("%s blob %d\n", oid, size)),
					strings.NewReader(body[:size]), strings.NewReader("\n"))
				remaining -= size
			}
			rejected := &rejectedBlobBody{}
			if extra != 0 {
				entries = append(entries, TreeEntry{OID: oid, Path: "overflow/go.mod"})
				frames = append(frames, strings.NewReader(fmt.Sprintf("%s blob %d\n", oid, extra)), rejected)
			}
			got, err := parseBlobBatch(io.MultiReader(frames...), entries)
			if extra != 0 {
				if err == nil || !strings.Contains(err.Error(), "batch exceeds limit") || !strings.Contains(err.Error(), "overflow/go.mod") || got != nil {
					t.Fatalf("over-limit batch: %d results, error %v; want batch limit and no contents", len(got), err)
				}
				if rejected.read {
					t.Fatal("read body after its header exceeded the aggregate limit")
				}
				return
			}
			if err != nil || len(got) != len(entries) {
				t.Fatalf("exact batch limit: %d results, error %v", len(got), err)
			}
			total := 0
			for _, content := range got {
				if len(content) > len(body) || content != body[:len(content)] {
					t.Fatal("blob contents changed")
				}
				total += len(content)
			}
			if total != maxManifestBatchBytes {
				t.Fatalf("returned %d bytes, want %d", total, maxManifestBatchBytes)
			}
		})
	}
}

func TestBlobBatchCountLimit(t *testing.T) {
	entry := TreeEntry{OID: strings.Repeat("a", 40), Path: "app/go.mod"}
	entries := make([]TreeEntry, maxManifestBlobs+1)
	for i := range entries {
		entries[i] = entry
	}
	t.Run("exact parser limit", func(t *testing.T) {
		out := strings.NewReader(strings.Repeat(entry.OID+" blob 0\n\n", maxManifestBlobs))
		got, err := parseBlobBatch(out, entries[:maxManifestBlobs])
		if err != nil || len(got) != maxManifestBlobs {
			t.Fatalf("exact count limit: %d results, error %v", len(got), err)
		}
	})
	t.Run("parser rejects before read", func(t *testing.T) {
		out := &rejectedBlobBody{}
		got, err := parseBlobBatch(out, entries)
		if err == nil || !strings.Contains(err.Error(), "manifest count exceeds limit") || got != nil || out.read {
			t.Fatalf("excess count: %d results, error %v, read=%v", len(got), err, out.read)
		}
	})
	t.Run("store rejects before git", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		s := &Store{Dir: filepath.Join(t.TempDir(), "not-a-store")}
		got, err := s.readBlobs(context.Background(), entries)
		if err == nil || !strings.Contains(err.Error(), "manifest count exceeds limit") || got != nil {
			t.Fatalf("excess count: %d results, error %v; want count rejection before Git lookup", len(got), err)
		}
	})
}

func TestReadBlobsRejectsDuplicateObjectAmplification(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{"app/go.mod": strings.Repeat("x", maxManifestBlobBytes)}, nil)
	entries, err := s.ListTree(context.Background(), tree, true)
	if err != nil {
		t.Fatal(err)
	}
	count := maxManifestBatchBytes/maxManifestBlobBytes + 1
	duplicates := make([]TreeEntry, count)
	for i := range duplicates {
		duplicates[i] = entries[0]
		duplicates[i].Path = fmt.Sprintf("app%d/go.mod", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := s.readBlobs(ctx, duplicates)
	if err == nil || !strings.Contains(err.Error(), "batch exceeds limit") || got != nil {
		t.Fatalf("duplicated object batch: %d results, error %v; want batch limit", len(got), err)
	}
	if ctx.Err() != nil {
		t.Fatalf("batch rejection did not stop Git before the deadline: %v", ctx.Err())
	}
}

// This process stands in for Git so cancellation and error paths do not depend
// on scheduler timing or the size of a real repository. The shell uses exec,
// leaving a single process for readBlobs to stop and reap.
func TestBlobBatchGitHelperProcess(t *testing.T) {
	mode := os.Getenv("LANDER_BLOB_HELPER_MODE")
	if mode == "" {
		return
	}
	if err := os.WriteFile(os.Getenv("LANDER_BLOB_HELPER_PID"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(2)
	}
	switch mode {
	case "limit":
		fmt.Printf("%s blob %d\n", strings.Repeat("a", 40), maxManifestBlobBytes+1)
	case "stderr":
		io.WriteString(os.Stderr, strings.Repeat("e", maxBatchStderrBytes*2))
		os.Exit(1)
	case "cancel":
	default:
		os.Exit(2)
	}
	time.Sleep(time.Hour)
	os.Exit(3)
}

func TestReadBlobsStopsAndReapsGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process fixture uses a POSIX shell")
	}
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("process fixture needs sh")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cancel", "limit", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			script := "#!" + shell + "\nexec \"$LANDER_BLOB_HELPER_BINARY\" -test.run=^TestBlobBatchGitHelperProcess$ -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("LANDER_BLOB_HELPER_BINARY", binary)
			t.Setenv("LANDER_BLOB_HELPER_PID", pidFile)
			t.Setenv("LANDER_BLOB_HELPER_MODE", mode)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			s := &Store{Dir: filepath.Join(dir, "store.git")}
			done := make(chan error, 1)
			go func() {
				got, err := s.readBlobs(ctx, []TreeEntry{{OID: strings.Repeat("a", 40), Path: "app/go.mod"}})
				if got != nil {
					done <- fmt.Errorf("failed read exposed %d blobs", len(got))
					return
				}
				done <- err
			}()
			var pid []byte
			for {
				pid, err = os.ReadFile(pidFile)
				if err == nil && len(pid) != 0 {
					break
				}
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
				select {
				case <-ctx.Done():
					t.Fatal("Git fixture did not start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("readBlobs did not stop Git promptly")
			}
			switch mode {
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("readBlobs error = %v, want context.Canceled", err)
				}
			case "limit":
				if err == nil || !strings.Contains(err.Error(), "exceeds limit") || ctx.Err() != nil {
					t.Fatalf("readBlobs error = %v, context = %v; want prompt blob limit rejection", err, ctx.Err())
				}
			case "stderr":
				var gitErr *GitError
				if !errors.As(err, &gitErr) {
					t.Fatalf("readBlobs error type = %T; want an error wrapping GitError", err)
				}
				if len(gitErr.Stderr) != maxBatchStderrBytes {
					t.Fatalf("retained %d stderr bytes, want %d", len(gitErr.Stderr), maxBatchStderrBytes)
				}
			}
			if err := exec.Command(shell, "-c", "kill -0 \"$1\" 2>/dev/null", "sh", string(pid)).Run(); err == nil {
				t.Fatalf("Git process %s still exists after readBlobs returned", pid)
			}
		})
	}
}

func TestLoadProjectsLimitsManifestSelection(t *testing.T) {
	ctx := context.Background()
	s, projectTree := fixtureStore(t, map[string]string{"go.mod": "module example.com/project\n"}, nil)
	workspace, err := s.GitStdin(ctx, "go 1.21\n", "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}

	// Reusing one subtree keeps the repository small while the scan selects many manifests.
	// The root workspace must count toward the same limit as project manifests.
	entries := make([]TreeEntry, 0, maxManifestBlobs+1)
	for i := range maxManifestBlobs {
		entries = append(entries, TreeEntry{
			Mode: "040000", Type: "tree", OID: projectTree, Path: fmt.Sprintf("project%05d", i),
		})
	}
	entries = append(entries, TreeEntry{Mode: "100644", Type: "blob", OID: workspace, Path: goWorkName})
	tree, err := s.MkTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := loadProjects(ctx, s, tree)
	if err == nil || !strings.Contains(err.Error(), "limit") || graph != nil {
		t.Fatalf("loadProjects = %v, %v; want no graph and a manifest limit error", graph, err)
	}
}
