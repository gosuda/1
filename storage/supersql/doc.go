// Package supersql is an embedded, single-process database that speaks a
// conformant subset of PostgreSQL 18 SQL. A host opens a Catalog with Open and
// supplies a certified clock; there is no server, no network listener, and no
// DSN — connections come from OpenConnector (for database/sql) or
// SQLQueryEngine (for a single materializing session).
//
// OpenOptions.Clock is required and borrowed: the host owns the clock
// provider's lifecycle and must close the database before closing it. Storage
// layout, the SQL surface, and the conformance position are generated under
// docs/reference by internal/tools/docgen.
package supersql

//go:generate go run ./internal/tools/docgen
