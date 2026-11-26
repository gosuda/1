//go:build windows && arm64

package embedded

import _ "embed"

//go:embed bin/zstd_server_windows_arm64.exe
var serverBinary []byte

const binaryName = "zstd_server_windows_arm64.exe"
