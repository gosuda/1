package neoparse

import pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"

func ParseToPgQuery(sql string) (*pg_query.ParseResult, error) {
	return ParseMode(sql, 0)
}
