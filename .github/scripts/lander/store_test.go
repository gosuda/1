package lander

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadBlobsPreservesContentsAndOrder(t *testing.T) {
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "store.git")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	files := []struct{ path, body string }{
		{"empty", ""},
		{"binary\nname", "before\x00after\n\n"},
		{"plain", "no newline"},
		{"newlines", "\n\n\n"},
	}
	var entries []TreeEntry
	var want []string
	for _, f := range files {
		oid, err := s.GitStdin(ctx, f.body, "hash-object", "-w", "--stdin")
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, TreeEntry{Type: "blob", OID: oid, Path: f.path})
		want = append(want, f.body)
	}
	entries = append(entries, entries[1])
	want = append(want, want[1])
	got, err := s.readBlobs(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("blobs = %q, want %q", got, want)
	}
}

func TestReadBlobsRejectsMissingAndNonBlobObjects(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{"file": "body\n"}, nil)
	ctx := context.Background()
	entries, err := s.ListTree(ctx, tree, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, oid, want string
	}{
		{"missing", strings.Repeat("0", len(tree)), "missing"},
		{"tree", tree, "unexpected batch header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := TreeEntry{Type: "blob", OID: tc.oid, Path: "app/go.mod"}
			got, err := s.readBlobs(ctx, append(slices.Clone(entries), bad))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), bad.Path) {
				t.Fatalf("readBlobs error = %v, want %q naming %s", err, tc.want, bad.Path)
			}
			if got != nil {
				t.Fatalf("failed batch exposed partial contents: %q", got)
			}
		})
	}
}

func TestReadBlobsRejectsInvalidObjectIDs(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "not-a-store")}
	for _, oid := range []string{"", "HEAD", strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 40) + "\n"} {
		t.Run(oid, func(t *testing.T) {
			_, err := s.readBlobs(context.Background(), []TreeEntry{{OID: oid, Path: "app/go.mod"}})
			if err == nil || !strings.Contains(err.Error(), "invalid object ID") || !strings.Contains(err.Error(), "app/go.mod") {
				t.Fatalf("readBlobs error = %v, want invalid object ID naming app/go.mod", err)
			}
		})
	}
}

func TestReadBlobsCancellation(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{"file": "body"}, nil)
	entries, err := s.ListTree(context.Background(), tree, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.readBlobs(ctx, entries); !errors.Is(err, context.Canceled) {
		t.Fatalf("readBlobs error = %v, want context.Canceled", err)
	}
}

func TestReadBlobsEmptySelection(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "not-a-store")}
	got, err := s.readBlobs(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("readBlobs(nil) = %q, %v, want nil, nil", got, err)
	}
}

func TestParseBlobBatchRejectsMalformedOutput(t *testing.T) {
	oid := strings.Repeat("a", 40)
	entry := TreeEntry{Type: "blob", OID: oid, Path: "app/go.mod"}
	valid := oid + " blob 1\nx\n"
	for _, tc := range []struct {
		name, out, want string
	}{
		{"missing header", "", "truncated batch header"},
		{"truncated header", oid + " blob 1", "truncated batch header"},
		{"missing object", oid + " missing\n", "is missing"},
		{"wrong object", strings.Repeat("b", 40) + " blob 1\nx\n", "unexpected batch header"},
		{"wrong type", oid + " tree 1\nx\n", "unexpected batch header"},
		{"extra header field", oid + " blob 1 extra\nx\n", "unexpected batch header"},
		{"extra separator", oid + "  blob 1\nx\n", "unexpected batch header"},
		{"missing size", oid + " blob \n", "invalid blob size"},
		{"negative size", oid + " blob -1\nx\n", "invalid blob size"},
		{"signed size", oid + " blob +1\nx\n", "invalid blob size"},
		{"invalid size", oid + " blob one\nx\n", "invalid blob size"},
		{"size overflow", oid + " blob 18446744073709551616\nx\n", "invalid blob size"},
		{"max size", oid + " blob 18446744073709551615\nx\n", "truncated blob"},
		{"truncated body", oid + " blob 2\nx\n", "truncated blob"},
		{"missing terminator", oid + " blob 1\nx", "truncated blob"},
		{"wrong terminator", oid + " blob 1\nx\x00", "missing blob terminator"},
		{"extra response", valid + valid, "unexpected trailing batch output"},
		{"extra newline", valid + "\n", "unexpected trailing batch output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBlobBatch(tc.out, []TreeEntry{entry})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseBlobBatch error = %v, want %q", err, tc.want)
			}
			if got != nil {
				t.Fatalf("invalid batch exposed contents: %q", got)
			}
		})
	}

	second := TreeEntry{Type: "blob", OID: oid, Path: "second/go.mod"}
	got, err := parseBlobBatch(valid, []TreeEntry{entry, second})
	if err == nil || !strings.Contains(err.Error(), second.Path) || got != nil {
		t.Fatalf("incomplete batch = %q, %v; want no contents and an error naming %s", got, err, second.Path)
	}
}
