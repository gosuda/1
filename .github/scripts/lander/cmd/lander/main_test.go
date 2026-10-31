package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizePEMRepairsMangledEnvValues(t *testing.T) {
	want := "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("A", 64) + "\n" + strings.Repeat("B", 10) + "\n-----END RSA PRIVATE KEY-----\n"
	body := strings.Repeat("A", 64) + strings.Repeat("B", 10)
	cases := map[string]string{
		"proper":         want,
		"literal \\n":    "-----BEGIN RSA PRIVATE KEY-----\\n" + body + "\\n-----END RSA PRIVATE KEY-----",
		"spaces":         "-----BEGIN RSA PRIVATE KEY----- " + body[:64] + " " + body[64:] + " -----END RSA PRIVATE KEY-----",
		"quoted":         "\"" + want + "\"",
		"one line":       "-----BEGIN RSA PRIVATE KEY-----" + body + "-----END RSA PRIVATE KEY-----",
		"crlf":           strings.ReplaceAll(want, "\n", "\r\n"),
		"base64 wrapped": base64.StdEncoding.EncodeToString([]byte(want)),
	}
	for name, in := range cases {
		got, err := normalizePEM(in)
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	_, err := normalizePEM("-----BEGIN RSA PRIVATE KEY----- truncated")
	if err == nil || strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error must describe the shape without echoing content: %v", err)
	}
}

func TestSecretPrefersFileThenValue(t *testing.T) {
	t.Setenv("T_FILE", "")
	t.Setenv("T_VAL", "")
	if _, err := secret("T_FILE", "T_VAL"); err == nil {
		t.Fatal("missing secret must be an error")
	}
	p := filepath.Join(t.TempDir(), "k")
	_ = os.WriteFile(p, []byte("from-file"), 0o600)
	t.Setenv("T_FILE", p)
	t.Setenv("T_VAL", "ignored")
	if got, _ := secret("T_FILE", "T_VAL"); string(got) != "from-file" {
		t.Fatalf("secret = %q", got)
	}
}

func TestSigningDerivesAllowedSignersFromThePrivateKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	priv, _ := os.ReadFile(key)
	sg, err := materializeSigning(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	signers, _ := os.ReadFile(sg.AllowedSigners)
	pub, _ := os.ReadFile(key + ".pub")
	if !strings.HasPrefix(string(signers), "lander@gosuda.org "+strings.Fields(string(pub))[0]+" "+strings.Fields(string(pub))[1]) {
		t.Fatalf("allowed signers = %q", signers)
	}
}

func signedWake(t *testing.T, secret, body string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	r := httptest.NewRequest(http.MethodPost, "/wake", strings.NewReader(body))
	r.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return r
}

func TestWakeHandlerOnlyWakesForWatchedRepositoriesAndRejectsBadSignatures(t *testing.T) {
	wake := make(chan struct{}, 1)
	watched := map[string]bool{"gosuda/1": true, "gosuda/HQQ": true}
	h := wakeHandler("s3cret", wake, func(repo string) bool { return repo == "" || watched[repo] })
	do := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	woken := func() bool {
		select {
		case <-wake:
			return true
		default:
			return false
		}
	}

	bad := signedWake(t, "other", `{"repository":{"full_name":"gosuda/1"}}`)
	if code := do(bad); code != http.StatusUnauthorized || woken() {
		t.Fatalf("bad signature: code %d, woken %v", code, woken())
	}
	if code := do(signedWake(t, "s3cret", `{"repository":{"full_name":"gosuda/unrelated"}}`)); code != http.StatusAccepted || woken() {
		t.Fatal("an unwatched repository must be accepted without waking the lander")
	}
	if code := do(signedWake(t, "s3cret", `{"repository":{"full_name":"gosuda/HQQ"}}`)); code != http.StatusAccepted || !woken() {
		t.Fatal("a mirror repository must wake the lander")
	}
	if code := do(signedWake(t, "s3cret", `{"zen":"ping","hook_id":1}`)); code != http.StatusAccepted || !woken() {
		t.Fatal("payloads without a repository (ping, magpie) must wake the lander")
	}
	// No secret configured: the endpoint does not exist.
	off := wakeHandler("", wake, func(string) bool { return true })
	w := httptest.NewRecorder()
	off.ServeHTTP(w, signedWake(t, "", "{}"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404 without a configured secret", w.Code)
	}
}

func TestEnvBoolIsStrict(t *testing.T) {
	for in, want := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true, "TRUE": true} {
		t.Setenv("T_BOOL", in)
		if got := envBool("T_BOOL"); got != want {
			t.Errorf("envBool(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCheckGitVersionRequiresMergeTreeMergeBase(t *testing.T) {
	for in, ok := range map[string]bool{
		"git version 2.39.5":               false,
		"git version 1.9.1":                false,
		"git version 2.40.0":               true,
		"git version 2.47.3":               true,
		"git version 3.0.0":                true,
		"git version 2.54.0 (Apple Git-1)": true,
	} {
		if err := checkGitVersion(in); (err == nil) != ok {
			t.Errorf("checkGitVersion(%q) = %v, want ok=%v", in, err, ok)
		}
	}
	if checkGitVersion("something else") == nil {
		t.Error("unparsable output must be an error")
	}
}
