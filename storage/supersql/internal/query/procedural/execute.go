package procedural

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type callDepthKey struct{}

type frame struct {
	program      *Program
	executor     SQLExecutor
	trigger      *TriggerContext
	result       model.TypeLayout
	values       []model.Value
	layouts      []model.TypeLayout
	assigned     []bool
	steps        int
	rows         int
	bytes        uint64
	results      [][]model.Value
	emit         func([]model.Value) error
	resultRows   int
	lastResult   SQLResult
	currentError error
	cursors      map[int]*routineCursor
}

type control uint8

const (
	controlNone control = iota
	controlReturn
	controlExit
	controlContinue
)

type signal struct {
	kind  control
	label string
}

type routineCursor struct {
	columns  []model.TypeLayout
	names    []string
	rows     [][]model.Value
	position int
	name     string
	noScroll bool
}

// resolvedSignature carries the layouts a call site inferred for a routine
// whose declaration contains polymorphic markers; inputs substitutes each
// marker-declared argument position and result substitutes a marker result.
type resolvedSignature struct {
	inputs []model.TypeLayout
	result model.TypeLayout
}

func markerLayout(layout model.TypeLayout) bool {
	return len(layout.Children) == 0 && len(layout.FieldNames) == 0 && layout.Type.Modifier == -1 && model.IsPseudoTypeID(layout.Type.ID)
}

// containsMarkerLayout reports whether a declared layout is or embeds a
// pseudo-type marker — a multi-output routine's record result carries marker
// children.
func containsMarkerLayout(layout model.TypeLayout) bool {
	if markerLayout(layout) {
		return true
	}
	for i := range layout.Children {
		if containsMarkerLayout(layout.Children[i]) {
			return true
		}
	}
	return false
}

func (p *Program) resolvedResult(signature *resolvedSignature) model.TypeLayout {
	if signature != nil && signature.result.Type.ID != 0 && containsMarkerLayout(p.result) {
		return signature.result
	}
	return p.result
}

func (p *Program) Execute(ctx context.Context, arguments []model.Value) (SQLResult, error) {
	return p.executeResolved(ctx, arguments, nil)
}

func (p *Program) executeResolved(ctx context.Context, arguments []model.Value, signature *resolvedSignature) (SQLResult, error) {
	if err := ctx.Err(); err != nil {
		return SQLResult{}, err
	}
	depth, _ := ctx.Value(callDepthKey{}).(int)
	if depth >= p.limits.MaxDepth {
		return SQLResult{}, ErrLimit
	}
	ctx = context.WithValue(ctx, callDepthKey{}, depth+1)
	executor, err := SQLExecutorFromContext(ctx)
	if err != nil {
		return SQLResult{}, err
	}
	result := p.resolvedResult(signature)
	return executor.RunRoutine(ctx, RoutineInvocation{Procedure: p.procedure, Atomic: p.atomic, SecurityDefiner: p.securityDefiner, Result: result, Settings: p.settings}, func(invocation context.Context) (SQLResult, error) {
		return p.executeWithSink(invocation, arguments, executor, nil, nil, signature)
	})
}

func (p *Program) ExecuteRows(ctx context.Context, arguments []model.Value, emit func([]model.Value) error) error {
	return p.executeRowsResolved(ctx, arguments, emit, nil)
}

func (p *Program) executeRowsResolved(ctx context.Context, arguments []model.Value, emit func([]model.Value) error, signature *resolvedSignature) error {
	if !p.returnsSet || emit == nil {
		return ErrInvalidProgram
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	depth, _ := ctx.Value(callDepthKey{}).(int)
	if depth >= p.limits.MaxDepth {
		return ErrLimit
	}
	ctx = context.WithValue(ctx, callDepthKey{}, depth+1)
	executor, err := SQLExecutorFromContext(ctx)
	if err != nil {
		return err
	}
	result := p.resolvedResult(signature)
	validated := func(row []model.Value) error {
		if len(row) != 1 {
			return sqlError("42804", "returned row does not match function result type", nil)
		}
		if result.Type.Storage == model.TypeComposite && len(result.Children) != 0 && !row[0].IsNull && row[0].Type == model.TypeComposite {
			// The consumer compares the produced row against the specified
			// shape before deeper value validation runs.
			fields := row[0].ArrayVal
			if len(fields) != len(result.Children) {
				return sqlError("42804", "function return row and query-specified return row do not match", nil)
			}
			for i, field := range fields {
				if !field.IsNull && field.Type != result.Children[i].Type.Storage {
					return sqlError("42804", "function return row and query-specified return row do not match", nil)
				}
			}
		}
		if err := executor.ValidateRoutineValue(ctx, result, row[0]); err != nil {
			return err
		}
		return emit(row)
	}
	_, err = executor.RunRoutine(ctx, RoutineInvocation{Procedure: p.procedure, Atomic: p.atomic, SecurityDefiner: p.securityDefiner, Result: result, Settings: p.settings}, func(invocation context.Context) (SQLResult, error) {
		return p.executeWithSink(invocation, arguments, executor, nil, validated, signature)
	})
	return err
}

func (p *Program) execute(ctx context.Context, arguments []model.Value, executor SQLExecutor, trigger *TriggerContext) (SQLResult, error) {
	return p.executeWithSink(ctx, arguments, executor, trigger, nil, nil)
}

func (p *Program) executeWithSink(ctx context.Context, arguments []model.Value, executor SQLExecutor, trigger *TriggerContext, emit func([]model.Value) error, signature *resolvedSignature) (SQLResult, error) {
	if len(arguments) != len(p.inputs) {
		return SQLResult{}, sqlError("22023", "routine argument count does not match compiled signature", nil)
	}
	f := &frame{program: p, executor: executor, trigger: trigger, result: p.resolvedResult(signature), values: make([]model.Value, len(p.datums)), layouts: make([]model.TypeLayout, len(p.datums)), assigned: make([]bool, len(p.datums)), emit: emit}
	for i, d := range p.datums {
		f.layouts[i] = d.Layout
		storage := d.Layout.Type.Storage
		if storage.Valid() {
			f.values[i] = model.NullVal(storage)
		}
		if d.Name == "found" {
			f.values[i], f.assigned[i] = model.BoolVal(false), true
		}
	}
	if trigger != nil {
		if err := f.initializeTrigger(trigger); err != nil {
			return SQLResult{}, err
		}
	}
	for i, index := range p.inputs {
		value := arguments[i].Clone()
		declared := p.datums[index].Layout
		if markerLayout(declared) {
			if signature == nil || i >= len(signature.inputs) || signature.inputs[i].Type.ID == 0 {
				return SQLResult{}, sqlError("42804", "polymorphic routine argument has no resolved type", nil)
			}
			f.layouts[index] = signature.inputs[i]
			declared = signature.inputs[i]
		}
		if value.Type != declared.Type.Storage && !value.IsNull {
			return SQLResult{}, sqlError("42804", "routine argument physical type does not match compiled signature", nil)
		}
		if value.IsNull {
			value.Type = declared.Type.Storage
		}
		f.values[index], f.assigned[index] = value, true
	}
	if len(p.outputs) != 0 {
		// Output-only parameters never take a call argument, so the input
		// substitution above cannot reach them; their concrete layouts ride
		// the resolved result instead — a single output is the result itself,
		// multiple outputs form its record children in declaration order.
		switch {
		case len(p.outputs) == 1:
			f.layouts[p.outputs[0]] = f.result
		case len(f.result.Children) == len(p.outputs):
			for i, index := range p.outputs {
				f.layouts[index] = f.result.Children[i]
			}
		}
		for _, index := range p.outputs {
			if !f.assigned[index] {
				f.values[index] = model.NullVal(f.layouts[index].Type.Storage)
			}
		}
	}
	for i, d := range p.datums {
		if f.assigned[i] || d.Default == 0 {
			continue
		}
		value, err := f.evaluate(ctx, d.Default)
		if err != nil {
			return SQLResult{}, err
		}
		if err := f.assign(i, value, nil); err != nil {
			return SQLResult{}, err
		}
	}
	sig, err := f.run(ctx, p.statements)
	if err != nil {
		return SQLResult{}, err
	}
	if sig.kind == controlExit || sig.kind == controlContinue {
		return SQLResult{}, ErrInvalidProgram
	}
	if p.language == "sql" && p.returnsSet && f.emit != nil {
		produced := f.producedLayout(f.lastResult)
		for _, row := range f.lastResult.Rows {
			if err := f.appendResult(row, produced); err != nil {
				return SQLResult{}, err
			}
		}
		return SQLResult{Columns: []model.TypeLayout{f.result}}, nil
	}
	if p.language == "sql" {
		if p.procedure {
			return f.outputParameters(), nil
		}
		if produced := f.producedLayout(f.lastResult); produced.Type.ID != 0 && f.resultMismatch(produced) {
			return SQLResult{}, sqlError("42804", "returned row does not match function result type", nil)
		}
		if len(f.lastResult.Rows) != 0 {
			// PostgreSQL takes the last row of a non-set routine's body as its
			// result; for a composite result the row's columns are the fields.
			last := f.lastResult.Rows[len(f.lastResult.Rows)-1]
			if f.result.Type.Storage == model.TypeComposite {
				last = []model.Value{{Type: model.TypeComposite, ArrayVal: last}}
			}
			f.lastResult.Rows = [][]model.Value{last}
		}
		return f.lastResult, nil
	}
	if p.procedure {
		return f.outputParameters(), nil
	}
	if p.returnsSet {
		return SQLResult{Columns: []model.TypeLayout{f.result}, Rows: f.results}, nil
	}
	if len(f.results) == 0 {
		return SQLResult{}, sqlError("2F005", "function reached end without RETURN", nil)
	}
	return SQLResult{Columns: []model.TypeLayout{f.result}, Rows: f.results[:1]}, nil
}

func (p *Program) ExecuteTrigger(ctx context.Context, trigger TriggerContext) (*model.Row, bool, error) {
	executor, err := SQLExecutorFromContext(ctx)
	if err != nil {
		return nil, false, err
	}
	result, err := executor.RunRoutine(ctx, RoutineInvocation{Atomic: p.atomic, SecurityDefiner: p.securityDefiner, Result: trigger.Layout, Settings: p.settings}, func(invocation context.Context) (SQLResult, error) {
		return p.execute(invocation, nil, executor, &trigger)
	})
	if err != nil {
		return nil, false, err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
		return nil, false, sqlError("42804", "trigger function did not return one row value", nil)
	}
	value := result.Rows[0][0]
	if value.IsNull {
		return nil, false, nil
	}
	if value.Type != model.TypeComposite || len(value.ArrayVal) != len(trigger.Layout.Children) {
		return nil, false, sqlError("42804", "trigger function returned an incompatible row", nil)
	}
	row := &model.Row{Values: make([]model.Value, len(value.ArrayVal))}
	for i := range value.ArrayVal {
		row.Values[i] = value.ArrayVal[i].Clone()
	}
	return row, true, nil
}

func (f *frame) initializeTrigger(trigger *TriggerContext) error {
	setRow := func(number int, row *model.Row) {
		if number <= 0 || number > len(f.values) {
			return
		}
		index := number - 1
		f.layouts[index] = trigger.Layout
		f.values[index] = model.NullVal(model.TypeComposite)
		if row != nil {
			fields := make([]model.Value, len(row.Values))
			for i := range row.Values {
				fields[i] = row.Values[i].Clone()
			}
			f.values[index] = model.Value{Type: model.TypeComposite, ArrayVal: fields}
		}
		f.assigned[index] = true
	}
	setRow(f.program.newDatum, trigger.New)
	setRow(f.program.oldDatum, trigger.Old)
	text := map[string]string{
		"tg_name": trigger.Name, "tg_when": trigger.Timing, "tg_op": trigger.Event,
		"tg_level": trigger.Level, "tg_table_name": trigger.TableName,
		"tg_relname": trigger.TableName, "tg_table_schema": trigger.SchemaName,
	}
	for i, d := range f.program.datums {
		switch d.Name {
		case "tg_relid":
			f.values[i], f.assigned[i] = model.Int64Val(int64(trigger.Relation)), true
		case "tg_nargs":
			f.values[i], f.assigned[i] = model.Int64Val(int64(len(trigger.Arguments))), true
		case "tg_argv":
			values := make([]model.Value, len(trigger.Arguments))
			for j, argument := range trigger.Arguments {
				values[j] = model.StringVal(argument)
			}
			f.values[i] = model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 0}}}
			f.assigned[i] = true
		default:
			if value, exists := text[d.Name]; exists {
				f.values[i], f.assigned[i] = model.StringVal(value), true
			}
		}
	}
	return nil
}

func (f *frame) outputParameters() SQLResult {
	value := model.NullVal(f.resultStorage())
	if len(f.program.outputs) == 1 {
		value = f.values[f.program.outputs[0]].Clone()
	} else if len(f.program.outputs) != 0 {
		value = model.Value{Type: model.TypeComposite, ArrayVal: make([]model.Value, len(f.program.outputs))}
		for i, index := range f.program.outputs {
			value.ArrayVal[i] = f.values[index].Clone()
		}
	}
	return SQLResult{Columns: []model.TypeLayout{f.result}, Rows: [][]model.Value{{value}}}
}

func (f *frame) run(ctx context.Context, statements []statement) (signal, error) {
	for _, stmt := range statements {
		if err := ctx.Err(); err != nil {
			return signal{}, err
		}
		f.steps++
		if f.steps > f.program.limits.MaxSteps {
			return signal{}, ErrLimit
		}
		sig, err := f.runStatement(ctx, stmt)
		if err != nil {
			return sig, f.contextualError(err, stmt.Line)
		}
		if sig.kind != controlNone {
			return sig, nil
		}
	}
	return signal{}, nil
}

func (f *frame) contextText(line int, operation string) string {
	text := "PL/pgSQL function"
	if f.program.language == "sql" {
		text = "SQL function"
	}
	if f.program.routineName != "" {
		text += " " + f.program.routineName
	}
	if line > 0 {
		text += " line " + strconv.Itoa(line)
	}
	if operation != "" {
		text += " at " + operation
	}
	return text
}

func (f *frame) contextualError(err error, line int) error {
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Context != "" {
		return err
	}
	owned := *sqlErr
	owned.Context = f.contextText(line, "SQL statement")
	owned.Cause = err
	return &owned
}

func (f *frame) runStatement(ctx context.Context, stmt statement) (signal, error) {
	switch stmt.Kind {
	case stmtBlock:
		return f.runBlock(ctx, stmt)
	case stmtAssign:
		value, err := f.evaluate(ctx, stmt.Query)
		if err != nil {
			return signal{}, err
		}
		return signal{}, f.assign(stmt.Target-1, value, nil)
	case stmtIf:
		matched, err := f.condition(ctx, stmt.Query)
		if err != nil {
			return signal{}, err
		}
		if matched {
			return f.run(ctx, stmt.Body)
		}
		for _, branch := range stmt.Branches {
			matched, err = f.condition(ctx, branch.Query)
			if err != nil {
				return signal{}, err
			}
			if matched {
				return f.run(ctx, branch.Body)
			}
		}
		return f.run(ctx, stmt.Else)
	case stmtCase:
		if stmt.Query != 0 {
			if _, err := f.evaluate(ctx, stmt.Query); err != nil {
				return signal{}, err
			}
		}
		for _, branch := range stmt.Branches {
			matched, err := f.condition(ctx, branch.Query)
			if err != nil {
				return signal{}, err
			}
			if matched {
				return f.run(ctx, branch.Body)
			}
		}
		if stmt.Strict {
			return f.run(ctx, stmt.Else)
		}
		return signal{}, sqlError("20000", "case not found", nil)
	case stmtLoop:
		return f.runLoop(ctx, stmt)
	case stmtExit:
		if stmt.Query != 0 {
			matched, err := f.condition(ctx, stmt.Query)
			if err != nil || !matched {
				return signal{}, err
			}
		}
		return signal{kind: controlExit, label: stmt.Label}, nil
	case stmtContinue:
		if stmt.Query != 0 {
			matched, err := f.condition(ctx, stmt.Query)
			if err != nil || !matched {
				return signal{}, err
			}
		}
		return signal{kind: controlContinue, label: stmt.Label}, nil
	case stmtReturn, stmtReturnNext, stmtReturnQuery:
		return f.runReturn(ctx, stmt)
	case stmtExecute, stmtPerform, stmtCall:
		result, err := f.executeStatement(ctx, stmt)
		if err != nil {
			return signal{}, err
		}
		f.lastResult = result
		if stmt.Kind == stmtPerform || stmt.SetsFound {
			f.setFound(len(result.Rows) != 0 || result.RowsAffected != 0)
		}
		return signal{}, nil
	case stmtOpen:
		return signal{}, f.openCursor(ctx, stmt)
	case stmtFetch:
		return signal{}, f.fetchCursor(ctx, stmt)
	case stmtClose:
		return signal{}, f.closeCursor(stmt.Cursor)
	case stmtGetDiagnostics:
		return signal{}, f.getDiagnostics(stmt)
	case stmtTransaction:
		f.clearCursors()
		return signal{}, f.executor.TransactionControl(ctx, TransactionCommand{Rollback: stmt.Rollback, Chain: stmt.Chain})
	case stmtRaise:
		return signal{}, f.raise(ctx, stmt)
	case stmtAssert:
		matched, err := f.condition(ctx, stmt.Query)
		if err != nil {
			return signal{}, err
		}
		if matched {
			return signal{}, nil
		}
		message := "assertion failed"
		if stmt.Auxiliary != 0 {
			value, evalErr := f.evaluate(ctx, stmt.Auxiliary)
			if evalErr != nil {
				return signal{}, evalErr
			}
			message, evalErr = textValue(value)
			if evalErr != nil {
				return signal{}, evalErr
			}
		}
		return signal{}, sqlError("P0004", message, nil)
	default:
		return signal{}, ErrInvalidProgram
	}
}

func (f *frame) runBlock(ctx context.Context, stmt statement) (signal, error) {
	if len(stmt.Exceptions) == 0 {
		sig, err := f.run(ctx, stmt.Body)
		if sig.kind == controlExit && sig.label != "" && sig.label == stmt.Label {
			return signal{}, err
		}
		return sig, err
	}
	var sig signal
	cursorState := f.saveCursors()
	err := f.executor.RunSubtransaction(ctx, func(subctx context.Context) error {
		var runErr error
		sig, runErr = f.run(subctx, stmt.Body)
		return runErr
	})
	if err == nil {
		return sig, nil
	}
	f.restoreCursors(cursorState)
	for _, handler := range stmt.Exceptions {
		if matchesCondition(err, handler.Conditions) {
			previous := f.currentError
			f.currentError = err
			restore := f.setExceptionVariables(err)
			sig, handlerErr := f.run(ctx, handler.Body)
			restore()
			f.currentError = previous
			return sig, handlerErr
		}
	}
	return signal{}, err
}

func (f *frame) setExceptionVariables(err error) func() {
	var sqlErr *model.Error
	code, message := "XX000", err.Error()
	if errors.As(err, &sqlErr) {
		code, message = sqlErr.Code, sqlErr.Message
	}
	var indices []int
	var values []model.Value
	var assigned []bool
	for i, descriptor := range f.program.datums {
		var text string
		switch descriptor.Name {
		case "sqlstate":
			text = code
		case "sqlerrm":
			text = message
		default:
			continue
		}
		indices = append(indices, i)
		values = append(values, f.values[i].Clone())
		assigned = append(assigned, f.assigned[i])
		f.values[i], f.assigned[i] = model.StringVal(text), true
	}
	return func() {
		for i, index := range indices {
			f.values[index] = values[i]
			f.assigned[index] = assigned[i]
		}
	}
}

type cursorSavepoint struct {
	open      map[int]*routineCursor
	positions map[int]int
}

func (f *frame) saveCursors() cursorSavepoint {
	if len(f.cursors) == 0 {
		return cursorSavepoint{}
	}
	state := cursorSavepoint{open: make(map[int]*routineCursor, len(f.cursors)), positions: make(map[int]int, len(f.cursors))}
	for number, cursor := range f.cursors {
		state.open[number] = cursor
		state.positions[number] = cursor.position
	}
	return state
}

func (f *frame) restoreCursors(state cursorSavepoint) {
	for number, cursor := range f.cursors {
		if state.open[number] != cursor {
			for i := range cursor.rows {
				clear(cursor.rows[i])
			}
			clear(cursor.rows)
			clear(cursor.columns)
		}
	}
	f.cursors = state.open
	for number, position := range state.positions {
		f.cursors[number].position = position
	}
}
func (f *frame) clearCursors() {
	for _, cursor := range f.cursors {
		for i := range cursor.rows {
			clear(cursor.rows[i])
		}
		clear(cursor.rows)
		clear(cursor.columns)
	}
	clear(f.cursors)
	f.cursors = nil
}

func (f *frame) runLoop(ctx context.Context, stmt statement) (signal, error) {
	runBody := func() (bool, signal, error) {
		sig, err := f.run(ctx, stmt.Body)
		if err != nil {
			return false, signal{}, err
		}
		if sig.kind == controlReturn {
			return false, sig, nil
		}
		if sig.kind == controlExit && (sig.label == "" || sig.label == stmt.Label) {
			return false, signal{}, nil
		}
		if sig.kind == controlContinue && (sig.label == "" || sig.label == stmt.Label) {
			return true, signal{}, nil
		}
		if sig.kind != controlNone {
			return false, sig, nil
		}
		return true, signal{}, nil
	}
	switch stmt.LoopKind {
	case "loop":
		for {
			again, sig, err := runBody()
			if err != nil || !again {
				return sig, err
			}
		}
	case "while":
		for {
			matched, err := f.condition(ctx, stmt.Parameters[0])
			if err != nil || !matched {
				return signal{}, err
			}
			again, sig, err := runBody()
			if err != nil || !again {
				return sig, err
			}
		}
	case "fori":
		lower, err := f.integer(ctx, stmt.Parameters[1])
		if err != nil {
			return signal{}, err
		}
		upper, err := f.integer(ctx, stmt.Parameters[2])
		if err != nil {
			return signal{}, err
		}
		step := int64(1)
		if stmt.Parameters[3] != 0 {
			step, err = f.integer(ctx, stmt.Parameters[3])
			if err != nil || step <= 0 {
				return signal{}, errors.Join(err, sqlError("22023", "BY value of FOR loop must be greater than zero", nil))
			}
		}
		if stmt.Reverse {
			step = -step
		}
		iterated := false
		for current := lower; (step > 0 && current <= upper) || (step < 0 && current >= upper); current += step {
			iterated = true
			if err := f.assign(stmt.Target-1, model.Int64Val(current), nil); err != nil {
				return signal{}, err
			}
			again, sig, err := runBody()
			if err != nil || !again {
				f.setFound(iterated)
				return sig, err
			}
			if step > 0 && current > math.MaxInt64-step || step < 0 && current < math.MinInt64-step {
				break
			}
		}
		f.setFound(iterated)
	case "fors", "forc", "ford":
		query := stmt.Parameters[4]
		if query == 0 {
			query = stmt.Parameters[5]
		}
		result, err := f.executeQuery(ctx, query, nil)
		if err != nil {
			return signal{}, err
		}
		return f.runQueryLoop(ctx, stmt, result, runBody)
	case "dynfors":
		// PostgreSQL plans the dynamic command when the loop starts, against the
		// routine's search_path, and drives an implicit no-scroll cursor. The
		// statement is executed through the same dynamic path EXECUTE uses, so
		// USING parameters are bound by position exactly as there.
		result, err := f.executeDynamic(ctx, stmt.Parameters[4], stmt.Using)
		if err != nil {
			return signal{}, err
		}
		return f.runQueryLoop(ctx, stmt, result, runBody)
	case "foreach_a":
		value, err := f.evaluate(ctx, stmt.Parameters[4])
		if err != nil {
			return signal{}, err
		}
		if value.IsNull {
			return signal{}, sqlError("22004", "FOREACH expression must not be null", nil)
		}
		if value.Type != model.TypeArray {
			return signal{}, sqlError("42804", "FOREACH expression is not an array", nil)
		}
		if stmt.Slice < 0 || stmt.Slice > len(value.Dimensions) {
			return signal{}, sqlError("2202E", fmt.Sprintf("slice dimension (%d) is out of the valid range 0..%d", stmt.Slice, len(value.Dimensions)), nil)
		}
		// PostgreSQL derives the loop variable's array-ness from its declared
		// type; a record or row variable can hold anything and is deliberately
		// exempt, so the check is skipped for those two kinds.
		descriptor := f.program.datums[stmt.Target-1]
		loopVarArray := descriptor.Kind != datumRecord && descriptor.Kind != datumRow && f.layouts[stmt.Target-1].Type.Storage == model.TypeArray
		if stmt.Slice > 0 && !loopVarArray {
			return signal{}, sqlError("42804", "FOREACH ... SLICE loop variable must be of an array type", nil)
		}
		if stmt.Slice == 0 && loopVarArray {
			return signal{}, sqlError("42804", "FOREACH loop variable must not be of an array type", nil)
		}
		iterated := false
		if stmt.Slice > 0 {
			// A slice is a contiguous run of the array's rightmost dimensions and
			// keeps those dimensions' bounds, so nested sub-arrays stay valid.
			dimensions := value.Dimensions[len(value.Dimensions)-stmt.Slice:]
			length := 1
			for _, dimension := range dimensions {
				length *= int(dimension.Length)
			}
			// An empty dimension makes the whole array have no elements, so the
			// iterator yields nothing rather than a run of empty slices.
			for start := 0; length > 0 && start+length <= len(value.ArrayVal); start += length {
				iterated = true
				slice := model.Value{Type: model.TypeArray, Dimensions: dimensions, ArrayVal: value.ArrayVal[start : start+length]}
				if err := f.assign(stmt.Target-1, slice, nil); err != nil {
					return signal{}, err
				}
				again, sig, err := runBody()
				if err != nil || !again {
					f.setFound(iterated)
					return sig, err
				}
			}
		} else {
			for _, element := range value.ArrayVal {
				iterated = true
				if err := f.assign(stmt.Target-1, element, nil); err != nil {
					return signal{}, err
				}
				again, sig, err := runBody()
				if err != nil || !again {
					f.setFound(iterated)
					return sig, err
				}
			}
		}
		f.setFound(iterated)
	default:
		return signal{}, ErrInvalidProgram
	}
	return signal{}, nil
}

// runQueryLoop drives a FOR-over-query loop (static or dynamic). PostgreSQL
// assigns FOUND after the loop finishes for any reason, including an EXIT or
// RETURN from the body, so the value never leaks out of the loop body itself.
func (f *frame) runQueryLoop(ctx context.Context, stmt statement, result SQLResult, runBody func() (bool, signal, error)) (signal, error) {
	f.lastResult = result
	f.setRecordLayout(stmt.Target-1, result.Columns, result.ColumnNames)
	iterated := false
	for _, row := range result.Rows {
		iterated = true
		if err := f.assign(stmt.Target-1, model.Value{}, row); err != nil {
			return signal{}, err
		}
		again, sig, err := runBody()
		if err != nil || !again {
			f.setFound(iterated)
			return sig, err
		}
	}
	f.setFound(iterated)
	return signal{}, nil
}

func (f *frame) runReturn(ctx context.Context, stmt statement) (signal, error) {
	if stmt.Kind == stmtReturnQuery {
		var result SQLResult
		var err error
		if stmt.Query != 0 {
			result, err = f.executeQuery(ctx, stmt.Query, nil)
		} else {
			result, err = f.executeDynamic(ctx, stmt.Auxiliary, stmt.Parameters)
		}
		if err != nil {
			return signal{}, err
		}
		f.setFound(len(result.Rows) != 0)
		produced := f.producedLayout(result)
		for _, row := range result.Rows {
			if err := f.appendResult(row, produced); err != nil {
				return signal{}, err
			}
		}
		return signal{}, nil
	}
	if stmt.Kind == stmtReturn && f.emit != nil {
		// A plain RETURN in a set-returning routine only terminates it;
		// RETURN NEXT and RETURN QUERY own row emission.
		return signal{kind: controlReturn}, nil
	}
	var row []model.Value
	var produced model.TypeLayout
	if stmt.Query != 0 {
		result, err := f.executeQuery(ctx, stmt.Query, nil)
		if err != nil {
			return signal{}, err
		}
		if len(result.Rows) != 1 {
			return signal{}, sqlError("21000", "RETURN expression did not produce exactly one row", nil)
		}
		row = result.Rows[0]
		produced = f.producedLayout(result)
	} else if stmt.Target != 0 {
		index := stmt.Target - 1
		if index < 0 || index >= len(f.values) || f.program.datums[index].Kind == datumField {
			return signal{}, ErrInvalidProgram
		}
		row = []model.Value{f.values[index].Clone()}
		produced = f.layouts[index]
	} else {
		output := f.outputParameters()
		if len(output.Rows) != 0 {
			row = output.Rows[0]
		}
	}
	if err := f.appendResult(row, produced); err != nil {
		return signal{}, err
	}
	if stmt.Kind == stmtReturnNext {
		return signal{}, nil
	}
	return signal{kind: controlReturn}, nil
}

func (f *frame) appendResult(row []model.Value, produced model.TypeLayout) error {
	if produced.Type.ID != 0 && f.resultMismatch(produced) {
		return sqlError("42804", "returned row does not match function result type", nil)
	}
	var output []model.Value
	if len(row) == 1 {
		output = []model.Value{row[0].Clone()}
		if output[0].IsNull {
			output[0].Type = f.resultStorage()
		}
	} else if f.result.Type.Storage == model.TypeComposite {
		fields := make([]model.Value, len(row))
		for i := range row {
			fields[i] = row[i].Clone()
		}
		output = []model.Value{{Type: model.TypeComposite, ArrayVal: fields}}
	} else {
		return sqlError("42804", "returned row does not match function result type", nil)
	}
	f.resultRows++
	if f.resultRows > f.program.limits.MaxRows {
		return ErrLimit
	}
	if f.emit != nil {
		return f.emit(output)
	}
	f.results = append(f.results, output)
	return nil
}

func (f *frame) resultStorage() model.DataType {
	if f.trigger != nil {
		return f.trigger.Layout.Type.Storage
	}
	return f.result.Type.Storage
}

// producedLayout is the column layout a query result produced, when it is a
// single scalar column; multi-column results pack into the composite result
// instead and carry no comparable layout.
func (f *frame) producedLayout(result SQLResult) model.TypeLayout {
	if len(result.Columns) != 1 {
		return model.TypeLayout{}
	}
	return result.Columns[0]
}

// resultMismatch detects produced layouts that share opaque byte storage with
// the resolved result but are different types: ranges and multiranges encode
// as raw bytes, so storage-level validation alone cannot tell a produced
// int4range from a declared int4multirange, and no implicit coercion exists
// between a range-family type and any other TypeBytes member.
func (f *frame) resultMismatch(produced model.TypeLayout) bool {
	if produced.Type.ID == f.result.Type.ID || produced.Type.Storage != model.TypeBytes || f.result.Type.Storage != model.TypeBytes {
		return false
	}
	_, resultRange := builtin.LookupRangeFamily(f.result.Type.ID)
	_, producedRange := builtin.LookupRangeFamily(produced.Type.ID)
	return resultRange || producedRange
}

func (f *frame) executeStatement(ctx context.Context, stmt statement) (SQLResult, error) {
	var result SQLResult
	var err error
	if stmt.Auxiliary != 0 {
		result, err = f.executeDynamic(ctx, stmt.Auxiliary, stmt.Parameters)
	} else if stmt.Kind == stmtCall {
		result, err = f.executeProcedureCall(ctx, stmt.Query)
	} else {
		result, err = f.executeQuery(ctx, stmt.Query, nil)
	}
	if err != nil {
		return SQLResult{}, err
	}
	if stmt.Target == 0 {
		return result, nil
	}
	if stmt.Strict && len(result.Rows) != 1 {
		code, message := "P0002", "query returned no rows"
		if len(result.Rows) > 1 {
			code, message = "P0003", "query returned more than one row"
		}
		return SQLResult{}, sqlError(code, message, nil)
	}

	f.setRecordLayout(stmt.Target-1, result.Columns, result.ColumnNames)
	if len(result.Rows) == 0 {
		return result, f.assign(stmt.Target-1, model.NullVal(f.layouts[stmt.Target-1].Type.Storage), nil)
	}
	return result, f.assign(stmt.Target-1, model.Value{}, result.Rows[0])
}

func (f *frame) openCursor(ctx context.Context, stmt statement) error {
	cursor := stmt.Target - 1
	if cursor < 0 || cursor >= len(f.program.datums) {
		return ErrInvalidProgram
	}
	if f.cursors != nil {
		if _, open := f.cursors[cursor]; open {
			return sqlError("42P03", "cursor is already open", nil)
		}
	}
	definition := f.program.datums[cursor]
	if stmt.Argument != 0 {
		if definition.CursorArgRow <= 0 || definition.CursorArgRow > len(f.values) {
			return ErrInvalidProgram
		}
		arguments, err := f.executeQuery(ctx, stmt.Argument, nil)
		if err != nil {
			return err
		}
		if len(arguments.Rows) != 1 {
			return sqlError("21000", "cursor argument expression did not return one row", nil)
		}
		target := definition.CursorArgRow - 1
		fields := f.program.datums[target].Fields
		if len(fields) != len(arguments.Rows[0]) {
			return sqlError("42804", "cursor arguments do not match declaration", nil)
		}
		for i, field := range fields {
			if err := f.assign(field, arguments.Rows[0][i], nil); err != nil {
				return err
			}
		}
	}
	query := stmt.Query
	if query == 0 {
		query = definition.CursorQuery
	}
	var result SQLResult
	var err error
	switch {
	case stmt.Auxiliary != 0:
		result, err = f.executeDynamic(ctx, stmt.Auxiliary, stmt.Parameters)
	case query != 0:
		result, err = f.executeQuery(ctx, query, nil)
	default:
		return sqlError("34000", "cursor query is not defined", nil)
	}
	if err != nil {
		return err
	}
	if f.cursors == nil {
		f.cursors = make(map[int]*routineCursor)
	}
	options := stmt.CursorOptions
	if options == 256 && definition.CursorOptions != 0 {
		options = definition.CursorOptions
	}
	portalName := definition.Name
	if f.assigned[cursor] && !f.values[cursor].IsNull {
		portalName, err = textValue(f.values[cursor])
		if err != nil {
			return err
		}
	}
	for _, open := range f.cursors {
		if open.name == portalName {
			return sqlError("42P03", "cursor is already open", nil)
		}
	}
	f.cursors[cursor] = &routineCursor{columns: result.Columns, names: result.ColumnNames, rows: result.Rows, name: portalName, noScroll: options&4 != 0}
	if err := f.assign(cursor, model.StringVal(portalName), nil); err != nil {
		delete(f.cursors, cursor)
		return err
	}
	f.lastResult = SQLResult{}
	return nil
}

func (f *frame) fetchCursor(ctx context.Context, stmt statement) error {
	cursor := stmt.Cursor - 1
	if cursor < 0 || cursor >= len(f.program.datums) || f.cursors == nil || f.cursors[cursor] == nil {
		return sqlError("34000", "cursor is not open", nil)
	}
	count := stmt.Count
	if stmt.Query != 0 {
		var err error
		count, err = f.integer(ctx, stmt.Query)
		if err != nil {
			return err
		}
	}
	state := f.cursors[cursor]
	if state.noScroll && (stmt.Direction != 0 || count < 0) {
		return sqlError("55000", "cursor can only scan forward", nil)
	}
	old := state.position
	switch stmt.Direction {
	case 0:
		state.position = cursorRelativePosition(old, count, len(state.rows))
	case 1:
		if count == -1<<63 {
			state.position = len(state.rows) + 1
		} else {
			state.position = cursorRelativePosition(old, -count, len(state.rows))
		}
	case 2:
		state.position = cursorAbsolutePosition(count, len(state.rows))
	case 3:
		state.position = cursorRelativePosition(old, count, len(state.rows))
	default:
		return ErrInvalidProgram
	}
	found := state.position > 0 && state.position <= len(state.rows)
	affected := uint64(0)
	if stmt.Move {
		affected = uint64(cursorMovedRows(old, state.position, len(state.rows)))
	} else if found {
		affected = 1
	}
	f.lastResult = SQLResult{RowsAffected: affected}
	foundStatus := found
	if stmt.Move {
		foundStatus = affected != 0
	}
	f.setFound(foundStatus)
	if stmt.Move || stmt.Target == 0 {
		return nil
	}
	target := stmt.Target - 1
	f.setRecordLayout(target, state.columns, state.names)
	if !found {
		return f.assign(target, model.NullVal(f.layouts[target].Type.Storage), nil)
	}
	return f.assign(target, model.Value{}, state.rows[state.position-1])
}

func (f *frame) closeCursor(number int) error {
	cursor := number - 1
	if cursor < 0 || f.cursors == nil || f.cursors[cursor] == nil {
		return sqlError("34000", "cursor is not open", nil)
	}
	delete(f.cursors, cursor)
	return nil
}

func cursorRelativePosition(position int, delta int64, rows int) int {
	limit := rows + 1
	if delta > int64(limit-position) {
		return limit
	}
	if delta < -int64(position) {
		return 0
	}
	return position + int(delta)
}

func cursorAbsolutePosition(position int64, rows int) int {
	if position < 0 {
		position = int64(rows) + 1 + position
	}
	if position < 0 {
		return 0
	}
	if position > int64(rows+1) {
		return rows + 1
	}
	return int(position)
}

func cursorMovedRows(from, to, rows int) int {
	if to > from {
		return min(to, rows) - min(from, rows)
	}
	return max(from, 1) - max(to, 1)
}

func (f *frame) setFound(found bool) {
	for i, descriptor := range f.program.datums {
		if descriptor.Name == "found" {
			f.values[i], f.assigned[i] = model.BoolVal(found), true
			return
		}
	}
}

func (f *frame) getDiagnostics(stmt statement) error {
	var diagnosticError error
	if stmt.Stacked {
		diagnosticError = f.currentError
		if diagnosticError == nil {
			return sqlError("0Z002", "GET STACKED DIAGNOSTICS cannot be used outside an exception handler", nil)
		}
	}
	var sqlErr *model.Error
	_ = errors.As(diagnosticError, &sqlErr)
	contextText := f.contextText(stmt.Line, "GET DIAGNOSTICS")
	for _, item := range stmt.Diagnostics {
		var value model.Value
		switch item.Kind {
		case "ROW_COUNT":
			count := f.lastResult.RowsAffected
			if count == 0 {
				count = uint64(len(f.lastResult.Rows))
			}
			if count > uint64(^uint64(0)>>1) {
				return ErrLimit
			}
			value = model.Int64Val(int64(count))
		case "PG_ROUTINE_OID":
			value = model.Int64Val(int64(f.program.routineOID))
		case "PG_CONTEXT":
			value = model.StringVal(contextText)
		case "RETURNED_SQLSTATE":
			if sqlErr != nil {
				value = model.StringVal(sqlErr.Code)
			} else {
				value = model.StringVal("")
			}
		case "MESSAGE_TEXT":
			if diagnosticError != nil {
				value = model.StringVal(diagnosticError.Error())
			} else {
				value = model.StringVal("")
			}
		case "PG_EXCEPTION_DETAIL":
			if sqlErr != nil {
				value = model.StringVal(sqlErr.Detail)
			} else {
				value = model.StringVal("")
			}
		case "PG_EXCEPTION_CONTEXT":
			context := contextText
			if sqlErr != nil && sqlErr.Context != "" {
				context = sqlErr.Context
			}
			value = model.StringVal(context)
		case "PG_EXCEPTION_HINT":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Hint }))
		case "COLUMN_NAME":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Column }))
		case "CONSTRAINT_NAME":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Constraint }))
		case "PG_DATATYPE_NAME":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Datatype }))
		case "TABLE_NAME":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Table }))
		case "SCHEMA_NAME":
			value = model.StringVal(sqlErrorField(sqlErr, func(value *model.Error) string { return value.Schema }))
		default:
			return ErrInvalidProgram
		}
		if err := f.assign(item.Target-1, value, nil); err != nil {
			return err
		}
	}
	return nil
}

func sqlErrorField(err *model.Error, field func(*model.Error) string) string {
	if err == nil {
		return ""
	}
	return field(err)
}

func (f *frame) executeDynamic(ctx context.Context, expression int, using []int) (SQLResult, error) {
	value, err := f.evaluate(ctx, expression)
	if err != nil {
		return SQLResult{}, err
	}
	query, err := textValue(value)
	if err != nil {
		return SQLResult{}, err
	}
	if len(query) > f.program.limits.MaxDynamicSQLBytes {
		return SQLResult{}, ErrLimit
	}
	parsed, err := zpgparse.Parse(query)
	if err != nil {
		return SQLResult{}, err
	}
	if len(parsed.Stmts) != 1 {
		return SQLResult{}, sqlError("42601", "dynamic query must contain one statement", nil)
	}
	parameters := make([]Parameter, len(using))
	for i, queryIndex := range using {
		value, layout, err := f.evaluateTyped(ctx, queryIndex)
		if err != nil {
			return SQLResult{}, err
		}
		parameters[i] = Parameter{Layout: layout, Value: value}
	}
	return f.executeRaw(ctx, parsed.Stmts[0], parameters, false)
}

func (f *frame) executeQuery(ctx context.Context, index int, parameters []Parameter) (SQLResult, error) {
	raw, err := f.program.rawStatement(index)
	if err != nil {
		return SQLResult{}, err
	}
	if parameters == nil {
		parameters, err = f.parameters(raw)
		if err != nil {
			return SQLResult{}, err
		}
	}
	return f.executeRaw(ctx, raw, parameters, false)
}
func (f *frame) executeProcedureCall(ctx context.Context, index int) (SQLResult, error) {
	raw, err := f.program.rawStatement(index)
	if err != nil {
		return SQLResult{}, err
	}
	parameters, err := f.parameters(raw)
	if err != nil {
		return SQLResult{}, err
	}
	return f.executeRaw(ctx, raw, parameters, true)
}

func (f *frame) executeRaw(ctx context.Context, raw *zpgparse.RawStmt, parameters []Parameter, allowTransactionControl bool) (SQLResult, error) {
	remainingRows := f.program.limits.MaxRows - f.rows
	remainingBytes := f.program.limits.MaxResultBytes - f.bytes
	if remainingRows <= 0 || remainingBytes == 0 {
		return SQLResult{}, ErrLimit
	}
	raw, parameters, err := expandTransitionRelations(ctx, raw, parameters, f.trigger)
	if err != nil {
		return SQLResult{}, err
	}
	result, err := f.executor.ExecuteSQL(ctx, SQLRequest{Statement: raw, Parameters: parameters, MaxRows: remainingRows, MaxBytes: remainingBytes, AllowTransactionControl: allowTransactionControl})
	if err != nil {
		return SQLResult{}, err
	}
	f.rows += len(result.Rows)
	for i := range result.Rows {
		for j := range result.Rows[i] {
			size, measureErr := model.ValueBytes(ctx, result.Rows[i][j], remainingBytes)
			if measureErr != nil || size > remainingBytes {
				return SQLResult{}, errors.Join(measureErr, ErrLimit)
			}
			f.bytes += size
			remainingBytes -= size
		}
	}
	if f.rows > f.program.limits.MaxRows || f.bytes > f.program.limits.MaxResultBytes {
		return SQLResult{}, ErrLimit
	}
	return result, nil
}

func (f *frame) parameters(raw *zpgparse.RawStmt) ([]Parameter, error) {
	var used map[uint32]struct{}
	parameters := make([]Parameter, len(f.values))
	for i := range f.values {
		if f.program.datums[i].Kind == datumField {
			if used == nil {
				used = make(map[uint32]struct{})
				collectParameterReferences(raw.Stmt, used)
			}
			if _, referenced := used[uint32(i+1)]; !referenced {
				// The statement never references this record field, so no
				// expression can read the slot. An unknown-typed NULL keeps an
				// unassigned parent record from failing statements that do not
				// touch the field; references bind the field's live layout.
				layout, ok := builtin.BuiltinTypeLayout(705, -1)
				if !ok {
					return nil, ErrInvalidProgram
				}
				parameters[i] = Parameter{Layout: layout, Value: model.NullVal(model.TypeString)}
				continue
			}
		}
		layout, value, err := f.parameter(i)
		if err != nil {
			return nil, err
		}
		parameters[i] = Parameter{Layout: layout, Value: value}
	}
	return parameters, nil
}

func collectParameterReferences(node *zpgparse.Node, used map[uint32]struct{}) {
	if node == nil {
		return
	}
	if ref := node.GetParamRef(); ref != nil && ref.Number > 0 {
		used[uint32(ref.Number)] = struct{}{}
	}
	_ = walkTransitionMessages(node.ProtoReflect(), func(child *zpgparse.Node) error {
		collectParameterReferences(child, used)
		return nil
	})
}

func (f *frame) parameter(index int) (model.TypeLayout, model.Value, error) {
	d := f.program.datums[index]
	if d.Kind != datumField {
		return f.layouts[index], f.values[index].Clone(), nil
	}
	parent, field, err := f.fieldIndex(d)
	if err != nil {
		return model.TypeLayout{}, model.Value{}, err
	}
	if field >= len(f.layouts[parent].Children) {
		return model.TypeLayout{}, model.Value{}, sqlError("42703", fmt.Sprintf("record %q has no field %q", f.program.datums[parent].Name, d.Field), nil)
	}
	layout := f.layouts[parent].Children[field]
	if f.values[parent].IsNull {
		return layout, model.NullVal(layout.Type.Storage), nil
	}
	if field >= len(f.values[parent].ArrayVal) {
		return model.TypeLayout{}, model.Value{}, sqlError("42703", fmt.Sprintf("record %q has no field %q", f.program.datums[parent].Name, d.Field), nil)
	}
	return layout, f.values[parent].ArrayVal[field].Clone(), nil
}

func (f *frame) fieldIndex(d datum) (int, int, error) {
	if d.Parent <= 0 || d.Parent > len(f.values) {
		return 0, 0, ErrInvalidProgram
	}
	parent := d.Parent - 1
	names := f.layouts[parent].FieldNames
	if len(names) == 0 {
		names = f.program.datums[parent].FieldNames
	}
	if len(names) == 0 && f.values[parent].IsNull {
		return 0, 0, sqlError("55000", fmt.Sprintf("record %q is not assigned yet", f.program.datums[parent].Name), nil)
	}
	for i, name := range names {
		if name == d.Field {
			return parent, i, nil
		}
	}
	return 0, 0, sqlError("42703", fmt.Sprintf("record %q has no field %q", f.program.datums[parent].Name, d.Field), nil)
}

func (f *frame) evaluate(ctx context.Context, query int) (model.Value, error) {
	value, _, err := f.evaluateTyped(ctx, query)
	return value, err
}

func (f *frame) evaluateTyped(ctx context.Context, query int) (model.Value, model.TypeLayout, error) {
	result, err := f.executeQuery(ctx, query, nil)
	if err != nil {
		return model.Value{}, model.TypeLayout{}, err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 1 || len(result.Columns) != 1 {
		return model.Value{}, model.TypeLayout{}, sqlError("21000", "procedural expression did not produce one value", nil)
	}
	return result.Rows[0][0], result.Columns[0], nil
}

func (f *frame) condition(ctx context.Context, query int) (bool, error) {
	value, err := f.evaluate(ctx, query)
	if err != nil || value.IsNull {
		return false, err
	}
	if value.Type != model.TypeBoolean {
		return false, sqlError("42804", "procedural condition is not boolean", nil)
	}
	return value.IntVal != 0, nil
}

func (f *frame) integer(ctx context.Context, query int) (int64, error) {
	value, err := f.evaluate(ctx, query)
	if err != nil {
		return 0, err
	}
	if value.IsNull || value.Type != model.TypeInt64 {
		return 0, sqlError("42804", "integer loop expression is not an integer", nil)
	}
	return value.IntVal, nil
}

func (f *frame) setRecordLayout(index int, columns []model.TypeLayout, names []string) {
	if index < 0 || index >= len(f.layouts) {
		return
	}
	kind := f.program.datums[index].Kind
	if kind != datumRecord && kind != datumRow {
		return
	}
	// A declared composite (%ROWTYPE, named composite) keeps its fixed field
	// names: assigning reshapes values, not the type. An anonymous record
	// adopts the producing query's column names, matching PostgreSQL. The
	// declared names live on the datum because the durable layout codec
	// strips them from Layout.
	declared := f.program.datums[index].FieldNames
	if len(declared) == len(columns) {
		names = declared
	} else if len(names) != len(columns) {
		names = declared
	}
	layout := f.layouts[index]
	layout.Type.Storage = model.TypeComposite
	layout.Children = append(layout.Children[:0], columns...)
	layout.FieldNames = append(layout.FieldNames[:0], names...)
	f.layouts[index] = layout
}

func (f *frame) assign(index int, value model.Value, row []model.Value) error {
	if index < 0 || index >= len(f.values) {
		return ErrInvalidProgram
	}
	d := f.program.datums[index]
	if d.Constant && f.assigned[index] {
		return sqlError("22005", fmt.Sprintf("variable %q is declared CONSTANT", d.Name), nil)
	}
	if d.Kind == datumField {
		parent, field, err := f.fieldIndex(d)
		if err != nil {
			return err
		}
		if field >= len(f.layouts[parent].Children) {
			return sqlError("42703", fmt.Sprintf("record %q has no field %q", f.program.datums[parent].Name, d.Field), nil)
		}
		if f.values[parent].IsNull || field >= len(f.values[parent].ArrayVal) {
			return sqlError("55000", fmt.Sprintf("record %q is not assigned yet", f.program.datums[parent].Name), nil)
		}
		layout := f.layouts[parent].Children[field]
		if value.Type != layout.Type.Storage && !value.IsNull {
			return sqlError("42804", fmt.Sprintf("value does not match field %q", d.Field), nil)
		}
		if value.IsNull {
			value.Type = layout.Type.Storage
		}
		f.values[parent].ArrayVal[field] = value.Clone()
		return nil
	}
	if row != nil {
		if d.Kind == datumRow {
			// A row target groups earlier datums (SELECT ... INTO a, b; FOR
			// a, b IN). Each element is assigned to the datum it names, not
			// just to the anonymous row.
			if len(d.Fields) == 1 && f.layouts[d.Fields[0]].Type.Storage == model.TypeComposite {
				// A %ROWTYPE target is a single-field row whose field is the
				// composite variable itself: assign the whole result row as one
				// record, whatever its width.
				fields := make([]model.Value, len(row))
				for i := range row {
					fields[i] = row[i].Clone()
				}
				return f.assign(d.Fields[0], model.Value{Type: model.TypeComposite, ArrayVal: fields}, nil)
			}
			if len(row) != len(d.Fields) {
				return sqlError("42804", "query result does not match row target fields", nil)
			}
			for i, field := range d.Fields {
				if err := f.assign(field, row[i], nil); err != nil {
					return err
				}
			}
		}
		if d.Kind == datumRow || d.Kind == datumRecord {
			fields := make([]model.Value, len(row))
			for i := range row {
				fields[i] = row[i].Clone()
			}
			value = model.Value{Type: model.TypeComposite, ArrayVal: fields}
		} else if len(row) == 1 {
			value = row[0].Clone()
		} else {
			return sqlError("42804", "query result does not match assignment target", nil)
		}
	}
	if d.NotNull && value.IsNull {
		return sqlError("23502", fmt.Sprintf("null value cannot be assigned to variable %q", d.Name), nil)
	}
	if d.Kind == datumRow && value.IsNull {
		for _, field := range d.Fields {
			if err := f.assign(field, model.NullVal(model.TypeAny), nil); err != nil {
				return err
			}
		}
	}
	if d.Kind != datumRecord && d.Kind != datumRow && value.Type != f.layouts[index].Type.Storage && !value.IsNull {
		return sqlError("42804", fmt.Sprintf("value does not match variable %q", d.Name), nil)
	}
	if value.IsNull {
		value.Type = f.layouts[index].Type.Storage
	}
	f.values[index], f.assigned[index] = value.Clone(), true
	return nil
}

func (f *frame) raise(ctx context.Context, stmt statement) error {
	if stmt.Message == "" && stmt.Condition == "" && len(stmt.Parameters) == 0 {
		if f.currentError != nil {
			return f.currentError
		}
		return sqlError("0Z002", "RAISE without parameters cannot be used outside an exception handler", nil)
	}
	formatArguments := len(stmt.Parameters) - len(stmt.OptionKinds)
	values := make([]string, formatArguments)
	for i := range formatArguments {
		value, err := f.evaluate(ctx, stmt.Parameters[i])
		if err != nil {
			return err
		}
		values[i] = displayValue(value)
	}
	message := formatRaise(stmt.Message, values)
	code := raiseConditionCode(stmt.Condition)
	var detail, hint, column, constraint, datatype, table, schema string
	for i, kind := range stmt.OptionKinds {
		value, err := f.evaluate(ctx, stmt.Parameters[formatArguments+i])
		if err != nil {
			return err
		}
		text, err := textValue(value)
		if err != nil {
			return err
		}
		switch kind {
		case 0:
			if !validRaiseCode(text) {
				return sqlError("22023", "ERRCODE must be a valid five-character SQLSTATE", nil)
			}
			code = text
		case 1:
			message = text
		case 2:
			detail = text
		case 3:
			hint = text
		case 4:
			column = text
		case 5:
			constraint = text
		case 6:
			datatype = text
		case 7:
			table = text
		case 8:
			schema = text
		default:
			return ErrInvalidProgram
		}
	}
	if message == "" {
		message = stmt.Condition
	}
	reported := model.Error{Code: code, Message: message, Detail: detail, Hint: hint, Column: column, Constraint: constraint, Datatype: datatype, Table: table, Schema: schema}
	if stmt.Level >= 21 {
		return &reported
	}
	f.executor.Report(ctx, reported)
	return nil
}

func validRaiseCode(code string) bool {
	if len(code) != 5 || code == "00000" {
		return false
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' && (code[i] < 'A' || code[i] > 'Z') {
			return false
		}
	}
	return true
}

func matchesCondition(err error, conditions []string) bool {
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) {
		return false
	}
	for _, condition := range conditions {
		code := raiseConditionCode(condition)
		if condition == "others" || code == sqlErr.Code || (len(code) == 2 && strings.HasPrefix(sqlErr.Code, code)) {
			return true
		}
	}
	return false
}

func raiseConditionCode(condition string) string {
	if len(condition) == 5 && !strings.ContainsRune(condition, '_') {
		return condition
	}
	codes := map[string]string{
		"raise_exception": "P0001", "no_data_found": "P0002", "too_many_rows": "P0003", "assert_failure": "P0004",
		"unique_violation": "23505", "not_null_violation": "23502", "foreign_key_violation": "23503", "check_violation": "23514",
		"division_by_zero": "22012", "invalid_text_representation": "22P02", "cardinality_violation": "21000", "case_not_found": "20000",
		"data_exception": "22", "integrity_constraint_violation": "23", "syntax_error_or_access_rule_violation": "42",
	}
	if code := codes[condition]; code != "" {
		return code
	}
	return "P0001"
}

func textValue(value model.Value) (string, error) {
	if value.IsNull {
		return "", sqlError("22004", "query string argument of EXECUTE is null", nil)
	}
	if value.Type != model.TypeString {
		return "", sqlError("42804", "value is not text", nil)
	}
	return value.StrVal, nil
}

func displayValue(value model.Value) string {
	if value.IsNull {
		return "<NULL>"
	}
	switch value.Type {
	case model.TypeString:
		return value.StrVal
	case model.TypeInt64, model.TypeBoolean:
		return strconv.FormatInt(value.IntVal, 10)
	case model.TypeFloat64:
		return strconv.FormatFloat(value.FloatVal, 'g', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func formatRaise(format string, values []string) string {
	var result strings.Builder
	argument := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			result.WriteByte(format[i])
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			result.WriteByte('%')
			i++
			continue
		}
		if argument < len(values) {
			result.WriteString(values[argument])
			argument++
		}
	}
	return result.String()
}
