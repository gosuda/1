package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// Parse captures lexical settings together with the owned AST and extension
// sidecars. Callers must retain the whole result, not discard its lexical policy.
func (transactions *Transactions) Parse(ctx context.Context, sql string, options zpgparse.ParseOptions) (result *zpgparse.ExtendedParseResult, err error) {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	defer func() {
		if err != nil {
			transactions.markFailed(err)
		}
	}()
	semantics := transactions.settings.Semantics()
	options.StandardConformingStrings = zpgparse.BooleanOptionFor(semantics.StandardConformingStrings)
	options.EscapeStringWarning = zpgparse.BooleanOptionFor(semantics.EscapeStringWarning)
	options.BackslashQuote, err = zpgparse.ParseBackslashQuote(semantics.BackslashQuote)
	if err != nil {
		return nil, err
	}
	result, err = zpgparse.ParseExtended(sql, options)
	if err != nil {
		var diagnostic *zpgparse.Error
		if errors.As(err, &diagnostic) {
			code := diagnostic.Code
			if code == "" {
				code = "42601"
			}
			return nil, &model.Error{Code: code, Message: diagnostic.Message, Detail: diagnostic.Detail, Position: diagnostic.Cursorpos, Cause: err}
		}
		return nil, err
	}
	if err := operation.Err(); err != nil {
		return nil, sessionTransactionError(err)
	}
	if transactions.settings.current[clientMinMessages].text == "error" {
		result.Diagnostics = nil
	}
	return result, nil
}
