//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package neosepia

import (
	"errors"
	"os"
	"syscall"
)

func lockDatabaseFile(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errors.Join(ErrDatabaseLocked, err)
	}
	return err
}
