package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeMessages(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	fixture, err := os.ReadFile("testdata/status.json")
	if err != nil {
		t.Fatal(err)
	}
	write("status", string(fixture))
	write("path", filepath.Join(bin, "herd-msg"))
	write("machines", `[{"id":"peer","label":"Peer","target":"fake-peer","enabled":true},{"id":"down","label":"Away","target":"fake-down","enabled":true},{"id":"disabled","label":"Disabled","target":"fake-disabled","enabled":false}]`)
	write("bin/herdr", "#!/bin/sh\ncat \"$FAKE_ROOT/machines\"\n")
	write("bin/herd-msg", `#!/bin/sh
printf 'call\n' >> "$FAKE_ROOT/args"
for arg do printf '%s\n' "$arg" >> "$FAKE_ROOT/args"; done
if [ -n "$CODEX_THREAD_ID$CLAUDE_CODE_SESSION_ID" ]; then echo 'agent identity leaked' >&2; exit 2; fi
case "$1" in
 status) cat "$FAKE_ROOT/status" ;;
 show) if [ "$2" = gone ]; then printf '{"error":"message gone is missing or expired"}'; exit 1; fi
       printf '{"id":"pending","state":"queued","body":"full body","attempts":[],"history":[]}' ;;
 release) printf '{"error":"only held mail can be released"}'; exit 2 ;;
 resolve) if [ "$6" != '2030-01-01T00:00:00Z' ]; then printf '{"error":"message changed; refresh before resolving"}'; exit 2; fi ;;
 prune) if [ "$6" != 1 ]; then printf '{"error":"unsent count changed"}'; exit 2; fi ;;
 *) printf '{"ok":true}' ;;
esac
`)
	write("bin/ssh", `#!/bin/sh
for last do :; done
printf '%s\n' "$last" >> "$FAKE_ROOT/ssh-args"
case "$*" in *fake-down*) echo 'connection refused' >&2; exit 255;; esac
case "$last" in
 'command -v herd-msg'*) printf 'lookup\n' >> "$FAKE_ROOT/lookups"; cat "$FAKE_ROOT/path" ;;
 *) /bin/sh -c "$last" ;;
esac
`)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_ROOT", root)
	t.Setenv("CODEX_THREAD_ID", "agent-thread")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "agent-conversation")
	machinesCache.Lock()
	machinesCache.list = nil
	machinesCache.at = time.Time{}
	machinesCache.Unlock()
	messagePaths = sync.Map{}
	t.Cleanup(func() {
		machinesCache.Lock()
		machinesCache.list = nil
		machinesCache.at = time.Time{}
		machinesCache.Unlock()
		messagePaths = sync.Map{}
	})
	return root
}

func messageRequest(t *testing.T, method, path, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/messages", requireAuth(handleMessages))
	mux.HandleFunc("/api/messages/", requireAuth(handleMessage))
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth {
		r.AddCookie(&http.Cookie{Name: cookieName, Value: signToken(time.Now().Add(time.Hour).Unix())})
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestMessagesContractAndPartialFailure(t *testing.T) {
	fakeMessages(t)
	w := messageRequest(t, "GET", "/api/messages", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var result struct {
		Machines []MessageMachine `json:"machines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Machines) != 3 {
		t.Fatal(w.Body)
	}
	for _, row := range result.Machines[:2] {
		if row.Status == nil || row.Status.Schema != 1 || len(row.Status.Pending) != 1 || row.Status.Pending[0].FirstLine != "hello" || !row.Status.Sessions[0].Blocked {
			t.Fatal(row)
		}
	}
	if result.Machines[2].Error == "" || result.Machines[2].Problem != "unavailable" {
		t.Fatal(result.Machines[2])
	}
}

func TestMessageGone(t *testing.T) {
	fakeMessages(t)
	w := messageRequest(t, "GET", "/api/messages/local/gone", "", true)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "missing or expired") {
		t.Fatal(w.Code, w.Body)
	}
}

func TestMessageAuthenticationAndMethods(t *testing.T) {
	root := fakeMessages(t)
	for _, tc := range []struct {
		method, path, body string
		auth               bool
		code               int
	}{
		{"GET", "/api/messages", "", false, 401},
		{"GET", "/api/messages/local/pending", "", false, 401},
		{"POST", "/api/messages/local/action", `{"action":"kill","value":"on","confirm":true}`, false, 401},
		{"POST", "/api/messages", "", true, 405},
		{"GET", "/api/messages/local/action", "", true, 405},
		{"POST", "/api/messages/local/pending", "", true, 405},
	} {
		if w := messageRequest(t, tc.method, tc.path, tc.body, tc.auth); w.Code != tc.code {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "args")); !os.IsNotExist(err) {
		t.Fatal("rejected request executed herd-msg")
	}
}

func TestMessageActionsValidationAndStaleRequests(t *testing.T) {
	root := fakeMessages(t)
	for _, body := range []string{
		`{"action":"kill","value":"on"}`,
		`{"action":"kill","value":"on; touch marker","confirm":true}`,
		`{"action":"resolve","id":"pending","value":"resend","updated":"2030-01-01T00:00:00Z"}`,
		`{"action":"resolve","id":"pending","value":"resend","confirm":true,"updated":"$(id)"}`,
		`{"action":"prune","id":"receiver","confirm":true}`,
		`{"action":"prune","id":"receiver","confirm":true,"unsent":-1}`,
		`{"action":"allow","id":"--help","value":"off","confirm":true}`,
		`{"action":"release","id":"pending;touch marker"}`,
		`{"action":"release","id":"pending\nkill"}`,
		`{"action":"unknown","id":"pending"}`,
	} {
		if w := messageRequest(t, "POST", "/api/messages/local/action", body, true); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "args")); !os.IsNotExist(err) {
		t.Fatal("invalid action executed herd-msg")
	}
	for _, tc := range []struct {
		body   string
		code   int
		detail string
	}{
		{`{"action":"kill","value":"on","confirm":true}`, 200, ""},
		{`{"action":"allow","id":"receiver","value":"off","confirm":true}`, 200, ""},
		{`{"action":"release","id":"pending"}`, 502, "only held"},
		{`{"action":"resolve","id":"pending","value":"resend","confirm":true,"updated":"2030-01-01T00:00:00Z"}`, 200, ""},
		{`{"action":"resolve","id":"pending","value":"resend","confirm":true,"updated":"2029-12-31T19:00:00-05:00"}`, 200, ""},
		{`{"action":"resolve","id":"pending","value":"drop","confirm":true,"updated":"2030-01-02T00:00:00Z"}`, 502, "message changed"},
		{`{"action":"prune","id":"receiver","confirm":true,"unsent":1}`, 200, ""},
		{`{"action":"prune","id":"receiver","confirm":true,"unsent":2}`, 502, "unsent count changed"},
	} {
		w := messageRequest(t, "POST", "/api/messages/local/action", tc.body, true)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.detail) {
			t.Fatal(tc, w.Code, w.Body)
		}
	}
	args, _ := os.ReadFile(filepath.Join(root, "args"))
	for _, flag := range []string{"--if-updated", "--expect-unsent", "--confirm"} {
		if !strings.Contains(string(args), flag) {
			t.Fatal("missing guard", flag)
		}
	}
}

func TestRemoteMessageQuotingAndPathCache(t *testing.T) {
	root := fakeMessages(t)
	// A remote home path may contain spaces. Every argument still arrives intact.
	path := filepath.Join(root, "remote tools")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.ReadFile(filepath.Join(root, "bin/herd-msg"))
	if err := os.WriteFile(filepath.Join(path, "herd-msg"), bin, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "path"), []byte(filepath.Join(path, "herd-msg")), 0600)
	for i := 0; i < 2; i++ {
		w := messageRequest(t, "GET", "/api/messages/peer/pending", "", true)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "full body") {
			t.Fatal(w.Code, w.Body)
		}
	}
	w := messageRequest(t, "POST", "/api/messages/peer/action", `{"action":"kill","value":"off","confirm":true}`, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	lookups, _ := os.ReadFile(filepath.Join(root, "lookups"))
	if string(lookups) != "lookup\n" {
		t.Fatal(string(lookups))
	}
	remote, _ := os.ReadFile(filepath.Join(root, "ssh-args"))
	if !strings.Contains(string(remote), "'kill' 'off' '--json'") {
		t.Fatal(string(remote))
	}
	for _, path := range []string{"/api/messages/unknown/pending", "/api/messages/disabled/pending", "/api/messages/local/--help", "/api/messages/local/a%3Btouch", "/api/messages/local/a%2Fb"} {
		if w := messageRequest(t, "GET", path, "", true); w.Code != 400 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
}

func TestMessageMissingBinaryUpgradeAndTimeout(t *testing.T) {
	root := fakeMessages(t)
	if err := os.Remove(filepath.Join(root, "bin/herd-msg")); err != nil {
		t.Fatal(err)
	}
	savedPath := os.Getenv("PATH")
	t.Setenv("PATH", filepath.Join(root, "bin"))
	_, err := messageSnapshot(context.Background(), Machine{})
	t.Setenv("PATH", savedPath)
	if err == nil || messageProblem(err) != "not_installed" {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "path"), []byte("/missing/herd-msg"), 0600)
	_, err = messageSnapshot(context.Background(), Machine{ID: "peer", Target: "fake-peer"})
	if err == nil || messageProblem(err) != "not_installed" {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "path"), []byte("/tmp/$(touch marker)"), 0600)
	messagePaths = sync.Map{}
	_, err = messageSnapshot(context.Background(), Machine{ID: "peer", Target: "fake-peer"})
	if err == nil || !strings.Contains(err.Error(), "invalid remote") {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "bin/herd-msg"), []byte("#!/bin/sh\necho '{\"schema\":2}'\n"), 0700)
	_, err = messageSnapshot(context.Background(), Machine{})
	if err == nil || messageProblem(err) != "upgrade" {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "bin/herd-msg"), []byte("#!/bin/sh\nsleep 5 &\nwait\n"), 0700)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = messageSnapshot(ctx, Machine{})
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("timeout failed", err, time.Since(start))
	}
}
