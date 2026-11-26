package bind

import (
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// PostgreSQL names physical index attributes uniquely before constructing the
// relation name, including repeated INCLUDE attributes. Only the prefix through
// NAMEDATALEN contributes; later attributes need no naming allocation here.
func indexNameAddition(keys, included []*zpgparse.Node) string {
	var names [64]string
	count := 0
	var addition strings.Builder
	addition.Grow(127)
	for _, attributes := range [...][]*zpgparse.Node{keys, included} {
		for _, attribute := range attributes {
			original := clipIndexIdentifier(indexAttributeName(attribute.GetIndexElem()), 63)
			name := original
			for suffix := 1; slices.Contains(names[:count], name); suffix++ {
				number := strconv.Itoa(suffix)
				name = clipIndexIdentifier(original, 63-len(number)) + number
			}
			names[count] = name
			count++
			if addition.Len() != 0 {
				addition.WriteByte('_')
			}
			addition.WriteString(name)
			if addition.Len() >= 64 {
				return addition.String()
			}
		}
	}
	return addition.String()
}

func indexAttributeName(element *zpgparse.IndexElem) string {
	if element.Name != "" {
		return element.Name
	}
	name := expressionName(element.Expr)
	if name == "?column?" || name == "" {
		return "expr"
	}
	return name
}

func clipIndexIdentifier(name string, maximum int) string {
	for len(name) > maximum {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}
