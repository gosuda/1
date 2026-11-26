//go:build darwin && arm64

package embedded

import _ "embed"

//go:embed bin/hqq-sqlite-server_darwin_arm64
var serverBinary []byte

const binaryName = "hqq-sqlite-server_darwin_arm64"
