package supersql

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// MaintenanceConfig bounds the background maintenance loop started by Open.
// Maintenance reclaims published commit segments and obsolete run files through
// the retention coordinator and the backend's collection engine.
type MaintenanceConfig struct {
	// Disabled stops Open from starting the loop. The embedder then owns
	// reclamation: nothing else retires commits/ or runs/ files.
	Disabled bool
	// Interval is the probe period. Zero selects DefaultMaintenanceInterval;
	// values below MinMaintenanceInterval are rejected by Open.
	Interval time.Duration
	// WALThreshold is the published commit-segment count at which one pass runs.
	// Zero selects DefaultWALThreshold.
	WALThreshold uint64
	// RunThreshold is the live run-file count at which one pass runs. Zero
	// selects DefaultRunThreshold.
	RunThreshold uint64
	// MaxRetiredTargets caps the retired objects one pass collects. Zero selects
	// DefaultMaxRetiredTargets.
	MaxRetiredTargets uint64
}

const (
	// DefaultMaintenanceInterval is the background probe period.
	DefaultMaintenanceInterval = 30 * time.Second
	// MinMaintenanceInterval is the smallest accepted probe period.
	MinMaintenanceInterval = 100 * time.Millisecond
	// DefaultWALThreshold is the published commit-segment count that triggers a
	// maintenance pass.
	DefaultWALThreshold = 64
	// DefaultRunThreshold is the live run-file count that triggers a maintenance
	// pass.
	DefaultRunThreshold = 64
	// DefaultMaxRetiredTargets caps the retired objects one pass collects.
	DefaultMaxRetiredTargets = 1024
)

// errMaintenanceOptions rejects a probe period Open cannot honor. It mirrors the
// retention coordinator's own option rejection.
var errMaintenanceOptions = &model.Error{Code: "22023", Message: "invalid maintenance configuration"}

// normalizedMaintenance fills every zero field with its default and rejects a
// probe period below MinMaintenanceInterval.
func normalizedMaintenance(config MaintenanceConfig) (MaintenanceConfig, error) {
	if config.Interval < 0 || (config.Interval > 0 && config.Interval < MinMaintenanceInterval) {
		return MaintenanceConfig{}, errMaintenanceOptions
	}
	config.Interval = cmp.Or(config.Interval, DefaultMaintenanceInterval)
	config.WALThreshold = cmp.Or(config.WALThreshold, DefaultWALThreshold)
	config.RunThreshold = cmp.Or(config.RunThreshold, DefaultRunThreshold)
	config.MaxRetiredTargets = cmp.Or(config.MaxRetiredTargets, DefaultMaxRetiredTargets)
	return config, nil
}

// MaintenanceError reports the most recent background maintenance failure, or
// nil when every pass since Open succeeded. The loop exits on a fatal failure
// and delivers it to Close; a long-running host observes the same error here
// without closing the database.
func (database *Catalog) MaintenanceError() error {
	if database == nil {
		return nil
	}
	if last := database.maintenanceLast.Load(); last != nil {
		return *last
	}
	return nil
}

// maintenanceLoop probes the backend backlog and reclaims on a fixed period. The
// loop never takes database.mu: Close holds it for the whole of closeAll, and
// Close joins this loop through maintenanceErr.
func (database *Catalog) maintenanceLoop(ctx context.Context) error {
	ticker := time.NewTicker(database.maintenance.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		err := database.maintainOnce(ctx)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return ctx.Err()
		case transientMaintenance(err):
			database.maintenanceLast.Store(&err)
		default:
			database.maintenanceLast.Store(&err)
			return err
		}
	}
}

// maintainOnce advances retention and then compacts at the durable floor. The
// floor-preserving collection reclaims commit segments and obsolete run files
// without moving the history boundary that the retention policy owns.
func (database *Catalog) maintainOnce(ctx context.Context) error {
	wal, runs, err := database.backend.MaintenanceBacklog(ctx)
	if err != nil {
		return err
	}
	uncompacted := min(runs, database.backend.UncompactedRunFiles())
	if wal < database.maintenance.WALThreshold && uncompacted < database.maintenance.RunThreshold {
		return nil
	}
	if err := database.retention.Advance(ctx); err != nil {
		return err
	}
	wal, runs, err = database.backend.MaintenanceBacklog(ctx)
	if err != nil {
		return err
	}
	uncompacted = min(runs, database.backend.UncompactedRunFiles())
	if wal < database.maintenance.WALThreshold && uncompacted < database.maintenance.RunThreshold {
		return nil
	}
	state, err := database.backend.RetentionState(ctx)
	if err != nil {
		return err
	}
	return database.backend.Collect(ctx, storage.GCRequest{Boundary: state.HistoryFloor})
}

// transientMaintenance reports failures a later pass can retry: contention with a
// live reader or writer, an unresolved commit, a resource limit, an unavailable
// clock and filesystem errors. Every other failure is fatal to the loop.
func transientMaintenance(err error) bool {
	switch {
	case errors.Is(err, storage.ErrSnapshotPinned),
		errors.Is(err, storage.ErrCommitIndeterminate),
		errors.Is(err, columnar.ErrBatchBudget),
		errors.Is(err, schema.ErrJobLimit),
		errors.Is(err, clock.ErrClockUnavailable),
		errors.Is(err, clock.ErrClockEpochChanged),
		errors.Is(err, clock.ErrClockRange),
		errors.Is(err, context.DeadlineExceeded):
		return true
	}
	var pathErr *fs.PathError
	return errors.As(err, &pathErr)
}
