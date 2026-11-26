// Package main implements licensecheck, which verifies that
// THIRD_PARTY_NOTICES.md stays in sync with the module graph, with the vendored
// file trees and with the PostgreSQL archive digest pinned in the corpus
// metadata.
//
// Rules:
//
//	(a) every module of `go list -m all` except the main module has a
//	    "### <module path>" entry carrying "License:" and "Source:" lines;
//	(b) every file under the covered roots matches a "Paths:" glob of some entry;
//	(c) every covered file whose first line is "// Code generated" carries
//	    "// Source:" and "// License:" lines within its first 8 lines
//	    (disabled by -skip-headers);
//	(d) the PostgreSQL archive digest in internal/zpgparse/NOTICE equals the
//	    digest pinned in testdata/postgres184/REVISION and in
//	    testdata/postgres/embedded_features.json.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// noticeBlock is one "### <heading>" section of the notices file together with
// the lines that follow it, excluding fenced code blocks.
type noticeBlock struct {
	heading string
	lines   []string
}

// parseBlocks splits the notices file into "### " sections. Lines inside fenced
// code blocks are dropped, because the embedded license texts contain headings
// and label-like lines of their own.
func parseBlocks(notices string) []noticeBlock {
	var blocks []noticeBlock
	fenced := false
	current := -1
	for _, line := range strings.Split(notices, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if strings.HasPrefix(line, "### ") {
			blocks = append(blocks, noticeBlock{heading: strings.TrimSpace(line[4:])})
			current = len(blocks) - 1
			continue
		}
		if current >= 0 {
			blocks[current].lines = append(blocks[current].lines, line)
		}
	}
	return blocks
}

// blockField returns the value of the first "label" line of the block.
func blockField(block noticeBlock, label string) string {
	for _, line := range block.lines {
		if strings.HasPrefix(line, label) {
			return strings.TrimSpace(line[len(label):])
		}
	}
	return ""
}

// blockPaths returns the globs listed after the "Paths:" line of the block.
func blockPaths(block noticeBlock) []string {
	var paths []string
	for index, line := range block.lines {
		if strings.TrimSpace(line) != "Paths:" {
			continue
		}
		for _, next := range block.lines[index+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" || isFieldLabel(trimmed) {
				break
			}
			paths = append(paths, trimmed)
		}
		break
	}
	return paths
}

// isFieldLabel reports whether the line looks like a "License:"/"Source:" style
// label, which terminates a Paths list.
func isFieldLabel(line string) bool {
	if line == "" || line[0] < 'A' || line[0] > 'Z' {
		return false
	}
	colon := strings.IndexByte(line, ':')
	if colon <= 0 {
		return false
	}
	for _, char := range line[:colon] {
		if char != ' ' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
}

// globRegexps caches the compiled form of the Paths globs. "**" matches any
// number of path segments, "*" and "?" match within one segment.
var globRegexps = map[string]*regexp.Regexp{}

func globRegexp(pattern string) *regexp.Regexp {
	if compiled, ok := globRegexps[pattern]; ok {
		return compiled
	}
	var builder strings.Builder
	builder.WriteString(`\A`)
	for index := 0; index < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[index:], "**/"):
			builder.WriteString(`(?:[^/]+/)*`)
			index += 3
		case strings.HasPrefix(pattern[index:], "**"):
			builder.WriteString(`.*`)
			index += 2
		case pattern[index] == '*':
			builder.WriteString(`[^/]*`)
			index++
		case pattern[index] == '?':
			builder.WriteString(`[^/]`)
			index++
		default:
			builder.WriteString(regexp.QuoteMeta(string(pattern[index])))
			index++
		}
	}
	builder.WriteString(`\z`)
	compiled, err := regexp.Compile(builder.String())
	if err != nil {
		// Unreachable for the quoted patterns this tool builds; an unparsable
		// pattern must not silently match everything.
		compiled = regexp.MustCompile(`\A\z`)
	}
	globRegexps[pattern] = compiled
	return compiled
}

// globMatch reports whether the slash-separated relative path matches the glob.
func globMatch(pattern, path string) bool {
	return globRegexp(pattern).MatchString(path)
}

// generatedLine matches the first line of a generated file.
var generatedLine = regexp.MustCompile(`^// Code generated`)

// headerLimit is how many leading lines rule (c) inspects.
const headerLimit = 8

// check reports every inconsistency between the notices file, the module list,
// the covered files and their generated-code headers. headers maps a covered
// file to its first headerLimit lines; passing nil disables rule (c).
func check(notices string, modules []string, files []string, headers map[string]string) []string {
	blocks := parseBlocks(notices)
	byHeading := make(map[string]noticeBlock, len(blocks))
	for _, block := range blocks {
		if _, ok := byHeading[block.heading]; !ok {
			byHeading[block.heading] = block
		}
	}

	var findings []string
	for _, module := range modules {
		block, ok := byHeading[module]
		if !ok {
			findings = append(findings, fmt.Sprintf("module %s has no '### %s' entry in the third-party notices", module, module))
			continue
		}
		if blockField(block, "License:") == "" {
			findings = append(findings, fmt.Sprintf("module %s entry lacks a 'License:' line", module))
		}
		if blockField(block, "Source:") == "" {
			findings = append(findings, fmt.Sprintf("module %s entry lacks a 'Source:' line", module))
		}
	}

	var globs []string
	for _, block := range blocks {
		globs = append(globs, blockPaths(block)...)
	}
	for _, file := range files {
		covered := false
		for _, glob := range globs {
			if globMatch(glob, file) {
				covered = true
				break
			}
		}
		if !covered {
			findings = append(findings, fmt.Sprintf("file %s is not covered by any 'Paths:' glob of the third-party notices", file))
		}
	}

	for _, file := range files {
		header, ok := headers[file]
		if !ok {
			continue
		}
		lines := strings.Split(header, "\n")
		if len(lines) > headerLimit {
			lines = lines[:headerLimit]
		}
		if !generatedLine.MatchString(lines[0]) {
			continue
		}
		if !hasHeaderLine(lines, "// Source:") {
			findings = append(findings, fmt.Sprintf("generated file %s lacks a '// Source:' line in its first %d lines", file, headerLimit))
		}
		if !hasHeaderLine(lines, "// License:") {
			findings = append(findings, fmt.Sprintf("generated file %s lacks a '// License:' line in its first %d lines", file, headerLimit))
		}
	}

	sort.Strings(findings)
	return findings
}

func hasHeaderLine(lines []string, label string) bool {
	for _, line := range lines {
		if strings.HasPrefix(line, label) {
			return true
		}
	}
	return false
}

var (
	noticeDigest   = regexp.MustCompile(`(?m)^Source archive sha256=([0-9a-f]{64})$`)
	revisionDigest = regexp.MustCompile(`(?m)^sha256=([0-9a-f]{64})$`)
)

// checkHashes reports rule (d): the PostgreSQL archive digest recorded in
// internal/zpgparse/NOTICE must equal the digest pinned in the corpus metadata
// files. An empty revision or embeddedFeatures string skips that comparison, so
// the rule activates as soon as the fixtures are present.
func checkHashes(zpgparseNotice, revision, embeddedFeatures string) []string {
	var findings []string
	want := ""
	if match := noticeDigest.FindStringSubmatch(zpgparseNotice); match != nil {
		want = match[1]
	} else {
		findings = append(findings, "internal/zpgparse/NOTICE has no 'Source archive sha256=<64 hex digits>' line")
	}
	if revision != "" {
		if match := revisionDigest.FindStringSubmatch(revision); match != nil {
			if want != "" && match[1] != want {
				findings = append(findings, fmt.Sprintf("PostgreSQL archive digest mismatch: internal/zpgparse/NOTICE %s, testdata/postgres184/REVISION %s", want, match[1]))
			}
		} else {
			findings = append(findings, "testdata/postgres184/REVISION has no 'sha256=<64 hex digits>' line")
		}
	}
	if embeddedFeatures != "" {
		var manifest struct {
			Source struct {
				SHA256 string `json:"sha256"`
			} `json:"source"`
		}
		if err := json.Unmarshal([]byte(embeddedFeatures), &manifest); err != nil {
			findings = append(findings, fmt.Sprintf("testdata/postgres/embedded_features.json cannot be decoded: %v", err))
		} else if manifest.Source.SHA256 != "" && want != "" && manifest.Source.SHA256 != want {
			findings = append(findings, fmt.Sprintf("PostgreSQL archive digest mismatch: internal/zpgparse/NOTICE %s, testdata/postgres/embedded_features.json %s", want, manifest.Source.SHA256))
		}
	}
	sort.Strings(findings)
	return findings
}
