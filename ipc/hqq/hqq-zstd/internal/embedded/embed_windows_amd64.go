//go:build windows && amd64

package embedded

import _ "embed"

//go:embed bin/zstd_server_windows_amd64.exe
var serverBinary []byte

const binaryName = "zstd_server_windows_amd64.exe"
