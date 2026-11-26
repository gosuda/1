package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
)

func inspectDatabaseDirectory(ctx context.Context, path string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, statErr := root.Lstat(databaseStateName)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return ErrIncompatibleFormat
		}
		valid, err := stateMagicPrefix(root, databaseStateName, false)
		if err != nil {
			return err
		}
		if !valid {
			return ErrIncompatibleFormat
		}
		return nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(16)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			switch entry.Name() {
			case ".lock":
				if !entry.Type().IsRegular() {
					return ErrIncompatibleFormat
				}
			case "commits", "runs":
				if !entry.IsDir() {
					return ErrIncompatibleFormat
				}
				empty, err := emptyNativeDirectory(root, entry.Name())
				if err != nil {
					return err
				}
				if !empty {
					return ErrIncompatibleFormat
				}
			case databaseStatePending:
				if !entry.Type().IsRegular() {
					return ErrIncompatibleFormat
				}
				valid, err := stateMagicPrefix(root, databaseStatePending, true)
				if err != nil {
					return err
				}
				if !valid {
					return ErrIncompatibleFormat
				}
			default:
				return ErrIncompatibleFormat
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

func stateMagicPrefix(root *os.Root, name string, partial bool) (_ bool, err error) {
	file, err := root.Open(name)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	var magic [len(databaseStateMagic)]byte
	count, readErr := io.ReadFull(file, magic[:])
	if partial && (readErr == io.EOF || readErr == io.ErrUnexpectedEOF) {
		return bytes.Equal(magic[:count], []byte(databaseStateMagic)[:count]), nil
	}
	if readErr != nil {
		return false, readErr
	}
	return string(magic[:]) == databaseStateMagic, nil
}

func emptyNativeDirectory(root *os.Root, name string) (_ bool, err error) {
	directory, err := root.Open(name)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	entries, readErr := directory.ReadDir(1)
	if readErr != nil && readErr != io.EOF {
		return false, readErr
	}
	return len(entries) == 0, nil
}
