//go:build !(linux && (amd64 || arm64)) && !(darwin && (amd64 || arm64)) && !(windows && (amd64 || arm64))

package embedded

var serverBinary []byte

const binaryName = ""
