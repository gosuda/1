// Package zstd provides a zero-cgo Go client for Zstandard (libzstd) generated
// by hqq-bindgen from zstd.idl, paired with the out-of-process C11 server in
// server/. See README.md for the architecture, ring geometry, and benchmark
// comparisons against in-process cgo.
//
// Regenerate the Go client and C server glue after modifying zstd.idl:
//
//	go generate ./...
package zstd

//go:generate go run gosuda.org/hqq/cmd/hqq-bindgen -go-out . -go-strip ZSTD_ -c-out server zstd.idl
