package engine

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestMain fails the package if any test drove a state change outside the
// generated FSM tables.
func TestMain(m *testing.M) {
	fsmPanicOnViolation = os.Getenv("GRT_FSM_PANIC") == "1"
	code := m.Run()
	if n, kinds := FSMViolations(); n != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d FSM transitions outside the verified IR:\n\t%s\n",
			n, strings.Join(kinds, "\n\t"))
		code = 1
	}
	os.Exit(code)
}
