//go:build !windows

package supersql

import (
	"errors"
	"os"
)

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	syncErr := handle.Sync()
	return errors.Join(syncErr, handle.Close())
}
