package session

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (transactions *Transactions) commandBindOptions() bind.Options {
	bytes := uint64(transactions.settings.limits.MaxValueBytes)*uint64(settingCount) + 65536
	return bind.Options{Collation: 950, DefaultCollation: 950, Builtin: transactions.settings.BuiltinOptions(950, 950), Limits: ir.Limits{MaxNodes: 4096, MaxOperands: 4096, MaxSlots: 4096, MaxDepth: 64, MaxBytes: bytes}}
}

// commandCursor owns a bounded fixed-inventory result, not a relational plan or snapshot.
// Rows lends its batches only until the consumer returns.
type commandCursor struct {
	columns []bind.ResultColumn
	values  [][]model.Value
	next    int
}

func (cursor *commandCursor) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if out == nil || out.MaxRows < 0 {
		return columnar.ErrInvalidBatch
	}
	if cursor.next == len(cursor.values) {
		return io.EOF
	}
	values := cursor.values[cursor.next]
	out.Columns = append(out.Columns[:0], make([]columnar.Vector, len(values))...)
	for i, value := range values {
		out.Columns[i] = columnar.Vector{Type: cursor.columns[i].Layout.Type, Encoding: columnar.Constant, Length: 1, Data: []byte(value.StrVal), Ranges: []columnar.Range{{Length: uint32(len(value.StrVal))}}}
		if value.IsNull {
			out.Columns[i].Nulls = []byte{1}
		}
	}
	out.Length = 1
	cursor.next++
	return nil
}

func (transactions *Transactions) runCommand(ctx context.Context, prepared *Prepared, consume func(context.Context, *Prepared, *Rows) error) (result ExecutionResult, err error) {
	defer func() { result.Warnings = transactions.settings.visibleWarnings(result.Warnings) }()
	command := prepared.bound.Command()
	recovery := false
	if transaction, ok := command.(bind.TransactionCommand); ok {
		recovery = transaction.Action == bind.CommitTransaction || transaction.Action == bind.RollbackTransaction || transaction.Action == bind.RollbackSavepoint
	}
	if transactions.active && !recovery {
		if err := transactions.ordinary(); err != nil {
			return result, err
		}
	}
	defer func() {
		if err != nil && !recovery {
			transactions.markFailed(err)
		}
	}()
	cursor := &commandCursor{columns: prepared.Columns()}
	before := *transactions.settings
	completed := false
	defer func() {
		if !completed && err == nil {
			if _, setting := command.(bind.SettingCommand); setting {
				*transactions.settings = before
			}
			transactions.markFailed(errSessionStatementPanic)
		}
	}()
	switch command := command.(type) {
	case bind.SettingCommand:
		result, cursor.values, err = transactions.executeSetting(ctx, command)
	case bind.TransactionCommand:
		result, err = transactions.executeTransactionCommand(ctx, command)
	case bind.ConstraintModeCommand:
		result, err = transactions.executeConstraintMode(ctx, command)
	case bind.TableLockCommand:
		result, err = transactions.executeTableLock(ctx, command)
	default:
		err = errPreparedInvocation
	}
	if err != nil {
		return result, err
	}
	rows := &Rows{ctx: ctx, command: cursor}
	defer func() {
		rows.mu.Lock()
		rows.closed = true
		rows.command, rows.ctx = nil, nil
		if rows.terminal != nil && rows.terminal != io.EOF {
			err = errors.Join(err, rows.terminal)
		}
		rows.mu.Unlock()
		if err != nil {
			if _, setting := command.(bind.SettingCommand); setting {
				*transactions.settings = before
			}
		}
	}()
	if consume != nil {
		err = consume(ctx, prepared, rows)
	}
	if err == nil {
		err = sessionTransactionError(context.Cause(ctx))
	}
	result.Warnings = append(prepared.Warnings(), result.Warnings...)
	completed = true
	return result, err
}

func (transactions *Transactions) executeTableLock(ctx context.Context, command bind.TableLockCommand) (ExecutionResult, error) {
	result := ExecutionResult{Tag: "LOCK TABLE"}
	if !transactions.active || transactions.transaction == nil {
		return result, settingError("25P01", "LOCK TABLE can only be used in transaction blocks")
	}
	if err := transactions.ordinary(); err != nil {
		return result, err
	}
	resources := transactions.settings.Resources()
	ctx = txn.WithLockTimeout(ctx, resources.LockTimeout)
	err := transactions.routineAtomicStatement(ctx, func(statement context.Context) error {
		return transactions.transaction.LockTables(statement, command.Requests, command.Nowait)
	})
	return result, err
}

func (transactions *Transactions) executeSetting(ctx context.Context, command bind.SettingCommand) (result ExecutionResult, rows [][]model.Value, err error) {
	stagedSettings := *transactions.settings
	settings := &stagedSettings
	scope := SessionScope
	if command.Local {
		scope = LocalScope
	}
	result.Tag = "SET"
	var changed SettingResult
	switch command.Action {
	case bind.SetSetting:
		var value string
		value, err = settingCommandText(command)
		if err == nil {
			changed, err = settings.Set(ctx, command.Name, value, scope)
		}
	case bind.SetFromCurrentSetting:
		// PostgreSQL assigns the value visible to this session at execution
		// time, so capture it under the serialized command boundary after the
		// transaction state is established. Character values are used as
		// stored; numeric and memory units keep their canonical text.
		var current model.Value
		current, err = settings.CurrentSetting(command.Name, false)
		if err != nil {
			break
		}
		if current.IsNull || current.Type != model.TypeString {
			err = settingError("22023", "session setting has no textual representation")
			break
		}
		changed, err = settings.Set(ctx, command.Name, current.StrVal, scope)
	case bind.ResetSetting:
		result.Tag = "RESET"
		changed, err = settings.Reset(ctx, command.Name, scope)
	case bind.ResetAllSettings:
		result.Tag = "RESET"
		err = settings.ResetAll(ctx)
	case bind.ShowSetting:
		result.Tag = "SHOW"
		var value model.Value
		value, err = settings.Show(command.Name)
		if err == nil {
			rows = [][]model.Value{{value}}
		}
	case bind.ShowAllSettings:
		result.Tag = "SHOW"
		for _, setting := range settings.ShowAll() {
			rows = append(rows, []model.Value{model.StringVal(setting.Name), setting.Value, model.StringVal(settingDescription(setting.Name))})
		}
	case bind.SetTransactionSettings, bind.SetSessionCharacteristics:
		staged := *settings
		prefix := ""
		if command.Action == bind.SetSessionCharacteristics {
			prefix = "default_"
		}
		characteristics := command.Characteristics
		assign := func(name, value string) {
			if err != nil {
				return
			}
			_, err = staged.Set(ctx, prefix+name, value, SessionScope)
		}
		if characteristics.Isolation != "" {
			assign("transaction_isolation", characteristics.Isolation)
		}
		if characteristics.ReadOnly != nil {
			assign("transaction_read_only", strconv.FormatBool(*characteristics.ReadOnly))
		}
		if characteristics.Deferrable != nil {
			assign("transaction_deferrable", strconv.FormatBool(*characteristics.Deferrable))
		}
		if err == nil {
			if command.Action == bind.SetTransactionSettings && !transactions.active {
				result.Warnings = append(result.Warnings, model.Error{Code: "25P01", Message: "SET TRANSACTION can only be used in transaction blocks"})
			} else {
				*settings = staged
			}
		}
	default:
		err = errPreparedInvocation
	}
	if err == nil {
		err = transactions.syncSettingPolicy(ctx, settings)
		if err == nil {
			*transactions.settings = *settings
		}
	}
	if changed.Warning != nil {
		result.Warnings = append(result.Warnings, *changed.Warning)
	}
	return result, rows, err
}

func settingCommandText(command bind.SettingCommand) (string, error) {
	if len(command.Values) == 0 {
		return "", settingError("42601", "setting requires a value")
	}
	list := strings.EqualFold(command.Name, "search_path") || strings.EqualFold(command.Name, "DateStyle")
	if !list && len(command.Values) != 1 {
		return "", settingError("42601", "setting requires a single value")
	}
	parts := make([]string, len(command.Values))
	for i, value := range command.Values {
		if value.IsNull {
			return "", settingError("22023", "setting cannot be null")
		}
		switch value.Type {
		case model.TypeString:
			parts[i] = value.StrVal
			if strings.EqualFold(command.Name, "search_path") {
				parts[i] = `"` + strings.ReplaceAll(value.StrVal, `"`, `""`) + `"`
			}
		case model.TypeInt64:
			parts[i] = strconv.FormatInt(value.IntVal, 10)
		case model.TypeBoolean:
			parts[i] = strconv.FormatBool(value.IntVal != 0)
		case model.TypeInterval:
			if !strings.EqualFold(command.Name, "TimeZone") || value.IntervalVal.Months != 0 || value.IntervalVal.Days != 0 {
				return "", settingError("22023", "time zone interval must not contain months or days")
			}
			parts[i] = strconv.FormatFloat(float64(value.IntervalVal.Microseconds)/3600000000, 'g', -1, 64)
		default:
			return "", settingError("22023", "invalid setting value type")
		}
	}
	return strings.Join(parts, ", "), nil
}

func settingDescription(name string) string {
	switch strings.ToLower(name) {
	case "search_path":
		return "Sets the schema search order for names that are not schema-qualified."
	case "timezone":
		return "Sets the time zone for displaying and interpreting time stamps."
	case "work_mem":
		return "Sets the maximum memory to be used for query workspaces."
	case "temp_file_limit":
		return "Limits the total size of temporary files used by each query."
	case "datestyle":
		return "Sets the display format for date and time values."
	case "intervalstyle":
		return "Sets the display format for interval values."
	case "extra_float_digits":
		return "Sets the number of digits displayed for floating-point values."
	case "bytea_output":
		return "Sets the output format for bytea."
	case "standard_conforming_strings":
		return "Causes ordinary strings to treat backslashes literally."
	case "escape_string_warning":
		return "Warns about backslash escapes in ordinary string literals."
	case "backslash_quote":
		return "Sets whether a backslash may escape a quote in string literals."
	case "client_encoding":
		return "Sets the client's character set encoding."
	case "client_min_messages":
		return "Sets the message levels that are sent to the client."
	case "application_name":
		return "Sets the application name to be reported in statistics and logs."
	case "check_function_bodies":
		return "Checks routine bodies during CREATE FUNCTION and CREATE PROCEDURE."
	case "default_text_search_config":
		return "Sets the default text search configuration."
	case "lc_monetary":
		return "Sets the locale to use for formatting monetary amounts."
	case "lc_numeric":
		return "Sets the locale to use for formatting numbers."
	case "lc_time":
		return "Sets the locale to use for formatting dates and times."
	case "default_transaction_isolation":
		return "Sets the transaction isolation level of each new transaction."
	case "transaction_isolation":
		return "Sets the current transaction's isolation level."
	case "default_transaction_read_only":
		return "Sets the default read-only status of new transactions."
	case "transaction_read_only":
		return "Sets the current transaction's read-only status."
	case "default_transaction_deferrable":
		return "Sets the default deferrable status of new transactions."
	case "transaction_deferrable":
		return "Sets the current transaction's deferrable status."
	case "statement_timeout":
		return "Sets the maximum allowed duration of any statement."
	case "lock_timeout":
		return "Sets the maximum allowed duration of any wait for a lock."
	case "idle_in_transaction_session_timeout":
		return "Sets the maximum allowed idle time between queries in a transaction."
	case "transaction_timeout":
		return "Sets the maximum allowed duration of any transaction."
	case "maintenance_work_mem":
		return "Sets the maximum memory to be used for maintenance operations."
	case "server_encoding":
		return "Shows the server's character set encoding."
	case "server_version":
		return "Shows the supported SQL dialect version."
	case "server_version_num":
		return "Shows the supported SQL dialect version as an integer."
	case "integer_datetimes":
		return "Shows whether datetimes are integer based."
	case "max_identifier_length":
		return "Shows the maximum identifier length."
	default:
		return ""
	}
}

func (transactions *Transactions) executeTransactionCommand(ctx context.Context, command bind.TransactionCommand) (result ExecutionResult, err error) {
	switch command.Action {
	case bind.BeginTransaction:
		result.Tag = "BEGIN"
		if transactions.active {
			result.Warnings = []model.Error{{Code: "25001", Message: "there is already a transaction in progress"}}
			return result, nil
		}
		err = transactions.begin(ctx, TransactionStart{Characteristics: BeginOptions{Isolation: command.Isolation, ReadOnly: command.ReadOnly, Deferrable: command.Deferrable}})
	case bind.CommitTransaction, bind.RollbackTransaction:
		result.Tag = "ROLLBACK"
		if command.Action == bind.CommitTransaction {
			result.Tag = "COMMIT"
		}
		if !transactions.active {
			if command.Chain {
				return result, settingError("25P01", "AND CHAIN requires an active transaction")
			}
			result.Warnings = []model.Error{{Code: "25P01", Message: "there is no transaction in progress"}}
			return result, nil
		}
		if command.Action == bind.CommitTransaction {
			result.Commit, err = transactions.commit(ctx, command.Chain)
			if !result.Commit.Committed {
				result.Tag = "ROLLBACK"
			}
		} else {
			err = transactions.rollback(ctx, command.Chain)
		}
	case bind.CreateSavepoint:
		result.Tag, err = "SAVEPOINT", transactions.savepoint(ctx, command.Savepoint)
	case bind.ReleaseSavepoint:
		result.Tag, err = "RELEASE", transactions.release(ctx, command.Savepoint)
	case bind.RollbackSavepoint:
		result.Tag, err = "ROLLBACK", transactions.rollbackTo(ctx, command.Savepoint)
	default:
		err = errPreparedInvocation
	}
	return result, err
}

func (settings *Settings) visibleWarnings(warnings []model.Error) []model.Error {
	switch settings.current[clientMinMessages].text {
	case "error":
		return nil
	default:
		return warnings
	}
}
