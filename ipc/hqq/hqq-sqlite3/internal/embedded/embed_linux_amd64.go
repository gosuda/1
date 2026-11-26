//go:build linux && amd64

package embedded

import _ "embed"

//go:embed bin/hqq-sqlite-server_linux_amd64
var serverBinary []byte

const binaryName = "hqq-sqlite-server_linux_amd64"
