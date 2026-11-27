//go:build windows

package supersql

// Windows does not support FlushFileBuffers on directory handles.
func syncDirectory(string) error {
	return nil
}
