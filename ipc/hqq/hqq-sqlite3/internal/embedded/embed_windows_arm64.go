//go:build windows && arm64

package embedded

import _ "embed"

//go:embed bin/hqq-sqlite-server_windows_arm64.exe
var serverBinary []byte

const binaryName = "hqq-sqlite-server_windows_arm64.exe"
