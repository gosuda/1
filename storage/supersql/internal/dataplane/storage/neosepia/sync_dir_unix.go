//go:build !windows

package neosepia

import (
	"errors"
	"os"
)

func syncCommitDirectory(root *os.Root) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	return directory.Sync()
}
