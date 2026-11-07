package procedural

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const SemanticVersion uint32 = 1

const programFormat = "supersql.procedural/1"

type datumKind uint8

const (
	datumScalar datumKind = iota + 1
	datumRow
	datumRecord
	datumField
	datumPromise
)

type datum struct {
	Kind       datumKind       `json:"kind"`
	Name       string          `json:"name,omitempty"`
	Layout     model.TypeLayout `json:"layout"`
	Constant   bool            `json:"constant,omitempty"`
	NotNull    bool            `json:"not_null,omitempty"`
	Default    int             `json:"default,omitempty"`
	Parent     int             `json:"parent,omitempty"`
	Field      string          `json:"field,omitempty"`
	FieldNames []string        `json:"field_names,omitempty"`
	CursorQuery  int             `json:"cursor_query,omitempty"`
	CursorArgRow int             `json:"cursor_arg_row,omitempty"`
	CursorOptions int             `json:"cursor_options,omitempty"`
	Fields     []int           `json:"fields,omitempty"`
}

type queryEncoding struct {
	Node []byte `json:"node"`
}

type stmtKind uint8

const (
	stmtBlock stmtKind = iota + 1
	stmtAssign
	stmtIf
	stmtCase
	stmtLoop
	stmtExit
	stmtContinue
	stmtReturn
	stmtReturnNext
	stmtReturnQuery
	stmtExecute
	stmtPerform
	stmtCall
	stmtRaise
	stmtAssert
	stmtOpen
	stmtFetch
	stmtClose
	stmtGetDiagnostics
	stmtTransaction
)

type branch struct {
	Query int         `json:"query"`
	Body  []statement `json:"body,omitempty"`
}

type exceptionHandler struct {
	Conditions []string    `json:"conditions"`
	Body       []statement `json:"body"`
}

type diagnostic struct {
	Kind   string `json:"kind"`
	Target int    `json:"target"`
}

type statement struct {
	Kind       stmtKind           `json:"kind"`
	Line       int                `json:"line,omitempty"`
	Label      string             `json:"label,omitempty"`
	Target     int                `json:"target,omitempty"`
	Query      int                `json:"query,omitempty"`
	Auxiliary  int                `json:"auxiliary,omitempty"`
	Parameters []int              `json:"parameters,omitempty"`
	Using      []int              `json:"using,omitempty"`
	Argument    int                `json:"argument,omitempty"`
	Cursor      int                `json:"cursor,omitempty"`
	Direction   int                `json:"direction,omitempty"`
	Count       int64              `json:"count,omitempty"`
	CursorOptions int             `json:"cursor_options,omitempty"`
	Move        bool               `json:"move,omitempty"`
	Stacked     bool               `json:"stacked,omitempty"`
	SetsFound   bool               `json:"sets_found,omitempty"`
	Diagnostics []diagnostic       `json:"diagnostics,omitempty"`
	OptionKinds []int              `json:"option_kinds,omitempty"`
	Body       []statement        `json:"body,omitempty"`
	Else       []statement        `json:"else,omitempty"`
	Branches   []branch           `json:"branches,omitempty"`
	Exceptions []exceptionHandler `json:"exceptions,omitempty"`
	LoopKind   string             `json:"loop_kind,omitempty"`
	Reverse    bool               `json:"reverse,omitempty"`
	Slice      int                `json:"slice,omitempty"`
	Strict     bool               `json:"strict,omitempty"`
	Rollback   bool               `json:"rollback,omitempty"`
	Chain      bool               `json:"chain,omitempty"`
	Message    string             `json:"message,omitempty"`
	Condition  string             `json:"condition,omitempty"`
	Level      int                `json:"level,omitempty"`
}

type programEncoding struct {
	Format       string          `json:"format"`
	Language     string          `json:"language"`
	Procedure    bool            `json:"procedure,omitempty"`
	Atomic       bool            `json:"atomic,omitempty"`
	ReturnsSet   bool            `json:"returns_set,omitempty"`
	Result       model.TypeLayout `json:"result"`
	Settings     []model.RoutineSetting `json:"settings,omitempty"`
	Datums       []datum         `json:"datums,omitempty"`
	Inputs       []int           `json:"inputs,omitempty"`
	Outputs      []int           `json:"outputs,omitempty"`
	NewDatum     int             `json:"new_datum,omitempty"`
	OldDatum     int             `json:"old_datum,omitempty"`
	Queries      []queryEncoding `json:"queries,omitempty"`
	Statements   []statement     `json:"statements"`
}

type queryPlan struct {
	node *zpgparse.Node
}

// Program owns decoded immutable routine semantics. Execute creates all mutable
// invocation state and never changes the program or its query trees.
type Program struct {
	language        string
	procedure       bool
	atomic          bool
	securityDefiner bool
	returnsSet      bool
	result          model.TypeLayout
	settings        []model.RoutineSetting
	datums          []datum
	inputs          []int
	outputs         []int
	newDatum        int
	oldDatum        int
	queries         []queryPlan
	statements      []statement
	limits          Limits
	routineOID      model.OID
	routineName     string
}

func encodeProgram(ctx context.Context, encoding programEncoding, limits Limits) (model.ExpressionDefinition, error) {
	if err := ctx.Err(); err != nil {
		return model.ExpressionDefinition{}, err
	}
	data, err := json.Marshal(encoding)
	if err != nil {
		return model.ExpressionDefinition{}, err
	}
	if uint64(len(data)) > limits.MaxProgramBytes {
		return model.ExpressionDefinition{}, ErrLimit
	}
	return model.ExpressionDefinition{SemanticVersion: SemanticVersion, Encoding: data}, nil
}

func Decode(ctx context.Context, definition model.ExpressionDefinition, limits Limits) (*Program, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if definition.SemanticVersion != SemanticVersion || len(definition.Encoding) == 0 || uint64(len(definition.Encoding)) > limits.MaxProgramBytes {
		return nil, ErrInvalidProgram
	}
	decoder := json.NewDecoder(bytes.NewReader(definition.Encoding))
	decoder.DisallowUnknownFields()
	var encoded programEncoding
	if err := decoder.Decode(&encoded); err != nil {
		return nil, fmt.Errorf("decode procedural program: %w", ErrInvalidProgram)
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || encoded.Format != programFormat || (encoded.Language != "sql" && encoded.Language != "plpgsql") || encoded.Atomic && encoded.Language != "sql" {
		return nil, ErrInvalidProgram
	}
	program := &Program{
		language: encoded.Language, procedure: encoded.Procedure, atomic: encoded.Atomic, returnsSet: encoded.ReturnsSet,
		result: encoded.Result, settings: encoded.Settings, datums: encoded.Datums, inputs: encoded.Inputs,
		outputs: encoded.Outputs, newDatum: encoded.NewDatum, oldDatum: encoded.OldDatum,
		statements: encoded.Statements, limits: limits,
	}
	if err := program.validate(ctx, encoded.Queries); err != nil {
		return nil, err
	}
	return program, nil
}

func (p *Program) validate(ctx context.Context, encoded []queryEncoding) error {
	// The result is a declared signature type, so routine-result pseudo-type
	// identities (trigger, void, ...) validate as declaration markers.
	if err := p.result.ValidateDeclaredLayout(); err != nil {
		return ErrInvalidProgram
	}
	settingNames := make(map[string]struct{}, len(p.settings))
	for _, setting := range p.settings {
		name := strings.ToLower(setting.Name)
		if name == "" || strings.IndexByte(setting.Name, 0) >= 0 || strings.IndexByte(setting.Value, 0) >= 0 || setting.Reset && setting.Value != "" {
			return ErrInvalidProgram
		}
		if _, duplicate := settingNames[name]; duplicate {
			return ErrInvalidProgram
		}
		settingNames[name] = struct{}{}
	}
	for i := range p.datums {
		d := &p.datums[i]
		validCursorOptions := d.CursorOptions == 0 || d.CursorOptions == 256 || d.CursorOptions == 258 || d.CursorOptions == 260
		if d.Kind < datumScalar || d.Kind > datumPromise || d.Default < 0 || d.Default > len(encoded) || d.CursorQuery < 0 || d.CursorQuery > len(encoded) || d.CursorArgRow < 0 || d.CursorArgRow > len(p.datums) || !validCursorOptions {
			return ErrInvalidProgram
		}
		if d.Kind != datumField && d.Layout.ValidateDeclaredLayout() != nil {
			return ErrInvalidProgram
		}
		if d.Kind == datumField && d.Parent == 0 {
			return ErrInvalidProgram
		}
		if d.Parent < 0 || d.Parent > len(p.datums) {
			return ErrInvalidProgram
		}
		for _, field := range d.Fields {
			if field < 0 || field >= len(p.datums) {
				return ErrInvalidProgram
			}
		}
	}
	if p.newDatum < 0 || p.newDatum > len(p.datums) || p.oldDatum < 0 || p.oldDatum > len(p.datums) {
		return ErrInvalidProgram
	}
	for _, index := range append(append([]int(nil), p.inputs...), p.outputs...) {
		if index < 0 || index >= len(p.datums) {
			return ErrInvalidProgram
		}
	}
	p.queries = make([]queryPlan, len(encoded))
	for i, encodedQuery := range encoded {
		if err := ctx.Err(); err != nil {
			return err
		}
		node := new(zpgparse.Node)
		if len(encodedQuery.Node) == 0 || uint64(len(encodedQuery.Node)) > p.limits.MaxProgramBytes || proto.Unmarshal(encodedQuery.Node, node) != nil || node.Node == nil {
			return ErrInvalidProgram
		}
		p.queries[i].node = node
	}
	count := 0
	if err := validateStatements(ctx, p.statements, p.limits, 0, &count, len(p.queries), len(p.datums)); err != nil {
		return err
	}
	return nil
}

func validateStatements(ctx context.Context, statements []statement, limits Limits, depth int, count *int, queries, datums int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	*count += len(statements)
	for _, stmt := range statements {
		validCursorOptions := stmt.CursorOptions == 0 || stmt.CursorOptions == 256 || stmt.CursorOptions == 258 || stmt.CursorOptions == 260
		if stmt.Kind < stmtBlock || stmt.Kind > stmtTransaction || stmt.Target < 0 || stmt.Target > datums || stmt.Cursor < 0 || stmt.Cursor > datums || stmt.Query < 0 || stmt.Query > queries || stmt.Auxiliary < 0 || stmt.Auxiliary > queries || stmt.Argument < 0 || stmt.Argument > queries || !validCursorOptions || stmt.Kind != stmtTransaction && (stmt.Rollback || stmt.Chain) {
			return ErrInvalidProgram
		}
		for _, query := range stmt.Parameters {
			if query < 0 || query > queries {
				return ErrInvalidProgram
			}
		}
		for _, query := range stmt.Using {
			if query <= 0 || query > queries {
				return ErrInvalidProgram
			}
		}
		if stmt.Slice < 0 || stmt.Slice > 6 {
			return ErrInvalidProgram
		}
		for _, item := range stmt.Diagnostics {
			if item.Target <= 0 || item.Target > datums || item.Kind == "" {
				return ErrInvalidProgram
			}
		}
		if err := validateStatements(ctx, stmt.Body, limits, depth+1, count, queries, datums); err != nil {
			return err
		}
		if err := validateStatements(ctx, stmt.Else, limits, depth+1, count, queries, datums); err != nil {
			return err
		}
		for _, b := range stmt.Branches {
			if b.Query <= 0 || b.Query > queries {
				return ErrInvalidProgram
			}
			if err := validateStatements(ctx, b.Body, limits, depth+1, count, queries, datums); err != nil {
				return err
			}
		}
		for _, handler := range stmt.Exceptions {
			if len(handler.Conditions) == 0 {
				return ErrInvalidProgram
			}
			if err := validateStatements(ctx, handler.Body, limits, depth+1, count, queries, datums); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Program) rawStatement(index int) (*zpgparse.RawStmt, error) {
	if index <= 0 || index > len(p.queries) {
		return nil, ErrInvalidProgram
	}
	node, ok := proto.Clone(p.queries[index-1].node).(*zpgparse.Node)
	if !ok || node == nil {
		return nil, ErrInvalidProgram
	}
	return &zpgparse.RawStmt{Stmt: node}, nil
}
