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
	reSpawnWid  = regexp.MustCompile(`workspace\s+(\S+)`)
	reSpawnPane = regexp.MustCompile(`pane=(\S+)`)
)

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
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := ListSessions()
	if err != nil {
		apiError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sessions": sessions})
}

func handleFolders(w http.ResponseWriter, r *http.Request) {
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

func handleSpawn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Dir        string `json:"dir"`
		Prompt     string `json:"prompt"`
		Model      string `json:"model"`
		Agent      string `json:"agent"`
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
	out, err := Spawn(body.Dir, body.Prompt, body.Model, body.Agent, body.Background)
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
	writeJSON(w, http.StatusOK, map[string]string{
		"result": out, "workspace_id": wid, "pane_id": pane, "agent": body.Agent, "dir": body.Dir,
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

	switch action {
	case "read":
		lines := 40
		if l := r.URL.Query().Get("lines"); l != "" {
			if n, err := strconv.Atoi(l); err == nil {
				lines = n
			}
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
	mux.HandleFunc("/api/spawn", requireAuth(handleSpawn))

	srv := &http.Server{
		Addr:         addr,
		Handler:      logRequests(mux),
		ReadTimeout:  20 * time.Second,
		WriteTimeout: 20 * time.Second,
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

var _ = fmt.Sprint // keep fmt import if trimmed later
