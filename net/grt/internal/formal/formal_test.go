package formal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileAndVerifyAllMachines(t *testing.T) {
	machines, reports, err := CompileAndVerifyAll()
	if err != nil {
		t.Fatalf("CompileAndVerifyAll failed: %v", err)
	}
	if len(machines) != 11 || len(reports) != 11 {
		t.Fatalf("expected 11 verified machines, got %d", len(machines))
	}
	totalCells := 0
	totalExplicit := 0
	for _, r := range reports {
		if !r.Deterministic || !r.Total || !r.AbsorbingTerminals || !r.TerminationReachable {
			t.Fatalf("machine %s failed verification flags: %+v", r.MachineName, r)
		}
		if r.ReachableStates != r.NumStates {
			t.Fatalf("machine %s reachable=%d want=%d", r.MachineName, r.ReachableStates, r.NumStates)
		}
		totalCells += r.TotalCells
		totalExplicit += r.ExplicitTransitions
		t.Logf("FSM %-16s states=%2d events=%2d cells=%3d explicit=%2d terminals=%d",
			r.MachineName, r.NumStates, r.NumEvents, r.TotalCells, r.ExplicitTransitions, r.TerminalStates)
	}
	t.Logf("Total verified FSM matrix cells=%d explicit_transitions=%d", totalCells, totalExplicit)
}

func TestVerifyAllComposedModels(t *testing.T) {
	reports, err := VerifyAllComposedModels()
	if err != nil {
		t.Fatalf("VerifyAllComposedModels failed: %v", err)
	}
	if len(reports) != 5 {
		t.Fatalf("expected 5 composed models, got %d", len(reports))
	}
	totalStates := 0
	totalTransitions := 0
	for _, r := range reports {
		if r.ReachableStates == 0 || r.TransitionsChecked == 0 {
			t.Fatalf("model %s explored empty state space: %+v", r.ModelName, r)
		}
		totalStates += r.ReachableStates
		totalTransitions += r.TransitionsChecked
		t.Logf("Composed Model %-22s reachable_states=%5d transitions=%6d invariants=%v",
			r.ModelName, r.ReachableStates, r.TransitionsChecked, r.InvariantsVerified)
	}
	t.Logf("Total composed state-space states=%d transitions=%d", totalStates, totalTransitions)
}

func TestNegativeCheckerDetectsViolations(t *testing.T) {
	// 1. Duplicate non-deterministic transition
	nonDet := cipherSelectionMachine()
	nonDet.Transitions = append(nonDet.Transitions, TransitionSpec{
		From: 0, Event: 0, To: 2, Disp: DispTransition,
	})
	if _, _, err := CompileAndVerifyMachine(nonDet); err == nil || !strings.Contains(err.Error(), "non-deterministic") {
		t.Fatalf("expected non-deterministic error, got %v", err)
	}

	// 2. Unreachable state
	unreach := ecnValidationMachine()
	unreach.Transitions = unreach.Transitions[:1] // drop transitions to BROKEN(2)
	if _, _, err := CompileAndVerifyMachine(unreach); err == nil || !strings.Contains(err.Error(), "unreachable state") {
		t.Fatalf("expected unreachable state error, got %v", err)
	}

	// 3. Non-absorbing terminal state
	nonAbsorb := ecnValidationMachine()
	nonAbsorb.Transitions = append(nonAbsorb.Transitions, TransitionSpec{
		From: 2, Event: 0, To: 1, Disp: DispTransition,
	})
	if _, _, err := CompileAndVerifyMachine(nonAbsorb); err == nil || !strings.Contains(err.Error(), "not absorbing") {
		t.Fatalf("expected non-absorbing terminal error, got %v", err)
	}

	// 4. Livelock trap (cannot reach terminal state)
	livelock := MachineSpec{
		Name:        "TrapMachine",
		GoPrefix:    "fsmTrap",
		StateType:   "uint8",
		Initial:     0,
		DefaultDisp: DispStay,
		States: []StateSpec{
			{ID: 0, Name: "START"},
			{ID: 1, Name: "TRAP"},
			{ID: 2, Name: "DONE", Terminal: true},
		},
		Events: []EventSpec{
			{ID: 0, Name: "EV_STEP"},
			{ID: 1, Name: "EV_FINISH"},
		},
		Transitions: []TransitionSpec{
			{From: 0, Event: 0, To: 1, Disp: DispTransition},
			{From: 0, Event: 1, To: 2, Disp: DispTransition},
			{From: 1, Event: 0, To: 1, Disp: DispStay}, // TRAP has no path to DONE
		},
	}
	if _, _, err := CompileAndVerifyMachine(livelock); err == nil || !strings.Contains(err.Error(), "livelock trap") {
		t.Fatalf("expected livelock trap error, got %v", err)
	}
}

func TestGeneratedArtifactsUpToDate(t *testing.T) {
	wantGo, err := GenerateEngineGo()
	if err != nil {
		t.Fatalf("GenerateEngineGo: %v", err)
	}
	genGoPath := filepath.Join("..", "engine", "z_formal_gen.go")
	gotGo, err := os.ReadFile(genGoPath)
	if err != nil {
		t.Fatalf("read %s: %v", genGoPath, err)
	}
	if !bytes.Equal(gotGo, wantGo) {
		t.Fatalf("%s is out of date with internal/formal IR; run `go run ./cmd/grtgen`", genGoPath)
	}

	wantTLA, wantCfg, err := GenerateTLA()
	if err != nil {
		t.Fatalf("GenerateTLA: %v", err)
	}
	tlaPath := filepath.Join("..", "..", "spec", "formal", "GRT11.tla")
	gotTLA, err := os.ReadFile(tlaPath)
	if err != nil {
		t.Fatalf("read %s: %v", tlaPath, err)
	}
	if string(gotTLA) != wantTLA {
		t.Fatalf("%s is out of date with internal/formal IR; run `go run ./cmd/grtgen`", tlaPath)
	}

	cfgPath := filepath.Join("..", "..", "spec", "formal", "GRT11.cfg")
	gotCfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	if string(gotCfg) != wantCfg {
		t.Fatalf("%s is out of date with internal/formal IR; run `go run ./cmd/grtgen`", cfgPath)
	}
}
