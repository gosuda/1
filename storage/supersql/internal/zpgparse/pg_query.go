// Package zpgparse is a CGO-free, pure Go PostgreSQL 18.4 parser.
//
// It mirrors the public surface of pg_query_go v6 (Parse, Scan, ParseToJSON,
// Deparse, Normalize, Fingerprint, Split, Summary, and related helpers)
// on top of the native Ragel/goyacc parser in internal/neoparse. Message
// and enum types are protobuf-compatible aliases defined in types.go and
// implemented in internal/pgproto; see NOTICE for provenance.
package zpgparse

//go:generate go run ./internal/cmd/genapi
//go:generate go run ./internal/cmd/genast

import (
	"errors"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/zpgparse/internal/neoparse"
)

// Error describes a PostgreSQL parser diagnostic. Location is a zero-based byte
// offset and Cursorpos is one-based.
type Error struct {
	Code      string
	Message   string
	Detail    string
	Hint      string
	Funcname  string
	Filename  string
	Lineno    int
	Cursorpos int
	Context   string
	Location  int
}

func (e *Error) Error() string { return e.Message }
func (e *Error) SQLState() string { return e.Code }

func Parse(input string) (*ParseResult, error) {
	result, err := neoparse.ParseToPgQuery(input)
	return result, publicParseError(err)
}

func Scan(input string) (*ScanResult, error) {
	result, err := neoparse.Scan(input)
	return result, publicParseError(err)
}

func publicParseError(err error) error {
	if err == nil {
		return nil
	}
	var native *neoparse.Error
	if errors.As(err, &native) {
		converted := &Error{
			Code: native.Code, Message: native.Message, Detail: native.Detail, Hint: native.Hint,
			Funcname: native.Funcname, Filename: native.Filename, Lineno: native.Lineno,
			Cursorpos: native.Cursorpos, Context: native.Context, Location: native.Location,
		}
		// The native scanner reports syntax errors without C-style provenance.
		// Upstream pg_query_go reports them as scan.l/scanner_yyerror; preserve
		// that contract for syntax errors only, leaving internal errors unlabeled.
		if strings.HasPrefix(converted.Message, "syntax error") {
			if converted.Funcname == "" {
				converted.Funcname = "scanner_yyerror"
			}
			if converted.Filename == "" {
				converted.Filename = "scan.l"
			}
		}
		return converted
	}
	return err
}
