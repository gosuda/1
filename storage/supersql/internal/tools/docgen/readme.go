package main

import (
	"os"
	"strings"
)

// The README quick start is a tested example: the region between these markers
// is replaced by the body of the example file, so the prose can never drift from
// the code that the test suite runs.
const (
	readmeBegin       = "<!-- docgen:begin readme-example -->"
	readmeEnd         = "<!-- docgen:end readme-example -->"
	readmeExampleFile = "example_readme_test.go"
	readmeName        = "README.md"
)

// renderReadme returns the README with its example region synchronized to
// example_readme_test.go. The second result reports whether the region exists:
// the markers and the example file are added by a separate change, and until
// then the synchronization is skipped rather than failed, so this renderer is
// safe to ship before the example exists.
func renderReadme(g *generator) ([]byte, bool, error) {
	readme, err := os.ReadFile(g.path(readmeName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	text := string(readme)
	begin := strings.Index(text, readmeBegin)
	end := strings.Index(text, readmeEnd)
	if begin < 0 || end < 0 || end < begin {
		return nil, false, nil
	}
	example, err := os.ReadFile(g.path(readmeExampleFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	body, ok := exampleBody(string(example))
	if !ok {
		return nil, false, nil
	}
	replacement := readmeBegin + "\n\n```go\n" + strings.TrimRight(body, "\n") + "\n```\n\n" + readmeEnd
	updated := text[:begin] + replacement + text[end+len(readmeEnd):]
	return []byte(updated), true, nil
}

// exampleBody returns the example file from the line after its package clause to
// the end of the file, verbatim.
func exampleBody(source string) (string, bool) {
	lines := strings.Split(source, "\n")
	for index, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "package ") {
			continue
		}
		if index+1 >= len(lines) {
			return "", false
		}
		return strings.Join(lines[index+1:], "\n"), true
	}
	return "", false
}
