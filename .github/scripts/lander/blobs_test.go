package lander

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadBlobsPreservesBytesAndRequestOrder(t *testing.T) {
	files := map[string]string{
		"empty":    "",
		"plain":    "no trailing newline",
		"trailing": "first\nsecond\n\n",
		"binary":   "before\x00after\n",
		"large":    strings.Repeat("line\n", 14000) + "end\n\n",
	}
	s, tree := fixtureStore(t, files, nil)
	entries, err := s.ListTree(context.Background(), tree, true)
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]TreeEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	for _, names := range [][]string{
		{"plain", "empty", "binary", "large", "plain", "trailing"},
		{"empty", "empty"},
	} {
		var requests []TreeEntry
		var want []string
		for _, name := range names {
			requests = append(requests, byPath[name])
			want = append(want, files[name])
		}
		got, err := s.readBlobs(context.Background(), requests)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("readBlobs(%q) did not preserve content and order", names)
		}
	}

	// Existing text callers still rely on Git trimming trailing newlines.
	got, err := s.Git(context.Background(), "cat-file", "blob", byPath["trailing"].OID)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimRight(files["trailing"], "\n"); got != want {
		t.Fatalf("Git text output = %q, want %q", got, want)
	}
}

func TestReadBlobsEmptyRequestNeedsNoRepository(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "does-not-exist")}
	got, err := s.readBlobs(context.Background(), nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty request = %q, %v", got, err)
	}
}

func TestReadBlobsRejectsMissingAndNonBlobObjects(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{"good": "valid\n"}, nil)
	entries, err := s.ListTree(context.Background(), tree, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		oid  string
	}{
		{"missing", strings.Repeat("0", 40)},
		{"tree", tree},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := []TreeEntry{entries[0], {Mode: "100644", Type: "blob", OID: tc.oid, Path: tc.name}}
			got, err := s.readBlobs(context.Background(), requests)
			if err == nil || got != nil {
				t.Fatalf("invalid object returned %q, %v; want no partial result and an error", got, err)
			}
		})
	}
}

func TestReadBlobsHonorsCanceledContext(t *testing.T) {
	s, tree := fixtureStore(t, map[string]string{"good": "valid\n"}, nil)
	entries, err := s.ListTree(context.Background(), tree, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.readBlobs(ctx, entries)
	if err == nil || got != nil {
		t.Fatalf("canceled request returned %q, %v; want no result and an error", got, err)
	}
}

func TestParseBlobBatchRejectsIncompleteOrInvalidResponses(t *testing.T) {
	a := TreeEntry{Mode: "100644", Type: "blob", OID: strings.Repeat("a", 40), Path: "a/go.mod"}
	b := TreeEntry{Mode: "100644", Type: "blob", OID: strings.Repeat("b", 40), Path: "b/go.mod"}
	frame := func(entry TreeEntry, body string) string {
		return fmt.Sprintf("%s blob %d\n%s\n", entry.OID, len(body), body)
	}
	for _, tc := range []struct {
		name    string
		entries []TreeEntry
		out     string
	}{
		{"missing header", []TreeEntry{a}, ""},
		{"unfinished header", []TreeEntry{a}, a.OID + " blob 3"},
		{"missing object", []TreeEntry{a}, a.OID + " missing\n"},
		{"wrong type", []TreeEntry{a}, a.OID + " tree 3\nabc\n"},
		{"extra header field", []TreeEntry{a}, a.OID + " blob 3 extra\nabc\n"},
		{"nonnumeric size", []TreeEntry{a}, a.OID + " blob three\nabc\n"},
		{"negative size", []TreeEntry{a}, a.OID + " blob -1\n\n"},
		{"overflowing size", []TreeEntry{a}, a.OID + " blob 18446744073709551616\n"},
		{"short body", []TreeEntry{a}, a.OID + " blob 4\nabc\n"},
		{"missing separator", []TreeEntry{a}, a.OID + " blob 3\nabc"},
		{"wrong separator", []TreeEntry{a}, a.OID + " blob 3\nabc\x00"},
		{"wrong order", []TreeEntry{a, b}, frame(b, "second") + frame(a, "first")},
		{"incomplete second body", []TreeEntry{a, b}, frame(a, "first") + b.OID + " blob 4\nab"},
		{"missing duplicate", []TreeEntry{a, a}, frame(a, "first")},
		{"extra response", []TreeEntry{a}, frame(a, "first") + frame(b, "second")},
		{"extra bytes", []TreeEntry{a}, frame(a, "first") + "unexpected"},
		{"unsolicited response", nil, frame(a, "first")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBlobBatch(tc.entries, tc.out)
			if err == nil || got != nil {
				t.Fatalf("invalid batch returned %q, %v; want no partial result and an error", got, err)
			}
		})
	}
}
