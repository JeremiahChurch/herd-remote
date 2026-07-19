package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// herdr.go wraps the `herdr` socket-API CLI and the `herd-spawn` helper.
// Every call shells out to the already-running herdr server, so this process
// needs no privileged access beyond the user's own herdr socket.

const (
	herdrBin  = "herdr"      // resolved from PATH (~/.local/bin/herdr)
	spawnBin  = "herd-spawn" // resolved from PATH
	cliTimout = 15 * time.Second
)

// Session is the flattened view the UI consumes: one agent-bearing pane.
type Session struct {
	WorkspaceID string `json:"workspace_id"`
	PaneID      string `json:"pane_id"`
	Label       string `json:"label"`   // workspace label = what shows in herdr's nav
	Name        string `json:"name"`    // agent-reported name (often folder/worktree)
	Agent       string `json:"agent"`   // claude | codex | ...
	Status      string `json:"status"`  // idle | working | blocked | done | unknown
	Cwd         string `json:"cwd"`     // foreground cwd
	Number      int    `json:"number"`  // stable-ish nav number
	Focused     bool   `json:"focused"` // currently focused in the TUI
}

// run executes a herdr subcommand and returns raw stdout.
func run(bin string, args ...string) ([]byte, error) {
	ctx, cancel := contextWithTimeout(cliTimout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("%s %s: %s", bin, strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return out, nil
}

// --- JSON shapes emitted by herdr (only the fields we use) ---

type agentListResp struct {
	Result struct {
		Agents []struct {
			Agent         string `json:"agent"`
			AgentStatus   string `json:"agent_status"`
			Cwd           string `json:"cwd"`
			ForegroundCwd string `json:"foreground_cwd"`
			Focused       bool   `json:"focused"`
			Name          string `json:"name"`
			PaneID        string `json:"pane_id"`
			WorkspaceID   string `json:"workspace_id"`
		} `json:"agents"`
	} `json:"result"`
}

type workspaceListResp struct {
	Result struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
			Label       string `json:"label"`
			Number      int    `json:"number"`
			AgentStatus string `json:"agent_status"`
			Focused     bool   `json:"focused"`
		} `json:"workspaces"`
	} `json:"result"`
}

type readResp struct {
	Result struct {
		Read struct {
			Text   string `json:"text"`
			PaneID string `json:"pane_id"`
		} `json:"read"`
	} `json:"result"`
}

// ListSessions merges `herdr agent list` (agent-bearing panes) with
// `herdr workspace list` (nav labels + numbers), keyed by workspace_id.
func ListSessions() ([]Session, error) {
	aOut, err := run(herdrBin, "agent", "list")
	if err != nil {
		return nil, err
	}
	var al agentListResp
	if err := json.Unmarshal(aOut, &al); err != nil {
		return nil, fmt.Errorf("parse agent list: %w", err)
	}

	// Workspace labels/numbers (best-effort; UI still works without them).
	labels := map[string]string{}
	numbers := map[string]int{}
	if wOut, err := run(herdrBin, "workspace", "list"); err == nil {
		var wl workspaceListResp
		if json.Unmarshal(wOut, &wl) == nil {
			for _, w := range wl.Result.Workspaces {
				labels[w.WorkspaceID] = w.Label
				numbers[w.WorkspaceID] = w.Number
			}
		}
	}

	out := make([]Session, 0, len(al.Result.Agents))
	for _, a := range al.Result.Agents {
		cwd := a.ForegroundCwd
		if cwd == "" {
			cwd = a.Cwd
		}
		s := Session{
			WorkspaceID: a.WorkspaceID,
			PaneID:      a.PaneID,
			Label:       labels[a.WorkspaceID],
			Name:        a.Name,
			Agent:       a.Agent,
			Status:      a.AgentStatus,
			Cwd:         cwd,
			Number:      numbers[a.WorkspaceID],
			Focused:     a.Focused,
		}
		if s.Label == "" {
			s.Label = a.Name
		}
		if s.Label == "" {
			s.Label = filepath.Base(cwd)
		}
		out = append(out, s)
	}

	// Sort: blocked first (needs attention), then working, then by nav number.
	rank := func(st string) int {
		switch st {
		case "blocked":
			return 0
		case "working":
			return 1
		case "idle":
			return 2
		case "done":
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Status), rank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		return out[i].Number < out[j].Number
	})
	return out, nil
}

// ReadPane returns the visible screen text for a pane.
func ReadPane(pane string, lines int) (string, error) {
	if lines <= 0 {
		lines = 40
	}
	// --source recent pulls scrollback history (not just the visible screen), so the
	// phone can scroll well past the current viewport; --format ansi keeps SGR colors.
	out, err := run(herdrBin, "agent", "read", pane, "--source", "recent", "--lines", fmt.Sprint(lines), "--format", "ansi")
	if err != nil {
		return "", err
	}
	var r readResp
	if err := json.Unmarshal(out, &r); err != nil {
		return "", fmt.Errorf("parse read: %w", err)
	}
	return r.Result.Read.Text, nil
}

// SendPrompt types text + Enter into a pane (the submit primitive).
func SendPrompt(pane, text string) error {
	_, err := run(herdrBin, "pane", "run", pane, text)
	return err
}

// SendKey sends a single verified control key to a pane.
// Allowed set is enforced here so the HTTP layer can't inject arbitrary tokens.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "C-c": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	"Tab": true, "Backspace": true, "Space": true,
	"y": true, "n": true, "1": true, "2": true, "3": true, "4": true, "5": true,
}

func SendKey(pane, key string) error {
	if !allowedKeys[key] {
		return fmt.Errorf("key not allowed: %q", key)
	}
	_, err := run(herdrBin, "pane", "send-keys", pane, key)
	return err
}

// Rename updates every place herdr shows a name for a session: the workspace
// label (left-nav), the agent label, and the pane label (both shown on pane
// borders / the agents panel). herdr has no way to rename an agent's *internal*
// session (Claude/Codex don't expose that), but this keeps all herdr surfaces
// consistent instead of leaving the stale folder name on the pane border.
func Rename(workspaceID, paneID, label string) error {
	label = strings.TrimSpace(label)
	if err := validateLabel(label); err != nil {
		return err
	}
	// Workspace label is the authoritative nav name; fail if it errors.
	if _, err := run(herdrBin, "workspace", "rename", workspaceID, label); err != nil {
		return err
	}
	// Agent + pane labels are best-effort (a pane may have no live agent).
	if paneID != "" {
		_, _ = run(herdrBin, "agent", "rename", paneID, label)
		_, _ = run(herdrBin, "pane", "rename", paneID, label)
	}
	return nil
}

// CloseWorkspace tears down a workspace (kills the session).
func CloseWorkspace(workspaceID string) error {
	_, err := run(herdrBin, "workspace", "close", workspaceID)
	return err
}

// codexDefaultModel is what a blank Model field means for a Codex spawn: match
// the user's ~/.codex/config.toml default so phone spawns get the same shape as
// a terminal `codexd`. Claude is left blank on purpose (its own default is Opus).
const codexDefaultModel = "gpt-5.6-sol"

// codexEfforts is the allowlist for the codex reasoning-effort passthrough, so the
// HTTP layer can only hand herd-spawn a known-good value (it becomes a shell token).
var codexEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true}

// reSessionID guards a resume target before it becomes a herd-spawn -r shell token.
// Claude ids are UUIDs; codex thread ids are UUID-ish - both are hex + dashes.
var reSessionID = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// Spawn creates a new workspace in dir and launches the chosen agent, optionally
// seeded. agent is "claude" (default) or "codex". name, when set, becomes the
// workspace label (herdr nav + this app's list) via herd-spawn's -l. effort is a
// codex-only reasoning-effort override (ignored for claude). resumeID, when set,
// resumes that existing session instead of starting fresh (model/effort are then the
// session's own, so they aren't re-sent).
func Spawn(dir, prompt, model, effort, agent, name, resumeID string, background bool) (string, error) {
	if agent == "" {
		agent = "claude"
	}
	if agent != "claude" && agent != "codex" {
		return "", fmt.Errorf("unknown agent: %q", agent)
	}
	if resumeID != "" && !reSessionID.MatchString(resumeID) {
		return "", fmt.Errorf("bad resume id")
	}
	if resumeID == "" && model == "" && agent == "codex" {
		model = codexDefaultModel
	}
	if effort != "" && !codexEfforts[effort] {
		return "", fmt.Errorf("unknown effort: %q", effort)
	}
	args := []string{"-a", agent}
	if background {
		args = append(args, "-b")
	}
	if resumeID != "" {
		args = append(args, "-r", resumeID)
	} else {
		if model != "" {
			args = append(args, "-m", model)
		}
		// effort is codex-only; herd-spawn ignores -e for claude, but don't send it.
		if effort != "" && agent == "codex" {
			args = append(args, "-e", effort)
		}
	}
	if name = strings.TrimSpace(name); name != "" {
		args = append(args, "-l", name)
	}
	// `--` stops flag parsing so a dir like "--list" is treated as a path, not an option.
	args = append(args, "--", dir)
	if prompt != "" {
		args = append(args, prompt)
	}
	out, err := run(spawnBin, args...)
	return strings.TrimSpace(string(out)), err
}

// Folders lists candidate spawn targets: immediate + one-level-deep dirs under
// $HOME (skipping dotdirs and junk), so the phone UI can present a picker.
func Folders() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{
		"node_modules": true, ".git": true, "vendor": true,
		"AppData": true, "Android": true, "Downloads": true, "downloads": true,
	}
	var dirs []string
	add := func(p string) { dirs = append(dirs, p) }

	top, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}
	for _, e := range top {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || skip[e.Name()] {
			continue
		}
		p := filepath.Join(home, e.Name())
		add(p)
		// one level deeper, so worktree/monorepo subdirs are reachable
		if sub, err := os.ReadDir(p); err == nil {
			for _, se := range sub {
				if !se.IsDir() || strings.HasPrefix(se.Name(), ".") || skip[se.Name()] {
					continue
				}
				add(filepath.Join(p, se.Name()))
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// --- session history (resume) ---
//
// Both agents persist every session to disk, so we can list resumable past sessions
// without touching either TUI:
//   claude: ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl - the uuid IS the resume id;
//           name = last "ai-title" record; cwd = from the transcript. (herdr's
//           agent_session.value for a live claude pane equals this uuid.)
//   codex:  ~/.codex/session_index.jsonl - a prebuilt {id, thread_name, updated_at}
//           index. cwd isn't in it, so codex folder-filtering is best-effort.

// HistoryEntry is one resumable past session from an agent's on-disk store.
type HistoryEntry struct {
	Agent    string `json:"agent"`    // claude | codex
	ID       string `json:"id"`       // resume target
	Name     string `json:"name"`     // ai-title (claude) / thread_name (codex)
	Cwd      string `json:"cwd"`      // claude: authoritative; codex: usually ""
	Modified int64  `json:"modified"` // unix seconds, for recency sort
}

// historyScanCap bounds how many of the most-recent claude transcripts we open to
// extract titles per request (each open is a file scan, so we cap the cost).
const historyScanCap = 80

// HistorySessions returns resumable past sessions from both agents, most-recent first.
// filter, when set (case-insensitive), matches claude entries on cwd and codex entries
// on name (codex has no cwd in its index). limit caps the returned list.
func HistorySessions(filter string, limit int) []HistoryEntry {
	if limit <= 0 {
		limit = 60
	}
	all := append(claudeHistory(), codexHistory()...)
	if filter = strings.ToLower(strings.TrimSpace(filter)); filter != "" {
		kept := all[:0]
		for _, e := range all {
			hay := e.Cwd
			if e.Agent == "codex" {
				hay = e.Name
			}
			if strings.Contains(strings.ToLower(hay), filter) {
				kept = append(kept, e)
			}
		}
		all = kept
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Modified > all[j].Modified })
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// claudeHistory scans the most-recent transcripts under ~/.claude/projects.
func claudeHistory() []HistoryEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	root := filepath.Join(home, ".claude", "projects")
	proj, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	type ref struct {
		path  string
		mtime time.Time
	}
	var refs []ref
	for _, pd := range proj {
		if !pd.IsDir() {
			continue
		}
		dir := filepath.Join(root, pd.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			refs = append(refs, ref{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	// newest first, then only open the top N (title extraction is a per-file scan)
	sort.Slice(refs, func(i, j int) bool { return refs[i].mtime.After(refs[j].mtime) })
	if len(refs) > historyScanCap {
		refs = refs[:historyScanCap]
	}
	out := make([]HistoryEntry, 0, len(refs))
	for _, r := range refs {
		name, cwd := claudeTitleAndCwd(r.path)
		out = append(out, HistoryEntry{
			Agent:    "claude",
			ID:       strings.TrimSuffix(filepath.Base(r.path), ".jsonl"),
			Name:     name,
			Cwd:      cwd,
			Modified: r.mtime.Unix(),
		})
	}
	return out
}

// claudeTitleAndCwd pulls the last "ai-title" and the first "cwd" from a transcript,
// scanning line-by-line so a multi-MB file isn't held fully parsed in memory.
func claudeTitleAndCwd(path string) (title, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // attachment lines can be large
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(`"ai-title"`)) {
			var t struct {
				AiTitle string `json:"aiTitle"`
			}
			if json.Unmarshal(line, &t) == nil && t.AiTitle != "" {
				title = t.AiTitle // keep the last one (most recent title)
			}
		} else if cwd == "" && bytes.Contains(line, []byte(`"cwd":`)) {
			var c struct {
				Cwd string `json:"cwd"`
			}
			if json.Unmarshal(line, &c) == nil {
				cwd = c.Cwd
			}
		}
	}
	return title, cwd
}

// codexHistory reads the prebuilt ~/.codex/session_index.jsonl index.
func codexHistory() []HistoryEntry {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	f, err := os.Open(filepath.Join(home, ".codex", "session_index.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	for sc.Scan() {
		var rec struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.ID == "" {
			continue
		}
		var mod int64
		if t, err := time.Parse(time.RFC3339, rec.UpdatedAt); err == nil {
			mod = t.Unix()
		}
		out = append(out, HistoryEntry{
			Agent: "codex", ID: rec.ID, Name: rec.ThreadName, Modified: mod,
		})
	}
	return out
}
