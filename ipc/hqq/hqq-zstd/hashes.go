package zstd

import "gosuda.org/hqq/secureexec"

// Version follows the v<WrapperMajor>.<WrapperMinor>.<NormalizedCVersion> scheme,
// where 10507 corresponds to ZSTD_VERSION_NUMBER (Zstandard 1.5.7).
const (
	Version           = "v0.3.10507"
	ZstdVersionNumber = 10507
)

// ReleaseHashes maps "<GOOS>/<GOARCH>" to the SHA-256 digest of the prebuilt
// zstd_server binary published with this module version.
var ReleaseHashes = map[string]secureexec.Hash{
	"darwin/amd64":  mustHash("90eeb7cdbc08a2018722f4965cd9e3d9c387e97b9b4c7910f2d2e1bde24c6cdb"),
	"darwin/arm64":  mustHash("7649b1b45dacd934cde8149f35f4fcc3937f6486fbac23ce976466719b3752bb"),
	"linux/amd64":   mustHash("b9ed44f83b962ecc135ceb32139476597603f7e1eec09cf685c49fc2b4233834"),
	"linux/arm64":   mustHash("14241982f6ee31d2b4a073425473c61726a85871347c996deecfa22a3bdcfda0"),
	"windows/amd64": mustHash("a09cefb43ae4f7ad1d8c28084217afc593eaa4965f111eed70dc0aafd47ac9ac"),
	"windows/arm64": mustHash("dd71d248658937d48a782bb42260f6f7f517215ff61b460403ddae923e0cff42"),
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
