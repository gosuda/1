package locks

import (
	"errors"
	"testing"
	"time"
)

func TestWaitGraphDiamondDAGLinearTime(t *testing.T) {
	graph, err := NewWaitGraph(DefaultWaitGraphLimits())
	if err != nil {
		t.Fatal(err)
	}

	// Build a 60-node diamond DAG: 30 layers of 2 nodes (WaitOwner 1..60),
	// where every node in layer k waits on both nodes in layer k+1.
	// Populating from the sink layer upward forces each BeginWait to traverse
	// the full downstream DAG, which has 2^29 paths from layer 0 (and 2^30
	// paths from a new root waiting on layer 0).
	const layers = 30
	start := time.Now()
	for layer := layers - 2; layer >= 0; layer-- {
		nextA := WaitOwner(2*(layer+1) + 1)
		nextB := WaitOwner(2*(layer+1) + 2)
		blockers := []WaitOwner{nextA, nextB}
		if err := graph.BeginWait(WaitOwner(2*layer+1), blockers); err != nil {
			t.Fatalf("layer %d node A BeginWait: %v", layer, err)
		}
		if err := graph.BeginWait(WaitOwner(2*layer+2), blockers); err != nil {
			t.Fatalf("layer %d node B BeginWait: %v", layer, err)
		}
	}

	// Non-cyclic reachability search from owner 61 across all 2^30 paths.
	const root = WaitOwner(2*layers + 1)
	if err := graph.BeginWait(root, []WaitOwner{1, 2}); err != nil {
		t.Fatalf("root BeginWait: %v", err)
	}

	// Cycle detection from both sink nodes back to root.
	sinkA := WaitOwner(2*(layers-1) + 1)
	sinkB := WaitOwner(2*(layers-1) + 2)
	if err := graph.BeginWait(sinkA, []WaitOwner{root}); !errors.Is(err, ErrDeadlockDetected) {
		t.Fatalf("sinkA cycle result = %v, want ErrDeadlockDetected", err)
	}
	if err := graph.BeginWait(sinkB, []WaitOwner{root}); !errors.Is(err, ErrDeadlockDetected) {
		t.Fatalf("sinkB cycle result = %v, want ErrDeadlockDetected", err)
	}

	elapsed := time.Since(start)
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("60-node diamond DAG reachability and cycle detection took %v, want < 50ms", elapsed)
	}

	for owner := WaitOwner(1); owner <= root; owner++ {
		graph.EndWait(owner)
	}
	graph.mu.Lock()
	defer graph.mu.Unlock()
	if len(graph.waits) != 0 || graph.edges != 0 || graph.usedBytes != 0 {
		t.Fatalf("retained wait graph state: waits=%d edges=%d bytes=%d", len(graph.waits), graph.edges, graph.usedBytes)
	}
}
