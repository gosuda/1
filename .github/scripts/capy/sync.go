package capy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Invertible reports whether an external tree can be mapped back into the monorepo without
// information loss. Exclusions are invertible (excluded paths are carried over from the monorepo);
// content rewrites are not.
func (wf Workflow) Invertible() bool {
	for _, t := range wf.Transformations {
		switch t.Kind {
		case TransformReplace, TransformStripLocalReplaces, TransformCargoStandalone:
			return false
		}
	}
	return true
}

func prefixEntries(ctx context.Context, repoDir string, rootTreeOID string, prefix string) ([]treeEntry, error) {
	sub, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "-q", fmt.Sprintf("%s:%s", rootTreeOID, prefix))
	if err != nil {
		return nil, nil // prefix absent
	}
	raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", strings.TrimSpace(sub))
	if err != nil {
		return nil, fmt.Errorf("ls-tree %s:%s: %w", rootTreeOID, prefix, err)
	}
	return parseLSTree(raw)
}

// NormalizeMirrorTree drops paths that export would never produce (excluded patterns) from an
// external tree so it can be compared with ExportTree output.
func NormalizeMirrorTree(ctx context.Context, repoDir string, mirrorTreeOID string, wf Workflow) (string, error) {
	raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", mirrorTreeOID)
	if err != nil {
		return "", fmt.Errorf("ls-tree %s: %w", mirrorTreeOID, err)
	}
	entries, err := parseLSTree(raw)
	if err != nil {
		return "", err
	}
	kept := entries[:0:0]
	for _, e := range entries {
		if !shouldExclude(e.Path, wf.Transformations) {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return mirrorTreeOID, nil
	}
	return writeIndexTree(ctx, repoDir, kept)
}

// ImportTree builds the monorepo root tree that corresponds to an external mirror tree, starting
// from monoTreeOID (the monorepo tree at the last sync point). The prefix is replaced by the mirror
// content plus every path the export drops, so monorepo-only files survive.
//
// Callers must pass the last-synced monorepo tree, not the current trunk: the regular 3-way merge
// of the result against the current trunk then keeps monorepo changes that never reached the mirror.
func ImportTree(ctx context.Context, repoDir string, monoTreeOID string, wf Workflow, mirrorTreeOID string) (string, error) {
	if !wf.Invertible() {
		return "", fmt.Errorf("workflow %s applies content transforms that cannot be inverted; inbound is export-only", wf.Name)
	}
	mirrorEntries, err := func() ([]treeEntry, error) {
		raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", mirrorTreeOID)
		if err != nil {
			return nil, fmt.Errorf("ls-tree %s: %w", mirrorTreeOID, err)
		}
		return parseLSTree(raw)
	}()
	if err != nil {
		return "", err
	}
	monoEntries, err := prefixEntries(ctx, repoDir, monoTreeOID, wf.OriginPrefix)
	if err != nil {
		return "", err
	}

	sub := make([]treeEntry, 0, len(mirrorEntries))
	for _, e := range mirrorEntries {
		if !shouldExclude(e.Path, wf.Transformations) {
			sub = append(sub, e)
		}
	}
	for _, e := range monoEntries {
		if shouldExclude(e.Path, wf.Transformations) {
			sub = append(sub, e)
		}
	}
	subTree, err := writeIndexTree(ctx, repoDir, sub)
	if err != nil {
		return "", err
	}
	return SpliceSubtree(ctx, repoDir, monoTreeOID, wf.OriginPrefix, subTree)
}

// SyncBaseline records the last point where the monorepo and one mirror agreed.
type SyncBaseline struct {
	Workflow string
	Mono     string // refs/suda/history commit whose export equals the mirror at Mirror
	Mirror   string // mirror commit
}

// FormatBaselineMessage renders the commit message stored on refs/suda/sync/<workflow>.
func FormatBaselineMessage(b SyncBaseline) string {
	return fmt.Sprintf("suda: sync baseline for %s\n\nSuda-Sync-Mono: %s\nSuda-Sync-Mirror: %s\n", b.Workflow, b.Mono, b.Mirror)
}

// ParseBaselineMessage extracts the baseline trailers from a sync commit message.
func ParseBaselineMessage(msg string) (SyncBaseline, error) {
	var b SyncBaseline
	for _, line := range strings.Split(msg, "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch k {
		case "Suda-Sync-Mono":
			b.Mono = strings.TrimSpace(v)
		case "Suda-Sync-Mirror":
			b.Mirror = strings.TrimSpace(v)
		}
	}
	if b.Mono == "" || b.Mirror == "" {
		return SyncBaseline{}, errors.New("sync baseline message lacks Suda-Sync-Mono/Suda-Sync-Mirror")
	}
	if first, _, _ := strings.Cut(msg, "\n"); strings.HasPrefix(first, "suda: sync baseline for ") {
		b.Workflow = strings.TrimPrefix(first, "suda: sync baseline for ")
	}
	return b, nil
}

// OriginRevID extracts the last GitOrigin-RevId (or another label) trailer value from msg.
func OriginRevID(msg string, label string) string {
	var id string
	for _, line := range strings.Split(msg, "\n") {
		if v, ok := strings.CutPrefix(line, label+": "); ok {
			id = strings.TrimSpace(v)
		}
	}
	return id
}
