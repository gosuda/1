package neosepia

import (
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const batchEncodeFixedOverhead = uint64(4 + 16 + 8 + 16 + 32 + 32 + 8 + 8)

type valueBudgetChecker struct {
	used  uint64
	limit uint64
}

func (c *valueBudgetChecker) charge(count, size uint64) error {
	if count > (c.limit-c.used)/size {
		return model.ErrValueBudget
	}
	c.used += count * size
	return nil
}

func (c *valueBudgetChecker) check(ctx context.Context, value model.Value, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 {
		return model.ErrValueDepth
	}
	for _, allocation := range [...]struct{ count, size uint64 }{
		{uint64(len(value.StrVal)), 1},
		{uint64(len(value.BytesVal)), 1},
		{uint64(len(value.VectorVal)), 4},
		{uint64(len(value.Dimensions)), uint64(unsafe.Sizeof(model.ArrayDimension{}))},
		{uint64(len(value.ArrayVal)), uint64(unsafe.Sizeof(model.Value{}))},
	} {
		if err := c.charge(allocation.count, allocation.size); err != nil {
			return err
		}
	}
	for _, child := range value.ArrayVal {
		if err := c.check(ctx, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func checkValueCopyBudget(ctx context.Context, value model.Value, maxBytes uint64) error {
	checker := valueBudgetChecker{limit: maxBytes}
	return checker.check(ctx, value, 0)
}

func validateMemtableBatchBudget(ctx context.Context, batch storage.MutationBatch, limits Limits) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if batchEncodeFixedOverhead > limits.WALBytes {
		return columnar.ErrBatchBudget
	}
	for _, layout := range batch.Layouts {
		for _, col := range layout.Columns {
			if col.HasMissingValue {
				if err := checkValueCopyBudget(ctx, col.MissingValue, limits.RowBytes); err != nil {
					return err
				}
			}
		}
	}
	cursor, err := batch.OpenMutations(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()

	totalBytes := batchEncodeFixedOverhead
	for {
		mut, nextErr := cursor.Next(ctx)
		if nextErr == io.EOF {
			return nil
		}
		if nextErr != nil {
			return nextErr
		}
		mutSize, sizeErr := storage.EncodedMutationSize(mut)
		if sizeErr != nil {
			return sizeErr
		}
		if mutSize > limits.MutationBytes {
			return columnar.ErrBatchBudget
		}
		if mutSize > math.MaxUint64-8 {
			return columnar.ErrBatchBudget
		}
		framedSize := mutSize + 8
		if framedSize > limits.WALBytes-totalBytes {
			return columnar.ErrBatchBudget
		}
		totalBytes += framedSize
		if mut.Op == storage.MutationUpsert {
			rowChecker := valueBudgetChecker{limit: limits.RowBytes}
			for _, v := range mut.Values {
				if err := rowChecker.check(ctx, v, 0); err != nil {
					return err
				}
			}
		}
	}
}

func (backend *Backend) ApplyMutations(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	backend.mu.Lock()
	limits := backend.limits
	backend.mu.Unlock()
	if !memtableEnabled(limits) {
		return backend.applyMutationDirect(ctx, batch)
	}
	// Pre-validate batch outside locks so concurrent callers hash in parallel.
	validated, err := validateMutationBatch(ctx, batch)
	if err != nil {
		return storage.ApplyResult{}, err
	}
	if err := validateMemtableBatchBudget(ctx, batch, limits); err != nil {
		return storage.ApplyResult{}, err
	}
	return backend.applyMutationGrouped(ctx, batch, validated)
}

func (backend *Backend) applyMutationGrouped(ctx context.Context, batch storage.MutationBatch, validated validatedMutationBatch) (storage.ApplyResult, error) {
	req := &groupCommitReq{
		ctx:       ctx,
		batch:     batch,
		validated: validated,
		done:      make(chan struct{}),
	}
	backend.groupMu.Lock()
	backend.groupQueue = append(backend.groupQueue, req)
	isLeader := !backend.groupActive
	if isLeader {
		backend.groupActive = true
	}
	backend.groupMu.Unlock()

	if isLeader {
		backend.runGroupCommitLeader()
		return req.result, req.err
	}
	select {
	case <-req.done:
		return req.result, req.err
	case <-ctx.Done():
		backend.groupMu.Lock()
		for i, queued := range backend.groupQueue {
			if queued == req {
				backend.groupQueue = append(backend.groupQueue[:i], backend.groupQueue[i+1:]...)
				backend.groupMu.Unlock()
				return storage.ApplyResult{}, ctx.Err()
			}
		}
		backend.groupMu.Unlock()
		// Leader already claimed this request; wait for its exact durable outcome.
		<-req.done
		return req.result, req.err
	}
}

func (backend *Backend) runGroupCommitLeader() {
	for {
		backend.groupMu.Lock()
		if len(backend.groupQueue) == 0 {
			backend.groupActive = false
			backend.groupMu.Unlock()
			return
		}
		n := min(len(backend.groupQueue), maxGroupCommitBatch)
		group := make([]*groupCommitReq, n)
		copy(group, backend.groupQueue[:n])
		copy(backend.groupQueue, backend.groupQueue[n:])
		clear(backend.groupQueue[len(backend.groupQueue)-n:])
		backend.groupQueue = backend.groupQueue[:len(backend.groupQueue)-n]
		backend.groupMu.Unlock()

		backend.processCommitGroup(group)
		for _, req := range group {
			close(req.done)
		}
	}
}

func (backend *Backend) processCommitGroup(group []*groupCommitReq) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()

	backend.mu.Lock()
	hasFault := backend.commitFault != nil
	backend.mu.Unlock()
	if hasFault {
		for _, req := range group {
			req.result, req.err = backend.commitOneMemtable(req.ctx, req.batch, req.validated)
		}
		return
	}

	// Sort concurrent commit requests by CommitTS ascending so concurrent writers
	// arriving slightly out of timestamp order commit cleanly in strict order.
	if len(group) > 1 {
		slices.SortStableFunc(group, func(a, b *groupCommitReq) int {
			return cmp.Compare(a.batch.CommitTS, b.batch.CommitTS)
		})
	}

	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		for _, req := range group {
			req.err = storage.ErrClosed
		}
		return
	}
	staged := make([]stagedGroupCommit, 0, len(group))
	stagedByTxID := make(map[recoveryTxnID]int, len(group))
	stagingTxns := backend.transactions
	nextPos := backend.frontier.LogPosition
	minTS := backend.closedTimestamp()
	activeLeap := backend.state.Scales[len(backend.state.Scales)-1].Scale.LeapHistoryID

	for _, req := range group {
		if err := req.ctx.Err(); err != nil {
			req.err = err
			continue
		}
		id := recoveryTxnID(req.batch.TxID)
		if previous, found := backend.transactions.Get(id); found {
			if previous.Identity != req.validated.identity {
				req.err = storage.ErrTxnIDReused
			} else {
				req.result = previous.Result
			}
			continue
		}
		if idx, inGroup := stagedByTxID[id]; inGroup {
			if staged[idx].decision.Identity != req.validated.identity {
				req.err = storage.ErrTxnIDReused
			} else {
				staged[idx].duplicates = append(staged[idx].duplicates, req)
			}
			continue
		}
		if backend.pending != nil {
			if backend.pendingTxnIDMismatch(req.batch.TxID, req.validated.identity) {
				req.err = storage.ErrTxnIDReused
			} else {
				req.err = storage.ErrCommitIndeterminate
			}
			continue
		}
		if backend.metadataUncertain {
			req.err = storage.ErrCommitIndeterminate
			continue
		}
		if req.batch.CommitTS <= minTS || nextPos == math.MaxUint64 {
			req.err = storage.ErrInvalidMutation
			continue
		}
		if req.batch.Epoch.LeapHistory != activeLeap || req.batch.Epoch.Assurance == ([16]byte{}) || req.batch.Epoch.Config == ([32]byte{}) {
			req.err = clock.ErrClockEpochChanged
			continue
		}
		position := nextPos + 1
		result := storage.ApplyResult{
			AppliedCount: req.batch.Count(),
			CommitTS:     req.batch.CommitTS,
			ChecksumCRC:  req.batch.ChecksumCRC,
			LogPosition:  position,
		}
		updatedTxns, _, err := stagingTxns.Set(id, transactionRecord{Identity: req.validated.identity, Result: result}, backend.limits.IdentityBytes-stagingTxns.NodeBytes())
		if err != nil {
			req.err = err
			continue
		}
		stagingTxns = updatedTxns
		nextPos = position
		minTS = req.batch.CommitTS
		stagedByTxID[id] = len(staged)
		staged = append(staged, stagedGroupCommit{
			req:      req,
			position: position,
			decision: runDecision{
				Position:      position,
				TxID:          req.batch.TxID,
				CommitTS:      req.batch.CommitTS,
				Identity:      req.validated.identity,
				MutationCount: req.batch.Count(),
			},
			result:       result,
			transactions: stagingTxns,
		})
	}
	if len(staged) == 0 {
		backend.mu.Unlock()
		return
	}
	if backend.flushErr != nil && backend.memtable.totalBytes >= memtableHardMaxBytes(backend.limits) {
		if err := backend.flushMemtableLocked(context.Background(), 0); err != nil {
			backend.mu.Unlock()
			for i := range staged {
				staged[i].setError(err)
			}
			return
		}
	}
	commitsRoot := backend.directory.commits
	walBytes := backend.limits.WALBytes
	rowBytes := backend.limits.RowBytes
	nodeBudget := memtableNodeBudget(backend.limits)
	dbCipher := backend.cipher
	archiver := backend.archiver
	backend.mu.Unlock()

	// Write & fsync all staged WAL segments with ONE single directory sync!
	linked, walErr := writeCommitGroupWithCipher(context.Background(), commitsRoot, staged, walBytes, dbCipher)
	if walErr == nil && archiver != nil {
		for _, item := range staged {
			if archErr := archiver.archiveCommit(context.Background(), commitsRoot, item.position, item.result.CommitTS); archErr != nil {
				walErr = errors.Join(storage.ErrCommitIndeterminate, archErr)
				break
			}
		}
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()

	if walErr != nil {
		if linked == 0 && !errors.Is(walErr, storage.ErrCommitIndeterminate) {
			for i := range staged {
				staged[i].setError(walErr)
			}
			return
		}
		// One or more segments may have been linked on disk. Record ONLY the
		// prefix whose publication was attempted, each with its own cumulative
		// transaction tree snapshot (never attaching unlinked siblings), and
		// mark metadata uncertain until ResolveCommit reconciles disk state.
		uncertain := min(linked+1, len(staged))
		backend.pending = buildPendingChain(staged[:uncertain])
		if linked > 0 {
			backend.metadataUncertain = true
		}
		indetErr := errors.Join(storage.ErrCommitIndeterminate, walErr)
		for i := range staged {
			staged[i].setError(indetErr)
		}
		return
	}

	// Stage every durably WAL-committed batch into the in-memory memtable.
	for i := range staged {
		item := &staged[i]
		if err := backend.memtable.stageBatchWithBudget(context.Background(), item.decision, item.req.batch.Epoch, item.req.batch, rowBytes, nodeBudget); err != nil {
			// WAL segments staged[i:] are already durable on disk; record them
			// as pending and mark metadata uncertain so no subsequent commit
			// can reuse their LogPosition and ResolveCommit can recover them.
			backend.pending = buildPendingChain(staged[i:])
			backend.metadataUncertain = true
			indetErr := errors.Join(storage.ErrCommitIndeterminate, err)
			for j := i; j < len(staged); j++ {
				staged[j].setError(indetErr)
			}
			return
		}
		id := recoveryTxnID(item.req.batch.TxID)
		updated, _, setErr := backend.transactions.Set(id, transactionRecord{Identity: item.decision.Identity, Result: item.result}, backend.limits.IdentityBytes-backend.transactions.NodeBytes())
		if setErr != nil {
			backend.pending = buildPendingChain(staged[i:])
			backend.metadataUncertain = true
			indetErr := errors.Join(storage.ErrCommitIndeterminate, setErr)
			for j := i; j < len(staged); j++ {
				staged[j].setError(indetErr)
			}
			return
		}
		backend.transactions = updated
		backend.frontier = recoveredPosition{LogPosition: item.result.LogPosition, CommitTS: item.result.CommitTS}
		backend.publishedEpoch = item.req.batch.Epoch
		item.setResult(item.result)
	}

	if backend.memtable.shouldFlush(backend.limits) {
		if err := backend.flushMemtableLocked(context.Background(), 0); err != nil {
			// Data is already durable in WAL and visible in memtable; keep memtable
			// intact so a subsequent flush/compaction can retry cleanly.
			_ = err
		}
	}
}

func (backend *Backend) commitOneMemtable(ctx context.Context, batch storage.MutationBatch, validated validatedMutationBatch) (storage.ApplyResult, error) {
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrClosed
	}
	id := recoveryTxnID(batch.TxID)
	if previous, found := backend.transactions.Get(id); found {
		backend.mu.Unlock()
		if previous.Identity != validated.identity {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return previous.Result, nil
	}
	if backend.pending != nil {
		same := backend.pendingTxnIDMismatch(batch.TxID, validated.identity)
		backend.mu.Unlock()
		if same {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if backend.metadataUncertain {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if batch.CommitTS <= backend.closedTimestamp() || backend.frontier.LogPosition == math.MaxUint64 {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrInvalidMutation
	}
	active := backend.state.Scales[len(backend.state.Scales)-1]
	if batch.Epoch.LeapHistory != active.Scale.LeapHistoryID || batch.Epoch.Assurance == ([16]byte{}) || batch.Epoch.Config == ([32]byte{}) {
		backend.mu.Unlock()
		return storage.ApplyResult{}, clock.ErrClockEpochChanged
	}
	position := backend.frontier.LogPosition + 1
	result := storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}
	transactions, _, err := backend.transactions.Set(id, transactionRecord{Identity: validated.identity, Result: result}, backend.limits.IdentityBytes-backend.transactions.NodeBytes())
	if err != nil {
		backend.mu.Unlock()
		return storage.ApplyResult{}, err
	}
	if backend.flushErr != nil && backend.memtable.totalBytes >= memtableHardMaxBytes(backend.limits) {
		if err := backend.flushMemtableLocked(ctx, 0); err != nil {
			backend.mu.Unlock()
			return storage.ApplyResult{}, err
		}
	}
	decision := runDecision{Position: position, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: validated.identity, MutationCount: batch.Count()}
	commitsRoot := backend.directory.commits
	walBytes := backend.limits.WALBytes
	commitFault := backend.commitFault
	dbCipher := backend.cipher
	archiver := backend.archiver
	backend.mu.Unlock()

	walRes, err := writeCommitWithCipher(ctx, commitsRoot, position, batch, validated.identity, walBytes, commitFault, dbCipher)
	if err == nil && archiver != nil {
		if archErr := archiver.archiveCommit(ctx, commitsRoot, position, batch.CommitTS); archErr != nil {
			err = errors.Join(storage.ErrCommitIndeterminate, archErr)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if err != nil && !errors.Is(err, storage.ErrCommitIndeterminate) {
		return storage.ApplyResult{}, err
	}
	batchCopy := batch
	backend.pending = &pendingCommit{decision: decision, epoch: batch.Epoch, transactions: transactions, batch: &batchCopy}
	if err != nil {
		return storage.ApplyResult{}, err
	}
	if err := backend.publishMemtableCommit(ctx, walRes, batch); err != nil {
		return storage.ApplyResult{}, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return walRes, nil
}

func (backend *Backend) publishMemtableCommit(ctx context.Context, result storage.ApplyResult, batch storage.MutationBatch) error {
	pending := backend.pending
	expected, found := pending.transactions.Get(recoveryTxnID(pending.decision.TxID))
	if !found || result != expected.Result {
		return storage.ErrCorruptStorage
	}
	if backend.commitFault != nil {
		if err := backend.commitFault(commitBeforeReaderPublication); err != nil {
			return err
		}
	}
	if err := backend.memtable.stageBatchWithBudget(ctx, pending.decision, pending.epoch, batch, backend.limits.RowBytes, memtableNodeBudget(backend.limits)); err != nil {
		backend.metadataUncertain = true
		return err
	}
	backend.frontier = recoveredPosition{LogPosition: result.LogPosition, CommitTS: result.CommitTS}
	backend.publishedEpoch = pending.epoch
	backend.transactions = pending.transactions
	backend.pending = pending.next
	if backend.memtable.shouldFlush(backend.limits) {
		_ = backend.flushMemtableLocked(context.WithoutCancel(ctx), 0)
	}
	if backend.commitFault != nil {
		return backend.commitFault(commitReaderPublished)
	}
	return nil
}

func buildPendingChain(items []stagedGroupCommit) *pendingCommit {
	var head, tail *pendingCommit
	for i := range items {
		item := items[i]
		batchCopy := item.req.batch
		node := &pendingCommit{
			decision:     item.decision,
			epoch:        item.req.batch.Epoch,
			transactions: item.transactions,
			batch:        &batchCopy,
		}
		if head == nil {
			head = node
			tail = node
		} else {
			tail.next = node
			tail = node
		}
	}
	return head
}

func (backend *Backend) hasPendingTxn(id model.TxnID) bool {
	for p := backend.pending; p != nil; p = p.next {
		if p.decision.TxID == id {
			return true
		}
	}
	return false
}

func (backend *Backend) pendingTxnIDMismatch(id model.TxnID, identity [32]byte) bool {
	for p := backend.pending; p != nil; p = p.next {
		if p.decision.TxID == id && p.decision.Identity != identity {
			return true
		}
	}
	return false
}

func (backend *Backend) applyMutationDirect(ctx context.Context, batch storage.MutationBatch) (storage.ApplyResult, error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrClosed
	}
	validated, err := validateMutationBatch(ctx, batch)
	if err != nil {
		backend.mu.Unlock()
		return storage.ApplyResult{}, err
	}
	id := recoveryTxnID(batch.TxID)
	if previous, found := backend.transactions.Get(id); found {
		backend.mu.Unlock()
		if previous.Identity != validated.identity {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return previous.Result, nil
	}
	if backend.pending != nil {
		same := backend.pendingTxnIDMismatch(batch.TxID, validated.identity)
		backend.mu.Unlock()
		if same {
			return storage.ApplyResult{}, storage.ErrTxnIDReused
		}
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if backend.metadataUncertain {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrCommitIndeterminate
	}
	if batch.CommitTS <= backend.closedTimestamp() || backend.frontier.LogPosition == math.MaxUint64 {
		backend.mu.Unlock()
		return storage.ApplyResult{}, storage.ErrInvalidMutation
	}
	active := backend.state.Scales[len(backend.state.Scales)-1]
	if batch.Epoch.LeapHistory != active.Scale.LeapHistoryID || batch.Epoch.Assurance == ([16]byte{}) || batch.Epoch.Config == ([32]byte{}) {
		backend.mu.Unlock()
		return storage.ApplyResult{}, clock.ErrClockEpochChanged
	}
	position := backend.frontier.LogPosition + 1
	result := storage.ApplyResult{AppliedCount: batch.Count(), CommitTS: batch.CommitTS, ChecksumCRC: batch.ChecksumCRC, LogPosition: position}
	transactions, _, err := backend.transactions.Set(id, transactionRecord{Identity: validated.identity, Result: result}, backend.limits.IdentityBytes-backend.transactions.NodeBytes())
	if err != nil {
		backend.mu.Unlock()
		return storage.ApplyResult{}, err
	}
	directory := backend.directory
	runs := backend.runs
	preparation := backend.preparationLimits()
	walBytes := backend.limits.WALBytes
	publicationBytes := backend.limits.PublicationBytes
	commitFault := backend.commitFault
	archiver := backend.archiver
	backend.mu.Unlock()
	submitted, err := commitMutationRuns(ctx, directory, runs, position, validated, preparation, walBytes, publicationBytes, commitFault)
	if err == nil && archiver != nil {
		if archErr := archiver.archiveCommit(ctx, directory.commits, position, batch.CommitTS); archErr != nil {
			err = errors.Join(storage.ErrCommitIndeterminate, archErr)
		}
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if err != nil && !errors.Is(err, storage.ErrCommitIndeterminate) {
		return storage.ApplyResult{}, err
	}
	backend.pending = &pendingCommit{decision: submitted.Decision, epoch: batch.Epoch, transactions: transactions, admission: submitted.Admission}
	if err != nil {
		return storage.ApplyResult{}, err
	}
	if err := backend.publishCommit(ctx, submitted.Result); err != nil {
		return storage.ApplyResult{}, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return submitted.Result, nil
}

func (backend *Backend) publishCommit(ctx context.Context, result storage.ApplyResult) error {
	pending := backend.pending
	expected, found := pending.transactions.Get(recoveryTxnID(pending.decision.TxID))
	if !found || result != expected.Result {
		return storage.ErrCorruptStorage
	}
	if backend.commitFault != nil {
		if err := backend.commitFault(commitBeforeReaderPublication); err != nil {
			return err
		}
	}
	if err := pending.admission.publish(ctx); err != nil {
		return err
	}
	backend.frontier = recoveredPosition{LogPosition: result.LogPosition, CommitTS: result.CommitTS}
	backend.publishedEpoch = pending.epoch
	backend.transactions = pending.transactions
	backend.pending = nil
	// Admission is consumed; finish decision bookkeeping before exposing a fault.
	if backend.commitFault != nil {
		return backend.commitFault(commitReaderPublished)
	}
	return nil
}

func (backend *Backend) ResolveCommit(ctx context.Context, id model.TxnID) (storage.CommitResolution, error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return storage.CommitResolution{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.CommitResolution{}, err
	}
	if id == (model.TxnID{}) {
		return storage.CommitResolution{}, storage.ErrInvalidMutation
	}
	if previous, found := backend.transactions.Get(recoveryTxnID(id)); found {
		return storage.CommitResolution{State: storage.CommitCommitted, Result: previous.Result}, nil
	}
	if backend.metadataUncertain {
		if err := backend.resolveMetadata(ctx); err != nil {
			return storage.CommitResolution{}, err
		}
	}
	if backend.pending == nil || !backend.hasPendingTxn(id) {
		return storage.CommitResolution{State: storage.CommitNotCommitted}, nil
	}
	if backend.pending.admission == nil {
		// Pending memtable commit (single or group chain): WAL segments
		// commits/<pos>.wal are the commit decisions. Reconcile in strict
		// LogPosition order from the head of the pending chain.
		for backend.pending != nil && backend.pending.admission == nil {
			head := backend.pending
			record, err := openCommitWithCipher(ctx, backend.directory.commits, head.decision.Position, backend.limits.recovery().Batch, backend.cipher)
			if errors.Is(err, os.ErrNotExist) {
				if err := ctx.Err(); err != nil {
					return storage.CommitResolution{}, err
				}
				if err := syncCommitDirectory(backend.directory.commits); err != nil {
					return storage.CommitResolution{}, err
				}
				// Group commits link strictly in ascending LogPosition order,
				// so once a position is absent on disk no subsequent segment
				// in the chain was linked either.
				backend.pending = nil
				break
			}
			if err != nil {
				return storage.CommitResolution{}, err
			}
			if record.Batch.TxID != head.decision.TxID || record.Identity != head.decision.Identity ||
				record.Result.CommitTS != head.decision.CommitTS || record.Result.AppliedCount != head.decision.MutationCount {
				_ = record.Close()
				return storage.CommitResolution{}, storage.ErrCorruptStorage
			}
			if err := syncCommitDirectory(backend.directory.commits); err != nil {
				_ = record.Close()
				return storage.CommitResolution{}, err
			}
			pubErr := backend.publishMemtableCommit(ctx, record.Result, record.Batch)
			closeErr := record.Close()
			if err := errors.Join(pubErr, closeErr); err != nil {
				if head.decision.TxID == id {
					return storage.CommitResolution{State: storage.CommitCommitted, Result: record.Result}, err
				}
				return storage.CommitResolution{}, err
			}
		}
		if previous, found := backend.transactions.Get(recoveryTxnID(id)); found {
			return storage.CommitResolution{State: storage.CommitCommitted, Result: previous.Result}, nil
		}
		return storage.CommitResolution{State: storage.CommitNotCommitted}, nil
	}
	resolution, _, err := resolveMutationRuns(ctx, backend.directory, backend.pending.decision, backend.preparationLimits(), backend.limits.recovery().Batch)
	if err != nil {
		return resolution, err
	}
	switch resolution.State {
	case storage.CommitNotCommitted:
		if err := backend.pending.admission.Close(); err != nil {
			return resolution, err
		}
		backend.pending = nil
	case storage.CommitCommitted:
		if err := backend.publishCommit(ctx, resolution.Result); err != nil {
			return resolution, err
		}
	}
	return resolution, nil
}
