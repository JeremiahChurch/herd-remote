package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const messageTimeout = 6 * time.Second

var reMessageID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var reMessagePath = regexp.MustCompile(`^/[A-Za-z0-9_./@+ -]+$`)

type MessageRow struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	State     string `json:"state"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
	FirstLine string `json:"first_line"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error"`
}
type MessageSession struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	State    string `json:"state"`
	OptedOut bool   `json:"opted_out"`
	Unsent   int    `json:"unsent"`
	Blocked  bool   `json:"blocked"`
}
type MessageStatus struct {
	Schema   int               `json:"schema"`
	Machine  string            `json:"machine"`
	Version  string            `json:"version"`
	Killed   bool              `json:"killed"`
	Counts   map[string]int    `json:"counts"`
	Sessions []MessageSession  `json:"sessions"`
	Pending  []MessageRow      `json:"pending"`
	Recent   []MessageRow      `json:"recent"`
	Links    []json.RawMessage `json:"links"`
	Health   []struct {
		Check  string `json:"check"`
		Detail string `json:"detail"`
		Fix    string `json:"fix"`
	} `json:"health"`
}
type MessageMachine struct {
	ID      string         `json:"id"`
	Label   string         `json:"label"`
	Status  *MessageStatus `json:"status,omitempty"`
	Error   string         `json:"error,omitempty"`
	Problem string         `json:"problem,omitempty"`
}

type messagePathEntry struct {
	sync.Mutex
	path string
}

var messagePaths sync.Map // SSH target to successful path lookup, for this process only.

// messageExec uses argv locally. Only SSH's remote command is a shell string.
func messageExec(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, errors.New("machine unreachable or command timed out")
	}
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A web server launched by an agent still acts as the logged-in operator.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "CODEX_THREAD_ID=") && !strings.HasPrefix(env, "CLAUDE_CODE_SESSION_ID=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		// Kill the child group; a detached descendant must not extend the deadline.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
		}
		return nil, errors.New("machine unreachable or command timed out")
	}
	out := stdout.Bytes()
	if err != nil {
		var body struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(out, &body) == nil && body.Error != "" {
			return nil, errors.New(body.Error)
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return out, nil
}

func messageSSH(ctx context.Context, m Machine, command string) ([]byte, error) {
	if !reSSHTarget.MatchString(m.Target) {
		return nil, errors.New("machine has no usable SSH target")
	}
	return messageExec(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", "--", m.Target, command)
}

func messagePath(ctx context.Context, m Machine) (string, error) {
	entry, _ := messagePaths.LoadOrStore(m.Target, &messagePathEntry{})
	cached := entry.(*messagePathEntry)
	cached.Lock()
	defer cached.Unlock()
	if cached.path != "" {
		return cached.path, nil
	}
	out, err := messageSSH(ctx, m, `command -v herd-msg || echo "$HOME/.local/bin/herd-msg"`)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(string(out), "\n")
	if len(path) > 1024 || !reMessagePath.MatchString(path) {
		return "", errors.New("invalid remote herd-msg path")
	}
	cached.path = path
	return path, nil
}

func messageCommand(ctx context.Context, m Machine, args ...string) ([]byte, error) {
	if m.ID == "" {
		return messageExec(ctx, "herd-msg", args...)
	}
	path, err := messagePath(ctx, m)
	if err != nil {
		return nil, err
	}
	argv := append([]string{"env", "-u", "CODEX_THREAD_ID", "-u", "CLAUDE_CODE_SESSION_ID", path}, args...)
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return messageSSH(ctx, m, strings.Join(quoted, " "))
}

func messageSnapshot(ctx context.Context, m Machine) (*MessageStatus, error) {
	out, err := messageCommand(ctx, m, "status", "--json")
	if err != nil {
		return nil, err
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(out, &header); err != nil {
		return nil, errors.New("invalid herd-msg status response")
	}
	if header.Schema != 1 {
		return nil, fmt.Errorf("unsupported status schema %d; upgrade herd-remote", header.Schema)
	}
	var status MessageStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return nil, errors.New("invalid herd-msg status response")
	}
	return &status, nil
}

func messageProblem(err error) string {
	var exit *exec.ExitError
	if errors.Is(err, exec.ErrNotFound) || (errors.As(err, &exit) && (exit.ExitCode() == 127 || exit.ExitCode() == 126)) {
		return "not_installed"
	}
	if strings.Contains(err.Error(), "upgrade herd-remote") {
		return "upgrade"
	}
	return "unavailable"
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	if !methodGuard(w, r, http.MethodGet) {
		return
	}
	machines := append([]Machine{{Label: "This machine"}}, Machines()...)
	machinesCache.Lock()
	discoveryError := machinesCache.err
	machinesCache.Unlock()
	rows := make([]MessageMachine, len(machines))
	var wg sync.WaitGroup
	for i, m := range machines {
		wg.Add(1)
		go func(i int, m Machine) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), messageTimeout)
			defer cancel()
			id := m.ID
			if id == "" {
				id = "local"
			}
			row := MessageMachine{ID: id, Label: m.Label}
			status, err := messageSnapshot(ctx, m)
			if err != nil {
				row.Error, row.Problem = err.Error(), messageProblem(err)
			} else {
				row.Status = status
			}
			rows[i] = row
		}(i, m)
	}
	wg.Wait()
	if discoveryError != "" {
		rows = append(rows, MessageMachine{ID: "discovery", Label: "Saved machines", Problem: "unavailable", Error: discoveryError})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"machines": rows})
}

type messageAction struct {
	Action  string `json:"action"`
	ID      string `json:"id"`
	Value   string `json:"value"`
	Confirm bool   `json:"confirm"`
	Updated string `json:"updated"`
	Unsent  *int   `json:"unsent"`
}

// Validate the whole request before constructing either local argv or remote shell text.
func messageActionArgs(a messageAction) ([]string, error) {
	if a.Action != "kill" && !reMessageID.MatchString(a.ID) {
		return nil, errors.New("invalid message or session ID")
	}
	switch a.Action {
	case "release":
		return []string{"release", a.ID, "--json"}, nil
	case "resolve":
		if a.Value != "resend" && a.Value != "drop" {
			return nil, errors.New("resolve requires resend or drop")
		}
		if !a.Confirm {
			return nil, errors.New("confirmation required; resend may run twice")
		}
		updated, err := time.Parse(time.RFC3339Nano, a.Updated)
		if err != nil {
			return nil, errors.New("refresh the message before resolving")
		}
		return []string{"resolve", a.ID, a.Value, "--confirm", "--if-updated", updated.UTC().Format(time.RFC3339Nano), "--json"}, nil
	case "prune":
		if !a.Confirm || a.Unsent == nil || *a.Unsent < 0 {
			return nil, errors.New("confirm the displayed unsent count before clearing")
		}
		return []string{"prune", "--session", a.ID, "--confirm", "--expect-unsent", strconv.Itoa(*a.Unsent), "--json"}, nil
	case "allow", "kill":
		if a.Value != "on" && a.Value != "off" {
			return nil, errors.New("value must be on or off")
		}
		if !a.Confirm {
			return nil, errors.New("confirmation required")
		}
		if a.Action == "allow" {
			return []string{"allow", "--session", a.ID, a.Value, "--json"}, nil
		}
		return []string{"kill", a.Value, "--json"}, nil
	default:
		return nil, errors.New("unknown message action")
	}
}

// /api/messages/{machine}/{id}, or POST /api/messages/{machine}/action.
// "local" names this box; remote IDs must resolve to an enabled saved machine.
func handleMessage(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/messages/"), "/")
	if len(parts) != 2 || !reMessageID.MatchString(parts[0]) {
		apiError(w, 400, "need /{machine}/{id} or /{machine}/action")
		return
	}
	machine := parts[0]
	if machine == "local" {
		machine = ""
	}
	m, err := lookupMachine(machine)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	var args []string
	if parts[1] == "action" {
		if !methodGuard(w, r, http.MethodPost) {
			return
		}
		var a messageAction
		dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			apiError(w, 400, "bad body")
			return
		}
		args, err = messageActionArgs(a)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
	} else {
		if !methodGuard(w, r, http.MethodGet) {
			return
		}
		if !reMessageID.MatchString(parts[1]) {
			apiError(w, 400, "invalid message ID")
			return
		}
		args = []string{"show", parts[1], "--json"}
	}
	ctx, cancel := context.WithTimeout(r.Context(), messageTimeout)
	defer cancel()
	out, err := messageCommand(ctx, m, args...)
	if err != nil {
		code := http.StatusBadGateway
		if args[0] == "show" && strings.HasPrefix(err.Error(), "message ") && strings.HasSuffix(err.Error(), " is missing or expired") {
			code = http.StatusNotFound
		}
		apiError(w, code, err.Error())
		return
	}
	if len(bytes.TrimSpace(out)) == 0 {
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	if !json.Valid(out) {
		apiError(w, 502, "invalid herd-msg response")
		return
	}
	writeJSON(w, 200, json.RawMessage(out))
}
