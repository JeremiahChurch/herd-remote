package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

var (
	// Anchored to herd-spawn's exact line: spawned workspace <wid> "<label>"  dir=<dir>  pane=<pane>
	reSpawnWid = regexp.MustCompile(`(?m)^spawned workspace (\S+) `)
	// pane=<pane> is followed by either end-of-line or `  seed="..."` (present only
	// when a first prompt was given). Anchoring on that trailer - instead of a bare
	// ` pane=(\S+)` - keeps a crafted label like `x pane=wZZ:p1` (mid-line, always
	// trailed by `"  dir=`) from hijacking the parse, and tolerates dirs with spaces.
	reSpawnPane = regexp.MustCompile(`(?m) pane=(\S+?)(?:  seed=|\s*$)`)
	// herdr pane/workspace ids look like w7C or wAJ:p1 - letters, digits, ':' only.
	reHerdrID = regexp.MustCompile(`^[A-Za-z0-9:_-]+$`)
	// reSafeLabel constrains user-supplied session names/labels: no leading '-'
	// (herdr rename / herd-spawn -l would read it as a flag) and no '=' (a crafted
	// name could otherwise forge a `pane=`/`seed=` token in herd-spawn's output).
	// Realistic labels - folder names, ticket-ish slugs - all fit.
	reSafeLabel = regexp.MustCompile(`^[A-Za-z0-9 ._:/][A-Za-z0-9 ._:/-]*$`)
)

// reLabelStrip matches any run of chars NOT allowed in a herdr label; sanitizeLabel
// replaces each run with a single space so an agent title becomes a safe label.
var reLabelStrip = regexp.MustCompile(`[^A-Za-z0-9 ._:/-]+`)

// sanitizeLabel coerces an arbitrary agent title into a reSafeLabel-valid string:
// disallowed chars -> space, collapse whitespace, drop a leading '-', cap at 100.
// May return "" (e.g. an all-punctuation title), which the caller treats as "no label".
func sanitizeLabel(s string) string {
	s = reLabelStrip.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace runs + trim
	s = strings.TrimSpace(strings.TrimLeft(s, "-"))
	if len(s) > 100 {
		s = strings.TrimSpace(s[:100])
	}
	return s
}

// validateLabel guards any name we hand to herdr rename / herd-spawn -l.
func validateLabel(s string) error {
	if s == "" {
		return fmt.Errorf("empty label")
	}
	if len(s) > 100 {
		return fmt.Errorf("label too long (max 100)")
	}
	if !reSafeLabel.MatchString(s) {
		return fmt.Errorf("label may use letters, digits, space, and . _ : / - (no leading -)")
	}
	return nil
}

// methodGuard rejects the request unless it uses the expected HTTP method.
func methodGuard(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

const (
	cookieName    = "herd_remote"
	sessionMaxAge = 30 * 24 * time.Hour // "super long login session"
)

var (
	serverSecret []byte // signs session cookies; persisted so restarts keep you logged in
	loginPass    string // shared secret; from HERD_REMOTE_PASSWORD or config file
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func configDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "herd-remote")
}

// loadOrCreateSecret persists a random 32-byte HMAC key so cookies survive restarts.
func loadOrCreateSecret() ([]byte, error) {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "secret")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

// loadPassword resolves the shared secret from env or config file.
func loadPassword() string {
	if p := os.Getenv("HERD_REMOTE_PASSWORD"); p != "" {
		return p
	}
	if b, err := os.ReadFile(filepath.Join(configDir(), "password")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// --- session cookie: base64(exp) . hex(hmac(secret, exp)) ---

func signToken(exp int64) string {
	msg := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, serverSecret)
	mac.Write([]byte(msg))
	return msg + "." + hex.EncodeToString(mac.Sum(nil))
}

func validToken(tok string) bool {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	want := signToken(exp)
	return subtle.ConstantTimeCompare([]byte(tok), []byte(want)) == 1
}

func authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return validToken(c.Value)
}

// --- HTTP helpers ---

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// requireAuth guards every /api route except /api/login.
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			apiError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "bad body")
		return
	}
	if loginPass == "" {
		apiError(w, http.StatusInternalServerError, "server has no password set")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(loginPass)) != 1 {
		apiError(w, http.StatusUnauthorized, "wrong password")
		return
	}
	exp := time.Now().Add(sessionMaxAge).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    signToken(exp),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMaxAge / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodPost) {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodGet) {
		return
	}
	sessions, err := ListSessions()
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": sessions})
}

func handleFolders(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodGet) {
		return
	}
	dirs, err := Folders()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	home, _ := os.UserHomeDir()
	// present ~-relative for compactness on a phone
	rel := make([]map[string]string, 0, len(dirs))
	for _, d := range dirs {
		short := d
		if strings.HasPrefix(d, home) {
			short = "~" + strings.TrimPrefix(d, home)
		}
		rel = append(rel, map[string]string{"path": d, "short": short})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"folders": rel})
}

func handleHistory(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodGet) {
		return
	}
	filter := r.URL.Query().Get("dir")
	limit := 60
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	entries := HistorySessions(filter, limit)
	home, _ := os.UserHomeDir()
	type outEntry struct {
		Agent    string `json:"agent"`
		ID       string `json:"id"`
		Name     string `json:"name"`
		Cwd      string `json:"cwd"`
		Short    string `json:"short"` // ~-relative cwd for a compact phone list
		Modified int64  `json:"modified"`
	}
	list := make([]outEntry, 0, len(entries))
	for _, e := range entries {
		short := e.Cwd
		if home != "" && strings.HasPrefix(e.Cwd, home) {
			short = "~" + strings.TrimPrefix(e.Cwd, home)
		}
		list = append(list, outEntry{e.Agent, e.ID, e.Name, e.Cwd, short, e.Modified})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": list})
}

func handleSpawn(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodPost) {
		return
	}
	var body struct {
		Dir        string `json:"dir"`
		Prompt     string `json:"prompt"`
		Model      string `json:"model"`
		Effort     string `json:"effort"`
		Agent      string `json:"agent"`
		Name       string `json:"name"`
		Resume     string `json:"resume"`
		Background bool   `json:"background"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "bad body")
		return
	}
	if body.Dir == "" {
		apiError(w, http.StatusBadRequest, "dir required")
		return
	}
	name := strings.TrimSpace(body.Name)
	// A resume label comes from the agent's own title (ai-title / thread_name), which
	// carries punctuation the label rule rejects - sanitize it into a safe label rather
	// than 400 the whole resume. A typed name is still validated strictly.
	if body.Resume != "" {
		name = sanitizeLabel(name)
	}
	if name != "" {
		if err := validateLabel(name); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	out, err := Spawn(body.Dir, body.Prompt, body.Model, body.Effort, body.Agent, name, body.Resume, body.Background)
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	// herd-spawn prints: spawned workspace <wid> "<label>"  dir=<dir>  pane=<pane>
	wid, pane := "", ""
	if m := reSpawnWid.FindStringSubmatch(out); m != nil {
		wid = m[1]
	}
	if m := reSpawnPane.FindStringSubmatch(out); m != nil {
		pane = m[1]
	}
	// herd-spawn's -l already labeled the workspace (herdr nav + this app's list).
	// Also stamp the pane/agent border so the name shows everywhere in herdr.
	if name != "" && pane != "" {
		_, _ = run(herdrBin, "pane", "rename", pane, name)
		_, _ = run(herdrBin, "agent", "rename", pane, name)
	}
	label := name
	writeJSON(w, http.StatusOK, map[string]string{
		"result": out, "workspace_id": wid, "pane_id": pane,
		"agent": body.Agent, "dir": body.Dir, "label": label,
	})
}

// /api/sessions/{pane-or-wid}/{action}
func handleSessionAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 {
		apiError(w, http.StatusBadRequest, "need /{id}/{action}")
		return
	}
	id, action := parts[0], parts[1]
	if !reHerdrID.MatchString(id) {
		apiError(w, http.StatusBadRequest, "bad id")
		return
	}
	// read is a GET; everything else mutates and must be POST.
	if action == "read" {
		if !methodGuard(w, r, http.MethodGet) {
			return
		}
	} else if !methodGuard(w, r, http.MethodPost) {
		return
	}

	switch action {
	case "read":
		lines := 40
		if l := r.URL.Query().Get("lines"); l != "" {
			if n, err := strconv.Atoi(l); err == nil {
				lines = n
			}
		}
		if lines < 1 {
			lines = 1
		} else if lines > 500 {
			lines = 500
		}
		text, err := ReadPane(id, lines)
		if err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"text": text})

	case "prompt":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "bad body")
			return
		}
		if err := SendPrompt(id, body.Text); err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case "key":
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "bad body")
			return
		}
		if err := SendKey(id, body.Key); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case "rename":
		var body struct {
			Label string `json:"label"`
			Pane  string `json:"pane"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
			apiError(w, http.StatusBadRequest, "bad body")
			return
		}
		if err := Rename(id, body.Pane, body.Label); err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	case "close":
		if err := CloseWorkspace(id); err != nil {
			apiError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})

	default:
		apiError(w, http.StatusNotFound, "unknown action")
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "missing ui", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func main() {
	addr := os.Getenv("HERD_REMOTE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8787" // bind loopback; expose to LAN via `expose-port add 8787`
	}

	var err error
	serverSecret, err = loadOrCreateSecret()
	if err != nil {
		log.Fatalf("secret: %v", err)
	}
	loginPass = loadPassword()
	if loginPass == "" {
		log.Printf("WARNING: no password set. Set HERD_REMOTE_PASSWORD or write %s/password", configDir())
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/logout", handleLogout)
	mux.HandleFunc("/api/sessions", requireAuth(handleSessions))
	mux.HandleFunc("/api/sessions/", requireAuth(handleSessionAction))
	mux.HandleFunc("/api/folders", requireAuth(handleFolders))
	mux.HandleFunc("/api/history", requireAuth(handleHistory))
	mux.HandleFunc("/api/spawn", requireAuth(handleSpawn))

	srv := &http.Server{
		Addr:         addr,
		Handler:      logRequests(mux),
		ReadTimeout:  20 * time.Second,
		WriteTimeout: 45 * time.Second, // > worst-case 2x herdr CLI timeout in ListSessions
	}
	log.Printf("herd-remote listening on http://%s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/api/sessions/") || !strings.HasSuffix(r.URL.Path, "/read") {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
