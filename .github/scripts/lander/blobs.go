package lander

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Project metadata is read in the lander before sandbox resource limits apply. Bound both the
// number of responses and their total body bytes, including repeated requests for the same OID.
const (
	maxManifestBlobs      = 16384
	maxManifestBlobBytes  = 1 << 20
	maxManifestBatchBytes = 32 << 20
	maxBatchStderrBytes   = 64 << 10
)

// readBlobs reads full blob object IDs from ListTree in one Git process. Results preserve request
// order and exact bytes. Invalid, missing, non-blob, or over-budget objects fail the entire read.
func (s *Store) readBlobs(ctx context.Context, entries []TreeEntry) ([]string, error) {
	if len(entries) > maxManifestBlobs {
		return nil, fmt.Errorf("read blobs: manifest count exceeds limit of %d", maxManifestBlobs)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, e := range entries {
		if (len(e.OID) != 40 && len(e.OID) != 64) || strings.Trim(e.OID, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("read %s: invalid object ID %q", e.Path, e.OID)
		}
		input.WriteString(e.OID)
		input.WriteByte('\n')
	}
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"cat-file", "--batch", "--buffer"}
	cmd := s.gitCommand(batchCtx, nil, args...)
	cmd.Stdin = strings.NewReader(input.String())
	var stderr cappedGitStderr
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("read blobs: open Git stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read blobs: %w", &GitError{Args: args, Err: err})
	}
	bodies, parseErr := parseBlobBatch(out, entries)
	if parseErr != nil {
		cancel() // Stop Git instead of draining an oversized or invalid response.
	}
	waitErr := cmd.Wait() // Reap the child and finish stderr collection on every started path.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		gitErr := &GitError{Args: args, Err: waitErr, Stderr: stderr.String()}
		return nil, fmt.Errorf("read blobs: %w", errors.Join(parseErr, gitErr))
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return bodies, nil
}

// parseBlobBatch consumes one size-delimited response per entry and then requires EOF. Each
// iteration leaves the reader at the next header. Check the byte budget before allocating or
// reading a body; contents cannot become framing, and rejected responses are never drained.
func parseBlobBatch(out io.Reader, entries []TreeEntry) ([]string, error) {
	if len(entries) > maxManifestBlobs {
		return nil, fmt.Errorf("read blobs: manifest count exceeds limit of %d", maxManifestBlobs)
	}
	// A SHA-256 OID, type, and decimal size fit well within this fixed header buffer.
	r := bufio.NewReaderSize(out, 256)
	var bodies []string
	if len(entries) > 0 {
		bodies = make([]string, 0, len(entries))
	}
	total := 0
	for _, e := range entries {
		header, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("read %s: batch header exceeds limit", e.Path)
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: truncated batch header: %w", e.Path, err)
		}
		fields := strings.Split(string(header[:len(header)-1]), " ")
		if len(fields) == 2 && fields[0] == e.OID && fields[1] == "missing" {
			return nil, fmt.Errorf("read %s: object %s is missing", e.Path, e.OID)
		}
		if len(fields) != 3 || fields[0] != e.OID || fields[1] != "blob" {
			return nil, fmt.Errorf("read %s: unexpected batch header %q", e.Path, header)
		}
		declared, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("read %s: invalid blob size %q: %w", e.Path, fields[2], err)
		}
		if declared > maxManifestBlobBytes {
			return nil, fmt.Errorf("read %s: blob size %d exceeds limit of %d bytes", e.Path, declared, maxManifestBlobBytes)
		}
		size := int(declared) // Conversion is safe after the per-blob limit.
		if size > maxManifestBatchBytes-total {
			return nil, fmt.Errorf("read %s: blob batch exceeds limit of %d bytes", e.Path, maxManifestBatchBytes)
		}
		total += size
		body := make([]byte, size+1) // Git's newline separator is required even for an empty blob.
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("read %s: truncated blob of size %d: %w", e.Path, size, err)
		}
		if body[size] != '\n' {
			return nil, fmt.Errorf("read %s: missing blob terminator", e.Path)
		}
		bodies = append(bodies, string(body[:size]))
	}
	if _, err := r.ReadByte(); err == nil {
		return nil, errors.New("read blobs: unexpected trailing batch output")
	} else if err != io.EOF {
		return nil, fmt.Errorf("read blobs: read trailing batch output: %w", err)
	}
	return bodies, nil
}

// cappedGitStderr keeps bounded diagnostics while allowing the subprocess to finish writing.
type cappedGitStderr struct {
	data []byte
}

func (b *cappedGitStderr) Write(p []byte) (int, error) {
	n := min(len(p), maxBatchStderrBytes-len(b.data))
	b.data = append(b.data, p[:n]...)
	return len(p), nil
}

func (b *cappedGitStderr) String() string { return string(b.data) }
