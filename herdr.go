package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
			Agent       string `json:"agent"`
			AgentStatus string `json:"agent_status"`
			Cwd         string `json:"cwd"`
			ForegroundCwd string `json:"foreground_cwd"`
			Focused     bool   `json:"focused"`
			Name        string `json:"name"`
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
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
	// --format ansi keeps SGR color codes so the UI can render them.
	out, err := run(herdrBin, "agent", "read", pane, "--source", "visible", "--lines", fmt.Sprint(lines), "--format", "ansi")
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
	if label == "" {
		return fmt.Errorf("empty label")
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

// Spawn creates a new workspace in dir and launches the chosen agent, optionally
// seeded. agent is "claude" (default) or "codex".
func Spawn(dir, prompt, model, agent string, background bool) (string, error) {
	if agent == "" {
		agent = "claude"
	}
	if agent != "claude" && agent != "codex" {
		return "", fmt.Errorf("unknown agent: %q", agent)
	}
	args := []string{"-a", agent}
	if background {
		args = append(args, "-b")
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, dir)
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
