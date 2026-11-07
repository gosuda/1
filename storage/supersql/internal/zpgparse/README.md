# zpgparse

Pure Go PostgreSQL 18.4 SQL and PL/pgSQL parser. No CGO or external parser runtime required. Go 1.27+.

Provides parsing, deparsing, normalization, fingerprints, statement splitting, and query summaries through a `pg_query_go`-style API.

## Internal package

This source is maintained within the SuperSQL module.

```go
import pg "github.com/gosuda/supersql-alpha/internal/zpgparse"

tree, err := pg.Parse("SELECT id FROM accounts WHERE balance > 100")
```

## Compatibility

Verified against 49,125 PostgreSQL corpus inputs, 854 PL/pgSQL cases, and upstream API tests. This does not guarantee identical behavior for every input or live-database validation.

Trigger output intentionally fixes malformed JSON in the upstream parser. Other catalog-free PL/pgSQL limitations remain.

## Development

```sh
CGO_ENABLED=0 go test ./...
go generate ./...                         # requires Ragel
```

Test fixtures under `testdata/` are copied verbatim from `gosuda/pg18_query_go@10d566809991b10ff1182369705c19743e16c155/testdata`.

Tests pass on Go 1.27, including race and pointer-safety checks. AST lint findings remain.

## License

PostgreSQL notices are retained in [LICENSE.POSTGRESQL](LICENSE.POSTGRESQL) and [NOTICE](NOTICE).
