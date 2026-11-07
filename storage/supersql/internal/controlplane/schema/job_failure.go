package schema

import "bytes"

// IndexJobFailure retains a canonical failing key, never a row payload. The
// cause preserves SQLSTATE and storage errors; Key is independently owned.
type IndexJobFailure struct {
	Key   []byte
	Cause error
}

func (failure *IndexJobFailure) Error() string { return "index job operation failed" }
func (failure *IndexJobFailure) Unwrap() error { return failure.Cause }

func indexJobFailure(key []byte, cause error) error {
	if cause == nil {
		return nil
	}
	return &IndexJobFailure{Key: bytes.Clone(key), Cause: cause}
}
