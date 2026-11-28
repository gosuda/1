//go:build !linux

package neosepia

import (
	"errors"
	"os"
)

// No atomic no-replace operation exists on this platform, so report
// errors.ErrUnsupported. publishArchivedFile routes that sentinel to a
// fail-closed error instead of replacing the destination.
func renameArchivedFileNoReplace(_ *os.Root, _, _ string) error {
	return errors.ErrUnsupported
}
