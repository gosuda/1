package lander

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrHalted is returned while the queue is stopped by a safety invariant.
var ErrHalted = errors.New("lander halted")

// VerifyTrunk enforces that refs/suda/history was written by the lander. When signing is
// configured, a tip that is neither validly signed nor acknowledged halts the queue
// (security invariant 4): an out-of-band push becomes visible instead of silently trusted.
func (l *Lander) VerifyTrunk(ctx context.Context) error {
	if l.Store.Signing == nil || l.Store.Signing.AllowedSigners == "" {
		return nil
	}
	tip, ok := l.Store.RevParse(ctx, trunkHistory)
	if !ok {
		return errors.New("trunk refs/suda/history missing")
	}
	if tip == l.AckTip {
		return nil
	}
	_, err := l.Store.Git(ctx, "-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+l.Store.Signing.AllowedSigners, "verify-commit", tip)
	if err != nil {
		l.halted = fmt.Errorf("%w: refs/suda/history tip %s is not signed by the lander key (acknowledge with LANDER_ACK_TIP): %v", ErrHalted, tip, err)
		return l.halted
	}
	return nil
}

// SyncControlPlane lands an overlay-sync commit when refs/heads/main was changed out of band so
// the trunk keeps the invariant that its overlay entries equal refs/heads/main.
func (l *Lander) SyncControlPlane(ctx context.Context) error {
	s := l.Store
	ctrl, ok := s.RevParse(ctx, trunkHeads+"main")
	if !ok {
		return nil
	}
	oldMain, _ := s.RevParse(ctx, trunkMain)
	oldHist, _ := s.RevParse(ctx, trunkHistory)
	ctrlTree, err := s.Tree(ctx, ctrl)
	if err != nil {
		return err
	}
	mainTree, err := s.Tree(ctx, oldMain)
	if err != nil {
		return err
	}
	names, err := s.overlayNames(ctx, ctrlTree)
	if err != nil {
		return err
	}
	want, err := s.withOverlay(ctx, mainTree, ctrlTree, names)
	if err != nil {
		return err
	}
	if want == mainTree {
		return nil
	}
	if l.DryRun {
		l.Log.Info("dry-run: control plane out of sync with trunk", "ctrl", ctrl)
		return nil
	}
	msg := fmt.Sprintf("chore(control-plane): sync refs/heads/main@%s into refs/suda/main\n\nSuda-Break-Glass: %s\n", ctrl[:7], ctrl)
	snap, err := s.CommitTree(ctx, want, nil, msg, nil)
	if err != nil {
		return err
	}
	hist, err := s.CommitTree(ctx, want, []string{oldHist}, msg, nil)
	if err != nil {
		return err
	}
	if err := s.Push(ctx, l.Trunk, Write, true,
		[]string{"refs/suda/main:" + oldMain, "refs/suda/history:" + oldHist},
		snap+":refs/suda/main", hist+":refs/suda/history"); err != nil {
		return fmt.Errorf("control-plane sync push: %w", err)
	}
	_, _ = s.Git(ctx, "update-ref", trunkMain, snap)
	_, _ = s.Git(ctx, "update-ref", trunkHistory, hist)
	l.Log.Info("control plane synced into trunk", "ctrl", ctrl, "hist", hist)
	return nil
}

// CopyTrunkToUser mirrors refs/suda/{main,history} into the user repository so workspace pushes
// have an advertised negotiation base. The copies carry no trust (landing verifies base trees
// against the real history); a member who rewrites them is undone on the next cycle.
//
// They are deliberately refs/suda/*, never branches or tags: a plain `git clone` of the user
// repository fetches every branch and tag, so a branch into the trunk would make every naive clone
// download the whole monorepo with its full history.
func (l *Lander) CopyTrunkToUser(ctx context.Context) error {
	if l.DryRun {
		return nil
	}
	s := l.Store
	var specs, leases []string
	for _, name := range []string{"main", "history"} {
		local, ok := s.RevParse(ctx, trunkSuda+name)
		if !ok {
			continue
		}
		remote, _ := s.RevParse(ctx, userSuda+name)
		if remote == local {
			continue
		}
		specs = append(specs, local+":refs/suda/"+name)
		leases = append(leases, "refs/suda/"+name+":"+remote)
	}
	if len(specs) == 0 {
		return nil
	}
	if err := s.Push(ctx, l.User, Write, false, leases, specs...); err != nil {
		return fmt.Errorf("copy trunk refs to user repository: %w", err)
	}
	for _, name := range []string{"main", "history"} {
		if v, ok := s.RevParse(ctx, trunkSuda+name); ok {
			_, _ = s.Git(ctx, "update-ref", userSuda+name, v)
		}
	}
	return nil
}

// PublishLanderBranch exports the deployable lander (Dockerfile, lander and capy sources from the
// control plane) to refs/heads/lander in the trunk repository.
func (l *Lander) PublishLanderBranch(ctx context.Context) error {
	s := l.Store
	ctrl, ok := s.RevParse(ctx, trunkHeads+"main")
	if !ok {
		return nil
	}
	landerDir, ok1 := s.RevParse(ctx, ctrl+":.github/scripts/lander")
	capyDir, ok2 := s.RevParse(ctx, ctrl+":.github/scripts/capy")
	if !ok1 || !ok2 {
		return nil
	}
	lEntries, err := s.ListTree(ctx, landerDir, true)
	if err != nil {
		return err
	}
	var entries []TreeEntry
	for _, e := range lEntries {
		if e.Path == "Dockerfile" || e.Path == ".dockerignore" {
			e.Path = strings.TrimPrefix(e.Path, "")
			entries = append(entries, e)
			continue
		}
		e.Path = "lander/" + e.Path
		entries = append(entries, e)
	}
	cEntries, err := s.ListTree(ctx, capyDir, true)
	if err != nil {
		return err
	}
	for _, e := range cEntries {
		e.Path = "capy/" + e.Path
		entries = append(entries, e)
	}
	// deploy/ helpers live next to the Dockerfile at the branch root.
	tree, err := s.BuildTree(ctx, entries)
	if err != nil {
		return err
	}
	old, haveOld := s.RevParse(ctx, trunkHeads+"lander")
	if haveOld {
		if oldTree, err := s.Tree(ctx, old); err == nil && oldTree == tree {
			return nil
		}
	}
	if l.DryRun {
		l.Log.Info("dry-run: would publish refs/heads/lander", "tree", tree)
		return nil
	}
	var parents []string
	lease := "refs/heads/lander:"
	if haveOld {
		parents = []string{old}
		lease += old
	}
	commit, err := s.CommitTree(ctx, tree, parents, fmt.Sprintf("lander: publish control-plane %s\n", ctrl), nil)
	if err != nil {
		return err
	}
	if err := s.Push(ctx, l.Trunk, Write, false, []string{lease}, commit+":refs/heads/lander"); err != nil {
		return fmt.Errorf("publish lander branch: %w", err)
	}
	_, _ = s.Git(ctx, "update-ref", trunkHeads+"lander", commit)
	l.Log.Info("published lander branch", "commit", commit)
	return nil
}

// userFrontDir holds, in the control plane, the files published as the root of the user
// repository's main branch (README and onboarding guide). Executable code is deliberately not
// part of it: members can write the user repository, so nothing they would run belongs there.
const userFrontDir = "docs/user-repo"

// PublishUserMain renders docs/user-repo of the control plane as refs/heads/main of the user
// repository. A member pushing over it is undone on the next cycle.
func (l *Lander) PublishUserMain(ctx context.Context) error {
	s := l.Store
	ctrl, ok := s.RevParse(ctx, trunkHeads+"main")
	if !ok {
		return nil
	}
	dir, ok := s.RevParse(ctx, ctrl+":"+userFrontDir)
	if !ok {
		return nil
	}
	entries, err := s.ListTree(ctx, dir, true)
	if err != nil {
		return err
	}
	tree, err := s.BuildTree(ctx, entries)
	if err != nil {
		return err
	}
	old, haveOld := s.RevParse(ctx, userMain)
	if haveOld {
		if oldTree, err := s.Tree(ctx, old); err == nil && oldTree == tree {
			return nil
		}
	}
	if l.DryRun {
		l.Log.Info("dry-run: would publish the user repository front page", "tree", tree)
		return nil
	}
	var parents []string
	lease := "refs/heads/main:"
	if haveOld {
		parents = []string{old}
		lease += old
		l.Log.Info("user repository main differs from the control plane; restoring", "tip", old)
	}
	commit, err := s.CommitTree(ctx, tree, parents, fmt.Sprintf("front page: publish control-plane %s\n", ctrl), nil)
	if err != nil {
		return err
	}
	if err := s.Push(ctx, l.User, Write, false, []string{lease}, commit+":refs/heads/main"); err != nil {
		return fmt.Errorf("publish user repository main: %w", err)
	}
	_, _ = s.Git(ctx, "update-ref", userMain, commit)
	return nil
}
