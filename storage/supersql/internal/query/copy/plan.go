// Package copy implements caller-stream PostgreSQL COPY framing and value codecs.
package copy

import (
	"context"
	"io"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// RowSourceFunction is an intrinsic IR identity. It is never resolved through
// the SQL function catalog.
const RowSourceFunction model.FunctionID = ^model.FunctionID(0)

type Direction uint8

const (
	From Direction = iota + 1
	To
)

type Format uint8

const (
	Text Format = iota + 1
	CSV
	Binary
)

type HeaderMode uint8

const (
	NoHeader HeaderMode = iota
	Header
	HeaderMatch
)

type ErrorMode uint8

const (
	StopOnError ErrorMode = iota
	IgnoreConversionErrors
)

type LogVerbosity uint8

const (
	LogDefault LogVerbosity = iota
	LogVerbose
	LogSilent
)

type Column struct {
	Name         string
	Layout       model.TypeLayout
	Nullable     bool
	ForceQuote   bool
	ForceNotNull bool
	ForceNull    bool
}

type Options struct {
	Format       Format
	Delimiter    byte
	Null         string
	Default      string
	HasDefault   bool
	Header       HeaderMode
	Quote        byte
	Escape       byte
	ErrorMode    ErrorMode
	RejectLimit  uint64
	LogVerbosity LogVerbosity
}

type Plan struct {
	direction Direction
	columns   []Column
	options   Options
	owned     uint64
}

func NewPlan(direction Direction, columns []Column, options Options) (*Plan, error) {
	if direction != From && direction != To {
		return nil, sqlError("42601", "COPY requires a direction")
	}
	if options.Format == 0 {
		options.Format = Text
	}
	if options.Format < Text || options.Format > Binary {
		return nil, sqlError("22023", "invalid COPY format")
	}
	if options.Delimiter == 0 {
		if options.Format == CSV {
			options.Delimiter = ','
		} else {
			options.Delimiter = '\t'
		}
	}
	if options.Quote == 0 {
		options.Quote = '"'
	}
	if options.Escape == 0 {
		options.Escape = options.Quote
	}
	if options.Delimiter == 0 || options.Delimiter == '\r' || options.Delimiter == '\n' || options.Delimiter >= 0x80 {
		return nil, sqlError("22023", "COPY delimiter must be one single-byte character")
	}
	if options.Format == Text && options.Delimiter == '\\' {
		return nil, sqlError("22023", "COPY text delimiter cannot be a backslash")
	}
	if options.Format == CSV {
		if options.Quote == 0 || options.Quote == '\r' || options.Quote == '\n' || options.Quote >= 0x80 || options.Escape == 0 || options.Escape == '\r' || options.Escape == '\n' || options.Escape >= 0x80 {
			return nil, sqlError("22023", "COPY quote and escape must be one single-byte character")
		}
		if options.Delimiter == options.Quote {
			return nil, sqlError("22023", "COPY delimiter and quote must be different")
		}
	}
	if options.Header > HeaderMatch {
		return nil, sqlError("22023", "invalid COPY header option")
	}
	if options.Format == Binary && (options.Header != NoHeader || options.HasDefault || options.ErrorMode != StopOnError || options.RejectLimit != 0 || options.LogVerbosity != LogDefault) {
		return nil, sqlError("22023", "COPY binary format does not allow text error-handling options")
	}
	if direction == To && (options.HasDefault || options.Header == HeaderMatch || options.ErrorMode != StopOnError || options.RejectLimit != 0 || options.LogVerbosity != LogDefault) {
		return nil, sqlError("22023", "COPY option is not valid for COPY TO")
	}
	if strings.IndexByte(options.Null, '\r') >= 0 || strings.IndexByte(options.Null, '\n') >= 0 {
		return nil, sqlError("22023", "COPY null representation must not contain a newline")
	}
	if options.HasDefault && (strings.IndexByte(options.Default, '\r') >= 0 || strings.IndexByte(options.Default, '\n') >= 0) {
		return nil, sqlError("22023", "COPY default representation must not contain a newline")
	}
	if options.HasDefault && options.Default == options.Null {
		return nil, sqlError("22023", "COPY default and null representations must be different")
	}
	if options.ErrorMode != StopOnError && options.ErrorMode != IgnoreConversionErrors || options.LogVerbosity > LogSilent {
		return nil, sqlError("22023", "invalid COPY error handling option")
	}
	if options.RejectLimit != 0 && options.ErrorMode != IgnoreConversionErrors {
		return nil, sqlError("22023", "COPY REJECT_LIMIT requires ON_ERROR ignore")
	}
	if options.Format != Binary && (strings.IndexByte(options.Null, options.Delimiter) >= 0 || options.HasDefault && strings.IndexByte(options.Default, options.Delimiter) >= 0) {
		return nil, sqlError("22023", "COPY null and default representations must not contain the delimiter")
	}
	owned := make([]Column, len(columns))
	var bytes uint64
	for i, column := range columns {
		if column.Name == "" || strings.IndexByte(column.Name, 0) >= 0 || column.Layout.Validate() != nil {
			return nil, sqlError("42804", "invalid COPY column metadata")
		}
		if options.Format != CSV && (column.ForceQuote || column.ForceNotNull || column.ForceNull) {
			return nil, sqlError("22023", "COPY FORCE options require CSV format")
		}
		if (direction == From && column.ForceQuote) || (direction == To && (column.ForceNotNull || column.ForceNull)) {
			return nil, sqlError("22023", "COPY FORCE option is not valid for this direction")
		}
		layout, size, err := model.CopyTypeLayout(context.Background(), column.Layout, ^uint64(0))
		if err != nil {
			return nil, err
		}
		column.Name, column.Layout = strings.Clone(column.Name), layout
		owned[i] = column
		bytes += uint64(len(column.Name)) + size
	}
	options.Null = strings.Clone(options.Null)
	options.Default = strings.Clone(options.Default)
	bytes += uint64(len(options.Null) + len(options.Default))
	return &Plan{direction: direction, columns: owned, options: options, owned: bytes}, nil
}

func (plan *Plan) Direction() Direction {
	if plan == nil {
		return 0
	}
	return plan.direction
}

func (plan *Plan) Options() Options {
	if plan == nil {
		return Options{}
	}
	options := plan.options
	options.Null = strings.Clone(options.Null)
	options.Default = strings.Clone(options.Default)
	return options
}

func (plan *Plan) Columns() []Column {
	if plan == nil {
		return nil
	}
	columns := slices.Clone(plan.columns)
	for i := range columns {
		columns[i].Name = strings.Clone(columns[i].Name)
		columns[i].Layout, _, _ = model.CopyTypeLayout(context.Background(), columns[i].Layout, ^uint64(0))
	}
	return columns
}

func (plan *Plan) Clone() *Plan {
	if plan == nil {
		return nil
	}
	return &Plan{direction: plan.direction, columns: plan.Columns(), options: plan.Options(), owned: plan.owned}
}

func (plan *Plan) OwnedBytes() uint64 {
	if plan == nil {
		return 0
	}
	return plan.owned
}

// Streams borrows Reader or Writer for one execution. COPY never closes caller
// resources. Exactly one direction-appropriate stream must be non-nil.
type Streams struct {
	Reader         io.Reader
	Writer         io.Writer
	MaxRecordBytes uint64
	Report         func(context.Context, Diagnostic)
}

type Diagnostic struct {
	Row    uint64
	Column string
	Err    error
}

func ValidateStreams(plan *Plan, streams *Streams) error {
	if plan == nil {
		return sqlError("22023", "COPY plan is missing")
	}
	return ValidateDirection(plan.direction, streams)
}

func ValidateDirection(direction Direction, streams *Streams) error {
	if streams == nil || streams.MaxRecordBytes == 0 {
		return sqlError("22023", "COPY requires bounded caller-supplied streams")
	}
	switch direction {
	case From:
		if streams.Reader == nil || streams.Writer != nil {
			return sqlError("22023", "COPY FROM requires only a caller-supplied reader")
		}
	case To:
		if streams.Writer == nil || streams.Reader != nil {
			return sqlError("22023", "COPY TO requires only a caller-supplied writer")
		}
	default:
		return sqlError("22023", "invalid COPY direction")
	}
	return nil
}

func sqlError(code, message string) error {
	return &model.Error{Code: code, Message: message}
}
