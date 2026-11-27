//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package neosepia

import "os"

func lockDatabaseFile(*os.File) error { return ErrLockUnsupported }
