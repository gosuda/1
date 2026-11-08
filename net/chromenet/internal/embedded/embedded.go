package embedded

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gosuda.org/hqq/secureexec"
)

// ErrUnsupportedPlatform is returned when no prebuilt server binary is embedded
// for the current OS and architecture.
var ErrUnsupportedPlatform = errors.New("embedded: no prebuilt server binary for current platform")

// Bytes returns the embedded server binary bytes and filename for the current
// OS and architecture, or (nil, "", false) if none is embedded.
func Bytes() ([]byte, string, bool) {
	if len(serverBinary) == 0 || binaryName == "" {
		return nil, "", false
	}
	return serverBinary, binaryName, true
}

// Materialize verifies that the embedded server binary matches expected and
// writes it atomically into a per-digest directory under the user cache
// directory (falling back to os.TempDir), returning the path to the executable.
func Materialize(expected secureexec.Hash) (string, error) {
	data, name, ok := Bytes()
	if !ok {
		return "", ErrUnsupportedPlatform
	}
	got := secureexec.Hash(sha256.Sum256(data))
	if got != expected {
		return "", &secureexec.HashMismatchError{Path: name, Got: got, Want: expected}
	}
	dir, err := cacheDir(expected)
	if err != nil {
		return "", err
	}
	target := filepath.Join(dir, name)
	if h, _, err := secureexec.HashFile(target); err == nil && h == expected {
		return target, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("embedded: create temp binary: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("embedded: write binary: %w", err)
	}
	if err := tmp.Chmod(0o700); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("embedded: chmod binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("embedded: close binary: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		if h, _, herr := secureexec.HashFile(target); herr == nil && h == expected {
			return target, nil
		}
		return "", fmt.Errorf("embedded: install binary: %w", err)
	}
	cleanup = false
	return target, nil
}

func cacheDir(expected secureexec.Hash) (string, error) {
	if base, err := os.UserCacheDir(); err == nil && base != "" {
		dir := filepath.Join(base, "hqq", "bin", expected.String())
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir, nil
		}
	}
	dir := filepath.Join(os.TempDir(), "hqq-bin-"+expected.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("embedded: create cache dir: %w", err)
	}
	return dir, nil
}
