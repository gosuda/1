package builtin

import (
	"context"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// ParseQualifiedName owns up to three PostgreSQL identifier components. Catalog
// names are truncated at 63 UTF-8 bytes, unlike parse_ident's untruncated output.
func ParseQualifiedName(ctx context.Context, input string, maxBytes uint64) ([]string, error) {
	if uint64(len(input)) > maxBytes {
		return nil, sqlError("54000", "qualified name exceeds budget")
	}
	iterator := identifierParts{input: input, strict: true}
	var parts [3]identifierPart
	count := 0
	var bytes uint64
	for !iterator.done {
		if count == len(parts) {
			return nil, sqlError("42601", "too many qualified name components")
		}
		part, err := iterator.next(ctx)
		if err != nil {
			return nil, err
		}
		end, emitted := part.start, 0
		for end < part.end && emitted < 63 {
			if part.quoted && input[end] == '"' {
				end++
			}
			end++
			emitted++
		}
		for end < part.end && !utf8.RuneStart(input[end]) {
			end--
		}
		part.end = end
		parts[count] = part
		count++
		bytes += uint64(emitted) + uint64(unsafe.Sizeof(""))
	}
	if bytes > maxBytes {
		return nil, sqlError("54000", "qualified name exceeds budget")
	}
	names := make([]string, count)
	var arena strings.Builder
	arena.Grow(int(bytes) - count*int(unsafe.Sizeof("")))
	for i, part := range parts[:count] {
		start := arena.Len()
		if err := part.appendTo(ctx, input, &arena); err != nil {
			return nil, err
		}
		names[i] = arena.String()[start:]
	}
	return names, ctx.Err()
}
