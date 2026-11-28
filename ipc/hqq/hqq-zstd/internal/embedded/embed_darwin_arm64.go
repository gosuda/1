//go:build darwin && arm64

package embedded

import _ "embed"

//go:embed bin/zstd_server_darwin_arm64
var serverBinary []byte

const binaryName = "zstd_server_darwin_arm64"
