//go:build linux && arm64

package embedded

import _ "embed"

//go:embed bin/hqq-sqlite-server_linux_arm64
var serverBinary []byte

const binaryName = "hqq-sqlite-server_linux_arm64"
