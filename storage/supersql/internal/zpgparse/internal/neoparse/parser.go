package neoparse

import (
	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
)

// ParseMode uses PostgreSQL RawParseMode values without modifying the input.
func ParseMode(sql string, mode int) (*pg_query.ParseResult, error) {
	lex := newLexer(sql)
	switch mode {
	case 0:
	case 1:
		lex.initialToken = MODE_TYPE_NAME
	case 2:
		lex.initialToken = MODE_PLPGSQL_EXPR
	case 3:
		lex.initialToken = MODE_PLPGSQL_ASSIGN1
	case 4:
		lex.initialToken = MODE_PLPGSQL_ASSIGN2
	case 5:
		lex.initialToken = MODE_PLPGSQL_ASSIGN3
	default:
		return nil, &Error{Message: "invalid raw parse mode", Location: -1}
	}
	yyParse(lex)
	if lex.err != nil {
		return nil, lex.err
	}
	return lex.parseResult(), nil
}

func (lex *Lexer) parseResult() *pg_query.ParseResult {
	result := &pg_query.ParseResult{Version: 180004}
	for i, stmt := range lex.statements {
		location := lex.statementLocations[i]
		result.Stmts = append(result.Stmts, &pg_query.RawStmt{
			Stmt:         stmt,
			StmtLocation: location.start,
			StmtLen:      location.length,
		})
	}
	return result
}
