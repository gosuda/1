//go:build windows

package maintenance

import "os"

// Windows does not support FlushFileBuffers on directory handles.
func syncDirectory(*os.Root, string) error {
	return nil
}
