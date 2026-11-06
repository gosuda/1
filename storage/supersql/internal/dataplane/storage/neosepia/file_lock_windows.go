package neosepia

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockDatabaseFile(file *os.File) error {
	var overlap syscall.Overlapped
	result, _, err := lockFileEx.Call(file.Fd(), 3, 0, 0xffffffff, 0xffffffff, uintptr(unsafe.Pointer(&overlap)))
	if result != 0 {
		return nil
	}
	if errors.Is(err, syscall.Errno(33)) {
		return errors.Join(ErrDatabaseLocked, err)
	}
	return err
}
