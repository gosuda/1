// Package supersql is an embedded, single-process database that speaks a
// conformant subset of PostgreSQL 18 SQL. A host opens a Catalog with Open (or
// a database/sql Connector with OpenConnector, or a pgx/v5-compatible pool via
// github.com/gosuda/supersql-alpha/pgx) and supplies a certified clock such as
// github.com/gosuda/supersql-alpha/clock/local; there is no server, no network
// listener, and no DSN.
//
// OpenOptions.Clock is required and borrowed: the host owns the clock
// provider's lifecycle and must close the database before closing it. Storage
// layout, the SQL surface, and the conformance position are generated under
// docs/reference by internal/tools/docgen.
package supersql

//go:generate go run ./internal/tools/docgen
