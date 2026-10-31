package lander

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const gcInterval = 24 * time.Hour

var stagedPRRe = regexp.MustCompile(`^(.+)-pr-(\d+)$`)

func (l *Lander) maybeGC(ctx context.Context) error {
	if !l.lastGC.IsZero() && l.Now().Sub(l.lastGC) < gcInterval {
		return nil
	}
	if err := l.GarbageCollect(ctx); err != nil {
		return err
	}
	l.lastGC = l.Now()
	return nil
}

// GarbageCollect prunes stale references in the user repository:
//   - refs/ws/* older than WorkspaceTTL (staged refs of the lander included),
//   - refs/ws/capy/<workflow>-pr-<N> whose pull request is no longer open,
//   - base/<id> + review/<id> pairs without an open pull request, once a day old.
//
// GitHub manages pack pruning on its side; the lander only removes references.
func (l *Lander) GarbageCollect(ctx context.Context) error {
	gc := &Store{Dir: l.Store.Dir + "-gc", Tokens: l.Store.Tokens, Log: l.Log}
	if err := gc.Init(ctx); err != nil {
		return err
	}
	// Commit dates are all that is needed, so fetch without blobs.
	if _, err := gc.Git(ctx, "config", "remote.user.url", l.User.URL); err != nil {
		return err
	}
	args := []string{"fetch", "-q", "--no-tags", "--prune", "--filter=blob:none", "user",
		"+refs/ws/*:refs/gc/ws/*", "+refs/heads/review/*:refs/gc/review/*", "+refs/heads/base/*:refs/gc/base/*"}
	if _, err := gc.GitRemote(ctx, l.User, Read, args...); err != nil {
		return fmt.Errorf("gc fetch: %w", err)
	}
	out, err := gc.Git(ctx, "for-each-ref", "--format=%(refname) %(objectname) %(committerdate:unix)", "refs/gc/")
	if err != nil {
		return err
	}

	cfg, cfgErr := l.LoadWorkflows(ctx)
	openPR := map[string]map[int]bool{} // workflow -> open PR numbers
	if cfgErr == nil {
		for _, wf := range cfg.Workflows {
			prs, err := l.Forge.OpenPulls(ctx, wf.DestinationRepo)
			if err != nil {
				openPR[wf.Name] = nil // unknown: never delete on uncertainty
				continue
			}
			set := map[int]bool{}
			for _, p := range prs {
				set[p.Number] = true
			}
			openPR[wf.Name] = set
		}
	}
	reviewOpen := map[string]bool{}
	userPRsKnown := false
	if prs, err := l.Forge.OpenPulls(ctx, l.User.Repo); err == nil {
		userPRsKnown = true
		for _, p := range prs {
			reviewOpen[p.HeadRef] = true
		}
	}

	now := l.Now()
	type del struct{ ref, sha string }
	var dels []del
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		ref, sha := f[0], f[1]
		ts, _ := strconv.ParseInt(f[2], 10, 64)
		age := now.Sub(time.Unix(ts, 0))
		switch {
		case strings.HasPrefix(ref, "refs/gc/ws/capy/"):
			name := strings.TrimPrefix(ref, "refs/gc/ws/capy/")
			if m := stagedPRRe.FindStringSubmatch(name); m != nil {
				n, _ := strconv.Atoi(m[2])
				if set, known := openPR[m[1]]; known && set != nil && !set[n] {
					dels = append(dels, del{"refs/ws/capy/" + name, sha})
					continue
				}
			}
			if age > l.WorkspaceTTL {
				dels = append(dels, del{"refs/ws/capy/" + name, sha})
			}
		case strings.HasPrefix(ref, "refs/gc/ws/"):
			if age > l.WorkspaceTTL {
				dels = append(dels, del{"refs/ws/" + strings.TrimPrefix(ref, "refs/gc/ws/"), sha})
			}
		case strings.HasPrefix(ref, "refs/gc/review/"), strings.HasPrefix(ref, "refs/gc/base/"):
			ns := "review"
			if strings.HasPrefix(ref, "refs/gc/base/") {
				ns = "base"
			}
			id := strings.TrimPrefix(ref, "refs/gc/"+ns+"/")
			if userPRsKnown && !reviewOpen["review/"+id] && age > 24*time.Hour {
				dels = append(dels, del{"refs/heads/" + ns + "/" + id, sha})
			}
		}
	}
	var errs []error
	for _, d := range dels {
		l.Log.Info("gc: pruning", "ref", d.ref, "dry-run", l.DryRun)
		if l.DryRun {
			continue
		}
		if err := l.Store.Push(ctx, l.User, Write, false, []string{d.ref + ":" + d.sha}, ":"+d.ref); err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", d.ref, err))
		}
	}
	return errors.Join(errs...)
}
