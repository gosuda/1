package supersql

import "github.com/gosuda/supersql-alpha/clock"

// NewTestClock exposes the deterministic in-tree test clock to the external
// example package, which cannot reach the unexported newTestClock. It exists
// only in the test binary.
func NewTestClock() clock.Clock { return newTestClock() }
