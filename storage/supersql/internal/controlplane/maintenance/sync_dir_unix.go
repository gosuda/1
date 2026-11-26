//go:build !windows

package maintenance

import (
	"errors"
	"os"
)

func syncDirectory(root *os.Root, name string) (err error) {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	return directory.Sync()
}
