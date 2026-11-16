package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// Settings has one owner and is not goroutine-safe. Begin/Commit/Rollback track
// settings only; the orchestrator must compose them with actual transaction
// outcomes. Errors leave settings unchanged; transaction abort handling belongs
// to that orchestrator. No deadlines, storage quotas, or SQL execution run here.
type Settings struct {
	resolver       SettingResolver
	limits         SettingLimits
	reset          settingValues
	current        settingValues
	retained       settingValues
	before         settingValues
	customCurrent  []customSetting
	customRetained []customSetting
	customBefore   []customSetting
	active         bool
	dataAccess     bool
	savepoints     []settingSavepoint
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
	name           string
	current        settingValues
	retained       settingValues
	customCurrent  []customSetting
	customRetained []customSetting
}

func NewSettings(ctx context.Context, opts SettingsOptions) (*Settings, error) {
	invalidStateLimits := opts.Limits.MaxValueBytes < 64 || opts.Limits.MaxSavepoints <= 0
	invalidResourceLimits := opts.Limits.WorkMemoryBytes <= 0 || opts.Limits.MaintenanceMemoryBytes <= 0 || opts.Limits.TemporaryFileBytes <= 0
	if opts.Resolver == nil || invalidStateLimits || invalidResourceLimits {
		return nil, settingError("22023", "settings require a resolver and positive host limits")
	}
	s := &Settings{resolver: opts.Resolver, limits: opts.Limits}
	for id, def := range settingDefinitions {
		value, err := s.parse(ctx, settingID(id), def.initial)
		if err != nil {
			return nil, err
		}
		s.current[id] = value
	}
	s.reset, s.retained = s.current, s.current
	return s, nil
}

func (*Settings) Principal() model.Principal { return model.Principal{} }
func (s *Settings) InTransaction() bool      { return s.active }

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
	if s.active {
		return settingError("25001", "a transaction is already active")
	}
	s.before = s.current
	s.retained = s.current
	s.customBefore = s.customCurrent
	s.customRetained = s.customCurrent
	s.current[transactionIsolation] = s.current[defaultIsolation]
	s.current[transactionReadOnly] = s.current[defaultReadOnly]
	s.current[transactionDeferrable] = s.current[defaultDeferrable]
	if opts.Isolation != "" {
		s.current[transactionIsolation].assigned = true
	}
	if opts.ReadOnly != nil {
		s.current[transactionReadOnly] = booleanValue(*opts.ReadOnly)
		s.current[transactionReadOnly].assigned = true
	}
	if opts.Deferrable != nil {
		s.current[transactionDeferrable] = booleanValue(*opts.Deferrable)
		s.current[transactionDeferrable].assigned = true
	}
	s.active, s.dataAccess = true, false
	return nil
}

// MarkDataAccess is called when the transaction takes its first data snapshot,
// not for SET/SHOW or a statement that fails before data access.
func (s *Settings) MarkDataAccess() error {
	if !s.active {
		return settingError("25P01", "no active transaction")
	}
	s.dataAccess = true
	return nil
}

func (s *Settings) Commit() error {
	if !s.active {
		return settingError("25P01", "no active transaction")
	}
	s.current = s.retained
	s.customCurrent = s.customRetained
	s.finish()
	return nil
}

func (s *Settings) Rollback() error {
	if !s.active {
		return settingError("25P01", "no active transaction")
	}
	s.current = s.before
	s.customCurrent = s.customBefore
	s.finish()
	return nil
}

func (s *Settings) finish() {
	s.current[transactionIsolation] = s.current[defaultIsolation]
	s.current[transactionReadOnly] = s.current[defaultReadOnly]
	s.current[transactionDeferrable] = s.current[defaultDeferrable]
	s.retained = s.current
	s.before = settingValues{}
	s.customRetained = s.customCurrent
	s.customBefore = nil
	clear(s.savepoints)
	s.savepoints = s.savepoints[:0]
	s.active, s.dataAccess = false, false
}

func (s *Settings) Savepoint(name string) error {
	if !s.active {
		return settingError("25P01", "no active transaction")
	}
	if name == "" || !s.validString(name) {
		return settingError("22023", "invalid savepoint name")
	}
	if len(s.savepoints) == s.limits.MaxSavepoints {
		return settingError("54000", "savepoint host limit exceeded")
	}
	s.savepoints = append(s.savepoints, settingSavepoint{
		name:           strings.Clone(name),
		current:        s.current,
		retained:       s.retained,
		customCurrent:  s.customCurrent,
		customRetained: s.customRetained,
	})
	return nil
}

func (s *Settings) RollbackTo(name string) error {
	index, err := s.savepointIndex(name)
	if err != nil {
		return err
	}
	point := s.savepoints[index]
	s.current, s.retained = point.current, point.retained
	s.customCurrent, s.customRetained = point.customCurrent, point.customRetained
	// FirstSnapshotSet is transaction-wide in PostgreSQL; it does not rewind.
	clear(s.savepoints[index+1:])
	s.savepoints = s.savepoints[:index+1]
	return nil
}

func (s *Settings) Release(name string) error {
	index, err := s.savepointIndex(name)
	if err != nil {
		return err
	}
	clear(s.savepoints[index:])
	s.savepoints = s.savepoints[:index]
	return nil
}

func (s *Settings) savepointIndex(name string) (int, error) {
	if !s.active {
		return 0, settingError("25P01", "no active transaction")
	}
	for i := len(s.savepoints) - 1; i >= 0; i-- {
		if s.savepoints[i].name == name {
			return i, nil
		}
	}
	return 0, settingError("3B001", "savepoint does not exist")
}

func (s *Settings) Show(name string) (model.Value, error) { return s.CurrentSetting(name, false) }

func (s *Settings) CurrentSetting(name string, missingOK bool) (model.Value, error) {
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
		for _, entry := range s.customCurrent {
			if entry.name == canonical {
				return model.StringVal(entry.text), nil
			}
		}
		if missingOK {
			return model.NullVal(model.TypeString), nil
		}
		return model.Value{}, &model.Error{Code: "42704", Message: "unrecognized configuration parameter", Detail: name}
	}
	return model.StringVal(s.current[id].text), nil
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
	id, custom, err := lookupSetting(name)
	if err != nil {
		return SettingResult{}, err
	}
	if scope != SessionScope && scope != LocalScope {
		return SettingResult{}, settingError("22023", "invalid setting scope")
	}
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
		if scope == LocalScope && !s.active {
			if warn {
				result.Warning = &model.Error{Code: "25P01", Message: "SET LOCAL can only be used in transaction blocks"}
			}
			return result, nil
		}
		nextCurrent, err := setCustomSetting(s.customCurrent, canonical, text, assigned)
		if err != nil {
			return SettingResult{}, err
		}
		if scope == SessionScope {
			nextRetained, err := setCustomSetting(s.customRetained, canonical, text, assigned)
			if err != nil {
				return SettingResult{}, err
			}
			s.customRetained = nextRetained
		}
		s.customCurrent = nextCurrent
		return result, nil
	}
	if settingDefinitions[id].kind == readOnlySetting {
		return SettingResult{}, settingError("55P02", "parameter cannot be changed")
	}
	value := s.reset[id]
	if raw == nil {
		if isTransactionSetting(id) {
			return SettingResult{}, settingError("0A000", "parameter cannot be reset")
		}
	} else {
		value, err = s.parse(ctx, id, *raw)
		if err != nil {
			return SettingResult{}, err
		}
	}
	value.assigned = raw != nil
	if err := s.checkTransactionChange(id, value); err != nil {
		return SettingResult{}, err
	}
	result := SettingResult{Value: model.StringVal(value.text)}
	if scope == LocalScope && !s.active {
		if warn {
			result.Warning = &model.Error{Code: "25P01", Message: "SET LOCAL can only be used in transaction blocks"}
		}
		return result, nil
	}
	s.current[id] = value
	if scope == SessionScope {
		s.retained[id] = value
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

func (s *Settings) checkTransactionChange(id settingID, value settingValue) error {
	if !s.active {
		return nil
	}
	switch id {
	case transactionReadOnly:
		if s.current[id].flag && !value.flag {
			if s.dataAccess || len(s.savepoints) > 0 {
				return settingError("25001", "cannot set transaction read-write mode after data access or in a subtransaction")
			}
			return settingError("25006", "read-only transaction cannot be upgraded")
		}
	case transactionDeferrable:
		if s.dataAccess || len(s.savepoints) > 0 {
			return settingError("25001", "transaction deferrability must be set before data access and outside subtransactions")
		}
	}
	return nil
}

// ResetAll leaves current transaction characteristics alone (PG GUC_NO_RESET_ALL).
func (s *Settings) ResetAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for id, def := range settingDefinitions {
		if def.kind == readOnlySetting || isTransactionSetting(settingID(id)) {
			continue
		}
		s.current[id], s.retained[id] = s.reset[id], s.reset[id]
	}
	if len(s.customCurrent) > 0 {
		resetCustom := make([]customSetting, len(s.customCurrent))
		for i, entry := range s.customCurrent {
			resetCustom[i] = customSetting{name: entry.name, text: "", assigned: false}
		}
		s.customCurrent = resetCustom
		s.customRetained = resetCustom
	}
	return nil
}

func isTransactionSetting(id settingID) bool {
	return id == transactionIsolation || id == transactionReadOnly || id == transactionDeferrable
}

// TransactionPolicy describes mode, not a constructed dataplane transaction.
type TransactionPolicy struct{ ReadOnly, Deferrable bool }

func (s *Settings) TransactionPolicy() TransactionPolicy {
	readOnly := s.current[transactionReadOnly].flag
	return TransactionPolicy{ReadOnly: readOnly, Deferrable: readOnly && s.current[transactionDeferrable].flag}
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
	temp := s.current[tempFileLimit].number
	if temp >= 0 {
		temp *= 1024
	}
	effectiveTemp := s.limits.TemporaryFileBytes
	if temp >= 0 {
		effectiveTemp = min(temp, effectiveTemp)
	}
	work, maintenance := s.current[workMem].number*1024, s.current[maintenanceWorkMem].number*1024
	return ResourcePolicy{
		StatementTimeout:         time.Duration(s.current[statementTimeout].number) * time.Millisecond,
		LockTimeout:              time.Duration(s.current[lockTimeout].number) * time.Millisecond,
		IdleInTransactionTimeout: time.Duration(s.current[idleInTransactionTimeout].number) * time.Millisecond,
		TransactionTimeout:       time.Duration(s.current[transactionTimeout].number) * time.Millisecond,
		WorkMemoryBytes:          work, MaintenanceMemoryBytes: maintenance, TempFileLimitBytes: temp,
		EffectiveWorkMemoryBytes:        min(work, s.limits.WorkMemoryBytes),
		EffectiveMaintenanceMemoryBytes: min(maintenance, s.limits.MaintenanceMemoryBytes),
		EffectiveTempFileBytes:          effectiveTemp,
	}
}

func (s *Settings) TimeZone() Zone { return s.current[timeZone].zone }

// SearchPath returns owned logical names, retaining $user and pg_temp for the
// binder to resolve against the same catalog snapshot as the statement. Missing
// schemas are not errors at SET time; pg_catalog/temp precedence is binder-owned.
func (s *Settings) SearchPath() []string {
	parts, _ := identifierList(s.current[searchPath].text)
	return parts
}

type SettingInfo struct {
	Name     string
	Value    model.Value
	ReadOnly bool
}

// ShowAll returns only the fixed supported inventory, in definition order.
func (s *Settings) ShowAll() []SettingInfo {
	result := make([]SettingInfo, len(settingDefinitions))
	for id, def := range settingDefinitions {
		result[id] = SettingInfo{Name: def.name, Value: model.StringVal(s.current[id].text), ReadOnly: def.kind == readOnlySetting}
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
	return SemanticSettings{
		SearchPath: s.SearchPath(), TimeZone: s.TimeZone(),
		DateStyle: s.current[dateStyle].text, IntervalStyle: s.current[intervalStyle].text,
		ExtraFloatDigits: int(s.current[extraFloatDigits].number), ByteaOutput: s.current[byteaOutput].text,
		StandardConformingStrings: s.current[standardConformingStrings].flag,
		EscapeStringWarning:       s.current[escapeStringWarning].flag,
		BackslashQuote:            s.current[backslashQuote].text, ClientEncoding: s.current[clientEncoding].text,
		ClientMinMessages: s.current[clientMinMessages].text, CheckFunctionBodies: s.current[checkFunctionBodies].flag,
		DefaultTextSearchConfig: s.current[defaultTextSearchConfig].text,
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
