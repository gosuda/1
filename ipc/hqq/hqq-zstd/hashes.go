package zstd

import "gosuda.org/hqq/secureexec"

// Version follows the v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion> scheme,
// where 10600 corresponds to ZSTD_VERSION_NUMBER (Zstandard 1.6.0).
const (
	Version           = "v0.3.10600"
	ZstdVersionNumber = 10600
)

// ReleaseHashes maps "<GOOS>/<GOARCH>" to the SHA-256 digest of the prebuilt
// zstd_server binary published with this module version.
var ReleaseHashes = map[string]secureexec.Hash{
	"darwin/amd64":  mustHash("cfe719aec9a2100a49eab43554f16b3dfc3292eed55c45c7eb5f13c9e098e91f"),
	"darwin/arm64":  mustHash("1106e14a7f9063efa32640bb4b530f6e716fe96841f7aef06410c1f4fcaa17fd"),
	"linux/amd64":   mustHash("129cb3d9b3b186d4d9bc3de8e96c8fbcbd21c57450b603342c79f0aad897d2d4"),
	"linux/arm64":   mustHash("85261e1310fe21df96d87e169f64c305f4dab052deb2a27e7f9e9013ef974cc5"),
	"windows/amd64": mustHash("30f3aaed2d6fd69eebd410829b44c6c80962b485ea13df8cd19666c4d2697c55"),
	"windows/arm64": mustHash("6658dc1976a072de6d1ad1dfe53feaa671c9d66816217156410e1bd33c698181"),
}

// ExpectedHash returns the pinned SHA-256 digest of zstd_server for goos/goarch.
func ExpectedHash(goos, goarch string) (secureexec.Hash, bool) {
	h, ok := ReleaseHashes[goos+"/"+goarch]
	return h, ok
}

func mustHash(s string) secureexec.Hash {
	h, err := secureexec.ParseHash(s)
	if err != nil {
		panic(err)
	}
	return h
}
