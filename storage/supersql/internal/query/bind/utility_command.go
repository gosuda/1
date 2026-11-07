package bind

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type Command interface{ boundCommand() }
type TransactionAction uint8

const (
	BeginTransaction TransactionAction = iota + 1
	CommitTransaction
	RollbackTransaction
	CreateSavepoint
	ReleaseSavepoint
	RollbackSavepoint
)

type TransactionCommand struct {
	Action    TransactionAction
	Savepoint string
	Chain     bool
	Isolation string
	ReadOnly *bool
	Deferrable *bool
}

func (TransactionCommand) boundCommand() {}

type SettingAction uint8

const (
	SetSetting SettingAction = iota + 1
	ResetSetting
	ResetAllSettings
	ShowSetting
	ShowAllSettings
	SetTransactionSettings
	SetSessionCharacteristics
	// SetFromCurrentSetting captures the current value of one setting at
	// execution and assigns it, implementing SET name FROM CURRENT.
	SetFromCurrentSetting
)

type SettingCommand struct {
	Action SettingAction
	Name   string
	Values []model.Value
	Local  bool
	Characteristics TransactionCommand
}

func (SettingCommand) boundCommand() {}
type TableLockCommand struct {
	Requests []txn.TableLockRequest
	Nowait   bool
}

func (TableLockCommand) boundCommand() {}


type InlineRoutineCommand struct {
	Statement *zpgparse.DoStmt
}
func (InlineRoutineCommand) boundCommand() {}
type ResolvedConstraintMode struct {
	Object     model.ObjectID
	Constraint model.ObjectID
	Schema     string
	Name       string
}

type ConstraintModeCommand struct {
	Names    []ResolvedConstraintMode
	Deferred bool
	All      bool
}
func (ConstraintModeCommand) boundCommand() {}

type PrepareCommand struct {
	Statement *zpgparse.PrepareStmt
}

func (PrepareCommand) boundCommand() {}

type ExecuteCommand struct {
	Statement *zpgparse.ExecuteStmt
}

func (ExecuteCommand) boundCommand() {}

type DeallocateCommand struct {
	Name string
	All  bool
}

func (DeallocateCommand) boundCommand() {}

type DeclareCursorCommand struct {
	Statement *zpgparse.DeclareCursorStmt
}

func (DeclareCursorCommand) boundCommand() {}

type FetchCursorCommand struct {
	Name      string
	Direction zpgparse.FetchDirection
	Count     int64
	Move      bool
}

func (FetchCursorCommand) boundCommand() {}

type CloseCursorCommand struct {
	Name string
	All  bool
}

func (CloseCursorCommand) boundCommand() {}

type DiscardCommand struct {
	Target zpgparse.DiscardMode
}

func (DiscardCommand) boundCommand() {}
func (s *BoundStatement) Command() Command {

	if setting, ok := s.command.(SettingCommand); ok {
		setting.Values = model.CloneValues(setting.Values)
		setting.Characteristics = cloneTransactionCommand(setting.Characteristics)
		return setting
	}
	if transaction, ok := s.command.(TransactionCommand); ok {
		return cloneTransactionCommand(transaction)
	}
	if inline, ok := s.command.(InlineRoutineCommand); ok {
		if inline.Statement != nil {
			inline.Statement = proto.Clone(inline.Statement).(*zpgparse.DoStmt)
		}
		return inline
	}
	if constraints, ok := s.command.(ConstraintModeCommand); ok {
		constraints.Names = slices.Clone(constraints.Names)
		return constraints
	}
	if tableLock, ok := s.command.(TableLockCommand); ok {
		tableLock.Requests = slices.Clone(tableLock.Requests)
		return tableLock
	}
	if prepare, ok := s.command.(PrepareCommand); ok {
		if prepare.Statement != nil {
			prepare.Statement = proto.Clone(prepare.Statement).(*zpgparse.PrepareStmt)
		}
		return prepare
	}
	if execute, ok := s.command.(ExecuteCommand); ok {
		if execute.Statement != nil {
			execute.Statement = proto.Clone(execute.Statement).(*zpgparse.ExecuteStmt)
		}
		return execute
	}
	if declare, ok := s.command.(DeclareCursorCommand); ok {
		if declare.Statement != nil {
			declare.Statement = proto.Clone(declare.Statement).(*zpgparse.DeclareCursorStmt)
		}
		return declare
	}
	if explain, ok := s.command.(ExplainCommand); ok {
		if explain.Query != nil {
			explain.Query = proto.Clone(explain.Query).(*zpgparse.Node)
		}
		return explain
	}
	return s.command
}

func cloneTransactionCommand(command TransactionCommand) TransactionCommand {
	if command.ReadOnly != nil { value := *command.ReadOnly; command.ReadOnly = &value }
	if command.Deferrable != nil { value := *command.Deferrable; command.Deferrable = &value }
	return command
}
func (b *binder) command(node *zpgparse.Node) (Command, []ResultColumn, error) {
	if n := node.GetExplainStmt(); n != nil {
		return bindExplain(n, ResultColumn{Name: "QUERY PLAN", Layout: b.primitive(25)})
	}
	if n := node.GetTransactionStmt(); n != nil {
		start := n.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_BEGIN || n.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_START
		end := n.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT || n.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK
		if n.Gid != "" || start && (n.Chain || n.SavepointName != "") || !start && len(n.Options) != 0 || end && n.SavepointName != "" || !start && !end && (n.Chain || n.SavepointName == "") {
			return nil, nil, bindError("42601", "invalid transaction command fields", int(n.Location)+1)
		}
		characteristics, err := bindTransactionCharacteristics(n.Options)
		if err != nil {
			return nil, nil, err
		}
		command := characteristics
		command.Savepoint, command.Chain = n.SavepointName, n.Chain
		switch n.Kind {
		case zpgparse.TransactionStmtKind_TRANS_STMT_BEGIN, zpgparse.TransactionStmtKind_TRANS_STMT_START:
			command.Action = BeginTransaction
		case zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT:
			command.Action = CommitTransaction
		case zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK:
			command.Action = RollbackTransaction
		case zpgparse.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
			command.Action = CreateSavepoint
		case zpgparse.TransactionStmtKind_TRANS_STMT_RELEASE:
			command.Action = ReleaseSavepoint
		case zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
			command.Action = RollbackSavepoint
		default:
			return nil, nil, missing("transaction command", int(n.Location)+1)
		}
		return command, nil, nil
	}
	if n := node.GetVariableShowStmt(); n != nil {
		command := SettingCommand{Action: ShowSetting, Name: n.Name}
		columns := []ResultColumn{{Name: n.Name, Layout: b.primitive(25)}}
		if n.Name == "all" {
			command.Action = ShowAllSettings
			columns = []ResultColumn{{Name: "name", Layout: b.primitive(25)}, {Name: "setting", Layout: b.primitive(25)}, {Name: "description", Layout: b.primitive(25)}}
		} else if err := ValidateSetting(n.Name, 0); err != nil {
			var sqlErr *model.Error
			if errors.As(err, &sqlErr) && sqlErr.Code == "42602" {
				return nil, nil, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: n.Name}
			}
			return nil, nil, err
		}
		return command, columns, nil
	}
	if n := node.GetVariableSetStmt(); n != nil {
		pos := int(n.Location) + 1
		command := SettingCommand{Name: n.Name, Local: n.IsLocal}
		if n.Kind == zpgparse.VariableSetKind_VAR_RESET_ALL {
			command.Action = ResetAllSettings
			return command, nil, nil
		}
		if n.Kind == zpgparse.VariableSetKind_VAR_SET_MULTI {
			if len(n.Args) == 0 {
				return nil, nil, bindError("42601", "transaction characteristics require values", pos)
			}
			var err error
			command.Characteristics, err = bindTransactionCharacteristics(n.Args)
			if err != nil {
				return nil, nil, err
			}
			switch n.Name {
			case "TRANSACTION":
				command.Action = SetTransactionSettings
			case "SESSION CHARACTERISTICS":
				command.Action = SetSessionCharacteristics
			default:
				return nil, nil, bindError("42601", "invalid transaction setting command", pos)
			}
			return command, nil, nil
		}
		if err := ValidateSetting(n.Name, pos); err != nil {
			return nil, nil, err
		}
		switch n.Kind {
		case zpgparse.VariableSetKind_VAR_SET_VALUE:
			command.Action = SetSetting
		case zpgparse.VariableSetKind_VAR_SET_DEFAULT, zpgparse.VariableSetKind_VAR_RESET:
			command.Action = ResetSetting
		case zpgparse.VariableSetKind_VAR_SET_CURRENT:
			// SET name FROM CURRENT is a session command whose value is not a
			// syntactic literal; the serialized command execution captures it.
			command.Action = SetFromCurrentSetting
		default:
			return nil, nil, missing("setting command form", pos)
		}
		if command.Action == SetFromCurrentSetting {
			return command, nil, nil
		}
		for _, arg := range n.Args {
			constant := arg.GetAConst()
			if constant == nil {
				cast := arg.GetTypeCast()
				if cast == nil || cast.Arg.GetAConst().GetSval() == nil {
					return nil, nil, bindError("42601", "setting requires a literal value", pos)
				}
				target, err := b.typeName(cast.TypeName)
				if err != nil {
					return nil, nil, err
				}
				if target.Type.ID != 1186 {
					return nil, nil, bindError("42601", "setting requires an interval literal", pos)
				}
				function, err := builtin.Resolve(1160, b.options.Builtin)
				if err != nil {
					return nil, nil, err
				}
				value, err := function.Call(b.ctx, []model.Value{model.StringVal(cast.Arg.GetAConst().GetSval().Sval), model.Int64Val(0), model.Int64Val(int64(target.Type.Modifier))}, builtin.CallBudget{MemoryBytes: b.options.Limits.MaxBytes})
				if err != nil {
					return nil, nil, err
				}
				command.Values = append(command.Values, value)
				continue
			}
			switch {
			case constant.GetSval() != nil:
				command.Values = append(command.Values, model.StringVal(constant.GetSval().Sval))
			case constant.GetIval() != nil:
				command.Values = append(command.Values, model.Int64Val(int64(constant.GetIval().Ival)))
			case constant.GetFval() != nil:
				command.Values = append(command.Values, model.StringVal(constant.GetFval().Fval))
			case constant.GetBoolval() != nil:
				command.Values = append(command.Values, model.BoolVal(constant.GetBoolval().Boolval))
			default:
				return nil, nil, missing("setting literal kind", pos)
			}
		}
		return command, nil, nil
	}
	if n := node.GetLockStmt(); n != nil {
		return b.bindTableLock(n)
	}
	if n := node.GetDoStmt(); n != nil {
		return InlineRoutineCommand{Statement: proto.Clone(n).(*zpgparse.DoStmt)}, nil, nil
	}
	if n := node.GetConstraintsSetStmt(); n != nil {
		command := ConstraintModeCommand{Deferred: n.Deferred, All: len(n.Constraints) == 0}
		seen := make(map[[2]model.ObjectID]struct{}, len(n.Constraints))
		for _, item := range n.Constraints {
			relation := item.GetRangeVar()
			if relation == nil || relation.Relname == "" || relation.Catalogname != "" {
				return nil, nil, bindError("42601", "invalid constraint name", 0)
			}
			if b.catalog == nil {
				command.Names = append(command.Names, ResolvedConstraintMode{Schema: relation.Schemaname, Name: relation.Relname})
				continue
			}
			resolved, err := b.resolveConstraintMode(relation.Schemaname, relation.Relname, n.Deferred)
			if err != nil {
				return nil, nil, err
			}
			for _, constraint := range resolved {
				key := [2]model.ObjectID{constraint.Object, constraint.Constraint}
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				command.Names = append(command.Names, constraint)
			}
		}
		return command, nil, nil
	}
	if n := node.GetPrepareStmt(); n != nil {
		if n.Name == "" || !preparableStatement(n.Query) {
			return nil, nil, bindError("42601", "invalid PREPARE command", 0)
		}
		return PrepareCommand{Statement: proto.Clone(n).(*zpgparse.PrepareStmt)}, nil, nil
	}
	if n := node.GetExecuteStmt(); n != nil {
		if n.Name == "" {
			return nil, nil, bindError("42601", "invalid EXECUTE command", 0)
		}
		return ExecuteCommand{Statement: proto.Clone(n).(*zpgparse.ExecuteStmt)}, nil, nil
	}
	if n := node.GetDeallocateStmt(); n != nil {
		all := n.Isall || n.Name == ""
		if all == (n.Name != "") {
			return nil, nil, bindError("42601", "invalid DEALLOCATE command", int(n.Location)+1)
		}
		return DeallocateCommand{Name: n.Name, All: all}, nil, nil
	}
	if n := node.GetDeclareCursorStmt(); n != nil {
		if n.Portalname == "" || n.Query == nil || n.Options&2 != 0 && n.Options&4 != 0 {
			return nil, nil, bindError("42601", "invalid DECLARE CURSOR command", 0)
		}
		return DeclareCursorCommand{Statement: proto.Clone(n).(*zpgparse.DeclareCursorStmt)}, nil, nil
	}
	if n := node.GetFetchStmt(); n != nil {
		if n.Portalname == "" || n.Direction < zpgparse.FetchDirection_FETCH_FORWARD || n.Direction > zpgparse.FetchDirection_FETCH_RELATIVE {
			return nil, nil, bindError("42601", "invalid FETCH command", 0)
		}
		return FetchCursorCommand{Name: n.Portalname, Direction: n.Direction, Count: n.HowMany, Move: n.Ismove}, nil, nil
	}
	if n := node.GetClosePortalStmt(); n != nil {
		return CloseCursorCommand{Name: n.Portalname, All: n.Portalname == ""}, nil, nil
	}
	if n := node.GetDiscardStmt(); n != nil {
		if n.Target < zpgparse.DiscardMode_DISCARD_ALL || n.Target > zpgparse.DiscardMode_DISCARD_TEMP {
			return nil, nil, bindError("42601", "invalid DISCARD command", 0)
		}
		return DiscardCommand{Target: n.Target}, nil, nil
	}
	return nil, nil, nil
}

func preparableStatement(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	return node.GetSelectStmt() != nil || node.GetInsertStmt() != nil || node.GetUpdateStmt() != nil || node.GetDeleteStmt() != nil || node.GetMergeStmt() != nil
}
func (b *binder) commandStatement(command Command, columns []ResultColumn) *BoundStatement {
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	if setting, ok := command.(SettingCommand); ok && (setting.Action == ShowSetting || setting.Action == ShowAllSettings) {
		effects = ir.Effects{Volatility: ir.Stable}
	}
	result := &BoundStatement{command: command, columns: slices.Clone(columns), dependencies: b.dependencies, provisional: b.provisional, effects: effects, warnings: b.warnings}
	if b.catalog != nil {
		result.timestamp = b.catalog.Timestamp()
	}
	return result
}

// BindCommand binds utility metadata that does not require catalog identity.
// SET CONSTRAINTS names remain syntax-only for the outside-transaction warning;
// active transactions bind them through Bind with a catalog snapshot.
func BindCommand(ctx context.Context, parsed *zpgparse.RawStmt, options Options) (*BoundStatement, error) {
	if parsed == nil || parsed.Stmt == nil {
		return nil, bindError("42601", "missing command", 0)
	}
	if err := ValidateStatementFeatures(ctx, parsed.Stmt); err != nil {
		return nil, err
	}
	b := binder{ctx: ctx, options: options}
	remaining, bytes := options.Limits.MaxNodes, options.Limits.MaxBytes
	if err := b.checkAST(parsed.Stmt.ProtoReflect(), 0, &remaining, &bytes); err != nil {
		return nil, err
	}
	command, columns, err := b.command(parsed.Stmt)
	if err != nil {
		return nil, err
	}
	if command == nil {
		return nil, bindError("42601", "expected a session command", 0)
	}
	return b.commandStatement(command, columns), nil
}

func (b *binder) resolveConstraintMode(schemaName, constraintName string, deferred bool) ([]ResolvedConstraintMode, error) {
	if b.catalog == nil {
		return nil, bindError("55000", "constraint catalog is unavailable", 0)
	}
	path := b.options.SearchPath
	if schemaName != "" {
		path = []string{schemaName}
	}
	var visited uint32
	for _, name := range path {
		schema, err := b.catalog.Resolve(b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
		if err != nil {
			if schemaName == "" && isMissingObject(err) {
				continue
			}
			return nil, err
		}
		if schema.Descriptor == nil || schema.Descriptor.Kind() != model.ObjectSchema || schema.Descriptor.State() != model.StatePublic {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		b.dependency(schema.Descriptor, 0)
		resolved := make([]ResolvedConstraintMode, 0, 1)
		err = b.catalog.ScanNamespace(b.ctx, schema.Descriptor.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if visited == b.options.Limits.MaxOperands {
				return bindError("54000", "constraint resolution limit exceeded", 0)
			}
			visited++
			table := published.Descriptor
			if table == nil || table.State() != model.StatePublic {
				return nil
			}
			spec, err := table.SpecWithin(b.ctx, b.descriptorLimits())
			if err != nil || spec.Table == nil {
				return err
			}
			matched := false
			appendMatch := func(id model.ObjectID, deferrable bool) error {
				if id == 0 {
					return model.ErrInvalidDescriptor
				}
				if deferred && !deferrable {
					return bindError("55000", "constraint is not deferrable", 0)
				}
				resolved = append(resolved, ResolvedConstraintMode{Object: table.ID(), Constraint: id, Schema: schema.Descriptor.Name().Name, Name: constraintName})
				matched = true
				return nil
			}
			for _, constraint := range spec.Table.Constraints {
				if !constraint.Mutation && constraint.Name == constraintName {
					if err := appendMatch(constraint.ID, constraint.Deferrable); err != nil {
						return err
					}
				}
			}
			for _, trigger := range spec.Table.Triggers {
				if trigger.Constraint && trigger.Name == constraintName {
					object := trigger.ConstraintTable
					if object == 0 {
						object = table.ID()
					}
					if object != table.ID() {
						continue
					}
					if err := appendMatch(trigger.ConstraintID, trigger.Deferrable); err != nil {
						return err
					}
				}
			}
			if matched {
				b.dependency(table, 0)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(resolved) != 0 {
			slices.SortFunc(resolved, func(left, right ResolvedConstraintMode) int {
				if left.Object < right.Object {
					return -1
				}
				if left.Object > right.Object {
					return 1
				}
				if left.Constraint < right.Constraint {
					return -1
				}
				if left.Constraint > right.Constraint {
					return 1
				}
				return 0
			})
			return slices.CompactFunc(resolved, func(left, right ResolvedConstraintMode) bool {
				return left.Object == right.Object && left.Constraint == right.Constraint
			}), nil
		}
		if schemaName != "" {
			break
		}
	}
	return nil, bindError("42704", "constraint does not exist", 0)
}

func bindTransactionCharacteristics(nodes []*zpgparse.Node) (TransactionCommand, error) {
	var command TransactionCommand
	for _, node := range nodes {
		option := node.GetDefElem()
		if option == nil || option.Arg.GetAConst() == nil || option.Arg.GetAConst().Isnull || option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC {
			return command, bindError("42601", "invalid transaction characteristic", 0)
		}
		value := option.Arg.GetAConst()
		switch option.Defname {
		case "transaction_isolation":
			if value.GetSval() == nil {
				return command, bindError("42601", "invalid isolation value", 0)
			}
			if err := ValidateIsolation(value.GetSval().Sval, int(option.Location)+1); err != nil {
				return command, err
			}
			command.Isolation = value.GetSval().Sval
		case "transaction_read_only", "transaction_deferrable":
			if value.GetIval() == nil || value.GetIval().Ival < 0 || value.GetIval().Ival > 1 {
				return command, bindError("42601", "invalid transaction mode", 0)
			}
			flag := value.GetIval().Ival == 1
			if option.Defname == "transaction_read_only" {
				command.ReadOnly = &flag
			} else {
				command.Deferrable = &flag
			}
		default:
			return command, bindError("42601", "unknown transaction characteristic", 0)
		}
	}
	return command, nil
}
