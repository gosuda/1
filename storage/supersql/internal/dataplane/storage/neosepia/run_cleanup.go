package neosepia

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func (set *runSet) activeRunNames(ctx context.Context) (map[string]struct{}, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil, storage.ErrClosed
	}
	names := make(map[string]struct{}, int(set.files)+1)
	for generation := set.head; generation != nil; generation = generation.parent {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if generation.baseGeneration != 0 {
			names[baseManifestName(generation.baseGeneration)] = struct{}{}
		} else {
			names[runManifestName(generation.position)] = struct{}{}
		}
		for _, file := range generation.files {
			names[file.descriptor.Name] = struct{}{}
		}
	}
	return names, nil
}

func cleanupObsoleteRuns(ctx context.Context, root *os.Root, set *runSet) (err error) {
	active, err := set.activeRunNames(ctx)
	if err != nil {
		return err
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
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return os.ErrInvalid
			}
			name := entry.Name()
			if _, retained := active[name]; retained {
				continue
			}
			if name != baseManifestPending && !obsoleteRunArtifactName(name) {
				continue
			}
			if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return syncCommitDirectory(root)
}

func obsoleteRunArtifactName(name string) bool {
	if strings.HasPrefix(name, "base-") && strings.HasSuffix(name, ".runs") {
		return true
	}
	if (strings.HasPrefix(name, "c-") || strings.HasPrefix(name, "f-")) && strings.HasSuffix(name, ".sst") {
		return true
	}
	if len(name) == 25 && strings.HasSuffix(name, ".runs") {
		return decimalPrefix(name[:20])
	}
	if len(name) == 33 && name[20] == '-' && strings.HasSuffix(name, ".sst") {
		return decimalPrefix(name[:20])
	}
	return false
}

func decimalPrefix(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
