package lander

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gosuda.org/0/scripts/capy"
)

// Lander is the single writer of the trunk repository.
type Lander struct {
	Config
	halted error
	lastGC time.Time
	// lastSweep is when mirrors were last probed for changes (see ReconcileMirrors).
	lastSweep   time.Time
	mirrorRepos atomic.Pointer[map[string]bool]

	mu           sync.Mutex
	mirrorStates map[string]*mirrorState
	dirtyRepos   map[string]bool
	onReconcile  func(workflow string) // test hook
}

// New validates cfg and returns a Lander.
func New(cfg Config) (*Lander, error) {
	if cfg.Store == nil || cfg.Forge == nil {
		return nil, errors.New("lander: Store and Forge are required")
	}
	if cfg.Trunk.URL == "" || cfg.User.URL == "" {
		return nil, errors.New("lander: Trunk and User remotes are required")
	}
	cfg.defaults()
	return &Lander{Config: cfg}, nil
}

// Outcome classifies the result of processing one landing request.
type Outcome int

const (
	// Landed means the trunk advanced.
	Landed Outcome = iota
	// AlreadyLanded means the trunk already contains the change.
	AlreadyLanded
	// Rejected means policy or merge failed; the request is closed.
	Rejected
	// Deferred means the request cannot be judged yet and is retried next cycle.
	Deferred
)

func (o Outcome) String() string {
	return [...]string{"landed", "already-landed", "rejected", "deferred"}[o]
}

// Request is one candidate change against the trunk.
type Request struct {
	ID        string // task id; also names the land ref
	LandRef   string // full ref in the user repository, empty for synthetic inbound requests
	Tip       string // commit in the store
	Requester string
	Approvers []string
	Pushed    time.Time
	// ExtraTrailers are appended to the landed commit message.
	ExtraTrailers []string
}

// Result reports the outcome of a landing attempt.
type Result struct {
	Outcome Outcome
	Reason  string // rejection reason or deferral cause
	Hist    string // history commit for Landed
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// fetchTrunk refreshes the trunk namespaces.
func (l *Lander) fetchTrunk(ctx context.Context) error {
	return l.Store.Fetch(ctx, l.Trunk,
		"+refs/heads/*:"+trunkHeads+"*",
		"+refs/suda/*:"+trunkSuda+"*")
}

// fetchUser refreshes the user-repository namespaces the lander reads.
func (l *Lander) fetchUser(ctx context.Context) error {
	if err := l.Store.Fetch(ctx, l.User,
		"+refs/heads/land/*:"+userLand+"*",
		"+refs/heads/review/*:"+userReview+"*",
		"+refs/heads/base/*:"+userBase+"*",
		"+refs/suda/*:"+userSuda+"*"); err != nil {
		return err
	}
	// An explicit refspec fails when the branch does not exist yet (fresh repository).
	if err := l.Store.FetchNoPrune(ctx, l.User, "+refs/heads/main:"+userMain); err != nil && !strings.Contains(err.Error(), "couldn't find remote ref") {
		return err
	}
	return nil
}

// DrainLandRequests processes every pending landing ref in activity order.
func (l *Lander) DrainLandRequests(ctx context.Context) error {
	if err := l.fetchUser(ctx); err != nil {
		return fmt.Errorf("fetch user repo: %w", err)
	}
	refs, err := l.Store.Git(ctx, "for-each-ref", "--format=%(refname) %(objectname)", strings.TrimSuffix(userLand, "/"))
	if err != nil {
		return err
	}
	var reqs []Request
	for _, line := range strings.Split(refs, "\n") {
		ref, sha, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		id := strings.TrimPrefix(ref, userLand)
		landRef := "refs/heads/land/" + id
		push, err := l.Forge.PushForTip(ctx, l.User.Repo, landRef, sha)
		if err != nil {
			l.Log.Warn("land request deferred: pusher unresolved", "ref", landRef, "tip", sha, "err", err)
			continue
		}
		req := Request{ID: id, LandRef: landRef, Tip: sha, Requester: push.Actor, Pushed: push.Time}
		if _, ok := l.Store.RevParse(ctx, userReview+id); ok {
			if rv, _ := l.Store.RevParse(ctx, userReview+id); rv == sha {
				req.Approvers, err = l.Forge.ApproversForTip(ctx, l.User.Repo, "review/"+id, sha)
				if err != nil {
					l.Log.Warn("approvals unavailable; continuing without", "id", id, "err", err)
				}
			}
		}
		reqs = append(reqs, req)
	}
	sort.SliceStable(reqs, func(i, j int) bool {
		if !reqs[i].Pushed.Equal(reqs[j].Pushed) {
			return reqs[i].Pushed.Before(reqs[j].Pushed)
		}
		return reqs[i].ID < reqs[j].ID
	})
	var errs []error
	for _, req := range reqs {
		if l.halted != nil {
			break
		}
		res, err := l.Land(ctx, req)
		if err != nil {
			l.Log.Error("landing failed", "id", req.ID, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", req.ID, err))
			continue
		}
		l.Log.Info("landing processed", "id", req.ID, "requester", req.Requester, "outcome", res.Outcome.String(), "reason", truncate(res.Reason, 200))
		if err := l.finishRequest(ctx, req, res); err != nil {
			errs = append(errs, fmt.Errorf("%s: finish: %w", req.ID, err))
		}
	}
	return errors.Join(errs...)
}

// finishRequest performs the idempotent post-processing of a processed land ref.
func (l *Lander) finishRequest(ctx context.Context, req Request, res Result) error {
	if res.Outcome == Deferred || req.LandRef == "" || l.DryRun {
		return nil
	}
	var errs []error
	state, desc := "success", "landed as "+res.Hist
	switch res.Outcome {
	case AlreadyLanded:
		desc = "already contained in the trunk"
	case Rejected:
		state, desc = "failure", truncate(res.Reason, 140)
	}
	if err := l.Forge.SetStatus(ctx, l.User.Repo, req.Tip, Status{State: state, Context: "suda/land", Description: desc}); err != nil {
		errs = append(errs, fmt.Errorf("status: %w", err))
	}
	leases := []string{req.LandRef + ":" + req.Tip}
	if err := l.Store.Push(ctx, l.User, Write, false, leases, ":"+req.LandRef); err != nil {
		errs = append(errs, fmt.Errorf("delete land ref: %w", err))
	}
	if res.Outcome != Rejected {
		for _, ns := range []string{"base", "review"} {
			ref := "refs/heads/" + ns + "/" + req.ID
			if sha, ok := l.Store.RevParse(ctx, "refs/user/"+ns+"/"+req.ID); ok {
				_ = l.Store.Push(ctx, l.User, Write, false, []string{ref + ":" + sha}, ":"+ref)
			}
		}
	}
	return errors.Join(errs...)
}

// Land evaluates req against the trunk and, when it passes every check, advances the trunk
// atomically. A lease failure means a break-glass push moved a trunk ref; the attempt is
// repeated from fresh state a bounded number of times.
func (l *Lander) Land(ctx context.Context, req Request) (Result, error) {
	var lastErr error
	for attempt := 1; attempt <= l.MaxLandAttempts; attempt++ {
		if attempt > 1 {
			if err := l.fetchTrunk(ctx); err != nil {
				return Result{}, err
			}
		}
		res, retry, err := l.landOnce(ctx, req)
		if err == nil && !retry {
			return res, nil
		}
		lastErr = err
		l.Log.Warn("land attempt failed", "id", req.ID, "attempt", attempt, "err", err)
	}
	if lastErr != nil && l.halted == nil {
		l.halted = fmt.Errorf("trunk moved repeatedly while landing %s: %w", req.ID, lastErr)
	}
	return Result{}, lastErr
}

func (l *Lander) landOnce(ctx context.Context, req Request) (res Result, retry bool, err error) {
	s := l.Store
	oldMain, ok := s.RevParse(ctx, trunkMain)
	if !ok {
		return Result{}, false, errors.New("trunk refs/suda/main missing")
	}
	oldHist, ok := s.RevParse(ctx, trunkHistory)
	if !ok {
		return Result{}, false, errors.New("trunk refs/suda/history missing")
	}
	oldCtrl, haveCtrl := s.RevParse(ctx, trunkHeads+"main")

	reject := func(format string, a ...any) (Result, bool, error) {
		return Result{Outcome: Rejected, Reason: fmt.Sprintf(format, a...)}, false, nil
	}

	if msg, _ := s.Meta(ctx, req.Tip); isEmbargoName(req.ID) || isEmbargoSubject(msg.Message) {
		allowed := false
		if l.EmbargoTeam != "" {
			allowed, err = l.Forge.TeamResolver(ctx)(l.EmbargoTeam, strings.ToLower(req.Requester))
			if err != nil {
				return Result{}, false, err
			}
		}
		if !allowed {
			// Generic on purpose: statuses are visible to every organization member.
			return reject("embargoed change: only the security response team may land it")
		}
	}

	roots, err := s.Git(ctx, "rev-list", "--max-parents=0", req.Tip)
	if err != nil {
		return Result{}, false, err
	}
	if f := strings.Fields(roots); len(f) != 1 {
		return reject("workspace must have exactly one root commit, found %d", len(f))
	}
	baseTree, err := s.Tree(ctx, strings.TrimSpace(roots))
	if err != nil {
		return Result{}, false, err
	}
	known, err := s.Git(ctx, "log", "-n", itoa(l.HistoryDepth), "--format=%T", trunkHistory)
	if err != nil {
		return Result{}, false, err
	}
	if !containsLine(known, baseTree) {
		return reject("unknown base tree %s: run git ws-sync", baseTree)
	}
	mainTree, err := s.Tree(ctx, oldMain)
	if err != nil {
		return Result{}, false, err
	}
	workTree, err := s.Tree(ctx, req.Tip)
	if err != nil {
		return Result{}, false, err
	}

	newTree := workTree
	if mainTree != baseTree {
		out, mergeErr := s.Git(ctx, "merge-tree", "--write-tree", "--name-only", "--merge-base="+baseTree, mainTree, workTree)
		if mergeErr != nil {
			var ge *GitError
			if errors.As(mergeErr, &ge) && ge.ExitCode() == 1 {
				return reject("merge conflict: %s", truncate(strings.Join(strings.Fields(out), " "), 400))
			}
			return Result{}, false, mergeErr
		}
		newTree, _, _ = strings.Cut(out, "\n")
	}
	if newTree == mainTree {
		return Result{Outcome: AlreadyLanded}, false, nil
	}

	if v, err := capy.CheckVisibilityDiff(ctx, s.Dir, mainTree, newTree); err != nil {
		return Result{}, false, err
	} else if len(v) > 0 {
		return reject("visibility: %s", summarize(v))
	}
	id := capy.Identity{Requester: req.Requester, Approvers: req.Approvers}
	if v, err := capy.CheckOwners(ctx, s.Dir, mainTree, newTree, id, l.Forge.TeamResolver(ctx)); err != nil {
		return Result{}, false, err
	} else if len(v) > 0 {
		return reject("OWNERS: %s", summarize(v))
	}

	if l.Presubmit != nil {
		changed, err := s.ChangedPaths(ctx, mainTree, newTree)
		if err != nil {
			return Result{}, false, err
		}
		pr, err := l.Presubmit.Run(ctx, PresubmitInput{Store: s, BaseTree: mainTree, NewTree: newTree, Changed: changed})
		if err != nil {
			return Result{}, false, fmt.Errorf("presubmit: %w", err)
		}
		if !pr.Passed {
			l.Log.Info("presubmit failed", "id", req.ID, "details", pr.Details)
			return reject("presubmit: %s", pr.Summary)
		}
	}

	// Control-plane edits: the overlay entries of the trunk tree equal refs/heads/main.
	var newCtrlCommit, newCtrlTree string
	var ctrlTreeOld string
	if haveCtrl {
		ctrlTreeOld, err = s.Tree(ctx, oldCtrl)
		if err != nil {
			return Result{}, false, err
		}
		names, err := s.overlayNames(ctx, ctrlTreeOld)
		if err != nil {
			return Result{}, false, err
		}
		newCtrlTree, err = s.extractOverlay(ctx, newTree, names)
		if err != nil {
			return Result{}, false, err
		}
	}

	meta, err := s.Meta(ctx, req.Tip)
	if err != nil {
		return Result{}, false, err
	}
	msg := strings.TrimRight(meta.Message, "\n") + "\n"
	if req.LandRef != "" {
		msg += "\nSuda-Landed-From: " + req.LandRef + "\n"
	}
	if req.Requester != "" {
		msg += "Suda-Requester: " + req.Requester + "\n"
	}
	for _, a := range req.Approvers {
		msg += "Suda-Approved-By: " + a + "\n"
	}
	for _, t := range req.ExtraTrailers {
		msg += t + "\n"
	}

	if l.DryRun {
		l.Log.Info("dry-run: would land", "id", req.ID, "tree", newTree)
		return Result{Outcome: Landed, Hist: "dry-run"}, false, nil
	}

	snap, err := s.CommitTree(ctx, newTree, nil, msg, &meta)
	if err != nil {
		return Result{}, false, err
	}
	hist, err := s.CommitTree(ctx, newTree, []string{oldHist}, msg, &meta)
	if err != nil {
		return Result{}, false, err
	}
	refspecs := []string{snap + ":refs/suda/main", hist + ":refs/suda/history"}
	leases := []string{"refs/suda/main:" + oldMain, "refs/suda/history:" + oldHist}
	if haveCtrl && newCtrlTree != ctrlTreeOld {
		newCtrlCommit, err = s.CommitTree(ctx, newCtrlTree, []string{oldCtrl}, msg, &meta)
		if err != nil {
			return Result{}, false, err
		}
		refspecs = append(refspecs, newCtrlCommit+":refs/heads/main")
		leases = append(leases, "refs/heads/main:"+oldCtrl)
	}
	if err := s.Push(ctx, l.Trunk, Write, true, leases, refspecs...); err != nil {
		return Result{}, true, fmt.Errorf("atomic trunk push: %w", err)
	}
	// Keep the local view consistent without waiting for the next fetch.
	_, _ = s.Git(ctx, "update-ref", trunkMain, snap)
	_, _ = s.Git(ctx, "update-ref", trunkHistory, hist)
	if newCtrlCommit != "" {
		_, _ = s.Git(ctx, "update-ref", trunkHeads+"main", newCtrlCommit)
	}
	return Result{Outcome: Landed, Hist: hist}, false, nil
}

func containsLine(haystack, needle string) bool {
	for _, l := range strings.Split(haystack, "\n") {
		if strings.TrimSpace(l) == needle {
			return true
		}
	}
	return false
}

// summarize joins at most five violations for a one-line status.
func summarize(v []string) string {
	if len(v) > 5 {
		return strings.Join(v[:5], "; ") + fmt.Sprintf("; … %d more", len(v)-5)
	}
	return strings.Join(v, "; ")
}

// NoteWake classifies a wake hint. A webhook is only a hint: it may name the repository that
// changed, and the lander uses that solely to decide how much to re-check, never what to do.
// relevant is false for repositories the lander does not watch (so org-wide webhooks cost
// nothing). A change in a configured mirror forces the next cycle to reconcile mirrors.
// An empty repo (for example a bare wake) is relevant but forces nothing.
func (l *Lander) NoteWake(repo string) (relevant bool) {
	if repo == "" || repo == l.User.Repo || repo == l.Trunk.Repo {
		return true
	}
	if m := l.mirrorRepos.Load(); m != nil && (*m)[repo] {
		l.mu.Lock()
		if l.dirtyRepos == nil {
			l.dirtyRepos = map[string]bool{}
		}
		l.dirtyRepos[repo] = true
		l.mu.Unlock()
		return true
	}
	return false
}
