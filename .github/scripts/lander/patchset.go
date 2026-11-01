package lander

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Changes and patchsets.
//
// A change is identified by the Change-Id trailer of its tip commit ("Change-Id: I" and 40 hex
// digits), which survives amends, rebases, and renamed branches. A patchset is one tip of the
// change that was sent out for review: `git ws-pr` pushes it to the immutable reference
// refs/ws/cl/<change-id>/<tip> of the user repository next to the review branch, so the commit a
// reviewer approved stays available after review/<id> moves on.
//
// An approval is bound by GitHub to a commit. It counts for a landing tip when that commit is the
// tip, or when the commit makes the same patch to its own base snapshot (a trivial rebase): the
// reviewer looked at exactly this diff. Anything else - a different diff, a whitespace edit, a
// changed mode, a conflict resolved differently, a patchset the lander cannot read - needs a new
// approval. Approvals are read from the pull requests of review/<id> and of every other review
// branch whose tip carries the same Change-Id, so renaming the workspace does not lose them.
// OWNERS are still evaluated against the trunk at landing time, never against the approved commit.

// maxCarriedApprovals bounds how many approvals of other commits are examined per landing request.
const maxCarriedApprovals = 8

// maxReviewBranchScan bounds how many review branches are read when looking for a change by id.
const maxReviewBranchScan = 200

var changeIDRe = regexp.MustCompile(`^Change-Id: (I[0-9a-f]{40})$`)

// ChangeID returns the Change-Id trailer of a commit message: the last one in the final
// paragraph, or "" when the message has none. A line elsewhere in the message is not a trailer.
func ChangeID(msg string) string {
	msg = strings.TrimRight(msg, " \t\r\n")
	i := strings.LastIndex(msg, "\n\n")
	if i < 0 {
		return ""
	}
	id := ""
	for _, line := range strings.Split(msg[i+2:], "\n") {
		if m := changeIDRe.FindStringSubmatch(strings.TrimRight(line, " \t\r")); m != nil {
			id = m[1]
		}
	}
	return id
}

// patchCache computes and remembers, for the commits of one landing request, the patch each
// makes to its own root snapshot.
type patchCache struct {
	s     *Store
	paths map[string]string // commit -> changed paths with status
	ids   map[string]string // commit -> patch id
}

func newPatchCache(s *Store) *patchCache {
	return &patchCache{s: s, paths: map[string]string{}, ids: map[string]string{}}
}

var errNoPatch = errors.New("commit makes no change to its root snapshot")

// root returns the single parentless commit below commit: the trunk snapshot a workspace was
// started from.
func (p *patchCache) root(ctx context.Context, commit string) (string, error) {
	if _, ok := p.s.RevParse(ctx, commit+"^{commit}"); !ok {
		return "", fmt.Errorf("commit %s is not in the store", commit)
	}
	roots, err := p.s.Git(ctx, "rev-list", "--max-parents=0", commit)
	if err != nil {
		return "", err
	}
	if f := strings.Fields(roots); len(f) == 1 {
		return f[0], nil
	}
	return "", fmt.Errorf("commit %s has %d root commits, want 1", commit, len(strings.Fields(roots)))
}

// changed lists the paths a commit changes against its root with their status, a cheap necessary
// condition for two commits to carry the same patch.
func (p *patchCache) changed(ctx context.Context, commit string) (string, error) {
	if v, ok := p.paths[commit]; ok {
		return v, nil
	}
	root, err := p.root(ctx, commit)
	if err != nil {
		return "", err
	}
	out, err := p.s.Git(ctx, "diff-tree", "-r", "--name-status", "--no-renames", "--no-ext-diff", root, commit)
	if err != nil {
		return "", err
	}
	p.paths[commit] = out
	return out, nil
}

// id returns the patch id of a commit's change to its root: git patch-id --verbatim over the full
// binary diff. Verbatim keeps whitespace significant (a whitespace edit is a different patch);
// hunk line numbers and blob ids are ignored, so a clean rebase onto a newer snapshot keeps the
// id while file names, modes, content, and binary payloads all take part in it.
func (p *patchCache) id(ctx context.Context, commit string) (string, error) {
	if v, ok := p.ids[commit]; ok {
		return v, nil
	}
	root, err := p.root(ctx, commit)
	if err != nil {
		return "", err
	}
	// Plumbing never runs external diff drivers or textconv filters; both are also disabled
	// explicitly because the lander must never execute repository-controlled commands.
	diff, err := p.s.Git(ctx, "diff-tree", "-p", "--binary", "--no-renames", "--no-ext-diff", "--no-textconv", root, commit)
	if err != nil {
		return "", err
	}
	out, err := p.s.GitStdin(ctx, diff+"\n", "patch-id", "--verbatim")
	if err != nil {
		return "", err
	}
	id, _, _ := strings.Cut(out, " ")
	if id == "" {
		return "", errNoPatch
	}
	p.ids[commit] = id
	return id, nil
}

// same reports whether a and b make the same patch to their own roots.
func (p *patchCache) same(ctx context.Context, a, b string) bool {
	pa, err := p.changed(ctx, a)
	if err != nil {
		return false
	}
	pb, err := p.changed(ctx, b)
	if err != nil || pa != pb {
		return false
	}
	ia, err := p.id(ctx, a)
	if err != nil {
		return false
	}
	ib, err := p.id(ctx, b)
	return err == nil && ia == ib
}

// reviewHeads returns the review branch ids whose pull requests can approve tip of landing
// request id: review/<id> when it exists, and every other review branch in the store whose tip
// carries the same Change-Id.
func (l *Lander) reviewHeads(ctx context.Context, id, tip string) []string {
	var heads []string
	if _, ok := l.Store.RevParse(ctx, userReview+id); ok {
		heads = append(heads, id)
	}
	meta, err := l.Store.Meta(ctx, tip)
	if err != nil {
		return heads
	}
	change := ChangeID(meta.Message)
	if change == "" {
		return heads
	}
	refs, err := l.Store.Git(ctx, "for-each-ref", "--format=%(refname)", strings.TrimSuffix(userReview, "/"))
	if err != nil {
		return heads
	}
	for n, ref := range strings.Fields(refs) {
		other := strings.TrimPrefix(ref, userReview)
		if other == id || n >= maxReviewBranchScan {
			continue
		}
		sha, ok := l.Store.RevParse(ctx, ref)
		if !ok {
			continue
		}
		if m, err := l.Store.Meta(ctx, sha); err == nil && ChangeID(m.Message) == change {
			heads = append(heads, other)
		}
	}
	return heads
}

// approvalsFor returns the reviewers whose approval counts for tip of landing request id, and the
// number of the review pull request (0 when there is none). Unavailable approvals are logged and
// skipped, as a landing request never waits for GitHub's review API.
func (l *Lander) approvalsFor(ctx context.Context, id, tip string) (approvers []string, pull int) {
	var pc *patchCache
	counted := map[string]bool{}
	examined := 0
	for _, head := range l.reviewHeads(ctx, id, tip) {
		rs, err := l.Forge.ReviewApprovals(ctx, l.User.Repo, "review/"+head)
		if err != nil {
			l.Log.Warn("approvals unavailable; continuing without", "id", id, "head", "review/"+head, "err", err)
			continue
		}
		if pull == 0 {
			pull = rs.Pull
		}
		for _, a := range rs.Approvals {
			if counted[a.Login] {
				continue
			}
			if a.Commit != tip {
				if examined >= maxCarriedApprovals {
					continue
				}
				examined++
				if pc == nil {
					pc = newPatchCache(l.Store)
				}
				if !pc.same(ctx, a.Commit, tip) {
					continue
				}
				l.Log.Info("approval carried to an equivalent patchset", "id", id, "reviewer", a.Login, "approved", a.Commit, "tip", tip)
			}
			counted[a.Login] = true
			approvers = append(approvers, a.Login)
		}
	}
	return approvers, pull
}
