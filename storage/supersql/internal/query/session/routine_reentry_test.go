package session

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

type trackedRoutineReadView struct {
	txn.ReadView
	closed int
}

func (view *trackedRoutineReadView) Close() error {
	view.closed++
	return nil
}

func TestRoutineRetiredFrameClosesAfterLastBorrower(t *testing.T) {
	view := &trackedRoutineReadView{}
	frame := &routineCapabilityFrame{view: view, ownedView: true, borrowers: 2, retired: true}
	executor := &routineExecutor{retiredFrames: []*routineCapabilityFrame{frame}}
	if err := executor.releaseFrame(frame); err != nil {
		t.Fatal(err)
	}
	if view.closed != 0 {
		t.Fatal("retired frame closed while a borrower remained")
	}
	if err := executor.releaseFrame(frame); err != nil {
		t.Fatal(err)
	}
	if view.closed != 1 {
		t.Fatalf("retired view close count = %d", view.closed)
	}
}

func TestRoutineCapabilityCleanupClosesCurrentAndRetiredViews(t *testing.T) {
	currentView := &trackedRoutineReadView{}
	retiredView := &trackedRoutineReadView{}
	executor := &routineExecutor{
		frame:         &routineCapabilityFrame{view: currentView, ownedView: true},
		retiredFrames: []*routineCapabilityFrame{{view: retiredView, ownedView: true, retired: true, borrowers: 1}},
	}
	if err := executor.closeCapabilities(); err != nil {
		t.Fatal(err)
	}
	if err := executor.closeCapabilities(); err != nil {
		t.Fatal(err)
	}
	if currentView.closed != 1 || retiredView.closed != 1 {
		t.Fatalf("view close counts = current %d, retired %d", currentView.closed, retiredView.closed)
	}
}
