//go:build !windows

package zstd_test

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// rssOf returns the resident set size of a process in KiB through ps, which macOS and Linux share.
func rssOf(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rss %q: %w", out, err)
	}
	return n, nil
}
