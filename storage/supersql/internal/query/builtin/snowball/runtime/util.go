// Source: https://raw.githubusercontent.com/snowballstem/snowball/d19326ac6c1b9a417fc872f7c2f845265a5e9ece/go/util.go
// Snowball BSD-3-Clause license: ../LICENSE.SNOWBALL.
package snowball

import (
	"unicode/utf8"
)

func splitAt(str string, mid int) (string, string) {
	return str[:mid], str[mid:]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func onCharBoundary(s string, pos int) bool {
	if pos <= 0 || pos >= len(s) {
		return true
	}
	return utf8.RuneStart(s[pos])
}

// RuneCountInString is a wrapper around utf8.RuneCountInString
// this allows us to not have to conditionally include
// the utf8 package into some stemmers and not others
func RuneCountInString(str string) int {
	return utf8.RuneCountInString(str)
}
