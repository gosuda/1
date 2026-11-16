package gstimeclock

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/gstime"
	rawclock "gosuda.org/gstime/clock"
	"gosuda.org/gstime/config"
	"gosuda.org/gstime/ntp"

	"github.com/gosuda/supersql-alpha/clock"
)

type sourceFunc func(context.Context, config.SourceConfig, gstime.RawNanos) (*ntp.MeasurementResult, error)

func (f sourceFunc) QuerySource(ctx context.Context, source config.SourceConfig, raw gstime.RawNanos) (*ntp.MeasurementResult, error) {
	return f(ctx, source, raw)
}

func providerConfig(t *testing.T) Config {
	t.Helper()
	raw := rawclock.NewSimulatedRawClock(1_000_000_000)
	raw.SetScaleEnvelope(gstime.RateScale(gstime.OneQ48), gstime.RateScale(gstime.OneQ48))
	return Config{
		Raw:         raw,
		LeapHistory: testHistory(t),
		Sync: config.Config{
			Assurance: config.AssuranceConfig{MinVotingDomains: 1, MinHonestCoverage: 1, MaxWidthNs: 1_000_000, MaxAgeNs: 1_000_000_000, MaxHoldoverAgeNs: 2_000_000_000},
			Raw:       config.RawConfig{BackendProfile: "simulated", ReadBoundNs: 100},
			Sources:   []config.SourceConfig{{FaultDomainID: "simulated", Endpoint: "simulated.invalid:123"}},
		},
	}
}

func fixedSource(_ context.Context, _ config.SourceConfig, raw gstime.RawNanos) (*ntp.MeasurementResult, error) {
	return &ntp.MeasurementResult{RawMid: raw, RawMidReadBound: 100, HardInterval: gstime.TimeInterval{Earliest: 1000, Latest: 1100}}, nil
}

func TestProviderCloseCancelsFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider, err := open(t.Context(), providerConfig(t), gstime.WithSourceQuerier(sourceFunc(fixedSource)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := provider.Close(); err != nil {
				t.Error(err)
			}
		})
		reading, err := provider.Read(t.Context())
		if err != nil || reading.Status != clock.Synced {
			t.Fatalf("startup read = %+v, %v", reading, err)
		}
		done := make(chan error, 1)
		go func() { done <- provider.WaitUntilAfter(t.Context(), reading.Earliest, reading.Epoch) }()
		synctest.Wait()
		if err := provider.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, clock.ErrClockUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("closed wait = %v", err)
		}
		if _, err := provider.Read(t.Context()); !errors.Is(err, clock.ErrClockUnavailable) {
			t.Fatalf("post-close read = %v", err)
		}
		if err := provider.Close(); err != nil {
			t.Fatalf("repeated Close = %v", err)
		}
	})
}

func TestProviderLifetimeCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		entered, exited := make(chan struct{}), make(chan struct{})
		initial := true
		source := sourceFunc(func(ctx context.Context, cfg config.SourceConfig, raw gstime.RawNanos) (*ntp.MeasurementResult, error) {
			if initial {
				initial = false
				return fixedSource(ctx, cfg, raw)
			}
			close(entered)
			defer close(exited)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		})
		provider, err := open(ctx, providerConfig(t), gstime.WithSourceQuerier(source), gstime.WithQueryTimeout(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := provider.Close(); err != nil {
				t.Error(err)
			}
		}()
		cancel(errors.New("startup complete"))
		<-entered
		reading, err := provider.Read(t.Context())
		if err != nil || reading.Status != clock.Synced {
			t.Fatalf("read after startup cancellation = %+v, %v", reading, err)
		}
		if err := provider.WaitUntilAfter(t.Context(), reading.Earliest-1, reading.Epoch); err != nil {
			t.Fatalf("fence after startup cancellation = %v", err)
		}
		select {
		case <-exited:
			t.Fatal("startup cancellation stopped the background source")
		default:
		}
		if err := provider.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("Close returned before its background source exited")
		}
	})
}

func TestProviderStartupCancellationJoinsSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		entered := make(chan struct{})
		exited := make(chan struct{})
		source := sourceFunc(func(ctx context.Context, _ config.SourceConfig, _ gstime.RawNanos) (*ntp.MeasurementResult, error) {
			close(entered)
			defer close(exited)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		})
		done := make(chan error, 1)
		cfg := providerConfig(t)
		go func() {
			provider, err := open(ctx, cfg, gstime.WithSourceQuerier(source), gstime.WithQueryTimeout(time.Hour))
			if provider != nil {
				err = errors.Join(err, provider.Close())
			}
			done <- err
		}()
		<-entered
		cause := errors.New("host startup canceled")
		cancel(cause)
		if err := <-done; !errors.Is(err, cause) || !errors.Is(err, gstime.ErrCancelled) {
			t.Fatalf("startup cancellation = %v", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("Open returned before its source exited")
		}
	})
}

func TestProviderStartupDeadlineJoinsSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		exited := make(chan struct{})
		source := sourceFunc(func(ctx context.Context, _ config.SourceConfig, _ gstime.RawNanos) (*ntp.MeasurementResult, error) {
			defer close(exited)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		provider, err := open(ctx, providerConfig(t), gstime.WithSourceQuerier(source), gstime.WithQueryTimeout(time.Hour))
		if provider != nil {
			t.Cleanup(func() { _ = provider.Close() })
			t.Fatal("startup returned a provider without assurance")
		}
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, gstime.ErrDeadlineExceeded) {
			t.Fatalf("startup deadline = %v", err)
		}
		select {
		case <-exited:
		default:
			t.Fatal("Open returned before its source exited")
		}
	})
}

func TestProviderRejectsInitialOutOfRangeInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var engine *gstime.SyncEngine
		source := sourceFunc(func(_ context.Context, _ config.SourceConfig, raw gstime.RawNanos) (*ntp.MeasurementResult, error) {
			return &ntp.MeasurementResult{RawMid: raw, RawMidReadBound: 100, HardInterval: gstime.TimeInterval{Earliest: -1000, Latest: -900}}, nil
		})
		provider, err := open(t.Context(), providerConfig(t), gstime.WithSourceQuerier(source), func(e *gstime.SyncEngine) { engine = e })
		if provider != nil {
			t.Cleanup(func() { _ = provider.Close() })
			t.Fatal("out-of-range startup returned a provider")
		}
		if !errors.Is(err, clock.ErrClockRange) {
			t.Fatalf("initial invalid interval = %v", err)
		}
		if err := engine.Start(t.Context()); err == nil {
			t.Cleanup(func() { _ = engine.Close() })
			t.Fatal("failed startup left its engine restartable")
		}
	})
}

func TestProviderRequiresExplicitConfiguration(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"raw", func(c *Config) { c.Raw = nil }},
		{"typed nil raw", func(c *Config) { c.Raw = (*rawclock.SimulatedRawClock)(nil) }},
		{"history", func(c *Config) { c.LeapHistory = nil }},
		{"width", func(c *Config) { c.Sync.Assurance.MaxWidthNs = 0 }},
		{"age", func(c *Config) { c.Sync.Assurance.MaxAgeNs = 0 }},
		{"holdover age", func(c *Config) { c.Sync.Assurance.MaxHoldoverAgeNs = 0 }},
		{"sources", func(c *Config) { c.Sync.Sources = nil }},
		{"endpoint", func(c *Config) { c.Sync.Sources[0].Endpoint = "" }},
		{"suspend omitted", func(c *Config) { c.Raw = rawclock.NewSystemRawClock() }},
		{"raw read bound", func(c *Config) { c.Sync.Raw.ReadBoundNs = 99 }},
		{"raw arithmetic range", func(c *Config) { c.Raw.(*rawclock.SimulatedRawClock).SetRaw(math.MaxUint64) }},
		{"invalid physical scale", func(c *Config) { c.Raw.(*rawclock.SimulatedRawClock).SetScaleEnvelope(0, 1) }},
		{"nonfinite scale", func(c *Config) { c.Sync.Raw.ScaleLowerPpm = math.NaN() }},
		{"fault budget overflow", func(c *Config) { c.Sync.Assurance.FaultBudget = math.MaxInt }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := providerConfig(t)
			tc.change(&cfg)
			provider, err := Open(t.Context(), cfg)
			if provider != nil {
				t.Cleanup(func() { _ = provider.Close() })
				t.Fatal("invalid configuration returned a provider")
			}
			if !errors.Is(err, clock.ErrClockUnavailable) {
				t.Fatalf("configuration failure = %v", err)
			}
		})
	}
}

func TestFailedRoundHoldoverExpiresFromLastSuccess(t *testing.T) {
	cfg := providerConfig(t)
	cfg.Sync.Assurance.MaxAgeNs = 2_000_000_000
	cfg.Sync.Assurance.MaxHoldoverAgeNs = 1_000_000_000
	raw := cfg.Raw.(*rawclock.SimulatedRawClock)
	id, err := cfg.Sync.ConfigID()
	if err != nil {
		t.Fatal(err)
	}
	service := gstime.NewClockService(raw, cfg.LeapHistory, id, cfg.Sync.Assurance.MaxWidthNs)
	adapter, err := New(service, cfg.LeapHistory)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := false
	source := sourceFunc(func(ctx context.Context, cfg config.SourceConfig, now gstime.RawNanos) (*ntp.MeasurementResult, error) {
		if unavailable {
			return nil, gstime.ErrPublicationUnavailable
		}
		return fixedSource(ctx, cfg, now)
	})
	engine, err := gstime.NewSyncEngine(cfg.Sync, service, gstime.WithSourceQuerier(source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := engine.PollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	initial, err := adapter.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unavailable = true
	for range 2 {
		raw.Advance(500_000_000)
		if err := engine.PollOnce(t.Context()); err == nil {
			t.Fatal("unavailable sources produced successful round")
		}
		reading, err := adapter.Read(t.Context())
		if err != nil || reading.Status != clock.Holdover || reading.Epoch != initial.Epoch {
			t.Fatalf("holdover reading = %+v, %v", reading, err)
		}
		if err := adapter.WaitUntilAfter(t.Context(), reading.Earliest-1, reading.Epoch); err != nil {
			t.Fatalf("valid holdover fence = %v", err)
		}
	}
	raw.Advance(1)
	if _, err := adapter.Read(t.Context()); !errors.Is(err, clock.ErrClockUnavailable) || !errors.Is(err, gstime.ErrBoundExpired) {
		t.Fatalf("holdover past last-success horizon = %v", err)
	}
	unavailable = false
	if err := engine.PollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if reading, err := adapter.Read(t.Context()); err != nil || reading.Status != clock.Synced {
		t.Fatalf("reacquired reading = %+v, %v", reading, err)
	}
}

func TestInvalidationReacquisitionUsesFreshFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, raw, adapter := testService(t, testHistory(t))
		anchor(t, service, raw)
		initial, err := adapter.Read(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Invalidate(); err != nil {
			t.Fatal(err)
		}
		if err := adapter.WaitUntilAfter(t.Context(), 0, initial.Epoch); !errors.Is(err, clock.ErrClockUnavailable) || !errors.Is(err, gstime.ErrRawFault) {
			t.Fatalf("invalidated fence = %v", err)
		}
		anchor(t, service, raw)
		recovered, err := adapter.Read(t.Context())
		if err != nil || recovered.Status != clock.Synced {
			t.Fatalf("reacquired reading = %+v, %v", recovered, err)
		}
		// Reacquisition happens under the service's new assurance epoch, so the
		// fence issued before invalidation is rejected instead of silently reused.
		if recovered.Epoch == initial.Epoch {
			t.Fatalf("reacquisition reused invalidated epoch %+v", recovered.Epoch)
		}
		if err := adapter.WaitUntilAfter(t.Context(), 0, initial.Epoch); !errors.Is(err, clock.ErrClockEpochChanged) {
			t.Fatalf("reacquisition accepted invalidated epoch: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- adapter.WaitUntilAfter(ctx, recovered.Earliest, recovered.Epoch) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("reacquired equality fence returned %v", err)
		default:
		}
		raw.Advance(201)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		otherService, otherRaw, other := testService(t, testHistory(t))
		anchor(t, otherService, otherRaw)
		if err := other.WaitUntilAfter(t.Context(), 0, initial.Epoch); !errors.Is(err, clock.ErrClockEpochChanged) {
			t.Fatalf("replacement service accepted old epoch: %v", err)
		}
	})
}
