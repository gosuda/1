package session

import (
	"sync"
	"testing"
)

func TestProtectedRoleSettingsRejectPrivilegeEscalation(t *testing.T) {
	s := newTestSettings(t)
	ctx := t.Context()

	// All direct writes via Set, Reset, or SetConfig to role/session identity
	// parameters must fail with SQLSTATE 42501.
	protected := []string{
		"supersql.active_role",
		"SuperSQL.Active_Role",
		"supersql.session_user",
		"SUPERSQL.SESSION_USER",
		"role",
		"ROLE",
		"session_authorization",
		"SESSION_AUTHORIZATION",
	}
	val := "supersql"
	for _, name := range protected {
		if _, err := s.SetConfig(ctx, name, &val, false); err == nil {
			t.Fatalf("SetConfig(%q, false) succeeded, want 42501", name)
		} else {
			requireCode(t, err, "42501")
		}
		if _, err := s.SetConfig(ctx, name, &val, true); err == nil {
			t.Fatalf("SetConfig(%q, true) succeeded, want 42501", name)
		} else {
			requireCode(t, err, "42501")
		}
		if _, err := s.SetConfig(ctx, name, nil, false); err == nil {
			t.Fatalf("SetConfig(%q, nil) succeeded, want 42501", name)
		} else {
			requireCode(t, err, "42501")
		}
		if _, err := s.Set(ctx, name, "supersql", SessionScope); err == nil {
			t.Fatalf("Set(%q) succeeded, want 42501", name)
		} else {
			requireCode(t, err, "42501")
		}
		if _, err := s.Reset(ctx, name, SessionScope); err == nil {
			t.Fatalf("Reset(%q) succeeded, want 42501", name)
		} else {
			requireCode(t, err, "42501")
		}
	}
	if s.CurrentRole() != "" || s.SessionUser() != "" {
		t.Fatalf("rejected writes mutated role state: role=%q session_user=%q", s.CurrentRole(), s.SessionUser())
	}
}

func TestProtectedRoleTransactionalAndSavepointSemantics(t *testing.T) {
	s := newTestSettings(t)
	ctx := t.Context()

	// Initial state.
	if s.CurrentRole() != "" || s.SessionUser() != "" {
		t.Fatalf("initial role=%q session_user=%q", s.CurrentRole(), s.SessionUser())
	}
	requireShow(t, s, "role", "none")
	requireShow(t, s, "session_authorization", "embedded")

	// Session-scoped SetRole and SHOW role / current_setting('role').
	requireSuccess(t, s.SetRole(ctx, "Alice", false))
	if s.CurrentRole() != "alice" {
		t.Fatalf("CurrentRole() = %q, want alice", s.CurrentRole())
	}
	requireShow(t, s, "role", "alice")
	got, err := s.CurrentSetting("role", false)
	requireSuccess(t, err)
	if got.StrVal != "alice" {
		t.Fatalf("current_setting('role') = %q, want alice", got.StrVal)
	}

	// SET LOCAL ROLE outside a transaction is a no-op.
	requireSuccess(t, s.SetRole(ctx, "ephemeral", true))
	if s.CurrentRole() != "alice" {
		t.Fatalf("local SetRole outside tx leaked: %q", s.CurrentRole())
	}

	// BEGIN; SET LOCAL ROLE bob; COMMIT -> reverts to alice.
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSuccess(t, s.SetRole(ctx, "bob", true))
	if s.CurrentRole() != "bob" {
		t.Fatalf("CurrentRole() inside tx = %q, want bob", s.CurrentRole())
	}
	requireShow(t, s, "role", "bob")
	requireSuccess(t, s.Commit())
	if s.CurrentRole() != "alice" {
		t.Fatalf("CurrentRole() after commit of local role = %q, want alice", s.CurrentRole())
	}

	// BEGIN; SET ROLE bob; ROLLBACK -> reverts to alice.
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSuccess(t, s.SetRole(ctx, "bob", false))
	if s.CurrentRole() != "bob" {
		t.Fatalf("CurrentRole() inside tx = %q, want bob", s.CurrentRole())
	}
	requireSuccess(t, s.Rollback())
	if s.CurrentRole() != "alice" {
		t.Fatalf("CurrentRole() after rollback = %q, want alice", s.CurrentRole())
	}

	// SAVEPOINT / ROLLBACK TO SAVEPOINT restores both session and local role changes.
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireSuccess(t, s.SetRole(ctx, "tenant_a", false))
	requireSuccess(t, s.Savepoint("sp1"))
	requireSuccess(t, s.SetRole(ctx, "tenant_b", false))
	requireSuccess(t, s.SetRole(ctx, "tenant_c", true))
	if s.CurrentRole() != "tenant_c" {
		t.Fatalf("CurrentRole() = %q, want tenant_c", s.CurrentRole())
	}
	requireSuccess(t, s.RollbackTo("sp1"))
	if s.CurrentRole() != "tenant_a" {
		t.Fatalf("CurrentRole() after RollbackTo(sp1) = %q, want tenant_a", s.CurrentRole())
	}
	requireSuccess(t, s.Commit())
	if s.CurrentRole() != "tenant_a" {
		t.Fatalf("CurrentRole() after Commit = %q, want tenant_a", s.CurrentRole())
	}

	// SetSessionUser sets session_user and resets currentRole override; ResetRole falls back to session_user.
	requireSuccess(t, s.SetSessionUser(ctx, "tenant_owner", false))
	if s.SessionUser() != "tenant_owner" || s.CurrentRole() != "tenant_owner" {
		t.Fatalf("after SetSessionUser: session=%q current=%q", s.SessionUser(), s.CurrentRole())
	}
	requireShow(t, s, "role", "none")
	requireShow(t, s, "session_authorization", "tenant_owner")

	requireSuccess(t, s.SetRole(ctx, "sub_role", false))
	if s.CurrentRole() != "sub_role" || s.SessionUser() != "tenant_owner" {
		t.Fatalf("after SetRole: session=%q current=%q", s.SessionUser(), s.CurrentRole())
	}
	requireSuccess(t, s.ResetRole(ctx, false))
	if s.CurrentRole() != "tenant_owner" {
		t.Fatalf("after ResetRole: current=%q, want tenant_owner", s.CurrentRole())
	}

	// ResetAll resets role override and custom GUCs, keeps session_user (PG GUC_NO_RESET_ALL).
	requireSetting(t, s, "app.tenant_id", "42", SessionScope)
	requireSuccess(t, s.SetRole(ctx, "sub_role", false))
	requireSuccess(t, s.ResetAll(ctx))
	if s.CurrentRole() != "tenant_owner" || s.SessionUser() != "tenant_owner" {
		t.Fatalf("after ResetAll: session=%q current=%q", s.SessionUser(), s.CurrentRole())
	}

	// ResetAllSessionState resets role, session_user, custom GUCs, and non-default built-ins cleanly.
	requireSetting(t, s, "application_name", "pooled_app", SessionScope)
	requireSetting(t, s, "app.tenant_id", "99", SessionScope)
	requireSuccess(t, s.ResetAllSessionState(ctx))
	if s.CurrentRole() != "" || s.SessionUser() != "" {
		t.Fatalf("after ResetAllSessionState: session=%q current=%q", s.SessionUser(), s.CurrentRole())
	}
	requireShow(t, s, "application_name", "")
	customVal, err := s.CurrentSetting("app.tenant_id", true)
	requireSuccess(t, err)
	if !customVal.IsNull {
		t.Fatalf("custom GUC survived ResetAllSessionState: %+v", customVal)
	}

	// ResetAllSessionState rejects inside an active transaction.
	requireSuccess(t, s.Begin(BeginOptions{}))
	requireCode(t, s.ResetAllSessionState(ctx), "25001")
	requireSuccess(t, s.Rollback())
}

func TestSettingsConcurrentReadsAndRollbackRaceFree(t *testing.T) {
	s := newTestSettings(t)
	ctx := t.Context()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Resources()
					_ = s.CurrentRole()
					_ = s.SessionUser()
					_ = s.TransactionPolicy()
					_ = s.Semantics()
					_, _ = s.CurrentSetting("role", true)
				}
			}
		}()
	}

	for range 100 {
		requireSuccess(t, s.Begin(BeginOptions{}))
		requireSuccess(t, s.SetRole(ctx, "worker", true))
		_, _ = s.Set(ctx, "lock_timeout", "50ms", LocalScope)
		requireSuccess(t, s.Rollback())
	}
	close(stop)
	wg.Wait()
}
