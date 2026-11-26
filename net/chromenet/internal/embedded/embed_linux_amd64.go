//go:build linux && amd64

package embedded

import _ "embed"

//go:embed bin/chrome_net_server_linux_amd64
var serverBinary []byte

const binaryName = "chrome_net_server_linux_amd64"
