package embedded

import "gosuda.org/hqq/secureexec"

// Version metadata matching the monorepo release standard.
const (
	Version = "v0.1.0"
)

// ReleaseHashes maps "<GOOS>/<GOARCH>" to the SHA-256 digest of the prebuilt
// chrome_net_server binary packaged with this module.
var ReleaseHashes = map[string]secureexec.Hash{
	"darwin/amd64":  mustHash("ec5e7a86fc84876b558720a082a2a4b0d411d1abb408f8157f212892f4d2bd01"),
	"darwin/arm64":  mustHash("3081120d7f95794aefc9279b2f355efd7fc6fcf0c8731e0c8d8e0785f87d0b57"),
	"linux/amd64":   mustHash("1ba7af832e70a7223f15435b73ce7568efc98c8c6ce2011e8f4bc0151f0fbac8"),
	"linux/arm64":   mustHash("8f6a7d53f227b1182afe0ac54c7d948f30517ae0a7a21b0f541d3b467e558536"),
	"windows/amd64": mustHash("6b9ed98f0f404c29e845d41820f155a2993586f4adba623a0ef8a8b887b2decf"),
	"windows/arm64": mustHash("423a41ce33acfbc1ff30c03013f98ca88f97a140fd9e0c45696f2475c522c4cb"),
}

// ExpectedHash returns the pinned SHA-256 digest of chrome_net_server for goos/goarch.
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
