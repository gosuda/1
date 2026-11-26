//go:build windows

package neosepia

import "os"

// Windows does not support FlushFileBuffers on directory handles.
func syncCommitDirectory(*os.Root) error {
	return nil
}
