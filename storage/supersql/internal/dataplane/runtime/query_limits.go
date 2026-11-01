package runtime

// Constrain lowers invocation caps without weakening the host or shared budgets.
// It must run before allocating invocation state. Zero spill bytes forbids spill.
func (q *Query) Constrain(workBytes, spillBytes int64) error {
	if workBytes <= 0 || spillBytes < 0 {
		return ErrInvalidConfig
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	q.work.mu.Lock()
	defer q.work.mu.Unlock()
	q.spill.mu.Lock()
	defer q.spill.mu.Unlock()
	if q.work.live > workBytes || q.spill.live > spillBytes {
		return resourceError("53200", "apply session resource limits", ErrBudget)
	}
	q.work.limit = min(q.work.limit, workBytes)
	q.spill.limit = min(q.spill.limit, spillBytes)
	return nil
}
