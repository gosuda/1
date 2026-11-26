package supersql_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	supersql "github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/clock"
)

// runReadme is the README quick start: open a connector, wrap it in database/sql, run DDL, a transaction and a query.
func runReadme(ctx context.Context, clk clock.Clock, dir string, out io.Writer) error {
	connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{Directory: dir, Clock: clk})
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector) // db.Close also closes the connector
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE accounts (id INT PRIMARY KEY, name VARCHAR, balance INT)"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "CREATE INDEX idx_acc_name ON accounts(name)"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, row := range []string{"(1, 'Alice', 50000)", "(2, 'Bob', 85000)"} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO accounts (id, name, balance) VALUES "+row); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT id, name, balance FROM accounts WHERE name = 'Alice'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, balance int64
		var name string
		if err := rows.Scan(&id, &name, &balance); err != nil {
			return err
		}
		fmt.Fprintf(out, "Account: %d | %s | %d\n", id, name, balance)
	}
	return rows.Err()
}
