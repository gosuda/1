// Package launcher starts the HQQ SQLite server (sqlite3/server) as a
// supervised child process and returns a database/sql handle to it.
//
// Start verifies the server binary with secureexec before it runs: the expected
// digest comes from Config.SHA256 or from a signed release manifest
// (secureexec/manifest) whose signature is checked first. The server is started
// with a parent lifeline (--parent-stdin), so it shuts down gracefully when this
// process exits, including when it is killed with SIGKILL.
//
// This package is the SQLite layer over gosuda.org/hqq/launch, which starts any
// HQQ server: it builds the server's command line from Config, holds the
// embedded release key (ReleasePublicKey), and starts the process with
// launch.Start. Server, StartError, and ReadyTimeoutError alias the types in
// launch, and the Err sentinels other than ErrNoReleaseKey alias launch's
// sentinels.
//
// Open and OpenManaged combine Start with sqlite.NewConnector.
//
// See README.md for the threat model, lifecycle guarantees, and release
// verification workflow.
package launcher
