package supersql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	qcopy "github.com/gosuda/supersql-alpha/internal/query/copy"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const nativeRLSMetaFile = "rls_meta.json"

type rlsPolicy struct {
	Name          string   `json:"name"`
	Permissive    bool     `json:"permissive"`
	Command       string   `json:"command"` // ALL, SELECT, INSERT, UPDATE, DELETE
	Roles         []string `json:"roles"`   // empty or ["public"] means all roles
	UsingExpr     string   `json:"using_expr,omitempty"`
	WithCheckExpr string   `json:"with_check_expr,omitempty"`
}

type rlsTableMeta struct {
	Schema     string               `json:"schema"`
	Name       string               `json:"name"`
	Owner      string               `json:"owner"`
	RLSEnabled bool                 `json:"rls_enabled"`
	ForceRLS   bool                 `json:"force_rls"`
	Policies   map[string]rlsPolicy `json:"policies,omitempty"`
}

type rlsRole struct {
	Name      string   `json:"name"`
	Superuser bool     `json:"superuser"`
	BypassRLS bool     `json:"bypass_rls"`
	MemberOf  []string `json:"member_of,omitempty"`
}

type rlsPersistedState struct {
	Roles  map[string]rlsRole       `json:"roles,omitempty"`
	Tables map[string]*rlsTableMeta `json:"tables,omitempty"`
}

func (s rlsPersistedState) hasAnyRolesOrTables() bool {
	return len(s.Roles) > 0 || len(s.Tables) > 0
}

func (s rlsPersistedState) hasAnyPoliciesOrRLS() bool {
	for _, meta := range s.Tables {
		if meta != nil && (meta.RLSEnabled || meta.ForceRLS || len(meta.Policies) > 0) {
			return true
		}
	}
	return false
}

func cloneRLSState(src rlsPersistedState) rlsPersistedState {
	dst := rlsPersistedState{
		Roles:  make(map[string]rlsRole, len(src.Roles)),
		Tables: make(map[string]*rlsTableMeta, len(src.Tables)),
	}
	for k, role := range src.Roles {
		role.MemberOf = slices.Clone(role.MemberOf)
		dst.Roles[k] = role
	}
	for k, tbl := range src.Tables {
		if tbl == nil {
			continue
		}
		cloned := *tbl
		cloned.Policies = make(map[string]rlsPolicy, len(tbl.Policies))
		for pk, pol := range tbl.Policies {
			pol.Roles = slices.Clone(pol.Roles)
			cloned.Policies[pk] = pol
		}
		dst.Tables[k] = &cloned
	}
	return dst
}

type rlsManager struct {
	mu         sync.RWMutex
	path       string
	legacyPath string
	state      rlsPersistedState
	pendingTx  atomic.Bool
}

func newRLSManager(directory, spillDir string) (*rlsManager, error) {
	mgr := &rlsManager{
		path:       filepath.Join(directory, nativeRLSMetaFile),
		legacyPath: filepath.Join(spillDir, "rls.json"),
		state: rlsPersistedState{
			Roles:  make(map[string]rlsRole),
			Tables: make(map[string]*rlsTableMeta),
		},
	}
	data, err := os.ReadFile(mgr.path)
	if errors.Is(err, os.ErrNotExist) && mgr.legacyPath != "" {
		data, err = os.ReadFile(mgr.legacyPath)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return mgr, nil
		}
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &mgr.state); err != nil {
			return nil, err
		}
		if mgr.state.Roles == nil {
			mgr.state.Roles = make(map[string]rlsRole)
		}
		if mgr.state.Tables == nil {
			mgr.state.Tables = make(map[string]*rlsTableMeta)
		}
	}
	return mgr, nil
}

func writeAtomicSynced(path string, data []byte) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close(), os.Remove(tmp))
	}
	if err := file.Sync(); err != nil {
		return errors.Join(err, file.Close(), os.Remove(tmp))
	}
	if err := file.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	if err := os.Rename(tmp, path); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := dirHandle.Sync()
	return errors.Join(syncErr, dirHandle.Close())
}

func (m *rlsManager) saveLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomicSynced(m.path, data); err != nil {
		return err
	}
	if m.legacyPath != "" {
		_ = writeAtomicSynced(m.legacyPath, data)
	}
	return nil
}

// loadRLSMetadata synchronizes the in-memory and JSON RLS metadata with the
// durable MVCC identity store during Open, and migrates pre-existing JSON
// metadata into the MVCC store if none was recorded there yet.
func (database *Catalog) loadRLSMetadata(ctx context.Context) error {
	if database == nil || database.rls == nil || database.identities == nil || database.manager == nil {
		return nil
	}
	var reader storage.SnapshotReader
	var err error
	if database.readOnly {
		state, stateErr := database.backend.State(ctx)
		if stateErr != nil {
			return stateErr
		}
		if state.ClosedTS == 0 {
			return nil
		}
		reader, err = database.manager.HistoricalSnapshot(ctx, state.ClosedTS)
	} else {
		reader, err = database.manager.ClosedReadView(ctx)
	}
	if err != nil {
		return err
	}
	data, present, readErr := database.identities.RLSMetadata(ctx, reader, 1<<20)
	closeErr := reader.Close()
	if closeErr != nil {
		return closeErr
	}
	if readErr != nil {
		return readErr
	}
	if present && len(data) > 0 {
		var loaded rlsPersistedState
		if err := json.Unmarshal(data, &loaded); err != nil {
			return err
		}
		if loaded.Roles == nil {
			loaded.Roles = make(map[string]rlsRole)
		}
		if loaded.Tables == nil {
			loaded.Tables = make(map[string]*rlsTableMeta)
		}
		database.rls.mu.Lock()
		database.rls.state = loaded
		var saveErr error
		if !database.readOnly {
			saveErr = database.rls.saveLocked()
		}
		database.rls.mu.Unlock()
		return saveErr
	}
	if database.readOnly {
		return nil
	}
	database.rls.mu.RLock()
	hasInitial := database.rls.state.hasAnyRolesOrTables()
	initialClone := cloneRLSState(database.rls.state)
	database.rls.mu.RUnlock()
	if hasInitial {
		return database.saveRLSState(ctx, nil, initialClone)
	}
	return nil
}

func (database *Catalog) syncCommittedRLSState(ctx context.Context) error {
	if database == nil || database.rls == nil || database.identities == nil || database.manager == nil || database.readOnly {
		return nil
	}
	view, err := database.manager.ClosedReadView(ctx)
	if err != nil {
		return err
	}
	data, present, readErr := database.identities.RLSMetadata(ctx, view, 1<<20)
	closeErr := view.Close()
	if closeErr != nil {
		return closeErr
	}
	if readErr != nil {
		return readErr
	}
	var loaded rlsPersistedState
	if present && len(data) > 0 {
		if err := json.Unmarshal(data, &loaded); err != nil {
			return err
		}
	}
	if loaded.Roles == nil {
		loaded.Roles = make(map[string]rlsRole)
	}
	if loaded.Tables == nil {
		loaded.Tables = make(map[string]*rlsTableMeta)
	}
	database.rls.mu.Lock()
	database.rls.state = loaded
	saveErr := database.rls.saveLocked()
	database.rls.mu.Unlock()
	return saveErr
}

func (database *Catalog) effectiveRLSState(ctx context.Context, scope *sessionScope) (rlsPersistedState, error) {
	if database == nil || database.rls == nil {
		return rlsPersistedState{
			Roles:  make(map[string]rlsRole),
			Tables: make(map[string]*rlsTableMeta),
		}, nil
	}
	if scope != nil && scope.txns != nil && scope.txns.ActiveIn(ctx) && scope.rlsTouched.Load() && database.identities != nil {
		view, err := scope.txns.ReadView(ctx, []txn.ReadDependency{database.identities.RLSMetadataDependency()})
		if err == nil {
			data, present, readErr := database.identities.RLSMetadata(ctx, view, 1<<20)
			_ = view.Close()
			if readErr == nil && present && len(data) > 0 {
				var txState rlsPersistedState
				if err := json.Unmarshal(data, &txState); err != nil {
					return rlsPersistedState{}, err
				}
				if txState.Roles == nil {
					txState.Roles = make(map[string]rlsRole)
				}
				if txState.Tables == nil {
					txState.Tables = make(map[string]*rlsTableMeta)
				}
				return txState, nil
			}
			if readErr == nil && !present {
				return rlsPersistedState{
					Roles:  make(map[string]rlsRole),
					Tables: make(map[string]*rlsTableMeta),
				}, nil
			}
		}
	}
	if database.rls.pendingTx.Load() && !database.readOnly {
		_ = database.syncCommittedRLSState(ctx)
	}
	database.rls.mu.RLock()
	cloned := cloneRLSState(database.rls.state)
	database.rls.mu.RUnlock()
	return cloned, nil
}

func (database *Catalog) saveRLSState(ctx context.Context, scope *sessionScope, nextState rlsPersistedState) error {
	if nextState.Roles == nil {
		nextState.Roles = make(map[string]rlsRole)
	}
	if nextState.Tables == nil {
		nextState.Tables = make(map[string]*rlsTableMeta)
	}
	data, err := json.MarshalIndent(nextState, "", "  ")
	if err != nil {
		return err
	}
	if scope != nil && scope.txns != nil && scope.txns.ActiveIn(ctx) && database.identities != nil {
		err := scope.txns.Statement(ctx, func(stmtCtx context.Context, transaction *txn.Transaction) error {
			return database.identities.SaveRLSMetadata(stmtCtx, transaction, data, 1<<20)
		})
		if err != nil {
			return err
		}
		scope.rlsTouched.Store(true)
		database.rls.pendingTx.Store(true)
		return nil
	}
	if database.manager != nil && database.identities != nil && !database.readOnly {
		tx, err := database.manager.Begin(ctx, database.resolvedTransactionOptions())
		if err != nil {
			return err
		}
		err = tx.Statement(ctx, func(stmtCtx context.Context) error {
			return database.identities.SaveRLSMetadata(stmtCtx, tx, data, 1<<20)
		})
		if err == nil {
			_, err = tx.Commit(ctx)
		}
		if closeErr := tx.Close(context.Background()); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	database.rls.mu.Lock()
	database.rls.state = cloneRLSState(nextState)
	database.rls.pendingTx.Store(false)
	saveErr := database.rls.saveLocked()
	database.rls.mu.Unlock()
	return saveErr
}

func rlsTableKey(schema, table string) string {
	if schema == "" {
		schema = "public"
	}
	return strings.ToLower(schema) + "." + strings.ToLower(table)
}

func (scope *sessionScope) activeRole() string {
	if scope == nil || scope.settings == nil {
		return "supersql"
	}
	val, err := scope.settings.CurrentSetting("supersql.active_role", true)
	if err == nil && !val.IsNull && val.StrVal != "" && val.StrVal != "none" {
		return val.StrVal
	}
	return scope.sessionUserRole()
}

func (scope *sessionScope) sessionUserRole() string {
	if scope == nil || scope.settings == nil {
		return "supersql"
	}
	val, err := scope.settings.CurrentSetting("supersql.session_user", true)
	if err == nil && !val.IsNull && val.StrVal != "" && val.StrVal != "none" {
		return val.StrVal
	}
	return "supersql"
}

func isDefaultOwnerRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "", "none", "supersql", "embedded", "postgres":
		return true
	}
	return false
}

func roleHasSuperuser(state rlsPersistedState, role string) bool {
	if isDefaultOwnerRole(role) {
		return true
	}
	visited := make(map[string]bool)
	var check func(string) bool
	check = func(current string) bool {
		key := strings.ToLower(strings.TrimSpace(current))
		if visited[key] {
			return false
		}
		visited[key] = true
		if isDefaultOwnerRole(key) {
			return true
		}
		r, ok := state.Roles[key]
		if !ok {
			return false
		}
		if r.Superuser {
			return true
		}
		for _, parent := range r.MemberOf {
			if check(parent) {
				return true
			}
		}
		return false
	}
	return check(role)
}

func roleHasBypassRLS(state rlsPersistedState, role string) bool {
	visited := make(map[string]bool)
	var check func(string) bool
	check = func(current string) bool {
		key := strings.ToLower(strings.TrimSpace(current))
		if visited[key] {
			return false
		}
		visited[key] = true
		r, ok := state.Roles[key]
		if !ok {
			return false
		}
		if r.BypassRLS || r.Superuser {
			return true
		}
		for _, parent := range r.MemberOf {
			if check(parent) {
				return true
			}
		}
		return false
	}
	return check(role)
}

func roleHasMembership(state rlsPersistedState, memberRole, targetRole string) bool {
	memberKey := strings.ToLower(strings.TrimSpace(memberRole))
	targetKey := strings.ToLower(strings.TrimSpace(targetRole))
	if memberKey == targetKey {
		return true
	}
	visited := make(map[string]bool)
	var walk func(string) bool
	walk = func(current string) bool {
		curKey := strings.ToLower(strings.TrimSpace(current))
		if curKey == targetKey {
			return true
		}
		if visited[curKey] {
			return false
		}
		visited[curKey] = true
		r, ok := state.Roles[curKey]
		if !ok {
			return false
		}
		for _, group := range r.MemberOf {
			if walk(group) {
				return true
			}
		}
		return false
	}
	return walk(memberKey)
}

func isSuperuserOrOwner(state rlsPersistedState, activeRole, tableOwner string) bool {
	if roleHasSuperuser(state, activeRole) {
		return true
	}
	if tableOwner != "" && strings.EqualFold(strings.TrimSpace(activeRole), strings.TrimSpace(tableOwner)) {
		return true
	}
	return false
}

func extractNodeSQLString(node *zpgparse.Node) string {
	if node == nil {
		return ""
	}
	if s := node.GetString_(); s != nil {
		return s.Sval
	}
	if c := node.GetAConst(); c != nil && !c.Isnull && c.GetSval() != nil {
		return c.GetSval().Sval
	}
	if rs := node.GetRoleSpec(); rs != nil {
		switch rs.Roletype {
		case zpgparse.RoleSpecType_ROLESPEC_CSTRING:
			return rs.Rolename
		case zpgparse.RoleSpecType_ROLESPEC_PUBLIC:
			return "public"
		}
	}
	return ""
}

// deparseRLSExpr converts a parsed AST expression back into a valid SQL expression string.
func deparseRLSExpr(node *zpgparse.Node) (string, error) {
	if node == nil {
		return "", nil
	}
	switch n := node.Node.(type) {
	case *zpgparse.Node_AConst:
		c := n.AConst
		if c.Isnull {
			return "NULL", nil
		}
		switch val := c.Val.(type) {
		case *zpgparse.A_Const_Ival:
			return strconv.FormatInt(int64(val.Ival.Ival), 10), nil
		case *zpgparse.A_Const_Fval:
			return val.Fval.Fval, nil
		case *zpgparse.A_Const_Boolval:
			if val.Boolval.Boolval {
				return "true", nil
			}
			return "false", nil
		case *zpgparse.A_Const_Sval:
			return "'" + strings.ReplaceAll(val.Sval.Sval, "'", "''") + "'", nil
		}
	case *zpgparse.Node_ColumnRef:
		var parts []string
		for _, field := range n.ColumnRef.Fields {
			if s := field.GetString_(); s != nil {
				parts = append(parts, quoteIdent(s.Sval))
			} else if field.GetAStar() != nil {
				parts = append(parts, "*")
			}
		}
		return strings.Join(parts, "."), nil
	case *zpgparse.Node_AExpr:
		ae := n.AExpr
		var opName string
		for _, nameNode := range ae.Name {
			if s := nameNode.GetString_(); s != nil {
				opName = s.Sval
			}
		}
		switch ae.Kind {
		case zpgparse.A_Expr_Kind_AEXPR_OP:
			if ae.Lexpr == nil {
				r, err := deparseRLSExpr(ae.Rexpr)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("(%s %s)", opName, r), nil
			}
			l, err := deparseRLSExpr(ae.Lexpr)
			if err != nil {
				return "", err
			}
			r, err := deparseRLSExpr(ae.Rexpr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(%s %s %s)", l, opName, r), nil
		case zpgparse.A_Expr_Kind_AEXPR_IN:
			l, err := deparseRLSExpr(ae.Lexpr)
			if err != nil {
				return "", err
			}
			list := ae.Rexpr.GetList()
			if list == nil {
				return "", &model.Error{Code: "0A000", Message: "unsupported IN expression in policy"}
			}
			var items []string
			for _, item := range list.Items {
				s, err := deparseRLSExpr(item)
				if err != nil {
					return "", err
				}
				items = append(items, s)
			}
			kw := "IN"
			if opName == "<>" {
				kw = "NOT IN"
			}
			return fmt.Sprintf("(%s %s (%s))", l, kw, strings.Join(items, ", ")), nil
		case zpgparse.A_Expr_Kind_AEXPR_DISTINCT:
			l, err := deparseRLSExpr(ae.Lexpr)
			if err != nil {
				return "", err
			}
			r, err := deparseRLSExpr(ae.Rexpr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(%s IS DISTINCT FROM %s)", l, r), nil
		case zpgparse.A_Expr_Kind_AEXPR_NOT_DISTINCT:
			l, err := deparseRLSExpr(ae.Lexpr)
			if err != nil {
				return "", err
			}
			r, err := deparseRLSExpr(ae.Rexpr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(%s IS NOT DISTINCT FROM %s)", l, r), nil
		}
	case *zpgparse.Node_BoolExpr:
		be := n.BoolExpr
		var args []string
		for _, arg := range be.Args {
			s, err := deparseRLSExpr(arg)
			if err != nil {
				return "", err
			}
			args = append(args, s)
		}
		switch be.Boolop {
		case zpgparse.BoolExprType_AND_EXPR:
			return "(" + strings.Join(args, " AND ") + ")", nil
		case zpgparse.BoolExprType_OR_EXPR:
			return "(" + strings.Join(args, " OR ") + ")", nil
		case zpgparse.BoolExprType_NOT_EXPR:
			if len(args) == 1 {
				return "(NOT " + args[0] + ")", nil
			}
		}
	case *zpgparse.Node_NullTest:
		nt := n.NullTest
		arg, err := deparseRLSExpr(nt.Arg)
		if err != nil {
			return "", err
		}
		if nt.Nulltesttype == zpgparse.NullTestType_IS_NULL {
			return fmt.Sprintf("(%s IS NULL)", arg), nil
		}
		return fmt.Sprintf("(%s IS NOT NULL)", arg), nil
	case *zpgparse.Node_BooleanTest:
		bt := n.BooleanTest
		arg, err := deparseRLSExpr(bt.Arg)
		if err != nil {
			return "", err
		}
		switch bt.Booltesttype {
		case zpgparse.BoolTestType_IS_TRUE:
			return fmt.Sprintf("(%s IS TRUE)", arg), nil
		case zpgparse.BoolTestType_IS_NOT_TRUE:
			return fmt.Sprintf("(%s IS NOT TRUE)", arg), nil
		case zpgparse.BoolTestType_IS_FALSE:
			return fmt.Sprintf("(%s IS FALSE)", arg), nil
		case zpgparse.BoolTestType_IS_NOT_FALSE:
			return fmt.Sprintf("(%s IS NOT FALSE)", arg), nil
		case zpgparse.BoolTestType_IS_UNKNOWN:
			return fmt.Sprintf("(%s IS UNKNOWN)", arg), nil
		case zpgparse.BoolTestType_IS_NOT_UNKNOWN:
			return fmt.Sprintf("(%s IS NOT UNKNOWN)", arg), nil
		}
	case *zpgparse.Node_FuncCall:
		fc := n.FuncCall
		var fnParts []string
		for _, nameNode := range fc.Funcname {
			if s := nameNode.GetString_(); s != nil {
				fnParts = append(fnParts, s.Sval)
			}
		}
		var args []string
		for _, arg := range fc.Args {
			s, err := deparseRLSExpr(arg)
			if err != nil {
				return "", err
			}
			args = append(args, s)
		}
		return fmt.Sprintf("%s(%s)", strings.Join(fnParts, "."), strings.Join(args, ", ")), nil
	case *zpgparse.Node_SqlvalueFunction:
		svf := n.SqlvalueFunction
		switch svf.Op {
		case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_USER:
			return "current_user", nil
		case zpgparse.SQLValueFunctionOp_SVFOP_CURRENT_ROLE:
			return "current_role", nil
		case zpgparse.SQLValueFunctionOp_SVFOP_SESSION_USER:
			return "session_user", nil
		case zpgparse.SQLValueFunctionOp_SVFOP_USER:
			return "user", nil
		}
	case *zpgparse.Node_TypeCast:
		tc := n.TypeCast
		arg, err := deparseRLSExpr(tc.Arg)
		if err != nil {
			return "", err
		}
		typeName := deparseTypeName(tc.TypeName)
		if typeName == "" {
			return arg, nil
		}
		return fmt.Sprintf("CAST(%s AS %s)", arg, typeName), nil
	case *zpgparse.Node_CoalesceExpr:
		ce := n.CoalesceExpr
		var args []string
		for _, arg := range ce.Args {
			s, err := deparseRLSExpr(arg)
			if err != nil {
				return "", err
			}
			args = append(args, s)
		}
		return fmt.Sprintf("COALESCE(%s)", strings.Join(args, ", ")), nil
	case *zpgparse.Node_SubLink:
		sl := n.SubLink
		subSel := sl.Subselect.GetSelectStmt()
		if subSel == nil {
			return "", &model.Error{Code: "0A000", Message: "unsupported subquery in RLS policy"}
		}
		subSQL, err := deparseSimpleSelect(subSel)
		if err != nil {
			return "", err
		}
		switch sl.SubLinkType {
		case zpgparse.SubLinkType_EXISTS_SUBLINK:
			return fmt.Sprintf("(EXISTS (%s))", subSQL), nil
		case zpgparse.SubLinkType_ANY_SUBLINK:
			testExpr, err := deparseRLSExpr(sl.Testexpr)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("(%s IN (%s))", testExpr, subSQL), nil
		case zpgparse.SubLinkType_EXPR_SUBLINK:
			return fmt.Sprintf("((%s))", subSQL), nil
		}
	}
	if out, err := zpgparse.DeparseExpression(node); err == nil {
		return out, nil
	}
	return "", &model.Error{Code: "0A000", Message: "unsupported expression in RLS policy"}
}

func deparseSimpleSelect(sel *zpgparse.SelectStmt) (string, error) {
	var targets []string
	for _, t := range sel.TargetList {
		rt := t.GetResTarget()
		if rt == nil {
			continue
		}
		s, err := deparseRLSExpr(rt.Val)
		if err != nil {
			return "", err
		}
		targets = append(targets, s)
	}
	var froms []string
	for _, f := range sel.FromClause {
		rv := f.GetRangeVar()
		if rv == nil {
			return "", &model.Error{Code: "0A000", Message: "unsupported FROM clause in policy subquery"}
		}
		tbl := quoteIdent(rv.Relname)
		if rv.Schemaname != "" {
			tbl = quoteIdent(rv.Schemaname) + "." + tbl
		}
		if rv.Alias != nil && rv.Alias.Aliasname != "" {
			tbl += " AS " + quoteIdent(rv.Alias.Aliasname)
		}
		froms = append(froms, tbl)
	}
	sql := "SELECT " + strings.Join(targets, ", ")
	if len(froms) > 0 {
		sql += " FROM " + strings.Join(froms, ", ")
	}
	if sel.WhereClause != nil {
		w, err := deparseRLSExpr(sel.WhereClause)
		if err != nil {
			return "", err
		}
		sql += " WHERE " + w
	}
	return sql, nil
}

func deparseTypeName(tn *zpgparse.TypeName) string {
	if tn == nil {
		return ""
	}
	var names []string
	for _, n := range tn.Names {
		if s := n.GetString_(); s != nil {
			names = append(names, s.Sval)
		}
	}
	if len(names) == 0 {
		return ""
	}
	last := strings.ToLower(names[len(names)-1])
	switch last {
	case "int4":
		return "integer"
	case "int8":
		return "bigint"
	case "int2":
		return "smallint"
	case "bool":
		return "boolean"
	case "float8":
		return "double precision"
	case "float4":
		return "real"
	default:
		return last
	}
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func rlsCatalogReadDependencies(store *catalog.Store) []txn.ReadDependency {
	if store == nil {
		return nil
	}
	layouts := store.Layouts()
	dependencies := make([]txn.ReadDependency, 0, len(layouts))
	full := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	for _, layout := range layouts {
		dependencies = append(dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: store.KeyLayout(), Spans: full})
	}
	return dependencies
}

func (database *Catalog) openRLSCommittedCatalogSnapshot(ctx context.Context) (*catalog.CatalogSnapshot, error) {
	if database == nil || database.catalog == nil || database.manager == nil {
		return nil, &model.Error{Code: "55000", Message: "catalog is unavailable"}
	}
	if database.readOnly {
		state, stateErr := database.backend.State(ctx)
		if stateErr != nil {
			return nil, stateErr
		}
		reader, err := database.manager.HistoricalSnapshot(ctx, state.ClosedTS)
		if err != nil {
			return nil, err
		}
		snap, err := database.catalog.Snapshot(reader, state.ClosedTS, catalog.OwnReader)
		if err != nil {
			_ = reader.Close()
			return nil, err
		}
		return snap, nil
	}
	view, err := database.manager.ClosedReadView(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := database.catalog.TransactionSnapshot(view, catalog.OwnReader)
	if err != nil {
		_ = view.Close()
		return nil, err
	}
	return snap, nil
}

func (database *Catalog) openRLSTransactionCatalogSnapshot(ctx context.Context, scope *sessionScope) (*catalog.CatalogSnapshot, error) {
	if scope == nil || scope.txns == nil || !scope.txns.ActiveIn(ctx) {
		return nil, &model.Error{Code: "25P01", Message: "no active transaction"}
	}
	txView, err := scope.txns.ReadView(ctx, rlsCatalogReadDependencies(database.catalog))
	if err != nil {
		return nil, err
	}
	snap, err := database.catalog.TransactionSnapshot(txView, catalog.OwnReader)
	if err != nil {
		_ = txView.Close()
		return nil, err
	}
	return snap, nil
}

func (database *Catalog) resolveCatalogTable(ctx context.Context, scope *sessionScope, schemaName, relName string) (resolvedSchema, resolvedName string, columns map[string]bool, columnOrder []string, err error) {
	if relName == "" {
		return "", "", nil, nil, &model.Error{Code: "42P01", Message: "relation does not exist"}
	}
	searchPath := []string{"public"}
	if scope != nil && scope.settings != nil {
		if sp := scope.settings.SearchPath(); len(sp) > 0 {
			searchPath = sp
		}
	}
	candidates := searchPath
	if schemaName != "" {
		candidates = []string{schemaName}
	}

	resolveFrom := func(snapshot *catalog.CatalogSnapshot) (string, string, map[string]bool, []string, error) {
		for _, cand := range candidates {
			ns, nsErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: cand})
			if errors.Is(nsErr, catalog.ErrUnknownObject) {
				continue
			}
			if nsErr != nil {
				return "", "", nil, nil, nsErr
			}
			rel, relErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: ns.Descriptor.ID(), Kind: model.ObjectTable, Name: relName})
			if errors.Is(relErr, catalog.ErrUnknownObject) {
				continue
			}
			if relErr != nil {
				return "", "", nil, nil, relErr
			}
			if rel.Descriptor.State() != model.StatePublic {
				continue
			}
			tbl, ok := rel.Descriptor.Table()
			if !ok {
				continue
			}
			cols := make(map[string]bool, len(tbl.Columns))
			order := make([]string, 0, len(tbl.Columns))
			for _, col := range tbl.Columns {
				if !col.SystemIdentity && col.State == model.StatePublic {
					cols[strings.ToLower(col.Name)] = true
					order = append(order, col.Name)
				}
			}
			return cand, relName, cols, order, nil
		}
		return "", "", nil, nil, &model.Error{Code: "42P01", Message: fmt.Sprintf("relation %q does not exist", relName)}
	}

	snapshot, err := database.openRLSCommittedCatalogSnapshot(ctx)
	if err != nil {
		return "", "", nil, nil, err
	}
	resSchema, resName, cols, order, resolveErr := resolveFrom(snapshot)
	_ = snapshot.Close()
	if resolveErr == nil || scope == nil || scope.txns == nil || !scope.txns.ActiveIn(ctx) {
		return resSchema, resName, cols, order, resolveErr
	}
	txSnap, txErr := database.openRLSTransactionCatalogSnapshot(ctx, scope)
	if txErr != nil {
		return "", "", nil, nil, resolveErr
	}
	defer func() { _ = txSnap.Close() }()
	return resolveFrom(txSnap)
}

func isCatalogOrSystemRelation(schemaName, relName string) bool {
	s := strings.ToLower(strings.TrimSpace(schemaName))
	r := strings.ToLower(strings.TrimSpace(relName))
	if s == "pg_catalog" || s == "information_schema" {
		return true
	}
	if s == "" && (strings.HasPrefix(r, "pg_") || strings.HasPrefix(r, "sql_")) {
		return true
	}
	return false
}

// relationExistsAsNonTable checks whether (schemaName, relName) exists in the
// catalog as a view, materialized view, or sequence rather than a base table.
func (database *Catalog) relationExistsAsNonTable(ctx context.Context, scope *sessionScope, schemaName, relName string) bool {
	if isCatalogOrSystemRelation(schemaName, relName) {
		return true
	}
	searchPath := []string{"public"}
	if scope != nil && scope.settings != nil {
		if sp := scope.settings.SearchPath(); len(sp) > 0 {
			searchPath = sp
		}
	}
	candidates := searchPath
	if schemaName != "" {
		candidates = []string{schemaName}
	}
	kinds := []model.ObjectKind{model.ObjectView, model.ObjectMaterializedView, model.ObjectSequence}
	checkIn := func(snapshot *catalog.CatalogSnapshot) bool {
		for _, cand := range candidates {
			ns, nsErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: cand})
			if nsErr != nil {
				continue
			}
			for _, kind := range kinds {
				rel, relErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: ns.Descriptor.ID(), Kind: kind, Name: relName})
				if relErr == nil && rel.Descriptor.State() == model.StatePublic {
					return true
				}
			}
		}
		return false
	}

	snapshot, err := database.openRLSCommittedCatalogSnapshot(ctx)
	if err == nil {
		found := checkIn(snapshot)
		_ = snapshot.Close()
		if found {
			return true
		}
	}
	if scope != nil && scope.txns != nil && scope.txns.ActiveIn(ctx) {
		if txSnap, txErr := database.openRLSTransactionCatalogSnapshot(ctx, scope); txErr == nil {
			found := checkIn(txSnap)
			_ = txSnap.Close()
			return found
		}
	}
	return false
}

// tryExecRLSCommand intercepts ROLE, POLICY, GRANT/REVOKE role, Row-Level Security ALTER TABLE,
// SHOW/SET ROLE, COPY FROM on RLS tables, EXECUTE on prepared statements with RLS, and pg_policies/pg_roles queries.
func (database *Catalog) tryExecRLSCommand(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, stream rowStream, streams *qcopy.Streams) (Outcome, bool, error) {
	if raw == nil || raw.Stmt == nil || database.rls == nil {
		return Outcome{}, false, nil
	}
	if scope != nil && scope.txns != nil && scope.txns.ActiveIn(ctx) {
		if txStmt := raw.Stmt.GetTransactionStmt(); txStmt != nil {
			return Outcome{}, false, nil
		}
		if scope.txns.Failed() {
			return Outcome{}, true, &model.Error{Code: "25P02", Message: "current transaction is aborted, commands ignored until end of transaction block"}
		}
	}
	failInTx := func(err error) (Outcome, bool, error) {
		if err != nil && scope != nil && scope.txns != nil && scope.txns.ActiveIn(ctx) {
			_, _ = scope.txns.RunStatement(ctx, &zpgparse.RawStmt{
				Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{}}},
			}, session.StatementExecution{}, nil)
		}
		return Outcome{}, true, err
	}

	node := raw.Stmt
	state, stateErr := database.effectiveRLSState(ctx, scope)
	if stateErr != nil {
		return failInTx(stateErr)
	}
	currentRole := scope.activeRole()

	// 1. CREATE ROLE <name> [WITH ...]
	if cr := node.GetCreateRoleStmt(); cr != nil {
		roleName := strings.TrimSpace(cr.Role)
		if roleName == "" {
			return failInTx(&model.Error{Code: "42601", Message: "role name cannot be empty"})
		}
		if !isSuperuserOrOwner(state, currentRole, "") {
			return failInTx(&model.Error{Code: "42501", Message: "permission denied to create role"})
		}
		roleKey := strings.ToLower(roleName)
		r := rlsRole{Name: roleName}
		for _, optNode := range cr.Options {
			def := optNode.GetDefElem()
			if def == nil {
				continue
			}
			val := true
			if def.Arg != nil {
				if b := def.Arg.GetBoolean(); b != nil {
					val = b.Boolval
				} else if iv := def.Arg.GetInteger(); iv != nil {
					val = iv.Ival != 0
				}
			}
			switch strings.ToLower(def.Defname) {
			case "superuser":
				r.Superuser = val
			case "nosuperuser":
				r.Superuser = false
			case "bypassrls":
				r.BypassRLS = val
			case "nobypassrls":
				r.BypassRLS = false
			case "rolemembers", "addroleto":
				if list := def.Arg.GetList(); list != nil {
					for _, item := range list.Items {
						if member := extractNodeSQLString(item); member != "" && strings.EqualFold(def.Defname, "addroleto") {
							r.MemberOf = appendUniqueRole(r.MemberOf, strings.ToLower(member))
						}
					}
				}
			}
		}
		if _, exists := state.Roles[roleKey]; exists {
			return failInTx(&model.Error{Code: "42710", Message: fmt.Sprintf("role %q already exists", roleName)})
		}
		state.Roles[roleKey] = r
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "CREATE ROLE", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 2. ALTER ROLE <name> [WITH ...]
	if ar := node.GetAlterRoleStmt(); ar != nil {
		roleName := extractNodeSQLString(&zpgparse.Node{Node: &zpgparse.Node_RoleSpec{RoleSpec: ar.Role}})
		if !isSuperuserOrOwner(state, currentRole, "") {
			return failInTx(&model.Error{Code: "42501", Message: "permission denied to alter role"})
		}
		roleKey := strings.ToLower(roleName)
		r, exists := state.Roles[roleKey]
		if !exists {
			return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("role %q does not exist", roleName)})
		}
		for _, optNode := range ar.Options {
			def := optNode.GetDefElem()
			if def == nil {
				continue
			}
			val := true
			if def.Arg != nil {
				if b := def.Arg.GetBoolean(); b != nil {
					val = b.Boolval
				} else if iv := def.Arg.GetInteger(); iv != nil {
					val = iv.Ival != 0
				}
			}
			switch strings.ToLower(def.Defname) {
			case "superuser":
				r.Superuser = val
			case "nosuperuser":
				r.Superuser = false
			case "bypassrls":
				r.BypassRLS = val
			case "nobypassrls":
				r.BypassRLS = false
			}
		}
		state.Roles[roleKey] = r
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "ALTER ROLE", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 3. DROP ROLE [IF EXISTS] <name>
	if dr := node.GetDropRoleStmt(); dr != nil {
		if !isSuperuserOrOwner(state, currentRole, "") {
			return failInTx(&model.Error{Code: "42501", Message: "permission denied to drop role"})
		}
		for _, roleNode := range dr.Roles {
			roleName := extractNodeSQLString(roleNode)
			roleKey := strings.ToLower(roleName)
			if _, exists := state.Roles[roleKey]; !exists {
				if !dr.MissingOk {
					return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("role %q does not exist", roleName)})
				}
				continue
			}
			delete(state.Roles, roleKey)
			for k, memberRole := range state.Roles {
				memberRole.MemberOf = removeRoleFromSlice(memberRole.MemberOf, roleKey)
				state.Roles[k] = memberRole
			}
		}
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "DROP ROLE", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 4. GRANT / REVOKE <role> TO / FROM <grantee> (table-level GRANT/REVOKE is rejected by binder with 0A000)
	if gr := node.GetGrantRoleStmt(); gr != nil {
		if !isSuperuserOrOwner(state, currentRole, "") {
			return failInTx(&model.Error{Code: "42501", Message: "permission denied to grant or revoke role"})
		}
		var grantedRoles []string
		for _, rNode := range gr.GrantedRoles {
			var name string
			if ag := rNode.GetAccessPriv(); ag != nil {
				name = ag.PrivName
			} else {
				name = extractNodeSQLString(rNode)
			}
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			if _, ok := state.Roles[name]; !ok && !isDefaultOwnerRole(name) {
				return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("role %q does not exist", name)})
			}
			grantedRoles = append(grantedRoles, name)
		}
		for _, gNode := range gr.GranteeRoles {
			grantee := strings.ToLower(strings.TrimSpace(extractNodeSQLString(gNode)))
			if grantee == "" {
				continue
			}
			r, ok := state.Roles[grantee]
			if !ok {
				return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("role %q does not exist", grantee)})
			}
			for _, granted := range grantedRoles {
				if gr.IsGrant {
					if roleHasMembership(state, granted, grantee) {
						return failInTx(&model.Error{Code: "0LP01", Message: fmt.Sprintf("role %q is already a member of role %q", granted, grantee)})
					}
					r.MemberOf = appendUniqueRole(r.MemberOf, granted)
				} else {
					r.MemberOf = removeRoleFromSlice(r.MemberOf, granted)
				}
			}
			state.Roles[grantee] = r
		}
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		tag := "REVOKE ROLE"
		if gr.IsGrant {
			tag = "GRANT ROLE"
		}
		return Outcome{Tag: tag, Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 5. SET [LOCAL] ROLE <role> / RESET ROLE / SHOW role
	if vset := node.GetVariableSetStmt(); vset != nil {
		lowerName := strings.ToLower(strings.TrimSpace(vset.Name))
		if lowerName == "supersql.session_user" {
			if !isSuperuserOrOwner(state, currentRole, "") {
				return failInTx(&model.Error{Code: "42501", Message: "permission denied to set session_user"})
			}
		}
		if vset.Kind == zpgparse.VariableSetKind_VAR_RESET_ALL {
			sessionUser := scope.sessionUserRole()
			if !isDefaultOwnerRole(sessionUser) && !roleHasSuperuser(state, sessionUser) {
				return failInTx(&model.Error{Code: "42501", Message: "permission denied to reset session authorization"})
			}
		}
		if lowerName == "role" || lowerName == "supersql.active_role" {
			targetRole := ""
			if vset.Kind == zpgparse.VariableSetKind_VAR_SET_VALUE {
				for _, arg := range vset.Args {
					if s := extractNodeSQLString(arg); s != "" {
						targetRole = s
					}
				}
			}
			if strings.EqualFold(targetRole, "none") {
				targetRole = ""
			}
			if targetRole == "" {
				sessionUser := scope.sessionUserRole()
				if !isDefaultOwnerRole(sessionUser) {
					targetRole = sessionUser
				}
			} else {
				roleKey := strings.ToLower(targetRole)
				_, exists := state.Roles[roleKey]
				if !exists && !isDefaultOwnerRole(roleKey) {
					return failInTx(&model.Error{Code: "22023", Message: fmt.Sprintf("role %q does not exist", targetRole)})
				}
				if !roleHasSuperuser(state, currentRole) && !roleHasMembership(state, currentRole, roleKey) {
					return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("permission denied to set role %q", targetRole)})
				}
			}
			rewritten := &zpgparse.RawStmt{
				Stmt: &zpgparse.Node{
					Node: &zpgparse.Node_VariableSetStmt{
						VariableSetStmt: &zpgparse.VariableSetStmt{
							Kind:    zpgparse.VariableSetKind_VAR_SET_VALUE,
							Name:    "supersql.active_role",
							IsLocal: vset.IsLocal,
							Args: []*zpgparse.Node{
								{Node: &zpgparse.Node_AConst{AConst: &zpgparse.A_Const{Val: &zpgparse.A_Const_Sval{Sval: &zpgparse.String{Sval: targetRole}}}}},
							},
						},
					},
				},
			}
			if targetRole == "" && vset.Kind == zpgparse.VariableSetKind_VAR_RESET {
				rewritten.Stmt.GetVariableSetStmt().Kind = zpgparse.VariableSetKind_VAR_RESET
				rewritten.Stmt.GetVariableSetStmt().Args = nil
			}
			res, err := scope.txns.RunStatement(ctx, rewritten, session.StatementExecution{}, nil)
			if err != nil {
				return Outcome{}, true, err
			}
			tag := "SET"
			if vset.Kind == zpgparse.VariableSetKind_VAR_RESET {
				tag = "RESET"
			}
			return Outcome{Tag: tag, Committed: res.Commit.Committed}, true, nil
		}
	}

	if vshow := node.GetVariableShowStmt(); vshow != nil && strings.EqualFold(vshow.Name, "role") {
		role := "none"
		if val, err := scope.settings.CurrentSetting("supersql.active_role", true); err == nil && !val.IsNull && val.StrVal != "" {
			role = val.StrVal
		}
		if stream != nil {
			escaped := strings.ReplaceAll(role, "'", "''")
			res, err := zpgparse.Parse("SELECT '" + escaped + "'::text AS role")
			if err != nil || res == nil || len(res.Stmts) == 0 {
				return Outcome{}, true, err
			}
			faked := &zpgparse.ExtendedParseResult{ParseResult: res}
			out, execErr := database.execStatement(ctx, scope, faked, 0, nil, 0, stream, nil)
			out.Tag = "SHOW"
			out.RowsAffected = 1
			return out, true, execErr
		}
		textType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}
		return Outcome{
			Tag:          "SHOW",
			RowsAffected: 1,
			Committed:    !scope.txns.ActiveIn(ctx),
			Columns:      []Column{{Name: "role", Type: textType, Layout: model.TypeLayout{Type: textType}}},
			Rows:         [][]model.Value{{model.StringVal(role)}},
			builtins:     scope.builtins(),
		}, true, nil
	}

	// 6. ALTER TABLE <table> ENABLE/DISABLE/FORCE/NO FORCE ROW LEVEL SECURITY
	if at := node.GetAlterTableStmt(); at != nil && at.Relation != nil {
		allRLS := len(at.Cmds) > 0
		for _, cmdNode := range at.Cmds {
			cmd := cmdNode.GetAlterTableCmd()
			if cmd == nil {
				allRLS = false
				break
			}
			switch cmd.Subtype {
			case zpgparse.AlterTableType_AT_EnableRowSecurity,
				zpgparse.AlterTableType_AT_DisableRowSecurity,
				zpgparse.AlterTableType_AT_ForceRowSecurity,
				zpgparse.AlterTableType_AT_NoForceRowSecurity:
			default:
				allRLS = false
			}
		}
		if allRLS {
			resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, at.Relation.Schemaname, at.Relation.Relname)
			if err != nil {
				return failInTx(err)
			}
			key := rlsTableKey(resSchema, resName)
			meta := state.Tables[key]
			owner := ""
			if meta != nil {
				owner = meta.Owner
			}
			if !isSuperuserOrOwner(state, currentRole, owner) {
				return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", resName)})
			}
			if meta == nil {
				meta = &rlsTableMeta{
					Schema:   resSchema,
					Name:     resName,
					Owner:    currentRole,
					Policies: make(map[string]rlsPolicy),
				}
				state.Tables[key] = meta
			}
			for _, cmdNode := range at.Cmds {
				cmd := cmdNode.GetAlterTableCmd()
				switch cmd.Subtype {
				case zpgparse.AlterTableType_AT_EnableRowSecurity:
					meta.RLSEnabled = true
				case zpgparse.AlterTableType_AT_DisableRowSecurity:
					meta.RLSEnabled = false
				case zpgparse.AlterTableType_AT_ForceRowSecurity:
					meta.ForceRLS = true
				case zpgparse.AlterTableType_AT_NoForceRowSecurity:
					meta.ForceRLS = false
				}
			}
			_ = cols
			if err := database.saveRLSState(ctx, scope, state); err != nil {
				return failInTx(err)
			}
			return Outcome{Tag: "ALTER TABLE", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
		}
	}

	// 7. CREATE POLICY <name> ON <table> ...
	if cp := node.GetCreatePolicyStmt(); cp != nil && cp.Table != nil {
		resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, cp.Table.Schemaname, cp.Table.Relname)
		if err != nil {
			return failInTx(err)
		}
		key := rlsTableKey(resSchema, resName)
		meta := state.Tables[key]
		owner := ""
		if meta != nil {
			owner = meta.Owner
		}
		if !isSuperuserOrOwner(state, currentRole, owner) {
			return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", resName)})
		}
		usingSQL, err := deparseRLSExpr(cp.Qual)
		if err != nil {
			return failInTx(err)
		}
		checkSQL, err := deparseRLSExpr(cp.WithCheck)
		if err != nil {
			return failInTx(err)
		}
		if err := validatePolicyColumnRefs(cp.Qual, cols); err != nil {
			return failInTx(err)
		}
		if err := validatePolicyColumnRefs(cp.WithCheck, cols); err != nil {
			return failInTx(err)
		}
		cmdName := strings.ToUpper(strings.TrimSpace(cp.CmdName))
		if cmdName == "" {
			cmdName = "ALL"
		}
		if cmdName == "SELECT" && checkSQL != "" {
			return failInTx(&model.Error{Code: "42601", Message: "WITH CHECK cannot be applied to SELECT policy"})
		}
		if cmdName == "DELETE" && checkSQL != "" {
			return failInTx(&model.Error{Code: "42601", Message: "WITH CHECK cannot be applied to DELETE policy"})
		}
		if cmdName == "INSERT" && usingSQL != "" {
			return failInTx(&model.Error{Code: "42601", Message: "only WITH CHECK expression allowed for INSERT policy"})
		}
		var roles []string
		for _, rNode := range cp.Roles {
			if rName := extractNodeSQLString(rNode); rName != "" {
				roles = append(roles, strings.ToLower(rName))
			}
		}
		if meta == nil {
			meta = &rlsTableMeta{
				Schema:   resSchema,
				Name:     resName,
				Owner:    currentRole,
				Policies: make(map[string]rlsPolicy),
			}
			state.Tables[key] = meta
		}
		if meta.Policies == nil {
			meta.Policies = make(map[string]rlsPolicy)
		}
		polKey := strings.ToLower(cp.PolicyName)
		if _, exists := meta.Policies[polKey]; exists {
			return failInTx(&model.Error{Code: "42710", Message: fmt.Sprintf("policy %q for table %q already exists", cp.PolicyName, resName)})
		}
		meta.Policies[polKey] = rlsPolicy{
			Name:          cp.PolicyName,
			Permissive:    cp.Permissive,
			Command:       cmdName,
			Roles:         roles,
			UsingExpr:     usingSQL,
			WithCheckExpr: checkSQL,
		}
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "CREATE POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 8. ALTER POLICY <name> ON <table> ...
	if ap := node.GetAlterPolicyStmt(); ap != nil && ap.Table != nil {
		resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, ap.Table.Schemaname, ap.Table.Relname)
		if err != nil {
			return failInTx(err)
		}
		key := rlsTableKey(resSchema, resName)
		meta := state.Tables[key]
		owner := ""
		if meta != nil {
			owner = meta.Owner
		}
		if !isSuperuserOrOwner(state, currentRole, owner) {
			return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", resName)})
		}
		var usingSQL, checkSQL string
		if ap.Qual != nil {
			if err := validatePolicyColumnRefs(ap.Qual, cols); err != nil {
				return failInTx(err)
			}
			usingSQL, err = deparseRLSExpr(ap.Qual)
			if err != nil {
				return failInTx(err)
			}
		}
		if ap.WithCheck != nil {
			if err := validatePolicyColumnRefs(ap.WithCheck, cols); err != nil {
				return failInTx(err)
			}
			checkSQL, err = deparseRLSExpr(ap.WithCheck)
			if err != nil {
				return failInTx(err)
			}
		}
		polKey := strings.ToLower(ap.PolicyName)
		if meta == nil || meta.Policies == nil {
			return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", ap.PolicyName, resName)})
		}
		pol, exists := meta.Policies[polKey]
		if !exists {
			return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", ap.PolicyName, resName)})
		}
		if len(ap.Roles) > 0 {
			pol.Roles = nil
			for _, rNode := range ap.Roles {
				if rName := extractNodeSQLString(rNode); rName != "" {
					pol.Roles = append(pol.Roles, strings.ToLower(rName))
				}
			}
		}
		if ap.Qual != nil {
			pol.UsingExpr = usingSQL
		}
		if ap.WithCheck != nil {
			pol.WithCheckExpr = checkSQL
		}
		meta.Policies[polKey] = pol
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "ALTER POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 8b. ALTER POLICY <name> ON <table> RENAME TO <newname>
	if ren := node.GetRenameStmt(); ren != nil && ren.RenameType == zpgparse.ObjectType_OBJECT_POLICY && ren.Relation != nil {
		resSchema, resName, _, _, err := database.resolveCatalogTable(ctx, scope, ren.Relation.Schemaname, ren.Relation.Relname)
		if err != nil {
			if ren.MissingOk {
				return Outcome{Tag: "ALTER POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
			}
			return failInTx(err)
		}
		key := rlsTableKey(resSchema, resName)
		meta := state.Tables[key]
		owner := ""
		if meta != nil {
			owner = meta.Owner
		}
		if !isSuperuserOrOwner(state, currentRole, owner) {
			return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", resName)})
		}
		oldKey := strings.ToLower(ren.Subname)
		newKey := strings.ToLower(ren.Newname)
		if meta == nil || meta.Policies == nil {
			if ren.MissingOk {
				return Outcome{Tag: "ALTER POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
			}
			return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", ren.Subname, resName)})
		}
		if _, exists := meta.Policies[newKey]; exists {
			return failInTx(&model.Error{Code: "42710", Message: fmt.Sprintf("policy %q for table %q already exists", ren.Newname, resName)})
		}
		pol, exists := meta.Policies[oldKey]
		if !exists {
			if ren.MissingOk {
				return Outcome{Tag: "ALTER POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
			}
			return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", ren.Subname, resName)})
		}
		delete(meta.Policies, oldKey)
		pol.Name = ren.Newname
		meta.Policies[newKey] = pol
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "ALTER POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 9. DROP POLICY [IF EXISTS] <name> ON <table>
	if dp := node.GetDropStmt(); dp != nil && dp.RemoveType == zpgparse.ObjectType_OBJECT_POLICY {
		for _, obj := range dp.Objects {
			list := obj.GetList()
			if list == nil || len(list.Items) < 2 {
				continue
			}
			var parts []string
			for _, item := range list.Items {
				if s := item.GetString_(); s != nil {
					parts = append(parts, s.Sval)
				}
			}
			if len(parts) < 2 {
				continue
			}
			polName := parts[len(parts)-1]
			tblName := parts[len(parts)-2]
			schemaName := ""
			if len(parts) >= 3 {
				schemaName = parts[0]
			}
			resSchema, resName, _, _, err := database.resolveCatalogTable(ctx, scope, schemaName, tblName)
			if err != nil {
				if dp.MissingOk {
					continue
				}
				return failInTx(err)
			}
			key := rlsTableKey(resSchema, resName)
			meta := state.Tables[key]
			owner := ""
			if meta != nil {
				owner = meta.Owner
			}
			if !isSuperuserOrOwner(state, currentRole, owner) {
				return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", resName)})
			}
			polKey := strings.ToLower(polName)
			if meta == nil || meta.Policies == nil {
				if !dp.MissingOk {
					return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", polName, resName)})
				}
				continue
			}
			if _, exists := meta.Policies[polKey]; !exists {
				if !dp.MissingOk {
					return failInTx(&model.Error{Code: "42704", Message: fmt.Sprintf("policy %q for table %q does not exist", polName, resName)})
				}
				continue
			}
			delete(meta.Policies, polKey)
		}
		if err := database.saveRLSState(ctx, scope, state); err != nil {
			return failInTx(err)
		}
		return Outcome{Tag: "DROP POLICY", Committed: !scope.txns.ActiveIn(ctx)}, true, nil
	}

	// 10. Non-superuser protection for table DROP / ALTER / TRUNCATE
	if !isSuperuserOrOwner(state, currentRole, "") {
		if dp := node.GetDropStmt(); dp != nil && dp.RemoveType == zpgparse.ObjectType_OBJECT_TABLE {
			return failInTx(&model.Error{Code: "42501", Message: "must be owner of table"})
		}
		if at := node.GetAlterTableStmt(); at != nil && at.Relation != nil {
			return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", at.Relation.Relname)})
		}
		if rn := node.GetRenameStmt(); rn != nil && rn.Relation != nil {
			return failInTx(&model.Error{Code: "42501", Message: fmt.Sprintf("must be owner of table %s", rn.Relation.Relname)})
		}
		if tr := node.GetTruncateStmt(); tr != nil {
			return failInTx(&model.Error{Code: "42501", Message: "must be owner of table to truncate"})
		}
	}

	// 11. COPY <table> FROM STDIN with RLS WITH CHECK enforcement
	if copyStmt := node.GetCopyStmt(); copyStmt != nil && copyStmt.IsFrom && copyStmt.Relation != nil {
		if out, handled, err := database.tryExecCopyFromRLS(ctx, scope, raw, copyStmt, state, currentRole, streams); handled {
			if err != nil {
				return failInTx(err)
			}
			return out, true, nil
		}
	}

	// 12. EXECUTE <prepared_name> with dynamic RLS re-evaluation
	if execStmt := node.GetExecuteStmt(); execStmt != nil {
		if out, handled, err := database.tryExecPreparedWithRLS(ctx, scope, execStmt, state, currentRole, stream); handled {
			if err != nil {
				return failInTx(err)
			}
			return out, true, nil
		}
	}

	// 13. DEALLOCATE / DISCARD cleanup for session prepared statements
	if dealloc := node.GetDeallocateStmt(); dealloc != nil {
		scope.deletePreparedStmt(dealloc.Name, dealloc.Isall)
	}
	if disc := node.GetDiscardStmt(); disc != nil && (disc.Target == zpgparse.DiscardMode_DISCARD_ALL || disc.Target == zpgparse.DiscardMode_DISCARD_PLANS) {
		scope.deletePreparedStmt("", true)
	}

	// 14. Dynamic pg_policies / pg_roles introspection when policies or roles exist
	if sel := node.GetSelectStmt(); sel != nil && len(sel.FromClause) == 1 {
		if rv := sel.FromClause[0].GetRangeVar(); rv != nil {
			relLower := strings.ToLower(rv.Relname)
			if (relLower == "pg_policies" || relLower == "pg_roles") && (rv.Schemaname == "" || strings.EqualFold(rv.Schemaname, "pg_catalog")) {
				if out, ok, err := database.tryQueryRLSCatalogView(ctx, scope, raw, relLower, state, stream); ok {
					return out, true, err
				}
			}
		}
	}

	return Outcome{}, false, nil
}

func appendUniqueRole(list []string, role string) []string {
	for _, existing := range list {
		if strings.EqualFold(existing, role) {
			return list
		}
	}
	return append(list, role)
}

func removeRoleFromSlice(list []string, role string) []string {
	out := list[:0]
	for _, existing := range list {
		if !strings.EqualFold(existing, role) {
			out = append(out, existing)
		}
	}
	return out
}

func (database *Catalog) tryExecCopyFromRLS(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, copyStmt *zpgparse.CopyStmt, state rlsPersistedState, activeRole string, streams *qcopy.Streams) (Outcome, bool, error) {
	if roleHasBypassRLS(state, activeRole) {
		return Outcome{}, false, nil
	}
	resSchema, resName, _, _, err := database.resolveCatalogTable(ctx, scope, copyStmt.Relation.Schemaname, copyStmt.Relation.Relname)
	if err != nil {
		var modelErr *model.Error
		if errors.As(err, &modelErr) && modelErr.Code == "42P01" {
			return Outcome{}, false, nil
		}
		return Outcome{}, true, err
	}
	tableMeta := state.Tables[rlsTableKey(resSchema, resName)]
	if !database.rlsAppliesToTable(state, tableMeta, activeRole) {
		return Outcome{}, false, nil
	}
	if streams == nil || streams.Reader == nil || streams.MaxRecordBytes == 0 {
		return Outcome{}, false, nil
	}
	checkNode, hasPols, err := database.buildEffectiveRLSExpr(ctx, scope, tableMeta, activeRole, "INSERT", true)
	if err != nil {
		return Outcome{}, true, err
	}
	if !hasPols || checkNode == nil {
		return Outcome{}, true, &model.Error{
			Code:    "42501",
			Message: fmt.Sprintf("new row violates row-level security policy for table %q", resName),
		}
	}
	checkSQL, err := deparseRLSExpr(checkNode)
	if err != nil {
		return Outcome{}, true, err
	}

	startedTx := false
	createdSavepoint := false
	const spName = "__supersql_rls_copy_sp"
	if !scope.txns.ActiveIn(ctx) {
		if err := scope.txns.Begin(ctx, session.TransactionStart{}); err != nil {
			return Outcome{}, true, err
		}
		startedTx = true
	} else {
		if _, err := database.runInternalStatement(ctx, scope, "SAVEPOINT "+spName, nil); err != nil {
			return Outcome{}, true, err
		}
		createdSavepoint = true
	}
	abortCopy := func(cause error) (Outcome, bool, error) {
		if createdSavepoint {
			_, _ = database.runInternalStatement(ctx, scope, "ROLLBACK TO SAVEPOINT "+spName, nil)
			_, _ = database.runInternalStatement(ctx, scope, "RELEASE SAVEPOINT "+spName, nil)
		} else if startedTx {
			_ = scope.txns.Rollback(ctx, false)
		}
		return Outcome{}, true, cause
	}

	qualified := quoteIdent(resSchema) + "." + quoteIdent(resName)
	beforeTotal, beforePass, err := database.countTableRLSCheck(ctx, scope, qualified, checkSQL)
	if err != nil {
		return abortCopy(err)
	}
	copyOut, err := database.runRawStatement(ctx, scope, raw, nil, 0, nil, streams)
	if err != nil {
		return abortCopy(err)
	}
	afterTotal, afterPass, err := database.countTableRLSCheck(ctx, scope, qualified, checkSQL)
	if err != nil {
		return abortCopy(err)
	}
	if (afterTotal - beforeTotal) != (afterPass - beforePass) {
		return abortCopy(&model.Error{
			Code:    "42501",
			Message: fmt.Sprintf("new row violates row-level security policy for table %q", resName),
		})
	}
	if createdSavepoint {
		if _, err := database.runInternalStatement(ctx, scope, "RELEASE SAVEPOINT "+spName, nil); err != nil {
			return Outcome{}, true, err
		}
	} else if startedTx {
		commitRes, err := scope.txns.Commit(ctx, false)
		if err != nil {
			return Outcome{}, true, err
		}
		copyOut.Committed = commitRes.Committed
	}
	return copyOut, true, nil
}

func (database *Catalog) countTableRLSCheck(ctx context.Context, scope *sessionScope, qualifiedTable, checkSQL string) (total int64, passing int64, err error) {
	totalOut, err := database.runInternalStatement(ctx, scope, "SELECT count(*) FROM "+qualifiedTable, nil)
	if err != nil {
		return 0, 0, err
	}
	passOut, err := database.runInternalStatement(ctx, scope, "SELECT count(*) FROM "+qualifiedTable+" WHERE "+checkSQL, nil)
	if err != nil {
		return 0, 0, err
	}
	if len(totalOut.Rows) > 0 && len(totalOut.Rows[0]) > 0 {
		total = totalOut.Rows[0][0].IntVal
	}
	if len(passOut.Rows) > 0 && len(passOut.Rows[0]) > 0 {
		passing = passOut.Rows[0][0].IntVal
	}
	return total, passing, nil
}

func (database *Catalog) runInternalStatement(ctx context.Context, scope *sessionScope, sqlText string, streams *qcopy.Streams) (Outcome, error) {
	parsed, err := scope.txns.Parse(ctx, sqlText, zpgparse.ParseOptions{})
	if err != nil {
		return Outcome{}, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return Outcome{}, ErrMissingStatement
	}
	return database.runRawStatement(ctx, scope, parsed.ParseResult.Stmts[0], nil, 0, nil, streams)
}

func (database *Catalog) tryExecPreparedWithRLS(ctx context.Context, scope *sessionScope, execStmt *zpgparse.ExecuteStmt, state rlsPersistedState, activeRole string, stream rowStream) (Outcome, bool, error) {
	if isDefaultOwnerRole(activeRole) && !state.hasAnyPoliciesOrRLS() {
		return Outcome{}, false, nil
	}
	prep := scope.getPreparedStmt(execStmt.Name)
	if prep == nil || prep.Query == nil {
		return Outcome{}, false, nil
	}
	var found bool
	var expectedParams int
	_ = scope.txns.VisitPreparedStatements(ctx, func(summary model.PreparedStatementSummary) error {
		if strings.EqualFold(summary.Name, execStmt.Name) {
			found = true
			expectedParams = len(summary.ParameterTypes)
		}
		return nil
	})
	if !found || len(execStmt.Params) != expectedParams {
		return Outcome{}, false, nil
	}
	clonedQuery := proto.Clone(prep.Query).(*zpgparse.Node)
	substituted, ok := substitutePreparedParams(clonedQuery, execStmt.Params, prep.Argtypes)
	if !ok {
		return Outcome{}, false, nil
	}
	raw := &zpgparse.RawStmt{Stmt: substituted}
	rewritten, rlsCheck, err := database.rewriteStatementForRLS(ctx, scope, raw)
	if err != nil {
		return Outcome{}, true, err
	}
	out, err := database.runRawStatementWithRLSCheck(ctx, scope, rewritten, rlsCheck, nil, 0, stream, nil)
	return out, true, err
}

func substitutePreparedParams(node *zpgparse.Node, params []*zpgparse.Node, argTypes []*zpgparse.Node) (*zpgparse.Node, bool) {
	if node == nil {
		return nil, true
	}
	if pr := node.GetParamRef(); pr != nil {
		idx := int(pr.Number) - 1
		if idx < 0 || idx >= len(params) {
			return nil, false
		}
		replacement := proto.Clone(params[idx]).(*zpgparse.Node)
		if idx < len(argTypes) && argTypes[idx] != nil && argTypes[idx].GetTypeName() != nil {
			replacement = &zpgparse.Node{
				Node: &zpgparse.Node_TypeCast{
					TypeCast: &zpgparse.TypeCast{
						Arg:      replacement,
						TypeName: proto.Clone(argTypes[idx].GetTypeName()).(*zpgparse.TypeName),
						Location: -1,
					},
				},
			}
		}
		return replacement, true
	}
	switch inner := node.Node.(type) {
	case *zpgparse.Node_SelectStmt:
		s := inner.SelectStmt
		for i, t := range s.TargetList {
			var ok bool
			if s.TargetList[i], ok = substitutePreparedParams(t, params, argTypes); !ok {
				return nil, false
			}
		}
		for i, f := range s.FromClause {
			var ok bool
			if s.FromClause[i], ok = substitutePreparedParams(f, params, argTypes); !ok {
				return nil, false
			}
		}
		if s.WhereClause != nil {
			var ok bool
			if s.WhereClause, ok = substitutePreparedParams(s.WhereClause, params, argTypes); !ok {
				return nil, false
			}
		}
		for i, v := range s.ValuesLists {
			var ok bool
			if s.ValuesLists[i], ok = substitutePreparedParams(v, params, argTypes); !ok {
				return nil, false
			}
		}
		if s.Larg != nil {
			if _, ok := substitutePreparedParams(&zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: s.Larg}}, params, argTypes); !ok {
				return nil, false
			}
		}
		if s.Rarg != nil {
			if _, ok := substitutePreparedParams(&zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: s.Rarg}}, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_InsertStmt:
		ins := inner.InsertStmt
		if ins.SelectStmt != nil {
			var ok bool
			if ins.SelectStmt, ok = substitutePreparedParams(ins.SelectStmt, params, argTypes); !ok {
				return nil, false
			}
		}
		if ins.OnConflictClause != nil {
			for i, t := range ins.OnConflictClause.TargetList {
				var ok bool
				if ins.OnConflictClause.TargetList[i], ok = substitutePreparedParams(t, params, argTypes); !ok {
					return nil, false
				}
			}
			if ins.OnConflictClause.WhereClause != nil {
				var ok bool
				if ins.OnConflictClause.WhereClause, ok = substitutePreparedParams(ins.OnConflictClause.WhereClause, params, argTypes); !ok {
					return nil, false
				}
			}
		}
	case *zpgparse.Node_UpdateStmt:
		upd := inner.UpdateStmt
		for i, t := range upd.TargetList {
			var ok bool
			if upd.TargetList[i], ok = substitutePreparedParams(t, params, argTypes); !ok {
				return nil, false
			}
		}
		if upd.WhereClause != nil {
			var ok bool
			if upd.WhereClause, ok = substitutePreparedParams(upd.WhereClause, params, argTypes); !ok {
				return nil, false
			}
		}
		for i, f := range upd.FromClause {
			var ok bool
			if upd.FromClause[i], ok = substitutePreparedParams(f, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_DeleteStmt:
		del := inner.DeleteStmt
		if del.WhereClause != nil {
			var ok bool
			if del.WhereClause, ok = substitutePreparedParams(del.WhereClause, params, argTypes); !ok {
				return nil, false
			}
		}
		for i, u := range del.UsingClause {
			var ok bool
			if del.UsingClause[i], ok = substitutePreparedParams(u, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_ResTarget:
		var ok bool
		if inner.ResTarget.Val, ok = substitutePreparedParams(inner.ResTarget.Val, params, argTypes); !ok {
			return nil, false
		}
	case *zpgparse.Node_AExpr:
		var ok bool
		if inner.AExpr.Lexpr, ok = substitutePreparedParams(inner.AExpr.Lexpr, params, argTypes); !ok {
			return nil, false
		}
		if inner.AExpr.Rexpr, ok = substitutePreparedParams(inner.AExpr.Rexpr, params, argTypes); !ok {
			return nil, false
		}
	case *zpgparse.Node_BoolExpr:
		for i, a := range inner.BoolExpr.Args {
			var ok bool
			if inner.BoolExpr.Args[i], ok = substitutePreparedParams(a, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_FuncCall:
		for i, a := range inner.FuncCall.Args {
			var ok bool
			if inner.FuncCall.Args[i], ok = substitutePreparedParams(a, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_TypeCast:
		var ok bool
		if inner.TypeCast.Arg, ok = substitutePreparedParams(inner.TypeCast.Arg, params, argTypes); !ok {
			return nil, false
		}
	case *zpgparse.Node_List:
		for i, item := range inner.List.Items {
			var ok bool
			if inner.List.Items[i], ok = substitutePreparedParams(item, params, argTypes); !ok {
				return nil, false
			}
		}
	case *zpgparse.Node_JoinExpr:
		var ok bool
		if inner.JoinExpr.Larg, ok = substitutePreparedParams(inner.JoinExpr.Larg, params, argTypes); !ok {
			return nil, false
		}
		if inner.JoinExpr.Rarg, ok = substitutePreparedParams(inner.JoinExpr.Rarg, params, argTypes); !ok {
			return nil, false
		}
		if inner.JoinExpr.Quals, ok = substitutePreparedParams(inner.JoinExpr.Quals, params, argTypes); !ok {
			return nil, false
		}
	case *zpgparse.Node_RangeSubselect:
		var ok bool
		if inner.RangeSubselect.Subquery, ok = substitutePreparedParams(inner.RangeSubselect.Subquery, params, argTypes); !ok {
			return nil, false
		}
	case *zpgparse.Node_SubLink:
		var ok bool
		if inner.SubLink.Subselect, ok = substitutePreparedParams(inner.SubLink.Subselect, params, argTypes); !ok {
			return nil, false
		}
		if inner.SubLink.Testexpr, ok = substitutePreparedParams(inner.SubLink.Testexpr, params, argTypes); !ok {
			return nil, false
		}
	}
	return node, true
}

func (database *Catalog) tryQueryRLSCatalogView(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, relLower string, state rlsPersistedState, stream rowStream) (Outcome, bool, error) {
	if relLower == "pg_policies" {
		hasPolicies := false
		for _, meta := range state.Tables {
			if len(meta.Policies) > 0 {
				hasPolicies = true
				break
			}
		}
		if !hasPolicies {
			return Outcome{}, false, nil
		}
		var valueRows []string
		for _, meta := range state.Tables {
			for _, pol := range meta.Policies {
				perm := "PERMISSIVE"
				if !pol.Permissive {
					perm = "RESTRICTIVE"
				}
				rolesStr := "{public}"
				if len(pol.Roles) > 0 {
					rolesStr = "{" + strings.Join(pol.Roles, ",") + "}"
				}
				qualVal := "NULL::text"
				if pol.UsingExpr != "" {
					qualVal = "'" + strings.ReplaceAll(pol.UsingExpr, "'", "''") + "'::text"
				}
				checkVal := "NULL::text"
				if pol.WithCheckExpr != "" {
					checkVal = "'" + strings.ReplaceAll(pol.WithCheckExpr, "'", "''") + "'::text"
				}
				valueRows = append(valueRows, fmt.Sprintf("('%s'::name, '%s'::name, '%s'::name, '%s'::text, '%s'::name[], '%s'::text, %s, %s)",
					strings.ReplaceAll(meta.Schema, "'", "''"),
					strings.ReplaceAll(meta.Name, "'", "''"),
					strings.ReplaceAll(pol.Name, "'", "''"),
					perm,
					strings.ReplaceAll(rolesStr, "'", "''"),
					pol.Command,
					qualVal,
					checkVal,
				))
			}
		}
		cteSQL := fmt.Sprintf("SELECT * FROM (VALUES %s) AS pg_policies(schemaname, tablename, policyname, permissive, roles, cmd, qual, with_check)", strings.Join(valueRows, ", "))
		return database.execRewrittenCatalogSelect(ctx, scope, raw, "pg_policies", cteSQL, stream)
	}

	if relLower == "pg_roles" {
		if len(state.Roles) == 0 {
			return Outcome{}, false, nil
		}
		var valueRows []string
		valueRows = append(valueRows, "('supersql'::name, true, true, true, true, true, true, -1, NULL::timestamptz, true, false, NULL::text[], 10::oid)")
		oidSeq := 16385
		roleKeys := make([]string, 0, len(state.Roles))
		for k := range state.Roles {
			roleKeys = append(roleKeys, k)
		}
		slices.Sort(roleKeys)
		for _, k := range roleKeys {
			r := state.Roles[k]
			if isDefaultOwnerRole(r.Name) {
				continue
			}
			valueRows = append(valueRows, fmt.Sprintf("('%s'::name, %v, true, false, false, true, false, -1, NULL::timestamptz, %v, false, NULL::text[], %d::oid)",
				strings.ReplaceAll(r.Name, "'", "''"),
				r.Superuser,
				r.BypassRLS,
				oidSeq,
			))
			oidSeq++
		}
		cteSQL := fmt.Sprintf("SELECT * FROM (VALUES %s) AS pg_roles(rolname, rolsuper, rolinherit, rolcreaterole, rolcreatedb, rolcanlogin, rolreplication, rolconnlimit, rolvaliduntil, rolbypassrls, rolpassword, rolconfig, oid)", strings.Join(valueRows, ", "))
		return database.execRewrittenCatalogSelect(ctx, scope, raw, "pg_roles", cteSQL, stream)
	}
	return Outcome{}, false, nil
}

func (database *Catalog) execRewrittenCatalogSelect(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, viewName, subselectSQL string, stream rowStream) (Outcome, bool, error) {
	subParsed, err := zpgparse.Parse(subselectSQL)
	if err != nil || len(subParsed.Stmts) == 0 {
		return Outcome{}, false, nil
	}
	cloned := proto.Clone(raw).(*zpgparse.RawStmt)
	sel := cloned.Stmt.GetSelectStmt()
	rv := sel.FromClause[0].GetRangeVar()
	aliasName := viewName
	if rv.Alias != nil && rv.Alias.Aliasname != "" {
		aliasName = rv.Alias.Aliasname
	}
	sel.FromClause[0] = &zpgparse.Node{
		Node: &zpgparse.Node_RangeSubselect{
			RangeSubselect: &zpgparse.RangeSubselect{
				Subquery: subParsed.Stmts[0].Stmt,
				Alias:    &zpgparse.Alias{Aliasname: aliasName},
			},
		},
	}
	faked := &zpgparse.ExtendedParseResult{ParseResult: &zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{cloned}}}
	out, execErr := database.execStatement(ctx, scope, faked, 0, nil, 0, stream, nil)
	return out, true, execErr
}

func validatePolicyColumnRefs(node *zpgparse.Node, cols map[string]bool) error {
	if node == nil || len(cols) == 0 {
		return nil
	}
	switch n := node.Node.(type) {
	case *zpgparse.Node_ColumnRef:
		fields := n.ColumnRef.Fields
		if len(fields) == 1 {
			if s := fields[0].GetString_(); s != nil {
				colName := strings.ToLower(s.Sval)
				if !cols[colName] {
					return &model.Error{Code: "42703", Message: fmt.Sprintf("column %q does not exist", s.Sval)}
				}
			}
		}
	case *zpgparse.Node_AExpr:
		if err := validatePolicyColumnRefs(n.AExpr.Lexpr, cols); err != nil {
			return err
		}
		return validatePolicyColumnRefs(n.AExpr.Rexpr, cols)
	case *zpgparse.Node_BoolExpr:
		for _, arg := range n.BoolExpr.Args {
			if err := validatePolicyColumnRefs(arg, cols); err != nil {
				return err
			}
		}
	case *zpgparse.Node_NullTest:
		return validatePolicyColumnRefs(n.NullTest.Arg, cols)
	case *zpgparse.Node_BooleanTest:
		return validatePolicyColumnRefs(n.BooleanTest.Arg, cols)
	case *zpgparse.Node_FuncCall:
		for _, arg := range n.FuncCall.Args {
			if err := validatePolicyColumnRefs(arg, cols); err != nil {
				return err
			}
		}
	case *zpgparse.Node_TypeCast:
		return validatePolicyColumnRefs(n.TypeCast.Arg, cols)
	case *zpgparse.Node_CoalesceExpr:
		for _, arg := range n.CoalesceExpr.Args {
			if err := validatePolicyColumnRefs(arg, cols); err != nil {
				return err
			}
		}
	}
	return nil
}

type rlsCheckPlan struct {
	tableName        string
	hasUserReturning bool
}

func appendRLSCheckReturningNode(returning **zpgparse.ReturningClause, checkNode *zpgparse.Node) bool {
	hadReturning := *returning != nil && len((*returning).Exprs) > 0
	if *returning == nil {
		*returning = &zpgparse.ReturningClause{}
	}
	(*returning).Exprs = append((*returning).Exprs, &zpgparse.Node{
		Node: &zpgparse.Node_ResTarget{
			ResTarget: &zpgparse.ResTarget{
				Name:     "__supersql_rls_check",
				Val:      checkNode,
				Location: -1,
			},
		},
	})
	return hadReturning
}

func (database *Catalog) rlsAppliesToTable(state rlsPersistedState, meta *rlsTableMeta, activeRole string) bool {
	if meta == nil || !meta.RLSEnabled {
		return false
	}
	if roleHasBypassRLS(state, activeRole) {
		return false
	}
	owner := meta.Owner
	if owner == "" {
		owner = "supersql"
	}
	isOwner := isDefaultOwnerRole(activeRole) || strings.EqualFold(activeRole, owner)
	if isOwner && !meta.ForceRLS {
		return false
	}
	return true
}

func policyAppliesToRoleAndCmd(state rlsPersistedState, pol rlsPolicy, activeRole, cmd string) bool {
	if pol.Command != "ALL" && pol.Command != "" && !strings.EqualFold(pol.Command, cmd) {
		return false
	}
	if len(pol.Roles) == 0 {
		return true
	}
	ownerRole := isDefaultOwnerRole(activeRole)
	for _, r := range pol.Roles {
		if r == "public" || strings.EqualFold(r, activeRole) || (ownerRole && isDefaultOwnerRole(r)) || roleHasMembership(state, activeRole, r) {
			return true
		}
	}
	return false
}

// buildEffectiveRLSExpr combines applicable PERMISSIVE policies with OR and RESTRICTIVE policies with AND.
// If forCheck is true, WithCheckExpr is used (falling back to UsingExpr when WithCheckExpr is empty and command is ALL/UPDATE/INSERT).
func (database *Catalog) buildEffectiveRLSExpr(ctx context.Context, scope *sessionScope, meta *rlsTableMeta, activeRole, cmd string, forCheck bool) (*zpgparse.Node, bool, error) {
	state, err := database.effectiveRLSState(ctx, scope)
	if err != nil {
		return nil, false, err
	}
	var permissiveExprs []string
	var restrictiveExprs []string
	for _, pol := range meta.Policies {
		if !policyAppliesToRoleAndCmd(state, pol, activeRole, cmd) {
			continue
		}
		exprSQL := pol.UsingExpr
		if forCheck {
			if pol.WithCheckExpr != "" {
				exprSQL = pol.WithCheckExpr
			}
		}
		if exprSQL == "" {
			if forCheck && pol.UsingExpr != "" {
				exprSQL = pol.UsingExpr
			} else {
				exprSQL = "true"
			}
		}
		if pol.Permissive {
			permissiveExprs = append(permissiveExprs, "("+exprSQL+")")
		} else {
			restrictiveExprs = append(restrictiveExprs, "("+exprSQL+")")
		}
	}
	if len(permissiveExprs) == 0 {
		return nil, false, nil
	}
	combined := "(" + strings.Join(permissiveExprs, " OR ") + ")"
	for _, r := range restrictiveExprs {
		combined = "(" + combined + " AND " + r + ")"
	}
	parsed, err := zpgparse.Parse("SELECT 1 WHERE " + combined)
	if err != nil || len(parsed.Stmts) == 0 {
		return nil, false, &model.Error{Code: "42601", Message: "invalid RLS policy expression"}
	}
	sel := parsed.Stmts[0].Stmt.GetSelectStmt()
	if sel == nil || sel.WhereClause == nil {
		return nil, false, &model.Error{Code: "42601", Message: "invalid RLS policy expression"}
	}
	return sel.WhereClause, true, nil
}

func makeFalseNode() *zpgparse.Node {
	return &zpgparse.Node{
		Node: &zpgparse.Node_AConst{
			AConst: &zpgparse.A_Const{
				Val: &zpgparse.A_Const_Boolval{
					Boolval: &zpgparse.Boolean{Boolval: false},
				},
			},
		},
	}
}

func conjoinWhere(existing, added *zpgparse.Node) *zpgparse.Node {
	if added == nil {
		return existing
	}
	if existing == nil {
		return added
	}
	return &zpgparse.Node{
		Node: &zpgparse.Node_BoolExpr{
			BoolExpr: &zpgparse.BoolExpr{
				Boolop:   zpgparse.BoolExprType_AND_EXPR,
				Args:     []*zpgparse.Node{existing, added},
				Location: -1,
			},
		},
	}
}

func qualifyUnqualifiedColumnRefs(node *zpgparse.Node, tableAlias string, tableCols map[string]bool) {
	if node == nil || tableAlias == "" || len(tableCols) == 0 {
		return
	}
	switch n := node.Node.(type) {
	case *zpgparse.Node_ColumnRef:
		fields := n.ColumnRef.Fields
		if len(fields) == 1 {
			if s := fields[0].GetString_(); s != nil && tableCols[strings.ToLower(s.Sval)] {
				n.ColumnRef.Fields = []*zpgparse.Node{
					{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: tableAlias}}},
					fields[0],
				}
			}
		}
	case *zpgparse.Node_AExpr:
		qualifyUnqualifiedColumnRefs(n.AExpr.Lexpr, tableAlias, tableCols)
		qualifyUnqualifiedColumnRefs(n.AExpr.Rexpr, tableAlias, tableCols)
	case *zpgparse.Node_BoolExpr:
		for _, arg := range n.BoolExpr.Args {
			qualifyUnqualifiedColumnRefs(arg, tableAlias, tableCols)
		}
	case *zpgparse.Node_NullTest:
		qualifyUnqualifiedColumnRefs(n.NullTest.Arg, tableAlias, tableCols)
	case *zpgparse.Node_BooleanTest:
		qualifyUnqualifiedColumnRefs(n.BooleanTest.Arg, tableAlias, tableCols)
	case *zpgparse.Node_FuncCall:
		for _, arg := range n.FuncCall.Args {
			qualifyUnqualifiedColumnRefs(arg, tableAlias, tableCols)
		}
	case *zpgparse.Node_TypeCast:
		qualifyUnqualifiedColumnRefs(n.TypeCast.Arg, tableAlias, tableCols)
	case *zpgparse.Node_CoalesceExpr:
		for _, arg := range n.CoalesceExpr.Args {
			qualifyUnqualifiedColumnRefs(arg, tableAlias, tableCols)
		}
	case *zpgparse.Node_SubLink:
		qualifyUnqualifiedColumnRefs(n.SubLink.Testexpr, tableAlias, tableCols)
	}
}

func (database *Catalog) rewriteSelectStmtForRLS(ctx context.Context, scope *sessionScope, sel *zpgparse.SelectStmt, state rlsPersistedState, activeRole string, cteNames map[string]bool) error {
	if sel == nil {
		return nil
	}
	localCTEs := make(map[string]bool, len(cteNames))
	for k, v := range cteNames {
		localCTEs[k] = v
	}
	if sel.WithClause != nil {
		for _, cteNode := range sel.WithClause.Ctes {
			if cte := cteNode.GetCommonTableExpr(); cte != nil && cte.Ctename != "" {
				localCTEs[strings.ToLower(cte.Ctename)] = true
			}
		}
		for _, cteNode := range sel.WithClause.Ctes {
			cte := cteNode.GetCommonTableExpr()
			if cte == nil || cte.Ctequery == nil {
				continue
			}
			if subSel := cte.Ctequery.GetSelectStmt(); subSel != nil {
				if err := database.rewriteSelectStmtForRLS(ctx, scope, subSel, state, activeRole, localCTEs); err != nil {
					return err
				}
			}
		}
	}
	if sel.Larg != nil {
		if err := database.rewriteSelectStmtForRLS(ctx, scope, sel.Larg, state, activeRole, localCTEs); err != nil {
			return err
		}
	}
	if sel.Rarg != nil {
		if err := database.rewriteSelectStmtForRLS(ctx, scope, sel.Rarg, state, activeRole, localCTEs); err != nil {
			return err
		}
	}
	for i, fromItem := range sel.FromClause {
		qual, replacement, err := database.collectFromItemRLSQual(ctx, scope, fromItem, state, activeRole, localCTEs, false)
		if err != nil {
			return err
		}
		if replacement != nil {
			sel.FromClause[i] = replacement
		}
		if qual != nil {
			sel.WhereClause = conjoinWhere(sel.WhereClause, qual)
		}
	}
	if err := database.rewriteExprSublinksForRLS(ctx, scope, sel.WhereClause, state, activeRole, localCTEs); err != nil {
		return err
	}
	if err := database.rewriteExprSublinksForRLS(ctx, scope, sel.HavingClause, state, activeRole, localCTEs); err != nil {
		return err
	}
	for _, target := range sel.TargetList {
		if rt := target.GetResTarget(); rt != nil {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, rt.Val, state, activeRole, localCTEs); err != nil {
				return err
			}
		}
	}
	return nil
}

// rewriteExprSublinksForRLS traverses scalar expressions (WHERE, HAVING, SELECT list, JOIN ON, etc.)
// to apply RLS to any nested SubLink subqueries (EXISTS, IN, scalar subqueries).
func (database *Catalog) rewriteExprSublinksForRLS(ctx context.Context, scope *sessionScope, node *zpgparse.Node, state rlsPersistedState, activeRole string, cteNames map[string]bool) error {
	if node == nil {
		return nil
	}
	switch n := node.Node.(type) {
	case *zpgparse.Node_SubLink:
		if n.SubLink.Testexpr != nil {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, n.SubLink.Testexpr, state, activeRole, cteNames); err != nil {
				return err
			}
		}
		if n.SubLink.Subselect != nil {
			if subSel := n.SubLink.Subselect.GetSelectStmt(); subSel != nil {
				if err := database.rewriteSelectStmtForRLS(ctx, scope, subSel, state, activeRole, cteNames); err != nil {
					return err
				}
			}
		}
	case *zpgparse.Node_BoolExpr:
		for _, arg := range n.BoolExpr.Args {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, arg, state, activeRole, cteNames); err != nil {
				return err
			}
		}
	case *zpgparse.Node_AExpr:
		if err := database.rewriteExprSublinksForRLS(ctx, scope, n.AExpr.Lexpr, state, activeRole, cteNames); err != nil {
			return err
		}
		if err := database.rewriteExprSublinksForRLS(ctx, scope, n.AExpr.Rexpr, state, activeRole, cteNames); err != nil {
			return err
		}
	case *zpgparse.Node_NullTest:
		return database.rewriteExprSublinksForRLS(ctx, scope, n.NullTest.Arg, state, activeRole, cteNames)
	case *zpgparse.Node_BooleanTest:
		return database.rewriteExprSublinksForRLS(ctx, scope, n.BooleanTest.Arg, state, activeRole, cteNames)
	case *zpgparse.Node_FuncCall:
		for _, arg := range n.FuncCall.Args {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, arg, state, activeRole, cteNames); err != nil {
				return err
			}
		}
	case *zpgparse.Node_TypeCast:
		return database.rewriteExprSublinksForRLS(ctx, scope, n.TypeCast.Arg, state, activeRole, cteNames)
	case *zpgparse.Node_CoalesceExpr:
		for _, arg := range n.CoalesceExpr.Args {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, arg, state, activeRole, cteNames); err != nil {
				return err
			}
		}
	case *zpgparse.Node_CaseExpr:
		if err := database.rewriteExprSublinksForRLS(ctx, scope, n.CaseExpr.Arg, state, activeRole, cteNames); err != nil {
			return err
		}
		for _, w := range n.CaseExpr.Args {
			if cw := w.GetCaseWhen(); cw != nil {
				if err := database.rewriteExprSublinksForRLS(ctx, scope, cw.Expr, state, activeRole, cteNames); err != nil {
					return err
				}
				if err := database.rewriteExprSublinksForRLS(ctx, scope, cw.Result, state, activeRole, cteNames); err != nil {
					return err
				}
			}
		}
		return database.rewriteExprSublinksForRLS(ctx, scope, n.CaseExpr.Defresult, state, activeRole, cteNames)
	case *zpgparse.Node_List:
		for _, item := range n.List.Items {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, item, state, activeRole, cteNames); err != nil {
				return err
			}
		}
	}
	return nil
}

func rewriteRangeVarAsRLSSubselect(rv *zpgparse.RangeVar, resSchema, resName string, columnOrder []string, usingNode *zpgparse.Node) *zpgparse.Node {
	aliasName := rv.Relname
	var colAliases []*zpgparse.Node
	if rv.Alias != nil {
		if rv.Alias.Aliasname != "" {
			aliasName = rv.Alias.Aliasname
		}
		colAliases = rv.Alias.Colnames
	}
	targets := make([]*zpgparse.Node, 0, max(1, len(columnOrder)))
	for _, colName := range columnOrder {
		targets = append(targets, &zpgparse.Node{
			Node: &zpgparse.Node_ResTarget{
				ResTarget: &zpgparse.ResTarget{
					Name: colName,
					Val: &zpgparse.Node{
						Node: &zpgparse.Node_ColumnRef{
							ColumnRef: &zpgparse.ColumnRef{
								Fields: []*zpgparse.Node{
									{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: colName}}},
								},
								Location: -1,
							},
						},
					},
					Location: -1,
				},
			},
		})
	}
	if len(targets) == 0 {
		targets = append(targets, &zpgparse.Node{
			Node: &zpgparse.Node_ResTarget{
				ResTarget: &zpgparse.ResTarget{
					Val: &zpgparse.Node{
						Node: &zpgparse.Node_ColumnRef{
							ColumnRef: &zpgparse.ColumnRef{
								Fields:   []*zpgparse.Node{{Node: &zpgparse.Node_AStar{AStar: &zpgparse.A_Star{}}}},
								Location: -1,
							},
						},
					},
					Location: -1,
				},
			},
		})
	}
	innerRV := &zpgparse.RangeVar{
		Schemaname: resSchema,
		Relname:    resName,
		Inh:        true,
		Relpersistence: "p",
		Alias:      &zpgparse.Alias{Aliasname: aliasName},
		Location:   rv.Location,
	}
	subSel := &zpgparse.SelectStmt{
		TargetList:  targets,
		FromClause:  []*zpgparse.Node{{Node: &zpgparse.Node_RangeVar{RangeVar: innerRV}}},
		WhereClause: usingNode,
	}
	return &zpgparse.Node{
		Node: &zpgparse.Node_RangeSubselect{
			RangeSubselect: &zpgparse.RangeSubselect{
				Subquery: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: subSel}},
				Alias:    &zpgparse.Alias{Aliasname: aliasName, Colnames: colAliases},
			},
		},
	}
}

func (database *Catalog) collectFromItemRLSQual(ctx context.Context, scope *sessionScope, item *zpgparse.Node, state rlsPersistedState, activeRole string, cteNames map[string]bool, insideJoin bool) (*zpgparse.Node, *zpgparse.Node, error) {
	if item == nil {
		return nil, nil, nil
	}
	if rv := item.GetRangeVar(); rv != nil {
		if rv.Schemaname == "" && cteNames[strings.ToLower(rv.Relname)] {
			return nil, nil, nil
		}
		resSchema, resName, cols, colOrder, err := database.resolveCatalogTable(ctx, scope, rv.Schemaname, rv.Relname)
		if err != nil {
			var modelErr *model.Error
			if errors.As(err, &modelErr) && modelErr.Code == "42P01" {
				if database.relationExistsAsNonTable(ctx, scope, rv.Schemaname, rv.Relname) {
					return nil, nil, nil
				}
				return nil, nil, err
			}
			return nil, nil, err
		}
		meta := state.Tables[rlsTableKey(resSchema, resName)]
		if !database.rlsAppliesToTable(state, meta, activeRole) {
			return nil, nil, nil
		}
		usingNode, hasPols, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "SELECT", false)
		if err != nil {
			return nil, nil, err
		}
		if !hasPols {
			usingNode = makeFalseNode()
		} else {
			alias := rv.Relname
			if rv.Alias != nil && rv.Alias.Aliasname != "" {
				alias = rv.Alias.Aliasname
			}
			qualifyUnqualifiedColumnRefs(usingNode, alias, cols)
		}
		if insideJoin {
			return nil, rewriteRangeVarAsRLSSubselect(rv, resSchema, resName, colOrder, usingNode), nil
		}
		return usingNode, nil, nil
	}
	if join := item.GetJoinExpr(); join != nil {
		_, lRep, err := database.collectFromItemRLSQual(ctx, scope, join.Larg, state, activeRole, cteNames, true)
		if err != nil {
			return nil, nil, err
		}
		if lRep != nil {
			join.Larg = lRep
		}
		_, rRep, err := database.collectFromItemRLSQual(ctx, scope, join.Rarg, state, activeRole, cteNames, true)
		if err != nil {
			return nil, nil, err
		}
		if rRep != nil {
			join.Rarg = rRep
		}
		if err := database.rewriteExprSublinksForRLS(ctx, scope, join.Quals, state, activeRole, cteNames); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	if sub := item.GetRangeSubselect(); sub != nil && sub.Subquery != nil {
		if subSel := sub.Subquery.GetSelectStmt(); subSel != nil {
			if err := database.rewriteSelectStmtForRLS(ctx, scope, subSel, state, activeRole, cteNames); err != nil {
				return nil, nil, err
			}
		}
	}
	return nil, nil, nil
}

// capturePreDDLRLSTables records existing RLS table metadata before DROP TABLE or ALTER TABLE RENAME
// so that afterDDLStatementForRLS can clean up or migrate persisted policies on success.
func (database *Catalog) capturePreDDLRLSTables(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt) []string {
	if raw == nil || raw.Stmt == nil || database.rls == nil {
		return nil
	}
	var keys []string
	if dp := raw.Stmt.GetDropStmt(); dp != nil && dp.RemoveType == zpgparse.ObjectType_OBJECT_TABLE {
		for _, obj := range dp.Objects {
			list := obj.GetList()
			if list == nil || len(list.Items) == 0 {
				continue
			}
			var parts []string
			for _, item := range list.Items {
				if s := item.GetString_(); s != nil {
					parts = append(parts, s.Sval)
				}
			}
			if len(parts) == 0 {
				continue
			}
			tblName := parts[len(parts)-1]
			schemaName := ""
			if len(parts) >= 2 {
				schemaName = parts[0]
			}
			if resSchema, resName, _, _, err := database.resolveCatalogTable(ctx, scope, schemaName, tblName); err == nil {
				keys = append(keys, rlsTableKey(resSchema, resName))
			} else {
				keys = append(keys, rlsTableKey(schemaName, tblName))
			}
		}
	} else if rn := raw.Stmt.GetRenameStmt(); rn != nil && rn.Relation != nil &&
		(rn.RenameType == zpgparse.ObjectType_OBJECT_TABLE || rn.RelationType == zpgparse.ObjectType_OBJECT_TABLE) {
		if resSchema, resName, _, _, err := database.resolveCatalogTable(ctx, scope, rn.Relation.Schemaname, rn.Relation.Relname); err == nil {
			keys = append(keys, rlsTableKey(resSchema, resName))
		} else {
			keys = append(keys, rlsTableKey(rn.Relation.Schemaname, rn.Relation.Relname))
		}
	}
	return keys
}

func (database *Catalog) afterDDLStatementForRLS(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, preKeys []string) error {
	if raw == nil || raw.Stmt == nil || database.rls == nil || len(preKeys) == 0 {
		return nil
	}
	state, err := database.effectiveRLSState(ctx, scope)
	if err != nil {
		return err
	}
	changed := false
	if dp := raw.Stmt.GetDropStmt(); dp != nil && dp.RemoveType == zpgparse.ObjectType_OBJECT_TABLE {
		for _, key := range preKeys {
			if _, exists := state.Tables[key]; exists {
				delete(state.Tables, key)
				changed = true
			}
		}
	} else if rn := raw.Stmt.GetRenameStmt(); rn != nil && rn.Relation != nil && rn.Newname != "" && len(preKeys) == 1 {
		oldKey := preKeys[0]
		if meta, exists := state.Tables[oldKey]; exists {
			delete(state.Tables, oldKey)
			meta.Name = rn.Newname
			newKey := rlsTableKey(meta.Schema, rn.Newname)
			state.Tables[newKey] = meta
			changed = true
		}
	}
	if changed {
		return database.saveRLSState(ctx, scope, state)
	}
	return nil
}

// rewriteStatementForRLS rewrites SELECT, INSERT, UPDATE, DELETE, MERGE, COPY TO,
// PREPARE, DECLARE CURSOR, CREATE TABLE AS, and CREATE VIEW/MATERIALIZED VIEW
// statements when RLS is enabled on referenced tables.
func (database *Catalog) rewriteStatementForRLS(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt) (*zpgparse.RawStmt, *rlsCheckPlan, error) {
	if raw == nil || raw.Stmt == nil || database.rls == nil {
		return raw, nil, nil
	}
	state, err := database.effectiveRLSState(ctx, scope)
	if err != nil {
		return nil, nil, err
	}
	activeRole := scope.activeRole()
	if roleHasBypassRLS(state, activeRole) {
		if prep := raw.Stmt.GetPrepareStmt(); prep != nil {
			scope.savePreparedStmt(prep)
		}
		return raw, nil, nil
	}
	if !state.hasAnyPoliciesOrRLS() && isDefaultOwnerRole(activeRole) {
		if prep := raw.Stmt.GetPrepareStmt(); prep != nil {
			scope.savePreparedStmt(prep)
		}
		return raw, nil, nil
	}

	node := raw.Stmt

	// 1. SELECT (including SELECT ... INTO)
	if sel := node.GetSelectStmt(); sel != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		if err := database.rewriteSelectStmtForRLS(ctx, scope, cloned.Stmt.GetSelectStmt(), state, activeRole, nil); err != nil {
			return nil, nil, err
		}
		return cloned, nil, nil
	}

	// 2. PREPARE <name> AS <query>
	if prep := node.GetPrepareStmt(); prep != nil && prep.Query != nil {
		scope.savePreparedStmt(prep)
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedPrep := cloned.Stmt.GetPrepareStmt()
		rewrittenInner, _, err := database.rewriteStatementForRLS(ctx, scope, &zpgparse.RawStmt{Stmt: clonedPrep.Query})
		if err != nil {
			return nil, nil, err
		}
		clonedPrep.Query = rewrittenInner.Stmt
		return cloned, nil, nil
	}

	// 3. DECLARE <cursor> CURSOR FOR <query>
	if cur := node.GetDeclareCursorStmt(); cur != nil && cur.Query != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedCur := cloned.Stmt.GetDeclareCursorStmt()
		rewrittenInner, _, err := database.rewriteStatementForRLS(ctx, scope, &zpgparse.RawStmt{Stmt: clonedCur.Query})
		if err != nil {
			return nil, nil, err
		}
		clonedCur.Query = rewrittenInner.Stmt
		return cloned, nil, nil
	}

	// 4. CREATE TABLE AS / CREATE MATERIALIZED VIEW
	if ctas := node.GetCreateTableAsStmt(); ctas != nil && ctas.Query != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedCTAS := cloned.Stmt.GetCreateTableAsStmt()
		rewrittenInner, _, err := database.rewriteStatementForRLS(ctx, scope, &zpgparse.RawStmt{Stmt: clonedCTAS.Query})
		if err != nil {
			return nil, nil, err
		}
		clonedCTAS.Query = rewrittenInner.Stmt
		return cloned, nil, nil
	}

	// 5. CREATE VIEW
	if viewStmt := node.GetViewStmt(); viewStmt != nil && viewStmt.Query != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedView := cloned.Stmt.GetViewStmt()
		rewrittenInner, _, err := database.rewriteStatementForRLS(ctx, scope, &zpgparse.RawStmt{Stmt: clonedView.Query})
		if err != nil {
			return nil, nil, err
		}
		clonedView.Query = rewrittenInner.Stmt
		return cloned, nil, nil
	}

	// 6. COPY <table> TO STDOUT / COPY (<query>) TO STDOUT
	if copyStmt := node.GetCopyStmt(); copyStmt != nil && !copyStmt.IsFrom {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedCopy := cloned.Stmt.GetCopyStmt()
		if clonedCopy.Query != nil {
			rewrittenInner, _, err := database.rewriteStatementForRLS(ctx, scope, &zpgparse.RawStmt{Stmt: clonedCopy.Query})
			if err != nil {
				return nil, nil, err
			}
			clonedCopy.Query = rewrittenInner.Stmt
			return cloned, nil, nil
		}
		if clonedCopy.Relation != nil {
			resSchema, resName, cols, colOrder, err := database.resolveCatalogTable(ctx, scope, clonedCopy.Relation.Schemaname, clonedCopy.Relation.Relname)
			if err != nil {
				var modelErr *model.Error
				if errors.As(err, &modelErr) && modelErr.Code == "42P01" && database.relationExistsAsNonTable(ctx, scope, clonedCopy.Relation.Schemaname, clonedCopy.Relation.Relname) {
					return raw, nil, nil
				}
				return nil, nil, err
			}
			meta := state.Tables[rlsTableKey(resSchema, resName)]
			if database.rlsAppliesToTable(state, meta, activeRole) {
				usingNode, hasPols, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "SELECT", false)
				if err != nil {
					return nil, nil, err
				}
				if !hasPols {
					usingNode = makeFalseNode()
				} else {
					qualifyUnqualifiedColumnRefs(usingNode, resName, cols)
				}
				var selectCols []string
				if len(clonedCopy.Attlist) > 0 {
					for _, att := range clonedCopy.Attlist {
						if s := att.GetString_(); s != nil {
							selectCols = append(selectCols, s.Sval)
						}
					}
				} else {
					selectCols = colOrder
				}
				targets := make([]*zpgparse.Node, 0, len(selectCols))
				for _, colName := range selectCols {
					targets = append(targets, &zpgparse.Node{
						Node: &zpgparse.Node_ResTarget{
							ResTarget: &zpgparse.ResTarget{
								Name: colName,
								Val: &zpgparse.Node{
									Node: &zpgparse.Node_ColumnRef{
										ColumnRef: &zpgparse.ColumnRef{
											Fields: []*zpgparse.Node{
												{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: colName}}},
											},
											Location: -1,
										},
									},
								},
								Location: -1,
							},
						},
					})
				}
				clonedCopy.Relation = nil
				clonedCopy.Attlist = nil
				clonedCopy.Query = &zpgparse.Node{
					Node: &zpgparse.Node_SelectStmt{
						SelectStmt: &zpgparse.SelectStmt{
							TargetList: targets,
							FromClause: []*zpgparse.Node{
								{
									Node: &zpgparse.Node_RangeVar{
										RangeVar: &zpgparse.RangeVar{
											Schemaname:     resSchema,
											Relname:        resName,
											Inh:            true,
											Relpersistence: "p",
											Location:       -1,
										},
									},
								},
							},
							WhereClause: usingNode,
						},
					},
				}
				return cloned, nil, nil
			}
		}
		return raw, nil, nil
	}

	// 7. DELETE
	if del := node.GetDeleteStmt(); del != nil && del.Relation != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedDel := cloned.Stmt.GetDeleteStmt()
		for i, uItem := range clonedDel.UsingClause {
			qual, rep, err := database.collectFromItemRLSQual(ctx, scope, uItem, state, activeRole, nil, false)
			if err != nil {
				return nil, nil, err
			}
			if rep != nil {
				clonedDel.UsingClause[i] = rep
			}
			if qual != nil {
				clonedDel.WhereClause = conjoinWhere(clonedDel.WhereClause, qual)
			}
		}
		if err := database.rewriteExprSublinksForRLS(ctx, scope, clonedDel.WhereClause, state, activeRole, nil); err != nil {
			return nil, nil, err
		}
		resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, clonedDel.Relation.Schemaname, clonedDel.Relation.Relname)
		if err != nil {
			return nil, nil, err
		}
		meta := state.Tables[rlsTableKey(resSchema, resName)]
		if !database.rlsAppliesToTable(state, meta, activeRole) {
			return cloned, nil, nil
		}
		usingNode, hasPols, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "DELETE", false)
		if err != nil {
			return nil, nil, err
		}
		if !hasPols {
			usingNode = makeFalseNode()
		} else {
			alias := clonedDel.Relation.Relname
			if clonedDel.Relation.Alias != nil && clonedDel.Relation.Alias.Aliasname != "" {
				alias = clonedDel.Relation.Alias.Aliasname
			}
			qualifyUnqualifiedColumnRefs(usingNode, alias, cols)
		}
		clonedDel.WhereClause = conjoinWhere(clonedDel.WhereClause, usingNode)
		return cloned, nil, nil
	}

	// 8. UPDATE
	if upd := node.GetUpdateStmt(); upd != nil && upd.Relation != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedUpd := cloned.Stmt.GetUpdateStmt()
		for i, fItem := range clonedUpd.FromClause {
			qual, rep, err := database.collectFromItemRLSQual(ctx, scope, fItem, state, activeRole, nil, false)
			if err != nil {
				return nil, nil, err
			}
			if rep != nil {
				clonedUpd.FromClause[i] = rep
			}
			if qual != nil {
				clonedUpd.WhereClause = conjoinWhere(clonedUpd.WhereClause, qual)
			}
		}
		if err := database.rewriteExprSublinksForRLS(ctx, scope, clonedUpd.WhereClause, state, activeRole, nil); err != nil {
			return nil, nil, err
		}
		for _, target := range clonedUpd.TargetList {
			if rt := target.GetResTarget(); rt != nil {
				if err := database.rewriteExprSublinksForRLS(ctx, scope, rt.Val, state, activeRole, nil); err != nil {
					return nil, nil, err
				}
			}
		}
		resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, clonedUpd.Relation.Schemaname, clonedUpd.Relation.Relname)
		if err != nil {
			return nil, nil, err
		}
		meta := state.Tables[rlsTableKey(resSchema, resName)]
		if !database.rlsAppliesToTable(state, meta, activeRole) {
			return cloned, nil, nil
		}
		usingNode, hasUsing, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", false)
		if err != nil {
			return nil, nil, err
		}
		alias := clonedUpd.Relation.Relname
		if clonedUpd.Relation.Alias != nil && clonedUpd.Relation.Alias.Aliasname != "" {
			alias = clonedUpd.Relation.Alias.Aliasname
		}
		if !hasUsing {
			usingNode = makeFalseNode()
		} else {
			qualifyUnqualifiedColumnRefs(usingNode, alias, cols)
		}
		clonedUpd.WhereClause = conjoinWhere(clonedUpd.WhereClause, usingNode)

		checkNode, hasCheck, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", true)
		if err != nil {
			return nil, nil, err
		}
		if !hasCheck {
			checkNode = makeFalseNode()
		} else {
			qualifyUnqualifiedColumnRefs(checkNode, alias, cols)
		}
		// PostgreSQL requires updated rows to also satisfy SELECT USING policies when present.
		if selNode, hasSel, selErr := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "SELECT", false); selErr == nil && hasSel {
			qualifyUnqualifiedColumnRefs(selNode, alias, cols)
			checkNode = conjoinWhere(checkNode, selNode)
		}
		hasUserReturning := appendRLSCheckReturningNode(&clonedUpd.ReturningClause, checkNode)
		return cloned, &rlsCheckPlan{tableName: resName, hasUserReturning: hasUserReturning}, nil
	}

	// 9. INSERT
	if ins := node.GetInsertStmt(); ins != nil && ins.Relation != nil {
		cloned := proto.Clone(raw).(*zpgparse.RawStmt)
		clonedIns := cloned.Stmt.GetInsertStmt()
		if clonedIns.SelectStmt != nil {
			if sel := clonedIns.SelectStmt.GetSelectStmt(); sel != nil {
				if err := database.rewriteSelectStmtForRLS(ctx, scope, sel, state, activeRole, nil); err != nil {
					return nil, nil, err
				}
			}
		}
		resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, clonedIns.Relation.Schemaname, clonedIns.Relation.Relname)
		if err != nil {
			return nil, nil, err
		}
		meta := state.Tables[rlsTableKey(resSchema, resName)]
		if !database.rlsAppliesToTable(state, meta, activeRole) {
			return cloned, nil, nil
		}
		alias := clonedIns.Relation.Relname
		if clonedIns.Relation.Alias != nil && clonedIns.Relation.Alias.Aliasname != "" {
			alias = clonedIns.Relation.Alias.Aliasname
		}
		checkNode, hasCheck, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "INSERT", true)
		if err != nil {
			return nil, nil, err
		}
		if !hasCheck {
			checkNode = makeFalseNode()
		} else {
			qualifyUnqualifiedColumnRefs(checkNode, alias, cols)
		}
		// If ON CONFLICT DO UPDATE is present, enforce UPDATE USING on the conflict update target
		// and combine UPDATE WITH CHECK into the returning check expression.
		if oc := clonedIns.OnConflictClause; oc != nil && oc.Action == zpgparse.OnConflictAction_ONCONFLICT_UPDATE {
			updUsing, hasUpdUsing, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", false)
			if err != nil {
				return nil, nil, err
			}
			if !hasUpdUsing {
				updUsing = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(updUsing, alias, cols)
			}
			oc.WhereClause = conjoinWhere(oc.WhereClause, updUsing)

			updCheck, hasUpdCheck, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", true)
			if err != nil {
				return nil, nil, err
			}
			if !hasUpdCheck {
				updCheck = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(updCheck, alias, cols)
			}
			checkNode = conjoinWhere(checkNode, updCheck)
		}
		hasUserReturning := appendRLSCheckReturningNode(&clonedIns.ReturningClause, checkNode)
		return cloned, &rlsCheckPlan{tableName: resName, hasUserReturning: hasUserReturning}, nil
	}

	// 10. MERGE INTO <target> USING <source> ON <join_cond> ...
	if mergeStmt := node.GetMergeStmt(); mergeStmt != nil && mergeStmt.Relation != nil {
		return database.rewriteMergeStmtForRLS(ctx, scope, raw, state, activeRole)
	}

	return raw, nil, nil
}

func (database *Catalog) rewriteMergeStmtForRLS(ctx context.Context, scope *sessionScope, raw *zpgparse.RawStmt, state rlsPersistedState, activeRole string) (*zpgparse.RawStmt, *rlsCheckPlan, error) {
	cloned := proto.Clone(raw).(*zpgparse.RawStmt)
	mergeStmt := cloned.Stmt.GetMergeStmt()

	// Rewrite source relation (table or subquery) for SELECT RLS.
	if mergeStmt.SourceRelation != nil {
		qual, rep, err := database.collectFromItemRLSQual(ctx, scope, mergeStmt.SourceRelation, state, activeRole, nil, true)
		if err != nil {
			return nil, nil, err
		}
		if rep != nil {
			mergeStmt.SourceRelation = rep
		} else if qual != nil {
			mergeStmt.JoinCondition = conjoinWhere(mergeStmt.JoinCondition, qual)
		}
	}
	if err := database.rewriteExprSublinksForRLS(ctx, scope, mergeStmt.JoinCondition, state, activeRole, nil); err != nil {
		return nil, nil, err
	}

	resSchema, resName, cols, _, err := database.resolveCatalogTable(ctx, scope, mergeStmt.Relation.Schemaname, mergeStmt.Relation.Relname)
	if err != nil {
		return nil, nil, err
	}
	meta := state.Tables[rlsTableKey(resSchema, resName)]
	if !database.rlsAppliesToTable(state, meta, activeRole) {
		return cloned, nil, nil
	}
	alias := mergeStmt.Relation.Relname
	if mergeStmt.Relation.Alias != nil && mergeStmt.Relation.Alias.Aliasname != "" {
		alias = mergeStmt.Relation.Alias.Aliasname
	}

	// Target table visibility in MERGE requires SELECT USING predicate on target rows.
	selUsing, hasSelUsing, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "SELECT", false)
	if err != nil {
		return nil, nil, err
	}
	if !hasSelUsing {
		selUsing = makeFalseNode()
	} else {
		qualifyUnqualifiedColumnRefs(selUsing, alias, cols)
	}
	mergeStmt.JoinCondition = conjoinWhere(mergeStmt.JoinCondition, selUsing)

	var needCheck bool
	var checkBranches []*zpgparse.Node
	for _, clauseNode := range mergeStmt.MergeWhenClauses {
		mc := clauseNode.GetMergeWhenClause()
		if mc == nil {
			continue
		}
		if mc.Condition != nil {
			if err := database.rewriteExprSublinksForRLS(ctx, scope, mc.Condition, state, activeRole, nil); err != nil {
				return nil, nil, err
			}
		}
		switch mc.CommandType {
		case zpgparse.CmdType_CMD_UPDATE:
			updUsing, hasUpdUsing, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", false)
			if err != nil {
				return nil, nil, err
			}
			if !hasUpdUsing {
				updUsing = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(updUsing, alias, cols)
			}
			mc.Condition = conjoinWhere(mc.Condition, updUsing)

			updCheck, hasUpdCheck, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "UPDATE", true)
			if err != nil {
				return nil, nil, err
			}
			if !hasUpdCheck {
				updCheck = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(updCheck, alias, cols)
			}
			checkBranches = append(checkBranches, updCheck)
			needCheck = true

		case zpgparse.CmdType_CMD_DELETE:
			delUsing, hasDelUsing, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "DELETE", false)
			if err != nil {
				return nil, nil, err
			}
			if !hasDelUsing {
				delUsing = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(delUsing, alias, cols)
			}
			mc.Condition = conjoinWhere(mc.Condition, delUsing)

		case zpgparse.CmdType_CMD_INSERT:
			insCheck, hasInsCheck, err := database.buildEffectiveRLSExpr(ctx, scope, meta, activeRole, "INSERT", true)
			if err != nil {
				return nil, nil, err
			}
			if !hasInsCheck {
				insCheck = makeFalseNode()
			} else {
				qualifyUnqualifiedColumnRefs(insCheck, alias, cols)
			}
			checkBranches = append(checkBranches, insCheck)
			needCheck = true
		}
	}

	if !needCheck {
		return cloned, nil, nil
	}
	var combinedCheck *zpgparse.Node
	for _, b := range checkBranches {
		combinedCheck = conjoinWhere(combinedCheck, b)
	}
	if combinedCheck == nil {
		combinedCheck = makeFalseNode()
	}
	hasUserReturning := appendRLSCheckReturningNode(&mergeStmt.ReturningClause, combinedCheck)
	return cloned, &rlsCheckPlan{tableName: resName, hasUserReturning: hasUserReturning}, nil
}

func verifyRLSCheckBatch(ctx context.Context, batch *columnar.Batch, tableName string) error {
	if batch == nil || len(batch.Columns) == 0 {
		return nil
	}
	checkColIdx := len(batch.Columns) - 1
	for lane := range batch.Rows() {
		checkVal, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[checkColIdx], batch.Lane(lane), 1<<20)
		if copyErr != nil {
			return copyErr
		}
		if checkVal.IsNull || checkVal.IntVal == 0 {
			return &model.Error{
				Code:    "42501",
				Message: fmt.Sprintf("new row violates row-level security policy for table %q", tableName),
			}
		}
	}
	return nil
}

type emptyRowSource struct{}

func (emptyRowSource) Next(_ context.Context, _ *columnar.Batch) error { return io.EOF }

type rlsCheckingRowSource struct {
	inner     rowSource
	tableName string
}

func (s *rlsCheckingRowSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := s.inner.Next(ctx, out); err != nil {
		return err
	}
	if len(out.Columns) == 0 {
		return nil
	}
	checkIdx := len(out.Columns) - 1
	for lane := range out.Rows() {
		val, _, copyErr := columnar.CopyValue(ctx, &out.Columns[checkIdx], out.Lane(lane), 1<<20)
		if copyErr != nil {
			return copyErr
		}
		if val.IsNull || val.IntVal == 0 {
			return &model.Error{
				Code:    "42501",
				Message: fmt.Sprintf("new row violates row-level security policy for table %q", s.tableName),
			}
		}
	}
	out.Columns = out.Columns[:checkIdx]
	return nil
}

func (database *Catalog) consumeRowsFromSource(ctx context.Context, source rowSource, outcome *Outcome, maxRows int) error {
	var batch columnar.Batch
	for {
		if maxRows > 0 && len(outcome.Rows) >= maxRows {
			return nil
		}
		err := source.Next(ctx, &batch)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		for lane := range batch.Rows() {
			if maxRows > 0 && len(outcome.Rows) >= maxRows {
				return nil
			}
			row := make([]model.Value, len(batch.Columns))
			for index := range batch.Columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[index], batch.Lane(lane), 1<<20)
				if err != nil {
					return err
				}
				row[index] = value
			}
			outcome.Rows = append(outcome.Rows, row)
		}
	}
}

func (database *Catalog) consumeWithRLSCheck(
	statementCtx context.Context,
	columns []bind.ResultColumn,
	statementBuiltins builtin.ResolveOptions,
	rows *session.Rows,
	check *rlsCheckPlan,
	stream rowStream,
	outcome *Outcome,
	maxRows int,
) error {
	checkedSource := &rlsCheckingRowSource{inner: rows, tableName: check.tableName}
	if !check.hasUserReturning {
		if err := drainRows(statementCtx, nil, statementBuiltins, checkedSource); err != nil {
			return err
		}
		if stream != nil {
			return stream(statementCtx, nil, statementBuiltins, emptyRowSource{})
		}
		return nil
	}
	userCols := columns
	if len(userCols) > 0 {
		userCols = userCols[:len(userCols)-1]
	}
	if stream != nil {
		err := stream(statementCtx, userCols, statementBuiltins, checkedSource)
		return errors.Join(err, drainRows(statementCtx, nil, statementBuiltins, checkedSource))
	}
	outcome.builtins = statementBuiltins
	for _, column := range userCols {
		outcome.Columns = append(outcome.Columns, Column{
			Name:     column.Name,
			Type:     column.Layout.Type,
			Layout:   column.Layout,
			Nullable: column.Nullable,
		})
	}
	err := database.consumeRowsFromSource(statementCtx, checkedSource, outcome, maxRows)
	return errors.Join(err, drainRows(statementCtx, nil, statementBuiltins, checkedSource))
}

