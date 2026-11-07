package engine

import "testing"

// An owner that answers a command and dies in the same iteration makes
// both cmd.res and c.done ready. postWait must consume the answer so the
// pooled command never carries a stale result to its next user.
func TestPostWaitDrainsResultOnDeath(t *testing.T) {
	for i := range 2000 {
		c := &Conn{mbox: make(chan connEvent, 1), done: make(chan struct{})}
		cmd := &appCmd{res: make(chan appResult, 1)}
		go func() {
			ev := <-c.mbox
			ev.cmd.res <- appResult{n: 7}
			close(c.done)
		}()
		r := c.postWait(cmd)
		if len(cmd.res) != 0 {
			t.Fatalf("iteration %d: result left buffered in pooled command", i)
		}
		if r.n != 7 {
			t.Fatalf("iteration %d: got n=%d err=%v, want the owner's answer", i, r.n, r.err)
		}
	}
}
