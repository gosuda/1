package maintenance

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type stagedTree struct {
	parent, root      *os.Root
	name, destination string
	path              string
	published         bool
}

func newStagedTree(ctx context.Context, destination string) (_ *stagedTree, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if destination == "" {
		return nil, ErrInvalid
	}
	path, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(path)
	if base == "." || base == string(filepath.Separator) {
		return nil, ErrInvalid
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	stage := &stagedTree{parent: parent, destination: base, path: path}
	defer func() {
		if err != nil {
			err = errors.Join(err, stage.close())
		}
	}()
	if err := checkDestination(ctx, parent, base); err != nil {
		return nil, err
	}
	name := ".checkpoint-copy-" + rand.Text()
	if err := parent.Mkdir(name, 0700); err != nil {
		return nil, err
	}
	stage.name = name
	stage.root, err = parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	for _, directory := range [...]string{"commits", "runs"} {
		if err := stage.root.Mkdir(directory, 0700); err != nil {
			return nil, err
		}
	}
	return stage, nil
}

func checkDestination(ctx context.Context, parent *os.Root, name string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrDestinationOccupied
	}
	root, err := openChildRoot(parent, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	entries, err := directory.ReadDir(1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) != 0 {
		return ErrDestinationOccupied
	}
	return ctx.Err()
}

func (stage *stagedTree) publish(ctx context.Context) error {
	for _, directory := range [...]string{"commits", "runs", "."} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDirectory(stage.root, directory); err != nil {
			return err
		}
	}
	err := stage.root.Close()
	stage.root = nil
	if err != nil {
		return err
	}
	if err := checkDestination(ctx, stage.parent, stage.destination); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A directory rename cannot replace a nonempty directory or a regular file.
	// This also protects a destination populated after checkDestination.
	if err := stage.parent.Rename(stage.name, stage.destination); err != nil {
		return err
	}
	stage.name = ""
	stage.published = true
	// Rename is the commit point. A subsequent durability error leaves the
	// published tree in place rather than deleting potentially adopted data.
	if err := syncDirectory(stage.parent, "."); err != nil {
		return &PublicationError{Destination: stage.path, Err: fmt.Errorf("sync parent directory: %w", err)}
	}
	return nil
}

func (stage *stagedTree) close() (err error) {
	if stage.root != nil {
		err = stage.root.Close()
		stage.root = nil
	}
	if stage.name != "" {
		err = errors.Join(err, stage.parent.RemoveAll(stage.name), syncDirectory(stage.parent, "."))
		stage.name = ""
	}
	if stage.parent != nil {
		err = errors.Join(err, stage.parent.Close())
		stage.parent = nil
	}
	if err != nil && stage.published {
		return &PublicationError{Destination: stage.path, Err: err}
	}
	return err
}
