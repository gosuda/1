//go:build linux && arm64

package embedded

import _ "embed"

//go:embed bin/chrome_net_server_linux_arm64
var serverBinary []byte

const binaryName = "chrome_net_server_linux_arm64"
