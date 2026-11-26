package session

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/bind"
)

// TestSetFromCurrentCapturesVisibleValueWithScopeAndSavepointSemantics pins the
// SQL SET name FROM CURRENT path separately from the routine-option callback.
// The assigned value is the one visible when the command executes, LOCAL stays
// local, and a savepoint rollback restores the pre-command value.
func TestSetFromCurrentCapturesVisibleValueWithScopeAndSavepointSemantics(t *testing.T) {
	settings := newTestSettings(t)
	transactions := &Transactions{settings: settings}
	requireSetting(t, settings, "application_name", "session", SessionScope)
	requireSuccess(t, settings.Begin(BeginOptions{}))
	requireSetting(t, settings, "application_name", "local-before", LocalScope)
	requireSuccess(t, settings.Savepoint("point"))

	// The capture must observe "local-before", not the session default.
	if _, _, err := transactions.executeSetting(t.Context(), bind.SettingCommand{Action: bind.SetFromCurrentSetting, Name: "application_name", Local: true}); err != nil {
		t.Fatal(err)
	}
	requireShow(t, settings, "application_name", "local-before")

	requireSetting(t, settings, "application_name", "local-after", LocalScope)
	requireShow(t, settings, "application_name", "local-after")
	requireSuccess(t, settings.RollbackTo("point"))
	requireShow(t, settings, "application_name", "local-before")
}

// TestSetFromCurrentUsesExecutionValueType pins that capture reads the typed
// setting text rather than parsing SQL literals; a memory setting keeps its
// canonical unit spelling.
func TestSetFromCurrentUsesExecutionValueType(t *testing.T) {
	settings := newTestSettings(t)
	transactions := &Transactions{settings: settings}
	requireSetting(t, settings, "work_mem", "8192", SessionScope)
	requireShow(t, settings, "work_mem", "8MB")
	if _, _, err := transactions.executeSetting(t.Context(), bind.SettingCommand{Action: bind.SetFromCurrentSetting, Name: "work_mem"}); err != nil {
		t.Fatal(err)
	}
	requireShow(t, settings, "work_mem", "8MB")
}
