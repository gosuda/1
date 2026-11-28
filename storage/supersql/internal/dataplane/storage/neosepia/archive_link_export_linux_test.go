package neosepia

import (
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

// RejectArchiveHardlinksForTest reaches the fallback through public SQL and
// PITR APIs in the external test package. It is absent from production builds.
func RejectArchiveHardlinksForTest(t *testing.T) *atomic.Uint64 {
	t.Helper()
	previous := archiveLink
	calls := new(atomic.Uint64)
	archiveLink = func(_ *os.Root, oldname, newname string) error {
		calls.Add(1)
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EOPNOTSUPP}
	}
	t.Cleanup(func() { archiveLink = previous })
	return calls
}
