package txn

import (
	"context"
	"crypto/rand"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (manager *Manager) Begin(ctx context.Context, options TransactionOptions) (*Transaction, error) {
	if err := ctx.Err(); err != nil {
		return nil, transactionError(err)
	}
	invalidMode := options.Mode > HistoricalReadOnly
	unexpectedTimestamp := options.Mode != HistoricalReadOnly && options.Timestamp != 0
	invalidHistoricalTimestamp := options.Mode == HistoricalReadOnly && options.Timestamp <= 0
	if invalidMode || unexpectedTimestamp || invalidHistoricalTimestamp || options.MaxViews < 0 {
		return nil, errTransactionOptions
	}
	if manager.historicalOnly && options.Mode != HistoricalReadOnly {
		return nil, ErrReadOnlyTransaction
	}
	if options.MemoryBytes == 0 {
		options.MemoryBytes = 64 << 20
	}
	if options.SpillBytes == 0 {
		options.SpillBytes = 1 << 30
	}
	if options.MaxViews == 0 {
		options.MaxViews = 64
	}
	if options.MemoryBytes > math.MaxInt || uint64(options.MaxViews) > options.MemoryBytes/4096 {
		return nil, errTransactionBudget
	}
	if options.Mode == ReadWrite && (options.Resolver == nil || options.KeyEncoder == nil) {
		return nil, errTransactionOptions
	}
	if options.Mode == ReadWrite && !options.DisableSpill && options.SpillDirectory == "" {
		return nil, errMutationLogDirectory
	}
	transaction := &Transaction{manager: manager, mode: options.Mode, timestamp: options.Timestamp, maxViews: options.MaxViews, gate: make(chan struct{}, 1), cleanupDone: make(chan struct{})}
	// Metadata and statement scratch cannot borrow from the overlay or the
	// separately spilled commit sequence. The view allowance is shared by the
	// views that are actually open, not divided by the maximum view count: a
	// statement's declared read set grows with the catalog, so a single view
	// must be able to use the whole allowance when it is the only one open.
	transaction.metadataLimit = options.MemoryBytes / 8
	transaction.scratchBytes = options.MemoryBytes / 8
	transaction.viewSpace.limit = options.MemoryBytes / 4
	if transaction.metadataLimit < uint64(unsafe.Sizeof(Transaction{}))+uint64(options.MaxViews)*16+uint64(unsafe.Sizeof(model.PublicationOwner{})) || transaction.viewSpace.limit < 1024 {
		return nil, errTransactionBudget
	}
	transaction.metadataUsed = uint64(unsafe.Sizeof(Transaction{})) + uint64(options.MaxViews)*16 + uint64(unsafe.Sizeof(model.PublicationOwner{}))
	transaction.publications.owner = &model.PublicationOwner{}
	transaction.publications.limit = (transaction.metadataLimit - transaction.metadataUsed) / 4
	transaction.metadataUsed += transaction.publications.limit
	transaction.views = make([]*transactionView, 0, options.MaxViews)
	if options.Mode == ReadWrite {
		var err error
		transaction.log, err = newMutationLog(mutationLogLimits{MemoryBytes: options.MemoryBytes / 8, SpillBytes: options.SpillBytes, CursorBytes: options.MemoryBytes / 16, MaxCursors: 2, SpillDirectory: options.SpillDirectory, DisableSpill: options.DisableSpill})
		if err != nil {
			return nil, err
		}
		transaction.overlay, err = NewOverlay(options.MemoryBytes / 4)
		if err != nil {
			return nil, errors.Join(err, transaction.log.Close())
		}
		transaction.resolver, transaction.encoder = options.Resolver, options.KeyEncoder
	}
	if _, err := rand.Read(transaction.id[:]); err != nil {
		return nil, errors.Join(err, transaction.closeConstruction())
	}
	if transaction.id == (model.TxnID{}) {
		return nil, errors.Join(errTransactionOptions, transaction.closeConstruction())
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(transactionError(err), transaction.closeConstruction())
	}
	if manager.closed {
		return nil, errors.Join(storage.ErrClosed, transaction.closeConstruction())
	}
	if manager.transactionCloseError != nil {
		return nil, errors.Join(manager.transactionCloseError, transaction.closeConstruction())
	}
	if len(manager.transactions) >= manager.maxTransactions {
		return nil, errors.Join(ErrManagerCapacity, transaction.closeConstruction())
	}
	if _, exists := manager.transactions[transaction.id]; exists {
		return nil, errors.Join(errTransactionOptions, transaction.closeConstruction())
	}
	transaction.life, transaction.cancel = context.WithCancelCause(manager.life)
	owner, err := manager.locks.register(transaction.life, transaction.id, options.WaitOwner)
	if err != nil {
		transaction.cancel(err)
		return nil, errors.Join(err, transaction.closeConstruction())
	}
	transaction.owner = owner
	transaction.life = owner.ctx
	manager.transactionCleanups.Add(1)
	manager.transactions[transaction.id] = transaction
	// A wound schedules one cleanup callback, not a parked goroutine per owner.
	transaction.gate <- struct{}{}
	transaction.stopCleanup = context.AfterFunc(transaction.life, func() {
		defer manager.transactionCleanups.Done()
		defer close(transaction.cleanupDone)
		if transaction.state.CompareAndSwap(uint32(Active), uint32(Aborting)) {
			transaction.publications.owner.Decide(model.PublicationAborted)
		}
		transaction.gate <- struct{}{}
		defer func() { <-transaction.gate }()
		force := manager.life.Err() != nil
		_ = transaction.abortLocked(force)
	})
	<-transaction.gate
	return transaction, nil
}

func (transaction *Transaction) closeConstruction() error {
	var err error
	if transaction.overlay != nil {
		err = transaction.overlay.Close()
	}
	if transaction.log != nil {
		err = errors.Join(err, transaction.log.Close())
	}
	return err
}

func (manager *Manager) closeTransactions() error {
	var err error
	for {
		manager.mu.Lock()
		var transaction *Transaction
		for _, candidate := range manager.transactions {
			transaction = candidate
			break
		}
		manager.mu.Unlock()
		if transaction == nil {
			manager.transactionCleanups.Wait()
			manager.mu.Lock()
			err = errors.Join(err, manager.transactionCloseError)
			manager.mu.Unlock()
			return err
		}
		err = errors.Join(err, transaction.shutdown())
	}
}
