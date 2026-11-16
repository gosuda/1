package lander

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// readBlobs reads ListTree entries in one Git process. Results preserve request order and all blob
// bytes, including trailing newlines. Missing or non-blob objects fail the entire read.
func (s *Store) readBlobs(ctx context.Context, entries []TreeEntry) ([]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, e := range entries {
		input.WriteString(e.OID)
		input.WriteByte('\n')
	}
	out, err := s.runRaw(ctx, nil, strings.NewReader(input.String()), "cat-file", "--batch", "--buffer")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read blobs: %w", err)
	}
	return parseBlobBatch(entries, out)
}

// parseBlobBatch consumes exactly one size-delimited blob response per entry. Each iteration leaves
// out at the next header; neither a blob's newlines nor its contents can become framing.
func parseBlobBatch(entries []TreeEntry, out string) ([]string, error) {
	bodies := make([]string, 0, len(entries))
	for _, e := range entries {
		header, rest, ok := strings.Cut(out, "\n")
		if !ok {
			return nil, fmt.Errorf("read %s: truncated Git batch header", e.Path)
		}
		fields := strings.Split(header, " ")
		if len(fields) != 3 || fields[0] != e.OID || fields[1] != "blob" {
			return nil, fmt.Errorf("read %s: unexpected Git batch header %q", e.Path, header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 {
			return nil, fmt.Errorf("read %s: invalid Git blob size %q", e.Path, fields[2])
		}
		if size >= len(rest) || rest[size] != '\n' {
			return nil, fmt.Errorf("read %s: truncated or malformed Git blob body", e.Path)
		}
		bodies = append(bodies, rest[:size])
		out = rest[size+1:]
	}
	if out != "" {
		return nil, fmt.Errorf("read blobs: unexpected trailing Git batch data")
	}
	return bodies, nil
}
