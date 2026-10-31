package lander

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gosuda.org/0/scripts/capy"
)

// Every open mirror pull request that is staged as refs/ws/capy/<wf>-pr-<N> also gets a review pull
// request on the user repository: base/<id> carries the staged base snapshot and review/<id> the staged
// commit, so GitHub shows exactly the contributor's change and approvals are bound to the review tip.
// The id is "capy-<wf>-pr-<N>". The lander owns these branches and pull requests: it updates them when
// the mirror pull request changes and closes them when it no longer applies.
const (
	reviewIDPrefix = "capy-"
	// maxReviewPulls bounds the open review pull requests the lander keeps. Anyone can open a pull
	// request on a public mirror, so the lander must not mirror an unbounded number of them.
	maxReviewPulls = 40
	maxTitleRunes  = 200
)

func reviewPullID(wf capy.Workflow, number int) string {
	return fmt.Sprintf("%s%s-pr-%d", reviewIDPrefix, wf.Name, number)
}

// reviewPullTitle prefixes the contributor's title with the mirror and number.
func reviewPullTitle(wf capy.Workflow, pr PullRequest) string {
	title := strings.Join(strings.FieldsFunc(pr.Title, func(r rune) bool { return r < ' ' || r == 0x7f }), " ")
	title = strings.TrimSpace(title)
	if title == "" {
		title = "(untitled)"
	}
	return truncateRunes(fmt.Sprintf("[%s#%d] %s", wf.Name, pr.Number, title), maxTitleRunes)
}

// reviewPullBody explains the import and how to land it. Everything taken from the mirror pull request
// (author, address, head) is shown as code or a link and never interpreted as markup.
func (l *Lander) reviewPullBody(wf capy.Workflow, pr PullRequest, id string) string {
	var b strings.Builder
	author := strings.Map(func(r rune) rune {
		if r == '`' || r < ' ' {
			return -1
		}
		return r
	}, pr.Author)
	if author == "" {
		author = "unknown"
	}
	head := pr.HeadSHA
	if len(head) > 7 {
		head = head[:7]
	}
	fmt.Fprintf(&b, "Imported from the mirror pull request %s#%d by `%s` (head `%s`).\n", wf.DestinationRepo, pr.Number, author, head)
	if pr.URL != "" {
		fmt.Fprintf(&b, "Original: %s\n", pr.URL)
	}
	if l.MagpieURL != "" {
		fmt.Fprintf(&b, "\nDiff of this change only: %s/diff/heads/base/%s..heads/review/%s\n",
			strings.TrimRight(l.MagpieURL, "/"), id, id)
	}
	fmt.Fprintf(&b, `
To land it, review it here (approvals bound to the exact tip count for `+"`OWNERS`"+`), then request the landing:

`+"```sh"+`
git fetch ws review/%[1]s
git push ws FETCH_HEAD:refs/heads/land/%[1]s
`+"```"+`

The lander exports the result to %[2]s and closes the original pull request. This pull request is
managed by the lander: it follows the original, and it is closed when the original is closed or no longer
applies. Do not merge it on GitHub.
`, id, wf.DestinationRepo)
	return b.String()
}

// syncReviewPull publishes base/<id> and review/<id> for a staged mirror pull request and opens or
// refreshes its review pull request. open is the lander's view of the open pull requests of the user
// repository and is updated in place.
func (l *Lander) syncReviewPull(ctx context.Context, wf capy.Workflow, pr PullRequest, open *[]PullRequest) error {
	if l.DryRun {
		return nil
	}
	s := l.Store
	name := fmt.Sprintf("%s-pr-%d", wf.Name, pr.Number)
	work, ok := s.RevParse(ctx, userStaged+name)
	if !ok {
		return nil
	}
	root, ok := s.RevParse(ctx, work+"^")
	if !ok {
		return nil
	}
	id := reviewPullID(wf, pr.Number)
	headRef := "review/" + id

	var existing *PullRequest
	capyOpen := 0
	for i := range *open {
		p := &(*open)[i]
		if p.HeadRef == headRef {
			existing = p
		}
		if strings.HasPrefix(p.HeadRef, "review/"+reviewIDPrefix) {
			capyOpen++
		}
	}
	if existing == nil && capyOpen >= maxReviewPulls {
		l.Log.Warn("too many open review pull requests; not opening another", "workflow", wf.Name, "pull", pr.Number, "limit", maxReviewPulls)
		return nil
	}

	curReview, _ := s.RevParse(ctx, userReview+id)
	curBase, _ := s.RevParse(ctx, userBase+id)
	moved := curReview != work
	if moved || curBase != root {
		if err := s.Push(ctx, l.User, Write, false, nil,
			"+"+root+":refs/heads/base/"+id, "+"+work+":refs/heads/review/"+id); err != nil {
			return fmt.Errorf("publish review branches %s: %w", id, err)
		}
		_, _ = s.Git(ctx, "update-ref", userBase+id, root)
		_, _ = s.Git(ctx, "update-ref", userReview+id, work)
	}

	title := reviewPullTitle(wf, pr)
	body := l.reviewPullBody(wf, pr, id)
	switch {
	case existing == nil:
		n, err := l.Forge.CreatePull(ctx, l.User.Repo, headRef, "base/"+id, title, body)
		if err != nil {
			return fmt.Errorf("open review pull request %s: %w", id, err)
		}
		*open = append(*open, PullRequest{Number: n, HeadRef: headRef, HeadSHA: work, Title: title})
		l.Log.Info("opened review pull request", "id", id, "number", n)
	case moved || existing.Title != title:
		if err := l.Forge.UpdatePull(ctx, l.User.Repo, existing.Number, title, body); err != nil {
			return fmt.Errorf("update review pull request %s: %w", id, err)
		}
		existing.Title, existing.HeadSHA = title, work
	}
	return nil
}

// closeStaleReviewPulls closes the open review pull requests of wf whose mirror pull request is not in
// keep: it was closed, or it no longer applies to the mirror.
func (l *Lander) closeStaleReviewPulls(ctx context.Context, wf capy.Workflow, open []PullRequest, keep map[int]bool) error {
	if l.DryRun {
		return nil
	}
	prefix := "review/" + reviewIDPrefix + wf.Name + "-pr-"
	var errs []error
	for _, p := range open {
		rest, ok := strings.CutPrefix(p.HeadRef, prefix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(rest)
		if err != nil || keep[n] {
			continue
		}
		comment := fmt.Sprintf("The mirror pull request %s#%d was closed or no longer applies to %s, so the lander retired this import.",
			wf.DestinationRepo, n, wf.DestinationRepo)
		if err := l.Forge.ClosePull(ctx, l.User.Repo, p.Number, comment); err != nil {
			errs = append(errs, fmt.Errorf("close review pull request %s: %w", p.HeadRef, err))
			continue
		}
		l.Log.Info("closed review pull request", "ref", p.HeadRef, "number", p.Number)
	}
	return errors.Join(errs...)
}
