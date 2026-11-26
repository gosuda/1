package grt

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gosuda.org/grt/internal/engine"
)

// TestMain fails the package if any scenario drove a state change outside
// the generated FSM tables.
func TestMain(m *testing.M) {
	code := m.Run()
	if n, kinds := engine.FSMViolations(); n != 0 {
		fmt.Fprintf(os.Stderr, "FAIL: %d FSM transitions outside the verified IR:\n\t%s\n",
			n, strings.Join(kinds, "\n\t"))
		code = 1
	}
	os.Exit(code)
}
