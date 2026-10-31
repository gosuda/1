package lander

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gosuda.org/0/scripts/capy"
)

// LoadWorkflows evaluates capy.star from the trunk control plane (refs/heads/main), so mirror
// configuration changes take effect without redeploying the lander.
func (l *Lander) LoadWorkflows(ctx context.Context) (*capy.Config, error) {
	blob, err := l.Store.Git(ctx, "cat-file", "blob", trunkHeads+"main:capy.star")
	if err != nil {
		return nil, fmt.Errorf("read capy.star: %w", err)
	}
	dir, err := os.MkdirTemp("", "lander-cfg-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "capy.star")
	if err := os.WriteFile(p, []byte(blob+"\n"), 0o600); err != nil {
		return nil, err
	}
	return capy.LoadConfig(p)
}

// mirrorState is what the lander remembers about one mirror between cycles. It is only an
// optimization: it is lost on restart, which causes one full pass, never a wrong decision.
type mirrorState struct {
	known    bool      // a reconcile completed without error
	settled  bool      // trunk and mirror agree, or were made to agree
	prefix   string    // object id of the workflow's prefix tree in the trunk at that reconcile
	tip      string    // mirror branch tip at that reconcile
	baseline string    // value of refs/suda/sync/<workflow> at that reconcile (empty when absent)
	pulls    string    // signature of the open pull requests at that reconcile
	retryAt  time.Time // when an unsettled or failed mirror is looked at again
}

const mirrorRetry = 2 * time.Minute

// pullSignature reduces open pull requests to a comparable string.
func pullSignature(prs []PullRequest) string {
	parts := make([]string, 0, len(prs))
	for _, p := range prs {
		parts = append(parts, fmt.Sprintf("%d@%s", p.Number, p.HeadSHA))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// ReconcileMirrors reconciles only the mirrors that need it:
//   - the workflow's prefix tree changed in the trunk since its last reconcile,
//   - a wake hint named the mirror,
//   - it has not been reconciled yet, or its last attempt failed or ended unsettled (retried every
//     mirrorRetry), or
//   - on a sweep, a cheap probe (git ls-remote of the branch plus one pull-request listing) shows a
//     different tip or pull-request set.
//
// A steady state therefore costs one rev-parse per mirror per cycle and, per sweep, one probe.
func (l *Lander) ReconcileMirrors(ctx context.Context, sweep bool) error {
	cfg, err := l.LoadWorkflows(ctx)
	if err != nil {
		return err
	}
	repos := make(map[string]bool, len(cfg.Workflows))
	for _, wf := range cfg.Workflows {
		repos[wf.DestinationRepo] = true
	}
	l.mirrorRepos.Store(&repos)

	mainTree, err := l.Store.Tree(ctx, trunkMain)
	if err != nil {
		return err
	}
	now := l.Now()
	l.mu.Lock()
	if l.mirrorStates == nil {
		l.mirrorStates = map[string]*mirrorState{}
	}
	l.mu.Unlock()

	type item struct {
		wf     capy.Workflow
		prefix string
		due    bool
		reason string
	}
	items := make([]*item, 0, len(cfg.Workflows))
	for _, wf := range cfg.Workflows {
		prefix, ok := l.Store.RevParse(ctx, mainTree+":"+wf.OriginPrefix)
		if !ok {
			l.Log.Warn("workflow prefix missing from trunk; skipping", "workflow", wf.Name, "prefix", wf.OriginPrefix)
			continue
		}
		l.mu.Lock()
		st := l.mirrorStates[wf.Name]
		dirty := l.dirtyRepos[wf.DestinationRepo]
		delete(l.dirtyRepos, wf.DestinationRepo)
		l.mu.Unlock()
		var reason string
		switch {
		case dirty:
			reason = "wake"
		case st == nil || !st.known:
			reason = "first-or-retry"
		case st.prefix != prefix:
			reason = "trunk-changed"
		case l.currentBaseline(ctx, wf) != st.baseline:
			reason = "baseline-changed" // deleted or rewritten out of band: self-heal
		case !st.settled && !now.Before(st.retryAt):
			reason = "unsettled-retry"
		}
		items = append(items, &item{wf: wf, prefix: prefix, due: reason != "", reason: reason})
	}

	if sweep {
		// Probe the quiet mirrors concurrently; probing never writes.
		sem := make(chan struct{}, 8)
		var wg sync.WaitGroup
		for _, it := range items {
			if it.due {
				continue
			}
			wg.Add(1)
			go func(it *item) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				l.mu.Lock()
				st := l.mirrorStates[it.wf.Name]
				l.mu.Unlock()
				if l.mirrorChanged(ctx, it.wf, st) {
					it.due, it.reason = true, "sweep"
				}
			}(it)
		}
		wg.Wait()
	}

	if err := l.Store.FetchNoPrune(ctx, l.User, "+refs/ws/capy/*:"+userStaged+"*"); err != nil {
		l.Log.Warn("fetch staged refs failed", "err", err)
	}
	var errs []error
	for _, it := range items {
		if l.halted != nil {
			break
		}
		if !it.due {
			continue
		}
		l.Log.Info("reconciling mirror", "workflow", it.wf.Name, "reason", it.reason)
		if l.onReconcile != nil {
			l.onReconcile(it.wf.Name)
		}
		settled, err := l.reconcile(ctx, it.wf)
		st := &mirrorState{prefix: it.prefix}
		if err != nil {
			l.Log.Error("reconcile failed", "workflow", it.wf.Name, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", it.wf.Name, err))
			st.retryAt = l.Now().Add(mirrorRetry)
		} else {
			st.known, st.settled = true, settled
			st.tip, _ = l.Store.RevParse(ctx, mirrorRef(it.wf, "head"))
			st.baseline = l.currentBaseline(ctx, it.wf)
			if prs, perr := l.Forge.OpenPulls(ctx, it.wf.DestinationRepo); perr == nil {
				st.pulls = pullSignature(prs)
			}
			if !settled {
				st.retryAt = l.Now().Add(mirrorRetry)
			}
		}
		// The prefix may have advanced during the reconcile (an inbound landing).
		if curTree, terr := l.Store.Tree(ctx, trunkMain); terr == nil {
			if cur, ok := l.Store.RevParse(ctx, curTree+":"+it.wf.OriginPrefix); ok {
				st.prefix = cur
			}
		}
		l.mu.Lock()
		l.mirrorStates[it.wf.Name] = st
		l.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (l *Lander) currentBaseline(ctx context.Context, wf capy.Workflow) string {
	v, _ := l.Store.RevParse(ctx, trunkSync+wf.Name)
	return v
}

// mirrorChanged is the cheap sweep probe: has the mirror branch tip or the set of open pull
// requests moved since the last reconcile? Probe failures count as "changed" so they surface.
func (l *Lander) mirrorChanged(ctx context.Context, wf capy.Workflow, st *mirrorState) bool {
	if st == nil || !st.known {
		return true
	}
	remote := l.Mirror(wf.DestinationRepo)
	out, err := l.Store.GitRemote(ctx, remote, Read, "ls-remote", remote.URL, "refs/heads/"+wf.Branch)
	if err != nil {
		return true
	}
	tip, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	if tip != st.tip {
		return true
	}
	prs, err := l.Forge.OpenPulls(ctx, wf.DestinationRepo)
	if err != nil {
		return true
	}
	return pullSignature(prs) != st.pulls
}

func mirrorRef(wf capy.Workflow, suffix string) string { return mirrorNS + wf.Name + "/" + suffix }

var internalTrailerRe = regexp.MustCompile(`(?m)^Suda-[A-Za-z-]+: .*\n?`)

// reconcile implements the decision table of the hardening plan §6.2 over three trees:
// M (export of the current trunk), X (mirror tip, export-normalized) and B (export of the
// trunk at the last sync baseline).
//
// settled reports that the trunk and the mirror agree (or were made to agree). An unsettled mirror
// (no baseline, inbound change deferred or staged, dry run) is retried on a timer.
func (l *Lander) reconcile(ctx context.Context, wf capy.Workflow) (settled bool, err error) {
	s := l.Store
	remote := l.Mirror(wf.DestinationRepo)
	if err := s.Fetch(ctx, remote,
		"+refs/heads/"+wf.Branch+":"+mirrorRef(wf, "head"),
		"+refs/pull/*/head:"+mirrorRef(wf, "pull/")+"*"); err != nil {
		return false, fmt.Errorf("fetch mirror %s: %w", wf.DestinationRepo, err)
	}

	for pass := 0; pass < 2; pass++ {
		done, ok, err := l.reconcilePass(ctx, wf, remote)
		if err != nil || done {
			settled = ok
			if err == nil {
				err = l.stagePulls(ctx, wf)
			}
			return settled, err
		}
	}
	return false, l.stagePulls(ctx, wf)
}

// reconcilePass returns done=false when it advanced the trunk and another pass should run.
func (l *Lander) reconcilePass(ctx context.Context, wf capy.Workflow, remote Remote) (done, settled bool, err error) {
	s := l.Store
	tip, ok := s.RevParse(ctx, mirrorRef(wf, "head"))
	if !ok {
		return true, false, fmt.Errorf("mirror %s has no branch %s", wf.DestinationRepo, wf.Branch)
	}
	histTip, _ := s.RevParse(ctx, trunkHistory)
	mainTree, err := s.Tree(ctx, trunkMain)
	if err != nil {
		return true, false, err
	}
	if _, ok := s.RevParse(ctx, mainTree+":"+wf.OriginPrefix); !ok {
		l.Log.Warn("workflow prefix missing from trunk; skipping", "workflow", wf.Name, "prefix", wf.OriginPrefix)
		return true, true, nil
	}
	M, err := capy.TransformSubtree(ctx, s.Dir, mainTree, wf)
	if err != nil {
		return true, false, err
	}
	xRaw, err := s.Tree(ctx, tip)
	if err != nil {
		return true, false, err
	}
	X, err := capy.NormalizeMirrorTree(ctx, s.Dir, xRaw, wf)
	if err != nil {
		return true, false, err
	}

	base, haveBase := l.readBaseline(ctx, wf)
	if M == X {
		return true, true, l.writeBaseline(ctx, wf, base, haveBase, histTip, tip)
	}
	if !haveBase {
		l.Log.Error("mirror diverged and no sync baseline exists; manual baseline required", "workflow", wf.Name)
		return true, false, nil
	}

	bMonoTree, err := s.Tree(ctx, base.Mono)
	if err != nil {
		return true, false, fmt.Errorf("baseline mono commit %s: %w", base.Mono, err)
	}
	B, err := capy.TransformSubtree(ctx, s.Dir, bMonoTree, wf)
	if err != nil {
		return true, false, err
	}

	if X != B {
		// The mirror changed since the baseline: it must be incorporated before export.
		res, err := l.inbound(ctx, wf, base, bMonoTree, mainTree, tip, xRaw)
		if err != nil {
			return true, false, err
		}
		switch res {
		case inboundLanded:
			return false, false, nil // re-evaluate against the advanced trunk
		case inboundPending:
			return true, false, nil
		}
	}
	if l.DryRun {
		return true, false, l.export(ctx, wf, remote, base, haveBase, histTip, tip, M)
	}
	return true, true, l.export(ctx, wf, remote, base, haveBase, histTip, tip, M)
}

type inboundResult int

const (
	// inboundContained: the trunk already holds the mirror's changes; export may proceed.
	inboundContained inboundResult = iota
	// inboundLanded: an inbound landing advanced the trunk; the caller re-evaluates.
	inboundLanded
	// inboundPending: the change is deferred or staged for a member; nothing is exported.
	inboundPending
)

// inbound incorporates mirror changes into the trunk.
func (l *Lander) inbound(ctx context.Context, wf capy.Workflow, base capy.SyncBaseline, bMonoTree, mainTree, tip, xRaw string) (inboundResult, error) {
	s := l.Store
	if !wf.Invertible() {
		l.Log.Warn("workflow is export-only (content transforms); mirror changes need manual import", "workflow", wf.Name)
		return inboundPending, l.stageInbound(ctx, wf, base, bMonoTree, tip, "main")
	}
	imp, err := capy.ImportTree(ctx, s.Dir, bMonoTree, wf, xRaw)
	if err != nil {
		return inboundPending, err
	}
	T := imp
	if mainTree != bMonoTree {
		out, mergeErr := s.Git(ctx, "merge-tree", "--write-tree", "--name-only", "--merge-base="+bMonoTree, mainTree, imp)
		if mergeErr != nil {
			var ge *GitError
			if errors.As(mergeErr, &ge) && ge.ExitCode() == 1 {
				l.Log.Warn("inbound conflict; staging for manual resolution", "workflow", wf.Name, "detail", truncate(out, 300))
				return inboundPending, l.stageInbound(ctx, wf, base, bMonoTree, tip, "main")
			}
			return inboundPending, mergeErr
		}
		T, _, _ = strings.Cut(out, "\n")
	}
	if T == mainTree {
		return inboundContained, nil
	}

	req, err := l.inboundRequest(ctx, wf, bMonoTree, imp, tip)
	if err != nil {
		return inboundPending, err
	}
	if req.Requester == "" {
		return inboundPending, nil // deferred: merger not visible yet
	}
	res, err := l.Land(ctx, req)
	if err != nil {
		return inboundPending, err
	}
	l.Log.Info("inbound landing processed", "workflow", wf.Name, "outcome", res.Outcome.String(), "reason", truncate(res.Reason, 200))
	switch res.Outcome {
	case Landed:
		return inboundLanded, nil
	case AlreadyLanded:
		return inboundContained, nil
	case Rejected:
		return inboundPending, l.stageInbound(ctx, wf, base, bMonoTree, tip, "main")
	}
	return inboundPending, nil
}

// inboundRequest builds the synthetic landing request for a mirror tip: a parentless root with
// the baseline trunk tree and a tip carrying the imported prefix.
func (l *Lander) inboundRequest(ctx context.Context, wf capy.Workflow, bMonoTree, imp, tip string) (Request, error) {
	s := l.Store
	root, err := s.CommitTree(ctx, bMonoTree, nil, "suda: inbound base\n", nil)
	if err != nil {
		return Request{}, err
	}
	meta, err := s.Meta(ctx, tip)
	if err != nil {
		return Request{}, err
	}
	msg := strings.TrimRight(internalTrailerRe.ReplaceAllString(meta.Message, ""), "\n") +
		fmt.Sprintf("\n\nSuda-Inbound-From: %s@%s\n", wf.DestinationRepo, tip)
	work, err := s.CommitTree(ctx, imp, []string{root}, msg, &meta)
	if err != nil {
		return Request{}, err
	}
	req := Request{ID: "inbound-" + wf.Name, Tip: work}
	push, err := l.Forge.PushForTip(ctx, wf.DestinationRepo, "refs/heads/"+wf.Branch, tip)
	if err != nil {
		l.Log.Warn("mirror pusher unresolved; deferring inbound", "workflow", wf.Name, "tip", tip, "err", err)
		return req, nil
	}
	req.Requester = push.Actor
	req.Pushed = push.Time
	return req, nil
}

// stageInbound publishes the import for manual landing as refs/ws/capy/<workflow>-<suffix>.
func (l *Lander) stageInbound(ctx context.Context, wf capy.Workflow, base capy.SyncBaseline, bMonoTree, tip, suffix string) error {
	if !wf.Invertible() {
		return nil
	}
	s := l.Store
	meta, err := s.Meta(ctx, tip)
	if err != nil {
		return err
	}
	xRaw, err := s.Tree(ctx, tip)
	if err != nil {
		return err
	}
	imp, err := capy.ImportTree(ctx, s.Dir, bMonoTree, wf, xRaw)
	if err != nil {
		return err
	}
	return l.publishStaged(ctx, wf.Name+"-"+suffix, bMonoTree, imp, meta, wf.DestinationRepo+"@"+tip)
}

func (l *Lander) publishStaged(ctx context.Context, name, bMonoTree, imp string, meta CommitMeta, origin string) error {
	s := l.Store
	if l.DryRun {
		l.Log.Info("dry-run: would stage", "ref", "refs/ws/capy/"+name)
		return nil
	}
	if old, ok := s.RevParse(ctx, userStaged+name); ok {
		if m, err := s.Meta(ctx, old); err == nil && strings.Contains(m.Message, "Suda-Inbound-From: "+origin+"\n") {
			return nil // already staged for this mirror state
		}
	}
	root, err := s.CommitTree(ctx, bMonoTree, nil, "suda: staged base\n", nil)
	if err != nil {
		return err
	}
	msg := strings.TrimRight(internalTrailerRe.ReplaceAllString(meta.Message, ""), "\n") + "\n\nSuda-Inbound-From: " + origin + "\n"
	work, err := s.CommitTree(ctx, imp, []string{root}, msg, &meta)
	if err != nil {
		return err
	}
	if err := s.Push(ctx, l.User, Write, false, nil, "+"+work+":refs/ws/capy/"+name); err != nil {
		return fmt.Errorf("stage %s: %w", name, err)
	}
	_, _ = s.Git(ctx, "update-ref", userStaged+name, work)
	l.Log.Info("staged external change", "ref", "refs/ws/capy/"+name, "origin", origin)
	return nil
}

// stagePulls publishes every open mirror pull request as refs/ws/capy/<workflow>-pr-<N>.
func (l *Lander) stagePulls(ctx context.Context, wf capy.Workflow) error {
	if !wf.Invertible() {
		return nil
	}
	s := l.Store
	base, haveBase := l.readBaseline(ctx, wf)
	if !haveBase {
		return nil
	}
	bMonoTree, err := s.Tree(ctx, base.Mono)
	if err != nil {
		return nil
	}
	prs, err := l.Forge.OpenPulls(ctx, wf.DestinationRepo)
	if err != nil {
		return fmt.Errorf("list pull requests of %s: %w", wf.DestinationRepo, err)
	}
	var errs []error
	for _, pr := range prs {
		head, ok := s.RevParse(ctx, mirrorRef(wf, fmt.Sprintf("pull/%d", pr.Number)))
		if !ok || head != pr.HeadSHA {
			continue // fetched state lags the API; next cycle
		}
		meta, err := s.Meta(ctx, head)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		xRaw, err := s.Tree(ctx, head)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		imp, err := capy.ImportTree(ctx, s.Dir, bMonoTree, wf, xRaw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		origin := fmt.Sprintf("%s#%d@%s", wf.DestinationRepo, pr.Number, head)
		if err := l.publishStaged(ctx, fmt.Sprintf("%s-pr-%d", wf.Name, pr.Number), bMonoTree, imp, meta, origin); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// export pushes the trunk's export tree on top of the mirror tip, guarded by a lease on that tip.
func (l *Lander) export(ctx context.Context, wf capy.Workflow, remote Remote, base capy.SyncBaseline, haveBase bool, histTip, tip, M string) error {
	s := l.Store
	if l.DryRun {
		l.Log.Info("dry-run: would export", "workflow", wf.Name, "tree", M)
		return nil
	}
	raw, meta, err := l.exportMessage(ctx, wf, base, haveBase, histTip)
	if err != nil {
		return err
	}
	msg := capy.FormatCommitMessage(raw, wf, histTip)
	commit, err := s.CommitTree(ctx, M, []string{tip}, msg, &meta)
	if err != nil {
		return err
	}
	ref := "refs/heads/" + wf.Branch
	if err := s.Push(ctx, remote, WriteWorkflows, false, []string{ref + ":" + tip}, commit+":"+ref); err != nil {
		return fmt.Errorf("export to %s: %w", wf.DestinationRepo, err)
	}
	_, _ = s.Git(ctx, "update-ref", mirrorRef(wf, "head"), commit)
	l.Log.Info("exported to mirror", "workflow", wf.Name, "commit", commit)
	return l.writeBaseline(ctx, wf, base, haveBase, histTip, commit)
}

// exportMessage squashes the trunk commits since the baseline that touched the prefix.
func (l *Lander) exportMessage(ctx context.Context, wf capy.Workflow, base capy.SyncBaseline, haveBase bool, histTip string) (string, CommitMeta, error) {
	s := l.Store
	rng := []string{histTip, "-n", "1"}
	if haveBase {
		rng = []string{base.Mono + ".." + histTip}
	}
	args := append([]string{"log", "--format=%H"}, rng...)
	args = append(args, "--", wf.OriginPrefix)
	out, err := s.Git(ctx, args...)
	if err != nil {
		return "", CommitMeta{}, err
	}
	commits := strings.Fields(out)
	if len(commits) == 0 {
		commits = []string{histTip}
	}
	newest, err := s.Meta(ctx, commits[0])
	if err != nil {
		return "", CommitMeta{}, err
	}
	clean := func(m string) string { return strings.TrimSpace(internalTrailerRe.ReplaceAllString(m, "")) }
	if len(commits) == 1 {
		return clean(newest.Message), newest, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: sync %d monorepo changes\n\n", wf.OriginPrefix, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		m, err := s.Meta(ctx, commits[i])
		if err != nil {
			return "", CommitMeta{}, err
		}
		subject, _, _ := strings.Cut(clean(m.Message), "\n")
		fmt.Fprintf(&b, "- %s\n", subject)
	}
	return b.String(), newest, nil
}

func (l *Lander) readBaseline(ctx context.Context, wf capy.Workflow) (capy.SyncBaseline, bool) {
	c, ok := l.Store.RevParse(ctx, trunkSync+wf.Name)
	if !ok {
		return capy.SyncBaseline{}, false
	}
	m, err := l.Store.Meta(ctx, c)
	if err != nil {
		return capy.SyncBaseline{}, false
	}
	b, err := capy.ParseBaselineMessage(m.Message)
	if err != nil {
		return capy.SyncBaseline{}, false
	}
	return b, true
}

// writeBaseline records (histTip, mirrorCommit) as the last agreement point when it changed.
func (l *Lander) writeBaseline(ctx context.Context, wf capy.Workflow, old capy.SyncBaseline, haveOld bool, histTip, mirrorCommit string) error {
	if haveOld && old.Mono == histTip && old.Mirror == mirrorCommit {
		return nil
	}
	if l.DryRun {
		l.Log.Info("dry-run: would write sync baseline", "workflow", wf.Name)
		return nil
	}
	s := l.Store
	// The baseline lives in its message. Its tree is empty on purpose: an export tree carries the
	// project's root .github/workflows, and GitHub refuses ref updates containing workflow files
	// unless the token has the workflows permission.
	empty, err := s.BuildTree(ctx, nil)
	if err != nil {
		return err
	}
	commit, err := s.CommitTree(ctx, empty, nil, capy.FormatBaselineMessage(capy.SyncBaseline{Workflow: wf.Name, Mono: histTip, Mirror: mirrorCommit}), nil)
	if err != nil {
		return err
	}
	ref := "refs/suda/sync/" + wf.Name
	lease := ref + ":"
	if prev, ok := s.RevParse(ctx, trunkSync+wf.Name); ok {
		lease += prev
	}
	if err := s.Push(ctx, l.Trunk, Write, false, []string{lease}, commit+":"+ref); err != nil {
		return fmt.Errorf("write baseline %s: %w", wf.Name, err)
	}
	_, _ = s.Git(ctx, "update-ref", trunkSync+wf.Name, commit)
	return nil
}
