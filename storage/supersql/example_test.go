package supersql_test

import (
	"context"
	"os"
)

// ExampleOpenConnector runs the README quick start verbatim: OpenConnector uses
// localclock.New(), sql.OpenDB adapts it, and the printed account proves the
// committed rows read back.
func ExampleOpenConnector() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "supersql-example-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	if err := runReadme(ctx, dir, os.Stdout); err != nil {
		panic(err)
	}

	// Output: Account: 1 | Alice | 50000
}
