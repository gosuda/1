package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// coveredRoots are the directory trees whose every file must be attributed to a
// notice entry.
var coveredRoots = []string{
	"testdata",
	"internal/zpgparse",
	"internal/query/builtin/snowball",
}

// coveredGlobs are the generated files outside those trees that must be
// attributed to a notice entry.
var coveredGlobs = []string{
	"internal/query/builtin/*_generated.go",
	"internal/query/bind/*_generated.go",
}

// headerBytes is how much of each covered file is read to build the header map.
const headerBytes = 4096

func main() {
	notices := flag.String("notices", "THIRD_PARTY_NOTICES.md", "path to the third-party notices file")
	root := flag.String("root", ".", "module root directory")
	skipHeaders := flag.Bool("skip-headers", false, "skip rule (c): generated files must carry '// Source:' and '// License:' headers")
	flag.Parse()

	findings, err := run(*notices, *root, *skipHeaders)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensecheck:", err)
		os.Exit(2)
	}
	for _, finding := range findings {
		fmt.Println(finding)
	}
	if len(findings) > 0 {
		os.Exit(1)
	}
}

// run collects every tool input and returns the findings of all rules.
func run(noticesPath, root string, skipHeaders bool) ([]string, error) {
	notices, err := os.ReadFile(noticesPath)
	if err != nil {
		return nil, err
	}
	modules, err := moduleList(root)
	if err != nil {
		return nil, err
	}
	files, err := coveredFiles(root)
	if err != nil {
		return nil, err
	}
	var headers map[string]string
	if !skipHeaders {
		headers, err = fileHeaders(root, files)
		if err != nil {
			return nil, err
		}
	}
	findings := check(string(notices), modules, files, headers)

	zpgparseNotice, err := readOptional(filepath.Join(root, "internal", "zpgparse", "NOTICE"))
	if err != nil {
		return nil, err
	}
	revision, err := readOptional(filepath.Join(root, "testdata", "postgres184", "REVISION"))
	if err != nil {
		return nil, err
	}
	features, err := readOptional(filepath.Join(root, "testdata", "postgres", "embedded_features.json"))
	if err != nil {
		return nil, err
	}
	findings = append(findings, checkHashes(zpgparseNotice, revision, features)...)
	sort.Strings(findings)
	return findings, nil
}

// moduleList returns the module paths of `go list -m all` without the main
// module.
func moduleList(root string) ([]string, error) {
	mainModule, err := goList(root, "-m")
	if err != nil {
		return nil, err
	}
	all, err := goList(root, "-m", "all")
	if err != nil {
		return nil, err
	}
	main := strings.TrimSpace(mainModule)
	var modules []string
	for _, line := range strings.Split(all, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == main {
			continue
		}
		modules = append(modules, fields[0])
	}
	return modules, nil
}

func goList(root string, args ...string) (string, error) {
	command := exec.Command("go", append([]string{"list"}, args...)...)
	command.Dir = root
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("go list %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

// coveredFiles returns the slash-separated, root-relative paths of every file
// that rules (b) and (c) apply to. Roots that do not exist yet are skipped.
func coveredFiles(root string) ([]string, error) {
	seen := make(map[string]bool)
	var files []string
	add := func(path string) {
		relative := filepath.ToSlash(path)
		if !seen[relative] {
			seen[relative] = true
			files = append(files, relative)
		}
	}
	for _, dir := range coveredRoots {
		base := filepath.Join(root, filepath.FromSlash(dir))
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			add(relative)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	for _, pattern := range coveredGlobs {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			relative, err := filepath.Rel(root, match)
			if err != nil {
				return nil, err
			}
			add(relative)
		}
	}
	sort.Strings(files)
	return files, nil
}

// fileHeaders maps every covered file to its first headerLimit lines.
func fileHeaders(root string, files []string) (map[string]string, error) {
	headers := make(map[string]string, len(files))
	buffer := make([]byte, headerBytes)
	for _, file := range files {
		handle, err := os.Open(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			return nil, err
		}
		count, readErr := io.ReadFull(handle, buffer)
		closeErr := handle.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		lines := strings.Split(string(buffer[:count]), "\n")
		if len(lines) > headerLimit {
			lines = lines[:headerLimit]
		}
		headers[file] = strings.Join(lines, "\n")
	}
	return headers, nil
}

func readOptional(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}
