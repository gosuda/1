# AGENTS.md

## Repository Guidelines

- Keep `CGO_ENABLED=0` compatibility across all packages (`go build ./...`).
- After changing SQL features, built-in catalogs, or public APIs/options, keep generated files in sync:
  - `go generate ./internal/query/bind ./internal/query/builtin`
  - `go run ./internal/tools/docgen` (verify with `go run ./internal/tools/docgen -check`)
- Preserve the `<!-- docgen:begin readme-example -->` ... `<!-- docgen:end readme-example -->` markers in `README.md`.
- Before finishing changes, verify locally:
  - `go build ./...`
  - `go mod download && go vet ./...` (ignoring only the generated `scan.rl` unreachable-code baseline)
  - `go run ./internal/tools/licensecheck`
  - `go test -short -count=1 ./...`
  - `go test -race -count=1 ./internal/dataplane/... ./internal/controlplane/... ./clock/... ./pgx/... .`
