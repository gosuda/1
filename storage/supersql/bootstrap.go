package supersql

import (
	"context"
	"errors"
	goruntime "runtime"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

// ensureHistoryPolicy installs the durable retention policy when the database
// has none, or updates it when OpenOptions.HistoryRetention explicitly requests
// a different window.
func (database *Catalog) ensureHistoryPolicy(ctx context.Context) error {
	transaction, err := database.manager.Begin(ctx, database.resolvedTransactionOptions())
	if err != nil {
		return err
	}
	mutated := false
	err = transaction.Statement(ctx, func(statement context.Context) error {
		policy, readErr := database.indexJobs.HistoryPolicy(statement, transaction)
		if readErr == nil {
			if database.historyRetention > 0 && policy.Retention != database.historyRetention {
				mutated = true
				return database.indexJobs.UpdateHistoryPolicy(statement, transaction, policy.Revision, database.historyRetention)
			}
			return nil
		}
		if !errors.Is(readErr, storage.ErrRowNotFound) {
			return readErr
		}
		mutated = true
		return database.indexJobs.CreateHistoryPolicy(statement, transaction, database.historyRetention)
	})
	if err == nil && mutated {
		_, err = transaction.Commit(ctx)
	}
	return errors.Join(err, transaction.Close(context.Background()))
}

// openCatalog loads the durable native catalog, bootstrapping a fresh database,
// and guarantees the default `public` schema exists for the default search path.
func (database *Catalog) openCatalog(ctx context.Context) error {
	binding := bind.Options{
		SearchPath: []string{"pg_catalog"}, Collation: defaultCollation, DefaultCollation: defaultCollation,
		Limits: database.irLimits(), Builtin: database.settings.BuiltinOptions(defaultCollation, defaultCollation),
	}
	config := session.NativeCatalogConfig{
		Catalog: database.catalog, Identities: database.identities, Binding: binding,
		Descriptors: database.limits, ManifestBytes: 8 << 20,
	}
	bootstrapped, err := database.catalogBootstrapped(ctx, config.ManifestBytes)
	if err != nil {
		return err
	}
	var native *session.NativeCatalog
	if bootstrapped {
		// Loading validates the durable record inside its own transaction block,
		// so a partially published catalog can never be completed silently. A
		// historical-only open pins the recovered closed timestamp, which needs
		// no fresh certification.
		start := session.TransactionStart{}
		if database.readOnly {
			state, stateErr := database.backend.State(ctx)
			if stateErr != nil {
				return stateErr
			}
			start.Historical = state.ClosedTS
		}
		if err := database.txns.Begin(ctx, start); err != nil {
			return err
		}
		if native, err = session.LoadNativeCatalog(ctx, database.txns, config); err != nil {
			return errors.Join(err, database.txns.Rollback(ctx, false))
		}
		if _, err := database.txns.Commit(ctx, false); err != nil {
			return err
		}
	} else if database.readOnly {
		return &model.Error{Code: "55000", Message: "historical read-only open requires a bootstrapped database"}
	} else if native, err = session.BootstrapNativeCatalog(ctx, database.txns, config); err != nil {
		return err
	}
	database.native = native
	principal := model.Principal{}
	if database.source, err = catalogrel.NewSource(catalogrel.SourceOptions{
		Snapshot:   catalogrel.CatalogStoreFactory{Store: database.catalog},
		Statistics: catalogrel.StatisticsStoreFactory{Store: database.statistics},
		Sequences:  database.sequences,
		Features:   newStackSQLMetadata{}, Standards: newStackSQLMetadata{},
		Database: catalogrel.Database{
			OID: 16384, Name: "supersql", Owner: principal.OID(), Encoding: 6,
			Collation: "C", CharacterType: "C", LocaleProvider: 'c', CollationOID: defaultCollation,
			AllowConnections: true, ConnectionLimit: -1,
			RoleOID: principal.OID(), RoleName: principal.Name(), RoleInherit: true,
			RoleCanLogin: true, RoleConnectionLimit: -1,
		},
		Registry: native.Registry,
		MaxRows:  1 << 20, MaxBytes: 64 << 20, MaxDescriptors: 65536,
		MaxDescriptorBytes: database.limits.MaxBytes, MaxDescriptorItems: database.limits.MaxItems,
		MaxDescriptorDepth: database.limits.MaxDepth,
	}); err != nil {
		return err
	}
	if database.readOnly {
		return nil
	}
	_, err = database.exec(ctx, "CREATE SCHEMA IF NOT EXISTS public", 0)
	return err
}

// catalogBootstrapped reports whether the durable native catalog manifest has
// already been published. Ordinary opens read through a certified current view;
// a historical-only open reads the recovered durable closed timestamp instead,
// so an absent manifest is distinguishable from a corrupt one without fresh
// certification.
func (database *Catalog) catalogBootstrapped(ctx context.Context, manifestBytes uint64) (bool, error) {
	if database.readOnly {
		state, err := database.backend.State(ctx)
		if err != nil {
			return false, err
		}
		if state.ClosedTS == 0 {
			return false, nil
		}
		reader, err := database.manager.HistoricalSnapshot(ctx, state.ClosedTS)
		if err != nil {
			return false, err
		}
		_, err = database.identities.CatalogManifest(ctx, reader, manifestBytes)
		closeErr := reader.Close()
		if err == nil {
			return true, closeErr
		}
		if errors.Is(err, storage.ErrRowNotFound) {
			return false, closeErr
		}
		return false, errors.Join(err, closeErr)
	}
	view, err := database.manager.CurrentReadView(ctx)
	if err != nil {
		return false, err
	}
	_, err = database.identities.CatalogManifest(ctx, view, manifestBytes)
	closeErr := view.Close()
	if err == nil {
		return true, closeErr
	}
	if errors.Is(err, storage.ErrRowNotFound) {
		return false, closeErr
	}
	return false, errors.Join(err, closeErr)
}

// bootstrapStores reserves the durable allocator and identity records once. The
// caller reuses the same transaction options for every later statement.
func (database *Catalog) bootstrapStores(ctx context.Context, options txn.TransactionOptions, fresh bool) error {
	if !fresh {
		return nil
	}
	bootstrap, err := database.manager.Begin(ctx, options)
	if err != nil {
		return err
	}
	err = bootstrap.Statement(ctx, func(statement context.Context) error {
		if err := database.sequences.InitializeAllocator(statement, bootstrap); err != nil {
			return err
		}
		return database.identities.InitializeIdentities(statement, bootstrap, schema.IdentityHighWaterMarks{
			ObjectID: 100, OID: 20000, JobID: 100,
		})
	})
	if err == nil {
		_, err = bootstrap.Commit(ctx)
	}
	return errors.Join(err, bootstrap.Close(context.Background()))
}

// prepareWorkers creates the shared worker pool and memory budget. Each
// statement still owns its own query account.
func (database *Catalog) prepareWorkers() error {
	parallelism := database.parallelism
	if parallelism <= 0 {
		parallelism = max(1, goruntime.GOMAXPROCS(0))
	}
	queueCapacity := max(2, parallelism*2)
	maxGroups := max(16, parallelism*4)
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: parallelism, QueueCapacity: queueCapacity, MaxGroups: maxGroups})
	if err != nil {
		return err
	}
	database.pool = pool
	budgetBytes := database.memoryBudgetBytes
	if budgetBytes == 0 {
		budgetBytes = 256 << 20
	}
	if database.budget, err = runtime.NewBudget(int64(budgetBytes), 4096); err != nil {
		return err
	}
	return nil
}

// transactionOptions is the single session transaction configuration. Resolver
// is filled in by compose once the index policy provider exists.
func (database *Catalog) transactionOptions() txn.TransactionOptions {
	memBytes := database.transactionMemoryBytes
	if memBytes == 0 {
		memBytes = 64 << 20
	}
	spillBytes := database.transactionSpillBytes
	if spillBytes == 0 {
		spillBytes = 256 << 20
	}
	return txn.TransactionOptions{
		Mode: txn.ReadWrite, KeyEncoder: database.keys,
		MemoryBytes: memBytes, SpillBytes: spillBytes, SpillDirectory: database.spill, MaxViews: 16,
	}
}

// resolvedTransactionOptions adds the sequence/identity/policy resolver chain.
// Call only after sequences, identities and policies exist; sessionConfig and
// the compose-time options both need the resolved form.
func (database *Catalog) resolvedTransactionOptions() txn.TransactionOptions {
	options := database.transactionOptions()
	options.Resolver = database.statistics.WithBaseResolver(database.sequences.WithBaseResolver(database.identities.WithBaseResolver(database.policies)))
	return options
}
