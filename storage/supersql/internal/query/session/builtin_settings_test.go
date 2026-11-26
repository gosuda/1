package session

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestBuiltinSettingsCaptureIntervalSignRules(t *testing.T) {
	settings := newTestSettings(t)
	postgres, err := builtin.Resolve(1160, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	requireSetting(t, settings, "IntervalStyle", "sql_standard", SessionScope)
	standard, err := builtin.Resolve(1160, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{model.StringVal("-1 2:03:04"), model.Int64Val(1186), model.Int64Val(-1)}
	oldValue, err := postgres.Call(t.Context(), arguments, builtin.CallBudget{MemoryBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	newValue, err := standard.Call(t.Context(), arguments, builtin.CallBudget{MemoryBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if oldValue.IntervalVal.Days != -1 || oldValue.IntervalVal.Microseconds != 7_384_000_000 {
		t.Fatalf("captured postgres interval=%+v", oldValue.IntervalVal)
	}
	if newValue.IntervalVal.Days != -1 || newValue.IntervalVal.Microseconds != -7_384_000_000 {
		t.Fatalf("captured sql_standard interval=%+v", newValue.IntervalVal)
	}
}

func TestBuiltinSettingsCaptureDateStyleAndOrder(t *testing.T) {
	settings := newTestSettings(t)
	requireSetting(t, settings, "DateStyle", "SQL, DMY", SessionScope)
	first, err := builtin.Resolve(1085, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	requireSetting(t, settings, "DateStyle", "German", SessionScope)
	second, err := builtin.Resolve(1085, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{{Type: model.TypeTimestamp, IntVal: 1}}
	oldValue, err := first.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 10, MemoryBytes: 10})
	if err != nil || oldValue.StrVal != "02/01/2000" {
		t.Fatalf("captured SQL date=%v,%v", oldValue, err)
	}
	newValue, err := second.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 10, MemoryBytes: 10})
	if err != nil || newValue.StrVal != "02.01.2000" {
		t.Fatalf("captured German date=%v,%v", newValue, err)
	}
}

func TestBuiltinSettingsCaptureTimezoneAbbreviationMeaning(t *testing.T) {
	settings := newTestSettings(t)
	requireSetting(t, settings, "TimeZone", "America/New_York", SessionScope)
	first, err := builtin.Resolve(2069, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	requireSetting(t, settings, "TimeZone", "GMT", SessionScope)
	second, err := builtin.Resolve(2069, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{model.StringVal("LMT"), {Type: model.TypeTimestamp}}
	value, err := first.Call(t.Context(), args, builtin.CallBudget{})
	if err != nil || value.IntVal != 17762000000 {
		t.Fatalf("captured New York LMT: %v, %v", value, err)
	}
	_, err = second.Call(t.Context(), args, builtin.CallBudget{})
	requireCode(t, err, "22023")
}

func TestBuiltinSettingsCaptureWideFixedTimezone(t *testing.T) {
	settings := newTestSettings(t)
	requireSetting(t, settings, "TimeZone", "167.5", SessionScope)
	call, err := builtin.Resolve(2069, settings.BuiltinOptions(950, 950))
	if err != nil {
		t.Fatal(err)
	}
	value, err := call.Call(t.Context(), []model.Value{model.StringVal("+167:30"), {Type: model.TypeTimestamp}}, builtin.CallBudget{})
	if err != nil || value.IntVal != -603000000000 {
		t.Fatalf("captured numeric timezone: %v, %v", value, err)
	}
}

func TestTimezoneAbbreviationSettingRemainsExcluded(t *testing.T) {
	settings := newTestSettings(t)
	_, err := settings.Set(t.Context(), "timezone_abbreviations", "Australia", SessionScope)
	requireCode(t, err, "0A000")
}
