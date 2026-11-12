//go:build darwin && amd64

package embedded

import _ "embed"

//go:embed bin/hqq-sqlite-server_darwin_amd64
var serverBinary []byte

const binaryName = "hqq-sqlite-server_darwin_amd64"
