package sqlite

import "gosuda.org/hqq/secureexec"

// Version follows the v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion> scheme,
// where 3540100 corresponds to SQLITE_VERSION_NUMBER (SQLite 3.54.1).
const (
	Version             = "v0.3.3540100"
	SQLiteVersionNumber = 3540100
)

// ReleaseHashes maps "<GOOS>/<GOARCH>" to the SHA-256 digest of the prebuilt
// hqq-sqlite-server binary published with this module version.
var ReleaseHashes = map[string]secureexec.Hash{
	"darwin/amd64":  mustHash("e4377970783727fd3d52a26d80643d452ebb753e4d0b88b918ca124217920436"),
	"darwin/arm64":  mustHash("4989cf6404402ecfafe6071495a73c99b8f2e08e3ef54caae05cd9463cf0dbe0"),
	"linux/amd64":   mustHash("602c78a15eb02338db0746f77a91a3db63b9f232bd746e0de91d79ae29bbf55c"),
	"linux/arm64":   mustHash("d2285b178a1ced9fc2149199e3798a59509489c89e4d86b07efc4e44702f7c20"),
	"windows/amd64": mustHash("0f31154de7e0fc9da5cd60412e062e19fc62915a6811d2514abe359899101ec9"),
	"windows/arm64": mustHash("a625132fa80c9d655db77f4d7f5cd51a2ed467971a34605a726df14933be781e"),
}

// ExpectedHash returns the pinned SHA-256 digest of hqq-sqlite-server for goos/goarch.
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
