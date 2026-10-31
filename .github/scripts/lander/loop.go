package lander

import (
	"context"
	"errors"
	"time"
)

// Cycle runs every stage once. Stages are independent: a failure in one is reported but does
// not skip the others, except that a halted queue (security invariant) stops all writes.
func (l *Lander) Cycle(ctx context.Context) error {
	l.halted = nil
	if err := l.fetchTrunk(ctx); err != nil {
		return err
	}
	if err := l.VerifyTrunk(ctx); err != nil {
		return err
	}
	var errs []error
	step := func(name string, f func(context.Context) error) {
		if l.halted != nil {
			return
		}
		if err := f(ctx); err != nil {
			l.Log.Error("stage failed", "stage", name, "err", err)
			errs = append(errs, err)
		}
	}
	step("control-plane-sync", l.SyncControlPlane)
	step("drain", l.DrainLandRequests)
	step("publish-lander", l.PublishLanderBranch)
	step("copy-trunk", l.CopyTrunkToUser)
	step("mirrors", l.maybeReconcileMirrors)
	step("gc", l.maybeGC)
	if l.halted != nil {
		errs = append(errs, l.halted)
	}
	return errors.Join(errs...)
}

// Run cycles until ctx ends. Each cycle starts after interval or when wake fires.
func (l *Lander) Run(ctx context.Context, interval time.Duration, wake <-chan struct{}) error {
	for {
		start := l.Now()
		if err := l.Cycle(ctx); err != nil {
			l.Log.Error("cycle finished with errors", "err", err)
		} else {
			l.Log.Debug("cycle ok", "took", l.Now().Sub(start))
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// maybeReconcileMirrors reconciles mirrors when the interval elapsed or a landing advanced the
// trunk since the last run. Mirror fetches dominate a cycle's cost and mirror events are rare, so
// they are polled less often than landing requests.
func (l *Lander) maybeReconcileMirrors(ctx context.Context) error {
	now := l.Now()
	forced := l.mirrorsForce.Swap(false)
	if !forced && l.MirrorInterval > 0 && !l.lastMirrors.IsZero() && now.Sub(l.lastMirrors) < l.MirrorInterval {
		return nil
	}
	l.lastMirrors = now
	return l.ReconcileMirrors(ctx)
}
