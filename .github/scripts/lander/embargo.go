package lander

import "strings"

// Embargoed security work lives in the user repository under opaque names so that it can be kept
// internally, reviewed, and rebased for as long as needed without ever reaching a public mirror.
// docs/security-embargo.md specifies the naming scheme and lifecycle. The lander enforces three
// rules that depend only on names:
//
//   - a landing request that names an embargoed change may only be landed by the security team
//     (landing exports to the public mirrors immediately, so a stray landing would disclose it);
//   - embargoed references are exempt from the short workspace TTL;
//   - nothing in an embargoed name is ever required to be descriptive (see the naming scheme).
//
// isEmbargoName is deliberately broad: any path segment that is "sec" or starts with "sec-" marks
// the whole name, so a mistake in the exact id format fails closed.
func isEmbargoName(name string) bool {
	for _, seg := range strings.Split(strings.Trim(name, "/"), "/") {
		if seg == "sec" || strings.HasPrefix(seg, "sec-") {
			return true
		}
	}
	return false
}

// isEmbargoSubject reports whether a commit subject carries the embargo marker "sec-...".
func isEmbargoSubject(msg string) bool {
	subject, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	return strings.HasPrefix(subject, "sec-")
}
