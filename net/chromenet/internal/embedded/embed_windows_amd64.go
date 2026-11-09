//go:build windows && amd64

package embedded

import _ "embed"

//go:embed bin/chrome_net_server_windows_amd64.exe
var serverBinary []byte

const binaryName = "chrome_net_server_windows_amd64.exe"
