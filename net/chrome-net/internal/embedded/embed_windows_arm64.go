//go:build windows && arm64

package embedded

import _ "embed"

//go:embed bin/chrome_net_server_windows_arm64.exe
var serverBinary []byte

const binaryName = "chrome_net_server_windows_arm64.exe"
