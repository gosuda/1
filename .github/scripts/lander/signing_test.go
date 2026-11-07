package lander

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsignedTrunkTipHaltsTheQueueUntilAcknowledged(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	e := newEnv(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "lander_key")
	sh(t, dir, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "lander", "-f", key)
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	signers := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(signers, []byte("lander@gosuda.org "+strings.TrimSpace(string(pub))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.l.Store.Signing = &Signing{KeyFile: key, AllowedSigners: signers}

	// The seeded tip was not written by the lander: the queue halts.
	e.devWork("a", map[string]string{"Q/q.go": "package q\n// a\n"}, "Q: a")
	before := e.trunkRef("refs/suda/main")
	err = e.l.Cycle(e.ctx)
	if !errors.Is(err, ErrHalted) {
		t.Fatalf("Cycle err = %v, want ErrHalted", err)
	}
	if e.trunkRef("refs/suda/main") != before {
		t.Fatal("halted lander wrote to the trunk")
	}

	// Acknowledging that tip resumes; the lander's own commits then verify without help.
	e.l.AckTip = e.trunkRef("refs/suda/history")
	e.cycle()
	if !strings.Contains(e.trunkFile("refs/suda/main", "Q/q.go"), "// a") {
		t.Fatal("landing did not proceed after acknowledgement")
	}
	e.l.AckTip = ""
	e.cycle()

	// An out-of-band push (not signed by the lander key) halts again.
	git(t, e.dev, "fetch", "-q", e.trunk, "+refs/suda/history:refs/remotes/trunk/hist")
	git(t, e.dev, "checkout", "-q", "-B", "oob", "refs/remotes/trunk/hist")
	write(t, e.dev, map[string]string{"Q/oob.go": "package q\n"})
	git(t, e.dev, "add", "-A")
	git(t, e.dev, "commit", "-qm", "out of band")
	git(t, e.dev, "push", "-q", e.trunk, "HEAD:refs/suda/history", "HEAD:refs/suda/main", "--force")
	if err := e.l.Cycle(e.ctx); !errors.Is(err, ErrHalted) {
		t.Fatalf("out-of-band push not detected: %v", err)
	}
}

// Changing the committer identity must not make the existing signed history look foreign: git
// matches the allowed-signers principal against the committer email, so both identities are listed.
func TestSigningKeepsVerifyingAcrossACommitterIdentityChange(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	e := newEnv(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	sh(t, dir, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	pub, _ := os.ReadFile(key + ".pub")
	bot := Identity{Name: "suda-lander[bot]", Email: "350558086+suda-lander[bot]@users.noreply.github.com"}
	signers := filepath.Join(dir, "allowed")
	line := LegacyIdentity.Email + "," + bot.Email + " " + strings.TrimSpace(string(pub)) + "\n"
	if err := os.WriteFile(signers, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	e.l.Store.Signing = &Signing{KeyFile: key, AllowedSigners: signers}
	e.l.AckTip = e.trunkRef("refs/suda/history")

	// First under the legacy identity ...
	e.devWork("a", map[string]string{"Q/q.go": "package q\n// a\n"}, "Q: a")
	e.cycle()
	e.l.AckTip = ""
	e.cycle()
	if got := git(t, e.trunk, "log", "-1", "--format=%ce", "refs/suda/history"); got != LegacyIdentity.Email {
		t.Fatalf("precondition: committer = %q", got)
	}

	// ... then under the bot identity: the old tip still verifies and the new one verifies too.
	e.l.Store.Identity = bot
	e.devWork("b", map[string]string{"Q/q.go": "package q\n// b\n"}, "Q: b")
	e.cycle()
	if got := git(t, e.trunk, "log", "-1", "--format=%ce", "refs/suda/history"); got != bot.Email {
		t.Fatalf("committer after the change = %q", got)
	}
	e.cycle() // verifies the bot-signed tip
}
