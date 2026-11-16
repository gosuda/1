//go:build linux && amd64

package embedded

import _ "embed"

//go:embed bin/zstd_server_linux_amd64
var serverBinary []byte

const binaryName = "zstd_server_linux_amd64"
