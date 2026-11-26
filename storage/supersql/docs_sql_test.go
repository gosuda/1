package supersql_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	supersql "github.com/gosuda/supersql-alpha"
	clocklocal "github.com/gosuda/supersql-alpha/clock/local"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type sqlDocBlock struct {
	file       string
	line       int
	expectCode string
	sql        string
}

// TestDocsSQLExamples parses every `<!-- doctest -->` and
// `<!-- doctest:expect-error SQLSTATE -->` fenced SQL code block in
// docs/sql/*.md and executes it against a fresh SuperSQL database so the SQL
// documentation stays verified against the real engine.
func TestDocsSQLExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("docs", "sql", "*.md"))
	if err != nil {
		t.Fatalf("glob docs/sql/*.md: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no markdown files found under docs/sql/")
	}

	var totalBlocks int
	for _, file := range files {
		blocks, err := extractSQLDocBlocks(file)
		if err != nil {
			t.Fatalf("extract blocks from %s: %v", file, err)
		}
		if len(blocks) == 0 {
			continue
		}
		totalBlocks += len(blocks)

		t.Run(filepath.Base(file), func(t *testing.T) {
			for i, blk := range blocks {
				t.Run(strings.TrimSuffix(filepath.Base(file), ".md")+"#"+itoa(i+1), func(t *testing.T) {
					runSQLDocBlock(t, blk)
				})
			}
		})
	}
	if totalBlocks < 10 {
		t.Fatalf("expected at least 10 doctest SQL blocks across docs/sql/*.md, found %d", totalBlocks)
	}
}

func runSQLDocBlock(t *testing.T, blk sqlDocBlock) {
	t.Helper()
	ctx := t.Context()
	db, err := supersql.Open(ctx, supersql.OpenOptions{
		Directory: t.TempDir(),
		Clock:     clocklocal.New(),
	})
	if err != nil {
		t.Fatalf("%s:%d: Open: %v", blk.file, blk.line, err)
	}
	defer func() { _ = db.Close() }()

	sqlDB := sql.OpenDB(supersql.NewConnectorWithCatalog(db))
	defer func() { _ = sqlDB.Close() }()

	// Use a single dedicated connection so session-scoped statements (BEGIN,
	// SET LOCAL ROLE, LISTEN, UNLISTEN) share one session.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("%s:%d: Conn: %v", blk.file, blk.line, err)
	}
	defer func() { _ = conn.Close() }()

	stmts, err := zpgparse.SplitWithParser(blk.sql, true)
	if err != nil {
		t.Fatalf("%s:%d: Split SQL: %v\nSQL:\n%s", blk.file, blk.line, err, blk.sql)
	}
	if len(stmts) == 0 {
		t.Fatalf("%s:%d: empty doctest SQL block", blk.file, blk.line)
	}

	for idx, raw := range stmts {
		stmt := strings.TrimSpace(raw)
		if stmt == "" {
			continue
		}
		isLast := idx == len(stmts)-1
		rows, err := conn.QueryContext(ctx, stmt)
		if err == nil {
			// Drain all rows to surface execution-time errors.
			cols, cerr := rows.Columns()
			if cerr != nil {
				err = cerr
			} else if len(cols) > 0 {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for j := range vals {
					ptrs[j] = &vals[j]
				}
				for rows.Next() {
					if serr := rows.Scan(ptrs...); serr != nil {
						err = serr
						break
					}
				}
				if err == nil {
					err = rows.Err()
				}
			}
			if closeErr := rows.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}

		if blk.expectCode != "" && isLast {
			if err == nil {
				t.Fatalf("%s:%d: expected SQLSTATE %s for %q, got nil error", blk.file, blk.line, blk.expectCode, stmt)
			}
			var sqlErr *supersql.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != blk.expectCode {
				t.Fatalf("%s:%d: statement %q got error %v, want SQLSTATE %s", blk.file, blk.line, stmt, err, blk.expectCode)
			}
			return
		}
		if err != nil {
			t.Fatalf("%s:%d: statement %q failed: %v", blk.file, blk.line, stmt, err)
		}
	}
}

func extractSQLDocBlocks(path string) ([]sqlDocBlock, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	var blocks []sqlDocBlock
	pending := false
	expectCode := ""
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "<!-- doctest -->" {
			pending = true
			expectCode = ""
			continue
		}
		if strings.HasPrefix(trimmed, "<!-- doctest:expect-error ") && strings.HasSuffix(trimmed, "-->") {
			code := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, "<!-- doctest:expect-error "), "-->"))
			pending = true
			expectCode = code
			continue
		}
		if pending && strings.HasPrefix(trimmed, "```sql") {
			startLine := i + 2
			var body []string
			for i++; i < len(lines); i++ {
				if strings.TrimSpace(lines[i]) == "```" {
					break
				}
				body = append(body, lines[i])
			}
			blocks = append(blocks, sqlDocBlock{
				file:       path,
				line:       startLine,
				expectCode: expectCode,
				sql:        strings.Join(body, "\n"),
			})
			pending = false
			expectCode = ""
			continue
		}
		if trimmed != "" {
			pending = false
			expectCode = ""
		}
	}
	return blocks, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	pos := len(digits)
	for n > 0 {
		pos--
		digits[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[pos:])
}
