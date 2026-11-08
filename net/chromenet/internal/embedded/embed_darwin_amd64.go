//go:build darwin && amd64

package embedded

import _ "embed"

//go:embed bin/chrome_net_server_darwin_amd64
var serverBinary []byte

const binaryName = "chrome_net_server_darwin_amd64"
