package gstimeclock

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"

	"gosuda.org/gstime"
	rawclock "gosuda.org/gstime/clock"
	"gosuda.org/gstime/config"

	"github.com/gosuda/supersql-alpha/clock"
)

type Config struct {
	Sync config.Config
	// Raw must include suspend; its physical bounds and sources remain caller-qualified.
	Raw         rawclock.RawClock
	LeapHistory *gstime.LeapHistory
}

// Provider owns its synchronization worker until Close cancels and joins it.
// Open's context bounds initialization; the supplied raw clock remains caller-owned.
type Provider struct {
	*certifiedClock
	engine    *gstime.SyncEngine
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, cfg Config) (*Provider, error) {
	return open(ctx, cfg)
}

func open(ctx context.Context, cfg Config, options ...gstime.SyncOption) (*Provider, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, errors.Join(clock.ErrClockUnavailable, err)
	}
	history, err := ownHistory(cfg.LeapHistory)
	if err != nil {
		return nil, err
	}
	cfg.Sync.Sources = slices.Clone(cfg.Sync.Sources)
	configID, err := cfg.Sync.ConfigID()
	if err != nil {
		return nil, errors.Join(clock.ErrClockUnavailable, err)
	}
	service := gstime.NewClockService(cfg.Raw, history, configID, cfg.Sync.Assurance.MaxWidthNs)
	engine, err := gstime.NewSyncEngine(cfg.Sync, service, options...)
	if err != nil {
		return nil, errors.Join(clock.ErrClockUnavailable, err)
	}
	lifetime, cancel := context.WithCancel(context.Background())
	p := &Provider{engine: engine, cancel: cancel}
	fail := func(err error) (*Provider, error) {
		return nil, errors.Join(clock.ErrClockUnavailable, providerError(ctx, err), p.Close())
	}
	if err := engine.Start(lifetime); err != nil {
		return fail(err)
	}
	if err := engine.WaitSync(ctx); err != nil {
		return fail(err)
	}
	borrowed, err := New(service, history)
	if err != nil {
		return fail(err)
	}
	p.certifiedClock = borrowed.(*certifiedClock)
	p.lifetime = lifetime
	p.maxWidth = cfg.Sync.Assurance.MaxWidthNs
	p.maxAge = cfg.Sync.Assurance.MaxAgeNs
	p.maxHoldoverAge = cfg.Sync.Assurance.MaxHoldoverAgeNs
	if _, err := p.Read(ctx); err != nil {
		return fail(err)
	}
	return p, nil
}

func validateConfig(cfg Config) error {
	if cfg.Raw == nil {
		return fmt.Errorf("%w: raw clock required", config.ErrInvalidConfig)
	}
	value := reflect.ValueOf(cfg.Raw)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("%w: nil raw clock", config.ErrInvalidConfig)
		}
	}
	if !cfg.Raw.IncludesSuspend() {
		return fmt.Errorf("%w: raw clock must include suspend", config.ErrInvalidConfig)
	}
	if cfg.Sync.Assurance.FaultBudget > (math.MaxInt-1)/2 {
		return fmt.Errorf("%w: fault budget overflows voting threshold", config.ErrInvalidConfig)
	}
	if err := cfg.Sync.Validate(); err != nil {
		return err
	}
	if cfg.Sync.Assurance.MaxAgeNs <= 0 || cfg.Sync.Assurance.MaxHoldoverAgeNs <= 0 || strings.TrimSpace(cfg.Sync.Raw.BackendProfile) == "" {
		return fmt.Errorf("%w: positive age bounds and raw backend profile required", config.ErrInvalidConfig)
	}
	if len(cfg.Sync.Sources) == 0 {
		return fmt.Errorf("%w: explicit upstream sources required", config.ErrInvalidConfig)
	}
	for _, source := range cfg.Sync.Sources {
		if strings.TrimSpace(source.Endpoint) == "" || strings.TrimSpace(source.FaultDomainID) == "" {
			return fmt.Errorf("%w: source endpoint and fault domain required", config.ErrInvalidConfig)
		}
	}
	low, high := cfg.Raw.ScaleEnvelope()
	if low <= 0 || high < low || math.IsNaN(cfg.Sync.Raw.ScaleLowerPpm) || math.IsNaN(cfg.Sync.Raw.ScaleUpperPpm) || math.IsInf(cfg.Sync.Raw.ScaleLowerPpm, 0) || math.IsInf(cfg.Sync.Raw.ScaleUpperPpm, 0) {
		return fmt.Errorf("%w: invalid raw scale envelope", config.ErrInvalidConfig)
	}
	reading := cfg.Raw.Read()
	if reading.Raw > math.MaxInt64 || reading.ReadBound > gstime.ErrorNs(cfg.Sync.Raw.ReadBoundNs) || cfg.Sync.Raw.ReadBoundNs > math.MaxInt64/2 {
		return fmt.Errorf("%w: raw reading exceeds configured or arithmetic bounds", config.ErrInvalidConfig)
	}
	return nil
}

func providerError(ctx context.Context, err error) error {
	if errors.Is(err, gstime.ErrCancelled) || errors.Is(err, gstime.ErrDeadlineExceeded) {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = context.Canceled
			if errors.Is(err, gstime.ErrDeadlineExceeded) {
				cause = context.DeadlineExceeded
			}
		}
		return errors.Join(cause, err)
	}
	return err
}

func (p *Provider) Close() error {
	p.closeOnce.Do(func() {
		p.cancel()
		p.closeErr = p.engine.Close()
	})
	return p.closeErr
}
