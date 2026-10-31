package lander

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// TreeEntry is one ls-tree record.
type TreeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

// ListTree returns the entries of tree. recursive lists blobs by full path.
func (s *Store) ListTree(ctx context.Context, tree string, recursive bool) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-z"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, tree)
	out, err := s.Git(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseLsTree(out)
}

func parseLsTree(out string) ([]TreeEntry, error) {
	var res []TreeEntry
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return nil, fmt.Errorf("malformed ls-tree record %q", rec)
		}
		res = append(res, TreeEntry{Mode: f[0], Type: f[1], OID: f[2], Path: path})
	}
	return res, nil
}

// BuildTree writes a tree from entries using a private index file.
func (s *Store) BuildTree(ctx context.Context, entries []TreeEntry) (string, error) {
	f, err := os.CreateTemp("", "lander-idx-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	f.Close()
	_ = os.Remove(idx)
	defer os.Remove(idx)

	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s %s\t%s\n", e.Mode, e.Type, e.OID, e.Path)
	}
	env := []string{"GIT_INDEX_FILE=" + idx}
	if b.Len() > 0 {
		if _, err := s.run(ctx, nil, env, strings.NewReader(b.String()), "update-index", "--index-info"); err != nil {
			return "", err
		}
	}
	return s.run(ctx, nil, env, nil, "write-tree")
}

// ChangedPaths lists paths that differ between two trees.
func (s *Store) ChangedPaths(ctx context.Context, a, b string) ([]string, error) {
	out, err := s.Git(ctx, "diff-tree", "-r", "-z", "--name-only", "--no-commit-id", a, b)
	if err != nil {
		return nil, err
	}
	var res []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			res = append(res, p)
		}
	}
	return res, nil
}

// pickTop returns the top-level entries of tree whose names are in names.
func (s *Store) pickTop(ctx context.Context, tree string, names map[string]bool) ([]TreeEntry, error) {
	all, err := s.ListTree(ctx, tree, true)
	if err != nil {
		return nil, err
	}
	var res []TreeEntry
	for _, e := range all {
		if names[topComponent(e.Path)] {
			res = append(res, e)
		}
	}
	return res, nil
}

func topComponent(p string) string {
	top, _, _ := strings.Cut(p, "/")
	return top
}

// overlayNames returns the top-level entry names of the control-plane tree.
func (s *Store) overlayNames(ctx context.Context, ctrlTree string) (map[string]bool, error) {
	all, err := s.ListTree(ctx, ctrlTree, false)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(all))
	for _, e := range all {
		names[e.Path] = true
	}
	return names, nil
}

// withOverlay returns target with every top-level entry named in names replaced by the entries
// of ctrl (names absent from ctrl are removed).
func (s *Store) withOverlay(ctx context.Context, target, ctrl string, names map[string]bool) (string, error) {
	targetEntries, err := s.ListTree(ctx, target, true)
	if err != nil {
		return "", err
	}
	ctrlEntries, err := s.pickTop(ctx, ctrl, names)
	if err != nil {
		return "", err
	}
	var res []TreeEntry
	for _, e := range targetEntries {
		if !names[topComponent(e.Path)] {
			res = append(res, e)
		}
	}
	res = append(res, ctrlEntries...)
	return s.BuildTree(ctx, res)
}

// extractOverlay returns the sub-tree of tree made of the top-level entries named in names.
func (s *Store) extractOverlay(ctx context.Context, tree string, names map[string]bool) (string, error) {
	entries, err := s.pickTop(ctx, tree, names)
	if err != nil {
		return "", err
	}
	return s.BuildTree(ctx, entries)
}
