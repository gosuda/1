package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// The state tests supply only the catalog entries they use. This is not a
// production timezone resolver or a timezone conformance test.
type settingsTestResolver struct{}

func (settingsTestResolver) ResolveTimeZone(ctx context.Context, name string) (Zone, error) {
	if err := ctx.Err(); err != nil {
		return Zone{}, err
	}
	if strings.EqualFold(name, "GMT") {
		return Zone{Name: "GMT", Fixed: true}, nil
	}
	if strings.EqualFold(name, "America/New_York") {
		return Zone{Name: "America/New_York"}, nil
	}
	return Zone{}, &model.Error{Code: "22023", Message: "unknown timezone"}
}

func (settingsTestResolver) ResolveTextSearchConfig(ctx context.Context, name string, path []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name == "pg_catalog.simple" {
		return name, nil
	}
	if name == "words" {
		for _, schema := range path {
			if schema == "docs" {
				return "docs.words", nil
			}
		}
	}
	return "", &model.Error{Code: "42704", Message: "text search configuration does not exist"}
}
func (settingsTestResolver) ResolveFormatLocale(ctx context.Context, numeric, monetary, clock string) (model.FormatLocale, error) {
	if err := ctx.Err(); err != nil {
		return model.FormatLocale{}, err
	}
	if !strings.EqualFold(numeric, "C") || !strings.EqualFold(monetary, "C") || !strings.EqualFold(clock, "C") {
		return model.FormatLocale{}, &model.Error{Code: "22023", Message: "unknown locale"}
	}
	return model.FormatLocale{
		NumericName: "C", MonetaryName: "C", TimeName: "C",
		DecimalPoint: ".", ThousandsSeparator: ",", PositiveSign: "+", NegativeSign: "-", CurrencySymbol: " ", ISOCurrencySymbol: " ",
		MonthNames:            [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"},
		AbbreviatedMonthNames: [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
		DayNames:              [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
		AbbreviatedDayNames:   [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"},
	}, nil
}

func newTestSettings(t *testing.T) *Settings {
	t.Helper()
	s, err := NewSettings(t.Context(), SettingsOptions{
		Resolver: settingsTestResolver{},
		Limits:   SettingLimits{MaxValueBytes: 4096, MaxSavepoints: 4, WorkMemoryBytes: 8 << 20, MaintenanceMemoryBytes: 32 << 20, TemporaryFileBytes: 64 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireSetting(t *testing.T, s *Settings, name, value string, scope SettingScope) {
	t.Helper()
	if _, err := s.Set(t.Context(), name, value, scope); err != nil {
		t.Fatalf("SET %s=%q: %v", name, value, err)
	}
}

func requireShow(t *testing.T, s *Settings, name, want string) {
	t.Helper()
	got, err := s.Show(name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != model.TypeString || got.IsNull || got.StrVal != want {
		t.Fatalf("SHOW %s = %v, want %q", name, got, want)
	}
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var state interface{ SQLState() string }
	if !errors.As(err, &state) || state.SQLState() != code {
		t.Fatalf("error %v, want SQLSTATE %s", err, code)
	}
}

func requireSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitRetainsSetAndDiscardsLocal(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "application_name", "before", SessionScope)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "application_name", "retained", SessionScope)
	requireSetting(t, s, "application_name", "local", LocalScope)
	requireShow(t, s, "application_name", "local")
	requireSuccess(t, s.Commit())
	requireShow(t, s, "application_name", "retained")

	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "application_name", "masked", LocalScope)
	requireSetting(t, s, "application_name", "last set", SessionScope)
	requireShow(t, s, "application_name", "last set")
	requireSuccess(t, s.Commit())
	requireShow(t, s, "application_name", "last set")
}

func TestRollbackRestoresSettingsBeforeTransaction(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "work_mem", "3MB", SessionScope)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "work_mem", "6MB", SessionScope)
	requireSetting(t, s, "work_mem", "9MB", LocalScope)
	requireSuccess(t, s.Rollback())
	requireShow(t, s, "work_mem", "3MB")
}

func TestSavepointRollbackRestoresMaskedSetAndEarlierSavepoints(t *testing.T) {
	s := newTestSettings(t)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "application_name", "set", SessionScope)
	requireSetting(t, s, "application_name", "local", LocalScope)
	requireSuccess(t, s.Savepoint("first"))
	requireSetting(t, s, "application_name", "child set", SessionScope)
	requireSuccess(t, s.Savepoint("second"))
	requireSetting(t, s, "application_name", "deep local", LocalScope)
	requireSuccess(t, s.RollbackTo("second"))
	requireShow(t, s, "application_name", "child set")
	requireSuccess(t, s.RollbackTo("first"))
	requireShow(t, s, "application_name", "local")
	requireCode(t, s.Release("second"), "3B001")
	requireSuccess(t, s.RollbackTo("first"))
	requireSuccess(t, s.Commit())
	requireShow(t, s, "application_name", "set")
}

func TestReleasePreservesChangesAndDuplicateNamesUncoverEarlierPoint(t *testing.T) {
	s := newTestSettings(t)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSuccess(t, s.Savepoint("same"))
	requireSetting(t, s, "application_name", "parent", SessionScope)
	requireSuccess(t, s.Savepoint("same"))
	requireSetting(t, s, "application_name", "child", SessionScope)
	requireSuccess(t, s.Release("same"))
	requireShow(t, s, "application_name", "child")
	requireSuccess(t, s.RollbackTo("same"))
	requireShow(t, s, "application_name", "")
}

func TestWeakIsolationRejectedBeforeBeginOrSettingMutation(t *testing.T) {
	s := newTestSettings(t)
	for _, level := range []string{"read committed", "read uncommitted", "repeatable read"} {
		requireCode(t, s.Begin(BeginOptions{Isolation: level}), "0A000")
		if s.InTransaction() {
			t.Fatal("rejected BEGIN opened transaction")
		}
		_, err := s.Set(t.Context(), "default_transaction_isolation", level, SessionScope)
		requireCode(t, err, "0A000")
		requireShow(t, s, "default_transaction_isolation", "serializable")
	}
	requireSuccess(t, s.Begin(BeginOptions{Isolation: "SERIALIZABLE"}))
	requireSuccess(t, s.MarkDataAccess())
	requireSuccess(t, s.Savepoint("after read"))
	requireSetting(t, s, "transaction_isolation", "SERIALIZABLE", LocalScope)
	_, err := s.Set(t.Context(), "transaction_isolation", "repeatable read", SessionScope)
	requireCode(t, err, "0A000")
	requireShow(t, s, "transaction_isolation", "serializable")
}

func TestReadOnlyCannotUpgradeAndDeferrableDoesNotGrantWrites(t *testing.T) {
	s := newTestSettings(t)
	readOnly, deferrable := true, true
	requireSuccess(t, s.Begin(BeginOptions{ReadOnly: &readOnly, Deferrable: &deferrable}))
	_, err := s.Set(t.Context(), "transaction_read_only", "off", SessionScope)
	requireCode(t, err, "25006")
	if !s.TransactionPolicy().ReadOnly || !s.TransactionPolicy().Deferrable {
		t.Fatal("read-only deferrable transaction lost its mode")
	}
	requireSuccess(t, s.MarkDataAccess())
	_, err = s.Set(t.Context(), "transaction_deferrable", "on", LocalScope)
	requireCode(t, err, "25001")
	_, err = s.Set(t.Context(), "transaction_read_only", "off", SessionScope)
	requireCode(t, err, "25001")
	requireSuccess(t, s.Rollback())
	requireSuccess(t, s.Begin(BeginOptions{Deferrable: &deferrable}))
	if s.TransactionPolicy().ReadOnly || s.TransactionPolicy().Deferrable {
		t.Fatal("DEFERRABLE changed writable transaction semantics")
	}
}

func TestSnapshotRestrictionSurvivesSavepointRollback(t *testing.T) {
	s := newTestSettings(t)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSuccess(t, s.Savepoint("before read"))
	requireSuccess(t, s.MarkDataAccess())
	requireSuccess(t, s.RollbackTo("before read"))
	requireSuccess(t, s.Release("before read"))
	_, err := s.Set(t.Context(), "transaction_deferrable", "on", SessionScope)
	requireCode(t, err, "25001")
}

func TestResetAllPreservesTransactionModeAndIsTransactional(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "application_name", "before", SessionScope)
	readOnly := true
	requireSuccess(t, s.Begin(BeginOptions{ReadOnly: &readOnly}))
	requireSuccess(t, s.ResetAll(t.Context()))
	requireShow(t, s, "application_name", "")
	requireShow(t, s, "transaction_read_only", "on")
	_, err := s.Reset(t.Context(), "transaction_read_only", SessionScope)
	requireCode(t, err, "0A000")
	requireSuccess(t, s.Rollback())
	requireShow(t, s, "application_name", "before")
}

func TestMissingSettingNullDoesNotHideExcludedSettings(t *testing.T) {
	s := newTestSettings(t)
	missing, err := s.CurrentSetting("not_a_guc", true)
	requireSuccess(t, err)
	if missing.Type != model.TypeString || !missing.IsNull {
		t.Fatalf("missing setting = %v, want typed text NULL", missing)
	}
	_, err = s.Show("not_a_guc")
	requireCode(t, err, "42704")
	for _, name := range []string{"shared_buffers", "plan_cache_mode", "idle_session_timeout", "session_authorization"} {
		_, err = s.CurrentSetting(name, true)
		requireCode(t, err, "0A000")
		_, err = s.Set(t.Context(), name, "on", SessionScope)
		requireCode(t, err, "0A000")
	}
	_, err = s.Set(t.Context(), "server_encoding", "UTF8", SessionScope)
	requireCode(t, err, "55P02")
	requireShow(t, s, "server_encoding", "UTF8")
	if s.Principal().Name() != "embedded" {
		t.Fatal("unexpected principal")
	}
}

func TestLocalOutsideTransactionExpiresAndSetConfigNullResets(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "application_name", "session", SessionScope)
	result, err := s.Set(t.Context(), "application_name", "local", LocalScope)
	requireSuccess(t, err)
	if result.Warning == nil || result.Warning.Code != "25P01" {
		t.Fatalf("missing SET LOCAL warning: %+v", result)
	}
	requireShow(t, s, "application_name", "session")
	value := "function local"
	got, err := s.SetConfig(t.Context(), "application_name", &value, true)
	requireSuccess(t, err)
	if got.StrVal != value {
		t.Fatalf("set_config result %q, want %q", got.StrVal, value)
	}
	requireShow(t, s, "application_name", "session")
	_, err = s.SetConfig(t.Context(), "application_name", nil, false)
	requireSuccess(t, err)
	requireShow(t, s, "application_name", "")
}

func TestUnitsRoundLikePostgresAndRespectHostCaps(t *testing.T) {
	s := newTestSettings(t)
	for _, test := range []struct {
		value, shown string
		duration     time.Duration
	}{
		{"1.5s", "1500ms", 1500 * time.Millisecond},
		{"2.5ms", "2ms", 2 * time.Millisecond},
		{".5s", "500ms", 500 * time.Millisecond},
		{"500us", "0", 0},
		{"1.01min", "61s", 61 * time.Second},
		{"0x10", "16ms", 16 * time.Millisecond},
		{"010", "8ms", 8 * time.Millisecond},
		{"2147483647ms", "2147483647ms", 2147483647 * time.Millisecond},
	} {
		t.Run(test.value, func(t *testing.T) {
			requireSetting(t, s, "statement_timeout", test.value, SessionScope)
			requireShow(t, s, "statement_timeout", test.shown)
			if got := s.Resources().StatementTimeout; got != test.duration {
				t.Fatalf("timeout %v, want %v", got, test.duration)
			}
		})
	}
	requireSetting(t, s, "work_mem", "30.1GB", SessionScope)
	requireShow(t, s, "work_mem", "30822MB")
	if got := s.Resources().EffectiveWorkMemoryBytes; got != 8<<20 {
		t.Fatalf("effective work memory %d, want 8MiB", got)
	}
	requireSetting(t, s, "maintenance_work_mem", "2147483647kB", SessionScope)
	if got := s.Resources().EffectiveMaintenanceMemoryBytes; got != 32<<20 {
		t.Fatalf("effective maintenance memory %d, want 32MiB", got)
	}
	requireSetting(t, s, "temp_file_limit", "-1", SessionScope)
	if policy := s.Resources(); policy.TempFileLimitBytes != -1 || policy.EffectiveTempFileBytes != 64<<20 {
		t.Fatalf("unlimited SQL policy lost host cap: %+v", policy)
	}
	requireSetting(t, s, "temp_file_limit", "0", SessionScope)
	if s.Resources().EffectiveTempFileBytes != 0 {
		t.Fatal("zero temp limit must forbid spill")
	}
}

func TestNumericAndEnumBoundariesRejectWithoutMutation(t *testing.T) {
	for _, test := range []struct{ name, value string }{
		{"work_mem", "63kB"}, {"work_mem", "2TB"}, {"maintenance_work_mem", "-1"},
		{"temp_file_limit", "-2"}, {"statement_timeout", "2147483648"}, {"lock_timeout", "-1"},
		{"transaction_timeout", "1MS"}, {"idle_in_transaction_session_timeout", "1h garbage"},
		{"extra_float_digits", "4"}, {"extra_float_digits", "-16"}, {"extra_float_digits", "1s"},
		{"extra_float_digits", "09"}, {"extra_float_digits", "NaN"},
		{"bytea_output", "h"}, {"IntervalStyle", "postgresql"}, {"client_min_messages", "fatal"},
		{"backslash_quote", "t"}, {"check_function_bodies", "o"}, {"escape_string_warning", " on "},
	} {
		t.Run(test.name+"/"+test.value, func(t *testing.T) {
			s := newTestSettings(t)
			before, err := s.Show(test.name)
			requireSuccess(t, err)
			_, err = s.Set(t.Context(), test.name, test.value, SessionScope)
			requireCode(t, err, "22023")
			requireShow(t, s, test.name, before.StrVal)
		})
	}
}

func TestBooleanAliasesEnumsAndUTF8OnlyEncoding(t *testing.T) {
	s := newTestSettings(t)
	for _, value := range []string{"t", "TRU", "yes", "y", "on", "1"} {
		requireSetting(t, s, "standard_conforming_strings", value, SessionScope)
		requireShow(t, s, "standard_conforming_strings", "on")
	}
	for _, value := range []string{"f", "FALS", "no", "n", "of", "0"} {
		requireSetting(t, s, "escape_string_warning", value, SessionScope)
		requireShow(t, s, "escape_string_warning", "off")
	}
	requireSetting(t, s, "backslash_quote", "YES", SessionScope)
	requireShow(t, s, "backslash_quote", "on")
	requireSetting(t, s, "client_min_messages", "debug", SessionScope)
	requireShow(t, s, "client_min_messages", "debug2")
	for _, value := range []string{"UTF-8", "unicode", "Utf_8"} {
		requireSetting(t, s, "client_encoding", value, SessionScope)
	}
	_, err := s.Set(t.Context(), "client_encoding", "LATIN1", SessionScope)
	requireCode(t, err, "0A000")
	_, err = s.Set(t.Context(), "client_encoding", "unknown", SessionScope)
	requireCode(t, err, "22023")
	requireShow(t, s, "client_encoding", "UTF8")
}

func TestDateStylePartialUpdatesDefaultsAndConflict(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "DateStyle", "German", SessionScope)
	requireShow(t, s, "DateStyle", "German, DMY")
	requireSetting(t, s, "DateStyle", "ISO", SessionScope)
	requireShow(t, s, "DateStyle", "ISO, DMY")
	requireSetting(t, s, "DateStyle", "DEFAULT, Postgres", SessionScope)
	requireShow(t, s, "DateStyle", "Postgres, MDY")
	requireSetting(t, s, "DateStyle", "european, German", SessionScope)
	requireShow(t, s, "DateStyle", "German, DMY")
	for _, value := range []string{"ISO, SQL", "DMY, MDY", "garbage", "ISO,"} {
		_, err := s.Set(t.Context(), "DateStyle", value, SessionScope)
		requireCode(t, err, "22023")
	}
	requireShow(t, s, "DateStyle", "German, DMY")
}

func TestSearchPathRetainsLogicalNamesAndResolvesTextSearchAtSet(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "search_path", `"$user", MISSING, "MiX""ed", docs, ""`, SessionScope)
	want := []string{"$user", "missing", `MiX"ed`, "docs", ""}
	if got := s.SearchPath(); !reflect.DeepEqual(got, want) {
		t.Fatalf("path %q, want %q", got, want)
	}
	requireSetting(t, s, "default_text_search_config", "words", SessionScope)
	requireShow(t, s, "default_text_search_config", "docs.words")
	requireSetting(t, s, "search_path", "public", SessionScope)
	_, err := s.Set(t.Context(), "default_text_search_config", "words", SessionScope)
	requireCode(t, err, "22023")
	requireShow(t, s, "default_text_search_config", "docs.words")
}

func TestFormatLocaleSettingsUseResolverIdentity(t *testing.T) {
	s := newTestSettings(t)
	for _, name := range []string{"lc_monetary", "lc_numeric", "lc_time"} {
		requireShow(t, s, name, "C")
		requireSetting(t, s, name, "c", SessionScope)
		requireShow(t, s, name, "C")
		_, err := s.Set(t.Context(), name, "missing", SessionScope)
		requireCode(t, err, "22023")
		requireShow(t, s, name, "C")
	}
}

func TestFixedTimezoneUsesSQLSignTruncationAndTZCodeRange(t *testing.T) {
	s := newTestSettings(t)
	for _, test := range []struct {
		input, shown string
		seconds      int32
	}{
		{"5.5", "<+05:30>-05:30", 19800},
		{"-3.25", "<-03:15>+03:15", -11700},
		{"0.0005", "<+00:00:01>-00:00:01", 1},
		{"167.9999", "<+167:59:59>-167:59:59", 604799},
	} {
		requireSetting(t, s, "TimeZone", test.input, SessionScope)
		requireShow(t, s, "TimeZone", test.shown)
		zone := s.TimeZone()
		if !zone.Fixed || zone.OffsetSeconds != test.seconds {
			t.Fatalf("timezone %q has offset %d, want %d", test.input, zone.OffsetSeconds, test.seconds)
		}
	}
	_, err := s.Set(t.Context(), "TimeZone", "168", SessionScope)
	requireCode(t, err, "22023")
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "TimeZone", "america/new_york", LocalScope)
	requireShow(t, s, "TimeZone", "America/New_York")
	requireSuccess(t, s.Rollback())
	requireShow(t, s, "TimeZone", "<+167:59:59>-167:59:59")
}

func TestCancellationDoesNotChangeSettings(t *testing.T) {
	s := newTestSettings(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Set(ctx, "application_name", "cancelled", SessionScope)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	requireShow(t, s, "application_name", "")
}

func TestApplicationNameClipsUTF8BeforeEscapingUnsafeBytes(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "application_name", strings.Repeat("a", 62)+"é", SessionScope)
	requireShow(t, s, "application_name", strings.Repeat("a", 62))
	requireSetting(t, s, "application_name", "a\né", SessionScope)
	requireShow(t, s, "application_name", `a\x0a\xc3\xa9`)
}

func TestSavepointHostLimitDoesNotDestroyUsableStack(t *testing.T) {
	s := newTestSettings(t)
	requireSuccess(t, s.Begin(BeginOptions{}))
	for _, name := range []string{"a", "b", "c", "d"} {
		requireSuccess(t, s.Savepoint(name))
	}
	requireCode(t, s.Savepoint("excess"), "54000")
	requireSetting(t, s, "application_name", "after limit", SessionScope)
	requireSuccess(t, s.RollbackTo("a"))
	requireShow(t, s, "application_name", "")
	requireSuccess(t, s.Savepoint("replacement"))
	requireSuccess(t, s.Commit())
}

func TestTransactionDefaultsApplyOnlyAtNextBegin(t *testing.T) {
	s := newTestSettings(t)
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "default_transaction_read_only", "on", SessionScope)
	requireSetting(t, s, "default_transaction_deferrable", "on", SessionScope)
	if s.TransactionPolicy().ReadOnly {
		t.Fatal("default mutation changed active transaction")
	}
	requireSuccess(t, s.Commit())
	requireSuccess(t, s.Begin(BeginOptions{}))
	if policy := s.TransactionPolicy(); !policy.ReadOnly || !policy.Deferrable {
		t.Fatalf("next transaction did not inherit defaults: %+v", policy)
	}
	requireSetting(t, s, "default_transaction_read_only", "off", LocalScope)
	requireSuccess(t, s.Commit())
	requireSuccess(t, s.Begin(BeginOptions{}))
	if !s.TransactionPolicy().ReadOnly {
		t.Fatal("LOCAL default escaped its transaction")
	}
}

func TestResourceMinimumAppliesAfterUnitRounding(t *testing.T) {
	s := newTestSettings(t)
	requireSetting(t, s, "work_mem", "65535B", SessionScope)
	if s.Resources().WorkMemoryBytes != 65536 {
		t.Fatal("byte input did not round to the minimum kB unit")
	}
	requireSetting(t, s, "lock_timeout", "2s", SessionScope)
	requireSetting(t, s, "idle_in_transaction_session_timeout", "3min", SessionScope)
	requireSetting(t, s, "transaction_timeout", "1h", SessionScope)
	policy := s.Resources()
	if policy.LockTimeout != 2*time.Second || policy.IdleInTransactionTimeout != 3*time.Minute || policy.TransactionTimeout != time.Hour {
		t.Fatalf("duration policies: %+v", policy)
	}
}

func TestCustomGUCPrefixParameters(t *testing.T) {
	ctx := t.Context()
	s := newTestSettings(t)

	// Unassigned custom GUC: SHOW and current_setting(..., false) fail with 42704;
	// current_setting(..., true) returns NULL.
	_, err := s.Show("nosuch.setting")
	requireCode(t, err, "42704")
	_, err = s.CurrentSetting("nosuch.setting", false)
	requireCode(t, err, "42704")
	val, err := s.CurrentSetting("nosuch.setting", true)
	requireSuccess(t, err)
	if !val.IsNull {
		t.Fatalf("expected NULL for unassigned custom GUC with missingOK=true, got %+v", val)
	}

	// SET nosuch.setting = 'nada'
	requireSetting(t, s, "nosuch.setting", "nada", SessionScope)
	requireShow(t, s, "nosuch.setting", "nada")
	val, err = s.CurrentSetting("nosuch.setting", true)
	requireSuccess(t, err)
	if val.IsNull || val.StrVal != "nada" {
		t.Fatalf("expected 'nada', got %+v", val)
	}

	// RESET custom.my_guc makes it go to empty string, not unknown.
	requireSetting(t, s, "custom.my_guc", "42", SessionScope)
	requireShow(t, s, "custom.my_guc", "42")
	_, err = s.Reset(ctx, "custom.my_guc", SessionScope)
	requireSuccess(t, err)
	requireShow(t, s, "custom.my_guc", "")

	// Multi-part qualified custom GUC and SET LOCAL / SAVEPOINT / COMMIT / ROLLBACK
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSetting(t, s, "app.tenant_id", "tenant_a", LocalScope)
	requireShow(t, s, "app.tenant_id", "tenant_a")
	requireSuccess(t, s.Savepoint("sp1"))
	requireSetting(t, s, "app.tenant_id", "tenant_b", LocalScope)
	requireShow(t, s, "app.tenant_id", "tenant_b")
	requireSuccess(t, s.RollbackTo("sp1"))
	requireShow(t, s, "app.tenant_id", "tenant_a")
	requireSuccess(t, s.Commit())

	// After COMMIT of SET LOCAL on a previously unset custom GUC, it is unassigned again.
	val, err = s.CurrentSetting("app.tenant_id", true)
	requireSuccess(t, err)
	if !val.IsNull {
		t.Fatalf("expected NULL after SET LOCAL commit, got %+v", val)
	}

	// Invalid custom names with non-identifier characters fail with 42602 on SET
	// and 42704 on SHOW / current_setting(..., false).
	_, err = s.Set(ctx, "custom.bad-guc", "42", SessionScope)
	requireCode(t, err, "42602")
	_, err = s.Show("custom.bad-guc")
	requireCode(t, err, "42704")
}

