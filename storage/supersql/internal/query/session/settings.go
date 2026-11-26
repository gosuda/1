package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

const maxCustomSettings = 256

// Zone is an immutable resolver identity, not a Go civil-time conversion policy.
// Fixed offsets use seconds east of UTC; named zones retain their canonical name.
type Zone struct {
	Name          string
	Fixed         bool
	OffsetSeconds int32
}

// SettingResolver is borrowed for the Settings lifetime. ResolveTimeZone handles
// nonnumeric PG18 IANA/POSIX names and INTERVAL syntax, rejects leap-second zones,
// and must not substitute Go civil-time conversions for PostgreSQL semantics.
// ResolveTextSearchConfig returns a canonical qualified logical name. Missing or
// malformed names (42704/42602) become GUC errors (22023) with their cause retained.
// ResolveFormatLocale validates three locale identities and returns their canonical
// names plus immutable numeric, monetary, and time formatting data. Other errors
// and cancellation propagate. No privileges are implied.
type SettingResolver interface {
	ResolveTimeZone(context.Context, string) (Zone, error)
	ResolveTextSearchConfig(context.Context, string, []string) (string, error)
	ResolveFormatLocale(context.Context, string, string, string) (model.FormatLocale, error)
}

// SettingLimits are host caps, independent of SQL GUC values. All are required.
// MaxValueBytes bounds each stored string; MaxSavepoints bounds undo ownership.
type SettingLimits struct {
	MaxValueBytes          int
	MaxSavepoints          int
	WorkMemoryBytes        int64
	MaintenanceMemoryBytes int64
	TemporaryFileBytes     int64
}

type SettingsOptions struct {
	Resolver SettingResolver
	Limits   SettingLimits
}

type SettingScope uint8

const (
	SessionScope SettingScope = iota
	LocalScope
)

// SettingResult carries the SQL text result and an optional SQL warning.
type SettingResult struct {
	Value   model.Value
	Warning *model.Error
}

// Settings synchronizes concurrent reads (such as timeout cleanup vs incoming
// statements) through mu. Begin/Commit/Rollback track settings and protected
// role identity only; the orchestrator must compose them with actual
// transaction outcomes. Errors leave settings unchanged; transaction abort
// handling belongs to that orchestrator.
type Settings struct {
	mu                   sync.RWMutex
	resolver             SettingResolver
	limits               SettingLimits
	state                settingState
	securityDefinerDepth int
}

type settingState struct {
	reset              settingValues
	current            settingValues
	retained           settingValues
	before             settingValues
	customCurrent      []customSetting
	customRetained     []customSetting
	customBefore       []customSetting
	currentRole        string
	retainedRole       string
	beforeRole         string
	sessionUser        string
	retainedSessionUsr string
	beforeSessionUsr   string
	active             bool
	dataAccess         bool
	savepoints         []settingSavepoint
}

type settingValues [settingCount]settingValue

type settingValue struct {
	text     string
	number   int64
	flag     bool
	zone     Zone
	assigned bool
}

type customSetting struct {
	name     string
	text     string
	assigned bool
}

type settingSavepoint struct {
	name               string
	current            settingValues
	retained           settingValues
	customCurrent      []customSetting
	customRetained     []customSetting
	currentRole        string
	retainedRole       string
	sessionUser        string
	retainedSessionUsr string
}

func NewSettings(ctx context.Context, opts SettingsOptions) (*Settings, error) {
	invalidStateLimits := opts.Limits.MaxValueBytes < 64 || opts.Limits.MaxSavepoints <= 0
	invalidResourceLimits := opts.Limits.WorkMemoryBytes <= 0 || opts.Limits.MaintenanceMemoryBytes <= 0 || opts.Limits.TemporaryFileBytes <= 0
	if opts.Resolver == nil || invalidStateLimits || invalidResourceLimits {
		return nil, settingError("22023", "settings require a resolver and positive host limits")
	}
	s := &Settings{resolver: opts.Resolver, limits: opts.Limits}
	for id, def := range settingDefinitions {
		value, err := s.parseLocked(ctx, settingID(id), def.initial)
		if err != nil {
			return nil, err
		}
		s.state.current[id] = value
	}
	s.state.reset, s.state.retained = s.state.current, s.state.current
	return s, nil
}

func (s *Settings) Principal() model.Principal {
	role := s.CurrentRole()
	if role == "" {
		return model.Principal{}
	}
	return model.NewPrincipal(role, 0)
}

func (s *Settings) InTransaction() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.active
}

// CurrentRole returns the active role for authorization and RLS evaluation.
// An empty string indicates the default built-in superuser principal.
func (s *Settings) CurrentRole() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentRoleLocked()
}

func (s *Settings) currentRoleLocked() string {
	if s.state.currentRole != "" {
		return s.state.currentRole
	}
	return s.state.sessionUser
}

// SessionUser returns the session's authenticated user role, or "" for the
// default built-in superuser principal.
func (s *Settings) SessionUser() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.sessionUser
}

// SetRole updates the protected active role. Only privileged authorization
// paths should invoke this after checking membership/superuser permissions.
func (s *Settings) SetRole(ctx context.Context, role string, local bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized := strings.ToLower(strings.TrimSpace(role))
	if normalized == "" || normalized == "none" {
		return s.ResetRole(ctx, local)
	}
	if !s.validString(normalized) {
		return badSetting("role", "invalid UTF8, zero byte, or host value-size limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if local && !s.state.active {
		return nil
	}
	cloned := strings.Clone(normalized)
	s.state.currentRole = cloned
	if !local {
		s.state.retainedRole = cloned
	}
	return nil
}

// ResetRole clears the active role override so execution reverts to SessionUser.
func (s *Settings) ResetRole(ctx context.Context, local bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if local && !s.state.active {
		return nil
	}
	s.state.currentRole = ""
	if !local {
		s.state.retainedRole = ""
	}
	return nil
}

// pushSecurityDefinerRole temporarily switches the active role to ownerName
// for a SECURITY DEFINER routine invocation and returns the previous raw role
// fields so popSecurityDefinerRole restores them unconditionally on exit.
func (s *Settings) pushSecurityDefinerRole(ownerName string) (prevCurrent, prevRetained string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prevCurrent, prevRetained = s.state.currentRole, s.state.retainedRole
	s.securityDefinerDepth++
	normalized := strings.ToLower(strings.TrimSpace(ownerName))
	if normalized == "" || normalized == "embedded" || normalized == "supersql" || normalized == "postgres" {
		s.state.currentRole = "supersql"
	} else {
		s.state.currentRole = strings.Clone(normalized)
	}
	return prevCurrent, prevRetained
}

func (s *Settings) popSecurityDefinerRole(prevCurrent, prevRetained string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.securityDefinerDepth > 0 {
		s.securityDefinerDepth--
	}
	s.state.currentRole = prevCurrent
	s.state.retainedRole = prevRetained
}

// InSecurityDefiner reports whether execution is currently inside one or more
// SECURITY DEFINER routine frames.
func (s *Settings) InSecurityDefiner() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.securityDefinerDepth > 0
}

// SetSessionUser updates the protected session_user identity and resets the
// current role override, matching PostgreSQL SET [LOCAL] SESSION AUTHORIZATION.
func (s *Settings) SetSessionUser(ctx context.Context, role string, local bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized := strings.ToLower(strings.TrimSpace(role))
	if normalized == "default" {
		normalized = ""
	}
	if normalized != "" && !s.validString(normalized) {
		return badSetting("session_authorization", "invalid UTF8, zero byte, or host value-size limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if local && !s.state.active {
		return nil
	}
	cloned := strings.Clone(normalized)
	s.state.sessionUser = cloned
	s.state.currentRole = ""
	if !local {
		s.state.retainedSessionUsr = cloned
		s.state.retainedRole = ""
	}
	return nil
}

// ResetAllSessionState resets all non-read-only settings, custom GUCs, active
// role, and session_user to initial connection defaults for safe pool reuse.
func (s *Settings) ResetAllSessionState(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.active {
		return settingError("25001", "cannot reset session state inside an active transaction")
	}
	for id, def := range settingDefinitions {
		if def.kind == readOnlySetting {
			continue
		}
		s.state.current[id] = s.state.reset[id]
		s.state.retained[id] = s.state.reset[id]
	}
	s.state.before = settingValues{}
	s.state.customCurrent = nil
	s.state.customRetained = nil
	s.state.customBefore = nil
	s.state.currentRole = ""
	s.state.retainedRole = ""
	s.state.beforeRole = ""
	s.state.sessionUser = ""
	s.state.retainedSessionUsr = ""
	s.state.beforeSessionUsr = ""
	clear(s.state.savepoints)
	s.state.savepoints = s.state.savepoints[:0]
	s.state.dataAccess = false
	return nil
}

func (s *Settings) snapshotState() settingState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.state
	st.savepoints = slices.Clone(st.savepoints)
	return st
}

func (s *Settings) restoreState(st settingState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.savepoints = slices.Clone(st.savepoints)
	s.state = st
}

func (s *Settings) cloneForStaging() *Settings {
	return &Settings{
		resolver: s.resolver,
		limits:   s.limits,
		state:    s.snapshotState(),
	}
}

func (s *Settings) commitStaged(staged *Settings) {
	st := staged.snapshotState()
	s.restoreState(st)
}

func (s *Settings) restoreStatementSettings(before settingState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.current = before.current
	s.state.retained = before.retained
	s.state.customCurrent = before.customCurrent
	s.state.customRetained = before.customRetained
	s.state.currentRole = before.currentRole
	s.state.retainedRole = before.retainedRole
	s.state.sessionUser = before.sessionUser
	s.state.retainedSessionUsr = before.retainedSessionUsr
}

func (s *Settings) savepointCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.state.savepoints)
}

func (s *Settings) findSavepointIndex(name string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.savepointIndexLocked(name)
}

func (s *Settings) transactionCharacteristics() transactionCharacteristics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return transactionCharacteristics{
		isolation:  s.state.current[transactionIsolation].text,
		readOnly:   s.state.current[transactionReadOnly].flag,
		deferrable: s.state.current[transactionDeferrable].flag,
	}
}

func (s *Settings) clientMinMessages() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.current[clientMinMessages].text
}

func (s *Settings) routineSettingsSnapshot() (current, retained settingValues) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.current, s.state.retained
}

func (s *Settings) restoreRoutineSettings(current, retained settingValues) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.current, s.state.retained = current, retained
}

func (s *Settings) formatLocaleNames() (numeric, monetary, clock string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.current[lcNumeric].text, s.state.current[lcMonetary].text, s.state.current[lcTime].text
}

func (s *Settings) catalogSettingsSnapshot() (current, reset settingValues) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.current, s.state.reset
}

// BeginOptions uses empty isolation and nil flags to inherit session defaults.
// Read-only mode cannot subsequently be upgraded to writable mode.
type BeginOptions struct {
	Isolation  string
	ReadOnly   *bool
	Deferrable *bool
}

func (s *Settings) Begin(opts BeginOptions) error {
	if opts.Isolation != "" {
		if err := bind.ValidateIsolation(opts.Isolation, 0); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.active {
		return settingError("25001", "a transaction is already active")
	}
	s.state.before = s.state.current
	s.state.retained = s.state.current
	s.state.customBefore = s.state.customCurrent
	s.state.customRetained = s.state.customCurrent
	s.state.beforeRole = s.state.currentRole
	s.state.retainedRole = s.state.currentRole
	s.state.beforeSessionUsr = s.state.sessionUser
	s.state.retainedSessionUsr = s.state.sessionUser
	s.state.current[transactionIsolation] = s.state.current[defaultIsolation]
	s.state.current[transactionReadOnly] = s.state.current[defaultReadOnly]
	s.state.current[transactionDeferrable] = s.state.current[defaultDeferrable]
	if opts.Isolation != "" {
		s.state.current[transactionIsolation].assigned = true
	}
	if opts.ReadOnly != nil {
		s.state.current[transactionReadOnly] = booleanValue(*opts.ReadOnly)
		s.state.current[transactionReadOnly].assigned = true
	}
	if opts.Deferrable != nil {
		s.state.current[transactionDeferrable] = booleanValue(*opts.Deferrable)
		s.state.current[transactionDeferrable].assigned = true
	}
	s.state.active, s.state.dataAccess = true, false
	return nil
}

// MarkDataAccess is called when the transaction takes its first data snapshot,
// not for SET/SHOW or a statement that fails before data access.
func (s *Settings) MarkDataAccess() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.active {
		return settingError("25P01", "no active transaction")
	}
	s.state.dataAccess = true
	return nil
}

func (s *Settings) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.active {
		return settingError("25P01", "no active transaction")
	}
	s.state.current = s.state.retained
	s.state.customCurrent = s.state.customRetained
	s.state.currentRole = s.state.retainedRole
	s.state.sessionUser = s.state.retainedSessionUsr
	s.finishLocked()
	return nil
}

func (s *Settings) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.active {
		return settingError("25P01", "no active transaction")
	}
	s.state.current = s.state.before
	s.state.customCurrent = s.state.customBefore
	s.state.currentRole = s.state.beforeRole
	s.state.sessionUser = s.state.beforeSessionUsr
	s.finishLocked()
	return nil
}

func (s *Settings) finishLocked() {
	s.state.current[transactionIsolation] = s.state.current[defaultIsolation]
	s.state.current[transactionReadOnly] = s.state.current[defaultReadOnly]
	s.state.current[transactionDeferrable] = s.state.current[defaultDeferrable]
	s.state.retained = s.state.current
	s.state.before = settingValues{}
	s.state.customRetained = s.state.customCurrent
	s.state.customBefore = nil
	s.state.retainedRole = s.state.currentRole
	s.state.beforeRole = ""
	s.state.retainedSessionUsr = s.state.sessionUser
	s.state.beforeSessionUsr = ""
	clear(s.state.savepoints)
	s.state.savepoints = s.state.savepoints[:0]
	s.state.active, s.state.dataAccess = false, false
}

func (s *Settings) Savepoint(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.active {
		return settingError("25P01", "no active transaction")
	}
	if name == "" || !s.validString(name) {
		return settingError("22023", "invalid savepoint name")
	}
	if len(s.state.savepoints) == s.limits.MaxSavepoints {
		return settingError("54000", "savepoint host limit exceeded")
	}
	s.state.savepoints = append(s.state.savepoints, settingSavepoint{
		name:               strings.Clone(name),
		current:            s.state.current,
		retained:           s.state.retained,
		customCurrent:      s.state.customCurrent,
		customRetained:     s.state.customRetained,
		currentRole:        s.state.currentRole,
		retainedRole:       s.state.retainedRole,
		sessionUser:        s.state.sessionUser,
		retainedSessionUsr: s.state.retainedSessionUsr,
	})
	return nil
}

func (s *Settings) RollbackTo(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := s.savepointIndexLocked(name)
	if err != nil {
		return err
	}
	point := s.state.savepoints[index]
	s.state.current, s.state.retained = point.current, point.retained
	s.state.customCurrent, s.state.customRetained = point.customCurrent, point.customRetained
	s.state.currentRole, s.state.retainedRole = point.currentRole, point.retainedRole
	s.state.sessionUser, s.state.retainedSessionUsr = point.sessionUser, point.retainedSessionUsr
	// FirstSnapshotSet is transaction-wide in PostgreSQL; it does not rewind.
	clear(s.state.savepoints[index+1:])
	s.state.savepoints = s.state.savepoints[:index+1]
	return nil
}

func (s *Settings) Release(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := s.savepointIndexLocked(name)
	if err != nil {
		return err
	}
	clear(s.state.savepoints[index:])
	s.state.savepoints = s.state.savepoints[:index]
	return nil
}

func (s *Settings) savepointIndexLocked(name string) (int, error) {
	if !s.state.active {
		return 0, settingError("25P01", "no active transaction")
	}
	for i := len(s.state.savepoints) - 1; i >= 0; i-- {
		if s.state.savepoints[i].name == name {
			return i, nil
		}
	}
	return 0, settingError("3B001", "savepoint does not exist")
}

func isProtectedRoleSetting(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "role", "session_authorization", "supersql.active_role", "supersql.session_user":
		return true
	default:
		return false
	}
}

func (s *Settings) Show(name string) (model.Value, error) { return s.CurrentSetting(name, false) }

func (s *Settings) CurrentSetting(name string, missingOK bool) (model.Value, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "role":
		if s.state.currentRole != "" {
			return model.StringVal(s.state.currentRole), nil
		}
		return model.StringVal("none"), nil
	case "session_authorization":
		if s.state.sessionUser != "" {
			return model.StringVal(s.state.sessionUser), nil
		}
		return model.StringVal(model.Principal{}.Name()), nil
	case "supersql.active_role":
		if role := s.currentRoleLocked(); role != "" {
			return model.StringVal(role), nil
		}
		if missingOK {
			return model.NullVal(model.TypeString), nil
		}
		return model.Value{}, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
	case "supersql.session_user":
		if s.state.sessionUser != "" {
			return model.StringVal(s.state.sessionUser), nil
		}
		if missingOK {
			return model.NullVal(model.TypeString), nil
		}
		return model.Value{}, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
	}
	id, custom, err := lookupSetting(name)
	if err != nil {
		var sqlErr *model.Error
		if errors.As(err, &sqlErr) && sqlErr.Code == "42602" {
			err = &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
			sqlErr = err.(*model.Error)
		}
		if missingOK && errors.As(err, &sqlErr) && sqlErr.Code == "42704" {
			return model.NullVal(model.TypeString), nil
		}
		return model.Value{}, err
	}
	if custom {
		canonical := strings.ToLower(name)
		for _, entry := range s.state.customCurrent {
			if entry.name == canonical {
				return model.StringVal(entry.text), nil
			}
		}
		if missingOK {
			return model.NullVal(model.TypeString), nil
		}
		return model.Value{}, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
	}
	return model.StringVal(s.state.current[id].text), nil
}

// Set accepts a decoded GUC value, never SQL source. Local assignments outside a
// transaction are validated but expire with the statement and emit 25P01.
func (s *Settings) Set(ctx context.Context, name, value string, scope SettingScope) (SettingResult, error) {
	return s.change(ctx, name, &value, scope, true)
}

func (s *Settings) Reset(ctx context.Context, name string, scope SettingScope) (SettingResult, error) {
	return s.change(ctx, name, nil, scope, true)
}

// SetConfig implements set_config's NULL-value reset and local flag. Unlike SET
// LOCAL, the function does not warn outside an explicit transaction block.
func (s *Settings) SetConfig(ctx context.Context, name string, value *string, local bool) (model.Value, error) {
	scope := SessionScope
	if local {
		scope = LocalScope
	}
	result, err := s.change(ctx, name, value, scope, false)
	return result.Value, err
}

func (s *Settings) change(ctx context.Context, name string, raw *string, scope SettingScope, warn bool) (SettingResult, error) {
	if err := ctx.Err(); err != nil {
		return SettingResult{}, err
	}
	if isProtectedRoleSetting(name) {
		return SettingResult{}, &model.Error{
			Code:    "42501",
			Message: fmt.Sprintf("permission denied to set parameter %q", strings.ToLower(strings.TrimSpace(name))),
		}
	}
	id, custom, err := lookupSetting(name)
	if err != nil {
		return SettingResult{}, err
	}
	if scope != SessionScope && scope != LocalScope {
		return SettingResult{}, settingError("22023", "invalid setting scope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if custom {
		text := ""
		assigned := false
		if raw != nil {
			if !s.validString(*raw) {
				return SettingResult{}, badSetting(name, "invalid UTF8, zero byte, or host value-size limit")
			}
			text = strings.Clone(*raw)
			assigned = true
		}
		canonical := strings.ToLower(name)
		result := SettingResult{Value: model.StringVal(text)}
		if scope == LocalScope && !s.state.active {
			if warn {
				result.Warning = &model.Error{Code: "25P01", Message: "SET LOCAL can only be used in transaction blocks"}
			}
			return result, nil
		}
		nextCurrent, err := setCustomSetting(s.state.customCurrent, canonical, text, assigned)
		if err != nil {
			return SettingResult{}, err
		}
		if scope == SessionScope {
			nextRetained, err := setCustomSetting(s.state.customRetained, canonical, text, assigned)
			if err != nil {
				return SettingResult{}, err
			}
			s.state.customRetained = nextRetained
		}
		s.state.customCurrent = nextCurrent
		return result, nil
	}
	if settingDefinitions[id].kind == readOnlySetting {
		return SettingResult{}, settingError("55P02", "parameter cannot be changed")
	}
	value := s.state.reset[id]
	if raw == nil {
		if isTransactionSetting(id) {
			return SettingResult{}, settingError("0A000", "parameter cannot be reset")
		}
	} else {
		value, err = s.parseLocked(ctx, id, *raw)
		if err != nil {
			return SettingResult{}, err
		}
	}
	value.assigned = raw != nil
	if err := s.checkTransactionChangeLocked(id, value); err != nil {
		return SettingResult{}, err
	}
	result := SettingResult{Value: model.StringVal(value.text)}
	if scope == LocalScope && !s.state.active {
		if warn {
			result.Warning = &model.Error{Code: "25P01", Message: "SET LOCAL can only be used in transaction blocks"}
		}
		return result, nil
	}
	s.state.current[id] = value
	if scope == SessionScope {
		s.state.retained[id] = value
	}
	return result, nil
}

func setCustomSetting(list []customSetting, canonical, text string, assigned bool) ([]customSetting, error) {
	cloned := slices.Clone(list)
	for i := range cloned {
		if cloned[i].name == canonical {
			cloned[i] = customSetting{name: canonical, text: text, assigned: assigned}
			return cloned, nil
		}
	}
	if len(cloned) >= maxCustomSettings {
		return nil, settingError("54000", "custom setting host limit exceeded")
	}
	return append(cloned, customSetting{name: strings.Clone(canonical), text: text, assigned: assigned}), nil
}

func (s *Settings) checkTransactionChangeLocked(id settingID, value settingValue) error {
	if !s.state.active {
		return nil
	}
	switch id {
	case transactionReadOnly:
		if s.state.current[id].flag && !value.flag {
			if s.state.dataAccess || len(s.state.savepoints) > 0 {
				return settingError("25001", "cannot set transaction read-write mode after data access or in a subtransaction")
			}
			return settingError("25006", "read-only transaction cannot be upgraded")
		}
	case transactionDeferrable:
		if s.state.dataAccess || len(s.state.savepoints) > 0 {
			return settingError("25001", "transaction deferrability must be set before data access and outside subtransactions")
		}
	}
	return nil
}

// ResetAll leaves current transaction characteristics and session_authorization
// alone (PG GUC_NO_RESET_ALL), while resetting role and ordinary GUCs.
func (s *Settings) ResetAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, def := range settingDefinitions {
		if def.kind == readOnlySetting || isTransactionSetting(settingID(id)) {
			continue
		}
		s.state.current[id], s.state.retained[id] = s.state.reset[id], s.state.reset[id]
	}
	if len(s.state.customCurrent) > 0 {
		resetCustom := make([]customSetting, len(s.state.customCurrent))
		for i, entry := range s.state.customCurrent {
			resetCustom[i] = customSetting{name: entry.name, text: "", assigned: false}
		}
		s.state.customCurrent = resetCustom
		s.state.customRetained = resetCustom
	}
	s.state.currentRole = ""
	s.state.retainedRole = ""
	return nil
}

func isTransactionSetting(id settingID) bool {
	return id == transactionIsolation || id == transactionReadOnly || id == transactionDeferrable
}

// TransactionPolicy describes mode, not a constructed dataplane transaction.
type TransactionPolicy struct{ ReadOnly, Deferrable bool }

func (s *Settings) TransactionPolicy() TransactionPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	readOnly := s.state.current[transactionReadOnly].flag
	return TransactionPolicy{ReadOnly: readOnly, Deferrable: readOnly && s.state.current[transactionDeferrable].flag}
}

// ResourcePolicy exposes validated settings and explicit effective host caps.
// Zero timeouts disable the SQL timeout; this layer does not run timers.
// TempFileLimitBytes == -1 means SQL-unlimited, never host-unlimited.
type ResourcePolicy struct {
	StatementTimeout                time.Duration
	LockTimeout                     time.Duration
	IdleInTransactionTimeout        time.Duration
	TransactionTimeout              time.Duration
	WorkMemoryBytes                 int64
	MaintenanceMemoryBytes          int64
	TempFileLimitBytes              int64
	EffectiveWorkMemoryBytes        int64
	EffectiveMaintenanceMemoryBytes int64
	EffectiveTempFileBytes          int64
}

func (s *Settings) Resources() ResourcePolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	temp := s.state.current[tempFileLimit].number
	if temp >= 0 {
		temp *= 1024
	}
	effectiveTemp := s.limits.TemporaryFileBytes
	if temp >= 0 {
		effectiveTemp = min(temp, effectiveTemp)
	}
	work, maintenance := s.state.current[workMem].number*1024, s.state.current[maintenanceWorkMem].number*1024
	return ResourcePolicy{
		StatementTimeout:         time.Duration(s.state.current[statementTimeout].number) * time.Millisecond,
		LockTimeout:              time.Duration(s.state.current[lockTimeout].number) * time.Millisecond,
		IdleInTransactionTimeout: time.Duration(s.state.current[idleInTransactionTimeout].number) * time.Millisecond,
		TransactionTimeout:       time.Duration(s.state.current[transactionTimeout].number) * time.Millisecond,
		WorkMemoryBytes:          work, MaintenanceMemoryBytes: maintenance, TempFileLimitBytes: temp,
		EffectiveWorkMemoryBytes:        min(work, s.limits.WorkMemoryBytes),
		EffectiveMaintenanceMemoryBytes: min(maintenance, s.limits.MaintenanceMemoryBytes),
		EffectiveTempFileBytes:          effectiveTemp,
	}
}

func (s *Settings) TimeZone() Zone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.current[timeZone].zone
}

// SearchPath returns owned logical names, retaining $user and pg_temp for the
// binder to resolve against the same catalog snapshot as the statement. Missing
// schemas are not errors at SET time; pg_catalog/temp precedence is binder-owned.
func (s *Settings) SearchPath() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.searchPathLocked()
}

func (s *Settings) searchPathLocked() []string {
	parts, _ := identifierList(s.state.current[searchPath].text)
	return parts
}

type SettingInfo struct {
	Name     string
	Value    model.Value
	ReadOnly bool
}

// ShowAll returns only the fixed supported inventory, in definition order.
func (s *Settings) ShowAll() []SettingInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]SettingInfo, len(settingDefinitions))
	for id, def := range settingDefinitions {
		result[id] = SettingInfo{Name: def.name, Value: model.StringVal(s.state.current[id].text), ReadOnly: def.kind == readOnlySetting}
	}
	return result
}

// SemanticSettings is an owned invocation snapshot. Canonical enum strings are
// validated on mutation, not reparsed for each expression lane.
type SemanticSettings struct {
	SearchPath                []string
	TimeZone                  Zone
	DateStyle                 string
	IntervalStyle             string
	ExtraFloatDigits          int
	ByteaOutput               string
	StandardConformingStrings bool
	EscapeStringWarning       bool
	BackslashQuote            string
	ClientEncoding            string
	ClientMinMessages         string
	CheckFunctionBodies       bool
	DefaultTextSearchConfig   string
}

func (s *Settings) Semantics() SemanticSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SemanticSettings{
		SearchPath: s.searchPathLocked(), TimeZone: s.state.current[timeZone].zone,
		DateStyle: s.state.current[dateStyle].text, IntervalStyle: s.state.current[intervalStyle].text,
		ExtraFloatDigits: int(s.state.current[extraFloatDigits].number), ByteaOutput: s.state.current[byteaOutput].text,
		StandardConformingStrings: s.state.current[standardConformingStrings].flag,
		EscapeStringWarning:       s.state.current[escapeStringWarning].flag,
		BackslashQuote:            s.state.current[backslashQuote].text, ClientEncoding: s.state.current[clientEncoding].text,
		ClientMinMessages: s.state.current[clientMinMessages].text, CheckFunctionBodies: s.state.current[checkFunctionBodies].flag,
		DefaultTextSearchConfig: s.state.current[defaultTextSearchConfig].text,
	}
}

func (s *Settings) validString(value string) bool {
	return len(value) <= s.limits.MaxValueBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func lookupSetting(name string) (settingID, bool, error) {
	for i := range len(name) {
		if name[i] >= utf8.RuneSelf {
			return 0, false, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
		}
	}
	for id, def := range settingDefinitions {
		if strings.EqualFold(name, def.name) {
			return settingID(id), false, nil
		}
	}
	if err := bind.ValidateSetting(name, 0); err != nil {
		return 0, false, err
	}
	if bind.IsCustomSettingName(name) {
		return 0, true, nil
	}
	return 0, false, settingError("XX000", "required setting has no state definition")
}

func settingError(code, message string) error { return &model.Error{Code: code, Message: message} }
func badSetting(name, value string) error {
	return &model.Error{Code: "22023", Message: fmt.Sprintf("invalid value for parameter %q", name), Detail: value}
}
