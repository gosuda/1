package capy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"
)

// Config holds the parsed workflows from a capy.star file.
type Config struct {
	Workflows []Workflow
}

// Workflow defines a bidirectional mapping between a monorepo prefix and an external repository.
type Workflow struct {
	Name            string
	OriginPrefix    string
	DestinationRepo string
	Branch          string
	Mode            string
	Transformations []Transformation
}

// TransformKind identifies a tree or metadata transformation operation.
type TransformKind string

const (
	TransformExclude            TransformKind = "exclude"
	TransformReplace            TransformKind = "replace"
	TransformStripLocalReplaces TransformKind = "gomod_strip_local_replaces"
	TransformCargoStandalone    TransformKind = "cargo_standalone_workspace"
	TransformExposeLabel        TransformKind = "metadata_expose_label"
	TransformScrubber           TransformKind = "metadata_scrubber"
)

// Transformation represents one declarative transformation rule in a Workflow.
type Transformation struct {
	Kind     TransformKind
	Patterns []string
	Before   string
	After    string
	Label    string
	Regex    *regexp.Regexp
}

// FindWorkflow looks up a workflow by name.
func (c *Config) FindWorkflow(name string) (Workflow, bool) {
	for _, wf := range c.Workflows {
		if wf.Name == name {
			return wf, true
		}
	}
	return Workflow{}, false
}

// LoadConfig evaluates a Starlark configuration file and returns its registered workflows.
func LoadConfig(configPath string) (*Config, error) {
	src, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", configPath, err)
	}

	cfg := &Config{}
	builtins := buildStarlarkModules(cfg)

	thread := &starlark.Thread{
		Name: "capy",
		Load: func(_ *starlark.Thread, module string) (starlark.StringDict, error) {
			if strings.HasSuffix(module, "defs.star") {
				return builtins, nil
			}
			return nil, fmt.Errorf("unsupported load module %q", module)
		},
	}

	opts := &syntax.FileOptions{
		TopLevelControl: true,
		GlobalReassign:  true,
	}
	if _, err := starlark.ExecFileOptions(opts, thread, configPath, src, builtins); err != nil {
		return nil, fmt.Errorf("eval %s: %w", configPath, err)
	}
	return cfg, nil
}

type transformValue struct {
	t Transformation
}

func (v *transformValue) String() string       { return fmt.Sprintf("transform(%s)", v.t.Kind) }
func (v *transformValue) Type() string         { return "transformation" }
func (v *transformValue) Freeze()              {}
func (v *transformValue) Truth() starlark.Bool { return starlark.True }
func (v *transformValue) Hash() (uint32, error) {
	return 0, errors.New("unhashable type: transformation")
}

func buildStarlarkModules(cfg *Config) starlark.StringDict {
	capyMod := &starlarkstruct.Module{
		Name: "capy",
		Members: starlark.StringDict{
			"monorepo_path": starlark.NewBuiltin("capy.monorepo_path", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var prefix string
				if err := starlark.UnpackArgs("capy.monorepo_path", args, kwargs, "prefix", &prefix); err != nil {
					return nil, err
				}
				cleaned := strings.Trim(path.Clean(prefix), "/")
				if cleaned == "" || cleaned == "." {
					return nil, errors.New("capy.monorepo_path: prefix must not be empty")
				}
				return starlarkstruct.FromStringDict(starlark.String("monorepo_path"), starlark.StringDict{
					"prefix": starlark.String(cleaned),
				}), nil
			}),
			"github_repo": starlark.NewBuiltin("capy.github_repo", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var repo string
				branch := "main"
				if err := starlark.UnpackArgs("capy.github_repo", args, kwargs, "repo", &repo, "branch?", &branch); err != nil {
					return nil, err
				}
				return starlarkstruct.FromStringDict(starlark.String("github_repo"), starlark.StringDict{
					"repo":   starlark.String(repo),
					"branch": starlark.String(branch),
				}), nil
			}),
			"workflow": starlark.NewBuiltin("capy.workflow", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var (
					name       string
					origin     *starlarkstruct.Struct
					dest       *starlarkstruct.Struct
					mode       = "SQUASH"
					transforms *starlark.List
				)
				if err := starlark.UnpackArgs("capy.workflow", args, kwargs,
					"name", &name,
					"origin", &origin,
					"destination", &dest,
					"mode?", &mode,
					"transformations?", &transforms,
				); err != nil {
					return nil, err
				}
				prefixVal, err := origin.Attr("prefix")
				if err != nil {
					return nil, fmt.Errorf("capy.workflow %s: invalid origin: %w", name, err)
				}
				repoVal, err := dest.Attr("repo")
				if err != nil {
					return nil, fmt.Errorf("capy.workflow %s: invalid destination repo: %w", name, err)
				}
				branchVal, err := dest.Attr("branch")
				if err != nil {
					return nil, fmt.Errorf("capy.workflow %s: invalid destination branch: %w", name, err)
				}
				prefixStr, _ := starlark.AsString(prefixVal)
				repoStr, _ := starlark.AsString(repoVal)
				branchStr, _ := starlark.AsString(branchVal)

				wf := Workflow{
					Name:            name,
					OriginPrefix:    prefixStr,
					DestinationRepo: repoStr,
					Branch:          branchStr,
					Mode:            mode,
				}
				if transforms != nil {
					iter := transforms.Iterate()
					defer iter.Done()
					var item starlark.Value
					for iter.Next(&item) {
						tv, ok := item.(*transformValue)
						if !ok {
							return nil, fmt.Errorf("capy.workflow %s: expected transformation, got %s", name, item.Type())
						}
						wf.Transformations = append(wf.Transformations, tv.t)
					}
				}
				cfg.Workflows = append(cfg.Workflows, wf)
				return starlark.None, nil
			}),
		},
	}

	coreMod := &starlarkstruct.Module{
		Name: "core",
		Members: starlark.StringDict{
			"exclude": starlark.NewBuiltin("core.exclude", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var list *starlark.List
				if err := starlark.UnpackArgs("core.exclude", args, kwargs, "paths", &list); err != nil {
					return nil, err
				}
				patterns, err := unpackStringList(list)
				if err != nil {
					return nil, err
				}
				return &transformValue{t: Transformation{Kind: TransformExclude, Patterns: patterns}}, nil
			}),
			"replace": starlark.NewBuiltin("core.replace", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var before, after string
				var paths *starlark.List
				if err := starlark.UnpackArgs("core.replace", args, kwargs, "before", &before, "after", &after, "paths?", &paths); err != nil {
					return nil, err
				}
				var patterns []string
				if paths != nil {
					var err error
					patterns, err = unpackStringList(paths)
					if err != nil {
						return nil, err
					}
				}
				return &transformValue{t: Transformation{
					Kind:     TransformReplace,
					Before:   before,
					After:    after,
					Patterns: patterns,
				}}, nil
			}),
		},
	}

	gomodMod := &starlarkstruct.Module{
		Name: "gomod",
		Members: starlark.StringDict{
			"strip_local_replaces": starlark.NewBuiltin("gomod.strip_local_replaces", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				if err := starlark.UnpackArgs("gomod.strip_local_replaces", args, kwargs); err != nil {
					return nil, err
				}
				return &transformValue{t: Transformation{Kind: TransformStripLocalReplaces}}, nil
			}),
		},
	}

	cargoMod := &starlarkstruct.Module{
		Name: "cargo",
		Members: starlark.StringDict{
			"standalone_workspace": starlark.NewBuiltin("cargo.standalone_workspace", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				if err := starlark.UnpackArgs("cargo.standalone_workspace", args, kwargs); err != nil {
					return nil, err
				}
				return &transformValue{t: Transformation{Kind: TransformCargoStandalone}}, nil
			}),
		},
	}

	metadataMod := &starlarkstruct.Module{
		Name: "metadata",
		Members: starlark.StringDict{
			"expose_label": starlark.NewBuiltin("metadata.expose_label", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var label string
				if err := starlark.UnpackArgs("metadata.expose_label", args, kwargs, "label", &label); err != nil {
					return nil, err
				}
				return &transformValue{t: Transformation{Kind: TransformExposeLabel, Label: label}}, nil
			}),
			"scrubber": starlark.NewBuiltin("metadata.scrubber", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
				var pattern string
				if err := starlark.UnpackArgs("metadata.scrubber", args, kwargs, "pattern", &pattern); err != nil {
					return nil, err
				}
				re, err := regexp.Compile(pattern)
				if err != nil {
					return nil, fmt.Errorf("metadata.scrubber: invalid regex %q: %w", pattern, err)
				}
				return &transformValue{t: Transformation{Kind: TransformScrubber, Regex: re}}, nil
			}),
		},
	}

	return starlark.StringDict{
		"capy":     capyMod,
		"core":     coreMod,
		"gomod":    gomodMod,
		"cargo":    cargoMod,
		"metadata": metadataMod,
	}
}

func unpackStringList(list *starlark.List) ([]string, error) {
	out := make([]string, 0, list.Len())
	iter := list.Iterate()
	defer iter.Done()
	var item starlark.Value
	for iter.Next(&item) {
		s, ok := starlark.AsString(item)
		if !ok {
			return nil, fmt.Errorf("expected string element, got %s", item.Type())
		}
		out = append(out, s)
	}
	return out, nil
}

type treeEntry struct {
	Mode string
	Type string
	OID  string
	Path string
}

// TransformSubtree extracts the workflow's origin prefix from rootTreeOID, applies all file
// transformations in memory, and returns the resulting standalone root tree SHA-1.
func TransformSubtree(ctx context.Context, repoDir string, rootTreeOID string, wf Workflow) (string, error) {
	subOID, err := gitOutput(ctx, repoDir, nil, "rev-parse", fmt.Sprintf("%s:%s", rootTreeOID, wf.OriginPrefix))
	if err != nil {
		return "", fmt.Errorf("resolve subtree %s in %s: %w", wf.OriginPrefix, rootTreeOID, err)
	}
	subOID = strings.TrimSpace(subOID)

	if !hasFileTransforms(wf) {
		return subOID, nil
	}

	raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", subOID)
	if err != nil {
		return "", fmt.Errorf("ls-tree %s: %w", subOID, err)
	}

	entries, err := parseLSTree(raw)
	if err != nil {
		return "", err
	}

	modified := false
	retained := make([]treeEntry, 0, len(entries))

	for _, e := range entries {
		if shouldExclude(e.Path, wf.Transformations) {
			modified = true
			continue
		}
		if e.Type == "blob" && needsContentInspection(e.Path, wf.Transformations) {
			content, err := gitReadBlob(ctx, repoDir, e.OID)
			if err != nil {
				return "", err
			}
			updated, changed := applyContentTransforms(e.Path, content, wf.Transformations)
			if changed {
				newOID, err := gitWriteBlob(ctx, repoDir, updated)
				if err != nil {
					return "", err
				}
				e.OID = newOID
				modified = true
			}
		}
		retained = append(retained, e)
	}

	if !modified {
		return subOID, nil
	}

	return writeIndexTree(ctx, repoDir, retained)
}

func hasFileTransforms(wf Workflow) bool {
	for _, t := range wf.Transformations {
		switch t.Kind {
		case TransformExclude, TransformReplace, TransformStripLocalReplaces, TransformCargoStandalone:
			return true
		}
	}
	return false
}

func shouldExclude(relPath string, transforms []Transformation) bool {
	for _, t := range transforms {
		if t.Kind != TransformExclude {
			continue
		}
		for _, pat := range t.Patterns {
			if matchGlob(pat, relPath) {
				return true
			}
		}
	}
	return false
}

func needsContentInspection(relPath string, transforms []Transformation) bool {
	base := path.Base(relPath)
	for _, t := range transforms {
		switch t.Kind {
		case TransformStripLocalReplaces:
			if base == "go.mod" {
				return true
			}
		case TransformCargoStandalone:
			if relPath == "Cargo.toml" {
				return true
			}
		case TransformReplace:
			if len(t.Patterns) == 0 {
				return true
			}
			for _, pat := range t.Patterns {
				if matchGlob(pat, relPath) {
					return true
				}
			}
		}
	}
	return false
}

var localReplaceBlockRe = regexp.MustCompile(`(?m)^replace\s+[^\n]+=>\s*\.\.?/[^\n]*\n?`)

func applyContentTransforms(relPath string, content []byte, transforms []Transformation) ([]byte, bool) {
	cur := string(content)
	base := path.Base(relPath)
	for _, t := range transforms {
		switch t.Kind {
		case TransformStripLocalReplaces:
			if base == "go.mod" {
				cur = localReplaceBlockRe.ReplaceAllString(cur, "")
			}
		case TransformCargoStandalone:
			if relPath == "Cargo.toml" && !strings.Contains(cur, "[workspace]") {
				cur = strings.TrimRight(cur, "\n") + "\n\n[workspace]\n"
			}
		case TransformReplace:
			matched := len(t.Patterns) == 0
			for _, pat := range t.Patterns {
				if matchGlob(pat, relPath) {
					matched = true
					break
				}
			}
			if matched {
				cur = strings.ReplaceAll(cur, t.Before, t.After)
			}
		}
	}
	out := []byte(cur)
	return out, !bytes.Equal(content, out)
}

func matchGlob(pattern, target string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	target = strings.TrimPrefix(target, "/")
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return target == prefix || strings.HasPrefix(target, prefix+"/")
	}
	if strings.HasPrefix(pattern, "**") {
		suffix := strings.TrimPrefix(pattern, "**")
		return strings.HasSuffix(target, suffix)
	}
	ok, err := path.Match(pattern, target)
	if err == nil && ok {
		return true
	}
	return false
}

// FormatCommitMessage scrubs internal markers and appends configured origin trailers.
func FormatCommitMessage(rawMsg string, wf Workflow, sourceRev string) string {
	msg := strings.TrimSpace(rawMsg)
	for _, t := range wf.Transformations {
		if t.Kind == TransformScrubber && t.Regex != nil {
			msg = strings.TrimSpace(t.Regex.ReplaceAllString(msg, ""))
		}
	}
	for _, t := range wf.Transformations {
		if t.Kind == TransformExposeLabel && t.Label != "" && sourceRev != "" {
			trailer := fmt.Sprintf("%s: %s", t.Label, sourceRev)
			if !strings.Contains(msg, trailer) {
				msg = msg + "\n\n" + trailer
			}
		}
	}
	return msg + "\n"
}

// SpliceSubtree replaces prefix inside baseTreeOID with subTreeOID using an isolated Git index.
func SpliceSubtree(ctx context.Context, repoDir string, baseTreeOID string, prefix string, subTreeOID string) (string, error) {
	cleaned := strings.Trim(path.Clean(prefix), "/")
	if cleaned == "" || cleaned == "." {
		return "", errors.New("SpliceSubtree: prefix must not be empty")
	}

	raw, err := gitOutput(ctx, repoDir, nil, "ls-tree", "-r", "-z", subTreeOID)
	if err != nil {
		return "", fmt.Errorf("ls-tree %s: %w", subTreeOID, err)
	}
	subEntries, err := parseLSTree(raw)
	if err != nil {
		return "", err
	}

	tmpIdx, cleanup, err := tempIndexFile()
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + tmpIdx, "GIT_NO_LAZY_FETCH=1"}
	if _, err := gitOutput(ctx, repoDir, env, "--no-lazy-fetch", "read-tree", baseTreeOID); err != nil {
		return "", fmt.Errorf("read-tree %s: %w", baseTreeOID, err)
	}
	if _, err := gitOutput(ctx, repoDir, env, "--no-lazy-fetch", "rm", "-r", "--cached", "--ignore-unmatch", "--", cleaned); err != nil {
		return "", fmt.Errorf("rm --cached %s: %w", cleaned, err)
	}

	var buf bytes.Buffer
	for _, e := range subEntries {
		fmt.Fprintf(&buf, "%s %s %s\t%s/%s\n", e.Mode, e.Type, e.OID, cleaned, e.Path)
	}
	if buf.Len() > 0 {
		if _, err := gitOutputWithStdin(ctx, repoDir, env, &buf, "--no-lazy-fetch", "update-index", "--index-info"); err != nil {
			return "", fmt.Errorf("update-index %s: %w", cleaned, err)
		}
	}

	out, err := gitOutput(ctx, repoDir, env, "--no-lazy-fetch", "write-tree", "--missing-ok")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// Identity carries the GitHub-authenticated principals behind a landing request. Commit author,
// committer, and message trailers are self-asserted and are never part of an Identity.
type Identity struct {
	// Requester is the login that pushed the landing ref tip (or the mirror merger for inbound changes).
	Requester string
	// Approvers are logins whose approval is bound to the exact tip being landed.
	Approvers []string
}

// TeamResolver reports whether login is an active member of team ("org/slug"). A nil resolver
// denies every team entry.
type TeamResolver func(team string, login string) (bool, error)

func normalizeLogin(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// CheckOwners verifies hierarchical google3 OWNERS rules between baseTreeOID and newTreeOID.
//
//   - OWNERS files are read only from baseTreeOID, so a change cannot approve itself by adding or
//     editing an OWNERS file. A new directory inherits from its nearest existing ancestor.
//   - An entry "org/team" is resolved through teams; the literal "*" matches nobody.
//   - experimental/<actor>/... is approved for the requester whose login equals <actor>; otherwise the
//     normal OWNERS chain applies.
//   - "set require-review" in any OWNERS file of the chain requires an owner other than the requester.
//   - A path whose OWNERS chain is empty is rejected (fail closed).
func CheckOwners(ctx context.Context, repoDir string, baseTreeOID string, newTreeOID string, id Identity, teams TeamResolver) ([]string, error) {
	raw, err := gitOutput(ctx, repoDir, nil, "diff-tree", "-r", "-z", "--no-commit-id", baseTreeOID, newTreeOID)
	if err != nil {
		return nil, fmt.Errorf("diff-tree %s %s: %w", baseTreeOID, newTreeOID, err)
	}
	entries, err := parseDiffTree(raw)
	if err != nil {
		return nil, err
	}

	ownersCache := make(map[string]*parsedOwnersFile)
	readOwners := func(dir string) *parsedOwnersFile {
		if cached, ok := ownersCache[dir]; ok {
			return cached
		}
		ownersPath := "OWNERS"
		if dir != "" && dir != "." {
			ownersPath = dir + "/OWNERS"
		}
		var parsed *parsedOwnersFile
		if content, err := gitReadTreePath(ctx, repoDir, baseTreeOID, ownersPath); err == nil {
			parsed = parseOwnersContent(string(content))
		}
		ownersCache[dir] = parsed
		return parsed
	}

	requester := normalizeLogin(id.Requester)
	principals := make([]string, 0, 1+len(id.Approvers))
	if requester != "" {
		principals = append(principals, requester)
	}
	var approvers []string
	for _, a := range id.Approvers {
		if n := normalizeLogin(a); n != "" && n != requester {
			approvers = append(approvers, n)
		}
	}
	principals = append(principals, approvers...)

	memberOf := func(owner string, login string) (bool, error) {
		if strings.Contains(owner, "/") {
			if teams == nil {
				return false, nil
			}
			return teams(owner, login)
		}
		return owner == login, nil
	}
	anyOwner := func(owners []string, logins []string) (bool, error) {
		for _, owner := range owners {
			if owner == "*" {
				continue
			}
			for _, login := range logins {
				ok, err := memberOf(owner, login)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
		}
		return false, nil
	}

	var violations []string
	for _, e := range entries {
		if parts := strings.Split(e.Path, "/"); parts[0] == "experimental" && len(parts) >= 3 && requester != "" && normalizeLogin(parts[1]) == requester {
			continue
		}
		chain := ownersChain(e.Path, readOwners)
		if len(chain.owners) == 0 {
			violations = append(violations, fmt.Sprintf("%s: no OWNERS entry covers this path", e.Path))
			continue
		}
		ok, err := anyOwner(chain.owners, principals)
		if err != nil {
			return nil, err
		}
		if ok && chain.requireReview {
			ok, err = anyOwner(chain.owners, approvers)
			if err != nil {
				return nil, err
			}
			if !ok {
				violations = append(violations, fmt.Sprintf("%s: requires review by an owner other than the requester %q (required one of %v)", e.Path, requester, chain.owners))
				continue
			}
		}
		if !ok {
			violations = append(violations, fmt.Sprintf("%s: missing OWNERS approval (requester=%q approvers=%v, required one of %v)", e.Path, requester, approvers, chain.owners))
		}
	}
	sort.Strings(violations)
	return violations, nil
}

type perFileRule struct {
	glob   string
	owners []string
}

type parsedOwnersFile struct {
	noparent      bool
	requireReview bool
	owners        []string
	perFile       []perFileRule
}

func parseOwnersContent(content string) *parsedOwnersFile {
	res := &parsedOwnersFile{}
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if line == "set noparent" {
			res.noparent = true
			continue
		}
		if line == "set require-review" {
			res.requireReview = true
			continue
		}
		if strings.HasPrefix(line, "per-file ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "per-file "))
			eqIdx := strings.IndexByte(rest, '=')
			if eqIdx > 0 {
				glob := strings.TrimSpace(rest[:eqIdx])
				rhs := strings.TrimSpace(rest[eqIdx+1:])
				if rhs == "set noparent" {
					continue
				}
				var ruleOwners []string
				for _, tok := range strings.FieldsFunc(rhs, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
					if tok != "" {
						ruleOwners = append(ruleOwners, strings.ToLower(strings.TrimPrefix(tok, "@")))
					}
				}
				res.perFile = append(res.perFile, perFileRule{glob: glob, owners: ruleOwners})
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && !strings.HasPrefix(fields[0], "@") && (fields[0] == "*" || strings.ContainsAny(fields[0], "*?. /")) {
			var ruleOwners []string
			for _, tok := range fields[1:] {
				ruleOwners = append(ruleOwners, strings.ToLower(strings.TrimPrefix(tok, "@")))
			}
			if fields[0] == "*" {
				res.owners = append(res.owners, ruleOwners...)
			} else {
				res.perFile = append(res.perFile, perFileRule{glob: fields[0], owners: ruleOwners})
			}
			continue
		}
		for _, tok := range fields {
			res.owners = append(res.owners, strings.ToLower(strings.TrimPrefix(tok, "@")))
		}
	}
	return res
}

type ownersChainResult struct {
	owners        []string
	requireReview bool
}

// ownersChain collects the owner set that governs relPath, walking from its directory up to the
// root until a "set noparent" is reached.
func ownersChain(relPath string, readOwners func(dir string) *parsedOwnersFile) ownersChainResult {
	baseName := path.Base(relPath)
	curDir := path.Dir(relPath)
	if curDir == "." {
		curDir = ""
	}

	var res ownersChainResult
	for {
		if pf := readOwners(curDir); pf != nil {
			for _, rule := range pf.perFile {
				if matchGlob(rule.glob, baseName) || matchGlob(rule.glob, relPath) {
					res.owners = append(res.owners, rule.owners...)
				}
			}
			res.owners = append(res.owners, pf.owners...)
			if pf.requireReview {
				res.requireReview = true
			}
			if pf.noparent {
				break
			}
		}
		if curDir == "" {
			break
		}
		parent := path.Dir(curDir)
		if parent == "." {
			curDir = ""
		} else {
			curDir = parent
		}
	}
	return res
}

func gitReadTreePath(ctx context.Context, repoDir string, treeOID string, relPath string) ([]byte, error) {
	if treeOID == "" {
		return nil, errors.New("empty treeOID")
	}
	oidOut, err := gitOutput(ctx, repoDir, nil, "rev-parse", "--verify", "-q", fmt.Sprintf("%s:%s", treeOID, relPath))
	if err != nil {
		return nil, err
	}
	return gitReadBlob(ctx, repoDir, strings.TrimSpace(oidOut))
}

func parseDiffTree(raw string) ([]treeEntry, error) {
	if raw == "" {
		return nil, nil
	}
	records := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")
	out := make([]treeEntry, 0, len(records)/2)
	for i := 0; i+1 < len(records); i += 2 {
		meta := strings.Fields(records[i])
		if len(meta) < 5 {
			return nil, fmt.Errorf("malformed diff-tree metadata %q", records[i])
		}
		mode := meta[1]
		entryType := "blob"
		if mode == "160000" {
			entryType = "commit"
		}
		out = append(out, treeEntry{
			Mode: mode,
			Type: entryType,
			OID:  meta[3],
			Path: records[i+1],
		})
	}
	return out, nil
}

func parseLSTree(raw string) ([]treeEntry, error) {
	if raw == "" {
		return nil, nil
	}
	records := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")
	out := make([]treeEntry, 0, len(records))
	for _, rec := range records {
		if rec == "" {
			continue
		}
		tabIdx := strings.IndexByte(rec, '\t')
		if tabIdx < 0 {
			return nil, fmt.Errorf("malformed ls-tree record %q", rec)
		}
		meta := strings.Fields(rec[:tabIdx])
		if len(meta) != 3 {
			return nil, fmt.Errorf("malformed ls-tree metadata %q", rec[:tabIdx])
		}
		out = append(out, treeEntry{
			Mode: meta[0],
			Type: meta[1],
			OID:  meta[2],
			Path: rec[tabIdx+1:],
		})
	}
	return out, nil
}

func gitReadBlob(ctx context.Context, repoDir string, oid string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "cat-file", "blob", oid)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file blob %s: %w", oid, err)
	}
	return out, nil
}

func gitWriteBlob(ctx context.Context, repoDir string, data []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, bytes.TrimSpace(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func writeIndexTree(ctx context.Context, repoDir string, entries []treeEntry) (string, error) {
	tmpIdx, cleanup, err := tempIndexFile()
	if err != nil {
		return "", err
	}
	defer cleanup()

	var buf bytes.Buffer
	for _, e := range entries {
		fmt.Fprintf(&buf, "%s %s %s\t%s\n", e.Mode, e.Type, e.OID, e.Path)
	}

	env := []string{"GIT_INDEX_FILE=" + tmpIdx, "GIT_NO_LAZY_FETCH=1"}
	if _, err := gitOutputWithStdin(ctx, repoDir, env, &buf, "--no-lazy-fetch", "update-index", "--index-info"); err != nil {
		return "", fmt.Errorf("update-index: %w", err)
	}
	out, err := gitOutput(ctx, repoDir, env, "--no-lazy-fetch", "write-tree", "--missing-ok")
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func tempIndexFile() (string, func(), error) {
	f, err := os.CreateTemp("", "capy-idx-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", nil, err
	}
	return abs, func() { _ = os.Remove(abs) }, nil
}

func gitOutput(ctx context.Context, repoDir string, extraEnv []string, args ...string) (string, error) {
	return gitOutputWithStdin(ctx, repoDir, extraEnv, nil, args...)
}

func gitOutputWithStdin(ctx context.Context, repoDir string, extraEnv []string, stdin *bytes.Buffer, args ...string) (string, error) {
	fullArgs := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return string(out), nil
}
