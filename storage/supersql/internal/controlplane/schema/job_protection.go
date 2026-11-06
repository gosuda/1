package schema

import (
	"context"
	"errors"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type ProtectedSnapshots interface {
	HistoricalSnapshot(context.Context, model.Timestamp) (storage.SnapshotReader, error)
}
type JobProtectionOptions struct {
	Jobs        *JobStore
	Snapshots   ProtectedSnapshots
	MaxJobs     int
	MemoryBytes uint64
}

type jobProtection struct {
	id        JobID
	timestamp model.Timestamp
	reader    storage.SnapshotReader
}

// JobProtections owns database-lifetime snapshot pins for durable jobs. Restore
// must succeed before GC or job scheduling starts. A caller retains a newly
// chosen snapshot across the job commit, then transfers it through Retain only
// after the commit is confirmed. Unknown commits keep that caller ownership.
// Close belongs after job/GC workers stop and before the backend closes.
type JobProtections struct {
	mu              sync.Mutex
	options         JobProtectionOptions
	entries         []jobProtection
	ready, closed   bool
	fence, closeErr error
}

var ErrJobProtectionNotReady = &model.Error{Code: "55000", Message: "schema job protections are not restored"}

func NewJobProtections(options JobProtectionOptions) (*JobProtections, error) {
	if options.Jobs == nil || options.Snapshots == nil || options.MaxJobs <= 0 || options.MemoryBytes == 0 {
		return nil, ErrJobLimit
	}
	base := uint64(unsafe.Sizeof(JobProtections{}))
	entry := uint64(unsafe.Sizeof(jobProtection{}))
	if base > options.MemoryBytes || uint64(options.MaxJobs) > (options.MemoryBytes-base)/(2*entry) {
		return nil, ErrJobLimit
	}
	return &JobProtections{options: options, entries: make([]jobProtection, 0, options.MaxJobs)}, nil
}
func (p *JobProtections) index(id JobID) (int, bool) {
	return slices.BinarySearchFunc(p.entries, id, func(entry jobProtection, id JobID) int {
		if entry.id < id {
			return -1
		}
		if entry.id > id {
			return 1
		}
		return 0
	})
}
func protectionTimestamp(reader storage.SnapshotReader, timestamp model.Timestamp) error {
	if reader == nil || timestamp <= 0 {
		return ErrJobRecord
	}
	if exact, ok := reader.(interface{ Timestamp() model.Timestamp }); ok && exact.Timestamp() != timestamp {
		return storage.ErrSnapshotNotClosed
	}
	return nil
}

// Retain takes exclusive reader ownership only on success. Errors leave the
// caller's pin alive, including after a confirmed durable protection commit.
// A successful duplicate closes the redundant pin; never pass an owned pin twice.
func (p *JobProtections) Retain(ctx context.Context, job Job, reader storage.SnapshotReader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Terminal() || job.ID == 0 || job.ProtectedTimestamp <= 0 || job.ProtectedTimestamp != job.SnapshotTS {
		return ErrJobRecord
	}
	if err := protectionTimestamp(reader, job.ProtectedTimestamp); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return storage.ErrClosed
	}
	if !p.ready {
		return ErrJobProtectionNotReady
	}
	if p.fence != nil {
		return p.fence
	}
	index, exists := p.index(job.ID)
	if exists {
		if p.entries[index].timestamp != job.ProtectedTimestamp {
			return ErrJobRecord
		}
		return reader.Close()
	}
	if len(p.entries) == p.options.MaxJobs {
		return ErrJobLimit
	}
	p.entries = append(p.entries, jobProtection{})
	copy(p.entries[index+1:], p.entries[index:len(p.entries)-1])
	p.entries[index] = jobProtection{id: job.ID, timestamp: job.ProtectedTimestamp, reader: reader}
	return nil
}

// Restore adopts all still-protected durable jobs from one recovered catalog
// snapshot. Failure closes only newly opened pins and never marks GC ready.
func (p *JobProtections) Restore(ctx context.Context, journal storage.SnapshotReader) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return storage.ErrClosed
	}
	if p.fence != nil {
		return p.fence
	}
	staged := make([]jobProtection, 0, p.options.MaxJobs-len(p.entries))
	defer func() {
		if err != nil {
			for _, entry := range staged {
				if cleanup := entry.reader.Close(); cleanup != nil {
					err = errors.Join(err, cleanup)
					p.fence = errors.Join(p.fence, cleanup)
					p.entries = append(p.entries, entry)
				}
			}
		}
	}()
	err = p.options.Jobs.Scan(ctx, journal, func(job Job) error {
		if job.Terminal() || job.ProtectedTimestamp == 0 {
			return nil
		}
		if index, exists := p.index(job.ID); exists {
			if p.entries[index].timestamp != job.ProtectedTimestamp {
				return ErrJobRecord
			}
			return nil
		}
		if len(staged)+len(p.entries) == p.options.MaxJobs {
			return ErrJobLimit
		}
		reader, err := p.options.Snapshots.HistoricalSnapshot(ctx, job.ProtectedTimestamp)
		if err != nil {
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			return err
		}
		if err := protectionTimestamp(reader, job.ProtectedTimestamp); err != nil {
			if reader != nil {
				err = errors.Join(err, reader.Close())
			}
			return err
		}
		staged = append(staged, jobProtection{id: job.ID, timestamp: job.ProtectedTimestamp, reader: reader})
		return nil
	})
	if err != nil {
		return err
	}
	p.entries = append(p.entries, staged...)
	clear(staged)
	slices.SortFunc(p.entries, func(a, b jobProtection) int {
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	p.ready = true
	return nil
}

// Release requires a visible terminal job record with protection removed. A
// stale nonterminal snapshot cannot accidentally release a live job's pin.
func (p *JobProtections) Release(ctx context.Context, journal JobReader, id JobID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return storage.ErrClosed
	}
	if !p.ready {
		return ErrJobProtectionNotReady
	}
	if p.fence != nil {
		return p.fence
	}
	job, err := p.options.Jobs.Get(ctx, journal, id)
	if err != nil {
		return err
	}
	if !job.Terminal() || job.ProtectedTimestamp != 0 {
		return ErrJobTransition
	}
	index, exists := p.index(id)
	if !exists {
		return nil
	}
	if err := p.entries[index].reader.Close(); err != nil {
		p.fence = err
		return err
	}
	p.entries = slices.Delete(p.entries, index, index+1)
	return nil
}

func (p *JobProtections) Require(id JobID, timestamp model.Timestamp) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return storage.ErrClosed
	}
	if !p.ready {
		return ErrJobProtectionNotReady
	}
	if p.fence != nil {
		return p.fence
	}
	index, exists := p.index(id)
	if !exists || timestamp <= 0 || p.entries[index].timestamp != timestamp {
		return ErrJobProtectionNotReady
	}
	return nil
}

func (p *JobProtections) Oldest() (model.Timestamp, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, false, storage.ErrClosed
	}
	if !p.ready {
		return 0, false, ErrJobProtectionNotReady
	}
	if p.fence != nil {
		return 0, false, p.fence
	}
	var oldest model.Timestamp
	for _, entry := range p.entries {
		if oldest == 0 || entry.timestamp < oldest {
			oldest = entry.timestamp
		}
	}
	return oldest, oldest != 0, nil
}
func (p *JobProtections) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	p.closeErr = p.fence
	for _, entry := range p.entries {
		p.closeErr = errors.Join(p.closeErr, entry.reader.Close())
	}
	p.entries = nil
	p.options = JobProtectionOptions{}
	return p.closeErr
}
