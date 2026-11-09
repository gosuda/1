package capy

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

// CheckLocalReplaces returns the sorted violation lines of go.mod files in treeOID that carry a local
// replace directive leaving the module directory (`replace x => ../y` or an absolute path). Committed go.mod
// files must stay buildable on their own, and so must the standalone mirror exported from them: sibling
// modules resolve through the root go.work, never through a path in go.mod. With a non-empty baseTreeOID,
// only go.mod files added or modified since the base are read, and a replace that already exists unchanged
// in the base go.mod at the same path is grandfathered. Control-plane files under .github/ are exempt: the
// lander pins its own sibling module that way.
func CheckLocalReplaces(ctx context.Context, repoDir string, baseTreeOID string, treeOID string) ([]string, error) {
	blobs, err := listBlobs(ctx, repoDir, treeOID)
	if err != nil {
		return nil, err
	}
	var baseOIDs map[string]string
	if baseTreeOID != "" {
		baseBlobs, err := listBlobs(ctx, repoDir, baseTreeOID)
		if err != nil {
			return nil, err
		}
		baseOIDs = make(map[string]string, len(baseBlobs))
		for _, e := range baseBlobs {
			if path.Base(e.Path) == "go.mod" {
				baseOIDs[e.Path] = e.OID
			}
		}
	}

	var out []string
	for _, e := range blobs {
		if path.Base(e.Path) != "go.mod" || strings.HasPrefix(e.Path, ".github/") {
			continue
		}
		baseOID, inBase := baseOIDs[e.Path]
		if inBase && baseOID == e.OID {
			continue
		}
		src, err := gitReadBlob(ctx, repoDir, e.OID)
		if err != nil {
			return nil, err
		}
		// A parse error still yields every directive read before it; the visibility check reports the file.
		gm, _ := parseGoMod(src)
		var old map[[2]string]bool
		if inBase {
			baseSrc, err := gitReadBlob(ctx, repoDir, baseOID)
			if err != nil {
				return nil, err
			}
			baseMod, _ := parseGoMod(baseSrc)
			old = make(map[[2]string]bool, len(baseMod.replaces))
			for _, rp := range baseMod.replaces {
				old[[2]string{rp.from, rp.to}] = true
			}
		}
		for _, rp := range gm.replaces {
			if !isLocalPath(rp.to) || !leavesModuleDir(rp.to) || old[[2]string{rp.from, rp.to}] {
				continue
			}
			out = append(out, fmt.Sprintf("%s:%d: local replace leaves the module directory: %s => %s (remove it; the root go.work resolves sibling modules locally)",
				e.Path, rp.line, rp.from, rp.to))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// leavesModuleDir reports whether the local replace target to, relative to the go.mod directory, points
// outside that directory.
func leavesModuleDir(to string) bool {
	if path.IsAbs(to) {
		return true
	}
	rel := path.Clean(to)
	return rel == ".." || strings.HasPrefix(rel, "../")
}
